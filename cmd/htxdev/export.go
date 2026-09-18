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

	read := st.Upcoming
	if *preview {
		read = st.UpcomingPreview
	}
	events, err := read(ctx, now)
	if err != nil {
		return err
	}

	file := exportFile{
		GeneratedAt: now,
		Preview:     *preview,
		Events:      make([]exportEvent, 0, len(events)),
	}
	for _, e := range events {
		file.Events = append(file.Events, toExportEvent(e, groups[e.GroupSlug]))
	}

	if err := writeJSON(*outPath, file); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%s: %d events", *outPath, len(file.Events))
	if *preview {
		var pending int
		for _, e := range file.Events {
			if e.Pending {
				pending++
			}
		}
		fmt.Fprintf(stdout, ", %d of them PENDING and not publishable", pending)
	}
	fmt.Fprintln(stdout, ".")

	if len(file.Events) == 0 && !*preview {
		// The likeliest reason by far, and a silent empty file is how somebody
		// spends an afternoon debugging the site instead.
		fmt.Fprintf(stdout,
			"Nothing to export. %d of %d groups are verified; until one is, every event stays pending.\n"+
				"Run with -preview to see what verifying them would publish.\n",
			countVerified(groups), len(groups))
	}
	return nil
}

// The export types are separate from core.Event on purpose, and it is the same
// argument as the narrow wire structs in the decoders, pointed the other way.
// This JSON is a published contract that a site and later an API read;
// core.Event is an internal domain type. Marshalling the domain type directly
// would mean every field added to it becomes public the moment it is added,
// including whatever the next decoder needs to carry around internally.
type exportFile struct {
	GeneratedAt time.Time `json:"generated_at"`
	// True when unpublished events are included. The site reads this and says
	// so on the page, because a preview that looks like the real thing is how
	// a screenshot of unverified data ends up somewhere public.
	Preview bool          `json:"preview,omitempty"`
	Events  []exportEvent `json:"events"`
}

type exportEvent struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Excerpt     string       `json:"excerpt,omitempty"`
	Start       time.Time    `json:"start"`
	End         *time.Time   `json:"end,omitempty"`
	AllDay      bool         `json:"all_day,omitempty"`
	Group       exportGroup  `json:"group"`
	Venue       *exportVenue `json:"venue,omitempty"`
	Room        string       `json:"room,omitempty"`
	URL         string       `json:"url,omitempty"`
	RegisterURL string       `json:"register_url,omitempty"`
	Categories  []string     `json:"categories,omitempty"`
	Virtual     bool         `json:"virtual,omitempty"`
	// How many feeds carried this event. Two means dedupe did something, and
	// it is the only place that fact is visible outside the database.
	Sources int `json:"sources,omitempty"`
	// Only ever set in a preview.
	Pending bool `json:"pending,omitempty"`
}

type exportGroup struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

type exportVenue struct {
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	City    string `json:"city,omitempty"`
	State   string `json:"state,omitempty"`
	Zip     string `json:"zip,omitempty"`
	URL     string `json:"url,omitempty"`
}

func toExportEvent(e core.Event, g core.Group) exportEvent {
	out := exportEvent{
		ID:          e.Fingerprint,
		Title:       e.Title,
		Excerpt:     e.Excerpt,
		Start:       e.Start,
		AllDay:      e.AllDay,
		Group:       exportGroup{Slug: e.GroupSlug, Name: g.Name, URL: g.URL},
		Room:        e.Room,
		URL:         e.URL,
		RegisterURL: e.RegisterURL,
		Categories:  e.Categories,
		Virtual:     e.Virtual,
		Sources:     len(e.Sources),
		Pending:     e.Status == store.StatusPending,
	}
	// A pointer, so an event with no end time is absent from the JSON rather
	// than present as year 1. The site can then treat "no end" as a state.
	if !e.End.IsZero() {
		end := e.End
		out.End = &end
	}
	if e.Venue.Name != "" {
		out.Venue = &exportVenue{
			Name: e.Venue.Name, Address: e.Venue.Address, City: e.Venue.City,
			State: e.Venue.State, Zip: e.Venue.Zip, URL: e.Venue.URL,
		}
	}
	// A group the registry no longer carries still has events in the database,
	// and a blank name renders as an empty line rather than as a clue.
	if out.Group.Name == "" {
		out.Group.Name = e.GroupSlug
	}
	return out
}

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
