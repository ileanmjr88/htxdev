package normalize

import (
	"strings"
	"testing"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
	"github.com/ileanmjr88/htxdev/internal/registry"
)

// The three feeds involved in every interesting case, with the priorities
// the registry actually assigns: a group's own calendar is 10, a venue's
// listing of it is 50.
const (
	ionFeed  = "https://iondistrict.com/wp-json/tribe/events/v1/events"
	hlugFeed = "https://calendar.google.com/calendar/ical/houstonlinuxusergroup%40gmail.com/public/basic.ics"
	hossFeed = "https://houstonopensourcesociety.com/meetings/"
)

func testRegistry() *registry.Registry {
	return &registry.Registry{
		Groups: []core.Group{
			{Slug: "ion-district", Name: "Ion District", Category: "startup"},
			{
				Slug: "houston-linux-user-group",
				Name: "Houston Linux User Group",
				// Verbatim from the registry in data/, curly apostrophe included.
				Aliases:  []string{"Houston Linux User’s Group", "Houston Linux", "HLUG"},
				Category: "dev",
			},
			{Slug: "houston-open-source-society", Name: "Houston Open Source Society",
				Aliases: []string{"HOSS", "Houston OSS"}, Category: "dev"},
		},
		Sources: []core.Source{
			{GroupSlug: "ion-district", Kind: core.KindTribe, URL: ionFeed, Priority: 50, Enabled: true},
			{GroupSlug: "houston-linux-user-group", Kind: core.KindICS, URL: hlugFeed, Priority: 10, Enabled: true},
			{GroupSlug: "houston-open-source-society", Kind: core.KindHTML, URL: hossFeed, Priority: 10, Enabled: true},
		},
	}
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// The exact cluster in the live database on 2026-09-17. Three events at one
// instant, from two feeds, of which exactly two are the same meeting.
func realCluster() []core.RawEvent {
	start := at("2026-09-24T23:00:00Z")
	return []core.RawEvent{
		{
			SourceKey: ionFeed, UpstreamID: "iondistrict.com?id=1",
			Title: "NASA Tech Talks: Enhancing Autonomous Onboard Navigation Systems",
			Start: start,
			// Ion's own programming: the organizer is the venue itself.
			Organizers: []core.RawOrganizer{{Name: "Ion"}},
			Venues:     []core.RawVenue{{Name: "Ion"}},
		},
		{
			SourceKey: ionFeed, UpstreamID: "iondistrict.com?id=2",
			Title: "Houston Linux User Group",
			Start: start,
			// The only thing that says whose event this is.
			Organizers: []core.RawOrganizer{{Name: "Houston Linux User’s Group"}},
			Venues:     []core.RawVenue{{Name: "Ion – Conference Room 030"}},
			URL:        "https://iondistrict.com/event/hlug/",
		},
		{
			SourceKey: hlugFeed, UpstreamID: "1vb0jpkre7nnn4vr4g4u2ekoh1@google.com",
			Title: "Houston Linux - Ion User Meeting",
			Start: start,
			// ICS carries no organizer at all, and HLUG's flat LOCATION string
			// is the worse venue record even though this feed wins on priority.
			Venues: []core.RawVenue{{Name: "The Ion, Room 30, 4201 Main St, Houston, TX 77002, USA"}},
		},
	}
}

func TestDeduplicatesTheRealCluster(t *testing.T) {
	events, _, problems := New(testRegistry(), nil).Events(realCluster())
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: three records, two of them one meeting", len(events))
	}

	byGroup := map[string]core.Event{}
	for _, e := range events {
		byGroup[e.GroupSlug] = e
	}

	nasa, ok := byGroup["ion-district"]
	if !ok {
		t.Fatal("no event attributed to ion-district")
	}
	if !strings.HasPrefix(nasa.Title, "NASA Tech Talks") {
		t.Errorf("ion-district event = %q, want the NASA talk", nasa.Title)
	}
	if len(nasa.Sources) != 1 {
		t.Errorf("NASA talk has %d sources, want 1: nothing else published it", len(nasa.Sources))
	}

	hlug, ok := byGroup["houston-linux-user-group"]
	if !ok {
		t.Fatal("no event attributed to houston-linux-user-group")
	}
	// HLUG's own feed is priority 10 and supplies identity.
	if hlug.Title != "Houston Linux - Ion User Meeting" {
		t.Errorf("title = %q, want the organizer's own wording", hlug.Title)
	}
	if hlug.Fingerprint != core.Fingerprint(core.KindICS, "1vb0jpkre7nnn4vr4g4u2ekoh1@google.com") {
		t.Errorf("fingerprint = %q, want the winning record's", hlug.Fingerprint)
	}
	if len(hlug.Sources) != 2 || hlug.Sources[0].SourceKey != hlugFeed || hlug.Sources[1].SourceKey != ionFeed {
		t.Errorf("sources = %+v, want the winner first then Ion", hlug.Sources)
	}
	// Each contributing record keeps its own fingerprint, which is what lets
	// the store find this event by any of them and keep events.fingerprint
	// write-once while the merge winner is free to change.
	if hlug.Sources[1].Fingerprint != core.Fingerprint(core.KindTribe, "iondistrict.com?id=2") {
		t.Errorf("Ion's provenance fingerprint = %q", hlug.Sources[1].Fingerprint)
	}
	// Gap-filled: HLUG's ICS has no URL, Ion's record does.
	if hlug.URL != "https://iondistrict.com/event/hlug/" {
		t.Errorf("url = %q, want it filled from the losing record", hlug.URL)
	}
}

// The case that makes start-time-alone wrong. HLUG and HOSS both meet
// Wednesdays at 6pm Central, so they collide on three separate instants in one
// live window and are never the same meeting.
func TestSameInstantDifferentGroupsStaySeparate(t *testing.T) {
	start := at("2026-10-21T23:00:00Z")
	events, _, problems := New(testRegistry(), nil).Events([]core.RawEvent{
		{SourceKey: hlugFeed, UpstreamID: "hlug-oct21", Title: "Houston Linux - User Meeting", Start: start},
		{SourceKey: hossFeed, UpstreamID: "third-wednesday_20261021T230000Z",
			Title: "Houston Open Source Society, Third Wednesday", Start: start},
	})
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: two groups meeting at the same hour", len(events))
	}
	if events[0].GroupSlug == events[1].GroupSlug {
		t.Errorf("both events landed on %q", events[0].GroupSlug)
	}
}

func TestGroupResolution(t *testing.T) {
	start := at("2026-10-01T18:00:00Z")
	cases := []struct {
		name      string
		feed      string
		organizer string
		want      string
	}{
		{"organizer names a known group", ionFeed, "Houston Linux User’s Group", "houston-linux-user-group"},
		{"straight apostrophe matches too", ionFeed, "Houston Linux User's Group", "houston-linux-user-group"},
		{"case does not matter", ionFeed, "houston linux user's group", "houston-linux-user-group"},
		{"an alias is enough", ionFeed, "HLUG", "houston-linux-user-group"},
		{"extra whitespace folded", ionFeed, "  Houston   Linux  ", "houston-linux-user-group"},
		{"the group's own slug", ionFeed, "houston-open-source-society", "houston-open-source-society"},
		// "Ion" is the venue's name; the group is "Ion District". No match, so
		// the feed's owner is used, which is Ion District anyway.
		{"ion's own programming falls back to the feed owner", ionFeed, "Ion", "ion-district"},
		{"an unknown organizer falls back", ionFeed, "Some Company LLC", "ion-district"},
		{"no organizer at all falls back", hlugFeed, "", "houston-linux-user-group"},
		{"a group's own feed is unaffected by a stray organizer", hlugFeed, "Some Company LLC", "houston-linux-user-group"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := core.RawEvent{SourceKey: tc.feed, UpstreamID: "x", Title: "T", Start: start}
			if tc.organizer != "" {
				e.Organizers = []core.RawOrganizer{{Name: tc.organizer}}
			}
			events, _, problems := New(testRegistry(), nil).Events([]core.RawEvent{e})
			if len(problems) != 0 {
				t.Fatalf("problems = %v", problems)
			}
			if len(events) != 1 {
				t.Fatalf("got %d events, want 1", len(events))
			}
			if events[0].GroupSlug != tc.want {
				t.Errorf("group = %q, want %q", events[0].GroupSlug, tc.want)
			}
		})
	}
}

// The first organizer that resolves wins. Ion sends one or two per event.
func TestFirstResolvableOrganizerWins(t *testing.T) {
	events, _, _ := New(testRegistry(), nil).Events([]core.RawEvent{{
		SourceKey: ionFeed, UpstreamID: "x", Title: "T", Start: at("2026-10-01T18:00:00Z"),
		Organizers: []core.RawOrganizer{{Name: "Some Company LLC"}, {Name: "HOSS"}},
	}})
	if len(events) != 1 || events[0].GroupSlug != "houston-open-source-society" {
		t.Fatalf("group = %+v, want the second organizer to have resolved", events)
	}
}

func TestMergePrefersTheLowerPriorityNumber(t *testing.T) {
	start := at("2026-10-01T18:00:00Z")
	// Deliberately listed venue-first, so a merge that just took the first
	// record would pick the wrong one.
	events, _, _ := New(testRegistry(), nil).Events([]core.RawEvent{
		{SourceKey: ionFeed, UpstreamID: "ion-copy", Title: "Ion's wording", Start: start,
			Organizers: []core.RawOrganizer{{Name: "HLUG"}}},
		{SourceKey: hlugFeed, UpstreamID: "own-copy", Title: "The organizer's wording", Start: start},
	})
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Title != "The organizer's wording" {
		t.Errorf("title = %q, want the group's own feed to win", events[0].Title)
	}
}

// Gap-filling is the half of merge worth arguing about: the winner is not
// always the richer record.
func TestMergeGapFillsFromLosers(t *testing.T) {
	start := at("2026-10-01T18:00:00Z")
	events, _, _ := New(testRegistry(), nil).Events([]core.RawEvent{
		{SourceKey: hlugFeed, UpstreamID: "own", Title: "Winner", Start: start},
		{SourceKey: ionFeed, UpstreamID: "ion", Title: "Loser", Start: start,
			Organizers:  []core.RawOrganizer{{Name: "HLUG"}},
			End:         start.Add(2 * time.Hour),
			URL:         "https://iondistrict.com/event/x/",
			RegisterURL: "https://luma.com/x",
			Virtual:     true},
	})
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	got := events[0]
	if got.Title != "Winner" {
		t.Errorf("title = %q, want the winner's", got.Title)
	}
	if !got.End.Equal(start.Add(2 * time.Hour)) {
		t.Errorf("end = %s, want it filled from the loser", got.End)
	}
	if got.URL == "" || got.RegisterURL == "" {
		t.Errorf("url = %q, registerURL = %q; want both filled", got.URL, got.RegisterURL)
	}
	// Virtual is a claim, not a gap: one feed saying so is enough, and a
	// silent feed is not a contradiction.
	if !got.Virtual {
		t.Error("virtual = false, want one feed's claim to carry")
	}
}

func TestUnregisteredSourceIsReportedNotDropped(t *testing.T) {
	start := at("2026-10-01T18:00:00Z")
	events, _, problems := New(testRegistry(), nil).Events([]core.RawEvent{
		{SourceKey: "https://nowhere.test/feed", UpstreamID: "orphan", Title: "Orphan", Start: start},
		{SourceKey: hlugFeed, UpstreamID: "fine", Title: "Fine", Start: start},
	})
	if len(events) != 1 || events[0].Title != "Fine" {
		t.Fatalf("events = %+v, want only the attributable one", events)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), "nowhere.test") {
		t.Fatalf("problems = %v, want one naming the unregistered feed", problems)
	}
}

// Same input, same output, every run. Fetch returns sources in registry order
// but events within a cluster arrive in whatever order the feeds listed them.
func TestOutputIsDeterministic(t *testing.T) {
	raw := realCluster()
	first, _, _ := New(testRegistry(), nil).Events(raw)

	// Reverse the input; the clustering must not care.
	reversed := make([]core.RawEvent, len(raw))
	for i, e := range raw {
		reversed[len(raw)-1-i] = e
	}
	second, _, _ := New(testRegistry(), nil).Events(reversed)

	if len(first) != len(second) {
		t.Fatalf("got %d then %d events", len(first), len(second))
	}
	byGroup := func(evs []core.Event) map[string]string {
		m := map[string]string{}
		for _, e := range evs {
			m[e.GroupSlug] = e.Fingerprint
		}
		return m
	}
	a, b := byGroup(first), byGroup(second)
	for g, fp := range a {
		if b[g] != fp {
			t.Errorf("group %s resolved to %q then %q", g, fp, b[g])
		}
	}
}

// The registry is human-edited by pull request, so two groups can end up
// claiming one name. First claim wins, and the point is that it wins the same
// way on every run rather than depending on map iteration order.
func TestAmbiguousAliasResolvesConsistently(t *testing.T) {
	reg := &registry.Registry{
		Groups: []core.Group{
			{Slug: "first-claim", Name: "First", Aliases: []string{"Shared Name"}},
			{Slug: "second-claim", Name: "Second", Aliases: []string{"Shared Name"}},
		},
		Sources: []core.Source{{GroupSlug: "first-claim", Kind: core.KindTribe, URL: ionFeed, Priority: 50}},
	}
	raw := []core.RawEvent{{SourceKey: ionFeed, UpstreamID: "x", Title: "T",
		Start: at("2026-10-01T18:00:00Z"), Organizers: []core.RawOrganizer{{Name: "Shared Name"}}}}

	for range 20 {
		events, _, _ := New(reg, nil).Events(raw)
		if len(events) != 1 || events[0].GroupSlug != "first-claim" {
			t.Fatalf("resolved to %+v, want first-claim every time", events)
		}
	}
}

// The real registry has to keep working with this code, and the alias that
// makes the live dedupe possible has to still be in it.
func TestAgainstTheRealRegistry(t *testing.T) {
	reg, err := registry.LoadDir("../../data")
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	n := New(reg, nil)

	start := at("2026-09-24T23:00:00Z")
	events, _, problems := n.Events([]core.RawEvent{
		{SourceKey: ionFeed, UpstreamID: "iondistrict.com?id=2", Title: "Houston Linux User Group", Start: start,
			Organizers: []core.RawOrganizer{{Name: "Houston Linux User’s Group"}}},
		{SourceKey: hlugFeed, UpstreamID: "1vb0jpkre7nnn4vr4g4u2ekoh1@google.com",
			Title: "Houston Linux - Ion User Meeting", Start: start},
	})
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: the alias in the registry is what merges these", len(events))
	}
	if events[0].GroupSlug != "houston-linux-user-group" {
		t.Errorf("group = %q", events[0].GroupSlug)
	}
}

// The other half of the cluster key. Grouping on the group alone would
// collapse every meeting a group has ever held into one event, which is the
// mirror image of grouping on the instant alone.
func TestSameGroupAtDifferentTimesStaysSeparate(t *testing.T) {
	events, _, problems := New(testRegistry(), nil).Events([]core.RawEvent{
		{SourceKey: hlugFeed, UpstreamID: "sep30", Title: "September meeting", Start: at("2026-09-30T23:00:00Z")},
		{SourceKey: hlugFeed, UpstreamID: "oct07", Title: "October meeting", Start: at("2026-10-07T23:00:00Z")},
		{SourceKey: hlugFeed, UpstreamID: "oct21", Title: "Another October meeting", Start: at("2026-10-21T23:00:00Z")},
	})
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: one group, three different nights", len(events))
	}
	seen := map[time.Time]bool{}
	for _, e := range events {
		if seen[e.Start] {
			t.Errorf("duplicate start %s", e.Start)
		}
		seen[e.Start] = true
	}
}

// The bug the live data found, and the reason the cluster key is not the whole
// story. Ion runs three different things at 10am on Fridays: NASA Office
// Hours, Mapping Houston's Innovation Ecosystem and SCORE Office Hours. None
// of their organizers resolves to a group, so all three fall back to the
// feed's owner and land in one cluster, and merging on (group, instant) alone
// published them as a single event.
//
// A merge needs one record per feed. A calendar does not list one event twice.
func TestSameFeedAtTheSameInstantIsNotAMerge(t *testing.T) {
	start := at("2026-09-25T15:00:00Z")
	// Fed in upstream-id order 65740, 64682, 64738, so the assertion on
	// output order below tests the sort rather than the input.
	events, _, problems := New(testRegistry(), nil).Events([]core.RawEvent{
		{SourceKey: ionFeed, UpstreamID: "iondistrict.com?id=65740", Title: "SCORE Office Hours", Start: start,
			Organizers: []core.RawOrganizer{{Name: "SCORE"}}},
		{SourceKey: ionFeed, UpstreamID: "iondistrict.com?id=64682", Title: "NASA Office Hours", Start: start,
			Organizers: []core.RawOrganizer{{Name: "NASA"}}},
		{SourceKey: ionFeed, UpstreamID: "iondistrict.com?id=64738", Title: "Mapping Houston's Innovation Ecosystem", Start: start},
	})
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: one feed at one instant is three different events", len(events))
	}
	for _, e := range events {
		if len(e.Sources) != 1 {
			t.Errorf("event %q has %d sources, want 1", e.Title, len(e.Sources))
		}
	}
	// Deterministic, so a split cluster does not shuffle between runs.
	if events[0].Title != "NASA Office Hours" {
		t.Errorf("first = %q, want the lowest upstream id", events[0].Title)
	}
}

// An ambiguous cluster is left alone rather than guessed at: nothing says
// which of Ion's two records the HLUG one pairs with.
func TestAmbiguousClusterIsNotMerged(t *testing.T) {
	start := at("2026-10-01T18:00:00Z")
	events, _, _ := New(testRegistry(), nil).Events([]core.RawEvent{
		{SourceKey: ionFeed, UpstreamID: "ion-a", Title: "Ion A", Start: start,
			Organizers: []core.RawOrganizer{{Name: "HLUG"}}},
		{SourceKey: ionFeed, UpstreamID: "ion-b", Title: "Ion B", Start: start,
			Organizers: []core.RawOrganizer{{Name: "HLUG"}}},
		{SourceKey: hlugFeed, UpstreamID: "own", Title: "HLUG's own", Start: start},
	})
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 left unmerged", len(events))
	}
}

func rejectList(t *testing.T, body string) *registry.Rejects {
	t.Helper()
	r, err := registry.LoadRejects(strings.NewReader(body))
	if err != nil {
		t.Fatalf("LoadRejects: %v", err)
	}
	return r
}

// Rejection happens after the merge, so rejecting ANY of an event's
// fingerprints rejects the event. Whoever wrote the entry named whichever
// feed's copy they were looking at, and should not have to know it was
// published twice.
func TestRejectingAnyFingerprintRejectsTheEvent(t *testing.T) {
	ionFP := core.Fingerprint(core.KindTribe, "iondistrict.com?id=2")
	hlugFP := core.Fingerprint(core.KindICS, "1vb0jpkre7nnn4vr4g4u2ekoh1@google.com")

	for _, fp := range []string{ionFP, hlugFP} {
		t.Run(fp, func(t *testing.T) {
			rejects := rejectList(t, "rejects:\n  - fingerprint: \""+fp+"\"\n    reason: Not for us\n")
			events, rejected, problems := New(testRegistry(), rejects).Events(realCluster())

			if len(problems) != 0 {
				t.Fatalf("problems = %v", problems)
			}
			if rejected != 1 {
				t.Errorf("rejected = %d, want 1", rejected)
			}
			// The NASA talk survives; only the merged HLUG event goes.
			if len(events) != 1 || events[0].GroupSlug != "ion-district" {
				t.Fatalf("events = %+v, want only the NASA talk", events)
			}
		})
	}
}

func TestRejectingLeavesEverythingElseAlone(t *testing.T) {
	rejects := rejectList(t, "rejects:\n  - fingerprint: \"ics:nothing-here\"\n    reason: Not for us\n")
	events, rejected, _ := New(testRegistry(), rejects).Events(realCluster())
	if rejected != 0 {
		t.Errorf("rejected = %d, want 0", rejected)
	}
	if len(events) != 2 {
		t.Errorf("got %d events, want the usual 2", len(events))
	}
}

// A nil reject list is the no-filter case and must not panic.
func TestNilRejectListRejectsNothing(t *testing.T) {
	events, rejected, _ := New(testRegistry(), nil).Events(realCluster())
	if rejected != 0 || len(events) != 2 {
		t.Errorf("got %d events and %d rejected, want 2 and 0", len(events), rejected)
	}
}
