package normalize

import (
	"strings"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// roomPrefixes are the words that make a comma-separated segment a room rather
// than part of a street address. Only consulted once the segment before it has
// already matched a curated venue, which is what keeps "Suite 600" in
// "1127 Eldridge Pkwy Suite 600" from being mistaken for one.
var roomPrefixes = []string{"room", "rooms", "conference room", "suite", "floor", "studio", "hall", "lobby", "auditorium"}

// resolveVenue decides where an event is, from whatever the feed said.
//
// Three shapes arrive and all three are in the live data:
//
//	[]RawVenue{{"Ion – Conference Room 030"}}          tribe, one venue, en dash
//	[]RawVenue{{"Ion – Lobby"}, {"Ion"}}               tribe, [room, building]
//	[]RawVenue{{"The Ion, Room 30, 4201 Main St, …"}}  ics, one flat LOCATION string
//
// The answer is a canonical venue name and, where there is one, the room split
// off it. An unrecognised venue is not an error: the architecture expects
// venues to be discovered from event data and curated in sources.yaml only
// when a name needs canonicalising. So the name survives even when nothing
// matches, and the store gives it a row.
func (n *Normalizer) resolveVenue(vs []core.RawVenue) (name, room string) {
	if len(vs) == 0 {
		return "", ""
	}

	// Ion sends the hierarchy outright on 10 of 67 events, as a two-element
	// array whose order is the hierarchy: room first, building last. Using the
	// last element rather than the second means a three-level array would
	// still name the building.
	if len(vs) > 1 {
		outer := strings.TrimSpace(vs[len(vs)-1].Name)
		inner := strings.TrimSpace(vs[0].Name)

		// The inner element is only a room when it names the outer one:
		// "Ion – Lobby" and "Ion Plaza" beside "Ion". Strip that prefix and
		// what is left is the room.
		if room, ok := stripVenuePrefix(inner, outer); ok {
			if canonical, matched := n.canonicalVenue(outer); matched {
				return canonical, room
			}
			return outer, room
		}

		// It does not, so it is somewhere else that happens to sit inside the
		// outer one's district, and the event is at the inner place rather
		// than the outer one. Ion sends [Greentown Labs, Ion]; Greentown Labs
		// is its own building a couple of streets away, and filing it as a
		// room of the Ion sends people to the wrong door. sources.yaml already
		// records the same trap for Industrious and Second Draught, which
		// share an address with the Ion and are separate venues.
		if canonical, matched := n.canonicalVenue(inner); matched {
			return canonical, ""
		}
		return inner, ""
	}

	raw := strings.TrimSpace(vs[0].Name)

	// Whole string first. "Ion", "Improving Houston", "Second Draught" and
	// every HOSS venue arrive this way and need no splitting at all.
	if canonical, ok := n.canonicalVenue(raw); ok {
		return canonical, ""
	}

	// The tribe form: an en dash separates building from room. Split on the
	// first one only, so "Ion – Conference Room 029 – 030" keeps the second
	// dash inside the room where it belongs.
	if before, after, ok := splitOnDash(raw); ok {
		if canonical, matched := n.canonicalVenue(before); matched {
			return canonical, after
		}
		return before, after
	}

	// The ICS form: one flat LOCATION string, comma separated, venue first.
	if before, after, ok := strings.Cut(raw, ","); ok {
		before = strings.TrimSpace(before)
		if canonical, matched := n.canonicalVenue(before); matched {
			// Only now is it safe to look for a room. The segment after a
			// venue we recognise is either a room or the start of an address
			// we already know, and a leading digit rules out the former.
			return canonical, roomSegment(after)
		}
		// Unrecognised: keep the venue name and drop the address, which
		// RawVenue could not parse into fields either. Phase 6 links to the
		// group, not to a map.
		return before, ""
	}

	return raw, ""
}

// stripVenuePrefix reports whether inner begins with outer, and returns what
// is left once the name and any separator are removed.
//
// Folded, so an en dash and a hyphen compare equal and case does not matter.
// "Ion – Lobby" against "Ion" gives "Lobby"; "Ion Plaza" gives "Plaza";
// "Greentown Labs" gives nothing, which is the whole point.
func stripVenuePrefix(inner, outer string) (room string, ok bool) {
	fi, fo := foldKey(inner), foldKey(outer)
	if fo == "" || !strings.HasPrefix(fi, fo) {
		return "", false
	}
	if fi == fo {
		// The two elements are the same place, so there is no room.
		return "", true
	}
	rest := strings.TrimLeft(fi[len(fo):], " -")
	if rest == "" {
		return "", true
	}
	// Recover the original casing by taking the same number of trailing runes
	// from inner, since folding only replaces runes one for one.
	ir := []rune(inner)
	if n := len([]rune(rest)); n <= len(ir) {
		return strings.TrimSpace(string(ir[len(ir)-n:])), true
	}
	return rest, true
}

// canonicalVenue matches a name against the curated list, by name or alias,
// and returns the curated spelling. That is the point of curating one: HLUG
// writes "The Ion" and Ion writes "Ion", and both have to end up as one venue
// or the same building appears twice on the site.
func (n *Normalizer) canonicalVenue(name string) (string, bool) {
	if v, ok := n.venuesByName[foldKey(name)]; ok {
		return v.Name, true
	}
	return "", false
}

// splitOnDash splits at the first dash of any width, once folding has made
// them comparable. Ion's separator is an en dash that arrives HTML-escaped as
// &#8211; and is unescaped by the tribe decoder before it ever gets here.
func splitOnDash(s string) (before, after string, found bool) {
	for _, sep := range []string{"–", "—", " - "} {
		if b, a, ok := strings.Cut(s, sep); ok {
			return strings.TrimSpace(b), strings.TrimSpace(a), true
		}
	}
	return s, "", false
}

// roomSegment returns the segment if it names a room, and "" if it is the
// beginning of a street address.
func roomSegment(rest string) string {
	seg, _, _ := strings.Cut(rest, ",")
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return ""
	}
	// Has to START with a room word, not merely contain one. That is what
	// keeps "1127 Eldridge Pkwy Suite 600" out: it contains "Suite" but
	// begins with a street number.
	//
	// There was an explicit leading-digit guard here as well, on the theory
	// that a street number is never a room. Mutation testing removed it and
	// nothing failed, because a segment starting with a digit cannot start
	// with a room word either. It was redundant rather than untested, so it
	// is gone; the case it was defending is the second one in TestRoomSegment.
	folded := foldKey(seg)
	for _, p := range roomPrefixes {
		if folded == p || strings.HasPrefix(folded, p+" ") {
			return seg
		}
	}
	return ""
}
