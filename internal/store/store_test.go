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

func ev(feed, id, title string, start time.Time) core.RawEvent {
	return core.RawEvent{SourceKey: feed, UpstreamID: id, Title: title, Start: start}
}

func save(t *testing.T, st *Store, at time.Time, events ...core.RawEvent) SaveResult {
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
		(fingerprint, source_id, title, starts_at, status, first_seen, last_seen)
		VALUES ('x', 999, 't', '2026-01-01T00:00:00Z', 'pending', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Error("inserting an event against a nonexistent source succeeded, want a constraint failure")
	}
}

// STRICT tables plus a CHECK. Without them SQLite's type affinity would accept
// a typo'd status and keep it, and every read afterwards would quietly filter
// the row out of existence.
func TestStatusIsConstrained(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	_, err := st.db.Exec(`INSERT INTO events
		(fingerprint, source_id, title, starts_at, status, first_seen, last_seen)
		VALUES ('x', (SELECT id FROM sources LIMIT 1), 't', '2026-01-01T00:00:00Z',
		        'publshed', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err == nil {
		t.Error("a misspelled status was accepted, want the CHECK constraint to reject it")
	}
}

func TestSaveEventsInsertsThenUpdates(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	events := []core.RawEvent{
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

	events := []core.RawEvent{
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

// Delete-then-insert, so an event whose venue list shrank does not keep the
// old rows. Ion sends [room, building] and the order is the hierarchy, so
// position has to be rewritten too.
func TestChildRowsAreReplacedNotAppended(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	e := ev(tribeFeed, "a", "One", runOne.Add(24*time.Hour))
	e.Venues = []core.RawVenue{{Name: "Room 028"}, {Name: "Ion"}}
	e.Organizers = []core.RawOrganizer{{Name: "HLUG"}}
	e.Categories = []string{"dev", "startup"}
	save(t, st, runOne, e)

	// Upstream trims the list.
	e.Venues = []core.RawVenue{{Name: "Ion"}}
	e.Organizers = nil
	e.Categories = []string{"dev"}
	save(t, st, runTwo, e)

	var venues, organizers, categories int
	if err := st.db.QueryRow(`
		SELECT (SELECT COUNT(*) FROM event_venues),
		       (SELECT COUNT(*) FROM event_organizers),
		       (SELECT COUNT(*) FROM event_categories)`).Scan(&venues, &organizers, &categories); err != nil {
		t.Fatal(err)
	}
	if venues != 1 || organizers != 0 || categories != 1 {
		t.Errorf("venues=%d organizers=%d categories=%d; want 1, 0, 1", venues, organizers, categories)
	}

	var name string
	if err := st.db.QueryRow(`SELECT name FROM event_venues WHERE position = 0`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Ion" {
		t.Errorf("venue at position 0 = %q, want the new list and not the old one", name)
	}
}

// Deleting a source takes its events with it, which is what ON DELETE CASCADE
// is for and only works because foreign keys are on.
func TestDeletingASourceCascades(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")
	e := ev(tribeFeed, "a", "One", runOne.Add(24*time.Hour))
	e.Venues = []core.RawVenue{{Name: "Ion"}}
	save(t, st, runOne, e)

	if _, err := st.db.Exec(`DELETE FROM sources WHERE url = ?`, tribeFeed); err != nil {
		t.Fatal(err)
	}

	var events, venues int
	if err := st.db.QueryRow(`SELECT (SELECT COUNT(*) FROM events), (SELECT COUNT(*) FROM event_venues)`).
		Scan(&events, &venues); err != nil {
		t.Fatal(err)
	}
	if events != 0 || venues != 0 {
		t.Errorf("events=%d venues=%d after deleting the source; want both 0", events, venues)
	}
}

func TestSaveEventsRejectsAnUnknownSource(t *testing.T) {
	st, _ := newStore(t)
	seed(t, st, "")

	_, err := st.SaveEvents(t.Context(),
		[]core.RawEvent{ev("https://never-registered.test/feed", "a", "One", runOne)}, runOne)
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

	_, err := st.SaveEvents(t.Context(), []core.RawEvent{ev(tribeFeed, "a", "One", runOne)}, time.Time{})
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
	_, err := st.SaveEvents(t.Context(), []core.RawEvent{
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
	allDay.VirtualURL = "https://meet.example.test/x"

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
