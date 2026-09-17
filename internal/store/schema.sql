-- htxdev's schema.
--
-- Embedded into the binary with go:embed and applied on every open, so a fresh
-- checkout and an existing database take the same path. Every statement is
-- IF NOT EXISTS, which makes opening an existing file a no-op rather than a
-- migration. That holds until the first change that has to alter a table;
-- there is no migration machinery here yet because there is nothing to
-- migrate, and inventing a versioning scheme before the first version exists
-- is how you end up maintaining two.
--
-- Every table is STRICT. Without it SQLite's type affinity accepts a string
-- into an INTEGER column and silently keeps it, which turns a Go bug into a
-- data bug that surfaces months later on read. Needs SQLite 3.37+; the driver
-- ships 3.53.
--
-- Timestamps are RFC3339 TEXT in UTC, not Unix integers. Three reasons, and
-- the first is specific to this project: htxdev.db is committed to git, so it
-- gets read by humans through `sqlite3 .dump`, and 2026-11-06T00:00:00Z is
-- legible where 1794528000 is not. RFC3339 in UTC also sorts correctly as a
-- string, so indexes and ORDER BY work without conversion, and it carries the
-- zone rather than leaving it to convention.

-- Groups, venues and sources mirror data/sources.yaml, which stays the
-- authoritative copy. The YAML is edited by pull request and the commit author
-- is the provenance for a verification; these tables exist so the rest of the
-- schema has something to reference and so the API can join without reparsing
-- YAML per request. Anything written here by a sync is derived, never decided.
CREATE TABLE IF NOT EXISTS groups (
    slug        TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    url         TEXT NOT NULL DEFAULT '',
    category    TEXT NOT NULL DEFAULT '',
    -- Blank means unverified, which means this group's events stay pending and
    -- never publish. Filling it in is the act of verification.
    verified_by TEXT NOT NULL DEFAULT '',
    verified_at TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE IF NOT EXISTS venues (
    id      INTEGER PRIMARY KEY,
    slug    TEXT NOT NULL UNIQUE,
    name    TEXT NOT NULL,
    address TEXT NOT NULL DEFAULT '',
    city    TEXT NOT NULL DEFAULT '',
    state   TEXT NOT NULL DEFAULT '',
    zip     TEXT NOT NULL DEFAULT '',
    url     TEXT NOT NULL DEFAULT ''
) STRICT;

-- D14: a source's identity in the database is this integer, and its identity
-- everywhere upstream of the database is its feed URL. RawEvent carries the
-- URL because int64 IDs come from here, and inventing them by load order would
-- mean reordering sources.yaml silently rewrites attribution on every
-- historical event.
CREATE TABLE IF NOT EXISTS sources (
    id         INTEGER PRIMARY KEY,
    group_slug TEXT NOT NULL REFERENCES groups(slug) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    url        TEXT NOT NULL UNIQUE,
    priority   INTEGER NOT NULL DEFAULT 0,
    enabled    INTEGER NOT NULL DEFAULT 1
) STRICT;

-- One row per event per source. Two sources carrying the same real-world event
-- are two rows here on purpose: this table records what each feed said, and
-- deciding they are the same event is Phase 5's job. The Houston Linux meeting
-- at the Ion arrives from both Ion's calendar and HLUG's own, and today that
-- is two rows with different titles.
CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY,

    -- D7. Write-once: set on insert and never updated, because it becomes the
    -- ICS UID in v1.1 and a UID that changes duplicates the event in every
    -- subscriber's calendar.
    fingerprint TEXT NOT NULL UNIQUE,
    source_id   INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,

    title        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    starts_at    TEXT NOT NULL,
    ends_at      TEXT NOT NULL DEFAULT '',
    all_day      INTEGER NOT NULL DEFAULT 0,
    url          TEXT NOT NULL DEFAULT '',
    register_url TEXT NOT NULL DEFAULT '',
    virtual_url  TEXT NOT NULL DEFAULT '',
    virtual      INTEGER NOT NULL DEFAULT 0,

    -- pending  : the owning group is unverified. Never published.
    -- published: a human reviewed the group's feed and signed for it.
    -- cancelled: seen before, absent now, under the conditions in the
    --            absence-means-cancelled contract. Phase 5 sets this; nothing
    --            in Phase 4 does, because the guards live in normalize.
    status TEXT NOT NULL CHECK (status IN ('pending', 'published', 'cancelled')),

    -- Write-once, and the reason this database is committed rather than
    -- rebuilt: it is the only record of when htxdev first saw an event, and
    -- rebuilding from feeds cannot recover it because feeds forget.
    first_seen TEXT NOT NULL,
    -- Bumped on every sync that still sees the event. An event whose last_seen
    -- falls behind the run is the input to cancellation.
    last_seen  TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS events_starts_at ON events(starts_at);
CREATE INDEX IF NOT EXISTS events_status_starts_at ON events(status, starts_at);
CREATE INDEX IF NOT EXISTS events_source_last_seen ON events(source_id, last_seen);

-- Child tables rather than JSON columns, for the same reason the decoders use
-- narrow structs rather than map[string]any: a column you have to name is a
-- column somebody decided to keep. position preserves feed order, which is
-- load-bearing for venues, where Ion sends [room, building] and the order is
-- the hierarchy.
CREATE TABLE IF NOT EXISTS event_venues (
    event_id    INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    upstream_id TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL,
    address     TEXT NOT NULL DEFAULT '',
    city        TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT '',
    zip         TEXT NOT NULL DEFAULT '',
    url         TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (event_id, position)
) STRICT;

-- No email column, deliberately. Ion sends one and it is PII; the decoder
-- already refuses to carry it, and leaving the column out means a later change
-- to that decoder has nowhere to put it.
CREATE TABLE IF NOT EXISTS event_organizers (
    event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    name     TEXT NOT NULL,
    url      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (event_id, position)
) STRICT;

CREATE TABLE IF NOT EXISTS event_categories (
    event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    name     TEXT NOT NULL,
    PRIMARY KEY (event_id, position)
) STRICT;
