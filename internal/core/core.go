// Package core holds htxdev's domain types. It imports only the standard
// library and is imported by everything else; nothing here knows about HTTP,
// SQLite, or any particular feed format.
package core

import "time"

type SourceKind string

const (
	KindTribe SourceKind = "tribe"
	KindICS   SourceKind = "ics"
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
	SourceID    int64  // stamped by the fetch layer in Phase 2, not the decoder
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

	VenueID     int64  // 0 means unresolved; 7 of 110 HLUG events have no LOCATION
	Room        string // "Conference Room 028"
	URL         string
	RegisterURL string

	Categories []string
	Virtual    bool

	FirstSeen time.Time
	LastSeen  time.Time
	SourceIDs []int64 // plural: dedupe merges the same event from several feeds
}
