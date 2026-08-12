// Package source decodes event feeds into core types. Each supported format
// gets one decoder; all of them produce []core.RawEvent and nothing else.
// Wire types here mirror a provider's payload exactly and stay unexported, so
// provider-specific fields cannot leak into the domain.
package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// tribeTimeLayout matches Ion's utc_start_date / utc_end_date: no zone marker,
// but the value is UTC. Parsed with time.Parse rather than ParseInLocation for
// exactly that reason; using the event's America/Chicago zone would shift it.
const tribeTimeLayout = "2006-01-02 15:04:05"

type tribeEnvelope struct {
	Total       int               `json:"total"`
	NextRestURL string            `json:"next_rest_url"`
	Events      []json.RawMessage `json:"events"`
}

type tribeEvent struct {
	GlobalID     string           `json:"global_id"`
	Title        string           `json:"title"`
	Description  string           `json:"description"`
	URL          string           `json:"url"`
	Website      string           `json:"website"`
	UTCStartDate string           `json:"utc_start_date"`
	UTCEndDate   string           `json:"utc_end_date"`
	AllDay       bool             `json:"all_day"`
	IsVirtual    bool             `json:"is_virtual"`
	VirtualURL   string           `json:"virtual_url"`
	Organizer    []tribeOrganizer `json:"organizer"`
	Venue        tribeVenues      `json:"venue"`
	Categories   []tribeCategory  `json:"categories"`
}

type tribeOrganizer struct {
	Organizer string `json:"organizer"`
	Website   string `json:"website"`
}

type tribeCategory struct {
	Name string `json:"name"`
}

type tribeVenue struct {
	GlobalID string `json:"global_id"`
	Venue    string `json:"venue"`
	Address  string `json:"address"`
	City     string `json:"city"`
	State    string `json:"stateprovince"`
	Zip      string `json:"zip"`
	URL      string `json:"url"`
}

// tribeVenues absorbs Ion's two shapes for the `venue` field: a single object
// on 39 of 50 events, and a [room, building] array on the other 11. Both
// normalize to a slice so callers never have to care which arrived.
type tribeVenues []tribeVenue

func (vs *tribeVenues) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)

	// Case 1: null, or nothing at all.
	if len(data) == 0 || string(data) == "null" {
		*vs = nil
		return nil
	}

	// Case 2: the array form. 11 of Ion's 50 events.
	if data[0] == '[' {
		return json.Unmarshal(data, (*[]tribeVenue)(vs))
	}

	// Case 3: the single-object form. The other 39.
	if data[0] == '{' {
		var one tribeVenue
		if err := json.Unmarshal(data, &one); err != nil {
			return err
		}
		*vs = tribeVenues{one}
		return nil
	}

	// Case 4: a shape we don't model
	return fmt.Errorf("venue: want object or array, got %s", data)
}

// TribePage is one decoded page of a tribe feed. Skipped holds the per-event
// failures that did not stop the page from being usable; the fetch layer
// decides whether too many of them means the source itself has failed.
type TribePage struct {
	Events  []core.RawEvent
	Skipped []error
	NextURL string // next_rest_url; the fetch layer follows it
	Total   int    // total events across all pages, not just this one
}

// ParseTribe decodes one page of The Events Calendar JSON. It is pure: give it
// a file and it cannot tell the difference from an HTTP body.
//
// A malformed envelope is fatal, since nothing can be salvaged from it. A
// malformed individual event is not: it lands in Skipped and the rest of the
// page still decodes. That is only possible because the envelope holds events
// as json.RawMessage, deferring their decode to this loop.
func ParseTribe(r io.Reader) (TribePage, error) {
	var env tribeEnvelope
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return TribePage{}, fmt.Errorf("decode envelope: %w", err)
	}

	page := TribePage{
		NextURL: env.NextRestURL,
		Total:   env.Total,
	}

	for i, raw := range env.Events {
		var te tribeEvent
		if err := json.Unmarshal(raw, &te); err != nil {
			page.Skipped = append(page.Skipped, fmt.Errorf("event %d: %w", i, err))
			continue
		}
		re, err := toRawEvent(te)
		if err != nil {
			page.Skipped = append(page.Skipped, err)
			continue
		}
		page.Events = append(page.Events, re)
	}

	return page, nil
}

// toRawEvent maps one decoded tribe event onto the domain's RawEvent. It
// derives nothing: no venue splitting, no organizer resolution, no excerpt,
// no fingerprint namespacing. All of that is normalize's job.
func toRawEvent(te tribeEvent) (core.RawEvent, error) {
	// An event with no stable upstream ID cannot get a usable Fingerprint.
	// Every such event would collide on the same value, corrupting dedupe and
	// eventually duplicating events in ICS subscribers' calendars. All 50
	// fixture events have one, so this fires only if Ion changes something.
	if te.GlobalID == "" {
		return core.RawEvent{}, errors.New("event has no global_id")
	}

	start, err := time.Parse(tribeTimeLayout, te.UTCStartDate)
	if err != nil {
		return core.RawEvent{}, fmt.Errorf("event %s: start: %w", te.GlobalID, err)
	}

	// An absent end is tolerated: an event with a start is still displayable,
	// and dropping it would lose real information over a cosmetic gap. An end
	// that is present but unparseable is a different thing, and signals a
	// format change worth failing on.
	var end time.Time
	if te.UTCEndDate != "" {
		end, err = time.Parse(tribeTimeLayout, te.UTCEndDate)
		if err != nil {
			return core.RawEvent{}, fmt.Errorf("event %s: end: %w", te.GlobalID, err)
		}
	}

	re := core.RawEvent{
		UpstreamID: te.GlobalID, // no "tribe:" prefix; namespacing happens at normalize
		Title:      html.UnescapeString(te.Title),
		// Description stays raw. Its entities are part of the markup, and it
		// gets unescaped once later when it is stripped to plain text.
		Description: te.Description,
		Start:       start,
		End:         end,
		AllDay:      te.AllDay,
		URL:         te.URL,
		RegisterURL: te.Website, // `website`, not `url`: the external signup link
		VirtualURL:  te.VirtualURL,
		Virtual:     te.IsVirtual,
	}

	for _, o := range te.Organizer {
		re.Organizers = append(re.Organizers, core.RawOrganizer{
			Name: html.UnescapeString(o.Organizer),
			URL:  o.Website,
		})
	}
	for _, v := range te.Venue {
		re.Venues = append(re.Venues, core.RawVenue{
			UpstreamID: v.GlobalID,
			Name:       html.UnescapeString(v.Venue),
			Address:    v.Address,
			City:       v.City,
			State:      v.State,
			Zip:        v.Zip,
			URL:        v.URL,
		})
	}
	for _, c := range te.Categories {
		re.Categories = append(re.Categories, html.UnescapeString(c.Name))
	}

	return re, nil
}
