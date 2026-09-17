package source

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// Expansion limits.
//
// maxOccurrences is a runaway guard, not a policy: it counts occurrences the
// rule generates from DTSTART, including ones behind the requested window, so
// the realistic worst case in these feeds (a weekly rule running since 2015,
// about 570) is nowhere near it. Hitting it means the rule is not what we
// think it is, and the event is skipped rather than half-expanded.
//
// maxPeriods bounds the outer loop separately, because a rule can generate
// nothing in a given period (BYMONTHDAY=31 in February) and a rule that
// generates nothing in *any* period would otherwise spin forever.
const (
	maxOccurrences = 5000
	maxPeriods     = 2000
)

// recurrence is the subset of RFC 5545's RRULE that htxdev expands.
//
// The subset is the design. An expander that quietly approximates a rule it
// does not understand produces dates that look plausible and are wrong, and
// nothing downstream can tell. This one refuses instead: an unsupported rule
// fails its event, the event lands in Skipped with the rule quoted in the
// error, and a human sees it. That is D6's "skip, count, report" applied to
// recurrence, and it is why hand-rolling this was preferable to the obvious
// library. See the package's Phase 3 notes for the version of that argument
// with the maintenance dates in it.
type recurrence struct {
	freq       string // DAILY, WEEKLY or MONTHLY
	interval   int    // >= 1
	count      int    // 0 when absent
	until      time.Time
	byDay      []byDay
	byMonthDay []int
}

// byDay is one BYDAY entry: an optional ordinal and a weekday. "3TH" is the
// third Thursday, "-1FR" the last Friday, "TH" every Thursday.
type byDay struct {
	ord     int
	weekday time.Weekday
}

var icsWeekdays = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

// expandRecurrence turns one recurring VEVENT into its occurrences inside
// [from, until].
//
// Every occurrence is stepped in the event's own location, never by adding a
// duration to an instant. A weekly 20:00 event is 20:00 local on both sides of
// a DST boundary, which is one hour less than seven times twenty-four apart
// across the November transition. Go's AddDate operates on the wall clock in
// the value's location, so doing it this way makes the DST case fall out
// rather than needing to be handled. Houston observes DST and the live HLUG
// rule crosses the boundary inside a 60-day window, so this is load-bearing on
// real data today, not defensive coding.
func expandRecurrence(base core.RawEvent, e *icsEvent, start, end, from, until time.Time) ([]core.RawEvent, error) {
	rec, err := parseRRULE(e.rrule, start.Location())
	if err != nil {
		return nil, fmt.Errorf("event %s: RRULE %q: %w", e.uid, e.rrule, err)
	}

	excluded, err := parseExDates(e.exdates)
	if err != nil {
		return nil, fmt.Errorf("event %s: %w", e.uid, err)
	}

	// Held as a duration rather than re-derived per occurrence. DTEND moves
	// with DTSTART, and a meeting scheduled 19:00-23:00 stays four hours long
	// on the far side of a DST change even though the instants shift.
	var duration time.Duration
	if !end.IsZero() {
		duration = end.Sub(start)
	}

	times, truncated := rec.occurrences(start, from, until)
	if truncated {
		return nil, fmt.Errorf("event %s: RRULE %q generated more than %d occurrences",
			e.uid, e.rrule, maxOccurrences)
	}

	out := make([]core.RawEvent, 0, len(times))
	for _, t := range times {
		if excluded[t.UTC().Unix()] {
			continue
		}
		ev := base
		ev.Start = t.UTC()
		if duration != 0 {
			ev.End = t.Add(duration).UTC()
		}
		// D7 says the fingerprint is the upstream stable ID, and for a
		// recurrence instance RFC 5545 says that identity is UID plus
		// RECURRENCE-ID, not UID alone. Without the suffix every instance of
		// the weekly HLUG social collides on one value, dedupe collapses nine
		// events into one, and the v1.1 ICS feed emits nine VEVENTs sharing a
		// UID. Google uses the same shape for its own split series, which is
		// where the _R20260523T000000 suffixes in the fixture come from.
		ev.UpstreamID = e.uid + "_" + t.UTC().Format(icsUTCLayout)
		out = append(out, ev)
	}
	return out, nil
}

// occurrences walks the rule from dtstart and returns the instants landing in
// [from, until], plus whether it gave up.
//
// Occurrences before `from` are generated and counted but not returned. They
// have to be generated because COUNT counts from DTSTART, so a rule with
// COUNT=10 whose first six instances are in the past has four left, and a
// walker that started at the window edge would wrongly report ten.
func (r recurrence) occurrences(dtstart, from, until time.Time) ([]time.Time, bool) {
	var (
		out       []time.Time
		generated int
		truncated bool
	)

	// emit reports whether to keep walking.
	emit := func(t time.Time) bool {
		// Candidates a period generates before DTSTART are not occurrences.
		// WEEKLY with BYDAY produces these in the first week whenever DTSTART
		// is not the earliest listed weekday.
		if t.Before(dtstart) {
			return true
		}
		generated++
		if r.count > 0 && generated > r.count {
			return false
		}
		if !r.until.IsZero() && t.After(r.until) {
			return false
		}
		if t.After(until) {
			return false
		}
		if generated > maxOccurrences {
			truncated = true
			return false
		}
		if t.Before(from) {
			return true // a real occurrence, just behind the window
		}
		out = append(out, t)
		return true
	}

	switch r.freq {
	case "DAILY":
		t := dtstart
		for p := 0; p < maxPeriods && emit(t); p++ {
			t = t.AddDate(0, 0, r.interval)
		}

	case "WEEKLY":
		if len(r.byDay) == 0 {
			t := dtstart
			for p := 0; p < maxPeriods && emit(t); p++ {
				t = t.AddDate(0, 0, 7*r.interval)
			}
			break
		}
		// parseRRULE guarantees interval == 1 here, which is what makes WKST
		// irrelevant and lets the week start anywhere. With INTERVAL >= 2 the
		// rule would need WKST to know which weeks are the every-other ones,
		// and getting that subtly wrong is worse than refusing it.
		week := dtstart.AddDate(0, 0, -int(dtstart.Weekday()))
		for p := 0; p < maxPeriods; p++ {
			stop := false
			for _, bd := range r.byDay {
				if !emit(week.AddDate(0, 0, int(bd.weekday))) {
					stop = true
					break
				}
			}
			if stop {
				break
			}
			week = week.AddDate(0, 0, 7)
		}

	case "MONTHLY":
		for p := 0; p < maxPeriods; p++ {
			stop := false
			for _, t := range r.monthDays(nthMonth(dtstart, p*r.interval), dtstart.Day()) {
				if !emit(t) {
					stop = true
					break
				}
			}
			if stop {
				break
			}
		}
	}

	return out, truncated
}

// monthDays returns the days a MONTHLY rule selects in the month whose first
// day is `first`, in ascending order.
func (r recurrence) monthDays(first time.Time, defaultDay int) []time.Time {
	last := daysInMonth(first)

	byMonthDay := r.byMonthDay
	// With neither BYDAY nor BYMONTHDAY, RFC 5545 takes the day from DTSTART.
	// A month too short to contain it is skipped rather than rolled forward:
	// "the 31st of every month" has no occurrence in February, and inventing
	// one on 1 March is the drift nthMonth already exists to prevent.
	if len(r.byDay) == 0 && len(byMonthDay) == 0 {
		byMonthDay = []int{defaultDay}
	}

	seen := map[int]bool{}
	for _, d := range byMonthDay {
		day := d
		if day < 0 {
			day = last + 1 + day // -1 is the last day
		}
		if day >= 1 && day <= last {
			seen[day] = true
		}
	}
	for _, bd := range r.byDay {
		switch {
		case bd.ord == 0:
			// Every such weekday in the month.
			offset := (int(bd.weekday) - int(first.Weekday()) + 7) % 7
			for day := 1 + offset; day <= last; day += 7 {
				seen[day] = true
			}
		case bd.ord > 0:
			offset := (int(bd.weekday) - int(first.Weekday()) + 7) % 7
			if day := 1 + offset + (bd.ord-1)*7; day <= last {
				seen[day] = true
			}
		default:
			lastWd := dayOf(first, last).Weekday()
			offset := (int(lastWd) - int(bd.weekday) + 7) % 7
			if day := last - offset + (bd.ord+1)*7; day >= 1 {
				seen[day] = true
			}
		}
	}

	days := make([]int, 0, len(seen))
	for d := range seen {
		days = append(days, d)
	}
	sortInts(days)

	out := make([]time.Time, 0, len(days))
	for _, d := range days {
		out = append(out, dayOf(first, d))
	}
	return out
}

// nthMonth returns the first day of the month n months after t, keeping t's
// clock time and location.
//
// Stepping months with AddDate(0, n, 0) directly would be wrong on any day
// past the 28th: Go normalizes overflow, so 31 January plus one month is
// 3 March, and a rule anchored on the 31st would drift forward every short
// month. Anchoring on day 1 and re-deriving the day is what avoids that.
func nthMonth(t time.Time, n int) time.Time {
	y, m, _ := t.Date()
	h, mi, s := t.Clock()
	return time.Date(y, m+time.Month(n), 1, h, mi, s, 0, t.Location())
}

func daysInMonth(first time.Time) int {
	y, m, _ := first.Date()
	// Day 0 of the next month is the last day of this one.
	return time.Date(y, m+1, 0, 0, 0, 0, 0, first.Location()).Day()
}

func dayOf(first time.Time, day int) time.Time {
	y, m, _ := first.Date()
	h, mi, s := first.Clock()
	return time.Date(y, m, day, h, mi, s, 0, first.Location())
}

func sortInts(xs []int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// parseRRULE reads the rule, or explains why it will not.
//
// loc is DTSTART's location, needed because an UNTIL without a Z is local time
// in that zone. Getting this backwards is the open bug in teambition/rrule-go
// (#67, "UNTIL is always interpreted as UTC"), and it silently truncates or
// extends a series by up to a day.
func parseRRULE(s string, loc *time.Location) (recurrence, error) {
	r := recurrence{interval: 1}
	var wkst string

	for part := range strings.SplitSeq(s, ";") {
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return recurrence{}, fmt.Errorf("part %q is not KEY=VALUE", part)
		}
		key, value = strings.ToUpper(strings.TrimSpace(key)), strings.TrimSpace(value)

		switch key {
		case "FREQ":
			switch strings.ToUpper(value) {
			case "DAILY", "WEEKLY", "MONTHLY":
				r.freq = strings.ToUpper(value)
			default:
				// YEARLY, HOURLY, MINUTELY and SECONDLY. None appear in a
				// VEVENT in any fixture; the only YEARLY rules in these feeds
				// are the DST transitions inside VTIMEZONE, which never reach
				// here because that component is skipped wholesale.
				return recurrence{}, fmt.Errorf("unsupported FREQ=%s", value)
			}

		case "INTERVAL":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return recurrence{}, fmt.Errorf("INTERVAL=%s is not a positive integer", value)
			}
			r.interval = n

		case "COUNT":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return recurrence{}, fmt.Errorf("COUNT=%s is not a positive integer", value)
			}
			r.count = n

		case "UNTIL":
			t, err := parseUntil(value, loc)
			if err != nil {
				return recurrence{}, err
			}
			r.until = t

		case "BYDAY":
			for v := range strings.SplitSeq(value, ",") {
				bd, err := parseByDay(strings.TrimSpace(v))
				if err != nil {
					return recurrence{}, err
				}
				r.byDay = append(r.byDay, bd)
			}

		case "BYMONTHDAY":
			for v := range strings.SplitSeq(value, ",") {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n == 0 || n < -31 || n > 31 {
					return recurrence{}, fmt.Errorf("BYMONTHDAY=%s is out of range", v)
				}
				r.byMonthDay = append(r.byMonthDay, n)
			}

		case "WKST":
			wkst = value

		default:
			// BYSETPOS, BYWEEKNO, BYYEARDAY, BYMONTH, BYHOUR and friends. Each
			// changes which dates a rule selects, so ignoring one produces a
			// confidently wrong schedule.
			return recurrence{}, fmt.Errorf("unsupported part %s=%s", key, value)
		}
	}

	if r.freq == "" {
		return recurrence{}, fmt.Errorf("no FREQ")
	}
	// RFC 5545 section 3.3.10: UNTIL and COUNT MUST NOT both appear.
	if r.count > 0 && !r.until.IsZero() {
		return recurrence{}, fmt.Errorf("COUNT and UNTIL are mutually exclusive")
	}

	switch r.freq {
	case "DAILY":
		if len(r.byDay) > 0 || len(r.byMonthDay) > 0 {
			return recurrence{}, fmt.Errorf("BYDAY and BYMONTHDAY are not supported with FREQ=DAILY")
		}
	case "WEEKLY":
		if len(r.byMonthDay) > 0 {
			return recurrence{}, fmt.Errorf("BYMONTHDAY is not meaningful with FREQ=WEEKLY")
		}
		for _, bd := range r.byDay {
			if bd.ord != 0 {
				return recurrence{}, fmt.Errorf("an ordinal BYDAY is not meaningful with FREQ=WEEKLY")
			}
		}
		// WKST only changes which weeks an interval selects, so it cannot
		// matter at INTERVAL=1. Every WKST in these feeds is on such a rule,
		// which is why accepting it costs nothing. Refusing the combination we
		// would have to guess at is the whole posture of this file.
		if len(r.byDay) > 0 && r.interval > 1 {
			return recurrence{}, fmt.Errorf("BYDAY with INTERVAL=%d needs WKST handling htxdev does not implement", r.interval)
		}
	case "MONTHLY":
		if len(r.byDay) > 0 && len(r.byMonthDay) > 0 {
			return recurrence{}, fmt.Errorf("BYDAY and BYMONTHDAY together need BYSETPOS to disambiguate")
		}
	}
	_ = wkst // accepted and deliberately unused; see above

	return r, nil
}

// parseByDay reads one BYDAY entry: an optional signed ordinal then a
// two-letter weekday.
func parseByDay(s string) (byDay, error) {
	if len(s) < 2 {
		return byDay{}, fmt.Errorf("BYDAY=%s is too short", s)
	}
	name := strings.ToUpper(s[len(s)-2:])
	wd, ok := icsWeekdays[name]
	if !ok {
		return byDay{}, fmt.Errorf("BYDAY=%s has no weekday", s)
	}
	bd := byDay{weekday: wd}
	if prefix := s[:len(s)-2]; prefix != "" {
		n, err := strconv.Atoi(prefix)
		if err != nil || n == 0 || n < -5 || n > 5 {
			return byDay{}, fmt.Errorf("BYDAY=%s has a bad ordinal", s)
		}
		bd.ord = n
	}
	return bd, nil
}

// parseUntil reads an UNTIL value. With a Z it is UTC; without one RFC 5545
// says it is local time in DTSTART's zone.
func parseUntil(v string, loc *time.Location) (time.Time, error) {
	switch {
	case strings.HasSuffix(v, "Z"):
		t, err := time.Parse(icsUTCLayout, v)
		if err != nil {
			return time.Time{}, fmt.Errorf("UNTIL=%s: %w", v, err)
		}
		return t, nil
	case len(v) == 8:
		t, err := time.ParseInLocation(icsDateLayout, v, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("UNTIL=%s: %w", v, err)
		}
		// A date-only UNTIL includes that whole day.
		return t.AddDate(0, 0, 1).Add(-time.Nanosecond), nil
	default:
		t, err := time.ParseInLocation(icsLocalLayout, v, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("UNTIL=%s: %w", v, err)
		}
		return t, nil
	}
}

// parseExDates collects the instants an EXDATE removes, keyed by Unix second
// so the comparison is between instants and not between wall clocks in
// whatever zone each side happened to be written in.
func parseExDates(props []icsProperty) (map[int64]bool, error) {
	if len(props) == 0 {
		return nil, nil
	}
	out := map[int64]bool{}
	for _, p := range props {
		for v := range strings.SplitSeq(p.Value, ",") {
			one := p
			one.Value = strings.TrimSpace(v)
			if one.Value == "" {
				continue
			}
			t, _, err := parseICSTime(one)
			if err != nil {
				return nil, fmt.Errorf("EXDATE: %w", err)
			}
			out[t.UTC().Unix()] = true
		}
	}
	return out, nil
}
