package fetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	//Deliberately not load-bearing. Events that skip below this line are
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

// Fetch retrieves every source concurrently, at most maxConcurrent at a time,
// and returns one Result per source in input order.
//
// It returns no error, deliberately. A source that fails records that failure
// in its own Result.Err and its neighbours are unaffected. That is also why the
// pool is a hand-rolled semaphore rather than errgroup: errgroup.WithContext
// cancels every sibling goroutine the moment one returns an error, which is
// exactly backwards here, where one dead feed must not blank the site.
//
// Results are assigned by index rather than appended, so no mutex is needed.
// Each goroutine owns one element, distinct elements are distinct memory, and
// wg.Wait is the happens-before edge that publishes those writes to the caller.
// Indexing is also what makes the returned order deterministic.
func Fetch(ctx context.Context, sources []core.Source) []Result {
	now := time.Now().UTC()
	results := make([]Result, len(sources))
	sem := make(chan struct{}, maxConcurrent)

	var wg sync.WaitGroup
	for i := range len(sources) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = get(ctx, sources[i], now)
		}()
	}
	wg.Wait()
	return results
}

func get(ctx context.Context, s core.Source, now time.Time) Result {
	switch s.Kind {
	case core.KindTribe:
		return getTribe(ctx, s, now)
	case core.KindICS:
		return Result{Source: s, Err: fmt.Errorf("kind %q: no decoder yet", s.Kind)}
	default:
		return Result{Source: s, Err: fmt.Errorf("unknown kind %q", s.Kind)}
	}
}

// getTribe fetches one tribe source across the rolling window and returns
// everything it saw as a single Result. Pagination is internal: callers get one
// Result per source, never one per page.
//
// It walks the feed's own next_rest_url rather than incrementing a page counter,
// so the server owns the definition of "next" and an empty NextURL is an
// explicit end rather than something inferred from a short page. Inferring it
// would mean guessing, and a wrong guess silently drops events that normalize
// then reads as cancellations.
func getTribe(ctx context.Context, s core.Source, now time.Time) Result {
	parsedURL, err := url.Parse(s.URL)
	if err != nil {
		return Result{Source: s, Err: fmt.Errorf("parse source url %q: %w", s.URL, err)}
	}
	windowStartDate := now.AddDate(0, 0, -1*lookbackDays)
	windowEndDate := now.AddDate(0, 0, fetchWindowDays)

	params := url.Values{}
	params.Set("start_date", windowStartDate.Format("2006-01-02 15:04:05"))
	params.Set("end_date", windowEndDate.Format("2006-01-02 15:04:05"))
	params.Set("per_page", strconv.Itoa(perPage))
	params.Set("status", "publish")
	params.Set("page", "1")

	parsedURL.RawQuery = params.Encode()
	pageURL := parsedURL.String()

	var (
		events  []core.RawEvent
		skipped []error
	)
	// pageURL doubles as the work queue. It is cleared whenever pagination ends
	// for a reason recorded below, so falling out of this loop with one still
	// set can only mean maxPages cut the walk short.
	pages := 0
	for pageURL != "" && pages < maxPages {
		pages++

		page, err := fetchPage(ctx, pageURL)
		if err != nil {
			return Result{Source: s, Err: err}
		}

		// Integer math rather than floats: fail when skips exceed one event in
		// skipTolerance. Pages under minPageForRatio are too small for a ratio
		// to mean anything, so they tolerate any skip.
		pageTotal := len(page.Events) + len(page.Skipped)
		if pageTotal >= minPageForRatio && len(page.Skipped)*skipTolerance > pageTotal {
			return Result{Source: s, Err: fmt.Errorf(
				"%s: %d of %d events unusable", pageURL, len(page.Skipped), pageTotal)}
		}

		events = append(events, page.Events...)
		skipped = append(skipped, page.Skipped...)

		// Stop by default; only a next page that clears the checks below puts
		// work back on the queue.
		pageURL = ""
		if page.NextURL == "" {
			continue
		}

		next, err := url.Parse(page.NextURL)
		if err != nil {
			skipped = append(skipped, fmt.Errorf("parse next_rest_url %q: %w", page.NextURL, err))
			continue
		}

		// Same origin only. next_rest_url is a string a third party controls,
		// so following it unchecked lets a compromised or simply misconfigured
		// feed aim this client wherever it likes. Host is compared
		// case-insensitively because host names are; Scheme is not, because
		// url.Parse has already lowercased it.
		//
		// This closes the door only part way: the client's default
		// CheckRedirect still follows up to ten hops, off-host included, so a
		// same-origin URL can still land elsewhere. That belongs in the client,
		// not here.
		if !strings.EqualFold(next.Host, parsedURL.Host) || next.Scheme != parsedURL.Scheme {
			skipped = append(skipped, fmt.Errorf("declining next_rest_url %q: not on %s://%s",
				page.NextURL, parsedURL.Scheme, parsedURL.Host))
			continue
		}

		pageURL = next.String()
	}

	// Truncation is not an error, it is an incompleteness. Events is missing
	// entries that exist upstream, which downstream cannot tell apart from a
	// cancellation, and a non-empty Skipped is what defers cancellation for the
	// cycle. Returning Err instead would throw away every page that did parse.
	if pageURL != "" {
		skipped = append(skipped, fmt.Errorf("stopped after %d pages with more to fetch", maxPages))
	}

	// Indexed rather than ranged: range yields a copy of each RawEvent, so
	// assigning through the loop variable would compile, run, and change
	// nothing.
	for i := range events {
		events[i].SourceKey = s.URL
	}

	return Result{
		Source:    s,
		Events:    events,
		Skipped:   skipped,
		FetchedAt: now,
		WindowEnd: windowEndDate,
	}
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
	// Discarded explicitly, matching LoadFile in internal/registry: this body
	// is read to completion or abandoned on an error that has already been
	// returned, and a close failure on a response body tells the caller
	// nothing it can act on.
	defer func() { _ = resp.Body.Close() }()

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
