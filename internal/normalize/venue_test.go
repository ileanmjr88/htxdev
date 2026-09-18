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

		// Ion's two-element array. The order is the hierarchy, but the inner
		// element is only a room when it names the outer one.
		//
		// A bare inner name that does not repeat the building is ambiguous:
		// "Forum Stairs" would be a room and "Greentown Labs" is a venue, and
		// nothing in the string distinguishes them. It resolves to a venue,
		// because the two ways of being wrong are not equal. A room filed as
		// its own venue is cosmetic and keeps its name. A separate building
		// filed as a room of another sends somebody to the wrong address. The
		// live data has no bare-room arrays and does have Greentown Labs, so
		// the cost is hypothetical and the benefit is not.
		{"array", []string{"Ion – Lobby", "Ion"}, "Ion", "Lobby"},
		{"array, room repeats the building", []string{"Ion – Conference Room 030", "Ion"}, "Ion", "Conference Room 030"},
		{"array, no dash but still the building", []string{"Ion Plaza", "Ion"}, "Ion", "Plaza"},
		// The case that changed the rule. Ion sends [Greentown Labs, Ion], and
		// Greentown Labs is its own building a couple of streets away, not a
		// room. Filing it as a room of the Ion sends people to the wrong door.
		{"array, a different building in the same district", []string{"Greentown Labs", "Ion"}, "Greentown Labs", ""},
		{"array, both elements the same place", []string{"Ion", "Ion"}, "Ion", ""},

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
			if gotVenue.Name != tc.wantVenue || gotRoom != tc.wantRoom {
				t.Errorf("resolveVenue(%q) = (%q, %q), want (%q, %q)",
					tc.in, gotVenue.Name, gotRoom, tc.wantVenue, tc.wantRoom)
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
	if venue.Name != "Ion Plaza" || room != "" {
		t.Errorf("resolveVenue(Ion Plaza) = (%q, %q); if this changed, an alias was added and the comment needs updating",
			venue.Name, room)
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
	if events[0].Venue.Name != "Ion" || events[0].Room != "Conference Room 030" {
		t.Errorf("venue = (%q, %q), want the curated one from the losing record",
			events[0].Venue.Name, events[0].Room)
	}
	// The curated address comes with it, which is the point of curating one.
	if events[0].Venue.Address != "4201 Main St" {
		t.Errorf("address = %q, want the curated one", events[0].Venue.Address)
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

// ICS sends one flat LOCATION string and the shape is regular across both
// Google Calendar and Meetup. It is still a string somebody typed, so every
// field is best-effort and a segment that does not fit is dropped rather than
// guessed at: missing beats wrong on an address.
func TestSplitFlatAddress(t *testing.T) {
	cases := []struct {
		name                   string
		in                     string
		addr, city, state, zip string
	}{
		{
			name: "the HLUG shape",
			in:   " 1127 Eldridge Pkwy Suite 600, Houston, TX 77077, USA",
			addr: "1127 Eldridge Pkwy Suite 600", city: "Houston", state: "TX", zip: "77077",
		},
		{
			name: "a unit number in the street",
			in:   " 2808 Caroline St #100, Houston, TX 77004, USA",
			addr: "2808 Caroline St #100", city: "Houston", state: "TX", zip: "77004",
		},
		{
			name: "no country",
			in:   " 3606 Beauchamp Blvd, Houston, TX 77009",
			addr: "3606 Beauchamp Blvd", city: "Houston", state: "TX", zip: "77009",
		},
		{
			name: "zip missing",
			in:   " 100 Main St, Houston, TX",
			addr: "100 Main St", city: "Houston", state: "TX",
		},
		{
			name: "state missing",
			in:   " 100 Main St, Houston, 77002",
			addr: "100 Main St", city: "Houston", zip: "77002",
		},
		{"street only", " 100 Main St", "100 Main St", "", "", ""},
		{"nothing", "", "", "", "", ""},
		{"only a country", " USA", "", "", "", ""},
		{
			// A nine-digit zip is not five, so it is dropped rather than
			// truncated into something that looks right and is not.
			name: "zip plus four",
			in:   " 100 Main St, Houston, TX 77002-1234",
			addr: "100 Main St", city: "Houston", state: "TX",
		},
		{
			// A state has to be two UPPERCASE letters. Every feed writes it
			// that way, and without the case requirement any two-letter word
			// in the segment becomes a state: "de", "la", "el" all appear in
			// Houston street and place names. Dropping a lowercase "tx" is
			// the cost, and missing beats wrong on an address.
			name: "lowercase is not a state",
			in:   " 100 Main St, Houston, tx 77002",
			addr: "100 Main St", city: "Houston", zip: "77002",
		},
		{
			name: "a two-letter word is not a state",
			in:   " 100 Main St, Houston, de 77002",
			addr: "100 Main St", city: "Houston", zip: "77002",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, c, s, z := splitFlatAddress(tc.in)
			if a != tc.addr || c != tc.city || s != tc.state || z != tc.zip {
				t.Errorf("splitFlatAddress(%q) = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
					tc.in, a, c, s, z, tc.addr, tc.city, tc.state, tc.zip)
			}
		})
	}
}

// A curated venue's address is canonical and a feed's is not. Ion's own
// payload spells its street three ways across five rooms and sometimes omits
// the state or the zip, so letting a feed win would make the address change
// depending on which room was booked.
func TestCuratedAddressBeatsTheFeed(t *testing.T) {
	n := New(venueRegistry(), nil)

	venue, room := n.resolveVenue([]core.RawVenue{{
		Name:    "Ion – Conference Room 028",
		Address: "4201 Main St.", // the feed's third spelling
		City:    "Houston",
	}})
	if venue.Name != "Ion" || room != "Conference Room 028" {
		t.Fatalf("resolved to (%q, %q)", venue.Name, room)
	}
	if venue.Address != "4201 Main St" {
		t.Errorf("address = %q, want the curated spelling", venue.Address)
	}
}

// A venue nobody curated keeps whatever its feed said, because that beats a
// bare name. Ion's payload carries structured address fields, so Second
// Draught arrives complete.
func TestDiscoveredVenueKeepsTheFeedsAddress(t *testing.T) {
	n := New(venueRegistry(), nil)

	venue, _ := n.resolveVenue([]core.RawVenue{{
		Name: "Second Draught", Address: "4201 Main St. Suite 130",
		City: "Houston", State: "TX", Zip: "77002",
	}})
	if venue.Name != "Second Draught" {
		t.Fatalf("name = %q", venue.Name)
	}
	if venue.Address != "4201 Main St. Suite 130" || venue.Zip != "77002" {
		t.Errorf("venue = %+v, want the feed's address kept", venue)
	}
}

// And an uncurated ICS venue gets its address out of the flat string, which is
// the only place it exists.
func TestDiscoveredICSVenueRecoversItsAddress(t *testing.T) {
	n := New(venueRegistry(), nil)

	venue, room := n.resolveVenue([]core.RawVenue{{
		Name: "Finn MacCool’s Irish Bar, 1127 Eldridge Pkwy Suite 600, Houston, TX 77077, USA",
	}})
	if venue.Name != "Finn MacCool’s Irish Bar" || room != "" {
		t.Fatalf("resolved to (%q, %q)", venue.Name, room)
	}
	if venue.Address != "1127 Eldridge Pkwy Suite 600" || venue.City != "Houston" ||
		venue.State != "TX" || venue.Zip != "77077" {
		t.Errorf("venue = %+v, want the address recovered from the LOCATION string", venue)
	}
}

// Three of the Meetup feeds send no LOCATION at all, so without a default
// their events have no venue even after they publish, and a discovery site
// that cannot say where to go has not solved the problem.
func TestGroupDefaultVenue(t *testing.T) {
	reg := venueRegistry()
	for i := range reg.Groups {
		if reg.Groups[i].Slug == "houston-linux-user-group" {
			reg.Groups[i].VenueSlug = "improving-houston"
		}
	}
	n := New(reg, nil)
	start := at("2026-10-01T18:00:00Z")

	t.Run("used when the feed names no venue", func(t *testing.T) {
		events, _, _ := n.Events([]core.RawEvent{
			{SourceKey: hlugFeed, UpstreamID: "a", Title: "No location", Start: start},
		})
		if len(events) != 1 {
			t.Fatalf("got %d events", len(events))
		}
		if events[0].Venue.Name != "Improving Houston" {
			t.Errorf("venue = %q, want the group's default", events[0].Venue.Name)
		}
		// The whole curated record, not just a name.
		if events[0].Venue.Address == "" && venueRegistry().Venues[1].Address != "" {
			t.Error("the default venue arrived without its address")
		}
	})

	// A default, not an override. The feed knows about the week the meeting
	// moved and sources.yaml does not.
	t.Run("a feed that names a venue wins", func(t *testing.T) {
		events, _, _ := n.Events([]core.RawEvent{
			{SourceKey: hlugFeed, UpstreamID: "b", Title: "Moved this week", Start: start,
				Venues: []core.RawVenue{{Name: "Ion – Conference Room 030"}}},
		})
		if len(events) != 1 {
			t.Fatalf("got %d events", len(events))
		}
		if events[0].Venue.Name != "Ion" || events[0].Room != "Conference Room 030" {
			t.Errorf("venue = (%q, %q), want the feed's", events[0].Venue.Name, events[0].Room)
		}
	})

	t.Run("a group with no default is left alone", func(t *testing.T) {
		events, _, _ := n.Events([]core.RawEvent{
			{SourceKey: hossFeed, UpstreamID: "c", Title: "No location, no default", Start: start},
		})
		if len(events) != 1 {
			t.Fatalf("got %d events", len(events))
		}
		if events[0].Venue.Name != "" {
			t.Errorf("venue = %q, want none invented", events[0].Venue.Name)
		}
	})
}
