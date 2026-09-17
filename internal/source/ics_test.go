package source

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// A window wide enough that nothing is filtered unless a test means it to be.
var (
	testFrom  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	testUntil = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
)

// icsDoc wraps VEVENT bodies in the minimum viable calendar. CRLF throughout,
// because that is what all three fixtures use and what RFC 5545 requires;
// a test built on bare LF would not notice a scanner that mishandled the CR.
func icsDoc(parts ...string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + strings.Join(parts, "") + "END:VCALENDAR\r\n"
}

func vevent(lines ...string) string {
	return "BEGIN:VEVENT\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\n"
}

func parseDoc(t *testing.T, doc string) ICSFeed {
	t.Helper()
	feed, err := ParseICS(strings.NewReader(doc), testFrom, testUntil)
	if err != nil {
		t.Fatalf("ParseICS: unexpected error: %v", err)
	}
	return feed
}

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("America/Chicago: %v", err)
	}
	return loc
}

// A stream that is not iCalendar has to fail loudly rather than decode as a
// calendar with no events. The distinction is not academic: an HTML error page
// served with a 200 would otherwise look exactly like a group that cancelled
// everything, and normalize deletes on absence.
func TestParseICSRejectsNonCalendar(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"html error page", "<!DOCTYPE html>\r\n<html><body>404 Not Found</body></html>\r\n"},
		{"empty body", ""},
		{"json", `{"events":[]}`},
		{"events with no calendar wrapper", vevent("UID:a@b", "DTSTART:20260601T170000Z")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseICS(strings.NewReader(tc.input), testFrom, testUntil)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), "iCalendar") {
				t.Errorf("error = %v, want it to say the stream is not iCalendar", err)
			}
		})
	}
}

// An empty but valid calendar is a group with nothing scheduled, which is a
// normal state and not a failure. empty-meetup.ics is a real feed in this
// shape.
func TestParseICSAcceptsAnEmptyCalendar(t *testing.T) {
	feed := parseDoc(t, icsDoc())
	if len(feed.Events) != 0 || len(feed.Skipped) != 0 {
		t.Fatalf("got %d events and %d skips, want none of either", len(feed.Events), len(feed.Skipped))
	}
}

// Folding is not cosmetic. HLUG folds 165 lines, and a LOCATION split across
// two physical lines decodes as half an address unless the scanner rejoins it.
func TestParseICSUnfoldsContinuationLines(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{
			// Copied from hlug-gcal.ics lines 1119-1120. The backslash of an
			// escaped comma is the last octet before the fold and the comma
			// is the first octet after it, so unfolding has to happen strictly
			// before unescaping or the pair is never seen as a pair.
			name:  "escape split across the fold",
			lines: []string{"LOCATION:Finn MacCool's Irish Bar\\, 1127 Eldridge Pkwy Suite 600\\, Houston\\", " , TX 77077"},
			want:  "Finn MacCool's Irish Bar, 1127 Eldridge Pkwy Suite 600, Houston, TX 77077",
		},
		{
			// A tab is the other legal continuation marker, and folding adds
			// no whitespace of its own: the fold can land mid-word and the
			// halves join with nothing between them.
			name:  "tab continuation mid-word",
			lines: []string{"LOCATION:Sesh Coworking\\, 2808 Car", "\toline St #100"},
			want:  "Sesh Coworking, 2808 Caroline St #100",
		},
		{
			name:  "three physical lines",
			lines: []string{"LOCATION:one", " two", " three"},
			want:  "onetwothree",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := append([]string{"UID:fold@test", "DTSTART:20260601T170000Z"}, tc.lines...)
			feed := parseDoc(t, icsDoc(vevent(lines...)))
			if len(feed.Events) != 1 {
				t.Fatalf("got %d events, want 1 (skipped: %v)", len(feed.Events), feed.Skipped)
			}
			if len(feed.Events[0].Venues) != 1 {
				t.Fatalf("got %d venues, want 1", len(feed.Events[0].Venues))
			}
			if got := feed.Events[0].Venues[0].Name; got != tc.want {
				t.Errorf("location = %q, want %q", got, tc.want)
			}
		})
	}
}

// VTIMEZONE carries its own DTSTART and RRULE describing DST transitions. Read
// as event data they would invent two events per feed, dated 1970 and
// recurring yearly forever. Every fixture has one, so this fires on real data.
func TestParseICSIgnoresVTIMEZONE(t *testing.T) {
	vtz := "BEGIN:VTIMEZONE\r\nTZID:America/Chicago\r\n" +
		"BEGIN:DAYLIGHT\r\nTZOFFSETFROM:-0600\r\nTZOFFSETTO:-0500\r\nTZNAME:CDT\r\n" +
		"DTSTART:19700308T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU\r\nEND:DAYLIGHT\r\n" +
		"BEGIN:STANDARD\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0600\r\nTZNAME:CST\r\n" +
		"DTSTART:19701101T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU\r\nEND:STANDARD\r\n" +
		"END:VTIMEZONE\r\n"

	feed := parseDoc(t, icsDoc(vtz, vevent("UID:real@test", "SUMMARY:Real", "DTSTART:20260601T170000Z")))

	if len(feed.Events) != 1 {
		t.Fatalf("got %d events, want only the real one: %+v", len(feed.Events), feed.Events)
	}
	if feed.Events[0].UpstreamID != "real@test" {
		t.Errorf("event = %q, want the VEVENT and not a VTIMEZONE component", feed.Events[0].UpstreamID)
	}
	// The nested DAYLIGHT/STANDARD ends must not be mistaken for the end of
	// VTIMEZONE, which is what the skip-depth counter exists for. If they were,
	// the STANDARD block's properties would land on the following VEVENT.
	if len(feed.Skipped) != 0 {
		t.Errorf("skipped = %v, want none", feed.Skipped)
	}
}

// A VALARM sits inside a VEVENT and has its own properties, including a
// TRIGGER and sometimes a SUMMARY. Those belong to the alarm, not the event.
func TestParseICSIgnoresNestedComponents(t *testing.T) {
	ev := "BEGIN:VEVENT\r\nUID:alarm@test\r\nSUMMARY:Real Title\r\nDTSTART:20260601T170000Z\r\n" +
		"BEGIN:VALARM\r\nACTION:DISPLAY\r\nSUMMARY:Reminder\r\nTRIGGER:-PT30M\r\nEND:VALARM\r\n" +
		"END:VEVENT\r\n"

	feed := parseDoc(t, icsDoc(ev))
	if len(feed.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(feed.Events))
	}
	if got := feed.Events[0].Title; got != "Real Title" {
		t.Errorf("title = %q, want the VEVENT's and not the VALARM's", got)
	}
}

// The reason this decoder has an allowlist rather than a property bag. The
// HLUG feed really does carry ATTENDEE;CN=houstonlinuxusergroup@gmail.com.
func TestParseICSNeverCarriesPIIProperties(t *testing.T) {
	feed := parseDoc(t, icsDoc(vevent(
		"UID:pii@test",
		"SUMMARY:Monthly Meeting",
		"DTSTART:20260601T170000Z",
		`ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;CN=houstonlinuxusergroup@gmail.com:mailto:houstonlinuxusergroup@gmail.com`,
		`ORGANIZER;CN="Doe, John":mailto:jdoe@example.com`,
		"X-CUSTOM-PHONE:713-555-0100",
	)))

	if len(feed.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(feed.Events))
	}
	// Formatting the whole struct catches a field we forgot existed, which
	// asserting on named fields would not.
	dump := fmt.Sprintf("%+v", feed.Events[0])
	for _, leak := range []string{"houstonlinuxusergroup@gmail.com", "jdoe@example.com", "Doe, John", "713-555-0100", "mailto"} {
		if strings.Contains(dump, leak) {
			t.Errorf("RawEvent contains %q:\n%s", leak, dump)
		}
	}
	if feed.Events[0].Title != "Monthly Meeting" {
		t.Errorf("title = %q, want the event to still decode", feed.Events[0].Title)
	}
}

// D5's three forms, plus the floating one the RFC defines and no fixture uses.
func TestParseICSTimeForms(t *testing.T) {
	loc := chicago(t)
	cases := []struct {
		name       string
		dtstart    string
		want       time.Time
		wantAllDay bool
	}{
		{
			name:    "utc with Z",
			dtstart: "DTSTART:20260903T020000Z",
			want:    time.Date(2026, 9, 3, 2, 0, 0, 0, time.UTC),
		},
		{
			// 19:00 Chicago in May is CDT, UTC-5, so 00:00 the next day.
			name:    "tzid",
			dtstart: "DTSTART;TZID=America/Chicago:20260529T190000",
			want:    time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
		},
		{
			// D10: midnight Chicago, not midnight UTC. November is CST, UTC-6.
			name:       "date only",
			dtstart:    "DTSTART;VALUE=DATE:20261106",
			want:       time.Date(2026, 11, 6, 0, 0, 0, 0, loc).UTC(),
			wantAllDay: true,
		},
		{
			name:    "floating local time",
			dtstart: "DTSTART:20260529T190000",
			want:    time.Date(2026, 5, 29, 19, 0, 0, 0, loc).UTC(),
		},
		{
			// Parameter order must not matter, and VALUE=DATE-TIME is not
			// VALUE=DATE.
			name:    "tzid after another parameter",
			dtstart: "DTSTART;VALUE=DATE-TIME;TZID=America/Chicago:20260529T190000",
			want:    time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			feed := parseDoc(t, icsDoc(vevent("UID:t@test", tc.dtstart)))
			if len(feed.Events) != 1 {
				t.Fatalf("got %d events, want 1 (skipped: %v)", len(feed.Events), feed.Skipped)
			}
			got := feed.Events[0]
			if !got.Start.Equal(tc.want) {
				t.Errorf("start = %s, want %s", got.Start.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			if got.AllDay != tc.wantAllDay {
				t.Errorf("allDay = %v, want %v", got.AllDay, tc.wantAllDay)
			}
			if got.Start.Location() != time.UTC {
				t.Errorf("start location = %v, want UTC in the domain type", got.Start.Location())
			}
		})
	}
}

// A TZID naming a zone the tz database does not have is a format change worth
// failing the event over, not something to silently treat as UTC.
func TestParseICSRejectsUnknownTZID(t *testing.T) {
	feed := parseDoc(t, icsDoc(vevent("UID:tz@test", "DTSTART;TZID=Mars/Olympus:20260529T190000")))
	if len(feed.Events) != 0 {
		t.Fatalf("got %d events, want 0", len(feed.Events))
	}
	if len(feed.Skipped) != 1 || !strings.Contains(feed.Skipped[0].Error(), "Mars/Olympus") {
		t.Fatalf("skipped = %v, want one naming the zone", feed.Skipped)
	}
}

func TestParseICSEventValidity(t *testing.T) {
	cases := []struct {
		name       string
		lines      []string
		wantEvents int
		wantSkips  int
		wantErrTxt string
	}{
		{
			name:       "no UID",
			lines:      []string{"SUMMARY:Anonymous", "DTSTART:20260601T170000Z"},
			wantSkips:  1,
			wantErrTxt: "no UID",
		},
		{
			name:       "no DTSTART",
			lines:      []string{"UID:nostart@test", "SUMMARY:Undated"},
			wantSkips:  1,
			wantErrTxt: "no DTSTART",
		},
		{
			name:       "unparseable DTSTART",
			lines:      []string{"UID:bad@test", "DTSTART:not-a-date"},
			wantSkips:  1,
			wantErrTxt: "DTSTART",
		},
		{
			// Tolerated, matching toRawEvent: an event with a start is still
			// displayable, and dropping it loses real information over a
			// cosmetic gap.
			name:       "no DTEND",
			lines:      []string{"UID:noend@test", "DTSTART:20260601T170000Z"},
			wantEvents: 1,
		},
		{
			// Present but unreadable is a different thing and signals a
			// format change.
			name:       "unparseable DTEND",
			lines:      []string{"UID:badend@test", "DTSTART:20260601T170000Z", "DTEND:garbage"},
			wantSkips:  1,
			wantErrTxt: "DTEND",
		},
		{
			// Dropped, but NOT skipped. Skipped means "we could not read
			// this, so do not trust its absence" and defers cancellation for
			// the whole source; a cancelled event is absent on purpose.
			name:  "cancelled",
			lines: []string{"UID:off@test", "DTSTART:20260601T170000Z", "STATUS:CANCELLED"},
		},
		{
			name:       "tentative still counts",
			lines:      []string{"UID:maybe@test", "DTSTART:20260601T170000Z", "STATUS:TENTATIVE"},
			wantEvents: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			feed := parseDoc(t, icsDoc(vevent(tc.lines...)))
			if len(feed.Events) != tc.wantEvents {
				t.Errorf("got %d events, want %d", len(feed.Events), tc.wantEvents)
			}
			if len(feed.Skipped) != tc.wantSkips {
				t.Fatalf("got %d skips, want %d: %v", len(feed.Skipped), tc.wantSkips, feed.Skipped)
			}
			if tc.wantErrTxt != "" && !strings.Contains(feed.Skipped[0].Error(), tc.wantErrTxt) {
				t.Errorf("skip = %v, want it to mention %q", feed.Skipped[0], tc.wantErrTxt)
			}
		})
	}
}

// One unusable event must not take the rest of the feed with it. Same rule as
// ParseTribe, one level down from "one dead source must not blank the site".
func TestParseICSKeepsGoingAfterABadEvent(t *testing.T) {
	feed := parseDoc(t, icsDoc(
		vevent("UID:good1@test", "SUMMARY:First", "DTSTART:20260601T170000Z"),
		vevent("SUMMARY:No UID", "DTSTART:20260602T170000Z"),
		vevent("UID:good2@test", "SUMMARY:Third", "DTSTART:20260603T170000Z"),
	))
	if len(feed.Events) != 2 || len(feed.Skipped) != 1 {
		t.Fatalf("got %d events and %d skips, want 2 and 1", len(feed.Events), len(feed.Skipped))
	}
	if feed.Events[0].Title != "First" || feed.Events[1].Title != "Third" {
		t.Errorf("events = %q, %q; want First and Third", feed.Events[0].Title, feed.Events[1].Title)
	}
}

// Events outside the window are dropped and NOT skipped. Skipping them would
// defer cancellation for the source on every single run, because HLUG carries
// 95 events that have already happened.
func TestParseICSFiltersToTheWindowWithoutSkipping(t *testing.T) {
	doc := icsDoc(
		vevent("UID:past@test", "DTSTART:20250601T170000Z"),
		vevent("UID:inside@test", "DTSTART:20260601T170000Z"),
		vevent("UID:future@test", "DTSTART:20280601T170000Z"),
	)
	feed, err := ParseICS(strings.NewReader(doc), testFrom, testUntil)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	if len(feed.Events) != 1 || feed.Events[0].UpstreamID != "inside@test" {
		t.Fatalf("events = %+v, want only inside@test", feed.Events)
	}
	if len(feed.Skipped) != 0 {
		t.Errorf("skipped = %v, want none: an out-of-window event is not a failure", feed.Skipped)
	}
}

// LOCATION is one unstructured string and stays one. Splitting it into address
// parts is Phase 5's job and needs the curated venue list to do properly.
func TestParseICSLocationBecomesOneUnsplitVenue(t *testing.T) {
	feed := parseDoc(t, icsDoc(vevent(
		"UID:loc@test",
		"DTSTART:20260601T170000Z",
		`LOCATION:The Ion\, Rooms 29 and 30\, 4201 Main St\, Houston\, TX 77002`,
	)))
	v := feed.Events[0].Venues
	if len(v) != 1 {
		t.Fatalf("got %d venues, want 1", len(v))
	}
	if v[0].Name != "The Ion, Rooms 29 and 30, 4201 Main St, Houston, TX 77002" {
		t.Errorf("venue name = %q, want the whole LOCATION unsplit", v[0].Name)
	}
	if v[0].Address != "" || v[0].City != "" || v[0].Zip != "" {
		t.Errorf("venue = %+v, want only Name populated", v[0])
	}
}

// 7 of HLUG's 110 events have no LOCATION at all.
func TestParseICSToleratesAbsentLocation(t *testing.T) {
	feed := parseDoc(t, icsDoc(vevent("UID:noloc@test", "DTSTART:20260601T170000Z")))
	if len(feed.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(feed.Events))
	}
	if len(feed.Events[0].Venues) != 0 {
		t.Errorf("venues = %+v, want none rather than one empty one", feed.Events[0].Venues)
	}
}

func TestUnescapeText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"comma", `Houston\, TX`, "Houston, TX"},
		{"semicolon", `a\;b`, "a;b"},
		{"newline", `line one\nline two`, "line one\nline two"},
		{"capital N is also a newline", `line one\Nline two`, "line one\nline two"},
		{"escaped backslash", `city\\.`, `city\.`},
		{
			// The reason this is one pass and not three ReplaceAll calls.
			// Unescaping \\ first would leave \n, and the next pass would turn
			// a literal backslash-n into a newline.
			name: "escaped backslash followed by n",
			in:   `city\\n more`,
			want: `city\n more`,
		},
		{"nothing to do", "plain text", "plain text"},
		{"trailing lone backslash", `ends with\`, `ends with\`},
		{"undefined escape is left alone", `a \q b`, `a \q b`},
		{"html entity survives", `&quot\;red&quot\;`, `&quot;red&quot;`},
		{"utf8 is untouched", `Finn MacCool’s \, Houston`, `Finn MacCool’s , Houston`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unescapeText(tc.in); got != tc.want {
				t.Errorf("unescapeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A quoted parameter value may contain the delimiters, which is the whole
// reason quoting exists in RFC 5545.
func TestParsePropertyHandlesQuotedParameters(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantName  string
		wantValue string
		wantTZID  string
	}{
		{"plain", "SUMMARY:Hello", "SUMMARY", "Hello", ""},
		{"value contains colons", "URL:https://example.com/a:b", "URL", "https://example.com/a:b", ""},
		{"tzid parameter", "DTSTART;TZID=America/Chicago:20260529T190000", "DTSTART", "20260529T190000", "America/Chicago"},
		{"quoted tzid", `DTSTART;TZID="America/Chicago":20260529T190000`, "DTSTART", "20260529T190000", "America/Chicago"},
		{"quoted param containing a colon", `ATTENDEE;CN="Doe: John":mailto:x@y`, "ATTENDEE", "mailto:x@y", ""},
		{"quoted param containing a semicolon", `ATTENDEE;CN="a;b";TZID=UTC:v`, "ATTENDEE", "v", "UTC"},
		{"lowercase name is normalised", "summary:Hello", "SUMMARY", "Hello", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := parseProperty(tc.line)
			if !ok {
				t.Fatalf("parseProperty(%q) returned not-ok", tc.line)
			}
			if p.Name != tc.wantName || p.Value != tc.wantValue || p.TZID != tc.wantTZID {
				t.Errorf("got name=%q value=%q tzid=%q, want %q / %q / %q",
					p.Name, p.Value, p.TZID, tc.wantName, tc.wantValue, tc.wantTZID)
			}
		})
	}

	for _, bad := range []string{"", "no colon here", ":value with no name"} {
		if _, ok := parseProperty(bad); ok {
			t.Errorf("parseProperty(%q) = ok, want not-ok", bad)
		}
	}
}

func parseFixture(t *testing.T, name string, from, until time.Time) ICSFeed {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	feed, err := ParseICS(f, from, until)
	if err != nil {
		t.Fatalf("ParseICS(%s): %v", name, err)
	}
	return feed
}

// The whole decoder against a real 110-event Google Calendar export, over the
// same 62-day window the fetch layer asks Ion for.
//
// The counts are pinned because they were measured independently before the
// decoder existed: 10 concrete VEVENTs land in this window and the one live
// RRULE contributes 9 more. A change to either number means the decoder
// changed its mind about something, which is exactly what this should catch.
func TestParseICSAgainstHLUGFixture(t *testing.T) {
	loc := chicago(t)
	from := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	feed := parseFixture(t, "hlug-gcal.ics", from, until)

	if len(feed.Skipped) != 0 {
		t.Fatalf("skipped = %v, want none: every event in this feed is well formed", feed.Skipped)
	}
	if len(feed.Events) != 19 {
		t.Fatalf("got %d events, want 19 (10 concrete + 9 expanded)", len(feed.Events))
	}
	if feed.Name != "houstonlinuxusergroup@gmail.com" {
		t.Errorf("name = %q, want X-WR-CALNAME", feed.Name)
	}

	var socials []time.Time
	seenID := map[string]bool{}
	for _, e := range feed.Events {
		if e.Start.Before(from) || e.Start.After(until) {
			t.Errorf("event %q at %s is outside the requested window", e.Title, e.Start)
		}
		// The VTIMEZONE components recur yearly from 1970. If any of them were
		// read as an event, it would show up here.
		if e.Start.Year() < 2026 {
			t.Errorf("event %q dated %s: a VTIMEZONE component leaked through", e.Title, e.Start)
		}
		if seenID[e.UpstreamID] {
			t.Errorf("duplicate UpstreamID %q: recurrence instances must be distinguishable", e.UpstreamID)
		}
		seenID[e.UpstreamID] = true

		if strings.Contains(e.Title, "Social at Finn MacCool") {
			socials = append(socials, e.Start)
		}
	}

	if len(socials) != 9 {
		t.Fatalf("got %d expanded socials, want 9", len(socials))
	}

	// The load-bearing assertion, on real data. Every instance is 20:00 in
	// Houston, on both sides of the 1 November DST change. Expanding by adding
	// 168 hours to an instant instead of stepping the local wall clock puts
	// the November ones at 19:00.
	var sawCDT, sawCST bool
	for _, s := range socials {
		local := s.In(loc)
		if local.Hour() != 20 || local.Minute() != 0 {
			t.Errorf("social at %s is %02d:%02d local, want 20:00", s, local.Hour(), local.Minute())
		}
		switch local.Format("MST") {
		case "CDT":
			sawCDT = true
		case "CST":
			sawCST = true
		}
	}
	if !sawCDT || !sawCST {
		t.Errorf("sawCDT=%v sawCST=%v; the window must straddle the DST change for this test to mean anything", sawCDT, sawCST)
	}

	// D10 on real data: HLUG's one VALUE=DATE event. It is also the calendar
	// owner's private appointment, which is what human review exists for and
	// what a decoder cannot fix.
	var allDay int
	for _, e := range feed.Events {
		if !e.AllDay {
			continue
		}
		allDay++
		if got := e.Start.In(loc); got.Hour() != 0 {
			t.Errorf("all-day event starts at %s local, want midnight Chicago", got.Format("15:04 MST"))
		}
	}
	if allDay != 1 {
		t.Errorf("got %d all-day events, want 1", allDay)
	}
}

func TestParseICSAgainstMeetupFixture(t *testing.T) {
	loc := chicago(t)
	feed := parseFixture(t, "code-and-coffee-meetup.ics",
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))

	if len(feed.Skipped) != 0 || len(feed.Events) != 1 {
		t.Fatalf("got %d events and %d skips, want 1 and 0: %v", len(feed.Events), len(feed.Skipped), feed.Skipped)
	}
	e := feed.Events[0]

	if e.UpstreamID != "event_315949333@meetup.com" {
		t.Errorf("uid = %q", e.UpstreamID)
	}
	// The ampersand is literal here. ICS escaping covers comma, semicolon,
	// backslash and newline, and nothing else, so unlike the tribe decoder
	// there are no HTML entities to unescape at this layer.
	if e.Title != "Houston Code & Coffee" {
		t.Errorf("title = %q, want the ampersand left alone", e.Title)
	}
	if want := time.Date(2026, 8, 30, 10, 0, 0, 0, loc); !e.Start.Equal(want) {
		t.Errorf("start = %s, want %s", e.Start, want)
	}
	// URL;VALUE=URI must not be mistaken for VALUE=DATE.
	if e.AllDay {
		t.Error("allDay = true; VALUE=URI on the URL property is not VALUE=DATE")
	}
	if e.URL != "https://www.meetup.com/houston-code-and-coffee/events/315949333/" {
		t.Errorf("url = %q", e.URL)
	}
	// Meetup folds its description across 17 physical lines and escapes both
	// newlines and a literal backslash inside it.
	if n := strings.Count(e.Description, "\n"); n != 29 {
		t.Errorf("description has %d newlines, want 29 from unescaped \\n", n)
	}
	if !strings.Contains(e.Description, `your city\.`) {
		t.Errorf("description lost the escaped backslash:\n%s", e.Description)
	}
	if strings.Contains(e.Description, `\n`) {
		t.Error("description still contains a literal backslash-n")
	}
}

// A group with nothing scheduled. Meetup serves a valid calendar with no
// VEVENTs rather than a 404, and that must read as "no events" and not as an
// error, or every quiet month would look like a broken feed.
func TestParseICSAgainstEmptyMeetupFixture(t *testing.T) {
	feed := parseFixture(t, "empty-meetup.ics", testFrom, testUntil)
	if len(feed.Events) != 0 || len(feed.Skipped) != 0 {
		t.Fatalf("got %d events and %d skips, want none", len(feed.Events), len(feed.Skipped))
	}
	if feed.Name != "Houston Robotics Group" {
		t.Errorf("name = %q, want the calendar name to decode anyway", feed.Name)
	}
}
