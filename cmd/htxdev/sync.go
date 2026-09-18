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

	"github.com/ileanmjr88/htxdev/internal/core"
	"github.com/ileanmjr88/htxdev/internal/fetch"
	"github.com/ileanmjr88/htxdev/internal/normalize"
	"github.com/ileanmjr88/htxdev/internal/registry"
	"github.com/ileanmjr88/htxdev/internal/store"
)

// Relative, because sync is meant to run from the repo root and Phase 7's
// workflow will do exactly that. The flag covers every other case.
const (
	defaultSourcesPath = "data/sources.yaml"

	// Relative for the same reason, and committed to git on purpose: it is the
	// permanent record of every event htxdev has ever seen, and first_seen
	// cannot be rebuilt from feeds because feeds forget.
	defaultDBPath = "htxdev.db"

	defaultRejectsPath = "data/rejects.yaml"
)

func runSync(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("htxdev sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sourcesPath := fs.String("sources", defaultSourcesPath, "path to the source registry")
	rejectsPath := fs.String("rejects", defaultRejectsPath, "path to the reject list")
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database")
	verbose := fs.Bool("v", false, "list every event fetched, not just the per-source summary")
	dryRun := fs.Bool("n", false, "fetch and report without writing to the database")

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

	// Loaded before anything is fetched, so a malformed reject list fails in a
	// second rather than after nine network round trips. A missing file is not
	// an error; the count is reported instead, so an absent filter is visible.
	rejects, err := registry.LoadRejectsFile(*rejectsPath)
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

	sum := report(stdout, reg, results, *verbose)

	// Writing is skippable on purpose. The database is a committed artifact,
	// so "let me look at what this run would do before it does it" is a real
	// question, and it is exactly the question somebody asks before filling in
	// a group's verified_by.
	if *dryRun {
		fmt.Fprintf(stdout, "\nDry run: nothing written to %s. %d rejects loaded.\n",
			*dbPath, rejects.Len())
		return verdict(sum)
	}

	stored, err := persist(ctx, *dbPath, reg, rejects, results, sum.fetchedAt)
	if err != nil {
		return err
	}
	reportStore(stdout, *dbPath, stored)

	return verdict(sum)
}

// persist mirrors the registry and writes everything the successful sources
// returned.
//
// Sources that failed contribute nothing, which is not the same as
// contributing an empty list: their events keep the last_seen they already
// had, so a feed being down for a day cannot look like every one of its events
// being cancelled. That is the first of the three guards in the
// absence-means-cancelled contract, and it is enforced by this loop rather
// than by anything in the store.
// stored is what one write did, from records in to rows on disk.
type stored struct {
	records  int // RawEvents handed to normalize
	events   int // what normalize concluded they were
	rejected int // events dropped by data/rejects.yaml
	listed   int // entries in the reject list, so an empty one is visible
	// Events carrying neither a venue nor an excerpt. See looksUnreviewed.
	unreviewed []core.Event
	problems   []error
	saved      store.SaveResult
	counts     store.Counts
}

// persist normalizes what the successful sources returned and writes it.
//
// Sources that failed contribute nothing, which is not the same as
// contributing an empty list: their events keep the last_seen they already
// had, so a feed being down for a day cannot look like every one of its events
// being cancelled. That is the first of the three guards in the
// absence-means-cancelled contract, and it is enforced by this loop rather
// than by anything downstream.
func persist(ctx context.Context, dbPath string, reg *registry.Registry, rejects *registry.Rejects, results []fetch.Result, seenAt time.Time) (stored, error) {
	var out stored

	st, err := store.Open(dbPath)
	if err != nil {
		return out, err
	}
	defer func() { _ = st.Close() }()

	// Before the events, always. Event rows reference source and group rows,
	// and a feed added to sources.yaml this morning has neither until this
	// runs.
	if err := st.SyncRegistry(ctx, reg.Groups, reg.Venues, reg.Sources); err != nil {
		return out, fmt.Errorf("sync registry into %s: %w", dbPath, err)
	}

	var raw []core.RawEvent
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		raw = append(raw, r.Events...)
	}
	out.records = len(raw)

	events, rejected, problems := normalize.New(reg, rejects).Events(raw)
	out.events, out.rejected, out.problems = len(events), rejected, problems
	out.listed = rejects.Len()

	out.unreviewed = looksUnreviewed(events)

	if out.saved, err = st.SaveEvents(ctx, events, seenAt); err != nil {
		return out, fmt.Errorf("save events to %s: %w", dbPath, err)
	}
	out.counts, err = st.Counts(ctx)
	return out, err
}

// looksUnreviewed returns events that carry neither a venue nor an excerpt.
//
// That is the shape a personal appointment takes on a shared group calendar.
// Reviewing all 110 events on the Houston Linux feed on 2026-09-18 found the
// pattern held across all 34 distinct titles: every private entry had neither,
// and every real group event had at least one. It caught "Dental", sitting in
// December outside the fetch window, which would have reached the site in
// October with nobody watching.
//
// Reported, never acted on. It is an observation about one calendar at one
// moment rather than a rule, and inferring "this is private" from "this is
// sparse" is exactly the guessing this project refuses elsewhere. A human
// decides, and data/rejects.yaml records the decision.
//
// Zero of the 74 events published today trip it, so noise is not the problem
// it would be if the threshold were looser. A legitimate event that trips it
// usually means the group needs a venue: line in sources.yaml, which is the
// same curation that fixes it for every future event.
func looksUnreviewed(events []core.Event) []core.Event {
	var out []core.Event
	for _, e := range events {
		if e.Venue.Name == "" && e.Excerpt == "" {
			out = append(out, e)
		}
	}
	return out
}

func reportStore(w io.Writer, dbPath string, st stored) {
	// The collapse is the headline. It is the only visible evidence that
	// dedupe did anything, and if it ever reads "76 records into 76 events"
	// then resolution has silently stopped matching organizers to groups.
	fmt.Fprintf(w, "\n%d records normalized into %d events", st.records, st.events)
	// records minus events is NOT the merge count once anything is rejected,
	// because a rejected event also disappears between the two numbers.
	// Reporting the difference said "3 merged" on a run with two merges and
	// one rejection, which is the kind of number somebody would later try to
	// reconcile against the database and fail to.
	if n := st.records - st.events - st.rejected; n > 0 {
		fmt.Fprintf(w, " (%d merged)", n)
	}
	fmt.Fprintln(w, ".")

	// Always printed, including when it is zero. A filter that quietly stopped
	// being loaded looks exactly like a filter with nothing in it, and only
	// one of those is a problem.
	fmt.Fprintf(w, "%d rejected by data/rejects.yaml (%d listed).\n", st.rejected, st.listed)

	for _, p := range st.problems {
		fmt.Fprintf(w, "  unattributable: %v\n", p)
	}

	// Loud on purpose, and above the row counts so it is not the last thing
	// scrolled past. This is the only warning in the pipeline about somebody's
	// private life reaching a public page.
	if n := len(st.unreviewed); n > 0 {
		fmt.Fprintf(w, "\n!! %d event(s) carry neither a venue nor a description, which is what a\n"+
			"!! personal appointment looks like on a shared group calendar. Check them:\n", n)
		for _, e := range st.unreviewed {
			fmt.Fprintf(w, "!!   %s  %s  [%s]\n",
				e.Start.In(houston).Format("2006-01-02 15:04"), e.Title, e.GroupSlug)
			fmt.Fprintf(w, "!!     %s\n", e.Fingerprint)
		}
		fmt.Fprintf(w, "!! If any is private, add its fingerprint to data/rejects.yaml.\n"+
			"!! If it is a real event, the group probably needs a venue: line in sources.yaml.\n")
	}

	fmt.Fprintf(w, "%s: %d new, %d updated", dbPath, st.saved.Inserted, st.saved.Updated)
	if st.saved.Promoted > 0 {
		fmt.Fprintf(w, ", %d promoted out of pending", st.saved.Promoted)
	}
	if st.saved.Merged > 0 {
		fmt.Fprintf(w, ", %d rows joined", st.saved.Merged)
	}
	fmt.Fprintf(w, ".\n%d rows: %d published, %d pending, %d cancelled.\n",
		st.counts.Total, st.counts.Published, st.counts.Pending, st.counts.Cancelled)
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
	sources  int
	ok       int
	events   int
	skipped  int
	verified int
	// Both come off the first successful Result rather than being recomputed.
	// fetchedAt is the instant the whole run shares, and storing that rather
	// than a fresh time.Now() is what keeps last_seen comparable with the
	// window the same run advertised.
	fetchedAt time.Time
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
				sum.fetchedAt = r.FetchedAt
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
// rawVenue renders what the feed said about where an event is, which is not
// what will be stored: normalize resolves "The Ion, Room 30, 4201 Main St…"
// and "Ion – Conference Room 030" to the same building. This listing is the
// "what did each feed actually send" view, and seeing the unresolved strings
// side by side is most of the reason to look at it.
func rawVenue(e core.RawEvent) string {
	switch len(e.Venues) {
	case 0:
		return ""
	case 1:
		return truncate(e.Venues[0].Name, 46)
	default:
		// Ion sends [room, building] and the order is the hierarchy, so the
		// last element is the building.
		return truncate(e.Venues[len(e.Venues)-1].Name+" / "+e.Venues[0].Name, 46)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "\u2026"
}

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
			fmt.Fprintf(w, "  %-26s  %-52s  %s\n", when, truncate(e.Title, 52), rawVenue(e))
		}
		for _, err := range r.Skipped {
			fmt.Fprintf(w, "  skipped: %v\n", err)
		}
	}
}
