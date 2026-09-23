package source

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// Bevy is the event platform behind Snowflake's user groups, Google Developer
// Groups and a number of other vendor communities. It publishes no calendar
// feed: a chapter page has no ICS link, and the URLs where one would live
// answer with the chapter page itself. Its robots.txt closes /api/ and /gql/
// to every crawler and asks for two seconds between requests. Checked
// 2026-09-23 against usergroups.snowflake.com.
//
// What is left is two public pages, read for the two things on them with a
// specification behind them:
//
//   - the chapter page, for its <a href> links to event pages;
//   - each event page, for the schema.org Event it carries as JSON-LD, which
//     is there for search engines and has a real UTC offset on every time.
//
// The chapter page also embeds all of this and more as Next.js data, which
// would save a request per event. It is not used, on purpose. That blob is a
// cache of Bevy's internal API responses keyed by internal hostnames, it is
// exactly what robots.txt keeps crawlers away from, and it sits beside the
// chapter team's names and avatars. Links and JSON-LD are the published
// surface; the blob is plumbing that happens to be visible.

// bevyEventPath is the path of an event page. Anything else a chapter page
// links to, including the /e/<code>/ short links, is not followed.
var bevyEventPath = regexp.MustCompile(`^/events/details/[a-z0-9-]+/$`)

// ParseBevyChapter returns the event pages a chapter page links to, resolved
// against base and in document order, each once.
//
// Only links on base's own host are returned. The fetcher requests every one
// of them, so this is what keeps a link in a chapter's description from
// aiming this client somewhere else, the same rule the tribe fetcher applies
// to next_rest_url.
//
// Upcoming and past events come back together, since the page's markup does
// not say which is which. The fetcher filters on the dates it reads from each
// event page. On the Houston chapter that is one upcoming and three past, and
// the past list is capped, so the waste is a few requests rather than a
// growing one.
func ParseBevyChapter(r io.Reader, base *url.URL) ([]string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}

	var out []string
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			if u, err := base.Parse(attr(n, "href")); err == nil &&
				u.Scheme == base.Scheme && strings.EqualFold(u.Host, base.Host) &&
				bevyEventPath.MatchString(u.Path) {
				u.RawQuery, u.Fragment = "", ""
				if s := u.String(); !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if len(out) == 0 {
		// Fatal, for the reason ParseHOSS gives. Every chapter lists its past
		// events, so a page with no event links at all is a redesign rather
		// than a quiet chapter, and reporting zero events would read
		// downstream as the chapter cancelling everything.
		return nil, errors.New("no event links found: the page structure has changed")
	}
	return out, nil
}

// ErrBevyCancelled is returned for an event page whose JSON-LD marks it
// cancelled. Not a failure: the fetcher leaves the event out, and its absence
// is what tells normalize it was cancelled, the same as a feed that drops one.
var ErrBevyCancelled = errors.New("event is cancelled")

// ldEvent is the part of a schema.org Event this reads. Deliberately narrow:
// Bevy's also carries performer, with a speaker's name and photo, and offers,
// neither of which htxdev has any use for.
//
// organizer is left out too, and that one is a decision rather than
// tidiness. Bevy names the platform ("Snowflake User Groups"), not the
// chapter, so passing it on would hand normalize an organizer that resolves to
// no group, or to the wrong one if somebody ever registered the parent.
type ldEvent struct {
	Type                typeList        `json:"@type"`
	Name                string          `json:"name"`
	StartDate           string          `json:"startDate"`
	EndDate             string          `json:"endDate"`
	Description         string          `json:"description"`
	EventStatus         string          `json:"eventStatus"`
	EventAttendanceMode string          `json:"eventAttendanceMode"`
	Location            json.RawMessage `json:"location"`
}

// ldLocation covers both shapes seen on Bevy: a Place with a PostalAddress
// for an in-person meeting, and a VirtualLocation with a url for an online
// one.
type ldLocation struct {
	Type    typeList        `json:"@type"`
	Name    string          `json:"name"`
	URL     string          `json:"url"`
	Address json.RawMessage `json:"address"`
}

type ldAddress struct {
	StreetAddress   string `json:"streetAddress"`
	AddressLocality string `json:"addressLocality"`
	AddressRegion   string `json:"addressRegion"`
	PostalCode      string `json:"postalCode"`
}

// typeList is a JSON-LD @type, which the spec allows to be a string or an
// array of strings.
type typeList []string

func (t *typeList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*t = typeList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*t = many
	return nil
}

// ParseBevyEvent decodes one Bevy event page.
//
// It returns ErrBevyCancelled for a cancelled event and an error for a page
// that carries no usable Event. A page is one event, so there is no Skipped
// here: the fetcher counts a failure against the source.
func ParseBevyEvent(r io.Reader) (core.RawEvent, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return core.RawEvent{}, fmt.Errorf("parse html: %w", err)
	}

	ev, err := findLDEvent(doc)
	if err != nil {
		return core.RawEvent{}, err
	}

	// The page's own statement of its address. Taken from markup rather than
	// from the URL it was fetched at, so a redirect cannot change an event's
	// identity.
	canonical := cleanURL(canonicalURL(doc))
	if canonical == "" {
		return core.RawEvent{}, errors.New(`no <link rel="canonical">`)
	}

	if strings.HasSuffix(ev.EventStatus, "/EventCancelled") {
		return core.RawEvent{}, ErrBevyCancelled
	}

	title := strings.TrimSpace(html.UnescapeString(ev.Name))
	if title == "" {
		return core.RawEvent{}, errors.New("event has no name")
	}

	// RFC3339 or nothing. Bevy writes a real offset on every time it
	// publishes, so a time without one is a format change, and guessing a
	// zone for it is the floating-time problem the ICS decoder has to handle
	// because it has no choice. This decoder does.
	start, err := time.Parse(time.RFC3339, ev.StartDate)
	if err != nil {
		return core.RawEvent{}, fmt.Errorf("startDate %q: %w", ev.StartDate, err)
	}
	var end time.Time
	if ev.EndDate != "" {
		// An unreadable end is not worth losing the event over. End is
		// optional everywhere else in the pipeline.
		if t, err := time.Parse(time.RFC3339, ev.EndDate); err == nil {
			end = t.UTC()
		}
	}

	u, _ := url.Parse(canonical) // cleanURL has already parsed it
	e := core.RawEvent{
		// Host and path of the canonical URL, in the same spirit as tribe's
		// "iondistrict.com?id=23861". Bevy also has /e/<code>/ short links,
		// which would survive a slug edit, but they appear only inside
		// scripts, and reading those is reading the internals this decoder
		// otherwise stays out of.
		UpstreamID: u.Host + u.Path,
		Title:      title,
		// Bevy's JSON-LD description is the first hundred characters or so
		// with an ellipsis, HTML-escaped. It is still the right field: the
		// excerpt is derived from it later, and it is what Bevy publishes for
		// exactly this purpose.
		Description: ev.Description,
		Start:       start.UTC(),
		End:         end,
		URL:         canonical,
	}

	switch {
	case strings.HasSuffix(ev.EventAttendanceMode, "/OnlineEventAttendanceMode"),
		strings.HasSuffix(ev.EventAttendanceMode, "/MixedEventAttendanceMode"):
		e.Virtual = true
	}

	locs, err := decodeLocations(ev.Location)
	if err != nil {
		return core.RawEvent{}, fmt.Errorf("location: %w", err)
	}
	for _, l := range locs {
		switch {
		case slices.Contains(l.Type, "VirtualLocation"):
			// Bevy's own short link to the event page, which is where an
			// attendee joins from. Not the meeting link itself; Bevy gives
			// that only to people who registered.
			e.Virtual = true
			if e.VirtualURL == "" {
				e.VirtualURL = cleanURL(l.URL)
			}
		case slices.Contains(l.Type, "Place"):
			v := core.RawVenue{Name: strings.TrimSpace(html.UnescapeString(l.Name))}
			if a, ok := decodeAddress(l.Address); ok {
				v.Address, v.City, v.State, v.Zip = a.StreetAddress, a.AddressLocality, a.AddressRegion, a.PostalCode
			}
			if v.Name != "" || v.Address != "" {
				e.Venues = append(e.Venues, v)
			}
		}
	}

	return e, nil
}

// findLDEvent returns the first schema.org Event among the page's JSON-LD
// blocks. A block that does not decode is passed over rather than failing the
// page, because a site is free to carry JSON-LD this has no interest in, and a
// malformed breadcrumb list should not cost an event.
func findLDEvent(doc *html.Node) (ldEvent, error) {
	var blocks []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "script" && attr(n, "type") == "application/ld+json" {
			if n.FirstChild != nil {
				blocks = append(blocks, n.FirstChild.Data)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	for _, b := range blocks {
		// A block may be one object or an array of them.
		var many []json.RawMessage
		if err := json.Unmarshal([]byte(b), &many); err != nil {
			many = []json.RawMessage{json.RawMessage(b)}
		}
		for _, raw := range many {
			var ev ldEvent
			if json.Unmarshal(raw, &ev) == nil && slices.Contains(ev.Type, "Event") {
				return ev, nil
			}
		}
	}
	return ldEvent{}, fmt.Errorf("no schema.org Event in %d JSON-LD block(s)", len(blocks))
}

func decodeLocations(raw json.RawMessage) ([]ldLocation, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one ldLocation
	if err := json.Unmarshal(raw, &one); err == nil {
		return []ldLocation{one}, nil
	}
	var many []ldLocation
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, err
	}
	return many, nil
}

// decodeAddress accepts the PostalAddress object Bevy sends. schema.org also
// allows a plain string, which is kept as the street address rather than
// parsed, because splitting an address string is guessing.
func decodeAddress(raw json.RawMessage) (ldAddress, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return ldAddress{}, false
	}
	var a ldAddress
	if err := json.Unmarshal(raw, &a); err == nil {
		return a, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
		return ldAddress{StreetAddress: strings.TrimSpace(s)}, true
	}
	return ldAddress{}, false
}

func canonicalURL(doc *html.Node) string {
	var found string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != "" {
			return
		}
		if n.Type == html.ElementNode && n.Data == "link" {
			for rel := range strings.FieldsSeq(attr(n, "rel")) {
				if strings.EqualFold(rel, "canonical") {
					found = attr(n, "href")
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}
