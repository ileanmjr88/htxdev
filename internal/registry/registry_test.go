package registry

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// The committed registry must always be loadable. This is the test that turns
// the loader into a CI gate: a pull request that breaks a group's file fails
// here rather than at the next sync.
func TestRealRegistryIsValid(t *testing.T) {
	reg, err := LoadDir("../../data")
	if err != nil {
		t.Fatalf("committed registry does not load:\n%v", err)
	}

	if len(reg.Groups) != 13 {
		t.Errorf("got %d groups, want 13", len(reg.Groups))
	}
	if len(reg.Venues) != 6 {
		t.Errorf("got %d venues, want 6", len(reg.Venues))
	}
	if len(reg.Sources) != 13 {
		t.Errorf("got %d sources, want 13", len(reg.Sources))
	}
	if got := len(reg.EnabledSources()); got != 13 {
		t.Errorf("got %d enabled sources, want 13", got)
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

	// Snowflake's Houston chapter is the source kind: bevy exists for, added
	// 2026-09-23. Bevy publishes no calendar, so if this ever changes kind it
	// is Snowflake having shipped one.
	var snowflake []core.Source
	for _, s := range reg.Sources {
		if s.GroupSlug == "snowflake-houston" {
			snowflake = append(snowflake, s)
		}
	}
	if len(snowflake) != 1 || snowflake[0].Kind != core.KindBevy {
		t.Errorf("snowflake-houston sources = %+v, want one of kind %q", snowflake, core.KindBevy)
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

	// HLUG was blocked here until 2026-09-18, on the grounds that its calendar
	// carries personal appointments beside real meetings. That was the right
	// assertion and it did its job: verifying the group meant reading all 110
	// events, which turned up a second private entry nobody knew about.
	//
	// What replaces it is the thing that actually has to stay true. Every
	// private entry found on that calendar is in data/rejects.yaml, and the
	// group being verified means the feed publishes, so an empty reject list
	// against a verified HLUG is a regression rather than a tidy-up.
	if verified["houston-linux-user-group"] {
		rej, err := LoadRejectsFile("../../data/rejects.yaml")
		if err != nil {
			t.Fatalf("load rejects: %v", err)
		}
		for _, fp := range []string{
			"ics:6kqj0pb66kqmab9pc8s66b9k6gpm4b9p71ij2bb269hmad1n6ks3gdj368@google.com",
			"ics:6li64dhj6dgm4b9m6or30b9k6ks3ibb271hj6b9gcpgj2dj5c9gmcp9n68@google.com",
		} {
			if _, ok := rej.Rejected(fp); !ok {
				t.Errorf("houston-linux-user-group is verified but %s is no longer rejected; "+
					"that is a personal appointment and removing it publishes somebody's private life", fp)
			}
		}
	}
}

// registry builds an in-memory registry directory. Keys are paths relative to
// its root, so "groups/a.yaml" is group a. Both directories exist even when a
// test puts nothing in one, because an empty venues/ is normal and a missing
// one is a separate test.
func registry(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{
		"groups": {Mode: fs.ModeDir},
		"venues": {Mode: fs.ModeDir},
	}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func TestLoadFlattensSourcesOntoGroups(t *testing.T) {
	reg, err := Load(registry(map[string]string{"groups/alpha.yaml": `
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
`}))
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
	// A group with no `active` key is active. Deleting its file is how you
	// deactivate one.
	if !reg.Groups[0].Active {
		t.Error("group should default to Active")
	}
}

// The filename is the slug. There is no slug key to disagree with it.
func TestLoadTakesSlugsFromFilenames(t *testing.T) {
	reg, err := Load(registry(map[string]string{
		"groups/beta.yaml":     "name: Beta\n",
		"groups/alpha.yaml":    "name: Alpha\nvenue: the-hall\n",
		"venues/the-hall.yaml": "name: The Hall\n",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Filename order, which fs.ReadDir guarantees, so a run is reproducible.
	if len(reg.Groups) != 2 || reg.Groups[0].Slug != "alpha" || reg.Groups[1].Slug != "beta" {
		t.Errorf("groups = %+v, want alpha then beta", reg.Groups)
	}
	if len(reg.Venues) != 1 || reg.Venues[0].Slug != "the-hall" {
		t.Errorf("venues = %+v, want the-hall", reg.Venues)
	}
	if reg.Groups[0].VenueSlug != "the-hall" {
		t.Errorf("VenueSlug = %q, want the-hall", reg.Groups[0].VenueSlug)
	}
}

// Renamed 2026-09-17. This is about Load() rejecting an invalid registry, and
// it collided with the test for LoadRejects(), which reads the reject list.
// Two different senses of the word, one of which is now a type.
func TestLoadRejectsInvalidRegistries(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string // substring the error must mention
	}{
		{
			name:  "unknown source kind",
			files: map[string]string{"groups/a.yaml": "name: A\nsources:\n  - {kind: rss, url: \"https://example.org/f\", enabled: true}\n"},
			want:  "unknown kind",
		},
		{
			name:  "non-https feed url",
			files: map[string]string{"groups/a.yaml": "name: A\nsources:\n  - {kind: ics, url: \"http://example.org/f.ics\", enabled: true}\n"},
			want:  "absolute https URL",
		},
		{
			name:  "relative feed url",
			files: map[string]string{"groups/a.yaml": "name: A\nsources:\n  - {kind: ics, url: \"/feed.ics\", enabled: true}\n"},
			want:  "absolute https URL",
		},
		{
			// Two groups pointing at one feed would ingest every event twice
			// and attribute it to whichever group happened to win.
			name: "same feed claimed twice",
			files: map[string]string{
				"groups/a.yaml": "name: A\nsources:\n  - {kind: ics, url: \"https://example.org/f.ics\", enabled: true}\n",
				"groups/b.yaml": "name: B\nsources:\n  - {kind: ics, url: \"https://example.org/f.ics\", enabled: true}\n",
			},
			want: "already claimed by group a",
		},
		{
			// Normalize resolves a feed's organizer by name. Two groups
			// answering to one would split a group's events by filename order,
			// and neither contributor can see the other's file.
			name: "alias claimed by two groups",
			files: map[string]string{
				"groups/a.yaml": "name: A\naliases: [PyLadies]\n",
				"groups/b.yaml": "name: B\naliases: [pyladies]\n",
			},
			want: `name "pyladies" is already claimed by a`,
		},
		{
			// Folded the way normalize folds, so a curly apostrophe does not
			// get two groups past the check.
			name: "name collision after folding",
			files: map[string]string{
				"groups/a.yaml": "name: \"Houston Linux User’s Group\"\n",
				"groups/b.yaml": "name: B\naliases: [\"houston linux user's group\"]\n",
			},
			want: "already claimed by a",
		},
		{
			name: "venue alias claimed by two venues",
			files: map[string]string{
				"venues/ion.yaml":   "name: Ion\naliases: [The Ion]\n",
				"venues/other.yaml": "name: Other\naliases: [the ion]\n",
				"groups/a.yaml":     "name: A\n",
			},
			want: "venues/other.yaml",
		},
		{
			name:  "verified_by without verified_at",
			files: map[string]string{"groups/a.yaml": "{name: A, verified_by: ileanmjr88}"},
			want:  "verified_at is blank",
		},
		{
			name:  "malformed verified_at",
			files: map[string]string{"groups/a.yaml": `{name: A, verified_by: ileanmjr88, verified_at: "August 2026"}`},
			want:  "not YYYY-MM-DD",
		},
		{
			name:  "missing group name",
			files: map[string]string{"groups/a.yaml": "url: https://example.org\n"},
			want:  "missing name",
		},
		{
			// Strict decoding catches a typo'd key instead of silently
			// ignoring it, which is the difference between a source that is
			// disabled and a source you think is enabled.
			name:  "unknown key is a typo, not a comment",
			files: map[string]string{"groups/a.yaml": "{name: A, enabeld: true}"},
			want:  "enabeld",
		},
		{
			// The old single-file shape, pasted into a group file. Strict
			// decoding rejects it rather than loading a nameless group.
			name:  "slug key is not a field",
			files: map[string]string{"groups/a.yaml": "{slug: a, name: A}"},
			want:  "slug",
		},
		{
			name:  "empty file",
			files: map[string]string{"groups/a.yaml": ""},
			want:  "groups/a.yaml: file is empty",
		},
		{
			name:  "no groups at all",
			files: map[string]string{},
			want:  "no groups",
		},
		{
			// Skipped in silence, a .yml would be a group that quietly
			// stopped syncing.
			name:  "wrong extension",
			files: map[string]string{"groups/a.yml": "name: A\n", "groups/b.yaml": "name: B\n"},
			want:  "groups/a.yml: want only <slug>.yaml files",
		},
		{
			name:  "filename is not a slug",
			files: map[string]string{"groups/Houston_PyLadies.yaml": "name: A\n"},
			want:  "is not a slug",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(registry(tc.files))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// Every problem in one pass, across files, so a contributor fixes them all at
// once rather than discovering them one CI run at a time. The broken file is
// reported and the others are still checked.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(registry(map[string]string{
		"groups/a.yaml": "url: https://example.org\n",
		"groups/b.yaml": "name: [not, a, string\n",
		"groups/c.yaml": "name: C\nsources:\n  - {kind: rss, url: \"https://example.org/f\", enabled: true}\n",
	}))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"groups/a.yaml: missing name", "groups/b.yaml", "unknown kind"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q:\n%s", want, msg)
		}
	}
}

func TestLoadDirMissing(t *testing.T) {
	if _, err := LoadDir("testdata/does-not-exist"); err == nil {
		t.Fatal("want error for a missing directory, got nil")
	}
}

func TestLoadRequiresBothDirectories(t *testing.T) {
	fsys := fstest.MapFS{"groups/a.yaml": {Data: []byte("name: A\n")}}
	_, err := Load(fsys)
	if err == nil || !strings.Contains(err.Error(), "venues/") {
		t.Fatalf("err = %v, want it to name the missing venues/", err)
	}
}

// A default venue has to name a real one. A typo would silently leave a group
// with no venue at all, which is the state it was added to fix.
func TestLoadRejectsAnUnknownDefaultVenue(t *testing.T) {
	_, err := Load(registry(map[string]string{
		"venues/improving-houston.yaml": "name: Improving Houston\n",
		"groups/a.yaml":                 "name: A\nvenue: improving-huston\n",
	}))
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "improving-huston") || !strings.Contains(err.Error(), "has no file in venues/") {
		t.Errorf("err = %v, want it to name the typo", err)
	}
}
