// Package normalize turns what feeds said into what htxdev concluded.
//
// It reads core.RawEvent and writes core.Event, which is the only place in the
// pipeline where those two types meet. Everything upstream is deliberately
// unenriched: the decoders record what arrived and the fetch layer records
// where it arrived from. Every judgement about what a record *means* is here.
//
// It imports core and registry and nothing else. No HTTP, no SQL, no clock:
// give it the same inputs twice and it produces the same output, which is what
// makes the dedupe decisions testable at all.
package normalize

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/ileanmjr88/htxdev/internal/core"
	"github.com/ileanmjr88/htxdev/internal/registry"
)

// Normalizer holds the curated data that resolution needs: which feed belongs
// to which group, and every name a group is known by.
type Normalizer struct {
	sources map[string]core.Source // feed URL, per D14
	groups  map[string]core.Group  // slug
	byName  map[string]string      // folded name or alias -> group slug
}

func New(reg *registry.Registry) *Normalizer {
	n := &Normalizer{
		sources: make(map[string]core.Source, len(reg.Sources)),
		groups:  make(map[string]core.Group, len(reg.Groups)),
		byName:  make(map[string]string),
	}
	for _, s := range reg.Sources {
		n.sources[s.URL] = s
	}
	for _, g := range reg.Groups {
		n.groups[g.Slug] = g
		// A group answers to its slug, its name, and every alias someone
		// else's feed writes it under. First claim wins, so a name two groups
		// both list resolves the same way on every run rather than depending
		// on map iteration order.
		for _, name := range append([]string{g.Slug, g.Name}, g.Aliases...) {
			if key := foldKey(name); key != "" {
				if _, taken := n.byName[key]; !taken {
					n.byName[key] = g.Slug
				}
			}
		}
	}
	return n
}

// resolved is one raw event with the two things resolution decided about it.
type resolved struct {
	raw   core.RawEvent
	src   core.Source
	group core.Group
}

// Events resolves, deduplicates and merges.
//
// Problems are returned rather than causing a failure, on the same principle
// as Skipped one layer up: one event nobody can attribute must not discard the
// other seventy-five.
func (n *Normalizer) Events(raw []core.RawEvent) ([]core.Event, []error) {
	var problems []error

	clusters := map[string][]resolved{}
	var order []string // insertion order, so output does not depend on map iteration

	for _, e := range raw {
		src, ok := n.sources[e.SourceKey]
		if !ok {
			problems = append(problems, fmt.Errorf("event %q: no source registered for %q",
				e.UpstreamID, e.SourceKey))
			continue
		}
		group, ok := n.groupFor(src, e)
		if !ok {
			problems = append(problems, fmt.Errorf("event %q: source %q names group %q, which is not in the registry",
				e.UpstreamID, e.SourceKey, src.GroupSlug))
			continue
		}

		key := clusterKey(group.Slug, e.Start)
		if _, seen := clusters[key]; !seen {
			order = append(order, key)
		}
		clusters[key] = append(clusters[key], resolved{raw: e, src: src, group: group})
	}

	out := make([]core.Event, 0, len(order))
	for _, k := range order {
		out = append(out, merge(clusters[k]))
	}
	return out, problems
}

// clusterKey is the dedupe key: the group an event belongs to, and the instant
// it starts.
//
// Not the title, which differs between feeds for the same meeting ("Houston
// Linux User Group" from Ion, "Houston Linux - Ion User Meeting" from HLUG).
// Not the venue, which also differs ("Ion – Conference Room 030" against "The
// Ion, Room 30, 4201 Main St…"). And emphatically not the start alone: in one
// live window five instants carry more than one event and only two of those
// are duplicates. HLUG and HOSS both meet Wednesdays at 6pm, so they collide
// constantly and are never the same event.
//
// The group is what separates them, which is why resolution has to happen
// first and why sources.yaml's aliases are load-bearing rather than decorative.
func clusterKey(groupSlug string, start time.Time) string {
	return groupSlug + "|" + start.UTC().Format(time.RFC3339)
}

// groupFor decides whose event this is.
//
// An organizer naming a group the registry knows beats the feed that carried
// it. That single rule is what makes a venue calendar usable: Ion publishes a
// dozen groups' events, and the organizer field is the only thing that says
// whose. It costs nothing on a group's own feed, where there is either no
// organizer at all (ICS carries none) or one naming the group itself.
//
// Falling back to the feed's owner covers both remaining cases: a group's own
// calendar, and Ion's own programming, whose organizer is "Ion" and matches no
// group because the group is named "Ion District".
func (n *Normalizer) groupFor(src core.Source, e core.RawEvent) (core.Group, bool) {
	for _, o := range e.Organizers {
		if slug, ok := n.byName[foldKey(o.Name)]; ok {
			return n.groups[slug], true
		}
	}
	g, ok := n.groups[src.GroupSlug]
	return g, ok
}

// merge collapses one cluster into the event htxdev publishes.
//
// Priority picks the winner, and the winner supplies identity: fingerprint,
// title and the links. A group's own feed is priority 10 and a venue's listing
// of it is 50, because the organizer is authoritative about their own event.
//
// Everything else is gap-filled from the losers in priority order, which is
// the part worth arguing about. Straight winner-takes-all throws away real
// information: for the Houston Linux meeting at the Ion, HLUG wins on priority
// and its record is the *worse* one, carrying a flat address string and no
// organizer where Ion's names the room. Taking identity from the winner and
// filling the blanks from everyone else keeps both.
func merge(rs []resolved) core.Event {
	// Stable, so equal-priority events keep feed order, and tie-broken on the
	// upstream id so a run is reproducible rather than dependent on which
	// goroutine finished first.
	slices.SortStableFunc(rs, func(a, b resolved) int {
		if c := cmp.Compare(a.src.Priority, b.src.Priority); c != 0 {
			return c
		}
		return cmp.Compare(a.raw.UpstreamID, b.raw.UpstreamID)
	})

	w := rs[0]
	ev := core.Event{
		Fingerprint: core.Fingerprint(w.src.Kind, w.raw.UpstreamID),
		GroupSlug:   w.group.Slug,
		Title:       w.raw.Title,
		Start:       w.raw.Start.UTC(),
		End:         w.raw.End,
		AllDay:      w.raw.AllDay,
		URL:         w.raw.URL,
		RegisterURL: w.raw.RegisterURL,
		Virtual:     w.raw.Virtual,
	}

	for _, r := range rs {
		if !slices.Contains(ev.SourceKeys, r.raw.SourceKey) {
			ev.SourceKeys = append(ev.SourceKeys, r.raw.SourceKey)
		}

		// Gap-filling proper. The winner is in this loop too and simply has
		// nothing to fill, which keeps the rule in one place instead of
		// stating it once for the winner and again for everyone else.
		if ev.End.IsZero() {
			ev.End = r.raw.End
		}
		if ev.URL == "" {
			ev.URL = r.raw.URL
		}
		if ev.RegisterURL == "" {
			ev.RegisterURL = r.raw.RegisterURL
		}
		// Virtual is a claim, not a gap: one feed saying an event has a stream
		// is enough, and no feed contradicts another by staying silent.
		if r.raw.Virtual {
			ev.Virtual = true
		}
	}

	if !ev.End.IsZero() {
		ev.End = ev.End.UTC()
	}
	return ev
}

// foldKey normalizes a name for matching: curly punctuation folded to ASCII,
// lowercased, whitespace collapsed.
//
// The apostrophe is not hypothetical. Ion publishes the HLUG organizer as
// "Houston Linux User’s Group" with U+2019, and sources.yaml records that
// exact byte sequence as an alias, so today an exact comparison would work.
// Folding is what keeps the match alive the day Ion switches to a straight
// quote, which is the kind of change nobody announces and which would
// otherwise silently stop two feeds deduplicating.
func foldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '‘', '’', 'ʼ': // ' ' ʼ
			b.WriteByte('\'')
		case '“', '”': // " "
			b.WriteByte('"')
		case '–', '—', '−': // en dash, em dash, minus
			b.WriteByte('-')
		case ' ', '​': // non-breaking space, zero-width space
			b.WriteByte(' ')
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
