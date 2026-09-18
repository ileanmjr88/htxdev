package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ileanmjr88/htxdev/internal/api"
	"github.com/ileanmjr88/htxdev/internal/core"
	"github.com/ileanmjr88/htxdev/internal/store"
)

// data/events.json is the rolling upcoming-only window the site reads. The
// database is the permanent record; this is the part of it that is current,
// regenerated on every export rather than appended to.
const defaultExportPath = "data/events.json"

func runExport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("htxdev export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database")
	outPath := fs.String("out", defaultExportPath, "where to write the JSON")
	preview := fs.Bool("preview", false,
		"include events from unverified groups, for looking at locally; never publish this")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("export takes no arguments, got %q", fs.Arg(0))
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	groups, err := st.Groups(ctx)
	if err != nil {
		return err
	}

	// From now, not from the window start. The database keeps everything it
	// has ever seen and the site shows what has not happened yet, which are
	// different questions asked of the same rows.
	now := time.Now().UTC()

	// generated_at is when the data was last current rather than when this
	// file was written, matching what the API serves. Exporting twice without
	// syncing in between now produces an identical file, which keeps the
	// committed copy out of a diff it has nothing to say in.
	generated, err := st.LastSynced(ctx)
	if err != nil {
		return err
	}
	if generated.IsZero() {
		generated = now
	}

	read := st.Upcoming
	if *preview {
		read = st.UpcomingPreview
	}
	events, err := read(ctx, now)
	if err != nil {
		return err
	}

	feed := api.NewFeed(events, groups, generated, *preview)

	if err := writeJSON(*outPath, feed); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%s: %d events", *outPath, len(feed.Events))
	if *preview {
		var pending int
		for _, e := range feed.Events {
			if e.Pending {
				pending++
			}
		}
		fmt.Fprintf(stdout, ", %d of them PENDING and not publishable", pending)
	}
	fmt.Fprintln(stdout, ".")

	if len(feed.Events) == 0 && !*preview {
		// The likeliest reason by far, and a silent empty file is how somebody
		// spends an afternoon debugging the site instead.
		fmt.Fprintf(stdout,
			"Nothing to export. %d of %d groups are verified; until one is, every event stays pending.\n"+
				"Run with -preview to see what verifying them would publish.\n",
			countVerified(groups), len(groups))
	}
	return nil
}

// The wire types live in internal/api, which owns the JSON contract for both
// consumers. This file is the API's response precomputed: the site reads a
// static copy, a caller reads it live, and neither can drift from the other
// because there is only one definition of the shape.

// writeJSON writes the file atomically, via a temporary file in the same
// directory and a rename.
//
// Not ceremony: the site reads this file and the cron rewrites it, so a reader
// arriving mid-write would otherwise get a truncated document. Same directory
// because rename is only atomic within a filesystem.
func writeJSON(path string, v any) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	// Indented, because this file is committed and a diff of one long line is
	// not a diff. Two spaces, matching what the Astro side will format to.
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".events-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op once the rename succeeds

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	// CreateTemp makes the file 0600, which is right for a temporary file and
	// wrong for one that gets committed and served. Git only records the
	// executable bit, so this is about the working copy rather than the repo.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename into %s: %w", path, err)
	}
	return nil
}

func countVerified(groups map[string]core.Group) int {
	var n int
	for _, g := range groups {
		if g.VerifiedBy != "" {
			n++
		}
	}
	return n
}
