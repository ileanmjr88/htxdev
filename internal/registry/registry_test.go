package registry

import (
	"strings"
	"testing"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// The committed registry must always be loadable. This is the test that turns
// the loader into a CI gate: a pull request that breaks sources.yaml fails here
// rather than at the next sync.
func TestRealSourcesYAMLIsValid(t *testing.T) {
	reg, err := LoadFile("../../data/sources.yaml")
	if err != nil {
		t.Fatalf("committed sources.yaml does not load:\n%v", err)
	}

	if len(reg.Groups) != 9 {
		t.Errorf("got %d groups, want 9", len(reg.Groups))
	}
	if len(reg.Venues) != 6 {
		t.Errorf("got %d venues, want 6", len(reg.Venues))
	}
	if len(reg.Sources) != 9 {
		t.Errorf("got %d sources, want 9", len(reg.Sources))
	}
	if got := len(reg.EnabledSources()); got != 9 {
		t.Errorf("got %d enabled sources, want 9", got)
	}

	// HOSS publishes no calendar, so it is the one source read as HTML. This
	// asserted "no sources at all" until 2026-09-17, when the decoder for
	// their meetings page landed. If this ever goes back to ics, that is them
	// having shipped a feed and this file getting simpler.
	if _, ok := reg.Group("houston-open-source-society"); !ok {
		t.Error("HOSS missing from the registry")
	}
	var hoss []core.Source
	for _, s := range reg.Sources {
		if s.GroupSlug == "houston-open-source-society" {
			hoss = append(hoss, s)
		}
	}
	if len(hoss) != 1 {
		t.Fatalf("HOSS has %d sources, want 1", len(hoss))
	}
	if hoss[0].Kind != core.KindHTML {
		t.Errorf("HOSS source kind = %q, want %q", hoss[0].Kind, core.KindHTML)
	}

	// The alias that lets HLUG's "The Ion, Rooms 29 and 30, ..." resolve to
	// the same venue Ion's own feed calls "Ion".
	var ion *core.Venue
	for i := range reg.Venues {
		if reg.Venues[i].Slug == "ion" {
			ion = &reg.Venues[i]
		}
	}
	if ion == nil {
		t.Fatal("no venue with slug ion")
	}
	if len(ion.Aliases) == 0 || ion.Aliases[0] != "The Ion" {
		t.Errorf("ion aliases = %v, want [The Ion]", ion.Aliases)
	}

	// Verification state. This asserted that NOTHING was verified until
	// 2026-09-17, which was true when it was written and stopped being true
	// the moment somebody did the thing the whole project is waiting for. The
	// useful assertions are about which groups, not how many.
	verified := map[string]bool{}
	for _, g := range reg.Groups {
		if g.VerifiedBy != "" {
			verified[g.Slug] = true
			// The loader already rejects a verified_by with no verified_at.
			// This says the same thing from the other side, because a
			// verification with no date is a decision with no provenance.
			if g.VerifiedAt.IsZero() {
				t.Errorf("group %s is verified by %q with no date", g.Slug, g.VerifiedBy)
			}
		}
	}

	if !verified["ion-district"] {
		t.Error("ion-district is not verified; it was on 2026-09-17, so this is a revert")
	}

	// HLUG must stay unverified until its private entries are handled.
	// Its public calendar runs off an account that is also somebody's personal
	// one, so it carries appointments beside real meetings. data/rejects.yaml
	// covers the one in the current window; verifying the group is a separate
	// decision that needs a look at what else is in there.
	if verified["houston-linux-user-group"] {
		t.Error("houston-linux-user-group is verified, but its feed carries personal entries; " +
			"see data/rejects.yaml and the note in sources.yaml before doing this")
	}
}

func TestLoadFlattensSourcesOntoGroups(t *testing.T) {
	const in = `
venues: []
groups:
  - slug: alpha
    name: Alpha
    sources:
      - kind: ics
        url: https://example.org/a.ics
        priority: 10
        enabled: true
      - kind: tribe
        url: https://example.org/wp-json/tribe/events/v1/events
        priority: 50
        enabled: false
`
	reg, err := Load(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reg.Sources) != 2 {
		t.Fatalf("got %d sources, want 2", len(reg.Sources))
	}
	for _, s := range reg.Sources {
		if s.GroupSlug != "alpha" {
			t.Errorf("source %s has GroupSlug %q, want alpha", s.URL, s.GroupSlug)
		}
	}
	if got := reg.EnabledSources(); len(got) != 1 || got[0].Kind != core.KindICS {
		t.Errorf("EnabledSources = %+v, want the one enabled ics source", got)
	}
	// A group with no `active` key is active. Removing it from the file is how
	// you deactivate one.
	if !reg.Groups[0].Active {
		t.Error("group should default to Active")
	}
}

// Renamed 2026-09-17. This is about Load() rejecting an invalid registry, and
// it collided with the test for LoadRejects(), which reads the reject list.
// Two different senses of the word, one of which is now a type.
func TestLoadRejectsInvalidRegistries(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // substring the error must mention
	}{
		{
			name: "duplicate group slug",
			in: `
groups:
  - {slug: a, name: A}
  - {slug: a, name: Again}`,
			want: "duplicate slug",
		},
		{
			name: "duplicate venue slug",
			in: `
venues:
  - {slug: ion, name: Ion}
  - {slug: ion, name: Ion Again}`,
			want: "duplicate slug",
		},
		{
			name: "unknown source kind",
			in: `
groups:
  - slug: a
    name: A
    sources:
      - {kind: rss, url: "https://example.org/f", enabled: true}`,
			want: "unknown kind",
		},
		{
			name: "non-https feed url",
			in: `
groups:
  - slug: a
    name: A
    sources:
      - {kind: ics, url: "http://example.org/f.ics", enabled: true}`,
			want: "absolute https URL",
		},
		{
			name: "relative feed url",
			in: `
groups:
  - slug: a
    name: A
    sources:
      - {kind: ics, url: "/feed.ics", enabled: true}`,
			want: "absolute https URL",
		},
		{
			// Two groups pointing at one feed would ingest every event twice
			// and attribute it to whichever group happened to win.
			name: "same feed claimed twice",
			in: `
groups:
  - slug: a
    name: A
    sources:
      - {kind: ics, url: "https://example.org/f.ics", enabled: true}
  - slug: b
    name: B
    sources:
      - {kind: ics, url: "https://example.org/f.ics", enabled: true}`,
			want: "already claimed",
		},
		{
			name: "verified_by without verified_at",
			in: `
groups:
  - {slug: a, name: A, verified_by: ileanmjr88}`,
			want: "verified_at is blank",
		},
		{
			name: "malformed verified_at",
			in: `
groups:
  - {slug: a, name: A, verified_by: ileanmjr88, verified_at: "August 2026"}`,
			want: "not YYYY-MM-DD",
		},
		{
			name: "missing group name",
			in: `
groups:
  - {slug: a}`,
			want: "missing name",
		},
		{
			// Strict decoding catches a typo'd key instead of silently
			// ignoring it, which is the difference between a source that is
			// disabled and a source you think is enabled.
			name: "unknown key is a typo, not a comment",
			in: `
groups:
  - {slug: a, name: A, enabeld: true}`,
			want: "enabeld",
		},
		{
			name: "empty file",
			in:   ``,
			want: "empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tc.in))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// Every problem in one pass, so a contributor fixes them all at once rather
// than discovering them one CI run at a time.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	const in = `
groups:
  - {slug: a}
  - {slug: a, name: Duplicate}
  - slug: c
    name: C
    sources:
      - {kind: rss, url: "https://example.org/f", enabled: true}
`
	_, err := Load(strings.NewReader(in))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"missing name", "duplicate slug", "unknown kind"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q:\n%s", want, msg)
		}
	}
}

func TestLoadFileMissing(t *testing.T) {
	if _, err := LoadFile("testdata/does-not-exist.yaml"); err == nil {
		t.Fatal("want error for a missing file, got nil")
	}
}

// A default venue has to name a real one. A typo would silently leave a group
// with no venue at all, which is the state it was added to fix.
func TestLoadRejectsAnUnknownDefaultVenue(t *testing.T) {
	_, err := Load(strings.NewReader(`
venues:
  - {slug: improving-houston, name: Improving Houston}
groups:
  - slug: a
    name: A
    venue: improving-huston
`))
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "improving-huston") || !strings.Contains(err.Error(), "not in the venues list") {
		t.Errorf("err = %v, want it to name the typo", err)
	}
}

func TestLoadAcceptsAKnownDefaultVenue(t *testing.T) {
	reg, err := Load(strings.NewReader(`
venues:
  - {slug: improving-houston, name: Improving Houston}
groups:
  - slug: a
    name: A
    venue: improving-houston
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reg.Groups[0].VenueSlug != "improving-houston" {
		t.Errorf("VenueSlug = %q", reg.Groups[0].VenueSlug)
	}
}
