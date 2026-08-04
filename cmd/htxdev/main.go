// Command htxdev reads an iCalendar file and prints one line per event.
//
// Stage 1: no abstractions. Parse a real feed and look at what comes out.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const defaultPath = "internal/source/testdata/ion.ics"

// event is one VEVENT, reduced to the fields we care about.
type event struct {
	UID     string
	Summary string
	Venue   string
	Start   time.Time
	URL     string
}

func main() {
	path := defaultPath
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "htxdev:", err)
		os.Exit(1)
	}

	central, err := time.LoadLocation("America/Chicago")
	if err != nil {
		fmt.Fprintln(os.Stderr, "htxdev:", err)
		os.Exit(1)
	}

	events, err := parseICS(string(data))
	if err != nil {
		fmt.Fprintln(os.Stderr, "htxdev:", err)
		os.Exit(1)
	}

	sort.Slice(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })

	for _, e := range events {
		fmt.Printf("%s  %-60s  %s\n",
			e.Start.In(central).Format("Mon 2006-01-02 3:04PM MST"),
			truncate(e.Summary, 60),
			e.Venue,
		)
	}
	fmt.Fprintf(os.Stderr, "\n%d events\n", len(events))
}

// parseICS pulls every VEVENT out of an iCalendar document.
func parseICS(doc string) ([]event, error) {
	var (
		events []event
		cur    *event
	)
	for _, line := range unfold(doc) {
		name, params, value := splitLine(line)
		switch name {
		case "BEGIN":
			if value == "VEVENT" {
				cur = &event{}
			}
		case "END":
			if value == "VEVENT" && cur != nil {
				events = append(events, *cur)
				cur = nil
			}
		}
		if cur == nil {
			continue // header, VTIMEZONE, or between events
		}
		switch name {
		case "UID":
			cur.UID = value
		case "SUMMARY":
			cur.Summary = unescape(value)
		case "URL":
			cur.URL = value
		case "LOCATION":
			cur.Venue = venueName(unescape(value))
		case "DTSTART":
			t, err := parseTime(value, params["TZID"])
			if err != nil {
				return nil, fmt.Errorf("DTSTART %q: %w", value, err)
			}
			cur.Start = t
		}
	}
	return events, nil
}

// unfold splits a document into content lines, rejoining RFC 5545 folded
// continuations (a CRLF followed by a space or tab).
func unfold(doc string) []string {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	var lines []string
	for line := range strings.SplitSeq(doc, "\n") {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] += line[1:]
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// splitLine breaks "DTSTART;TZID=America/Chicago:20260804T170000" into its
// name, parameters, and value.
func splitLine(line string) (name string, params map[string]string, value string) {
	name, value, ok := strings.Cut(line, ":")
	if !ok {
		return "", nil, ""
	}
	params = map[string]string{}
	if base, rest, ok := strings.Cut(name, ";"); ok {
		for p := range strings.SplitSeq(rest, ";") {
			k, v, _ := strings.Cut(p, "=")
			params[strings.ToUpper(k)] = strings.Trim(v, `"`)
		}
		name = base
	}
	return strings.ToUpper(name), params, value
}

// parseTime handles the three DTSTART forms: UTC ("...Z"), zoned via a TZID
// parameter, and a bare date for all-day events.
func parseTime(value, tzid string) (time.Time, error) {
	if strings.HasSuffix(value, "Z") {
		return time.ParseInLocation("20060102T150405Z", value, time.UTC)
	}
	loc := time.UTC
	if tzid != "" {
		l, err := time.LoadLocation(tzid)
		if err != nil {
			return time.Time{}, err
		}
		loc = l
	}
	if len(value) == 8 { // all-day event, VALUE=DATE
		return time.ParseInLocation("20060102", value, loc)
	}
	return time.ParseInLocation("20060102T150405", value, loc)
}

// unescape reverses RFC 5545 TEXT escaping.
func unescape(s string) string {
	return strings.NewReplacer(
		`\n`, "\n",
		`\N`, "\n",
		`\,`, ",",
		`\;`, ";",
		`\\`, `\`,
	).Replace(s)
}

// venueName takes the first component of a LOCATION, which The Events Calendar
// builds as "Venue, Street, City, State, Zip, Country".
func venueName(location string) string {
	name, _, _ := strings.Cut(location, ", ")
	return name
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
