package source

import (
	"strings"
	"testing"
	"time"
)

// expand runs one recurring VEVENT through the real decoder and returns its
// occurrences rendered in Houston time. Going through ParseICS rather than
// calling the expander directly keeps these tests honest about the seam
// between the two: a rule that parses but never reaches expansion would pass
// a unit test of the expander alone.
func expand(t *testing.T, from, until time.Time, lines ...string) ([]string, ICSFeed) {
	t.Helper()
	feed, err := ParseICS(strings.NewReader(icsDoc(vevent(lines...))), from, until)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	loc := chicago(t)
	out := make([]string, 0, len(feed.Events))
	for _, e := range feed.Events {
		out = append(out, e.Start.In(loc).Format("Mon 2006-01-02 15:04 MST"))
	}
	return out, feed
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func day(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, time.UTC)
}

// A rule htxdev cannot expand must fail its event and say why. It must never
// approximate: a wrong date looks exactly like a right one to everything
// downstream, and to the person who shows up.
func TestParseRRULERefusesRatherThanGuesses(t *testing.T) {
	cases := []struct {
		name    string
		rrule   string
		wantErr string
	}{
		{"yearly", "FREQ=YEARLY", "FREQ=YEARLY"},
		{"hourly", "FREQ=HOURLY;INTERVAL=6", "FREQ=HOURLY"},
		{"no freq", "INTERVAL=2", "no FREQ"},
		{"bysetpos", "FREQ=MONTHLY;BYDAY=TH;BYSETPOS=-1", "BYSETPOS"},
		{"byweekno", "FREQ=WEEKLY;BYWEEKNO=3", "BYWEEKNO"},
		{"byyearday", "FREQ=DAILY;BYYEARDAY=100", "BYYEARDAY"},
		{"bymonth", "FREQ=MONTHLY;BYMONTH=3", "BYMONTH="},
		{"interval zero", "FREQ=WEEKLY;INTERVAL=0", "INTERVAL"},
		{"negative interval", "FREQ=WEEKLY;INTERVAL=-1", "INTERVAL"},
		{"count and until together", "FREQ=WEEKLY;COUNT=3;UNTIL=20260601T000000Z", "mutually exclusive"},
		{"byday with interval over one", "FREQ=WEEKLY;BYDAY=MO,WE;INTERVAL=2", "WKST"},
		{"ordinal byday with weekly", "FREQ=WEEKLY;BYDAY=3MO", "ordinal"},
		{"byday and bymonthday together", "FREQ=MONTHLY;BYDAY=3TH;BYMONTHDAY=15", "BYSETPOS"},
		{"unknown weekday", "FREQ=WEEKLY;BYDAY=XX", "weekday"},
		{"bad ordinal", "FREQ=MONTHLY;BYDAY=9TH", "ordinal"},
		{"bymonthday out of range", "FREQ=MONTHLY;BYMONTHDAY=40", "BYMONTHDAY"},
		{"not key equals value", "FREQ=WEEKLY;JUSTASTRING", "KEY=VALUE"},
		{"bad until", "FREQ=WEEKLY;UNTIL=nonsense", "UNTIL"},
		{"bymonthday with weekly", "FREQ=WEEKLY;BYMONTHDAY=15", "BYMONTHDAY"},
		{"byday with daily", "FREQ=DAILY;BYDAY=MO", "BYDAY"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, feed := expand(t, day(2026, 1, 1, 0), day(2027, 1, 1, 0),
				"UID:rule@test", "DTSTART:20260105T180000Z", "RRULE:"+tc.rrule)

			if len(feed.Events) != 0 {
				t.Errorf("got %d events, want none: an unexpandable rule must not emit a guess", len(feed.Events))
			}
			if len(feed.Skipped) != 1 {
				t.Fatalf("got %d skips, want 1", len(feed.Skipped))
			}
			got := feed.Skipped[0].Error()
			if !strings.Contains(got, tc.wantErr) {
				t.Errorf("skip = %q, want it to mention %q", got, tc.wantErr)
			}
			// The rule itself has to be in the message. Someone reading a sync
			// log needs to know which rule, not just that one failed.
			if !strings.Contains(got, tc.rrule) {
				t.Errorf("skip = %q, want it to quote the rule", got)
			}
		})
	}
}

// The reason this expander is hand-written. Houston observes DST, and the live
// HLUG rule crosses the November boundary inside a 60-day window.
func TestRecurrenceStepsTheWallClockNotTheInstant(t *testing.T) {
	got, feed := expand(t, day(2026, 10, 20, 0), day(2026, 11, 21, 0),
		"UID:dst@test",
		"DTSTART;TZID=America/Chicago:20261022T200000",
		"DTEND;TZID=America/Chicago:20261022T230000",
		"RRULE:FREQ=WEEKLY")

	want := []string{
		"Thu 2026-10-22 20:00 CDT",
		"Thu 2026-10-29 20:00 CDT",
		"Thu 2026-11-05 20:00 CST", // clocks went back on 1 November
		"Thu 2026-11-12 20:00 CST",
		"Thu 2026-11-19 20:00 CST",
	}
	if !equalStrings(got, want) {
		t.Fatalf("occurrences =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// The assertion that actually bites. 29 October to 5 November spans the
	// transition, and those two instants are 169 hours apart rather than 168,
	// because that local day had 25 hours in it. An expander that added 7*24h
	// to a UTC instant would produce 168 here and leave the event at 19:00
	// local for the rest of the year.
	across := feed.Events[2].Start.Sub(feed.Events[1].Start)
	if across != 169*time.Hour {
		t.Errorf("gap across the DST boundary = %v, want 169h", across)
	}
	if before := feed.Events[1].Start.Sub(feed.Events[0].Start); before != 168*time.Hour {
		t.Errorf("gap away from the boundary = %v, want 168h", before)
	}

	// DTEND has to move with DTSTART, and the meeting is still three hours
	// long on the far side.
	for i, e := range feed.Events {
		if d := e.End.Sub(e.Start); d != 3*time.Hour {
			t.Errorf("occurrence %d lasts %v, want 3h", i, d)
		}
	}
}

func TestRecurrenceFrequencies(t *testing.T) {
	cases := []struct {
		name    string
		dtstart string
		rrule   string
		from    time.Time
		until   time.Time
		want    []string
	}{
		{
			name:    "daily",
			dtstart: "DTSTART;TZID=America/Chicago:20260302T090000",
			rrule:   "FREQ=DAILY;COUNT=3",
			from:    day(2026, 3, 1, 0), until: day(2026, 4, 1, 0),
			want: []string{"Mon 2026-03-02 09:00 CST", "Tue 2026-03-03 09:00 CST", "Wed 2026-03-04 09:00 CST"},
		},
		{
			name:    "daily with interval",
			dtstart: "DTSTART;TZID=America/Chicago:20260302T090000",
			rrule:   "FREQ=DAILY;INTERVAL=10;COUNT=3",
			from:    day(2026, 3, 1, 0), until: day(2026, 4, 1, 0),
			// 8 March is the spring transition, so the third one is CDT.
			want: []string{"Mon 2026-03-02 09:00 CST", "Thu 2026-03-12 09:00 CDT", "Sun 2026-03-22 09:00 CDT"},
		},
		{
			name:    "weekly with interval",
			dtstart: "DTSTART;TZID=America/Chicago:20260601T180000",
			rrule:   "FREQ=WEEKLY;INTERVAL=2;COUNT=3",
			from:    day(2026, 6, 1, 0), until: day(2026, 8, 1, 0),
			want: []string{"Mon 2026-06-01 18:00 CDT", "Mon 2026-06-15 18:00 CDT", "Mon 2026-06-29 18:00 CDT"},
		},
		{
			name:    "weekly on several days",
			dtstart: "DTSTART;TZID=America/Chicago:20260601T180000",
			rrule:   "FREQ=WEEKLY;BYDAY=MO,WE;COUNT=4",
			from:    day(2026, 6, 1, 0), until: day(2026, 8, 1, 0),
			want: []string{
				"Mon 2026-06-01 18:00 CDT", "Wed 2026-06-03 18:00 CDT",
				"Mon 2026-06-08 18:00 CDT", "Wed 2026-06-10 18:00 CDT",
			},
		},
		{
			// The shape most Houston meetups actually use.
			name:    "monthly on the third Thursday",
			dtstart: "DTSTART;TZID=America/Chicago:20260115T180000",
			rrule:   "FREQ=MONTHLY;BYDAY=3TH",
			from:    day(2026, 1, 1, 0), until: day(2026, 5, 1, 0),
			want: []string{
				"Thu 2026-01-15 18:00 CST", "Thu 2026-02-19 18:00 CST",
				"Thu 2026-03-19 18:00 CDT", "Thu 2026-04-16 18:00 CDT",
			},
		},
		{
			name:    "monthly on the last Friday",
			dtstart: "DTSTART;TZID=America/Chicago:20260130T180000",
			rrule:   "FREQ=MONTHLY;BYDAY=-1FR",
			from:    day(2026, 1, 1, 0), until: day(2026, 5, 1, 0),
			want: []string{
				"Fri 2026-01-30 18:00 CST", "Fri 2026-02-27 18:00 CST",
				"Fri 2026-03-27 18:00 CDT", "Fri 2026-04-24 18:00 CDT",
			},
		},
		{
			name:    "monthly on every Tuesday",
			dtstart: "DTSTART;TZID=America/Chicago:20260602T180000",
			rrule:   "FREQ=MONTHLY;BYDAY=TU;COUNT=6",
			from:    day(2026, 6, 1, 0), until: day(2026, 8, 1, 0),
			want: []string{
				"Tue 2026-06-02 18:00 CDT", "Tue 2026-06-09 18:00 CDT", "Tue 2026-06-16 18:00 CDT",
				"Tue 2026-06-23 18:00 CDT", "Tue 2026-06-30 18:00 CDT", "Tue 2026-07-07 18:00 CDT",
			},
		},
		{
			// A month with no 31st has no occurrence. Rolling forward to
			// 1 March instead is the classic AddDate(0,1,0) bug.
			name:    "monthly on the 31st skips short months",
			dtstart: "DTSTART;TZID=America/Chicago:20260131T120000",
			rrule:   "FREQ=MONTHLY;BYMONTHDAY=31",
			from:    day(2026, 1, 1, 0), until: day(2026, 8, 1, 0),
			want: []string{
				"Sat 2026-01-31 12:00 CST", "Tue 2026-03-31 12:00 CDT",
				"Sun 2026-05-31 12:00 CDT", "Fri 2026-07-31 12:00 CDT",
			},
		},
		{
			// With neither BYDAY nor BYMONTHDAY the day comes from DTSTART,
			// and the same short-month rule applies.
			name:    "monthly defaults to the DTSTART day",
			dtstart: "DTSTART;TZID=America/Chicago:20260131T120000",
			rrule:   "FREQ=MONTHLY",
			from:    day(2026, 1, 1, 0), until: day(2026, 8, 1, 0),
			want: []string{
				"Sat 2026-01-31 12:00 CST", "Tue 2026-03-31 12:00 CDT",
				"Sun 2026-05-31 12:00 CDT", "Fri 2026-07-31 12:00 CDT",
			},
		},
		{
			name:    "monthly on the last day of the month",
			dtstart: "DTSTART;TZID=America/Chicago:20260131T120000",
			rrule:   "FREQ=MONTHLY;BYMONTHDAY=-1;COUNT=4",
			from:    day(2026, 1, 1, 0), until: day(2026, 8, 1, 0),
			want: []string{
				"Sat 2026-01-31 12:00 CST", "Sat 2026-02-28 12:00 CST",
				"Tue 2026-03-31 12:00 CDT", "Thu 2026-04-30 12:00 CDT",
			},
		},
		{
			name:    "monthly with an interval",
			dtstart: "DTSTART;TZID=America/Chicago:20260115T180000",
			rrule:   "FREQ=MONTHLY;INTERVAL=3;BYDAY=3TH",
			from:    day(2026, 1, 1, 0), until: day(2026, 11, 1, 0),
			want: []string{
				"Thu 2026-01-15 18:00 CST", "Thu 2026-04-16 18:00 CDT",
				"Thu 2026-07-16 18:00 CDT", "Thu 2026-10-15 18:00 CDT",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, feed := expand(t, tc.from, tc.until, "UID:freq@test", tc.dtstart, "RRULE:"+tc.rrule)
			if len(feed.Skipped) != 0 {
				t.Fatalf("skipped = %v, want none", feed.Skipped)
			}
			if !equalStrings(got, tc.want) {
				t.Errorf("occurrences =\n  %s\nwant\n  %s",
					strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "))
			}
		})
	}
}

// COUNT counts occurrences from DTSTART, not from the window. A rule with
// COUNT=5 whose first three instances are behind the window has two left, and
// a walker that started at the window edge would wrongly report five.
func TestRecurrenceCountIsMeasuredFromDTSTART(t *testing.T) {
	got, _ := expand(t, day(2026, 1, 20, 0), day(2026, 12, 1, 0),
		"UID:count@test",
		"DTSTART;TZID=America/Chicago:20260101T180000",
		"RRULE:FREQ=WEEKLY;COUNT=5")

	want := []string{"Thu 2026-01-22 18:00 CST", "Thu 2026-01-29 18:00 CST"}
	if !equalStrings(got, want) {
		t.Errorf("occurrences = %v, want %v (Jan 1, 8 and 15 are behind the window but still consume COUNT)", got, want)
	}
}

func TestRecurrenceUntil(t *testing.T) {
	cases := []struct {
		name  string
		rrule string
		want  []string
	}{
		{
			// With a Z, UNTIL is UTC. 22 January 00:00Z is 21 January 18:00
			// in Houston, so the occurrence at 18:00 CST on the 22nd is after
			// it and is excluded.
			name:  "utc until",
			rrule: "FREQ=WEEKLY;UNTIL=20260122T000000Z",
			want:  []string{"Thu 2026-01-01 18:00 CST", "Thu 2026-01-08 18:00 CST", "Thu 2026-01-15 18:00 CST"},
		},
		{
			// Without a Z, RFC 5545 says UNTIL is local time in DTSTART's
			// zone. Reading it as UTC instead is the open bug in the library
			// this package deliberately does not depend on, and it moves the
			// cut-off by six hours, which here is a whole extra occurrence.
			name:  "local until",
			rrule: "FREQ=WEEKLY;UNTIL=20260122T190000",
			want: []string{
				"Thu 2026-01-01 18:00 CST", "Thu 2026-01-08 18:00 CST",
				"Thu 2026-01-15 18:00 CST", "Thu 2026-01-22 18:00 CST",
			},
		},
		{
			// A date-only UNTIL includes the whole of that day.
			name:  "date only until",
			rrule: "FREQ=WEEKLY;UNTIL=20260115",
			want:  []string{"Thu 2026-01-01 18:00 CST", "Thu 2026-01-08 18:00 CST", "Thu 2026-01-15 18:00 CST"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, feed := expand(t, day(2026, 1, 1, 0), day(2026, 6, 1, 0),
				"UID:until@test", "DTSTART;TZID=America/Chicago:20260101T180000", "RRULE:"+tc.rrule)
			if len(feed.Skipped) != 0 {
				t.Fatalf("skipped = %v", feed.Skipped)
			}
			if !equalStrings(got, tc.want) {
				t.Errorf("occurrences = %v, want %v", got, tc.want)
			}
		})
	}
}

// EXDATE removes an instance the rule generated. HLUG's live rule has one.
func TestRecurrenceExDate(t *testing.T) {
	got, feed := expand(t, day(2026, 7, 1, 0), day(2026, 9, 1, 0),
		"UID:exdate@test",
		"DTSTART;TZID=America/Chicago:20260701T200000",
		"RRULE:FREQ=WEEKLY;COUNT=4",
		"EXDATE;TZID=America/Chicago:20260708T200000")

	want := []string{"Wed 2026-07-01 20:00 CDT", "Wed 2026-07-15 20:00 CDT", "Wed 2026-07-22 20:00 CDT"}
	if !equalStrings(got, want) {
		t.Fatalf("occurrences = %v, want %v", got, want)
	}
	// COUNT=4 generated four and EXDATE removed one, leaving three. An
	// implementation that excluded before counting would run to 29 July.
	if len(feed.Events) != 3 {
		t.Errorf("got %d events, want 3: an excluded date still consumes a COUNT slot", len(feed.Events))
	}
}

// An EXDATE written in a different zone still has to match, because what it
// identifies is an instant and not a wall clock.
func TestRecurrenceExDateMatchesAcrossZones(t *testing.T) {
	got, _ := expand(t, day(2026, 7, 1, 0), day(2026, 9, 1, 0),
		"UID:exzone@test",
		"DTSTART;TZID=America/Chicago:20260701T200000",
		"RRULE:FREQ=WEEKLY;COUNT=3",
		"EXDATE:20260709T010000Z") // 8 July 20:00 CDT, written as UTC

	want := []string{"Wed 2026-07-01 20:00 CDT", "Wed 2026-07-15 20:00 CDT"}
	if !equalStrings(got, want) {
		t.Errorf("occurrences = %v, want %v", got, want)
	}
}

// Every instance needs its own identity. D7 makes the fingerprint the upstream
// stable ID, and for a recurrence instance RFC 5545 defines that as UID plus
// RECURRENCE-ID. Sharing one UID across nine events collapses them in dedupe
// and emits nine VEVENTs with the same UID in the v1.1 feed.
func TestRecurrenceInstancesGetDistinctIDs(t *testing.T) {
	_, feed := expand(t, day(2026, 6, 1, 0), day(2026, 8, 1, 0),
		"UID:ids@test", "DTSTART;TZID=America/Chicago:20260601T180000", "RRULE:FREQ=WEEKLY;COUNT=4")

	if len(feed.Events) != 4 {
		t.Fatalf("got %d events, want 4", len(feed.Events))
	}
	seen := map[string]bool{}
	for _, e := range feed.Events {
		if seen[e.UpstreamID] {
			t.Errorf("duplicate UpstreamID %q", e.UpstreamID)
		}
		seen[e.UpstreamID] = true
		if !strings.HasPrefix(e.UpstreamID, "ids@test_") {
			t.Errorf("UpstreamID = %q, want the base UID plus an instance suffix", e.UpstreamID)
		}
		// The suffix is the instance start in UTC, which is what makes it
		// stable: re-running the sync tomorrow produces the same string.
		if !strings.HasSuffix(e.UpstreamID, e.Start.UTC().Format(icsUTCLayout)) {
			t.Errorf("UpstreamID = %q, want it to end with the instance start %s",
				e.UpstreamID, e.Start.UTC().Format(icsUTCLayout))
		}
	}
}

// A non-recurring event keeps its bare UID. Only generated instances are
// suffixed, so nothing changes for the 106 of HLUG's 110 events that carry no
// rule at all.
func TestNonRecurringEventKeepsItsBareUID(t *testing.T) {
	feed := parseDoc(t, icsDoc(vevent("UID:plain@test", "DTSTART:20260601T170000Z")))
	if len(feed.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(feed.Events))
	}
	if got := feed.Events[0].UpstreamID; got != "plain@test" {
		t.Errorf("UpstreamID = %q, want the bare UID", got)
	}
}

// A rule with no UNTIL and no COUNT is infinite, so the window is the only
// thing that stops it.
func TestRecurrenceIsBoundedByTheWindow(t *testing.T) {
	got, _ := expand(t, day(2026, 6, 1, 0), day(2026, 6, 22, 0),
		"UID:infinite@test", "DTSTART;TZID=America/Chicago:20260601T180000", "RRULE:FREQ=WEEKLY")

	want := []string{"Mon 2026-06-01 18:00 CDT", "Mon 2026-06-08 18:00 CDT", "Mon 2026-06-15 18:00 CDT"}
	if !equalStrings(got, want) {
		t.Errorf("occurrences = %v, want %v", got, want)
	}
}

// A period can generate candidates that fall before DTSTART, and those are not
// occurrences. WEEKLY with BYDAY produces one whenever DTSTART is not the
// earliest listed weekday in its own week, and MONTHLY produces one whenever
// the selected day falls earlier in DTSTART's month than DTSTART does.
func TestRecurrenceDropsCandidatesBeforeDTSTART(t *testing.T) {
	cases := []struct {
		name    string
		dtstart string
		rrule   string
		from    time.Time
		until   time.Time
		want    []string
	}{
		{
			// The week containing Wednesday 3 June also contains Monday
			// 1 June, which is before the series begins.
			name:    "weekly byday earlier in the first week",
			dtstart: "DTSTART;TZID=America/Chicago:20260603T180000",
			rrule:   "FREQ=WEEKLY;BYDAY=MO,WE;COUNT=3",
			from:    day(2026, 5, 1, 0), until: day(2026, 7, 1, 0),
			want: []string{
				"Wed 2026-06-03 18:00 CDT",
				"Mon 2026-06-08 18:00 CDT",
				"Wed 2026-06-10 18:00 CDT",
			},
		},
		{
			// The third Thursday of January 2026 is the 15th, five days
			// before this series starts, so January has no occurrence.
			name:    "monthly byday earlier in the first month",
			dtstart: "DTSTART;TZID=America/Chicago:20260120T180000",
			rrule:   "FREQ=MONTHLY;BYDAY=3TH;COUNT=2",
			from:    day(2026, 1, 1, 0), until: day(2026, 6, 1, 0),
			want: []string{
				"Thu 2026-02-19 18:00 CST",
				"Thu 2026-03-19 18:00 CDT",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, feed := expand(t, tc.from, tc.until, "UID:before@test", tc.dtstart, "RRULE:"+tc.rrule)
			if len(feed.Skipped) != 0 {
				t.Fatalf("skipped = %v", feed.Skipped)
			}
			if !equalStrings(got, tc.want) {
				t.Errorf("occurrences =\n  %s\nwant\n  %s",
					strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "))
			}
		})
	}
}
