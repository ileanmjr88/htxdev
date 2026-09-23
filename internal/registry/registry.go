// Package registry loads the curated list of groups, venues and feeds from
// data/: one file per group in data/groups and one per venue in data/venues,
// each named for its slug. Those files are edited by pull request, often by
// people who do not run the code, so this loader is the CI gate: it validates
// everything it can and reports every problem at once rather than the first
// one.
package registry

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// Registry is the parsed, validated contents of the registry directory.
type Registry struct {
	Groups  []core.Group
	Venues  []core.Venue
	Sources []core.Source // flattened out of the groups that declare them
}

// wire types mirror the YAML exactly and stay unexported, the same rule the
// feed decoders follow. Nothing YAML-shaped escapes this package.
//
// Neither carries a slug. The filename is the slug, so there is one place to
// write it and nothing for a copied file to get out of step with.
type wireVenue struct {
	Name    string   `yaml:"name"`
	Aliases []string `yaml:"aliases"`
	Address string   `yaml:"address"`
	City    string   `yaml:"city"`
	State   string   `yaml:"state"`
	Zip     string   `yaml:"zip"`
	URL     string   `yaml:"url"`
}

type wireGroup struct {
	Name     string   `yaml:"name"`
	URL      string   `yaml:"url"`
	Category string   `yaml:"category"`
	Aliases  []string `yaml:"aliases"`
	// Where the group meets when its feed does not say. A venue slug, which is
	// a filename in venues/.
	Venue string `yaml:"venue"`
	// Blank in the file until a human verifies the source. Kept as a string
	// rather than time.Time so an empty value is not a parse error.
	VerifiedBy string       `yaml:"verified_by"`
	VerifiedAt string       `yaml:"verified_at"`
	Sources    []wireSource `yaml:"sources"`
}

type wireSource struct {
	Kind     string `yaml:"kind"`
	URL      string `yaml:"url"`
	Priority int    `yaml:"priority"`
	Enabled  bool   `yaml:"enabled"`
}

const (
	venuesDir = "venues"
	groupsDir = "groups"
)

// slugPattern is what a filename has to be. Lowercase so that two files
// differing only in case cannot both exist on Linux and collide on macOS, and
// no leading, trailing or doubled hyphen because every slug already in use
// looks like this and a typo is likelier than a need.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// LoadDir reads and validates the registry rooted at dir.
func LoadDir(dir string) (*Registry, error) {
	reg, err := Load(os.DirFS(dir))
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", dir, err)
	}
	return reg, nil
}

// Load parses and validates a registry from fsys. Split from LoadDir so tests
// can hand it an fstest.MapFS and stay off the filesystem.
//
// Files load in filename order, which fs.ReadDir guarantees. Order used to be
// whatever the one big file said, and the one place it mattered, which group
// wins a name two of them claim, is now a validation error instead.
func Load(fsys fs.FS) (*Registry, error) {
	reg := &Registry{}
	var problems []string

	venues, ps := decodeDir[wireVenue](fsys, venuesDir)
	problems = append(problems, ps...)

	seenVenue := map[string]bool{}
	venueNames := map[string]string{} // folded name -> the venue slug that claimed it
	for _, v := range venues {
		wv, where := v.wire, v.file
		seenVenue[v.slug] = true

		if wv.Name == "" {
			problems = append(problems, where+": missing name")
		}
		problems = append(problems, claimNames(venueNames, v.slug, where, wv.Name, wv.Aliases)...)
		reg.Venues = append(reg.Venues, core.Venue{
			Slug:    v.slug,
			Name:    wv.Name,
			Aliases: wv.Aliases,
			Address: wv.Address,
			City:    wv.City,
			State:   wv.State,
			Zip:     wv.Zip,
			URL:     wv.URL,
		})
	}

	groups, ps := decodeDir[wireGroup](fsys, groupsDir)
	problems = append(problems, ps...)
	if len(groups) == 0 && len(ps) == 0 {
		problems = append(problems, groupsDir+"/: no groups")
	}

	groupNames := map[string]string{}
	seenFeed := map[string]string{} // feed URL -> the group slug that claimed it

	for _, g := range groups {
		wg, where := g.wire, g.file

		if wg.Name == "" {
			problems = append(problems, where+": missing name")
		}

		// A name is how another feed's organizer resolves to a group, so two
		// groups answering to one name would split that group's events by
		// whichever file happened to sort first. Nobody editing one file can
		// see the other, which is why it is checked here and not left to
		// review.
		problems = append(problems, claimNames(groupNames, g.slug, where, wg.Name, wg.Aliases)...)

		var verifiedAt time.Time
		if wg.VerifiedAt != "" {
			t, err := time.Parse(time.DateOnly, wg.VerifiedAt)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: verified_at %q is not YYYY-MM-DD", where, wg.VerifiedAt))
			} else {
				verifiedAt = t
			}
		}
		// verified_by without verified_at is a half-finished verification and
		// leaves no record of when it happened.
		if wg.VerifiedBy != "" && wg.VerifiedAt == "" {
			problems = append(problems, where+": verified_by is set but verified_at is blank")
		}

		// A default venue has to name a real one. A typo here would silently
		// give a group no venue at all, which is exactly the state it was
		// added to fix.
		if wg.Venue != "" && !seenVenue[wg.Venue] {
			problems = append(problems, fmt.Sprintf("%s: venue %q has no file in %s/", where, wg.Venue, venuesDir))
		}

		reg.Groups = append(reg.Groups, core.Group{
			Slug:       g.slug,
			Name:       wg.Name,
			URL:        wg.URL,
			Category:   wg.Category,
			Aliases:    wg.Aliases,
			VenueSlug:  wg.Venue,
			VerifiedBy: wg.VerifiedBy,
			VerifiedAt: verifiedAt,
			// The file has no `active` key. Presence in the registry is what
			// makes a group active; deleting its file is how you deactivate one.
			Active: true,
		})

		for j, ws := range wg.Sources {
			swhere := fmt.Sprintf("%s source %d", where, j)

			switch core.SourceKind(ws.Kind) {
			case core.KindTribe, core.KindICS, core.KindHTML, core.KindBevy:
			default:
				problems = append(problems, fmt.Sprintf("%s: unknown kind %q, want one of %q, %q, %q, %q",
					swhere, ws.Kind, core.KindTribe, core.KindICS, core.KindHTML, core.KindBevy))
				continue
			}

			if ws.URL == "" {
				problems = append(problems, swhere+": missing url")
				continue
			}
			u, err := url.Parse(ws.URL)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				problems = append(problems, fmt.Sprintf("%s: url %q must be an absolute https URL", swhere, ws.URL))
				continue
			}
			if owner, dup := seenFeed[ws.URL]; dup {
				problems = append(problems, fmt.Sprintf("%s: url already claimed by group %s; it would be ingested twice",
					swhere, owner))
				continue
			}
			seenFeed[ws.URL] = g.slug

			reg.Sources = append(reg.Sources, core.Source{
				GroupSlug: g.slug,
				Kind:      core.SourceKind(ws.Kind),
				URL:       ws.URL,
				Priority:  ws.Priority,
				Enabled:   ws.Enabled,
			})
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("%d problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	return reg, nil
}

// entry is one decoded registry file.
type entry[T any] struct {
	slug string
	file string // "groups/pyhou.yaml", which is what a contributor needs to see
	wire T
}

// decodeDir strictly decodes every file in dir. A file that fails is reported
// and left out, so one broken file does not hide the problems in the rest.
func decodeDir[T any](fsys fs.FS, dir string) ([]entry[T], []string) {
	des, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("%s/: %v", dir, err)}
	}

	var (
		out      []entry[T]
		problems []string
	)
	for _, de := range des {
		file := path.Join(dir, de.Name())
		// Everything in these directories is a registry entry. A README or a
		// .yml would otherwise be skipped in silence, and a group whose file
		// is skipped is a group that quietly stopped syncing.
		slug, ok := strings.CutSuffix(de.Name(), ".yaml")
		if de.IsDir() || !ok {
			problems = append(problems, file+": want only <slug>.yaml files here")
			continue
		}
		if !slugPattern.MatchString(slug) {
			problems = append(problems, fmt.Sprintf("%s: %q is not a slug; use lowercase letters, digits and single hyphens", file, slug))
			continue
		}

		f, err := fsys.Open(file)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", file, err))
			continue
		}
		var w T
		err = yaml.NewDecoder(f, yaml.Strict()).Decode(&w)
		_ = f.Close() // read-only; a close error tells us nothing
		if err != nil {
			if errors.Is(err, io.EOF) {
				problems = append(problems, file+": file is empty")
			} else {
				problems = append(problems, fmt.Sprintf("%s: %v", file, err))
			}
			continue
		}
		out = append(out, entry[T]{slug: slug, file: file, wire: w})
	}
	return out, problems
}

// claimNames records every name slug answers to, folded the way normalize
// matches them, and reports any another file already took.
func claimNames(claimed map[string]string, slug, where, name string, aliases []string) []string {
	var problems []string
	for _, n := range append([]string{slug, name}, aliases...) {
		key := core.FoldName(n)
		if key == "" {
			continue
		}
		if owner, taken := claimed[key]; taken && owner != slug {
			problems = append(problems, fmt.Sprintf("%s: name %q is already claimed by %s", where, n, owner))
			continue
		}
		claimed[key] = slug
	}
	return problems
}

// EnabledSources returns the feeds a sync run should fetch.
func (r *Registry) EnabledSources() []core.Source {
	var out []core.Source
	for _, s := range r.Sources {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

// Group looks up a group by slug.
func (r *Registry) Group(slug string) (core.Group, bool) {
	for _, g := range r.Groups {
		if g.Slug == slug {
			return g, true
		}
	}
	return core.Group{}, false
}

// IsVerified reports whether a source's owning group has been verified. Events
// from an unverified source stay pending and are never published, which is the
// control against a feed carrying something its publisher did not mean to share.
func (r *Registry) IsVerified(s core.Source) bool {
	g, ok := r.Group(s.GroupSlug)
	return ok && g.VerifiedBy != ""
}
