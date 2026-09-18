// Package registry loads data/sources.yaml, the curated list of groups,
// venues and feeds. That file is edited by pull request, often by people who
// do not run the code, so this loader is the CI gate: it validates everything
// it can and reports every problem at once rather than the first one.
package registry

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// Registry is the parsed, validated contents of sources.yaml.
type Registry struct {
	Groups  []core.Group
	Venues  []core.Venue
	Sources []core.Source // flattened out of the groups that declare them
}

// wire types mirror the YAML exactly and stay unexported, the same rule the
// feed decoders follow. Nothing YAML-shaped escapes this package.
type wireFile struct {
	Venues []wireVenue `yaml:"venues"`
	Groups []wireGroup `yaml:"groups"`
}

type wireVenue struct {
	Slug    string   `yaml:"slug"`
	Name    string   `yaml:"name"`
	Aliases []string `yaml:"aliases"`
	Address string   `yaml:"address"`
	City    string   `yaml:"city"`
	State   string   `yaml:"state"`
	Zip     string   `yaml:"zip"`
	URL     string   `yaml:"url"`
}

type wireGroup struct {
	Slug     string   `yaml:"slug"`
	Name     string   `yaml:"name"`
	URL      string   `yaml:"url"`
	Category string   `yaml:"category"`
	Aliases  []string `yaml:"aliases"`
	// Where the group meets when its feed does not say. A venue slug from the
	// venues list above.
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

// LoadFile reads and validates sources.yaml at path.
func LoadFile(path string) (*Registry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only; a close error tells us nothing

	reg, err := Load(f)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", path, err)
	}
	return reg, nil
}

// Load parses and validates a registry from r. Split from LoadFile so tests
// can hand it a string and stay off the filesystem.
func Load(r io.Reader) (*Registry, error) {
	var wf wireFile
	if err := yaml.NewDecoder(r, yaml.Strict()).Decode(&wf); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file is empty")
		}
		return nil, err
	}

	reg := &Registry{}
	var problems []string

	seenVenue := map[string]bool{}
	for i, wv := range wf.Venues {
		where := fmt.Sprintf("venue %d", i)
		if wv.Slug == "" {
			problems = append(problems, where+": missing slug")
			continue
		}
		where = "venue " + wv.Slug
		if seenVenue[wv.Slug] {
			problems = append(problems, where+": duplicate slug")
			continue
		}
		seenVenue[wv.Slug] = true

		if wv.Name == "" {
			problems = append(problems, where+": missing name")
		}
		reg.Venues = append(reg.Venues, core.Venue{
			Slug:    wv.Slug,
			Name:    wv.Name,
			Aliases: wv.Aliases,
			Address: wv.Address,
			City:    wv.City,
			State:   wv.State,
			Zip:     wv.Zip,
			URL:     wv.URL,
		})
	}

	seenGroup := map[string]bool{}
	seenFeed := map[string]string{} // feed URL -> the group slug that claimed it

	for i, wg := range wf.Groups {
		where := fmt.Sprintf("group %d", i)
		if wg.Slug == "" {
			problems = append(problems, where+": missing slug")
			continue
		}
		where = "group " + wg.Slug
		if seenGroup[wg.Slug] {
			problems = append(problems, where+": duplicate slug")
			continue
		}
		seenGroup[wg.Slug] = true

		if wg.Name == "" {
			problems = append(problems, where+": missing name")
		}

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

		// A default venue has to name one of the venues above. A typo here
		// would silently give a group no venue at all, which is exactly the
		// state it was added to fix.
		if wg.Venue != "" && !seenVenue[wg.Venue] {
			problems = append(problems, fmt.Sprintf("%s: venue %q is not in the venues list", where, wg.Venue))
		}

		reg.Groups = append(reg.Groups, core.Group{
			Slug:       wg.Slug,
			Name:       wg.Name,
			URL:        wg.URL,
			Category:   wg.Category,
			Aliases:    wg.Aliases,
			VenueSlug:  wg.Venue,
			VerifiedBy: wg.VerifiedBy,
			VerifiedAt: verifiedAt,
			// The file has no `active` key. Presence in the registry is what
			// makes a group active; removing it is how you deactivate one.
			Active: true,
		})

		for j, ws := range wg.Sources {
			swhere := fmt.Sprintf("%s source %d", where, j)

			switch core.SourceKind(ws.Kind) {
			case core.KindTribe, core.KindICS, core.KindHTML:
			default:
				problems = append(problems, fmt.Sprintf("%s: unknown kind %q, want one of %q, %q, %q",
					swhere, ws.Kind, core.KindTribe, core.KindICS, core.KindHTML))
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
			seenFeed[ws.URL] = wg.Slug

			reg.Sources = append(reg.Sources, core.Source{
				GroupSlug: wg.Slug,
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
