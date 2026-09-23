package source

import (
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

var bevyBase = mustURL("https://usergroups.snowflake.com/houston/")

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// The Houston chapter page, saved 2026-09-23: one upcoming event and three
// past ones, in the order the page lists them.
func TestParseBevyChapterAgainstFixture(t *testing.T) {
	links, err := ParseBevyChapter(openFixture(t, "bevy-chapter.html"), bevyBase)
	if err != nil {
		t.Fatalf("ParseBevyChapter: %v", err)
	}
	const p = "https://usergroups.snowflake.com/events/details/snowflake-houston-presents-"
	want := []string{
		p + "houston-user-group-relaunch-kickoff-meeting/",
		p + "houston-user-group-meeting-data-governance-at-conocophillips/",
		p + "snowflake-houston-user-group-meeting/",
		p + "houston-virtual-user-group-how-tailored-brands-made-the-leap-from-on-prem-to-snowflake/",
	}
	if strings.Join(links, "\n") != strings.Join(want, "\n") {
		t.Errorf("links =\n  %s\nwant\n  %s", strings.Join(links, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestParseBevyChapterFollowsOnlyEventPagesOnItsOwnHost(t *testing.T) {
	doc := `<html><body>
		<a href="/events/details/a/">relative</a>
		<a href="https://usergroups.snowflake.com/events/details/a/">same page, absolute</a>
		<a href="/events/details/b/?utm_source=x#rsvp">query and fragment</a>
		<a href="https://evil.example/events/details/c/">another host</a>
		<a href="http://usergroups.snowflake.com/events/details/d/">downgraded scheme</a>
		<a href="/e/mrv45p/">short link</a>
		<a href="/events/details/e/attendees/">not an event page</a>
		<a href="/houston/">the chapter</a>
		<a>no href</a>
	</body></html>`
	links, err := ParseBevyChapter(strings.NewReader(doc), bevyBase)
	if err != nil {
		t.Fatalf("ParseBevyChapter: %v", err)
	}
	want := []string{
		"https://usergroups.snowflake.com/events/details/a/",
		"https://usergroups.snowflake.com/events/details/b/",
	}
	if strings.Join(links, " ") != strings.Join(want, " ") {
		t.Errorf("links = %v, want %v", links, want)
	}
}

// Every chapter lists its past events, so a page with none is a redesign.
// Returning no links without an error would cancel the chapter's events.
func TestParseBevyChapterWithNoEventLinksIsFatal(t *testing.T) {
	_, err := ParseBevyChapter(strings.NewReader(`<html><body><a href="/houston/">x</a></body></html>`), bevyBase)
	if err == nil || !strings.Contains(err.Error(), "page structure has changed") {
		t.Fatalf("err = %v, want a structure-change error", err)
	}
}

// The relaunch meeting the chapter asked to have listed.
func TestParseBevyEventAgainstFixture(t *testing.T) {
	e, err := ParseBevyEvent(openFixture(t, "bevy-event.html"))
	if err != nil {
		t.Fatalf("ParseBevyEvent: %v", err)
	}

	const page = "https://usergroups.snowflake.com/events/details/snowflake-houston-presents-houston-user-group-relaunch-kickoff-meeting/"
	if e.Title != "Houston User Group Relaunch Kickoff Meeting" {
		t.Errorf("Title = %q", e.Title)
	}
	// 18:00 at -05:00. The offset is in the data, so nothing is guessed.
	if want := time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC); !e.Start.Equal(want) || e.Start.Location() != time.UTC {
		t.Errorf("Start = %s, want %s in UTC", e.Start, want)
	}
	if want := time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC); !e.End.Equal(want) {
		t.Errorf("End = %s, want %s", e.End, want)
	}
	if e.URL != page {
		t.Errorf("URL = %q, want the canonical page", e.URL)
	}
	if want := strings.TrimPrefix(page, "https://"); e.UpstreamID != want {
		t.Errorf("UpstreamID = %q, want %q", e.UpstreamID, want)
	}
	if e.Virtual || e.VirtualURL != "" {
		t.Errorf("Virtual = %v, VirtualURL = %q; this is an in-person meeting", e.Virtual, e.VirtualURL)
	}
	if !strings.HasPrefix(e.Description, "🚀 Mastering CoCo at Scale") {
		t.Errorf("Description = %q", e.Description)
	}

	if len(e.Venues) != 1 {
		t.Fatalf("Venues = %+v, want one", e.Venues)
	}
	v := e.Venues[0]
	if v.Name != "NOV" || v.Address != "10353 Richmond Avenue" || v.City != "Houston" || v.State != "TX" || v.Zip != "77042" {
		t.Errorf("venue = %+v", v)
	}

	// Bevy's organizer is the platform, not the chapter, and its performer is
	// a person. Neither belongs in a RawEvent.
	if len(e.Organizers) != 0 {
		t.Errorf("Organizers = %+v, want none", e.Organizers)
	}
}

func TestParseBevyEventVirtualFixture(t *testing.T) {
	e, err := ParseBevyEvent(openFixture(t, "bevy-event-virtual.html"))
	if err != nil {
		t.Fatalf("ParseBevyEvent: %v", err)
	}
	if !e.Virtual {
		t.Error("Virtual = false, want true")
	}
	if e.VirtualURL != "https://usergroups.snowflake.com/e/m4pq7x/" {
		t.Errorf("VirtualURL = %q", e.VirtualURL)
	}
	if len(e.Venues) != 0 {
		t.Errorf("Venues = %+v, want none for an online event", e.Venues)
	}
	// The apostrophe arrives as a plain character in JSON; nothing to
	// unescape, and nothing should be mangled.
	if !strings.Contains(e.Title, "Tailored Brands' Made the Leap") {
		t.Errorf("Title = %q", e.Title)
	}
	if want := time.Date(2020, 6, 23, 20, 30, 0, 0, time.UTC); !e.Start.Equal(want) {
		t.Errorf("Start = %s, want %s", e.Start, want)
	}
}

// bevyPage wraps JSON-LD blocks in the minimum a Bevy event page has.
func bevyPage(canonical string, blocks ...string) string {
	var b strings.Builder
	b.WriteString(`<html><head>`)
	if canonical != "" {
		b.WriteString(`<link rel="canonical" href="` + canonical + `">`)
	}
	for _, blk := range blocks {
		b.WriteString(`<script type="application/ld+json">` + blk + `</script>`)
	}
	b.WriteString(`</head><body></body></html>`)
	return b.String()
}

const bevyCanonical = "https://usergroups.snowflake.com/events/details/x/"

func TestParseBevyEventShapes(t *testing.T) {
	cases := []struct {
		name  string
		doc   string
		check func(t *testing.T, title string, venues int, virtual bool, street string)
	}{
		{
			name: "@type as an array",
			doc:  bevyPage(bevyCanonical, `{"@type":["Event","SocialEvent"],"name":"A","startDate":"2026-10-08T18:00:00-05:00"}`),
		},
		{
			name: "block holding an array",
			doc:  bevyPage(bevyCanonical, `[{"@type":"BreadcrumbList"},{"@type":"Event","name":"A","startDate":"2026-10-08T18:00:00-05:00"}]`),
		},
		{
			// A site can carry JSON-LD this has no use for, broken or not,
			// and it must not cost the event.
			name: "unrelated and malformed blocks before the event",
			doc: bevyPage(bevyCanonical,
				`{"@type":"Organization","name":"x"}`,
				`{not json`,
				`{"@type":"Event","name":"A","startDate":"2026-10-08T18:00:00-05:00"}`),
		},
		{
			name: "address as a plain string",
			doc:  bevyPage(bevyCanonical, `{"@type":"Event","name":"A","startDate":"2026-10-08T18:00:00-05:00","location":{"@type":"Place","name":"Hall","address":"1 Main St, Houston"}}`),
			check: func(t *testing.T, _ string, venues int, _ bool, street string) {
				if venues != 1 || street != "1 Main St, Houston" {
					t.Errorf("venues = %d, street = %q; want the string kept whole", venues, street)
				}
			},
		},
		{
			name: "hybrid event is virtual and keeps its venue",
			doc: bevyPage(bevyCanonical, `{"@type":"Event","name":"A","startDate":"2026-10-08T18:00:00-05:00",
				"eventAttendanceMode":"https://schema.org/MixedEventAttendanceMode",
				"location":[{"@type":"Place","name":"Hall"},{"@type":"VirtualLocation","url":"https://usergroups.snowflake.com/e/abc/"}]}`),
			check: func(t *testing.T, _ string, venues int, virtual bool, _ string) {
				if venues != 1 || !virtual {
					t.Errorf("venues = %d, virtual = %v; want one venue and virtual", venues, virtual)
				}
			},
		},
		{
			// The attendance mode alone is enough. A location is optional in
			// schema.org, and an online event without one is still online.
			name: "online event with no location",
			doc: bevyPage(bevyCanonical, `{"@type":"Event","name":"A","startDate":"2026-10-08T18:00:00-05:00",
				"eventAttendanceMode":"https://schema.org/OnlineEventAttendanceMode"}`),
			check: func(t *testing.T, _ string, venues int, virtual bool, _ string) {
				if venues != 0 || !virtual {
					t.Errorf("venues = %d, virtual = %v; want no venue and virtual", venues, virtual)
				}
			},
		},
		{
			name: "entities in the name are unescaped",
			doc:  bevyPage(bevyCanonical, `{"@type":"Event","name":"Q&amp;A Night","startDate":"2026-10-08T18:00:00-05:00"}`),
			check: func(t *testing.T, title string, _ int, _ bool, _ string) {
				if title != "Q&A Night" {
					t.Errorf("Title = %q", title)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := ParseBevyEvent(strings.NewReader(tc.doc))
			if err != nil {
				t.Fatalf("ParseBevyEvent: %v", err)
			}
			street := ""
			if len(e.Venues) > 0 {
				street = e.Venues[0].Address
			}
			if tc.check != nil {
				tc.check(t, e.Title, len(e.Venues), e.Virtual, street)
			}
		})
	}
}

func TestParseBevyEventFailures(t *testing.T) {
	const ok = `{"@type":"Event","name":"A","startDate":"2026-10-08T18:00:00-05:00"}`
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"no JSON-LD at all", bevyPage(bevyCanonical), "no schema.org Event"},
		{"JSON-LD but no Event", bevyPage(bevyCanonical, `{"@type":"Organization"}`), "no schema.org Event"},
		{"no canonical link", bevyPage("", ok), "canonical"},
		{"canonical is not a URL", bevyPage("javascript:alert(1)", ok), "canonical"},
		{"no name", bevyPage(bevyCanonical, `{"@type":"Event","startDate":"2026-10-08T18:00:00-05:00"}`), "no name"},
		// A floating time would need a zone guessed for it. Bevy always
		// sends an offset, so one without is a format change.
		{"start with no offset", bevyPage(bevyCanonical, `{"@type":"Event","name":"A","startDate":"2026-10-08T18:00:00"}`), "startDate"},
		{"date-only start", bevyPage(bevyCanonical, `{"@type":"Event","name":"A","startDate":"2026-10-08"}`), "startDate"},
		{"location of the wrong shape", bevyPage(bevyCanonical, `{"@type":"Event","name":"A","startDate":"2026-10-08T18:00:00-05:00","location":42}`), "location"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBevyEvent(strings.NewReader(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseBevyEventCancelled(t *testing.T) {
	doc := bevyPage(bevyCanonical, `{"@type":"Event","name":"A","startDate":"2026-10-08T18:00:00-05:00","eventStatus":"https://schema.org/EventCancelled"}`)
	if _, err := ParseBevyEvent(strings.NewReader(doc)); !errors.Is(err, ErrBevyCancelled) {
		t.Fatalf("err = %v, want ErrBevyCancelled", err)
	}
}

// An unreadable end costs the end, not the event.
func TestParseBevyEventToleratesABadEnd(t *testing.T) {
	doc := bevyPage(bevyCanonical, `{"@type":"Event","name":"A","startDate":"2026-10-08T18:00:00-05:00","endDate":"soon"}`)
	e, err := ParseBevyEvent(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("ParseBevyEvent: %v", err)
	}
	if !e.End.IsZero() {
		t.Errorf("End = %s, want zero", e.End)
	}
}
