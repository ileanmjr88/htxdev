package source

import (
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// HOSSPage is one decoded meetings page. Same shape as TribePage and ICSFeed
// minus the parts that do not apply: no pagination and no calendar name.
type HOSSPage struct {
	Events  []core.RawEvent
	Skipped []error
}

// ParseHOSS decodes Houston Open Source Society's meetings page.
//
// This is the only source htxdev reads as HTML, and it is a deliberate
// exception rather than a pattern to copy. HOSS publishes no calendar: their
// atom.xml carries talk write-ups and member bios, not events, which was
// checked rather than assumed. What their meetings page does carry is
// <time datetime="2026-10-07T18:00:00-05:00">, which is semantic HTML with a
// specification behind it. Reading that is a different act from pattern
// matching over prose, and it is the line that makes this defensible.
//
// The standing preference is still to ask a group for an ICS feed. HOSS are an
// open source group with a Discord; it is a small favour and it would let this
// file be deleted. Treat that as the goal, not this.
//
// The page is a schedule, not a calendar: it lists the weekly slots (First
// Wednesday through Fifth Wednesday) and the next date for each. So a sync
// yields about five events covering roughly a month, refreshed every run,
// rather than everything inside the window. That is less than the other
// sources give and it is all there is.
func ParseHOSS(r io.Reader, from, until time.Time) (HOSSPage, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return HOSSPage{}, fmt.Errorf("parse html: %w", err)
	}

	cards := findByClass(doc, "meeting-card-compact")
	if len(cards) == 0 {
		// Fatal, like a missing BEGIN:VCALENDAR. A page that still parses as
		// HTML but has none of the structure we rely on means the site was
		// redesigned, and reporting zero events would read downstream as HOSS
		// cancelling everything at once.
		return HOSSPage{}, fmt.Errorf("no meeting cards found: the page structure has changed")
	}

	var page HOSSPage
	// The page also carries a next-meeting-highlight block that repeats
	// whichever card is soonest. Deduplicating on the instant here keeps that
	// from becoming a second event, and is cheap insurance against a future
	// layout that repeats a card somewhere else too.
	seen := map[int64]bool{}

	for i, card := range cards {
		week := strings.TrimSpace(textOf(firstByClass(card, "meeting-week")))
		if week == "" {
			page.Skipped = append(page.Skipped, fmt.Errorf("card %d: no meeting-week label", i))
			continue
		}

		timeEl := firstTag(card, "time")
		if timeEl == nil {
			// A slot with no next date is normal when a month has no fifth
			// Wednesday, so this is not a failure of the card.
			continue
		}
		raw := attr(timeEl, "datetime")
		if raw == "" {
			page.Skipped = append(page.Skipped, fmt.Errorf("card %q: <time> has no datetime attribute", week))
			continue
		}

		// The attribute is RFC3339 with a real offset, which is the whole
		// reason this source is workable: no zone has to be guessed, unlike
		// the floating-time case in the ICS decoder.
		start, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			page.Skipped = append(page.Skipped, fmt.Errorf("card %q: datetime %q: %w", week, raw, err))
			continue
		}
		if start.Before(from) || start.After(until) {
			continue
		}
		if seen[start.Unix()] {
			continue
		}
		seen[start.Unix()] = true

		e := core.RawEvent{
			// D7 wanted a "hash:" form for a source with no stable ID. HOSS
			// turned out not to need one: the weekly slot is a structural
			// identifier the site already uses in its own URLs, so the pair
			// (slot, instant) names an occurrence exactly. A legible ID beats
			// a digest whenever one exists, and this becomes an ICS UID in
			// v1.1 where somebody may have to read it.
			UpstreamID: slugify(week) + "_" + start.UTC().Format("20060102T150405Z"),

			// The one derivation in this decoder, and it is unavoidable: the
			// page gives no per-meeting title, only the slot label. Leaving
			// Title empty would ship five untitled events. The group name is
			// hard-coded because this decoder is already specific to HOSS by
			// construction.
			Title: "Houston Open Source Society, " + week,
			Start: start.UTC(),
		}

		// No end time on the page, so End stays zero, which the rest of the
		// pipeline already tolerates. No URL either: the compact cards link to
		// the venue, not to the meeting.
		if venue := strings.TrimSpace(textOf(firstByClass(card, "meeting-location"))); venue != "" {
			e.Venues = []core.RawVenue{{Name: venue}}
		}

		page.Events = append(page.Events, e)
	}

	return page, nil
}

// slugify turns "First Wednesday" into "first-wednesday", matching the form
// HOSS already uses in its own meeting URLs.
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '/':
			// Collapse runs, and never lead with a separator.
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// The tree helpers below are deliberately tiny. A CSS selector library would
// do all of this, but it is one more dependency for four functions, and the
// document shape this reads is four levels deep.

func hasClass(n *html.Node, want string) bool {
	if n.Type != html.ElementNode {
		return false
	}
	for field := range strings.FieldsSeq(attr(n, "class")) {
		if field == want {
			return true
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// findByClass returns every element carrying the class, without descending
// into one that already matched. Nested cards are not a thing here, and not
// recursing keeps a layout change from returning the same card twice.
func findByClass(n *html.Node, class string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if hasClass(n, class) {
			out = append(out, n)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func firstByClass(n *html.Node, class string) *html.Node {
	if found := findByClass(n, class); len(found) > 0 {
		return found[0]
	}
	return nil
}

func firstTag(n *html.Node, tag string) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := firstTag(c, tag); found != nil {
			return found
		}
	}
	return nil
}

// textOf concatenates the text under a node, collapsing whitespace. The
// collapsing matters: the markup is minified, so a venue name arrives with
// newlines and indentation around it depending on where the generator broke
// the line.
func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}
