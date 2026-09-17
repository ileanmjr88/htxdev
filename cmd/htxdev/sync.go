package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ileanmjr88/htxdev/internal/fetch"
	"github.com/ileanmjr88/htxdev/internal/registry"
)

// Relative, because sync is meant to run from the repo root and Phase 7's
// workflow will do exactly that. The flag covers every other case.
const defaultSourcesPath = "data/sources.yaml"

func runSync(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("htxdev sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sourcesPath := fs.String("sources", defaultSourcesPath, "path to the source registry")
	verbose := fs.Bool("v", false, "list every event fetched, not just the per-source summary")

	if err := fs.Parse(args); err != nil {
		return err // flag already printed why; main lets ErrHelp exit 0
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("sync takes no arguments, got %q", fs.Arg(0))
	}

	// The error already carries the path and, for a validation failure, every
	// problem in the file with its line and column. Wrapping it again would
	// only bury that.
	reg, err := registry.LoadFile(*sourcesPath)
	if err != nil {
		return err
	}

	sources := reg.EnabledSources()
	if len(sources) == 0 {
		return fmt.Errorf("%s: no enabled sources", *sourcesPath)
	}

	results := fetch.Fetch(ctx, sources)

	// fetch reports a cancelled run as a per-source error, because from inside
	// a worker that is all a cancellation looks like. Asking the context here
	// is what keeps Ctrl-C from printing as seven independently broken feeds.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("interrupted: %w", err)
	}

	return verdict(report(stdout, reg, results, *verbose))
}

// verdict decides the process exit status from what the run produced.
//
// One dead feed must not fail the run. That is the fetch layer's whole posture
// and Phase 7's cron leans on it: somebody else's WordPress outage should not
// turn the nightly sync red, and six of seven sources failing is the normal
// state of this repo until the ICS decoder lands in Phase 3. A run where
// nothing at all came back is a different thing. It produced no data, and a
// total failure is far more likely to be ours than theirs.
//
// Split out of runSync so the rule can be tested without a live source, which
// the registry's https-only check otherwise makes unreachable here.
func verdict(sum syncSummary) error {
	if sum.ok == 0 {
		return fmt.Errorf("all %d sources failed", sum.sources)
	}
	return nil
}

// syncSummary is the tally report accumulates as it prints, kept so the exit
// code is decided from counts rather than by walking the results a second time.
type syncSummary struct {
	sources   int
	ok        int
	events    int
	skipped   int
	verified  int
	windowEnd time.Time
}

func report(w io.Writer, reg *registry.Registry, results []fetch.Result, verbose bool) syncSummary {
	sum := syncSummary{sources: len(results)}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tGROUP\tKIND\tEVENTS\tNOTE")

	for _, r := range results {
		status, count := "ok", "-"
		var notes []string

		if r.Err != nil {
			status = "fail"
			notes = append(notes, r.Err.Error())
		} else {
			sum.ok++
			sum.events += len(r.Events)
			count = strconv.Itoa(len(r.Events))
			// Only a successful Result carries the window it asked for, so
			// take it from the first one that has it rather than recomputing
			// the same constants here and hoping the two agree.
			if sum.windowEnd.IsZero() {
				sum.windowEnd = r.WindowEnd
			}
			if !reg.IsVerified(r.Source) {
				notes = append(notes, "unverified, would stay pending")
			}
		}

		sum.skipped += len(r.Skipped)
		if n := len(r.Skipped); n > 0 {
			notes = append(notes, fmt.Sprintf("%d skipped", n))
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			status, r.Source.GroupSlug, r.Source.Kind, count, strings.Join(notes, "; "))
	}
	_ = tw.Flush() // stdout; a flush failure here is not actionable

	for _, g := range reg.Groups {
		if g.VerifiedBy != "" {
			sum.verified++
		}
	}

	fmt.Fprintf(w, "\n%d events from %d of %d sources", sum.events, sum.ok, sum.sources)
	if !sum.windowEnd.IsZero() {
		fmt.Fprintf(w, ", window ends %s", sum.windowEnd.In(houston).Format("Mon 2006-01-02"))
	}
	fmt.Fprintln(w, ".")

	if sum.skipped > 0 {
		fmt.Fprintf(w, "%d events skipped, so cancellation defers this cycle.\n", sum.skipped)
	}
	if sum.verified < len(reg.Groups) {
		fmt.Fprintf(w, "%d of %d groups verified. Unverified groups publish nothing "+
			"until someone reviews their first sync.\n", sum.verified, len(reg.Groups))
	}

	if verbose {
		reportEvents(w, results)
	}
	return sum
}

// reportEvents lists what actually came back, which is the point of running
// sync by hand at this phase. The summary says a feed answered; the listing is
// the only thing that says whether what it returned is Houston tech
// programming or a recurring placeholder.
func reportEvents(w io.Writer, results []fetch.Result) {
	for _, r := range results {
		if len(r.Events) == 0 && len(r.Skipped) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", r.Source.URL)
		for _, e := range r.Events {
			when := e.Start.In(houston).Format("Mon 2006-01-02 15:04")
			if e.AllDay {
				when = e.Start.In(houston).Format("Mon 2006-01-02") + " all day"
			}
			fmt.Fprintf(w, "  %-26s  %s\n", when, e.Title)
		}
		for _, err := range r.Skipped {
			fmt.Fprintf(w, "  skipped: %v\n", err)
		}
	}
}
