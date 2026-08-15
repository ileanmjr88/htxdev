package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
	"github.com/ileanmjr88/htxdev/internal/source"
)

const (
	maxConcurrent  = 4        // other people's servers, twice a day
	maxBodyBytes   = 10 << 20 // 10MB; Ion's is ~1MB
	maxPages       = 20
	requestTimeout = 10 * time.Second
	userAgent      = "htxdev/0.1 (+https://github.com/ileanmjr88/htxdev)"

	// Sixty days of Ion is roughly 48 events, so 100 keeps the common case at
	// a single request with room to grow. At the API's default of 50 it would
	// sit two events under the page size and start paginating the first time
	// Ion adds a series, silently and with no test covering it.
	perPage = 100

	minPageForRatio = 4 // below this, tolerate any skip

	// A page fails its source when more than one event in skipTolerance is
	// unusable. A genuine format change (a date layout or the venue shape
	// moving under us) takes out nearly every event on the page, while one odd
	// entry takes out one, so any threshold in this range separates them.
	//
	// Deliberately not load-bearing. Events that skip below this line are
	// still missing from Events, which downstream cannot distinguish from a
	// cancellation. That is handled by deferring cancellation whenever Skipped
	// is non-empty, not by tuning this number.
	skipTolerance = 3
)

// The window Ion is asked for, in calendar days.
//
// Sixty, measured against testdata/ion-tribe.json: a two-year ask returns 80
// events, but everything past mid-September is recurring series (Cup of Joey
// weekly, NASA Office Hours weekly, ENRG HTX and HLUG monthly). The long tail
// is recurrence, not programming, and Houston groups mostly schedule a month
// ahead. Sixty is wide enough to watch a reschedule move and narrow enough not
// to hoard two years of placeholder instances.
//
// Days rather than a time.Duration because these are calendar spans, not
// timeouts: 60*24*time.Hour drifts by an hour across a DST boundary and
// Houston observes DST. Apply them with t.AddDate(0, 0, n).
const (
	fetchWindowDays = 60

	//Start slightly in the past, so an event that began yesterday and runs
	//through today does not drop out of the window mid-event. Also absorbs
	// whether Ion's start_date query filter is site-local or UTC, which its
	// utc_start_date response field does not settle.
	lookbackDays = 2
)

type Result struct {
	Source  core.Source
	Events  []core.RawEvent
	Skipped []error // per-event failures; the source still succeeded
	Err     error   // source-level failure; Events is empty when set

	// FetchedAt and WindowEnd record what this run actually asked for, rather
	// than leaving normalize to recompute it from the same constants and hope
	// the two agree. Absence from Events only means "cancelled" for events
	// starting well inside WindowEnd; close to the edge it is more likely a
	// reschedule that moved past the horizon, and normalize cannot tell those
	// apart without knowing where the horizon was.
	FetchedAt time.Time
	WindowEnd time.Time
}

func get(ctx context.Context, s core.Source) Result {
	switch s.Kind {
	case core.KindTribe:
		return getTribe(ctx, s)
	case core.KindICS:
		return Result{Source: s, Err: fmt.Errorf("kind %q: no decoder yet", s.Kind)}
	default:
		return Result{Source: s, Err: fmt.Errorf("unknown kind %q", s.Kind)}
	}
}

// TODO: scaffolding so the package compiles and fetchPage can be tested.
// Replace with the real thing: build the window query onto s.URL, loop to
// maxPages calling fetchPage, accumulate Events and Skipped, apply
// skipTolerance per page, validate NextURL against s.URL's host before
// following it, then stamp SourceKey, FetchedAt and WindowEnd.
func getTribe(ctx context.Context, s core.Source) Result {
	return Result{Source: s, Err: errors.New("getTribe: not implemented")}
}

// client is shared by every request this package makes. Its nil Transport
// means http.DefaultTransport, so connections pool across sources rather than
// being rebuilt per page.
//
// Deliberately no Timeout field: the per-request context already owns
// cancellation, and two timeout mechanisms means a later debugging session
// spent working out which one fired. CheckRedirect is where a same-host
// redirect policy would go if one is ever wanted; the default silently follows
// up to ten hops, including off-host.
var client = &http.Client{}

// fetchPage performs one GET and decodes the result. It knows nothing about
// sources, windows or pagination: hand it a URL, get back one page or an
// error.
//
// The decode happens here rather than in the caller because this function owns
// the request deadline. Returning resp.Body upward would let the deferred
// cancel() fire while the caller was still reading, surfacing as a
// context.Canceled part-way through a JSON document and looking for all the
// world like a flaky network.
func fetchPage(ctx context.Context, rawURL string) (source.TribePage, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return source.TribePage{}, fmt.Errorf("new request %s: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return source.TribePage{}, fmt.Errorf("get %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	// Strictly 200, not the 2xx range. A 204 carries no body and a 206 carries
	// a fragment; both would fail later as an opaque decode error instead of
	// as the thing that actually happened.
	if resp.StatusCode != http.StatusOK {
		return source.TribePage{}, fmt.Errorf("get %s: unexpected status %s", rawURL, resp.Status)
	}

	// One byte past the ceiling, so an oversized body stays distinguishable
	// from a complete one. Reading exactly maxBodyBytes would truncate in
	// silence and surface as "unexpected EOF" from the decoder, sending
	// whoever reads that log into ParseTribe to debug a problem about size.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return source.TribePage{}, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if len(body) > maxBodyBytes {
		return source.TribePage{}, fmt.Errorf("read %s: body exceeds %d bytes", rawURL, maxBodyBytes)
	}

	page, err := source.ParseTribe(bytes.NewReader(body))
	if err != nil {
		return source.TribePage{}, fmt.Errorf("parse %s: %w", rawURL, err)
	}
	return page, nil
}
