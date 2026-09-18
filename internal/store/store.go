// Package store is htxdev's SQLite persistence layer. It owns the schema, the
// upsert, and the pending gate, and it imports only internal/core: the
// registry, the fetch layer and the decoders are all upstream of it and none
// of them are named here.
//
// The database file is a committed git artifact, not scratch space. That is a
// deliberate decision with measured numbers behind it (roughly 30 MB of repo
// per year at ten sources), and it is what makes first_seen recoverable at
// all, since feeds forget. Two rules keep it cheap and both live outside this
// package: never VACUUM in CI, because rewriting the whole file defeats git's
// delta compression, and give the sync workflow a concurrency group, because
// two overlapping runs produce a binary merge conflict nobody can resolve.
package store

import (
	"cmp"
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"

	// Pure Go, no cgo, which is what CGO_ENABLED=0 in the Makefile commits us
	// to. The common alternative, mattn/go-sqlite3, will not build under it.
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// Timestamps are RFC3339 in UTC. See the header comment in schema.sql for why
// text rather than Unix integers.
const timeLayout = time.RFC3339

// An event's lifecycle. pending is the default for anything from a group no
// human has signed for, and the only status Upcoming will return is published.
const (
	StatusPending   = "pending"
	StatusPublished = "published"
	StatusCancelled = "cancelled"
)

type Store struct{ db *sql.DB }

// Open opens or creates the database at path and applies the schema.
func Open(path string) (*Store, error) {
	// The pragmas go in the DSN rather than being executed after opening,
	// because the driver applies them to every connection it makes and
	// foreign_keys is a per-connection setting that SQLite has OFF by default.
	// A PRAGMA run once against the pool would be silently lost the moment a
	// connection was replaced, and the foreign keys in this schema would stop
	// being enforced without anything failing.
	//
	// Note what is absent: journal_mode is left at SQLite's default rather
	// than being set to WAL. WAL is the usual advice and it is wrong here.
	// This file is committed to git, WAL adds a -wal and a -shm beside it, and
	// the concurrency it buys is worthless to a single-writer batch job that
	// runs twice a day.
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// One connection. SQLite takes a single writer lock for the whole
	// database, so a pool of connections here does not buy throughput, it
	// buys SQLITE_BUSY errors between goroutines of the same program.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema to %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SyncRegistry mirrors the human-edited registry into the database.
//
// It takes slices rather than a *registry.Registry so this package depends on
// core alone. That is not ceremony: it is what lets a test build a two-group
// registry inline without a YAML file, and it keeps the direction of the
// import graph obvious.
//
// Rows are never deleted here. A group removed from sources.yaml stops being
// fetched, because EnabledSources no longer returns it, but its history stays,
// which is the entire point of a permanent record. Deactivating is an absence
// upstream, not a DELETE down here.
func (s *Store) SyncRegistry(ctx context.Context, groups []core.Group, venues []core.Venue, sources []core.Source) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit has succeeded

	for _, g := range groups {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO groups (slug, name, url, category, verified_by, verified_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(slug) DO UPDATE SET
				name = excluded.name, url = excluded.url, category = excluded.category,
				verified_by = excluded.verified_by, verified_at = excluded.verified_at`,
			g.Slug, g.Name, g.URL, g.Category, g.VerifiedBy, formatTime(g.VerifiedAt)); err != nil {
			return fmt.Errorf("group %s: %w", g.Slug, err)
		}
	}

	for _, v := range venues {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO venues (slug, name, address, city, state, zip, url)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(slug) DO UPDATE SET
				name = excluded.name, address = excluded.address, city = excluded.city,
				state = excluded.state, zip = excluded.zip, url = excluded.url`,
			v.Slug, v.Name, v.Address, v.City, v.State, v.Zip, v.URL); err != nil {
			return fmt.Errorf("venue %s: %w", v.Slug, err)
		}
	}

	for _, src := range sources {
		// Conflict on url, not on id: the URL is the source's identity
		// everywhere upstream of this database, per D14, and the integer id is
		// this table's answer to "what does that URL mean here". Re-running a
		// sync must not mint a second row for the same feed, or every event
		// would be re-attributed on the next run.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sources (group_slug, kind, url, priority, enabled)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(url) DO UPDATE SET
				group_slug = excluded.group_slug, kind = excluded.kind,
				priority = excluded.priority, enabled = excluded.enabled`,
			src.GroupSlug, string(src.Kind), src.URL, src.Priority, boolToInt(src.Enabled)); err != nil {
			return fmt.Errorf("source %s: %w", src.URL, err)
		}
	}

	return tx.Commit()
}

// SaveResult reports what one call changed, so a sync can say something more
// useful than that it finished.
type SaveResult struct {
	Inserted int
	Updated  int
	// Promoted counts rows that were pending and are now published, which
	// happens on the first sync after somebody fills in a group's verified_by.
	Promoted int
	// Merged counts rows that were separate events and have now been joined,
	// which happens when a second feed starts publishing something a first
	// feed already carried.
	Merged int
}

// SaveEvents writes what normalize concluded.
//
// Everything happens in one transaction. A sync that dies half way through
// leaves the database exactly as it was, which matters more than usual here:
// the file is a git artifact, so a partial write is a partial write that gets
// pushed. It also means last_seen either advances for every event in the run
// or for none of them, and cancellation reads last_seen.
//
// Nothing is deleted and nothing is cancelled. An event that has stopped
// appearing simply stops having its last_seen bumped, and deciding what that
// means needs the guards in the absence-means-cancelled contract, which need
// information this function does not have.
func (s *Store) SaveEvents(ctx context.Context, events []core.Event, seenAt time.Time) (SaveResult, error) {
	var res SaveResult
	if len(events) == 0 {
		return res, nil
	}
	// A zero seenAt would write empty first_seen and last_seen strings, which
	// satisfy NOT NULL and are useless: cancellation compares last_seen, and
	// "" compares before every real timestamp. Caller error, so say so rather
	// than quietly substituting time.Now and hiding where it came from.
	if seenAt.IsZero() {
		return res, errors.New("seenAt is zero; pass the instant the fetch run shared")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	sources, err := loadSources(ctx, tx)
	if err != nil {
		return res, err
	}
	verified, err := loadVerifiedGroups(ctx, tx)
	if err != nil {
		return res, err
	}

	stamp := formatTime(seenAt)

	for _, e := range events {
		if len(e.Sources) == 0 {
			return res, fmt.Errorf("event %q has no sources", e.Fingerprint)
		}

		// The pending gate. An event is publishable only because a human
		// filled in verified_by for its group, and this is where that
		// decision turns into a column.
		status := StatusPending
		if verified[e.GroupSlug] {
			status = StatusPublished
		}

		fingerprints := make([]string, 0, len(e.Sources))
		for _, src := range e.Sources {
			if _, ok := sources[src.SourceKey]; !ok {
				return res, fmt.Errorf("event %q: no source row for %q; sync the registry first",
					e.Fingerprint, src.SourceKey)
			}
			fingerprints = append(fingerprints, src.Fingerprint)
		}

		id, oldStatus, existed, merged, err := claim(ctx, tx, fingerprints)
		if err != nil {
			return res, fmt.Errorf("event %q: %w", e.Fingerprint, err)
		}
		res.Merged += merged

		venueID, err := venueRow(ctx, tx, e.Venue)
		if err != nil {
			return res, fmt.Errorf("event %q: %w", e.Fingerprint, err)
		}

		if !existed {
			// first_seen and last_seen are the same value on the first sync,
			// and their being equal is exactly what "never seen before now"
			// means.
			r, err := tx.ExecContext(ctx, `
				INSERT INTO events (fingerprint, group_slug, title, excerpt, starts_at, ends_at,
					all_day, venue_id, venue_name, room, url, register_url, virtual,
					status, first_seen, last_seen)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				e.Fingerprint, e.GroupSlug, e.Title, e.Excerpt, formatTime(e.Start), formatTime(e.End),
				boolToInt(e.AllDay), venueID, e.Venue.Name, e.Room, e.URL, e.RegisterURL,
				boolToInt(e.Virtual), status, stamp, stamp)
			if err != nil {
				return res, fmt.Errorf("insert %s: %w", e.Fingerprint, err)
			}
			if id, err = r.LastInsertId(); err != nil {
				return res, err
			}
			res.Inserted++
		} else {
			// fingerprint and first_seen are absent from the SET list on
			// purpose. Both are write-once per D7: the fingerprint becomes a
			// published ICS UID, and first_seen is the one fact in this row
			// that cannot be rebuilt from a feed.
			//
			// A row that was cancelled and has reappeared comes back to life
			// here, which is what a rescheduled event looks like from outside.
			if _, err := tx.ExecContext(ctx, `
				UPDATE events SET
					group_slug = ?, title = ?, excerpt = ?, starts_at = ?, ends_at = ?,
					all_day = ?, venue_id = ?, venue_name = ?, room = ?, url = ?,
					register_url = ?, virtual = ?, status = ?, last_seen = ?
				WHERE id = ?`,
				e.GroupSlug, e.Title, e.Excerpt, formatTime(e.Start), formatTime(e.End),
				boolToInt(e.AllDay), venueID, e.Venue.Name, e.Room, e.URL, e.RegisterURL,
				boolToInt(e.Virtual), status, stamp, id); err != nil {
				return res, fmt.Errorf("update %s: %w", e.Fingerprint, err)
			}
			res.Updated++
			if oldStatus == StatusPending && status == StatusPublished {
				res.Promoted++
			}
		}

		if err := replaceProvenance(ctx, tx, id, e, sources); err != nil {
			return res, fmt.Errorf("provenance of %s: %w", e.Fingerprint, err)
		}
		if err := replaceCategories(ctx, tx, id, e.Categories); err != nil {
			return res, fmt.Errorf("categories of %s: %w", e.Fingerprint, err)
		}
	}

	return res, tx.Commit()
}

// claim finds the event these fingerprints belong to, merging rows if they
// turn out to name more than one.
//
// More than one is a real state, not a corruption. A feed record can exist as
// its own event for months before a second feed starts publishing the same
// meeting, and the run where that happens is the run where two rows become
// one. The survivor is the one with the earliest first_seen, because that is
// the fact the whole committed database exists to preserve, and the losers'
// fingerprints are repointed at it so nothing loses its provenance.
func claim(ctx context.Context, tx *sql.Tx, fingerprints []string) (id int64, status string, existed bool, merged int, err error) {
	type row struct {
		id        int64
		status    string
		firstSeen string
	}
	var rows []row
	seen := map[int64]bool{}

	for _, fp := range fingerprints {
		var r row
		err := tx.QueryRowContext(ctx, `
			SELECT e.id, e.status, e.first_seen
			FROM events e JOIN event_fingerprints f ON f.event_id = e.id
			WHERE f.fingerprint = ?`, fp).Scan(&r.id, &r.status, &r.firstSeen)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return 0, "", false, 0, fmt.Errorf("look up %s: %w", fp, err)
		}
		if !seen[r.id] {
			seen[r.id] = true
			rows = append(rows, r)
		}
	}

	if len(rows) == 0 {
		return 0, "", false, 0, nil
	}

	// Earliest first_seen wins. RFC3339 in UTC sorts correctly as a string,
	// which is one of the reasons the column is text.
	slices.SortStableFunc(rows, func(a, b row) int { return cmp.Compare(a.firstSeen, b.firstSeen) })
	survivor := rows[0]

	// The losing rows go, and ON DELETE CASCADE takes their provenance with
	// them. There was an UPDATE here first, repointing their fingerprints at
	// the survivor, until mutation testing neutered it and nothing failed:
	// replaceProvenance rewrites the survivor's fingerprints from e.Sources
	// moments later, so the repoint was overwritten every time. Redundant
	// rather than untested, so it is gone.
	//
	// Nothing is lost by that. A fingerprint on a losing row and absent from
	// e.Sources is a record that has stopped merging into this event, and it
	// should stop pointing at it.
	for _, loser := range rows[1:] {
		if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id = ?`, loser.id); err != nil {
			return 0, "", false, 0, fmt.Errorf("delete merged row: %w", err)
		}
		merged++
	}

	return survivor.id, survivor.status, true, merged, nil
}

// venueRow maps a resolved venue onto a row, creating one for a venue
// discovered from event data and filling in anything the curated row is
// missing.
//
// Discovery is the normal case, not the exception: sources.yaml curates a
// venue only when its name needs canonicalising or it appears under several
// spellings. "Sesh Coworking" and "Greentown Labs" arrive with no curation
// behind them and still have to be somewhere, with whatever address their feed
// supplied.
func venueRow(ctx context.Context, tx *sql.Tx, v core.Venue) (int64, error) {
	if v.Name == "" {
		return 0, nil
	}

	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM venues WHERE name = ?`, v.Name).Scan(&id)
	switch {
	case err == nil:
		// Fill in blanks only. A curated venue's address is the canonical one
		// and must survive contact with the feeds: Ion's own payload spells
		// its street three ways across five rooms and sometimes omits the
		// state or the zip, so letting a feed overwrite sources.yaml would
		// make the address change depending on which room was booked.
		if _, err := tx.ExecContext(ctx, `
			UPDATE venues SET
				address = CASE WHEN address = '' THEN ? ELSE address END,
				city    = CASE WHEN city    = '' THEN ? ELSE city    END,
				state   = CASE WHEN state   = '' THEN ? ELSE state   END,
				zip     = CASE WHEN zip     = '' THEN ? ELSE zip     END,
				url     = CASE WHEN url     = '' THEN ? ELSE url     END
			WHERE id = ?`, v.Address, v.City, v.State, v.Zip, v.URL, id); err != nil {
			return 0, fmt.Errorf("enrich venue %q: %w", v.Name, err)
		}
		return id, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("look up venue %q: %w", v.Name, err)
	}

	slug := venueSlug(v.Name)
	r, err := tx.ExecContext(ctx, `
		INSERT INTO venues (slug, name, address, city, state, zip, url)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET name = excluded.name`,
		slug, v.Name, v.Address, v.City, v.State, v.Zip, v.URL)
	if err != nil {
		return 0, fmt.Errorf("create venue %q: %w", v.Name, err)
	}
	if id, err = r.LastInsertId(); err != nil || id == 0 {
		// ON CONFLICT DO UPDATE does not always report a useful last id, so
		// read it back rather than trusting it.
		if err := tx.QueryRowContext(ctx, `SELECT id FROM venues WHERE slug = ?`, slug).Scan(&id); err != nil {
			return 0, fmt.Errorf("read back venue %q: %w", v.Name, err)
		}
	}
	return id, nil
}

func venueSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// replaceProvenance rewrites which feed records make up an event.
//
// Delete then insert, rather than a diff. The list is a handful of rows and
// rewriting it handles the case a diff would have to special-case anyway: a
// feed that stopped publishing an event another feed still carries.
func replaceProvenance(ctx context.Context, tx *sql.Tx, eventID int64, e core.Event, sources map[string]sourceRow) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_fingerprints WHERE event_id = ?`, eventID); err != nil {
		return err
	}
	for _, src := range e.Sources {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO event_fingerprints (event_id, fingerprint, source_id) VALUES (?, ?, ?)
			 ON CONFLICT(fingerprint) DO UPDATE SET event_id = excluded.event_id, source_id = excluded.source_id`,
			eventID, src.Fingerprint, sources[src.SourceKey].id); err != nil {
			return err
		}
	}
	return nil
}

func replaceCategories(ctx context.Context, tx *sql.Tx, eventID int64, categories []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_categories WHERE event_id = ?`, eventID); err != nil {
		return err
	}
	for i, c := range categories {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO event_categories (event_id, position, name) VALUES (?, ?, ?)`, eventID, i, c); err != nil {
			return err
		}
	}
	return nil
}

// sourceRow is what SaveEvents needs to know about a source.
type sourceRow struct {
	id   int64
	kind core.SourceKind
}

// loadSources reads the whole table once per call rather than querying per
// event. Seventy events across nine sources would otherwise be seventy
// lookups of nine rows.
func loadSources(ctx context.Context, tx *sql.Tx) (map[string]sourceRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT url, id, kind FROM sources`)
	if err != nil {
		return nil, fmt.Errorf("load sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]sourceRow{}
	for rows.Next() {
		var (
			url  string
			r    sourceRow
			kind string
		)
		if err := rows.Scan(&url, &r.id, &kind); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		r.kind = core.SourceKind(kind)
		out[url] = r
	}
	return out, rows.Err()
}

// loadVerifiedGroups reads the gate: which groups a human has signed for.
func loadVerifiedGroups(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT slug, verified_by <> '' FROM groups`)
	if err != nil {
		return nil, fmt.Errorf("load groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]bool{}
	for rows.Next() {
		var (
			slug string
			v    bool
		)
		if err := rows.Scan(&slug, &v); err != nil {
			return nil, err
		}
		out[slug] = v
	}
	return out, rows.Err()
}

// Counts is the per-status tally a sync prints.
type Counts struct {
	Total     int
	Published int
	Pending   int
	Cancelled int
}

func (s *Store) Counts(ctx context.Context) (Counts, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM events GROUP BY status`)
	if err != nil {
		return Counts{}, fmt.Errorf("count events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var c Counts
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return Counts{}, err
		}
		c.Total += n
		switch status {
		case StatusPublished:
			c.Published = n
		case StatusPending:
			c.Pending = n
		case StatusCancelled:
			c.Cancelled = n
		}
	}
	return c, rows.Err()
}

// Upcoming returns published events starting at or after from, soonest first.
//
// The status filter in this query is the publishing gate. Everything else in
// the pipeline can work perfectly and nothing reaches a reader until a human
// has put their handle in verified_by, because this is the only way events
// leave the database and it will not return a pending row.
//
// This is the read that becomes Phase 8's EventStore, and the interface gets
// defined there with its handler rather than here, per D3.
func (s *Store) Upcoming(ctx context.Context, from time.Time) ([]core.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.fingerprint, e.group_slug, e.title, e.excerpt, e.starts_at, e.ends_at,
		       e.all_day, e.venue_id, e.venue_name, e.room, e.url, e.register_url, e.virtual,
		       e.first_seen, e.last_seen,
		       COALESCE(v.address, ''), COALESCE(v.city, ''), COALESCE(v.state, ''),
		       COALESCE(v.zip, ''), COALESCE(v.url, '')
		FROM events e LEFT JOIN venues v ON v.id = e.venue_id
		WHERE e.status = ? AND e.starts_at >= ?
		ORDER BY e.starts_at, e.id`, StatusPublished, formatTime(from))
	if err != nil {
		return nil, fmt.Errorf("query upcoming: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		out []core.Event
		ids []int64
	)
	for rows.Next() {
		var (
			e                                 core.Event
			starts, ends, firstSeen, lastSeen string
			allDay, virtual                   int
		)
		if err := rows.Scan(&e.ID, &e.Fingerprint, &e.GroupSlug, &e.Title, &e.Excerpt,
			&starts, &ends, &allDay, &e.Venue.ID, &e.Venue.Name, &e.Room,
			&e.URL, &e.RegisterURL, &virtual, &firstSeen, &lastSeen,
			&e.Venue.Address, &e.Venue.City, &e.Venue.State, &e.Venue.Zip, &e.Venue.URL); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		for _, f := range []struct {
			dst *time.Time
			src string
		}{{&e.Start, starts}, {&e.End, ends}, {&e.FirstSeen, firstSeen}, {&e.LastSeen, lastSeen}} {
			if *f.dst, err = parseTime(f.src); err != nil {
				return nil, fmt.Errorf("event %d: %w", e.ID, err)
			}
		}
		e.AllDay = allDay != 0
		e.Virtual = virtual != 0

		out = append(out, e)
		ids = append(ids, e.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := s.attachCategories(ctx, out, ids); err != nil {
		return nil, err
	}
	return out, s.attachSources(ctx, out, ids)
}

// attachCategories and attachSources fill the child collections in one query
// each rather than one per event, which is the difference between 3 queries
// and 3 times the row count.
func (s *Store) attachCategories(ctx context.Context, events []core.Event, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, name FROM event_categories ORDER BY event_id, position`)
	if err != nil {
		return fmt.Errorf("load categories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := indexByID(events)
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		if e, ok := byID[id]; ok {
			e.Categories = append(e.Categories, name)
		}
	}
	return rows.Err()
}

func (s *Store) attachSources(ctx context.Context, events []core.Event, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.event_id, s.url, f.fingerprint
		FROM event_fingerprints f JOIN sources s ON s.id = f.source_id
		ORDER BY f.event_id, s.priority, f.fingerprint`)
	if err != nil {
		return fmt.Errorf("load provenance: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := indexByID(events)
	for rows.Next() {
		var (
			id      int64
			url, fp string
		)
		if err := rows.Scan(&id, &url, &fp); err != nil {
			return err
		}
		if e, ok := byID[id]; ok {
			e.Sources = append(e.Sources, core.EventSource{SourceKey: url, Fingerprint: fp})
		}
	}
	return rows.Err()
}

func indexByID(events []core.Event) map[int64]*core.Event {
	m := make(map[int64]*core.Event, len(events))
	for i := range events {
		m[events[i].ID] = &events[i]
	}
	return m
}

// formatTime renders an instant for storage, and a zero time as the empty
// string. Formatting a zero time.Time normally would write "0001-01-01T00:00:00Z",
// which sorts before every real timestamp and reads like a date rather than
// like the absence of one.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(timeLayout, s)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
