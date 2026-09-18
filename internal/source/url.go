package source

import (
	"net/url"
	"strings"
)

// cleanURL turns whatever a feed put in a URL field into something safe to
// render as a link, or into "" if it cannot.
//
// Written because a live feed shipped `www.houstonlinux.org ` as a signup
// link: no scheme, one trailing space. Nothing validated it, so it reached the
// site as a bare href, the browser read it as a relative path, and the
// Register button on a real event pointed at
// https://htxdev.ilean.me/www.houstonlinux.org. A 404 is worse than no link,
// because the reader cannot tell it is our fault rather than the group's.
//
// This runs in the decoder rather than in normalize, for the same reason D5
// puts timestamp parsing here: a value that cannot be a URL should not get far
// enough to become a core.Event field that later code has to keep re-checking.
//
// Repairing a missing scheme rather than dropping the value is deliberate but
// narrow. `www.houstonlinux.org` is unambiguously meant to be a link, and the
// host answers https and redirects http to it. The repair only fires on
// something already shaped like a bare host, and https is the only scheme it
// will ever invent, so the worst case is a dead https link to a host somebody
// typed rather than a link to the wrong place.
func cleanURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	if !hasScheme(s) {
		// Whitespace only disqualifies a value that has no scheme. Without one
		// this is prose ("see example.org for details"), and prepending https
		// to a sentence produces a link to nowhere. With a scheme it is a URL
		// that needs encoding, not rejecting, and url.Parse does that below.
		if strings.ContainsAny(s, " \t\r\n") {
			return ""
		}
		// A bare host is the only shape worth repairing. Anything without a
		// dot is not a hostname.
		if !strings.Contains(s, ".") {
			return ""
		}
		s = "https://" + s
	}

	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	// An allowlist, not a denylist. javascript: and data: are the reason, and
	// this is also what stops ftp: and anything else with a real host, which
	// the empty-host check below would otherwise let through.
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	// Catches both a scheme with nothing after it and a site-relative path
	// that had https:// prepended, since "https:///events/5" parses with an
	// empty host. An explicit HasPrefix(s, "/") test above was removed as dead
	// code once mutation testing showed nothing could reach it.
	if u.Host == "" {
		return ""
	}
	return u.String()
}

// hasScheme reports whether s already starts with a URL scheme. Deliberately
// not a check for "http", because finding "mailto:" or "javascript:" matters:
// those have a scheme and must fail the allowlist below rather than have
// https:// prepended to them.
func hasScheme(s string) bool {
	i := strings.Index(s, ":")
	if i <= 0 {
		return false
	}
	for j, r := range s[:i] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case j > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}
