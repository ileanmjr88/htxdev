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
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
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
}

// SaveEvents writes what a fetch produced.
//
// Everything happens in one transaction. A sync that dies half way through
// leaves the database exactly as it was, which matters more than usual here:
// the file is committed, so a partial write is a partial write that gets
// pushed. It also means last_seen either advances for every event in the run
// or for none of them, and cancellation reads last_seen.
//
// Nothing is deleted and nothing is cancelled. An event that has stopped
// appearing simply stops having its last_seen bumped, and deciding what that
// means is Phase 5's job, under guards that live in normalize and need
// information this function does not have.
func (s *Store) SaveEvents(ctx context.Context, events []core.RawEvent, seenAt time.Time) (SaveResult, error) {
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

	stamp := formatTime(seenAt)

	for _, e := range events {
		src, ok := sources[e.SourceKey]
		if !ok {
			// The registry has to be synced before events referencing it. A
			// missing source means the caller skipped that step, and guessing
			// would attribute real events to nothing.
			return res, fmt.Errorf("event %q: no source row for %q; sync the registry first",
				e.UpstreamID, e.SourceKey)
		}

		fingerprint := core.Fingerprint(src.kind, e.UpstreamID)

		// The pending gate. An event is publishable only because a human
		// filled in verified_by for its group, and this is where that decision
		// turns into a column.
		status := StatusPending
		if src.verified {
			status = StatusPublished
		}

		var (
			id        int64
			oldStatus string
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, status FROM events WHERE fingerprint = ?`, fingerprint).Scan(&id, &oldStatus)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			id, err = insertEvent(ctx, tx, src.id, fingerprint, status, stamp, e)
			if err != nil {
				return res, fmt.Errorf("insert %s: %w", fingerprint, err)
			}
			res.Inserted++

		case err != nil:
			return res, fmt.Errorf("look up %s: %w", fingerprint, err)

		default:
			// fingerprint and first_seen are absent from the SET list on
			// purpose. Both are write-once per D7: the fingerprint becomes a
			// published ICS UID, and first_seen is the one fact in this row
			// that cannot be rebuilt from a feed.
			//
			// A row that was cancelled and has reappeared comes back to life
			// here, which is what a rescheduled event looks like from outside.
			if _, err := tx.ExecContext(ctx, `
				UPDATE events SET
					source_id = ?, title = ?, description = ?, starts_at = ?, ends_at = ?,
					all_day = ?, url = ?, register_url = ?, virtual_url = ?, virtual = ?,
					status = ?, last_seen = ?
				WHERE id = ?`,
				src.id, e.Title, e.Description, formatTime(e.Start), formatTime(e.End),
				boolToInt(e.AllDay), e.URL, e.RegisterURL, e.VirtualURL, boolToInt(e.Virtual),
				status, stamp, id); err != nil {
				return res, fmt.Errorf("update %s: %w", fingerprint, err)
			}
			res.Updated++
			if oldStatus == StatusPending && status == StatusPublished {
				res.Promoted++
			}
		}

		if err := replaceChildren(ctx, tx, id, e); err != nil {
			return res, fmt.Errorf("children of %s: %w", fingerprint, err)
		}
	}

	return res, tx.Commit()
}

// sourceRow is what SaveEvents needs to know about a source: its database
// identity, the namespace its fingerprints live in, and whether a human has
// signed for the group behind it.
type sourceRow struct {
	id       int64
	kind     core.SourceKind
	verified bool
}

// loadSources reads the whole table once per call rather than querying per
// event. Seventy events across seven sources would otherwise be seventy
// lookups of seven rows.
func loadSources(ctx context.Context, tx *sql.Tx) (map[string]sourceRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT s.url, s.id, s.kind, g.verified_by <> ''
		FROM sources s JOIN groups g ON g.slug = s.group_slug`)
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
		if err := rows.Scan(&url, &r.id, &kind, &r.verified); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		r.kind = core.SourceKind(kind)
		out[url] = r
	}
	return out, rows.Err()
}

func insertEvent(ctx context.Context, tx *sql.Tx, sourceID int64, fingerprint, status, stamp string, e core.RawEvent) (int64, error) {
	r, err := tx.ExecContext(ctx, `
		INSERT INTO events (
			fingerprint, source_id, title, description, starts_at, ends_at, all_day,
			url, register_url, virtual_url, virtual, status, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fingerprint, sourceID, e.Title, e.Description, formatTime(e.Start), formatTime(e.End),
		boolToInt(e.AllDay), e.URL, e.RegisterURL, e.VirtualURL, boolToInt(e.Virtual),
		// first_seen and last_seen are the same value on the first sync, and
		// their being equal is exactly what "never seen before now" means.
		status, stamp, stamp)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// replaceChildren rewrites an event's venues, organizers and categories.
//
// Delete-then-insert rather than a diff. These are ordered lists of at most a
// handful of rows whose position is part of their meaning, so working out
// which ones changed costs more code than rewriting them and would have to get
// the reordering case right anyway.
func replaceChildren(ctx context.Context, tx *sql.Tx, eventID int64, e core.RawEvent) error {
	for _, table := range []string{"event_venues", "event_organizers", "event_categories"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE event_id = ?`, eventID); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}

	for i, v := range e.Venues {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO event_venues (event_id, position, upstream_id, name, address, city, state, zip, url)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			eventID, i, v.UpstreamID, v.Name, v.Address, v.City, v.State, v.Zip, v.URL); err != nil {
			return err
		}
	}
	for i, o := range e.Organizers {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO event_organizers (event_id, position, name, url) VALUES (?, ?, ?, ?)`,
			eventID, i, o.Name, o.URL); err != nil {
			return err
		}
	}
	for i, c := range e.Categories {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO event_categories (event_id, position, name) VALUES (?, ?, ?)`,
			eventID, i, c); err != nil {
			return err
		}
	}
	return nil
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
// Several core.Event fields stay zero here: Excerpt, VenueID, Room and
// Categories are all conclusions normalize has not drawn yet, and SourceIDs
// holds exactly one entry because nothing has been merged. This is the read
// that becomes Phase 8's EventStore, and the interface gets defined there with
// its handler rather than here, per D3.
func (s *Store) Upcoming(ctx context.Context, from time.Time) ([]core.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.fingerprint, s.group_slug, e.title, e.starts_at, e.ends_at,
		       e.all_day, e.url, e.register_url, e.virtual, e.first_seen, e.last_seen, e.source_id
		FROM events e JOIN sources s ON s.id = e.source_id
		WHERE e.status = ? AND e.starts_at >= ?
		ORDER BY e.starts_at`, StatusPublished, formatTime(from))
	if err != nil {
		return nil, fmt.Errorf("query upcoming: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.Event
	for rows.Next() {
		var (
			e                                 core.Event
			starts, ends, firstSeen, lastSeen string
			allDay, virtual                   int
			sourceID                          int64
		)
		if err := rows.Scan(&e.ID, &e.Fingerprint, &e.GroupSlug, &e.Title, &starts, &ends,
			&allDay, &e.URL, &e.RegisterURL, &virtual, &firstSeen, &lastSeen, &sourceID); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}

		if e.Start, err = parseTime(starts); err != nil {
			return nil, fmt.Errorf("event %d start: %w", e.ID, err)
		}
		if e.End, err = parseTime(ends); err != nil {
			return nil, fmt.Errorf("event %d end: %w", e.ID, err)
		}
		if e.FirstSeen, err = parseTime(firstSeen); err != nil {
			return nil, fmt.Errorf("event %d first_seen: %w", e.ID, err)
		}
		if e.LastSeen, err = parseTime(lastSeen); err != nil {
			return nil, fmt.Errorf("event %d last_seen: %w", e.ID, err)
		}
		e.AllDay = allDay != 0
		e.Virtual = virtual != 0
		e.SourceIDs = []int64{sourceID}

		out = append(out, e)
	}
	return out, rows.Err()
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
