package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
)

const (
	tribeFeed = "https://example.test/tribe"
	icsFeed   = "https://example.test/calendar.ics"
)

var (
	runOne = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	runTwo = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "htxdev.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

// seed mirrors a registry with one group and two feeds. verifiedBy is the
// whole pending gate: blank means nothing this group publishes ever reaches a
// reader.
func seed(t *testing.T, st *Store, verifiedBy string) {
	t.Helper()
	g := core.Group{Slug: "g1", Name: "Group One", URL: "https://example.test", Category: "dev", VerifiedBy: verifiedBy}
	if verifiedBy != "" {
		g.VerifiedAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	}
	srcs := []core.Source{
		{GroupSlug: "g1", Kind: core.KindTribe, URL: tribeFeed, Priority: 50, Enabled: true},
		{GroupSlug: "g1", Kind: core.KindICS, URL: icsFeed, Priority: 10, Enabled: true},
	}
	if err := st.SyncRegistry(t.Context(), []core.Group{g}, nil, srcs); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
}

// ev builds what normalize would have concluded: one event, from one feed.
func ev(feed, id, title string, start time.Time) core.Event {
	kind := core.KindTribe
	if feed == icsFeed {
		kind = core.KindICS
	}
	fp := core.Fingerprint(kind, id)
	return core.Event{
		Fingerprint: fp,
		GroupSlug:   "g1",
		Title:       title,
		Start:       start,
		Sources:     []core.EventSource{{SourceKey: feed, Fingerprint: fp}},
	}
}

// merged builds an event that two feeds both published, which is what dedupe
// produces and what event_fingerprints exists to record.
func merged(title string, start time.Time, a, b core.Event) core.Event {
	e := a
	e.Title = title
	e.Start = start
	e.Sources = append(append([]core.EventSource{}, a.Sources...), b.Sources...)
	return e
}

func save(t *testing.T, st *Store, at time.Time, events ...core.Event) SaveResult {
	t.Helper()
	res, err := st.SaveEvents(t.Context(), events, at)
	if err != nil {
		t.Fatalf("SaveEvents: %v", err)
	}
	return res
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "htxdev.db")
	for i := range 3 {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
}

// The journal mode decision, asserted rather than left as a comment. WAL is
// the usual advice for SQLite and it is wrong for a file that lives in git: it
// puts a -wal and a -shm next to the database, neither of which is in
// .gitignore, and it buys concurrency a twice-daily batch job does not want.
func TestOpenLeavesExactlyOneFileOnDisk(t *testing.T) {
	st, path := newStore(t)
	seed(t, st, "")
	save(t, st, runOne, ev(tribeFeed, "a", "One", runOne))

	var mode string
	if err := st.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want anything but wal for a committed database", mode)
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Errorf("%s exists; a committed database must be one file", path+suffix)
		}
	}
}

// SQLite has foreign keys OFF by default, and the schema is full of them. The
// pragma travels in the DSN so the driver reapplies it to every connection.
func TestForeignKeysAreEnforced(t *testing.T) {
	st, _ := newStore(t)

	var on int
	if err := st.db.QueryRow("PRAGMA foreign_keys").Scan(&on); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if on != 1 {
		t.Fatalf("foreign_keys = %d, want 1", on)
	}

	_, err := st.db.Exec(`INSERT INTO events
		(fingerprint, group_slug, title, starts_at, status, first_seen, last_seen)
		VALUES ('x', 'no-such-group', 't', '2026-01-01T00:00:00Z', 'pending',
		        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Error("inserting an event against a nonexistent group succeeded, want a constraint failure")
	}
}

// STRICT tables plus a CHECK. Without them SQLite's type affinity would accept
// a typo'd status and keep it, and every read afterwards would quietly filter
// the row out of existence.
func TestStatusIsConstrained(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	_, err := st.db.Exec(`INSERT INTO events
		(fingerprint, group_slug, title, starts_at, status, first_seen, last_seen)
		VALUES ('x', 'g1', 't', '2026-01-01T00:00:00Z',
		        'publshed', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Error("a misspelled status was accepted, want the CHECK constraint to reject it")
	}
}

func TestSaveEventsInsertsThenUpdates(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	events := []core.Event{
		ev(tribeFeed, "a", "One", runOne.Add(48*time.Hour)),
		ev(tribeFeed, "b", "Two", runOne.Add(72*time.Hour)),
	}

	first := save(t, st, runOne, events...)
	if first.Inserted != 2 || first.Updated != 0 {
		t.Fatalf("first sync = %+v, want 2 inserted", first)
	}

	second := save(t, st, runTwo, events...)
	if second.Inserted != 0 || second.Updated != 2 {
		t.Fatalf("second sync = %+v, want 2 updated and nothing inserted", second)
	}

	counts, err := st.Counts(t.Context())
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts.Total != 2 {
		t.Errorf("total = %d, want 2: re-running a sync must not duplicate rows", counts.Total)
	}
}

// The reason this database is committed rather than rebuilt. first_seen is the
// only fact in the row that cannot be recovered from a feed, because feeds
// only publish what is current.
func TestFirstSeenIsWriteOnceAndLastSeenAdvances(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")
	e := ev(tribeFeed, "a", "One", runOne.Add(48*time.Hour))

	save(t, st, runOne, e)

	// A later sync sees the event again, with the title edited upstream.
	e.Title = "One, renamed"
	save(t, st, runTwo, e)

	var first, last, title string
	if err := st.db.QueryRow(
		`SELECT first_seen, last_seen, title FROM events WHERE fingerprint = ?`,
		core.Fingerprint(core.KindTribe, "a")).Scan(&first, &last, &title); err != nil {
		t.Fatalf("query: %v", err)
	}

	if want := formatTime(runOne); first != want {
		t.Errorf("first_seen = %q, want it frozen at %q", first, want)
	}
	if want := formatTime(runTwo); last != want {
		t.Errorf("last_seen = %q, want it advanced to %q", last, want)
	}
	// Mutable fields still move; only the two write-once ones do not.
	if title != "One, renamed" {
		t.Errorf("title = %q, want the update to have landed", title)
	}
}

// D7: the fingerprint is namespaced by source kind and becomes a published ICS
// UID in v1.1, so the same upstream ID from two kinds must not collide.
func TestFingerprintNamespacesByKind(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	// Same upstream ID, different feeds.
	save(t, st, runOne,
		ev(tribeFeed, "shared-id", "From tribe", runOne.Add(48*time.Hour)),
		ev(icsFeed, "shared-id", "From ics", runOne.Add(48*time.Hour)),
	)

	counts, err := st.Counts(t.Context())
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts.Total != 2 {
		t.Fatalf("total = %d, want 2 distinct rows", counts.Total)
	}

	var fingerprints []string
	rows, err := st.db.Query(`SELECT fingerprint FROM events ORDER BY fingerprint`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			t.Fatal(err)
		}
		fingerprints = append(fingerprints, f)
	}
	want := []string{"ics:shared-id", "tribe:shared-id"}
	if len(fingerprints) != 2 || fingerprints[0] != want[0] || fingerprints[1] != want[1] {
		t.Errorf("fingerprints = %v, want %v", fingerprints, want)
	}
}

// The gate itself. Everything upstream can work perfectly and nothing reaches
// a reader until a human puts their handle in verified_by.
func TestPendingGate(t *testing.T) {
	cases := []struct {
		name       string
		verifiedBy string
		want       string
	}{
		{"unverified group stays pending", "", StatusPending},
		{"verified group publishes", "ileanmjr88", StatusPublished},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newStore(t)
			seed(t, st, tc.verifiedBy)
			save(t, st, runOne, ev(tribeFeed, "a", "One", runOne.Add(48*time.Hour)))

			var got string
			if err := st.db.QueryRow(`SELECT status FROM events`).Scan(&got); err != nil {
				t.Fatalf("query: %v", err)
			}
			if got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}

			up, err := st.Upcoming(t.Context(), runOne)
			if err != nil {
				t.Fatalf("Upcoming: %v", err)
			}
			wantVisible := tc.want == StatusPublished
			if got := len(up) > 0; got != wantVisible {
				t.Errorf("Upcoming returned %d events, want visible=%v", len(up), wantVisible)
			}
		})
	}
}

// The first sync after somebody fills in verified_by. Events already in the
// database come out of pending rather than having to be re-fetched.
func TestVerifyingAGroupPromotesItsExistingEvents(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	events := []core.Event{
		ev(tribeFeed, "a", "One", runOne.Add(48*time.Hour)),
		ev(tribeFeed, "b", "Two", runOne.Add(72*time.Hour)),
	}
	save(t, st, runOne, events...)

	counts, _ := st.Counts(t.Context())
	if counts.Pending != 2 || counts.Published != 0 {
		t.Fatalf("before verification: %+v, want 2 pending", counts)
	}

	// A human edits sources.yaml and the next sync mirrors it.
	seed(t, st, "ileanmjr88")
	res := save(t, st, runTwo, events...)

	if res.Promoted != 2 {
		t.Errorf("promoted = %d, want 2", res.Promoted)
	}
	counts, _ = st.Counts(t.Context())
	if counts.Published != 2 || counts.Pending != 0 {
		t.Errorf("after verification: %+v, want 2 published", counts)
	}

	// And promotion is reported once, not on every subsequent run.
	again := save(t, st, runTwo.Add(24*time.Hour), events...)
	if again.Promoted != 0 {
		t.Errorf("promoted = %d on the third run, want 0", again.Promoted)
	}
}

// Revoking a verification has to work too. It is a deliberate human act, and
// the only reason to do it is having found something that should not publish.
func TestUnverifyingAGroupTakesItsEventsBackOffline(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "ileanmjr88")
	e := ev(tribeFeed, "a", "One", runOne.Add(48*time.Hour))
	save(t, st, runOne, e)

	seed(t, st, "")
	save(t, st, runTwo, e)

	up, err := st.Upcoming(t.Context(), runOne)
	if err != nil {
		t.Fatalf("Upcoming: %v", err)
	}
	if len(up) != 0 {
		t.Errorf("Upcoming returned %d events after the group was unverified, want 0", len(up))
	}
}

// A cancelled event that reappears in a feed is a reschedule, not a ghost.
func TestReappearingEventComesBackOutOfCancelled(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "ileanmjr88")
	e := ev(tribeFeed, "a", "One", runOne.Add(48*time.Hour))
	save(t, st, runOne, e)

	// Phase 5 will do this properly, with guards. Here it just sets the state.
	if _, err := st.db.Exec(`UPDATE events SET status = ?`, StatusCancelled); err != nil {
		t.Fatal(err)
	}

	save(t, st, runTwo, e)

	counts, _ := st.Counts(t.Context())
	if counts.Published != 1 || counts.Cancelled != 0 {
		t.Errorf("counts = %+v, want the event revived as published", counts)
	}
}

func TestUpcomingOrdersAndFilters(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "ileanmjr88")

	past := runOne.Add(-48 * time.Hour)
	soon := runOne.Add(24 * time.Hour)
	later := runOne.Add(240 * time.Hour)

	// Saved out of order on purpose: the ORDER BY has to do the work.
	save(t, st, runOne,
		ev(tribeFeed, "c", "Later", later),
		ev(tribeFeed, "a", "Past", past),
		ev(tribeFeed, "b", "Soon", soon),
	)

	up, err := st.Upcoming(t.Context(), runOne)
	if err != nil {
		t.Fatalf("Upcoming: %v", err)
	}
	if len(up) != 2 {
		t.Fatalf("got %d events, want 2: the past one is filtered out", len(up))
	}
	if up[0].Title != "Soon" || up[1].Title != "Later" {
		t.Errorf("order = %q, %q; want Soon then Later", up[0].Title, up[1].Title)
	}
	if up[0].GroupSlug != "g1" {
		t.Errorf("GroupSlug = %q, want it joined from the source", up[0].GroupSlug)
	}
	if up[0].Fingerprint != core.Fingerprint(core.KindTribe, "b") {
		t.Errorf("Fingerprint = %q", up[0].Fingerprint)
	}
	if !up[0].FirstSeen.Equal(runOne) || !up[0].LastSeen.Equal(runOne) {
		t.Errorf("FirstSeen/LastSeen = %s / %s, want both %s", up[0].FirstSeen, up[0].LastSeen, runOne)
	}
}

// An absent end time is a real state, and it has to survive the round trip as
// a zero Time rather than as year 1.
func TestZeroEndTimeRoundTrips(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "ileanmjr88")

	withEnd := ev(tribeFeed, "a", "Has an end", runOne.Add(24*time.Hour))
	withEnd.End = runOne.Add(26 * time.Hour)
	save(t, st, runOne, withEnd, ev(tribeFeed, "b", "No end", runOne.Add(48*time.Hour)))

	up, err := st.Upcoming(t.Context(), runOne)
	if err != nil {
		t.Fatalf("Upcoming: %v", err)
	}
	if len(up) != 2 {
		t.Fatalf("got %d events, want 2", len(up))
	}
	if !up[0].End.Equal(withEnd.End) {
		t.Errorf("End = %s, want %s", up[0].End, withEnd.End)
	}
	if !up[1].End.IsZero() {
		t.Errorf("End = %s, want the zero time for an event with no DTEND", up[1].End)
	}
}

// Delete-then-insert, so an event whose category list shrank does not keep
// the old rows.
func TestCategoriesAreReplacedNotAppended(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	e := ev(tribeFeed, "a", "One", runOne.Add(24*time.Hour))
	e.Categories = []string{"dev", "startup"}
	save(t, st, runOne, e)

	e.Categories = []string{"hardware"}
	save(t, st, runTwo, e)

	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM event_categories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("got %d category rows, want 1", n)
	}
	var name string
	if err := st.db.QueryRow(`SELECT name FROM event_categories WHERE position = 0`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "hardware" {
		t.Errorf("category = %q, want the new list and not the old one", name)
	}
}

// A venue nobody curated still gets a row, because the architecture expects
// venues to be discovered from event data.
func TestDiscoveredVenuesGetRows(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "ileanmjr88")

	a := ev(tribeFeed, "a", "One", runOne.Add(24*time.Hour))
	a.Venue = core.Venue{Name: "Zion Lutheran Church", Address: "3606 Beauchamp Blvd", City: "Houston"}
	b := ev(tribeFeed, "b", "Two", runOne.Add(48*time.Hour))
	b.Venue, b.Room = core.Venue{Name: "Zion Lutheran Church"}, "Fellowship Hall"
	save(t, st, runOne, a, b)

	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM venues WHERE name = ?`, "Zion Lutheran Church").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d venue rows, want 1: two events at one place is one venue", n)
	}

	up, err := st.Upcoming(t.Context(), runOne)
	if err != nil {
		t.Fatalf("Upcoming: %v", err)
	}
	if len(up) != 2 {
		t.Fatalf("got %d events, want 2", len(up))
	}
	if up[0].Venue.ID == 0 || up[0].Venue.ID != up[1].Venue.ID {
		t.Errorf("venue ids = %d and %d, want one shared non-zero id", up[0].Venue.ID, up[1].Venue.ID)
	}
	// The address the first event supplied is on the row, and the second
	// event supplying none did not blank it.
	if up[0].Venue.Address != "3606 Beauchamp Blvd" || up[1].Venue.Address != "3606 Beauchamp Blvd" {
		t.Errorf("addresses = %q and %q, want both kept from the first sighting",
			up[0].Venue.Address, up[1].Venue.Address)
	}
	if up[1].Room != "Fellowship Hall" {
		t.Errorf("room = %q, want it kept per event rather than on the venue", up[1].Room)
	}
}

// Deleting a source takes its provenance rows with it, which is what
// ON DELETE CASCADE is for and only works because foreign keys are on. The
// event itself survives: a feed going away is not the event going away, and
// another feed may still be publishing it.
func TestDeletingASourceCascadesToProvenance(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")
	save(t, st, runOne, ev(tribeFeed, "a", "One", runOne.Add(24*time.Hour)))

	if _, err := st.db.Exec(`DELETE FROM sources WHERE url = ?`, tribeFeed); err != nil {
		t.Fatal(err)
	}

	var events, provenance int
	if err := st.db.QueryRow(
		`SELECT (SELECT COUNT(*) FROM events), (SELECT COUNT(*) FROM event_fingerprints)`).
		Scan(&events, &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance != 0 {
		t.Errorf("got %d provenance rows, want 0", provenance)
	}
	if events != 1 {
		t.Errorf("got %d events, want the event to outlive the feed", events)
	}
}

func TestSaveEventsRejectsAnUnknownSource(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	_, err := st.SaveEvents(t.Context(),
		[]core.Event{ev("https://never-registered.test/feed", "a", "One", runOne)}, runOne)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "never-registered.test") {
		t.Errorf("err = %v, want it to name the unregistered feed", err)
	}
}

func TestSaveEventsRejectsAZeroTimestamp(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	_, err := st.SaveEvents(t.Context(), []core.Event{ev(tribeFeed, "a", "One", runOne)}, time.Time{})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "seenAt") {
		t.Errorf("err = %v, want it to name the zero timestamp", err)
	}
}

// One transaction per call. The file is committed to git, so a half-written
// sync is a half-written sync that gets pushed.
func TestSaveEventsIsAtomic(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	// Good, good, then one referencing a source that does not exist.
	_, err := st.SaveEvents(t.Context(), []core.Event{
		ev(tribeFeed, "a", "One", runOne.Add(24*time.Hour)),
		ev(tribeFeed, "b", "Two", runOne.Add(48*time.Hour)),
		ev("https://never-registered.test/feed", "c", "Three", runOne.Add(72*time.Hour)),
	}, runOne)
	if err == nil {
		t.Fatal("want an error, got nil")
	}

	counts, err := st.Counts(t.Context())
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts.Total != 0 {
		t.Errorf("%d rows written, want 0: the two good events before the failure must roll back", counts.Total)
	}
}

func TestSyncRegistryIsIdempotent(t *testing.T) {
	st, _ := newStore(t)
	for range 3 {
		seed(t, st, "")
	}

	var groups, sources int
	if err := st.db.QueryRow(`SELECT (SELECT COUNT(*) FROM groups), (SELECT COUNT(*) FROM sources)`).
		Scan(&groups, &sources); err != nil {
		t.Fatal(err)
	}
	if groups != 1 || sources != 2 {
		t.Errorf("groups=%d sources=%d after three syncs; want 1 and 2", groups, sources)
	}
}

// Source ids have to survive a re-sync. They are what event rows point at, and
// minting a new one per run would re-attribute every historical event.
func TestSyncRegistryKeepsSourceIDsStable(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	var before int64
	if err := st.db.QueryRow(`SELECT id FROM sources WHERE url = ?`, tribeFeed).Scan(&before); err != nil {
		t.Fatal(err)
	}

	seed(t, st, "ileanmjr88") // a real edit to the same registry

	var after int64
	if err := st.db.QueryRow(`SELECT id FROM sources WHERE url = ?`, tribeFeed).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("source id changed from %d to %d across a re-sync", before, after)
	}
}

func TestSaveEventsWithNothingToSave(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	res, err := st.SaveEvents(t.Context(), nil, runOne)
	if err != nil {
		t.Fatalf("SaveEvents: %v", err)
	}
	if res != (SaveResult{}) {
		t.Errorf("res = %+v, want the zero value", res)
	}
}

func TestFingerprint(t *testing.T) {
	cases := []struct {
		kind core.SourceKind
		id   string
		want string
	}{
		{core.KindTribe, "iondistrict.com?id=23861", "tribe:iondistrict.com?id=23861"},
		{core.KindICS, "1dq0opq36iu6a564pmlq9hjj6r@google.com", "ics:1dq0opq36iu6a564pmlq9hjj6r@google.com"},
		{core.KindICS, "event_315949333@meetup.com", "ics:event_315949333@meetup.com"},
	}
	for _, tc := range cases {
		if got := core.Fingerprint(tc.kind, tc.id); got != tc.want {
			t.Errorf("Fingerprint(%q, %q) = %q, want %q", tc.kind, tc.id, got, tc.want)
		}
	}
	// It becomes an ICS UID verbatim in v1.1, where lines fold at 75 octets.
	for _, tc := range cases {
		if got := core.Fingerprint(tc.kind, tc.id); len(got) > 70 {
			t.Errorf("Fingerprint = %q is %d bytes, too long to be an ICS UID comfortably", got, len(got))
		}
	}
}

func TestParseAndFormatTimeRoundTrip(t *testing.T) {
	for _, tc := range []time.Time{{}, runOne, time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC)} {
		got, err := parseTime(formatTime(tc))
		if err != nil {
			t.Fatalf("parseTime(formatTime(%s)): %v", tc, err)
		}
		if !got.Equal(tc) {
			t.Errorf("round trip of %s gave %s", tc, got)
		}
	}
	if formatTime(time.Time{}) != "" {
		t.Errorf("a zero time must format as empty, not as year 1")
	}
}

// sources.yaml is the authoritative copy, so an edit to it has to reach the
// mirror. Row identity is keyed on the URL and stays put; everything else
// about the source is whatever the file says now.
func TestSyncRegistryUpdatesMutableSourceFields(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	edited := []core.Source{
		{GroupSlug: "g1", Kind: core.KindTribe, URL: tribeFeed, Priority: 10, Enabled: false},
		{GroupSlug: "g1", Kind: core.KindICS, URL: icsFeed, Priority: 10, Enabled: true},
	}
	g := core.Group{Slug: "g1", Name: "Group One Renamed"}
	if err := st.SyncRegistry(t.Context(), []core.Group{g}, nil, edited); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}

	var (
		priority int
		enabled  int
	)
	if err := st.db.QueryRow(`SELECT priority, enabled FROM sources WHERE url = ?`, tribeFeed).
		Scan(&priority, &enabled); err != nil {
		t.Fatal(err)
	}
	if priority != 10 {
		t.Errorf("priority = %d, want 10: a re-sync has to carry the edit through", priority)
	}
	if enabled != 0 {
		t.Errorf("enabled = %d, want 0", enabled)
	}

	var name string
	if err := st.db.QueryRow(`SELECT name FROM groups WHERE slug = 'g1'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Group One Renamed" {
		t.Errorf("group name = %q, want the edit", name)
	}
}

// SQLite has no boolean type, so these are integers, and an inverted
// conversion is invisible until something renders an all-day event at
// midnight or hides a virtual one.
func TestBooleanColumnsRoundTrip(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "ileanmjr88")

	allDay := ev(tribeFeed, "a", "All day", runOne.Add(24*time.Hour))
	allDay.AllDay = true
	allDay.Virtual = true

	plain := ev(tribeFeed, "b", "Ordinary", runOne.Add(48*time.Hour))

	save(t, st, runOne, allDay, plain)

	up, err := st.Upcoming(t.Context(), runOne)
	if err != nil {
		t.Fatalf("Upcoming: %v", err)
	}
	if len(up) != 2 {
		t.Fatalf("got %d events, want 2", len(up))
	}
	if !up[0].AllDay || !up[0].Virtual {
		t.Errorf("first event allDay=%v virtual=%v, want both true", up[0].AllDay, up[0].Virtual)
	}
	if up[1].AllDay || up[1].Virtual {
		t.Errorf("second event allDay=%v virtual=%v, want both false", up[1].AllDay, up[1].Virtual)
	}
}

// An event is found by ANY of its fingerprints, which is what lets
// events.fingerprint stay write-once while the merge winner is free to change.
func TestEventIsFoundByAnyOfItsFingerprints(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	start := runOne.Add(24 * time.Hour)
	fromIon := ev(tribeFeed, "ion-copy", "Ion's wording", start)
	fromHLUG := ev(icsFeed, "own-copy", "The organizer's wording", start)

	// First sync: only Ion carries it.
	save(t, st, runOne, fromIon)

	// Second sync: HLUG starts publishing it too, and normalize now hands over
	// one event built from both records, with HLUG's record winning.
	both := merged("The organizer's wording", start, fromHLUG, fromIon)
	res := save(t, st, runTwo, both)

	if res.Inserted != 0 || res.Updated != 1 {
		t.Fatalf("res = %+v, want the existing row matched through Ion's fingerprint", res)
	}

	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d event rows, want 1", n)
	}

	var fingerprint, title, firstSeen string
	if err := st.db.QueryRow(`SELECT fingerprint, title, first_seen FROM events`).
		Scan(&fingerprint, &title, &firstSeen); err != nil {
		t.Fatal(err)
	}
	// D7: identity is whichever record was seen FIRST, not the current merge
	// winner. Following the winner would churn a published ICS UID.
	if want := core.Fingerprint(core.KindTribe, "ion-copy"); fingerprint != want {
		t.Errorf("fingerprint = %q, want %q kept from the first sync", fingerprint, want)
	}
	if title != "The organizer's wording" {
		t.Errorf("title = %q, want the new winner's", title)
	}
	if firstSeen != formatTime(runOne) {
		t.Errorf("first_seen = %q, want it frozen at the first sync", firstSeen)
	}

	// Both records are now provenance for the one event.
	var provenance int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM event_fingerprints`).Scan(&provenance); err != nil {
		t.Fatal(err)
	}
	if provenance != 2 {
		t.Errorf("got %d provenance rows, want 2", provenance)
	}
}

// Two rows becoming one. A feed record can live as its own event for months
// before a second feed starts publishing the same meeting, and that run is
// where the rows join.
func TestTwoExistingRowsMergeIntoOne(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	start := runOne.Add(24 * time.Hour)
	fromIon := ev(tribeFeed, "ion-copy", "Ion's wording", start)
	fromHLUG := ev(icsFeed, "own-copy", "The organizer's wording", start)

	// Both exist separately: normalize did not yet know they were the same,
	// which is what an alias being missing from sources.yaml looks like.
	save(t, st, runOne, fromIon)
	save(t, st, runOne.Add(time.Hour), fromHLUG)

	var before int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 2 {
		t.Fatalf("got %d rows before the merge, want 2", before)
	}

	// The alias gets added, and now one event claims both fingerprints.
	res := save(t, st, runTwo, merged("The organizer's wording", start, fromHLUG, fromIon))
	if res.Merged != 1 {
		t.Errorf("merged = %d, want 1", res.Merged)
	}

	var after int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 1 {
		t.Fatalf("got %d rows after the merge, want 1", after)
	}

	// The survivor is the one with the earliest first_seen, because that is
	// the fact the committed database exists to preserve.
	var fingerprint, firstSeen string
	if err := st.db.QueryRow(`SELECT fingerprint, first_seen FROM events`).Scan(&fingerprint, &firstSeen); err != nil {
		t.Fatal(err)
	}
	if want := core.Fingerprint(core.KindTribe, "ion-copy"); fingerprint != want {
		t.Errorf("survivor = %q, want the older row %q", fingerprint, want)
	}
	if firstSeen != formatTime(runOne) {
		t.Errorf("first_seen = %q, want the earlier of the two", firstSeen)
	}

	// Nothing lost its provenance in the move.
	var provenance int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM event_fingerprints`).Scan(&provenance); err != nil {
		t.Fatal(err)
	}
	if provenance != 2 {
		t.Errorf("got %d provenance rows, want both repointed at the survivor", provenance)
	}
}

// Upcoming returns provenance, because "which feeds say this is happening" is
// a real question and the only evidence dedupe did anything.
func TestUpcomingCarriesProvenanceAndCategories(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "ileanmjr88")

	start := runOne.Add(24 * time.Hour)
	a := ev(icsFeed, "own", "Merged event", start)
	b := ev(tribeFeed, "ion", "Ion's copy", start)
	e := merged("Merged event", start, a, b)
	e.Categories = []string{"dev"}
	e.Venue, e.Room = core.Venue{Name: "Ion"}, "Conference Room 030"
	save(t, st, runOne, e)

	up, err := st.Upcoming(t.Context(), runOne)
	if err != nil {
		t.Fatalf("Upcoming: %v", err)
	}
	if len(up) != 1 {
		t.Fatalf("got %d events, want 1", len(up))
	}
	got := up[0]
	if len(got.Sources) != 2 {
		t.Errorf("sources = %+v, want both feeds", got.Sources)
	}
	if len(got.Categories) != 1 || got.Categories[0] != "dev" {
		t.Errorf("categories = %v, want [dev]", got.Categories)
	}
	if got.Venue.Name != "Ion" || got.Room != "Conference Room 030" || got.Venue.ID == 0 {
		t.Errorf("venue = (%d, %q, %q), want a resolved id and the room",
			got.Venue.ID, got.Venue.Name, got.Room)
	}
}
