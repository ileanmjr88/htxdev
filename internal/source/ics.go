package source

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// The three DTSTART value forms, per D5. Which one applies is decided by the
// property's parameters and by whether the value carries a Z, never by
// guessing from the length alone.
const (
	icsUTCLayout   = "20060102T150405Z" // DTSTART:20250903T020000Z
	icsLocalLayout = "20060102T150405"  // DTSTART;TZID=America/Chicago:20260529T190000
	icsDateLayout  = "20060102"         // DTSTART;VALUE=DATE:20261106
)

// houston resolves the zone that date-only and floating values are anchored
// in. htxdev is a Houston-only service, so there is a correct answer here
// rather than a policy decision.
//
// This needs the IANA database. The cmd binary embeds it with time/tzdata;
// under `go test` it comes from the host, which every platform this builds on
// has. Loaded once because LoadLocation reads and parses a file each call.
var houston = sync.OnceValues(func() (*time.Location, error) {
	return time.LoadLocation("America/Chicago")
})

// icsProperty is one unfolded content line: NAME;PARAM=VALUE;...:VALUE
//
// It keeps exactly two of the parameters iCalendar defines, because those are
// the only two htxdev acts on. This is D4 applied to a line-oriented format:
// the narrow wire struct that keeps Ion's custom_fields out of the domain has
// the same job here, and the risk is the same. ATTENDEE arrives carrying
// CN=houstonlinuxusergroup@gmail.com in the HLUG feed, and a property bag
// would carry it all the way to the database.
type icsProperty struct {
	Name   string
	TZID   string // TZID=America/Chicago
	IsDate bool   // VALUE=DATE, which is how an all-day event announces itself
	Value  string // still escaped; see unescapeText
}

// ICSFeed is one decoded iCalendar stream. Unlike TribePage there is no
// NextURL or Total: an ICS feed is a single document that carries everything
// the calendar is willing to publish, so there is nothing to paginate.
type ICSFeed struct {
	Name    string // X-WR-CALNAME. Diagnostics only, never the group name.
	Events  []core.RawEvent
	Skipped []error
}

// ParseICS decodes an iCalendar stream into RawEvents. Like ParseTribe it is
// pure and cannot reach the network.
//
// [from, until] is required rather than convenient. An ICS feed is unbounded
// in both directions: HLUG publishes 95 events that have already happened, and
// an RRULE with no UNTIL and no COUNT describes infinitely many still to come,
// so "decode this feed" is not a well-defined request without a window. This
// is the same filtering Ion does server-side from getTribe's start_date and
// end_date parameters; the difference is only that an ICS server will not do
// it for you, so the decoder must.
//
// The fetch layer passes the same window it records on Result.WindowEnd, so
// what normalize is told about the horizon and what was actually expanded
// cannot drift apart.
//
// A stream that is not iCalendar at all is fatal. A single unusable VEVENT is
// not: it lands in Skipped and the rest of the feed still decodes, which is D6
// one level down, the same split ParseTribe makes.
func ParseICS(r io.Reader, from, until time.Time) (ICSFeed, error) {
	sc := newICSScanner(r)

	var (
		feed      ICSFeed
		cur       *icsEvent
		nEvent    int
		skipDepth int  // >0 while inside a component whose contents we ignore
		inCal     bool // saw BEGIN:VCALENDAR
	)

	for sc.Scan() {
		p, ok := parseProperty(sc.Text())
		if !ok {
			continue // blank or malformed line; ICS has no other kind
		}

		switch p.Name {
		case "BEGIN":
			switch {
			case skipDepth > 0:
				// Already inside something we are ignoring. Count the nesting
				// so END:DAYLIGHT does not look like the end of VTIMEZONE.
				skipDepth++
			case equalFold(p.Value, "VCALENDAR"):
				inCal = true
			case equalFold(p.Value, "VEVENT") && cur == nil:
				nEvent++
				cur = &icsEvent{index: nEvent}
			default:
				// VTIMEZONE, VALARM, VFREEBUSY, VJOURNAL, a nested VEVENT.
				//
				// VTIMEZONE is the one that matters. It contains its own
				// DTSTART and RRULE describing DST transitions, and reading
				// those as event data would invent two events per feed dated
				// 1970 and recurring yearly. We ignore the calendar's copy of
				// the timezone rules and resolve TZID against the IANA
				// database instead, which is both more current than a copy
				// Google emitted once and the same source time.LoadLocation
				// already uses everywhere else in this program.
				skipDepth = 1
			}

		case "END":
			switch {
			case skipDepth > 0:
				skipDepth--
			case equalFold(p.Value, "VEVENT") && cur != nil:
				events, err := cur.toRawEvents(from, until)
				if err != nil {
					feed.Skipped = append(feed.Skipped, err)
				}
				feed.Events = append(feed.Events, events...)
				cur = nil
			}

		default:
			switch {
			case skipDepth > 0:
			case cur != nil:
				cur.set(p)
			case equalFold(p.Name, "X-WR-CALNAME"):
				feed.Name = unescapeText(p.Value)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return ICSFeed{}, fmt.Errorf("read feed: %w", err)
	}

	// Fatal, by the same reasoning that makes a bad tribe envelope fatal:
	// nothing can be salvaged and something upstream has changed shape. An
	// HTML error page served with a 200 lands here, which is the case worth
	// catching, since it would otherwise decode as a feed with zero events and
	// read downstream as every event being cancelled at once.
	if !inCal {
		return ICSFeed{}, fmt.Errorf("not an iCalendar stream: no BEGIN:VCALENDAR")
	}
	if cur != nil {
		feed.Skipped = append(feed.Skipped, fmt.Errorf("event %d: stream ended inside a VEVENT", cur.index))
	}

	return feed, nil
}

// icsEvent accumulates the properties of one VEVENT. The set method below is
// the allowlist; see the comment there.
type icsEvent struct {
	index int // 1-based, only for naming an event that has no UID to name it by

	uid         string
	summary     string
	description string
	location    string
	url         string
	status      string
	conference  string // X-GOOGLE-CONFERENCE

	start   icsProperty // kept whole: the parameters decide how to read the value
	end     icsProperty
	rrule   string
	exdates []icsProperty
}

// set records the properties htxdev uses and drops every other one.
//
// The default case is the point of this function, not an oversight. ATTENDEE
// carries CN= and a mailto:, ORGANIZER carries an address, and both are in the
// HLUG feed today. A decoder that collected properties into a map would carry
// them into RawEvent, the database and the public API, which is exactly the
// failure this project already found twice.
func (e *icsEvent) set(p icsProperty) {
	switch p.Name {
	case "UID":
		e.uid = p.Value
	case "SUMMARY":
		e.summary = p.Value
	case "DESCRIPTION":
		e.description = p.Value
	case "LOCATION":
		e.location = p.Value
	case "URL":
		e.url = p.Value
	case "STATUS":
		e.status = p.Value
	case "X-GOOGLE-CONFERENCE":
		e.conference = p.Value
	case "DTSTART":
		e.start = p
	case "DTEND":
		e.end = p
	case "RRULE":
		e.rrule = p.Value
	case "EXDATE":
		e.exdates = append(e.exdates, p)
	}
}

// toRawEvents maps one VEVENT onto the domain. It returns a slice rather than
// a single event because one recurring VEVENT is many events: Google publishes
// a weekly meeting as one component plus an RRULE and leaves expansion to the
// consumer. That is the structural difference from toRawEvent, where one tribe
// record is always exactly one event.
func (e *icsEvent) toRawEvents(from, until time.Time) ([]core.RawEvent, error) {
	// No UID, no usable Fingerprint. D7 makes the upstream ID load-bearing:
	// every event without one would collide on the same value, and the value
	// becomes the ICS UID verbatim in v1.1. RFC 5545 makes UID mandatory, and
	// all 111 fixture events have one, so this fires only on a format change.
	if e.uid == "" {
		return nil, fmt.Errorf("event %d: no UID", e.index)
	}
	if e.start.Name == "" {
		return nil, fmt.Errorf("event %s: no DTSTART", e.uid)
	}

	// The feed is telling us this one is off. Dropping it without recording a
	// skip is deliberate and the distinction matters downstream: Skipped means
	// "we could not read this, so do not trust its absence", while a cancelled
	// event is absent on purpose and absence is exactly the right conclusion.
	if equalFold(e.status, "CANCELLED") {
		return nil, nil
	}

	start, allDay, err := parseICSTime(e.start)
	if err != nil {
		return nil, fmt.Errorf("event %s: DTSTART: %w", e.uid, err)
	}

	// An absent DTEND is tolerated, matching toRawEvent: an event with a start
	// is still displayable. A DTEND that is present and unreadable is a format
	// change and fails the event.
	var end time.Time
	if e.end.Name != "" {
		end, _, err = parseICSTime(e.end)
		if err != nil {
			return nil, fmt.Errorf("event %s: DTEND: %w", e.uid, err)
		}
	}

	base := core.RawEvent{
		UpstreamID: e.uid, // no "ics:" prefix; namespacing happens at normalize
		Title:      unescapeText(e.summary),
		// Escaping is transport, so it comes off here. Any HTML entities
		// underneath stay, the same way tribe leaves its Description raw for
		// the excerpt step to deal with once.
		Description: unescapeText(e.description),
		Start:       start.UTC(),
		End:         utcOrZero(end),
		AllDay:      allDay,
		URL:         e.url,
		VirtualURL:  e.conference,
		Virtual:     e.conference != "",
	}

	// LOCATION is one unstructured string: "Finn MacCool's Irish Bar, 1127
	// Eldridge Pkwy Suite 600, Houston, TX 77077, USA". Splitting it into
	// address parts is venue resolution, which is Phase 5's job and needs the
	// curated venue list to do properly. RawVenue.Name holding the whole
	// string is what D5's "unenriched, not unparsed" means here, and it is the
	// real test of whether core.RawVenue sits at the right boundary: the tribe
	// decoder fills six fields, this one fills one, and normalize reads both.
	if e.location != "" {
		base.Venues = []core.RawVenue{{Name: unescapeText(e.location)}}
	}

	if e.rrule == "" {
		// Outside the window. Not an error and not a skip: the feed is fine,
		// this event simply is not the question being asked. Recording it as
		// skipped would be actively harmful, because a non-empty Skipped
		// defers cancellation for the whole source, and HLUG would defer on
		// every run forever on the strength of 95 events from last year.
		if base.Start.Before(from) || base.Start.After(until) {
			return nil, nil
		}
		return []core.RawEvent{base}, nil
	}
	return expandRecurrence(base, e, start, end, from, until)
}

// parseICSTime resolves one date-time property to an instant, per D5.
//
// The returned time keeps its own location rather than being normalized to UTC
// here, because recurrence expansion has to step in the event's local wall
// clock. Callers storing into RawEvent convert with UTC().
func parseICSTime(p icsProperty) (t time.Time, allDay bool, err error) {
	v := p.Value

	switch {
	// Form 3, the all-day case behind D10. Stored as midnight Chicago rather
	// than midnight UTC: an all-day timestamp is only meaningful rendered back
	// in Chicago, and midnight UTC is 19:00 the previous day here, which
	// prints the wrong date for five hours of every day.
	case p.IsDate:
		loc, err := houston()
		if err != nil {
			return time.Time{}, false, fmt.Errorf("load America/Chicago: %w", err)
		}
		t, err := time.ParseInLocation(icsDateLayout, v, loc)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("date %q: %w", v, err)
		}
		return t, true, nil

	// Form 1. The Z is the zone marker, so time.Parse yields UTC directly.
	case strings.HasSuffix(v, "Z"):
		t, err := time.Parse(icsUTCLayout, v)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("utc time %q: %w", v, err)
		}
		return t, false, nil

	// Form 2. ParseInLocation, not Parse: the value has no offset and means
	// local time in the named zone. This is the mirror image of the tribe
	// decoder, where the value also carries no marker but is already UTC, so
	// using ParseInLocation there would double-shift it.
	case p.TZID != "":
		loc, err := time.LoadLocation(p.TZID)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("unknown TZID %q: %w", p.TZID, err)
		}
		t, err := time.ParseInLocation(icsLocalLayout, v, loc)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("local time %q: %w", v, err)
		}
		return t, false, nil

	// RFC 5545 calls this floating local time: no Z, no TZID, meaning
	// "whatever the clock says wherever the reader is". For a Houston events
	// service the reader is in Houston. None of the three fixtures contain
	// one, so this is the branch written from the spec rather than from data,
	// and the one to distrust first if a new feed comes out wrong.
	default:
		loc, err := houston()
		if err != nil {
			return time.Time{}, false, fmt.Errorf("load America/Chicago: %w", err)
		}
		t, err := time.ParseInLocation(icsLocalLayout, v, loc)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("floating time %q: %w", v, err)
		}
		return t, false, nil
	}
}

// utcOrZero keeps a zero End zero. t.UTC() on a zero Time produces a non-zero
// value that IsZero reports false for, which would make "no end time" look
// like "ends on 1 January year 1".
func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// unescapeText reverses RFC 5545 section 3.3.11 escaping in one pass.
//
// One pass, not three ReplaceAll calls. Turning \\ into \ first makes a
// literal backslash followed by the letter n indistinguishable from the \n
// escape, so the next pass would turn it into a newline. This is not
// hypothetical: the Meetup feed contains "select Houston as your city\\." and
// the sequential version corrupts any description that pairs the two.
func unescapeText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		case '\\', ';', ',':
			b.WriteByte(s[i])
		default:
			// Not an escape the RFC defines. Keep both bytes rather than
			// silently eating the backslash, so a feed doing something we do
			// not model stays visible instead of quietly losing characters.
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// parseProperty splits one unfolded content line into name, the parameters we
// care about, and the raw value.
func parseProperty(line string) (icsProperty, bool) {
	head, value, ok := cutUnquoted(line, ':')
	if !ok {
		return icsProperty{}, false
	}

	name, params, _ := cutUnquoted(head, ';')
	p := icsProperty{Name: strings.ToUpper(strings.TrimSpace(name)), Value: value}
	if p.Name == "" {
		return icsProperty{}, false
	}

	for params != "" {
		var kv string
		kv, params, _ = cutUnquoted(params, ';')
		k, v, _ := strings.Cut(kv, "=")
		v = strings.Trim(v, `"`)
		switch strings.ToUpper(k) {
		case "TZID":
			p.TZID = v
		case "VALUE":
			p.IsDate = equalFold(v, "DATE")
		}
		// Every other parameter is dropped. See the type's comment.
	}
	return p, true
}

// cutUnquoted splits s at the first sep outside a double-quoted parameter
// value. A plain strings.Cut is wrong here: RFC 5545 allows quoting a
// parameter value precisely so it can contain the delimiters, and
// `ATTENDEE;CN="Doe, John":mailto:x@y` would otherwise split inside the name.
func cutUnquoted(s string, sep byte) (before, after string, found bool) {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case sep:
			if !inQuote {
				return s[:i], s[i+1:], true
			}
		}
	}
	return s, "", false
}

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }

// icsScanner yields unfolded content lines.
//
// RFC 5545 folds long lines at 75 octets and marks the continuation with a
// leading space or tab, which the reader strips. Folding is not cosmetic and
// cannot be ignored: HLUG folds 165 of its lines, and a LOCATION split across
// two physical lines parses as a LOCATION with half an address followed by a
// line with no colon in it.
//
// Deciding a line is complete requires reading the next one, so the scanner
// keeps a single line of lookahead.
type icsScanner struct {
	sc   *bufio.Scanner
	peek string
	held bool
	line string
}

func newICSScanner(r io.Reader) *icsScanner {
	return &icsScanner{sc: bufio.NewScanner(r)}
}

func (s *icsScanner) physical() (string, bool) {
	if s.held {
		s.held = false
		return s.peek, true
	}
	if s.sc.Scan() {
		return s.sc.Text(), true
	}
	return "", false
}

func (s *icsScanner) Scan() bool {
	first, ok := s.physical()
	if !ok {
		return false
	}

	var b strings.Builder
	b.WriteString(first)
	for {
		next, ok := s.physical()
		if !ok {
			break
		}
		// A blank line is not a continuation even though it is not a content
		// line either. Treating it as one would glue the next property onto
		// the previous value.
		if next != "" && (next[0] == ' ' || next[0] == '\t') {
			b.WriteString(next[1:])
			continue
		}
		s.peek, s.held = next, true
		break
	}

	s.line = b.String()
	return true
}

func (s *icsScanner) Text() string { return s.line }

// Err reports a read failure. bufio.ScanLines strips the CR of a CRLF pair, so
// nothing here has to know the fixtures are CRLF, which all three are.
func (s *icsScanner) Err() error { return s.sc.Err() }
