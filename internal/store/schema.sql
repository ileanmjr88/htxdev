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

-- Groups, venues and sources mirror the registry in data/, which stays the
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
-- mean reordering the registry silently rewrites attribution on every
-- historical event.
CREATE TABLE IF NOT EXISTS sources (
    id         INTEGER PRIMARY KEY,
    group_slug TEXT NOT NULL REFERENCES groups(slug) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    url        TEXT NOT NULL UNIQUE,
    priority   INTEGER NOT NULL DEFAULT 0,
    enabled    INTEGER NOT NULL DEFAULT 1
) STRICT;

-- One row per real-world event, after normalize has decided which feed records
-- describe the same thing. The Houston Linux meeting at the Ion arrives from
-- both Ion's calendar and HLUG's own; this holds one row for it, and
-- event_fingerprints records that two feeds contributed.
CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY,

    -- D7, and the reason event_fingerprints exists below. This is the
    -- fingerprint of whichever record was seen FIRST, assigned once and never
    -- recomputed. Deliberately NOT the current merge winner: if a
    -- higher-priority feed starts carrying an event later the winner changes,
    -- and a fingerprint that followed it would churn. It becomes a published
    -- ICS UID in v1.1, where churn duplicates the event in every subscriber's
    -- calendar.
    fingerprint TEXT NOT NULL UNIQUE,

    group_slug TEXT NOT NULL REFERENCES groups(slug) ON DELETE CASCADE,

    title     TEXT NOT NULL,
    excerpt   TEXT NOT NULL DEFAULT '',
    starts_at TEXT NOT NULL,
    ends_at   TEXT NOT NULL DEFAULT '',
    all_day   INTEGER NOT NULL DEFAULT 0,

    -- 0 means unresolved, which is normal rather than exceptional: venues are
    -- discovered from event data and curated in the registry only when a name
    -- needs canonicalising. venue_name keeps what it resolved to either way.
    venue_id   INTEGER NOT NULL DEFAULT 0,
    venue_name TEXT NOT NULL DEFAULT '',
    room       TEXT NOT NULL DEFAULT '',

    url          TEXT NOT NULL DEFAULT '',
    register_url TEXT NOT NULL DEFAULT '',
    virtual      INTEGER NOT NULL DEFAULT 0,

    status TEXT NOT NULL CHECK (status IN ('pending', 'published', 'cancelled')),

    first_seen TEXT NOT NULL,
    last_seen  TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS events_starts_at ON events(starts_at);
CREATE INDEX IF NOT EXISTS events_status_starts_at ON events(status, starts_at);
CREATE INDEX IF NOT EXISTS events_group ON events(group_slug, starts_at);

-- Every feed record that has ever merged into an event, and which source it
-- came from. Two jobs.
--
-- First, identity. An event is found by ANY of its fingerprints, which is what
-- lets events.fingerprint stay write-once while the merge winner is free to
-- change. Without this, an event first seen only on Ion and later also
-- published by HLUG would change identity the moment the higher-priority feed
-- appeared, which is exactly what D7 forbids.
--
-- Second, provenance. "Which feeds say this is happening" is a real question
-- for a discovery site, and it is the only evidence that dedupe did anything.
CREATE TABLE IF NOT EXISTS event_fingerprints (
    event_id    INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    fingerprint TEXT NOT NULL UNIQUE,
    source_id   INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    PRIMARY KEY (event_id, fingerprint)
) STRICT;

CREATE INDEX IF NOT EXISTS event_fingerprints_source ON event_fingerprints(source_id);

-- One row per category. A list, because an event can carry several, and a
-- table rather than a delimited string for the same reason the decoders use
-- narrow structs: a column you have to name is a column somebody decided to
-- keep, and a comma-joined text field is a parser waiting to be written.
--
-- Venues and organizers used to have tables like this one, holding whatever
-- each feed called the place and whoever it named. Both are gone. normalize
-- resolves the venue to one id and room before anything is written, and the
-- only organizer that ever mattered was the one naming a group, which becomes
-- group_slug. Keeping a table of organizer names with no reader is how an
-- email column gets added to it one day.
CREATE TABLE IF NOT EXISTS event_categories (
    event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    name     TEXT NOT NULL,
    PRIMARY KEY (event_id, position)
) STRICT;
