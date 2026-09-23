package source

import (
	"os"
	"strings"
	"testing"
	"time"
)

func hossDoc(cards ...string) string {
	return `<!doctype html><html><body><main>` + strings.Join(cards, "") + `</main></body></html>`
}

// hossCard mirrors the real markup, including the unquoted attribute values
// the site's generator emits. html.Parse handles those; a regex over the same
// document would need to handle both forms.
func hossCard(week, venue, datetime string) string {
	t := ""
	if datetime != "" {
		t = `<div class=meeting-next-date>Next: <time datetime=` + datetime + `>label</time></div>`
	}
	return `<div class=meeting-card-compact>` +
		`<div class=meeting-week>` + week + `</div>` +
		`<div class=meeting-location><a href=/meetings/venues/x/>` + venue + `</a></div>` +
		t + `</div>`
}

func parseHOSS(t *testing.T, doc string, from, until time.Time) HOSSPage {
	t.Helper()
	page, err := ParseHOSS(strings.NewReader(doc), from, until)
	if err != nil {
		t.Fatalf("ParseHOSS: %v", err)
	}
	return page
}

// The whole decoder against the real page, saved 2026-09-17.
func TestParseHOSSAgainstFixture(t *testing.T) {
	f, err := os.Open("testdata/hoss-meetings.html")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	page, err := ParseHOSS(f, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ParseHOSS: %v", err)
	}
	if len(page.Skipped) != 0 {
		t.Fatalf("skipped = %v, want none", page.Skipped)
	}

	// Five weekly slots, in document order rather than date order. The page
	// lists First through Fifth Wednesday, and the next date for each, so
	// Fourth Wednesday is the soonest and appears fourth.
	want := []struct {
		title string
		start time.Time
		venue string
		id    string
	}{
		{"Houston Open Source Society, First Wednesday", time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC), "Improving Houston", "first-wednesday_20261007T230000Z"},
		{"Houston Open Source Society, Second Wednesday", time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC), "Zion Lutheran Church", "second-wednesday_20261014T230000Z"},
		{"Houston Open Source Society, Third Wednesday", time.Date(2026, 10, 21, 23, 0, 0, 0, time.UTC), "The Ion", "third-wednesday_20261021T230000Z"},
		{"Houston Open Source Society, Fourth Wednesday", time.Date(2026, 9, 23, 23, 0, 0, 0, time.UTC), "Bayland Community Center", "fourth-wednesday_20260923T230000Z"},
		{"Houston Open Source Society, Fifth Wednesday", time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC), "Bayland Community Center", "fifth-wednesday_20260930T230000Z"},
	}

	if len(page.Events) != len(want) {
		t.Fatalf("got %d events, want %d", len(page.Events), len(want))
	}
	for i, w := range want {
		got := page.Events[i]
		if got.Title != w.title {
			t.Errorf("event %d title = %q, want %q", i, got.Title, w.title)
		}
		// 18:00 with a -05:00 offset is 23:00 UTC. The offset is in the
		// attribute, so unlike the ICS floating case there is no zone to guess.
		if !got.Start.Equal(w.start) {
			t.Errorf("event %d start = %s, want %s", i, got.Start.Format(time.RFC3339), w.start.Format(time.RFC3339))
		}
		if got.UpstreamID != w.id {
			t.Errorf("event %d id = %q, want %q", i, got.UpstreamID, w.id)
		}
		if len(got.Venues) != 1 || got.Venues[0].Name != w.venue {
			t.Errorf("event %d venues = %+v, want one named %q", i, got.Venues, w.venue)
		}
		if !got.End.IsZero() {
			t.Errorf("event %d has an end time; the page publishes none", i)
		}
	}

	// The page repeats whichever meeting is soonest in a next-meeting-highlight
	// block. Six <time> elements, five events.
	ids := map[string]bool{}
	for _, e := range page.Events {
		if ids[e.UpstreamID] {
			t.Errorf("duplicate UpstreamID %q: the highlight block was counted twice", e.UpstreamID)
		}
		ids[e.UpstreamID] = true
	}

	// The Third Wednesday venue is already an alias on the curated `ion` venue,
	// which is the join Phase 5 will make.
	if page.Events[2].Venues[0].Name != "The Ion" {
		t.Errorf("third Wednesday venue = %q, want the string the registry already aliases",
			page.Events[2].Venues[0].Name)
	}
}

// A page that still parses as HTML but has none of the structure we rely on
// means the site was redesigned. Reporting zero events would read downstream
// as HOSS cancelling everything at once, which is the failure mode the
// absence-means-cancelled contract exists to avoid.
func TestParseHOSSRejectsAPageWithoutCards(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"empty document", ""},
		{"valid html, no cards", `<html><body><h1>Meetings</h1><p>See our Discord.</p></body></html>`},
		{"a redesign that renamed the class", hossDoc(strings.Replace(
			hossCard("First Wednesday", "Improving Houston", "2026-10-07T18:00:00-05:00"),
			"meeting-card-compact", "meeting-card-v2", 1))},
		{"json served with the wrong content type", `{"meetings": []}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseHOSS(strings.NewReader(tc.doc), testFrom, testUntil)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), "structure has changed") {
				t.Errorf("err = %v, want it to say the page structure changed", err)
			}
		})
	}
}

func TestParseHOSSCardHandling(t *testing.T) {
	const good = "2026-06-03T18:00:00-05:00"

	cases := []struct {
		name       string
		cards      []string
		wantEvents int
		wantSkips  int
		wantErrTxt string
	}{
		{
			name:       "a slot with no next date is not a failure",
			cards:      []string{hossCard("First Wednesday", "Improving Houston", good), hossCard("Fifth Wednesday", "Bayland", "")},
			wantEvents: 1,
		},
		{
			// Single token on purpose: the real page emits unquoted attribute
			// values, and HTML truncates those at the first space, so a
			// two-word value would arrive here as one word anyway.
			name:       "an unparseable datetime is skipped and counted",
			cards:      []string{hossCard("First Wednesday", "Improving", good), hossCard("Second Wednesday", "Zion", "not-a-date")},
			wantEvents: 1,
			wantSkips:  1,
			wantErrTxt: "not-a-date",
		},
		{
			name:       "a time element with no datetime attribute",
			cards:      []string{hossCard("First Wednesday", "Improving", good), `<div class=meeting-card-compact><div class=meeting-week>Second Wednesday</div><time>soon</time></div>`},
			wantEvents: 1,
			wantSkips:  1,
			wantErrTxt: "datetime",
		},
		{
			name:       "a card with no week label",
			cards:      []string{hossCard("First Wednesday", "Improving", good), `<div class=meeting-card-compact><time datetime=` + good + `>x</time></div>`},
			wantEvents: 1,
			wantSkips:  1,
			wantErrTxt: "meeting-week",
		},
		{
			name:       "a card with no venue still yields an event",
			cards:      []string{`<div class=meeting-card-compact><div class=meeting-week>First Wednesday</div><time datetime=` + good + `>x</time></div>`},
			wantEvents: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := parseHOSS(t, hossDoc(tc.cards...),
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))

			if len(page.Events) != tc.wantEvents {
				t.Errorf("got %d events, want %d", len(page.Events), tc.wantEvents)
			}
			if len(page.Skipped) != tc.wantSkips {
				t.Fatalf("got %d skips, want %d: %v", len(page.Skipped), tc.wantSkips, page.Skipped)
			}
			if tc.wantErrTxt != "" && !strings.Contains(page.Skipped[0].Error(), tc.wantErrTxt) {
				t.Errorf("skip = %v, want it to mention %q", page.Skipped[0], tc.wantErrTxt)
			}
		})
	}
}

// Out of window is dropped, not skipped, for the same reason it is in the ICS
// decoder: a non-empty Skipped defers cancellation for the whole source.
func TestParseHOSSFiltersToTheWindowWithoutSkipping(t *testing.T) {
	page := parseHOSS(t, hossDoc(
		hossCard("First Wednesday", "Improving", "2020-01-01T18:00:00-06:00"),
		hossCard("Second Wednesday", "Zion", "2026-06-10T18:00:00-05:00"),
		hossCard("Third Wednesday", "The Ion", "2030-01-01T18:00:00-06:00"),
	), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))

	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(page.Events))
	}
	if page.Events[0].Title != "Houston Open Source Society, Second Wednesday" {
		t.Errorf("kept the wrong event: %q", page.Events[0].Title)
	}
	if len(page.Skipped) != 0 {
		t.Errorf("skipped = %v, want none: out of window is not a failure", page.Skipped)
	}
}

// Two cards landing on the same instant are the same meeting listed twice,
// which is what the next-meeting-highlight block does on the real page.
func TestParseHOSSDeduplicatesRepeatedInstants(t *testing.T) {
	page := parseHOSS(t, hossDoc(
		hossCard("Fourth Wednesday", "Bayland", "2026-06-24T18:00:00-05:00"),
		hossCard("Next Meeting", "Bayland", "2026-06-24T18:00:00-05:00"),
	), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))

	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1: the highlight repeats a card", len(page.Events))
	}
}

// The offset in the attribute is authoritative, which is what makes this
// source workable at all: nothing has to be guessed about the zone.
func TestParseHOSSRespectsTheOffset(t *testing.T) {
	cases := []struct {
		datetime string
		want     time.Time
	}{
		{"2026-10-07T18:00:00-05:00", time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC)}, // CDT
		{"2026-12-02T18:00:00-06:00", time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC)},  // CST
		{"2026-10-07T23:00:00Z", time.Date(2026, 10, 7, 23, 0, 0, 0, time.UTC)},      // already UTC
	}
	for _, tc := range cases {
		t.Run(tc.datetime, func(t *testing.T) {
			page := parseHOSS(t, hossDoc(hossCard("First Wednesday", "Improving", tc.datetime)),
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
			if len(page.Events) != 1 {
				t.Fatalf("got %d events, want 1 (%v)", len(page.Events), page.Skipped)
			}
			if !page.Events[0].Start.Equal(tc.want) {
				t.Errorf("start = %s, want %s", page.Events[0].Start.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			if page.Events[0].Start.Location() != time.UTC {
				t.Errorf("start location = %v, want UTC in the domain type", page.Events[0].Start.Location())
			}
		})
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"First Wednesday", "first-wednesday"},
		{"Fifth Wednesday", "fifth-wednesday"},
		{"  Third   Wednesday  ", "third-wednesday"},
		{"Next Meeting", "next-meeting"},
		{"Wednesday/Fourth", "wednesday-fourth"},
		{"2nd Tuesday", "2nd-tuesday"},
		{"Café Night", "caf-night"}, // non-ASCII dropped: this becomes an ICS UID
		{"---", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := slugify(tc.in); got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Whitespace collapsing is not cosmetic: the generator minifies, so a venue
// name arrives wrapped in whatever indentation the line break left behind.
func TestTextOfCollapsesWhitespace(t *testing.T) {
	page := parseHOSS(t, hossDoc(
		`<div class=meeting-card-compact>`+
			"<div class=meeting-week>\n   First\n   Wednesday\n  </div>"+
			"<div class=meeting-location>\n  <a href=/x/>\n   Improving   Houston\n  </a>\n </div>"+
			`<time datetime=2026-06-03T18:00:00-05:00>x</time></div>`,
	), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))

	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1 (%v)", len(page.Events), page.Skipped)
	}
	if got := page.Events[0].Venues[0].Name; got != "Improving Houston" {
		t.Errorf("venue = %q, want whitespace collapsed", got)
	}
	if got := page.Events[0].Title; got != "Houston Open Source Society, First Wednesday" {
		t.Errorf("title = %q, want whitespace collapsed", got)
	}
}

// findByClass stops at a match rather than descending into it. Nesting is not
// something the current page does, but a layout change that wrapped a card in
// another card would otherwise report the same meeting twice, and the
// duplicate would carry a different slot label so the instant-level
// deduplication would not catch it either.
func TestFindByClassDoesNotDescendIntoAMatch(t *testing.T) {
	nested := `<div class=meeting-card-compact>` +
		`<div class=meeting-week>First Wednesday</div>` +
		`<div class=meeting-location><a href=/x/>Improving</a></div>` +
		`<time datetime=2026-06-03T18:00:00-05:00>x</time>` +
		// A second card inside the first.
		`<div class=meeting-card-compact>` +
		`<div class=meeting-week>Second Wednesday</div>` +
		`<time datetime=2026-06-10T18:00:00-05:00>y</time></div>` +
		`</div>`

	page := parseHOSS(t, hossDoc(nested),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))

	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1: the inner card is inside the outer one", len(page.Events))
	}
	if page.Events[0].Title != "Houston Open Source Society, First Wednesday" {
		t.Errorf("title = %q, want the outer card's", page.Events[0].Title)
	}
}

// class is a space-separated list, so matching has to be on whole tokens. A
// substring test would treat a redesign's meeting-card-compact-wide as a card
// and quietly keep producing events from markup nobody verified.
func TestClassMatchingIsWholeTokens(t *testing.T) {
	cases := []struct {
		name      string
		class     string
		wantMatch bool
	}{
		{"exact", "meeting-card-compact", true},
		{"one of several", "card meeting-card-compact featured", true},
		{"extra whitespace", "  meeting-card-compact  ", true},
		{"longer class with the name as a prefix", "meeting-card-compact-wide", false},
		{"longer class with the name as a suffix", "legacy-meeting-card-compact", false},
		{"unrelated", "meeting-card-map", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := hossDoc(`<div class="` + tc.class + `">` +
				`<div class=meeting-week>First Wednesday</div>` +
				`<time datetime=2026-06-03T18:00:00-05:00>x</time></div>`)

			page, err := ParseHOSS(strings.NewReader(doc),
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))

			if !tc.wantMatch {
				// No cards at all, which is the redesign signal.
				if err == nil {
					t.Fatalf("class %q was treated as a card, want it ignored", tc.class)
				}
				return
			}
			if err != nil {
				t.Fatalf("class %q: %v", tc.class, err)
			}
			if len(page.Events) != 1 {
				t.Errorf("class %q: got %d events, want 1", tc.class, len(page.Events))
			}
		})
	}
}
