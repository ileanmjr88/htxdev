package source

import "testing"

func TestCleanURL(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// The one that shipped. Ion's feed carried this as a signup link for a
		// real HLUG meeting; with no scheme the browser read it as a relative
		// path and the Register button pointed at
		// https://htxdev.ilean.me/www.houstonlinux.org.
		{"the live bug: no scheme, trailing space", "www.houstonlinux.org ", "https://www.houstonlinux.org"},

		{"already absolute", "https://houstonlinux.org/", "https://houstonlinux.org/"},
		{"http stays http", "http://example.org/x", "http://example.org/x"},
		{"bare host", "example.org", "https://example.org"},
		{"bare host with path", "example.org/events/5", "https://example.org/events/5"},
		{"leading and trailing space", "  https://example.org  ", "https://example.org"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},

		// A path is relative to a site this code does not know. Inventing an
		// origin would be inventing a destination.
		{"site-relative path", "/events/5", ""},
		{"site-relative path with a dot", "/events/5.html", ""},
		{"protocol-relative", "//example.org/x", ""},

		// Added after mutation testing: without the scheme allowlist this
		// passes, because unlike javascript: and mailto: it has a real host
		// and the empty-host check does not catch it.
		{"ftp has a host and still must not pass", "ftp://example.org/x", ""},

		// A space in a path is an encoding problem, not a disqualification.
		{"space in a path is encoded", "https://example.org/a b", "https://example.org/a%20b"},
		{"schemeless prose with a dot is not a URL", "see example.org for details", ""},

		// This is the one that makes the whitespace check load-bearing rather
		// than dead. url.Parse rejects a space in a HOST, so "see example.org
		// for details" fails there anyway; a space after the host parses fine
		// and would become a link. Schemeless with a space is prose.
		{"schemeless with a space after the host", "example.org/a b", ""},
		{"a sentence, not a URL", "see our website for details", ""},
		{"no dot, not a host", "tbd", ""},

		// The reason the scheme check is an allowlist rather than a search for
		// "http". Prepending https:// to these would produce a working link to
		// something that should never be linked.
		{"javascript", "javascript:alert(1)", ""},
		{"data", "data:text/html,x", ""},
		{"mailto", "mailto:someone@example.org", ""},
		{"file", "file:///etc/passwd", ""},

		{"scheme but no host", "https://", ""},
		{"embedded newline", "https://example.org/\nx", ""},
		{"embedded tab", "https://example.org/\tx", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanURL(tc.in); got != tc.want {
				t.Errorf("cleanURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Whatever comes out is either empty or safe to put in an href. Stated as a
// property rather than as more rows, because the risk is an input nobody
// thought to write down.
func TestCleanURLOutputIsAlwaysSafe(t *testing.T) {
	inputs := []string{
		"www.houstonlinux.org ", "javascript:alert(1)", "JAVASCRIPT:alert(1)",
		"  data:text/html,x  ", "/relative", "example.org", "vbscript:msgbox(1)",
		"https://ok.example/path?q=1#f", "", "::::", "http://", "//protocol-relative.example",
		"HTTPS://UPPER.EXAMPLE", "\u0000https://example.org",
	}
	for _, in := range inputs {
		got := cleanURL(in)
		if got == "" {
			continue
		}
		if len(got) < 8 || (got[:7] != "http://" && got[:8] != "https://") {
			t.Errorf("cleanURL(%q) = %q, which is neither empty nor http(s)", in, got)
		}
	}
}
