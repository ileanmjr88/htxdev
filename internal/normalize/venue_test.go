package normalize

import (
	"strings"
	"testing"

	"github.com/ileanmjr88/htxdev/internal/core"
	"github.com/ileanmjr88/htxdev/internal/registry"
)

// The curated venues, matching data/sources.yaml. The "The Ion" alias is the
// one that matters: HLUG writes it that way and Ion writes "Ion", and both
// have to end up as one building or it appears twice on the site.
func venueRegistry() *registry.Registry {
	reg := testRegistry()
	reg.Venues = []core.Venue{
		{Slug: "ion", Name: "Ion", Aliases: []string{"The Ion"}, Address: "4201 Main St"},
		{Slug: "improving-houston", Name: "Improving Houston"},
		{Slug: "txrx-labs", Name: "TXRX Labs"},
	}
	return reg
}

// Every venue string in the live database on 2026-09-17, plus the two array
// shapes. Pinned because these were measured, not imagined.
func TestResolveVenueAgainstLiveStrings(t *testing.T) {
	n := New(venueRegistry(), nil)

	cases := []struct {
		name      string
		in        []string // one element, or [room, building]
		wantVenue string
		wantRoom  string
	}{
		// tribe, the building alone
		{"bare building", []string{"Ion"}, "Ion", ""},
		// tribe, en dash separating a room
		{"en dash room", []string{"Ion – Lobby"}, "Ion", "Lobby"},
		{"en dash conference room", []string{"Ion – Conference Room 028"}, "Ion", "Conference Room 028"},
		// Splitting on the FIRST dash only, so the second stays in the room.
		{"two dashes", []string{"Ion – Conference Room 029 – 030"}, "Ion", "Conference Room 029 – 030"},
		{"a room that is not a room number", []string{"Ion – Forum Stairs"}, "Ion", "Forum Stairs"},

		// ics, one flat LOCATION string. The alias is what makes these work.
		{"ics flat string with a room", []string{"The Ion, Room 30, 4201 Main St, Houston, TX 77002, USA"}, "Ion", "Room 30"},
		{"ics flat string, plural rooms", []string{"The Ion, Rooms 29 and 30, 4201 Main St, Houston, TX 77002, USA"}, "Ion", "Rooms 29 and 30"},
		// The segment after the venue is a street number, not a room. Getting
		// this wrong would put "1127 Eldridge Pkwy Suite 600" in the room field.
		{"ics flat string, address not a room", []string{"Finn MacCool’s Irish Bar, 1127 Eldridge Pkwy Suite 600, Houston, TX 77077, USA"}, "Finn MacCool’s Irish Bar", ""},
		{"ics flat string, uncurated venue", []string{"Sesh Coworking, 2808 Caroline St #100, Houston, TX 77004, USA"}, "Sesh Coworking", ""},

		// html, HOSS. Plain names, one of which is already curated by alias.
		{"hoss curated by name", []string{"Improving Houston"}, "Improving Houston", ""},
		{"hoss curated by alias", []string{"The Ion"}, "Ion", ""},
		{"hoss discovered", []string{"Zion Lutheran Church"}, "Zion Lutheran Church", ""},
		{"hoss discovered, two words", []string{"Bayland Community Center"}, "Bayland Community Center", ""},

		// Uncurated venues keep their names. The architecture expects venues
		// to be discovered from event data and curated only when a name needs
		// canonicalising.
		{"discovered", []string{"Second Draught"}, "Second Draught", ""},
		{"discovered, another", []string{"Greentown Labs"}, "Greentown Labs", ""},

		// Ion's [room, building] array, where the order IS the hierarchy.
		{"array", []string{"Ion – Lobby", "Ion"}, "Ion", "Lobby"},
		{"array, room repeats the building", []string{"Ion – Conference Room 030", "Ion"}, "Ion", "Conference Room 030"},
		{"array, bare room", []string{"Forum Stairs", "Ion"}, "Ion", "Forum Stairs"},

		{"nothing", nil, "", ""},
		{"empty name", []string{""}, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var vs []core.RawVenue
			for _, s := range tc.in {
				vs = append(vs, core.RawVenue{Name: s})
			}
			gotVenue, gotRoom := n.resolveVenue(vs)
			if gotVenue != tc.wantVenue || gotRoom != tc.wantRoom {
				t.Errorf("resolveVenue(%q) = (%q, %q), want (%q, %q)",
					tc.in, gotVenue, gotRoom, tc.wantVenue, tc.wantRoom)
			}
		})
	}
}

// The known miss, asserted rather than left as a surprise. Ion sends one event
// at "Ion Plaza", which has no dash to split on, so it stays a venue of its
// own. That is a curation decision for sources.yaml, not a parsing one, and
// pinning it here means adding the alias will make this test fail loudly
// rather than silently changing behaviour.
func TestIonPlazaIsNotResolvedYet(t *testing.T) {
	n := New(venueRegistry(), nil)
	venue, room := n.resolveVenue([]core.RawVenue{{Name: "Ion Plaza"}})
	if venue != "Ion Plaza" || room != "" {
		t.Errorf("resolveVenue(Ion Plaza) = (%q, %q); if this changed, an alias was added and the comment needs updating",
			venue, room)
	}
}

// A venue that resolves beats one that does not, whichever feed it came from.
// The whole reason to curate a building is that it arrives under several
// spellings and has to end up as one.
func TestMergePrefersAResolvedVenue(t *testing.T) {
	start := at("2026-10-01T18:00:00Z")
	events, _, _ := New(venueRegistry(), nil).Events([]core.RawEvent{
		// Winner on priority, but its venue is not curated.
		{SourceKey: hlugFeed, UpstreamID: "own", Title: "Winner", Start: start,
			Venues: []core.RawVenue{{Name: "Somewhere Uncurated"}}},
		{SourceKey: ionFeed, UpstreamID: "ion", Title: "Loser", Start: start,
			Organizers: []core.RawOrganizer{{Name: "HLUG"}},
			Venues:     []core.RawVenue{{Name: "Ion – Conference Room 030"}}},
	})
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Title != "Winner" {
		t.Errorf("title = %q, want the priority winner's", events[0].Title)
	}
	if events[0].VenueName != "Ion" || events[0].Room != "Conference Room 030" {
		t.Errorf("venue = (%q, %q), want the curated one from the losing record",
			events[0].VenueName, events[0].Room)
	}
}

// D9 on the merged event.
func TestCategoryComesFromTheGroup(t *testing.T) {
	start := at("2026-10-01T18:00:00Z")
	events, _, _ := New(venueRegistry(), nil).Events([]core.RawEvent{{
		SourceKey: ionFeed, UpstreamID: "x", Title: "T", Start: start,
		Organizers: []core.RawOrganizer{{Name: "HLUG"}},
		// Ion's own taxonomy, which must not become htxdev's.
		Categories: []string{"Founders & Startups", "Networking", "3rd Party Registration"},
	}})
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	got := events[0].Categories
	if len(got) != 1 || got[0] != "dev" {
		t.Errorf("categories = %v, want [dev] from the resolved group", got)
	}
	for _, c := range got {
		if strings.Contains(c, "&") || strings.Contains(c, "Party") {
			t.Errorf("category %q came from Ion's marketing buckets", c)
		}
	}
}

func TestRoomSegment(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Room 30, 4201 Main St, Houston", "Room 30"},
		{"Rooms 29 and 30, 4201 Main St", "Rooms 29 and 30"},
		{"Conference Room 028, 4201 Main St", "Conference Room 028"},
		{"Suite 210, Houston", "Suite 210"},
		{"Lobby, Houston", "Lobby"},
		// A street number is an address, not a room.
		{"1127 Eldridge Pkwy Suite 600, Houston, TX", ""},
		{"2808 Caroline St #100, Houston", ""},
		{"Houston, TX 77002", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := roomSegment(tc.in); got != tc.want {
			t.Errorf("roomSegment(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
