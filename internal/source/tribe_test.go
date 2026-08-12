package source

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Ion sends the `venue` field as a single object on most events and as a
// [room, building] array on the rest. These cases pin down every shape the
// decoder can be handed, including the two that mean "no venue" without being
// failures: null and [].
func TestTribeVenuesUnmarshalJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{"single object", `{"venue":"Ion"}`, 1, false},
		{"room and building", `[{"venue":"Ion – Lobby"},{"venue":"Ion"}]`, 2, false},
		{"empty array", `[]`, 0, false},
		{"null", `null`, 0, false},
		{"string", `"just a string"`, 0, true},
		{"number", `123`, 0, true},
		{"truncated object", `{"venue":`, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got tribeVenues
			err := json.Unmarshal([]byte(tc.input), &got)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (len %d)", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.wantLen {
				t.Errorf("len = %d, want %d", len(got), tc.wantLen)
			}
		})
	}
}

// The array form carries the venue hierarchy directly: element 0 is the
// specific space, element 1 is the parent building. Phase 5 relies on both
// surviving the decode rather than being collapsed here.
func TestTribeVenuesKeepsBothElements(t *testing.T) {
	const input = `[{"venue":"Ion – Lobby","global_id":"iondistrict.com?id=28080"},
	                {"venue":"Ion","global_id":"iondistrict.com?id=1077"}]`

	var got tribeVenues
	if err := json.Unmarshal([]byte(input), &got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Venue != "Ion – Lobby" {
		t.Errorf("got[0].Venue = %q, want %q", got[0].Venue, "Ion – Lobby")
	}
	if got[1].Venue != "Ion" {
		t.Errorf("got[1].Venue = %q, want %q", got[1].Venue, "Ion")
	}
}

// The object form maps Ion's subfields, including `stateprovince` rather than
// `state` or `province`. All three appear in the feed and only stateprovince
// is populated whenever any of them is.
func TestTribeVenueFieldMapping(t *testing.T) {
	const input = `{
		"global_id": "iondistrict.com?id=1077",
		"venue": "Ion",
		"address": "4201 Main Street",
		"city": "Houston",
		"province": "TX",
		"state": "TX",
		"stateprovince": "TX",
		"zip": "77002",
		"url": "https://iondistrict.com/venue/ion/"
	}`

	var got tribeVenues
	if err := json.Unmarshal([]byte(input), &got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}

	v := got[0]
	for _, f := range []struct {
		field string
		got   string
		want  string
	}{
		{"GlobalID", v.GlobalID, "iondistrict.com?id=1077"},
		{"Venue", v.Venue, "Ion"},
		{"Address", v.Address, "4201 Main Street"},
		{"City", v.City, "Houston"},
		{"State", v.State, "TX"},
		{"Zip", v.Zip, "77002"},
		{"URL", v.URL, "https://iondistrict.com/venue/ion/"},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q, want %q", f.field, f.got, f.want)
		}
	}
}

// The regression test for the mistake in the original spec, which described
// the 11 array-shaped events as empty arrays. They are not empty: each holds
// [room, building]. Building to that description would have silently dropped
// the location on 11 events that all have one.
func TestIonFixtureVenueShapes(t *testing.T) {
	f, err := os.Open("testdata/ion-tribe.json")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	var payload struct {
		Events []struct {
			Title string      `json:"title"`
			Venue tribeVenues `json:"venue"`
		} `json:"events"`
	}
	if err := json.NewDecoder(f).Decode(&payload); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	if len(payload.Events) != 50 {
		t.Fatalf("fixture has %d events, want 50", len(payload.Events))
	}

	byCount := make(map[int]int)
	for _, e := range payload.Events {
		byCount[len(e.Venue)]++
	}

	want := map[int]int{0: 0, 1: 39, 2: 11}
	for n, wantEvents := range want {
		if byCount[n] != wantEvents {
			t.Errorf("%d events have %d venue(s), want %d", byCount[n], n, wantEvents)
		}
	}

	// Nothing outside 0, 1, 2 should exist. A three-venue event would mean the
	// [room, building] assumption no longer holds.
	for n, count := range byCount {
		if _, expected := want[n]; !expected {
			t.Errorf("unexpected: %d events have %d venues", count, n)
		}
	}
}

// Ion sends both utc_start_date and start_date, five hours apart, and both
// parse cleanly. Picking the wrong one shifts every event silently, so this
// asserts the exact instant rather than merely that parsing succeeded.
func TestToRawEventUsesUTCFields(t *testing.T) {
	te := tribeEvent{
		GlobalID:     "iondistrict.com?id=1",
		UTCStartDate: "2026-08-04 22:00:00",
		UTCEndDate:   "2026-08-05 00:00:00",
	}

	got, err := toRawEvent(te)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantStart := time.Date(2026, 8, 4, 22, 0, 0, 0, time.UTC)
	if !got.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want %v", got.Start, wantStart)
	}
	if got.Start.Location() != time.UTC {
		t.Errorf("Start location = %v, want UTC", got.Start.Location())
	}
	wantEnd := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	if !got.End.Equal(wantEnd) {
		t.Errorf("End = %v, want %v", got.End, wantEnd)
	}
}

// Start is required; end is not. An event with a start is still displayable,
// but an end that is present and unparseable signals a format change.
func TestToRawEventTimestampStrictness(t *testing.T) {
	cases := []struct {
		name    string
		start   string
		end     string
		wantErr bool
	}{
		{"both valid", "2026-08-04 22:00:00", "2026-08-05 00:00:00", false},
		{"absent end is tolerated", "2026-08-04 22:00:00", "", false},
		{"missing start is fatal", "", "2026-08-05 00:00:00", true},
		{"garbage start is fatal", "not a date", "2026-08-05 00:00:00", true},
		{"garbage end is fatal", "2026-08-04 22:00:00", "not a date", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			te := tribeEvent{
				GlobalID:     "iondistrict.com?id=1",
				UTCStartDate: tc.start,
				UTCEndDate:   tc.end,
			}
			_, err := toRawEvent(te)
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// Without a stable upstream ID every event would collide on the same
// Fingerprint, corrupting dedupe. Fail loudly rather than emit a bad key.
func TestToRawEventRequiresGlobalID(t *testing.T) {
	te := tribeEvent{UTCStartDate: "2026-08-04 22:00:00"}
	if _, err := toRawEvent(te); err == nil {
		t.Fatal("want error for missing global_id, got nil")
	}
}

// Ion's escaping is inconsistent: some names arrive entity-encoded and others
// already decoded. Unescaping is correct for both, since it is a no-op on text
// with no entities. Description is markup and must stay raw.
func TestToRawEventUnescaping(t *testing.T) {
	te := tribeEvent{
		GlobalID:     "iondistrict.com?id=1",
		UTCStartDate: "2026-08-04 22:00:00",
		Title:        "ENRG HTX &#8211; August 2026",
		Description:  "<p>Register &amp; attend</p>",
		Organizer: []tribeOrganizer{
			{Organizer: "Liu Idea Lab for Innovation &#038; Entrepreneurship", Website: "https://lilie.rice.edu"},
			{Organizer: "Houston Linux User’s Group"},
		},
		Venue:      tribeVenues{{Venue: "Ion &#8211; Conference Room 028"}},
		Categories: []tribeCategory{{Name: "Founders &amp; Startups"}},
	}

	got, err := toRawEvent(te)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if want := "ENRG HTX – August 2026"; got.Title != want {
		t.Errorf("Title = %q, want %q", got.Title, want)
	}
	// Markup keeps its entities; they are unescaped later, once, when stripped.
	if want := "<p>Register &amp; attend</p>"; got.Description != want {
		t.Errorf("Description = %q, want %q", got.Description, want)
	}
	if want := "Liu Idea Lab for Innovation & Entrepreneurship"; got.Organizers[0].Name != want {
		t.Errorf("Organizers[0].Name = %q, want %q", got.Organizers[0].Name, want)
	}
	// Already-decoded text must survive unescaping unchanged.
	if want := "Houston Linux User’s Group"; got.Organizers[1].Name != want {
		t.Errorf("Organizers[1].Name = %q, want %q", got.Organizers[1].Name, want)
	}
	if want := "Ion – Conference Room 028"; got.Venues[0].Name != want {
		t.Errorf("Venues[0].Name = %q, want %q", got.Venues[0].Name, want)
	}
	if want := "Founders & Startups"; got.Categories[0] != want {
		t.Errorf("Categories[0] = %q, want %q", got.Categories[0], want)
	}
}

// `website` is the external signup link and `url` is the Ion event page.
// Swapping them would send everyone to the wrong place.
func TestToRawEventURLMapping(t *testing.T) {
	te := tribeEvent{
		GlobalID:     "iondistrict.com?id=1",
		UTCStartDate: "2026-08-04 22:00:00",
		URL:          "https://iondistrict.com/event/seip-demo-day/",
		Website:      "https://luma.com/7cin813l",
	}

	got, err := toRawEvent(te)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "https://iondistrict.com/event/seip-demo-day/"; got.URL != want {
		t.Errorf("URL = %q, want %q", got.URL, want)
	}
	if want := "https://luma.com/7cin813l"; got.RegisterURL != want {
		t.Errorf("RegisterURL = %q, want %q", got.RegisterURL, want)
	}
}

// SourceKey is stamped by the fetch layer, and UpstreamID carries the raw
// global_id with no "tribe:" prefix. The namespace is added at normalize, so
// the ICS decoder can do the identical thing with a UID.
func TestToRawEventLeavesDerivationAlone(t *testing.T) {
	te := tribeEvent{
		GlobalID:     "iondistrict.com?id=61797",
		UTCStartDate: "2026-08-04 22:00:00",
	}

	got, err := toRawEvent(te)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.SourceKey != "" {
		t.Errorf("SourceKey = %q, want empty; the decoder must not set it", got.SourceKey)
	}
	if want := "iondistrict.com?id=61797"; got.UpstreamID != want {
		t.Errorf("UpstreamID = %q, want %q", got.UpstreamID, want)
	}
}

func TestParseTribeFixture(t *testing.T) {
	f, err := os.Open("testdata/ion-tribe.json")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	page, err := ParseTribe(f)
	if err != nil {
		t.Fatalf("ParseTribe: %v", err)
	}

	if len(page.Events) != 50 {
		t.Errorf("decoded %d events, want 50", len(page.Events))
	}
	if len(page.Skipped) != 0 {
		t.Errorf("skipped %d events, want 0: %v", len(page.Skipped), page.Skipped)
	}
	if page.Total != 80 {
		t.Errorf("Total = %d, want 80", page.Total)
	}
	if page.NextURL == "" {
		t.Error("NextURL is empty; the fixture is page 1 of 2")
	}

	// Spot-check the first event end to end.
	e := page.Events[0]
	if want := "iondistrict.com?id=61797"; e.UpstreamID != want {
		t.Errorf("UpstreamID = %q, want %q", e.UpstreamID, want)
	}
	if want := "CEOs: Build With AI, Exit With a Premium"; e.Title != want {
		t.Errorf("Title = %q, want %q", e.Title, want)
	}
	if want := "https://luma.com/7cin813l"; e.RegisterURL != want {
		t.Errorf("RegisterURL = %q, want %q", e.RegisterURL, want)
	}
	wantStart := time.Date(2026, 8, 4, 22, 0, 0, 0, time.UTC)
	if !e.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want %v", e.Start, wantStart)
	}
	if len(e.Organizers) != 1 || e.Organizers[0].Name != "Ion" {
		t.Errorf("Organizers = %+v, want one named Ion", e.Organizers)
	}
	if len(e.Venues) != 1 || e.Venues[0].Name != "Ion" {
		t.Errorf("Venues = %+v, want one named Ion", e.Venues)
	}

	// Every event must carry a start and an upstream ID, or dedupe breaks.
	for i, ev := range page.Events {
		if ev.UpstreamID == "" {
			t.Errorf("event %d has no UpstreamID", i)
		}
		if ev.Start.IsZero() {
			t.Errorf("event %d has a zero Start", i)
		}
	}
}

// One bad event must not cost the whole page. This is the behavior that lets a
// schema surprise degrade instead of blanking a venue's calendar.
func TestParseTribeSkipsBadEvents(t *testing.T) {
	const payload = `{
	  "total": 4,
	  "next_rest_url": "",
	  "events": [
	    {"global_id":"a","title":"Good","utc_start_date":"2026-08-04 22:00:00"},
	    {"global_id":"b","title":"Unparseable start","utc_start_date":"not a date"},
	    "this is not even an object",
	    {"global_id":"","title":"No global_id","utc_start_date":"2026-08-04 22:00:00"},
	    {"global_id":"c","title":"Also good","utc_start_date":"2026-08-05 22:00:00"}
	  ]
	}`

	page, err := ParseTribe(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParseTribe returned a fatal error: %v", err)
	}

	if len(page.Events) != 2 {
		t.Errorf("decoded %d events, want 2", len(page.Events))
	}
	if len(page.Skipped) != 3 {
		t.Errorf("skipped %d events, want 3: %v", len(page.Skipped), page.Skipped)
	}
	for _, e := range page.Events {
		if e.UpstreamID != "a" && e.UpstreamID != "c" {
			t.Errorf("unexpected surviving event %q", e.UpstreamID)
		}
	}
}

// A malformed envelope has nothing salvageable in it, so it is fatal.
func TestParseTribeBadEnvelope(t *testing.T) {
	for _, input := range []string{``, `not json`, `{"events":`} {
		if _, err := ParseTribe(strings.NewReader(input)); err == nil {
			t.Errorf("input %q: want error, got nil", input)
		}
	}
}

// An empty but well-formed page is valid, not an error. Three of the eight
// registry feeds return zero events today.
func TestParseTribeEmptyPage(t *testing.T) {
	page, err := ParseTribe(strings.NewReader(`{"total":0,"events":[]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page.Events) != 0 || len(page.Skipped) != 0 {
		t.Errorf("got %d events / %d skipped, want 0/0", len(page.Events), len(page.Skipped))
	}
}
