package fetch

import (
	"bytes"
	"context"
	"errors"
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

// Fetcher retrieves and decodes one source. Its implementations are the
// per-kind strategies: one request shape, one decoder and one set of format
// quirks each.
//
// D3 said to extract this once a second implementation existed rather than
// guessing at it in Phase 1, and the shape the two of them turned out to share
// is narrower than one designed up front would have been. No Close, no
// separate Validate step, and no error return: a source that fails records
// that on its own Result, for the same reason Fetch returns no error.
//
// It lives in this package and not in internal/source because the consumer is
// what defines the interface it needs. internal/source could not declare this
// type even if it wanted to, since Result belongs here, and the whole property
// that makes that package worth keeping separate is that it cannot reach the
// network.
//
// Worth being straight about what it buys today, given it has one method and
// both implementations sit in this file: it is a name for the extension point
// and a seam for a fake, not decoupling. The table below is the part that pays
// for itself, because adding a kind stops meaning editing a switch.
type Fetcher interface {
	Get(ctx context.Context, s core.Source, now time.Time) Result
}

// fetchers maps a source kind to its implementation. Adding a kind is a line
// here plus a decoder in internal/source.
var fetchers = map[core.SourceKind]Fetcher{
	core.KindTribe: tribeFetcher{},
	core.KindICS:   icsFetcher{},
	core.KindHTML:  hossFetcher{},
	core.KindBevy:  bevyFetcher{delay: bevyCrawlDelay},
}

func get(ctx context.Context, s core.Source, now time.Time) Result {
	f, ok := fetchers[s.Kind]
	if !ok {
		// Unreachable by way of internal/registry, which rejects an unknown
		// kind when the registry loads. Kept because this package does not
		// depend on having been called through the registry, and a Result with
		// Err set is a cheaper answer than a panic.
		return Result{Source: s, Err: fmt.Errorf("unknown kind %q", s.Kind)}
	}
	return f.Get(ctx, s, now)
}

type tribeFetcher struct{}

type icsFetcher struct{}

type hossFetcher struct{}

// bevyFetcher carries its delay as a field rather than reading the constant,
// so a test can build one with no delay without touching package state that
// a parallel test might be reading.
type bevyFetcher struct {
	delay time.Duration
}

const (
	// Bevy's robots.txt says Crawl-delay: 2 for every agent it names. Honoured
	// between every request to one chapter, which is the only place this
	// client makes more than one request to a host in quick succession.
	bevyCrawlDelay = 2 * time.Second

	// A ceiling on event pages per chapter, the counterpart of maxPages. The
	// Houston chapter links four, and Bevy caps the past events it lists, so
	// reaching this means something other than a busy chapter.
	maxBevyEvents = 20
)

// window is the span every source in a run is asked for, derived once from the
// run's shared now so that a tribe query string and an ICS filter cannot
// disagree about where the horizon is.
func window(now time.Time) (start, end time.Time) {
	return now.AddDate(0, 0, -lookbackDays), now.AddDate(0, 0, fetchWindowDays)
}

// tooManySkips applies D6's ratio: a decoded unit fails its source when more
// than one record in skipTolerance was unusable. Units below minPageForRatio
// are too small for a ratio to mean anything and tolerate any skip.
//
// Shared by both fetchers, which is the reason it is a function rather than
// two copies of the arithmetic. What "a unit" means differs: for tribe it is
// one page, because that is where a format change shows up; for ICS it is the
// whole feed, because an ICS feed is a single document with nothing to
// paginate.
func tooManySkips(events, skipped int) bool {
	total := events + skipped
	return total >= minPageForRatio && skipped*skipTolerance > total
}

// Get fetches one tribe source across the rolling window and returns
// everything it saw as a single Result. Pagination is internal: callers get one
// Result per source, never one per page.
//
// It walks the feed's own next_rest_url rather than incrementing a page counter,
// so the server owns the definition of "next" and an empty NextURL is an
// explicit end rather than something inferred from a short page. Inferring it
// would mean guessing, and a wrong guess silently drops events that normalize
// then reads as cancellations.
func (tribeFetcher) Get(ctx context.Context, s core.Source, now time.Time) Result {
	parsedURL, err := url.Parse(s.URL)
	if err != nil {
		return Result{Source: s, Err: fmt.Errorf("parse source url %q: %w", s.URL, err)}
	}
	windowStartDate, windowEndDate := window(now)

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

		// Integer math rather than floats; see tooManySkips. Applied per page
		// rather than per source, which is the stricter reading: a format
		// change takes out one page completely, and averaging it across a
		// healthy page would hide it.
		pageTotal := len(page.Events) + len(page.Skipped)
		if tooManySkips(len(page.Events), len(page.Skipped)) {
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

// getBody performs one GET and returns the whole response body.
//
// It returns bytes rather than an io.ReadCloser, which is the point. This
// function owns the request deadline, so handing the body upward would let the
// deferred cancel() fire while the caller was still reading, surfacing as a
// context.Canceled part-way through a document and looking for all the world
// like a flaky network. Reading to completion inside the deadline means the
// caller gets either a whole body or an error, never a live stream on a dead
// context.
//
// Split out of fetchPage when the ICS fetcher arrived and needed the same
// request policy with a different decoder. Everything kind-specific, the
// decode included, stays with the caller.
func getBody(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request %s: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", rawURL, err)
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
		return nil, fmt.Errorf("get %s: unexpected status %s", rawURL, resp.Status)
	}

	// One byte past the ceiling, so an oversized body stays distinguishable
	// from a complete one. Reading exactly maxBodyBytes would truncate in
	// silence and surface as "unexpected EOF" from the decoder, sending
	// whoever reads that log into the decoder to debug a problem about size.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rawURL, err)
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("read %s: body exceeds %d bytes", rawURL, maxBodyBytes)
	}

	return body, nil
}

// fetchPage performs one request and decodes one page of a tribe feed. It
// knows nothing about sources, windows or pagination: hand it a URL, get back
// one page or an error.
func fetchPage(ctx context.Context, rawURL string) (source.TribePage, error) {
	body, err := getBody(ctx, rawURL)
	if err != nil {
		return source.TribePage{}, err
	}
	page, err := source.ParseTribe(bytes.NewReader(body))
	if err != nil {
		return source.TribePage{}, fmt.Errorf("parse %s: %w", rawURL, err)
	}
	return page, nil
}

// fetchICS performs one request and decodes an iCalendar feed.
//
// The window reaches the decoder rather than being applied afterwards because
// an ICS feed is unbounded in both directions and a recurrence rule with no
// UNTIL is infinite. Ion does this filtering server-side from getTribe's
// start_date and end_date query parameters; an ICS server will not, so somebody
// has to, and doing it inside the decode is what keeps an infinite rule from
// having to be materialised first and trimmed second.
func fetchICS(ctx context.Context, rawURL string, from, until time.Time) (source.ICSFeed, error) {
	body, err := getBody(ctx, rawURL)
	if err != nil {
		return source.ICSFeed{}, err
	}
	feed, err := source.ParseICS(bytes.NewReader(body), from, until)
	if err != nil {
		return source.ICSFeed{}, fmt.Errorf("parse %s: %w", rawURL, err)
	}
	return feed, nil
}

// Get fetches one iCalendar source and returns everything inside the window as
// a single Result.
//
// Much shorter than the tribe fetcher, and the difference is where the work
// sits rather than how much there is. There is no pagination because an ICS
// feed is one document, and no query string because the server offers no way
// to ask for less, so the whole calendar arrives and the decoder filters it.
// HLUG sends 110 events to answer a question about 19.
func (icsFetcher) Get(ctx context.Context, s core.Source, now time.Time) Result {
	windowStart, windowEnd := window(now)

	feed, err := fetchICS(ctx, s.URL, windowStart, windowEnd)
	if err != nil {
		return Result{Source: s, Err: err}
	}

	// One caveat this shares with nothing else in the pipeline: a skip can come
	// from an event outside the window, because an event's date has to be
	// parsed before it can be compared to the window, so a malformed DTSTART on
	// something from last year still counts here. That is the behaviour worth
	// having. A date this decoder cannot read is a format change whichever year
	// it is in, and the ratio existing at all is to make a format change loud.
	return finish(s, feed.Events, feed.Skipped, now, windowEnd)
}

// Get fetches HOSS's meetings page.
//
// Structurally identical to the ICS fetcher, because both are one request over
// one document. The only thing that differs is which decoder runs, which is
// exactly what the Fetcher interface is for, and the shared tail is in finish.
//
// This one reads HTML, which is the worst kind of source to depend on. See the
// comment on ParseHOSS for why it is defensible here and why the goal is to
// delete it.
func (hossFetcher) Get(ctx context.Context, s core.Source, now time.Time) Result {
	windowStart, windowEnd := window(now)

	body, err := getBody(ctx, s.URL)
	if err != nil {
		return Result{Source: s, Err: err}
	}
	page, err := source.ParseHOSS(bytes.NewReader(body), windowStart, windowEnd)
	if err != nil {
		return Result{Source: s, Err: fmt.Errorf("parse %s: %w", s.URL, err)}
	}
	return finish(s, page.Events, page.Skipped, now, windowEnd)
}

// finish applies the ratio and the attribution stamp that every non-paginated
// fetcher does identically, and builds the Result.
//
// Shared rather than copied because these are the steps that have to stay in
// agreement across kinds: a source whose events are mostly unusable fails the
// same way whatever format it arrived in, and SourceKey is always the
// registry's URL. The tribe fetcher does not use this, because pagination
// means it applies the ratio per page and can fail part way through a walk.
func finish(s core.Source, events []core.RawEvent, skipped []error, now, windowEnd time.Time) Result {
	if tooManySkips(len(events), len(skipped)) {
		return Result{Source: s, Err: fmt.Errorf("%s: %d of %d events unusable",
			s.URL, len(skipped), len(events)+len(skipped))}
	}

	// Indexed rather than ranged, for the reason the tribe fetcher gives:
	// range yields a copy of each RawEvent.
	for i := range events {
		events[i].SourceKey = s.URL
	}

	return Result{
		Source:    s,
		Events:    events,
		Skipped:   skipped,
		FetchedAt: now,
		WindowEnd: windowEnd,
	}
}

// Get fetches a Bevy chapter: the chapter page for its event links, then each
// event page for its JSON-LD, one at a time and two seconds apart.
//
// Sequential on purpose. Fetch already runs sources concurrently, and a
// chapter's requests all go to one host that has asked to be paced, so the
// gain from parallelising within it would be spent breaking that request.
// Four event pages cost about eight seconds, twice a day.
//
// An event page that fails to fetch or decode is a skip, not a failure of the
// source: the chapter page answered, so the source is up, and a non-empty
// Skipped is what stops a missing event from reading as a cancellation.
func (f bevyFetcher) Get(ctx context.Context, s core.Source, now time.Time) Result {
	windowStart, windowEnd := window(now)

	base, err := url.Parse(s.URL)
	if err != nil {
		return Result{Source: s, Err: fmt.Errorf("parse source url %q: %w", s.URL, err)}
	}
	body, err := getBody(ctx, s.URL)
	if err != nil {
		return Result{Source: s, Err: err}
	}
	links, err := source.ParseBevyChapter(bytes.NewReader(body), base)
	if err != nil {
		return Result{Source: s, Err: fmt.Errorf("parse %s: %w", s.URL, err)}
	}

	var (
		events  []core.RawEvent
		skipped []error
	)
	if len(links) > maxBevyEvents {
		skipped = append(skipped, fmt.Errorf("stopped after %d of %d event pages", maxBevyEvents, len(links)))
		links = links[:maxBevyEvents]
	}

	for _, link := range links {
		if err := pause(ctx, f.delay); err != nil {
			return Result{Source: s, Err: err}
		}
		body, err := getBody(ctx, link)
		if err != nil {
			skipped = append(skipped, err)
			continue
		}
		e, err := source.ParseBevyEvent(bytes.NewReader(body))
		if errors.Is(err, source.ErrBevyCancelled) {
			continue
		}
		if err != nil {
			skipped = append(skipped, fmt.Errorf("parse %s: %w", link, err))
			continue
		}
		// Past events are linked alongside upcoming ones and only their own
		// page says which is which, so the window is applied here rather than
		// in the decoder.
		if e.Start.Before(windowStart) || e.Start.After(windowEnd) {
			continue
		}
		events = append(events, e)
	}

	return finish(s, events, skipped, now, windowEnd)
}

// pause waits d, or returns early with the context's error.
func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
