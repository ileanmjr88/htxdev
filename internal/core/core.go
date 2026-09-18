// Package core holds htxdev's domain types. It imports only the standard
// library and is imported by everything else; nothing here knows about HTTP,
// SQLite, or any particular feed format.
package core

import "time"

type SourceKind string

const (
	KindTribe SourceKind = "tribe"
	KindICS   SourceKind = "ics"

	// KindHTML is a page scraped for its semantic markup rather than a feed.
	// It exists for exactly one source, Houston Open Source Society, who
	// publish no calendar but do publish <time datetime="..."> on their
	// meetings page. That is machine-readable markup with a spec behind it,
	// not a regex over prose, which is the line between this being reasonable
	// and being a scraper to maintain forever.
	//
	// Still the worst kind of source to have, and the standing preference is
	// to ask a group for an ICS feed first. Retire this the day HOSS ships one.
	KindHTML SourceKind = "html"
)

type Group struct {
	Slug       string // "houston-oss"
	Name       string
	URL        string   // homepage, always link back
	Category   string   // default for its events
	Aliases    []string // names it appears under in other feeds
	VerifiedBy string   // GitHub handle; empty means events stay pending
	VerifiedAt time.Time
	Active     bool
}

type Venue struct {
	ID      int64    // DB identity; 0 means unresolved
	Slug    string   // set only when curated in sources.yaml, e.g. "ion"
	Name    string   // "Ion"
	Aliases []string // "The Ion", how other feeds write it
	Address string
	City    string
	State   string
	Zip     string
	URL     string
}

type Source struct {
	ID        int64
	GroupSlug string // back-reference; the YAML nests these under a group
	Kind      SourceKind
	URL       string
	Priority  int // lower wins when the same event appears twice
	Enabled   bool
}

// RawOrganizer is a feed's claim about who runs an event.
// Deliberately no Email field: Ion sends one and it is PII.
type RawOrganizer struct {
	Name string
	URL  string // Ion's organizer.website. Empty for ICS.
}

// RawVenue is what a feed said about a place, before resolution.
type RawVenue struct {
	UpstreamID string // Ion's venue global_id; empty for ICS
	Name       string // "Ion – Conference Room 028", or a whole LOCATION string
	Address    string // ICS leaves these empty. It only has one string.
	City       string
	State      string
	Zip        string
	URL        string
}

// RawEvent is what a feed literally said. No derivation.
type RawEvent struct {
	// SourceKey is the feed URL this record came from, stamped by the fetch
	// layer rather than the decoder. It is a URL and not Source.ID because
	// int64 IDs come from the database, which does not exist yet at fetch
	// time. Inventing IDs by load order would mean reordering sources.yaml
	// silently rewrites the attribution stored against every historical event.
	SourceKey   string
	UpstreamID  string // global_id or ICS UID. Becomes Event.Fingerprint
	Title       string // unescaped at decode
	Description string // raw HTML, whole. Excerpt is derived later
	Start       time.Time
	End         time.Time
	AllDay      bool   // date-only upstream
	URL         string // event page on the source. Absent in Google ICS.
	RegisterURL string // Ion's `website`. No ICS equivalent.
	VirtualURL  string // Ion's virtual_url, or X-GOOGLE-CONFERENCE
	Virtual     bool

	Organizers []RawOrganizer // Ion: 1 or 2. ICS: none.
	Venues     []RawVenue     // Ion: 1 or 2. ICS: 0 or 1.
	Categories []string       // Ion's taxonomy names, unescaped. ICS: none.
}

// Event is what htxdev concluded, after normalize and dedupe.
type Event struct {
	ID          int64
	Fingerprint string // "tribe:iondistrict.com?id=23861". Write-once at FirstSeen.

	GroupSlug string // the organizer, not necessarily the publisher
	Title     string
	Excerpt   string // derived from RawEvent.Description
	Start     time.Time
	End       time.Time
	AllDay    bool

	// Venue is where this resolved to, whole rather than as a name and an id.
	// Its ID is 0 until the database has a row, for the reason D14 gives for
	// RawEvent.SourceKey: normalize runs before anything is written, and a
	// venue discovered from event data may have no row at all until this event
	// creates it. An empty Name means nothing resolved, which is normal: 7 of
	// HLUG's 110 events carry no LOCATION.
	//
	// A curated venue arrives here with the address sources.yaml gives it,
	// which is the point of curating one. Ion's own feed spells its address
	// three ways across five rooms ("4201 Main Street", "4201 Main St",
	// "4201 Main St.") and sometimes omits the state or the zip. A discovered
	// venue arrives with whatever its feed said, because that beats nothing.
	Venue       Venue
	Room        string // "Conference Room 028", split off the venue name
	URL         string
	RegisterURL string

	Categories []string
	Virtual    bool

	FirstSeen time.Time
	LastSeen  time.Time

	// Sources holds every feed record that merged into this event, in the
	// order they won: the first supplied the title and the links.
	//
	// Paired rather than two parallel slices, because the store needs to know
	// which feed each fingerprint came from and index-aligned slices go wrong
	// the first time one feed contributes two records to one event, which a
	// venue calendar listing a co-hosted meeting twice really does.
	Sources []EventSource
}

// EventSource is one feed record that merged into an Event.
//
// SourceKey is the feed URL rather than a database id, per D14: normalize runs
// before anything is written. Fingerprint is that record's own identity, which
// is not necessarily the Event's: an Event keeps the fingerprint of whichever
// record was seen first, and D7 forbids recomputing it when a higher-priority
// feed shows up later and wins the merge.
type EventSource struct {
	SourceKey   string
	Fingerprint string
}

// Fingerprint is an event's permanent identity: the upstream stable ID,
// namespaced by the kind of source that supplied it.
//
// D7, and deliberately not derived from content. A fingerprint has two jobs
// that pull against each other: it is a tiebreak for dedupe, and in v1.1 it
// becomes the UID of the ICS feed htxdev publishes. Include the title and an
// organizer fixing a typo changes the UID, which duplicates the event in every
// subscriber's calendar. So it is the ID the source already assigned.
//
// Assigned once when an event is first seen and never recomputed. If a
// higher-priority source later starts carrying the same event, the row that
// wins dedupe changes, and recomputing would churn the fingerprint with it.
//
// Upstream IDs already embed a host, so they are close to globally unique on
// their own; the kind prefix is cheap insurance against a future source that
// is sloppier. A source with no stable ID of its own would need a "hash:"
// form built from the group slug and start time, per D7. Neither decoder can
// produce an event without an upstream ID today, so that case does not exist
// yet and is not invented here.
func Fingerprint(kind SourceKind, upstreamID string) string {
	return string(kind) + ":" + upstreamID
}
