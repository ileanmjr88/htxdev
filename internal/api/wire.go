// Package api serves htxdev's events over HTTP, and owns the JSON shape they
// are served in.
//
// It owns that shape for both consumers, not just this one. cmd/htxdev export
// writes the same types to data/events.json, so the static file the site reads
// is the API's response precomputed rather than a second format that has to be
// kept in step with it. There is one contract and one place it is defined.
package api

import (
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// The wire types are separate from core.Event on purpose, and it is the same
// argument as the narrow wire structs in the decoders pointed the other way.
// This JSON is a published contract that a site, an API and whatever somebody
// else builds all read; core.Event is an internal domain type. Marshalling the
// domain type directly would make every field added to it public the moment it
// was added, including whatever the next decoder needs to carry internally.

// Feed is a whole response: what htxdev knows about, when it knew it.
type Feed struct {
	GeneratedAt time.Time `json:"generated_at"`
	// True when unpublished events are included. Set only by a preview export;
	// the server never sets it, because the server cannot serve them.
	Preview bool    `json:"preview,omitempty"`
	Events  []Event `json:"events"`
}

type Event struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Excerpt     string     `json:"excerpt,omitempty"`
	Start       time.Time  `json:"start"`
	End         *time.Time `json:"end,omitempty"`
	AllDay      bool       `json:"all_day,omitempty"`
	Group       Group      `json:"group"`
	Venue       *Venue     `json:"venue,omitempty"`
	Room        string     `json:"room,omitempty"`
	URL         string     `json:"url,omitempty"`
	RegisterURL string     `json:"register_url,omitempty"`
	Categories  []string   `json:"categories,omitempty"`
	Virtual     bool       `json:"virtual,omitempty"`
	// How many feeds carried this event. Two means dedupe did something, and
	// it is the only place that fact is visible outside the database.
	Sources int `json:"sources,omitempty"`
	// Only ever set in a preview.
	Pending bool `json:"pending,omitempty"`
}

type Group struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

type Venue struct {
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	City    string `json:"city,omitempty"`
	State   string `json:"state,omitempty"`
	Zip     string `json:"zip,omitempty"`
	URL     string `json:"url,omitempty"`
}

// NewFeed converts what the store returned into what goes over the wire.
func NewFeed(events []core.Event, groups map[string]core.Group, at time.Time, preview bool) Feed {
	f := Feed{GeneratedAt: at, Preview: preview, Events: make([]Event, 0, len(events))}
	for _, e := range events {
		f.Events = append(f.Events, NewEvent(e, groups[e.GroupSlug]))
	}
	return f
}

func NewEvent(e core.Event, g core.Group) Event {
	out := Event{
		ID:          e.Fingerprint,
		Title:       e.Title,
		Excerpt:     e.Excerpt,
		Start:       e.Start,
		AllDay:      e.AllDay,
		Group:       Group{Slug: e.GroupSlug, Name: g.Name, URL: g.URL},
		Room:        e.Room,
		URL:         e.URL,
		RegisterURL: e.RegisterURL,
		Categories:  e.Categories,
		Virtual:     e.Virtual,
		Sources:     len(e.Sources),
		Pending:     e.Status == "pending",
	}
	// A pointer, so an event with no end time is absent from the JSON rather
	// than present as year 1. A consumer can then treat "no end" as a state.
	if !e.End.IsZero() {
		end := e.End
		out.End = &end
	}
	if e.Venue.Name != "" {
		out.Venue = &Venue{
			Name: e.Venue.Name, Address: e.Venue.Address, City: e.Venue.City,
			State: e.Venue.State, Zip: e.Venue.Zip, URL: e.Venue.URL,
		}
	}
	// A group the registry no longer carries still has events in the database,
	// and a blank name renders as an empty line rather than as a clue.
	if out.Group.Name == "" {
		out.Group.Name = e.GroupSlug
	}
	return out
}
