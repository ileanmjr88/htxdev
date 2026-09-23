package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// The decoder's own fixture, read across packages rather than copied. It is
// 400KB, and a second copy that quietly drifted from the first would be worse
// than the relative path.
const fixturePath = "../source/testdata/ion-tribe.json"

// The happy path is the whole chain in one assertion: request built, header
// sent, body read within the cap, ParseTribe run over it. Pinning the
// fixture's real numbers means a change in either the decoder or this
// transport surfaces here.
func TestFetchPageDecodesFixture(t *testing.T) {
	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Asserted inside the handler rather than captured to a variable:
		// httptest serves on its own goroutine, and reading a captured
		// variable back in the test body is a data race.
		//
		// The User-Agent is how Ion's operators can identify this traffic and
		// find a human. Sending Go's default would make htxdev anonymous.
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("user-agent = %q, want %q", got, userAgent)
		}
		w.Write(body)
	}))
	defer srv.Close()

	page, err := fetchPage(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page.Events) != 50 {
		t.Errorf("events = %d, want 50", len(page.Events))
	}
	if page.Total != 80 {
		t.Errorf("total = %d, want 80", page.Total)
	}
	if page.NextURL == "" {
		t.Error("next url is empty; the fixture is page 1 of 2")
	}
	if len(page.Skipped) != 0 {
		t.Errorf("skipped %d events, want 0: %v", len(page.Skipped), page.Skipped)
	}
}

// Strictly 200. The 2xx entries here are the reason: a 204 has no body and a
// 206 has a fragment, so letting them through would fail later inside
// ParseTribe as an opaque JSON error rather than as the status that caused it.
func TestFetchPageRejectsNon200(t *testing.T) {
	codes := []int{
		http.StatusInternalServerError,
		http.StatusNotFound,
		http.StatusTooManyRequests,
		http.StatusNoContent,
		http.StatusPartialContent,
	}

	for _, code := range codes {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			_, err := fetchPage(t.Context(), srv.URL)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), fmt.Sprint(code)) {
				t.Errorf("error %q does not name the status code", err)
			}
		})
	}
}

// The cap is a refusal, not a chunk size. This also pins the reason for the
// +1: the error has to say the body was too large, not fail downstream as a
// truncated document, which would send the next reader into ParseTribe to
// debug a problem about size.
func TestFetchPageRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("a"), maxBodyBytes+1))
	}))
	defer srv.Close()

	_, err := fetchPage(t.Context(), srv.URL)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error %q does not report the size limit", err)
	}
}

// A decode failure has to name its feed. With a dozen sources in the registry,
// an unadorned json error tells you nothing about which one changed shape.
func TestFetchPageWrapsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"events": not json}`)
	}))
	defer srv.Close()

	_, err := fetchPage(t.Context(), srv.URL)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error %q does not name the url", err)
	}
}

// Cancellation has to survive being wrapped. getTribe will need errors.Is to
// tell "Ion was slow" apart from "the feed changed shape", and every %w
// between the transport and that check is what keeps the chain intact. A %v
// anywhere in the middle would break this test.
func TestFetchPageCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran; the request should have been cancelled before it left")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := fetchPage(ctx, srv.URL); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// A fixed instant, so the window the test asserts on is the window the code
// built. Real dates rather than a zero time, because a zero time formats to
// year 1 and would hide a sign error in the lookback.
var testNow = time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC)

// tribePage builds one page of feed. The bad events carry no global_id, which
// is how ParseTribe is made to skip an event without hand-writing malformed
// JSON that Ion would never actually serve. Skips in these tests therefore come
// from the same code path a real format change would take.
func tribePage(good, bad int, nextURL string) string {
	events := make([]string, 0, good+bad)
	for i := range good {
		events = append(events, fmt.Sprintf(
			`{"global_id":"ion-%d","title":"Event %d","utc_start_date":"2026-08-20 18:00:00"}`, i, i))
	}
	for range bad {
		events = append(events, `{"title":"no global_id","utc_start_date":"2026-08-20 18:00:00"}`)
	}
	return fmt.Sprintf(`{"total":%d,"next_rest_url":%q,"events":[%s]}`,
		good+bad, nextURL, strings.Join(events, ","))
}

func tribeSource(u string) core.Source {
	return core.Source{Kind: core.KindTribe, URL: u, Enabled: true}
}

// The window is a contract with normalize, not a detail: absence only means
// "cancelled" for events comfortably inside WindowEnd, so WindowEnd has to be
// the same instant that went out as end_date. Asserting both in one test is
// what stops them drifting apart.
func TestGetTribeBuildsWindowQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		want := map[string]string{
			"start_date": "2026-08-13 09:30:00", // testNow - lookbackDays
			"end_date":   "2026-10-14 09:30:00", // testNow + fetchWindowDays
			"per_page":   "100",
			"status":     "publish",
			"page":       "1",
		}
		for k, v := range want {
			if got := q.Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
		fmt.Fprint(w, tribePage(2, 0, ""))
	}))
	defer srv.Close()

	res := tribeFetcher{}.Get(t.Context(), tribeSource(srv.URL), testNow)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if got := res.WindowEnd.Format("2006-01-02 15:04:05"); got != "2026-10-14 09:30:00" {
		t.Errorf("WindowEnd = %s, want the end_date that was sent", got)
	}
	if !res.FetchedAt.Equal(testNow) {
		t.Errorf("FetchedAt = %v, want %v", res.FetchedAt, testNow)
	}
}

// Pagination is invisible above this layer: two pages in, one Result out. The
// next URL is built from r.Host so it is same-origin by construction, which is
// also what Ion does.
func TestGetTribeFollowsNextURL(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch page := r.URL.Query().Get("page"); page {
		case "1":
			fmt.Fprint(w, tribePage(3, 0, "http://"+r.Host+"/?page=2"))
		case "2":
			fmt.Fprint(w, tribePage(2, 0, "")) // empty next: the feed says stop
		default:
			t.Errorf("unexpected page %q", page)
		}
	}))
	defer srv.Close()

	res := tribeFetcher{}.Get(t.Context(), tribeSource(srv.URL), testNow)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
	if len(res.Events) != 5 {
		t.Errorf("events = %d, want 5 across both pages", len(res.Events))
	}
	if len(res.Skipped) != 0 {
		t.Errorf("skipped = %v, want none", res.Skipped)
	}
}

// SourceKey is the bare source URL, not the paginated one the request went to.
// Page 2's URL differs from page 1's, so a single distinct value here is what
// proves attribution survives pagination. It also pins the indexed assignment:
// a ranged loop would leave every key empty and still compile.
func TestGetTribeStampsSourceKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, tribePage(2, 0, "http://"+r.Host+"/?page=2"))
			return
		}
		fmt.Fprint(w, tribePage(2, 0, ""))
	}))
	defer srv.Close()

	res := tribeFetcher{}.Get(t.Context(), tribeSource(srv.URL), testNow)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if len(res.Events) != 4 {
		t.Fatalf("events = %d, want 4", len(res.Events))
	}
	for i, e := range res.Events {
		if e.SourceKey != srv.URL {
			t.Errorf("event %d SourceKey = %q, want %q", i, e.SourceKey, srv.URL)
		}
	}
}

// next_rest_url is attacker-controllable in the sense that matters: it is a
// string from someone else's WordPress install. Following it off-origin would
// let a compromised feed aim this client at anything reachable from the runner.
// The refusal is an incompleteness, not a failure, so page 1 survives and the
// skip entry is what defers cancellation.
func TestGetTribeDeclinesOffOriginNextURL(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("off-origin next_rest_url was followed")
	}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, tribePage(3, 0, other.URL))
	}))
	defer srv.Close()

	res := tribeFetcher{}.Get(t.Context(), tribeSource(srv.URL), testNow)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if len(res.Events) != 3 {
		t.Errorf("events = %d, want the 3 from page 1", len(res.Events))
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %v, want one refusal", res.Skipped)
	}
	if !strings.Contains(res.Skipped[0].Error(), "declining") {
		t.Errorf("skip %q does not say the url was declined", res.Skipped[0])
	}
}

// Both servers are on 127.0.0.1 and differ only by port, so this also pins that
// the comparison uses Host and not Hostname. Hostname drops the port, which
// would make every loopback server look like the same origin and quietly pass
// the test above for the wrong reason.
func TestGetTribeOriginCheckIncludesPort(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, tribePage(1, 0, other.URL))
	}))
	defer srv.Close()

	if !strings.HasPrefix(other.URL, "http://127.0.0.1:") || !strings.HasPrefix(srv.URL, "http://127.0.0.1:") {
		t.Skip("servers are not both on 127.0.0.1; nothing to distinguish")
	}

	res := tribeFetcher{}.Get(t.Context(), tribeSource(srv.URL), testNow)
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %v, want the port mismatch refused", res.Skipped)
	}
}

// A feed that never stops offering a next page cannot be allowed to spin this
// forever, but stopping silently is worse than spinning: the events past the
// cut are absent from Events, and absence is how normalize infers cancellation.
// So the stop has to leave a mark in Skipped, and Err has to stay nil so the
// pages that did parse are kept.
func TestGetTribeTruncatesAtMaxPages(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, tribePage(2, 0, "http://"+r.Host+"/")) // always another page
	}))
	defer srv.Close()

	res := tribeFetcher{}.Get(t.Context(), tribeSource(srv.URL), testNow)
	if res.Err != nil {
		t.Fatalf("truncation must not fail the source: %v", res.Err)
	}
	if got := hits.Load(); got != maxPages {
		t.Errorf("requests = %d, want maxPages (%d)", got, maxPages)
	}
	if len(res.Events) != 2*maxPages {
		t.Errorf("events = %d, want %d", len(res.Events), 2*maxPages)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("skipped = %v, want one truncation notice", res.Skipped)
	}
	if !strings.Contains(res.Skipped[0].Error(), "stopped after") {
		t.Errorf("skip %q does not report the truncation", res.Skipped[0])
	}
}

// The ratio separates "one odd event" from "the feed changed shape". The two
// halves of the condition are load-bearing in opposite directions, so both the
// boundary and the small-page floor are pinned here. minPageForRatio exists
// because one skip out of two is a third of nothing.
func TestGetTribeSkipRatio(t *testing.T) {
	tests := []struct {
		name      string
		good, bad int
		wantErr   bool
	}{
		{"exactly one in three is tolerated", 7, 3, false},
		{"more than one in three fails the source", 6, 4, true},
		{"page below the floor tolerates any ratio", 1, 2, false},
		{"page below the floor tolerates everything skipping", 0, 3, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tribePage(tc.good, tc.bad, ""))
			}))
			defer srv.Close()

			res := tribeFetcher{}.Get(t.Context(), tribeSource(srv.URL), testNow)
			if tc.wantErr {
				if res.Err == nil {
					t.Fatal("want error, got nil")
				}
				// Err means the source is excluded from cancellation entirely,
				// which is only safe if it carries no half-page of events for
				// normalize to mistake for the whole feed.
				if len(res.Events) != 0 {
					t.Errorf("events = %d, want none alongside Err", len(res.Events))
				}
				return
			}
			if res.Err != nil {
				t.Fatalf("unexpected error: %v", res.Err)
			}
			if len(res.Events) != tc.good {
				t.Errorf("events = %d, want %d", len(res.Events), tc.good)
			}
			if len(res.Skipped) != tc.bad {
				t.Errorf("skipped = %d, want %d", len(res.Skipped), tc.bad)
			}
		})
	}
}

// Fetch returns a slice of these, so a Result that failed without naming its
// Source is an error nobody can attribute to a feed. Every early return owes
// Source, and the transport path is the one most likely to forget it.
func TestGetTribeTransportFailureKeepsSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := tribeSource(srv.URL)
	res := tribeFetcher{}.Get(t.Context(), s, testNow)
	if res.Err == nil {
		t.Fatal("want error, got nil")
	}
	if res.Source.URL != s.URL {
		t.Errorf("Source.URL = %q, want %q", res.Source.URL, s.URL)
	}
	if len(res.Events) != 0 {
		t.Errorf("events = %d, want none alongside Err", len(res.Events))
	}
}

// A cancelled context must surface as the transport error it is, not as a
// truncation. The distinction matters because truncation writes to Skipped and
// lets cancellation proceed on the events that did arrive, which for a shut
// down run would be most of the calendar.
func TestGetTribeCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran; the request should have been cancelled before it left")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res := tribeFetcher{}.Get(ctx, tribeSource(srv.URL), testNow)
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", res.Err)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("skipped = %v, want none; cancellation is not truncation", res.Skipped)
	}
}

// The premise of the entire design, from D15 and §4 of the architecture: one
// dead feed must not take down the batch.
//
// Index 3 carries this test twice over. It proves the error was recorded
// against the source that actually failed, and because a 500 returns faster
// than a served page it also proves ordering: a Fetch that appended results as
// they completed would land this error at index 0 and fail here.
func TestFetchKeepsGoingWhenOneSourceFails(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, tribePage(3, 0, ""))
	}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	const badIndex = 3
	sources := make([]core.Source, 7)
	for i := range sources {
		if i == badIndex {
			sources[i] = tribeSource(bad.URL)
			continue
		}
		sources[i] = tribeSource(good.URL)
	}

	results := Fetch(t.Context(), sources)

	if len(results) != len(sources) {
		t.Fatalf("results = %d, want %d; every source gets exactly one", len(results), len(sources))
	}
	if results[badIndex].Err == nil {
		t.Errorf("results[%d].Err = nil, want the 500 recorded", badIndex)
	}
	if n := len(results[badIndex].Events); n != 0 {
		t.Errorf("results[%d] carried %d events, want none from a failed source", badIndex, n)
	}

	for i, res := range results {
		if i == badIndex {
			continue
		}
		if res.Err != nil {
			t.Errorf("results[%d].Err = %v, want nil; one bad feed must not fail its neighbours", i, res.Err)
		}
		if len(res.Events) != 3 {
			t.Errorf("results[%d] events = %d, want 3", i, len(res.Events))
		}
		if res.Source.URL != good.URL {
			t.Errorf("results[%d].Source.URL = %q, want %q; results are out of input order",
				i, res.Source.URL, good.URL)
		}
	}
}

// The semaphore is the only thing bounding this, and deleting it would leave
// every other test in the package green. This is the one that would notice.
//
// atomic.Int32.Add returns the post-increment value, so cur is a safe
// observation of how many requests were in flight including this one, with no
// separate maximum to keep in sync.
//
// The sleep is load-bearing. Without it a request finishes faster than the
// next goroutine is scheduled, concurrency never climbs above 1, and the
// assertion holds for a reason that has nothing to do with the semaphore. The
// peak check at the bottom is what proves that did not happen.
func TestFetchCapsConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)

		// Standard atomic-max: retry until this observation is recorded or a
		// larger one already is. t.Fatalf is unusable here, since it would
		// call runtime.Goexit on the server's goroutine rather than the test's.
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		if cur > maxConcurrent {
			t.Errorf("in flight = %d, want at most %d", cur, maxConcurrent)
		}

		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, tribePage(1, 0, ""))
	}))
	defer srv.Close()

	sources := make([]core.Source, 12)
	for i := range sources {
		sources[i] = tribeSource(srv.URL)
	}

	results := Fetch(t.Context(), sources)

	for i, res := range results {
		if res.Err != nil {
			t.Fatalf("results[%d].Err = %v, want nil", i, res.Err)
		}
	}
	if got := peak.Load(); got < 2 {
		t.Errorf("peak concurrency = %d; nothing overlapped, so the cap was never exercised", got)
	}
}

// The HLUG export, read across packages for the same reason as the tribe
// fixture: it is 52KB and a second copy would drift.
const icsFixturePath = "../source/testdata/hlug-gcal.ics"

// Chosen so window(icsNow) is exactly 2026-09-15 to 2026-11-16, the span
// internal/source's fixture test uses. Both suites then agree on the number
// 19, and a disagreement means the window arithmetic moved rather than the
// decoder.
var icsNow = time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)

func icsSource(u string) core.Source {
	return core.Source{GroupSlug: "hlug", Kind: core.KindICS, URL: u, Enabled: true}
}

func icsBody(events ...string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + strings.Join(events, "") + "END:VCALENDAR\r\n"
}

func icsEventLines(lines ...string) string {
	return "BEGIN:VEVENT\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\n"
}

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("user-agent = %q, want %q", got, userAgent)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The whole ICS chain end to end against the real Google export: request,
// body read inside the cap, ParseICS over it with the run's window, ratio,
// stamping.
func TestICSFetcherDecodesFixture(t *testing.T) {
	body, err := os.ReadFile(icsFixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := serve(t, http.StatusOK, string(body))

	res := icsFetcher{}.Get(t.Context(), icsSource(srv.URL), icsNow)

	if res.Err != nil {
		t.Fatalf("Err = %v, want nil", res.Err)
	}
	if len(res.Events) != 19 {
		t.Fatalf("got %d events, want 19 (10 concrete plus 9 expanded)", len(res.Events))
	}
	if len(res.Skipped) != 0 {
		t.Errorf("skipped = %v, want none", res.Skipped)
	}

	// Attribution is the source's URL, set by this layer and not the decoder,
	// because the decoder is handed a reader and has no idea where it came
	// from.
	for i, e := range res.Events {
		if e.SourceKey != srv.URL {
			t.Fatalf("event %d SourceKey = %q, want %q", i, e.SourceKey, srv.URL)
		}
	}

	if !res.FetchedAt.Equal(icsNow) {
		t.Errorf("FetchedAt = %s, want the run's shared now %s", res.FetchedAt, icsNow)
	}
	// Normalize needs this to know how far absence can be trusted, and it has
	// to be the window actually asked for rather than one recomputed later.
	if want := icsNow.AddDate(0, 0, fetchWindowDays); !res.WindowEnd.Equal(want) {
		t.Errorf("WindowEnd = %s, want %s", res.WindowEnd, want)
	}
	for i, e := range res.Events {
		if e.Start.Before(icsNow.AddDate(0, 0, -lookbackDays)) || e.Start.After(res.WindowEnd) {
			t.Errorf("event %d at %s is outside the window the Result advertises", i, e.Start)
		}
	}
}

func TestICSFetcherSourceLevelFailures(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"server error", http.StatusInternalServerError, "", "unexpected status"},
		{"not found", http.StatusNotFound, "", "unexpected status"},
		{"html error page served as 200", http.StatusOK, "<html>Not Found</html>", "iCalendar"},
		{"empty body", http.StatusOK, "", "iCalendar"},
		{"json", http.StatusOK, `{"events":[]}`, "iCalendar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, tc.status, tc.body)
			res := icsFetcher{}.Get(t.Context(), icsSource(srv.URL), icsNow)

			if res.Err == nil {
				t.Fatalf("Err = nil, want an error (got %d events)", len(res.Events))
			}
			if !strings.Contains(res.Err.Error(), tc.wantErr) {
				t.Errorf("Err = %v, want it to mention %q", res.Err, tc.wantErr)
			}
			if len(res.Events) != 0 {
				t.Errorf("got %d events alongside an Err, want none", len(res.Events))
			}
		})
	}
}

// A calendar with nothing in the window is a quiet group, not a broken feed.
// Two of the six live ICS sources are in this state today, and reporting them
// as failures would train whoever reads the sync log to ignore it.
func TestICSFetcherTreatsAnEmptyWindowAsSuccess(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no events at all", icsBody()},
		{"every event outside the window", icsBody(
			icsEventLines("UID:old@test", "DTSTART:20200101T170000Z"),
			icsEventLines("UID:far@test", "DTSTART:20301231T170000Z"),
		)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, http.StatusOK, tc.body)
			res := icsFetcher{}.Get(t.Context(), icsSource(srv.URL), icsNow)

			if res.Err != nil {
				t.Fatalf("Err = %v, want nil", res.Err)
			}
			if len(res.Events) != 0 {
				t.Errorf("got %d events, want none", len(res.Events))
			}
			// The important half. A non-empty Skipped defers cancellation for
			// the whole source, so an out-of-window event must not land there
			// or this group would defer on every run forever.
			if len(res.Skipped) != 0 {
				t.Errorf("skipped = %v, want none", res.Skipped)
			}
		})
	}
}

// D6's ratio, applied to a whole ICS feed rather than to a page, because an
// ICS feed has no pages.
func TestICSFetcherRatio(t *testing.T) {
	good := func(n int) []string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, icsEventLines(
				fmt.Sprintf("UID:good%d@test", i),
				fmt.Sprintf("DTSTART:202610%02dT170000Z", i+1),
			))
		}
		return out
	}
	// No UID is how a real format change reaches the skip path, and it is the
	// same shape the tribe helper induces by dropping global_id.
	bad := func(n int) []string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, icsEventLines("SUMMARY:No UID", fmt.Sprintf("DTSTART:202610%02dT170000Z", i+1)))
		}
		return out
	}

	cases := []struct {
		name       string
		good, bad  int
		wantFailed bool
	}{
		{"one bad in four is tolerated", 3, 1, false},
		{"three bad in four fails the source", 1, 3, true},
		{"two bad in four is over one in three", 2, 2, true},
		// The boundary itself. The rule is "more than one in three", so
		// exactly one in three is tolerated. Nothing else in either ratio
		// table lands on the equality, which is what let a > silently become
		// a >= when this was mutation-tested.
		{"exactly one bad in three is tolerated", 4, 2, false},
		{"just over one in three fails", 3, 2, true},
		// Below minPageForRatio no ratio is meaningful, so a small feed keeps
		// its good event rather than blanking a legitimate group.
		{"one bad in three is below the floor", 2, 1, false},
		{"one bad and nothing else", 0, 1, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, http.StatusOK, icsBody(append(good(tc.good), bad(tc.bad)...)...))
			res := icsFetcher{}.Get(t.Context(), icsSource(srv.URL), icsNow)

			if tc.wantFailed {
				if res.Err == nil {
					t.Fatalf("Err = nil, want the source to fail (%d events, %d skips)", len(res.Events), len(res.Skipped))
				}
				// Err set means Events is empty, per the type's contract.
				if len(res.Events) != 0 {
					t.Errorf("got %d events alongside an Err, want none", len(res.Events))
				}
				return
			}
			if res.Err != nil {
				t.Fatalf("Err = %v, want the good events kept", res.Err)
			}
			if len(res.Events) != tc.good {
				t.Errorf("got %d events, want %d", len(res.Events), tc.good)
			}
			if len(res.Skipped) != tc.bad {
				t.Errorf("got %d skips, want %d: a tolerated skip still has to be counted", len(res.Skipped), tc.bad)
			}
		})
	}
}

// The dispatch table. Phase 1 had one decoder and a switch; this is what D3
// said to extract once the second one existed.
func TestGetDispatchesOnKind(t *testing.T) {
	srv := serve(t, http.StatusOK, icsBody(icsEventLines("UID:d@test", "DTSTART:20261001T170000Z")))

	t.Run("ics reaches the ics fetcher", func(t *testing.T) {
		res := get(t.Context(), icsSource(srv.URL), icsNow)
		if res.Err != nil || len(res.Events) != 1 {
			t.Fatalf("got %d events, Err = %v; want the ICS decoder to have run", len(res.Events), res.Err)
		}
	})

	t.Run("tribe reaches the tribe fetcher", func(t *testing.T) {
		// Served ICS, asked for as tribe. The failure has to come from the
		// JSON decoder, which is how we know which fetcher ran.
		res := get(t.Context(), tribeSource(srv.URL), icsNow)
		if res.Err == nil {
			t.Fatal("Err = nil, want a decode failure")
		}
		if strings.Contains(res.Err.Error(), "iCalendar") {
			t.Errorf("Err = %v, want the tribe decoder's complaint and not the ICS one", res.Err)
		}
	})

	t.Run("an unknown kind is reported, not panicked on", func(t *testing.T) {
		res := get(t.Context(), core.Source{Kind: "gopher", URL: srv.URL}, icsNow)
		if res.Err == nil || !strings.Contains(res.Err.Error(), "gopher") {
			t.Fatalf("Err = %v, want it to name the unknown kind", res.Err)
		}
	})

	t.Run("every kind the registry accepts has a fetcher", func(t *testing.T) {
		// The table and core's kind list are edited separately, and a kind
		// that loads from the registry but has no entry here would fail every
		// source of that kind at runtime with nothing catching it earlier.
		for _, k := range []core.SourceKind{core.KindTribe, core.KindICS, core.KindHTML, core.KindBevy} {
			if _, ok := fetchers[k]; !ok {
				t.Errorf("kind %q has no Fetcher", k)
			}
		}
	})
}

const hossFixturePath = "../source/testdata/hoss-meetings.html"

// The third Fetcher implementation, which is what makes the interface a
// conclusion rather than a guess. Same window as the ICS tests, so all five of
// the fixture's meetings fall inside it.
func TestHOSSFetcherDecodesFixture(t *testing.T) {
	body, err := os.ReadFile(hossFixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := serve(t, http.StatusOK, string(body))

	src := core.Source{GroupSlug: "houston-open-source-society", Kind: core.KindHTML, URL: srv.URL, Enabled: true}
	res := hossFetcher{}.Get(t.Context(), src, icsNow)

	if res.Err != nil {
		t.Fatalf("Err = %v, want nil", res.Err)
	}
	if len(res.Events) != 5 {
		t.Fatalf("got %d events, want 5 weekly slots", len(res.Events))
	}
	for i, e := range res.Events {
		if e.SourceKey != srv.URL {
			t.Errorf("event %d SourceKey = %q, want %q", i, e.SourceKey, srv.URL)
		}
	}
	if !res.FetchedAt.Equal(icsNow) {
		t.Errorf("FetchedAt = %s, want %s", res.FetchedAt, icsNow)
	}
	if want := icsNow.AddDate(0, 0, fetchWindowDays); !res.WindowEnd.Equal(want) {
		t.Errorf("WindowEnd = %s, want %s", res.WindowEnd, want)
	}
}

func TestHOSSFetcherSourceLevelFailures(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"server error", http.StatusInternalServerError, "", "unexpected status"},
		{"a redesigned page", http.StatusOK, "<html><body><h1>Meetings</h1></body></html>", "structure has changed"},
		{"empty body", http.StatusOK, "", "structure has changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, tc.status, tc.body)
			src := core.Source{GroupSlug: "hoss", Kind: core.KindHTML, URL: srv.URL}
			res := hossFetcher{}.Get(t.Context(), src, icsNow)

			if res.Err == nil {
				t.Fatalf("Err = nil, want an error (got %d events)", len(res.Events))
			}
			if !strings.Contains(res.Err.Error(), tc.wantErr) {
				t.Errorf("Err = %v, want it to mention %q", res.Err, tc.wantErr)
			}
			if len(res.Events) != 0 {
				t.Errorf("got %d events alongside an Err, want none", len(res.Events))
			}
		})
	}
}

// html has to reach the HTML decoder and nothing else. Serving an HTML page and
// asking for it as ics proves the dispatch is real and not accidental.
func TestGetDispatchesHTMLKind(t *testing.T) {
	body, err := os.ReadFile(hossFixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := serve(t, http.StatusOK, string(body))

	res := get(t.Context(), core.Source{GroupSlug: "hoss", Kind: core.KindHTML, URL: srv.URL}, icsNow)
	if res.Err != nil || len(res.Events) != 5 {
		t.Fatalf("html kind: got %d events, Err = %v; want 5", len(res.Events), res.Err)
	}

	res = get(t.Context(), icsSource(srv.URL), icsNow)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "iCalendar") {
		t.Errorf("ics kind over an HTML body: Err = %v, want the ICS decoder's complaint", res.Err)
	}
}

// bevyChapter serves a Bevy chapter at / linking to each path, and each path
// from pages. Links are relative, so they resolve to the test server the way
// the real chapter's resolve to usergroups.snowflake.com. A path missing from
// pages answers 404. The returned func reports every path requested so far,
// under the lock the handler writes it with.
func bevyChapter(t *testing.T, paths []string, pages map[string]string) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/" {
			var b strings.Builder
			b.WriteString("<html><body>")
			for _, p := range paths {
				b.WriteString(`<a href="` + p + `">event</a>`)
			}
			b.WriteString("</body></html>")
			_, _ = w.Write([]byte(b.String()))
			return
		}
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func bevyEventPage(t *testing.T, fixture string) string {
	t.Helper()
	b, err := os.ReadFile("../source/testdata/" + fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func bevyLD(status, start string) string {
	return `<html><head><link rel="canonical" href="https://usergroups.snowflake.com/events/details/x/">` +
		`<script type="application/ld+json">{"@type":"Event","name":"X","startDate":"` + start +
		`","eventStatus":"https://schema.org/` + status + `"}</script></head></html>`
}

// The day the chapter asked to be listed. The relaunch meeting is 15 days
// out, inside the window; the 2020 one is not.
var bevyNow = time.Date(2026, 9, 23, 17, 0, 0, 0, time.UTC)

func TestBevyFetcherReadsEveryLinkedEventAndKeepsTheWindow(t *testing.T) {
	srv, seen := bevyChapter(t,
		[]string{
			"/events/details/relaunch/",
			"/events/details/virtual-2020/",
			"/events/details/called-off/",
			"/events/details/gone/",
		},
		map[string]string{
			"/events/details/relaunch/":     bevyEventPage(t, "bevy-event.html"),
			"/events/details/virtual-2020/": bevyEventPage(t, "bevy-event-virtual.html"),
			"/events/details/called-off/":   bevyLD("EventCancelled", "2026-10-01T18:00:00-05:00"),
		})
	src := core.Source{GroupSlug: "snowflake-houston", Kind: core.KindBevy, URL: srv.URL + "/"}

	res := bevyFetcher{}.Get(t.Context(), src, bevyNow)

	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if len(res.Events) != 1 || res.Events[0].Title != "Houston User Group Relaunch Kickoff Meeting" {
		t.Fatalf("events = %+v, want only the relaunch meeting", res.Events)
	}
	if res.Events[0].SourceKey != src.URL {
		t.Errorf("SourceKey = %q, want the chapter URL", res.Events[0].SourceKey)
	}
	// The cancelled event is left out quietly; its absence is the signal. The
	// 404 is a skip, which is what keeps it from reading as a cancellation.
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Error(), "404") {
		t.Errorf("skipped = %v, want exactly the 404", res.Skipped)
	}
	if got, want := seen(), 5; len(got) != want {
		t.Errorf("made %d requests (%v), want %d: the chapter and every event page", len(got), got, want)
	}
	if !res.WindowEnd.Equal(bevyNow.AddDate(0, 0, fetchWindowDays)) {
		t.Errorf("WindowEnd = %s", res.WindowEnd)
	}
}

func TestBevyFetcherChapterFailuresFailTheSource(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"server error", http.StatusInternalServerError, "", "unexpected status"},
		{"no event links", http.StatusOK, "<html><body>redesigned</body></html>", "page structure has changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, tc.status, tc.body)
			res := bevyFetcher{}.Get(t.Context(), core.Source{Kind: core.KindBevy, URL: srv.URL + "/"}, bevyNow)
			if res.Err == nil || !strings.Contains(res.Err.Error(), tc.want) {
				t.Fatalf("Err = %v, want it to mention %q", res.Err, tc.want)
			}
		})
	}
}

// Every event page unreadable is a format change, and the ratio fails the
// source for it rather than publishing an empty chapter.
func TestBevyFetcherFailsWhenEveryEventPageIsUnreadable(t *testing.T) {
	paths := []string{"/events/details/a/", "/events/details/b/", "/events/details/c/", "/events/details/d/"}
	pages := map[string]string{}
	for _, p := range paths {
		pages[p] = "<html><head></head></html>"
	}
	srv, _ := bevyChapter(t, paths, pages)
	res := bevyFetcher{}.Get(t.Context(), core.Source{Kind: core.KindBevy, URL: srv.URL + "/"}, bevyNow)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "4 of 4 events unusable") {
		t.Fatalf("Err = %v, want the ratio to fail the source", res.Err)
	}
}

func TestBevyFetcherStopsAtMaxBevyEvents(t *testing.T) {
	var paths []string
	pages := map[string]string{}
	for i := range maxBevyEvents + 3 {
		p := fmt.Sprintf("/events/details/e%d/", i)
		paths = append(paths, p)
		pages[p] = bevyLD("EventScheduled", "2026-10-08T18:00:00-05:00")
	}
	srv, seen := bevyChapter(t, paths, pages)
	res := bevyFetcher{}.Get(t.Context(), core.Source{Kind: core.KindBevy, URL: srv.URL + "/"}, bevyNow)

	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if got := len(seen()); got != maxBevyEvents+1 {
		t.Errorf("made %d requests, want %d", got, maxBevyEvents+1)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Error(), "stopped after") {
		t.Errorf("skipped = %v, want the truncation recorded", res.Skipped)
	}
}

// The pause between requests is where a Bevy sync spends nearly all its time,
// so it has to give way to Ctrl-C rather than finish its two seconds.
func TestBevyFetcherPauseHonoursCancellation(t *testing.T) {
	srv, seen := bevyChapter(t, []string{"/events/details/a/"}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	f := bevyFetcher{delay: time.Hour}

	done := make(chan Result, 1)
	go func() { done <- f.Get(ctx, core.Source{Kind: core.KindBevy, URL: srv.URL + "/"}, bevyNow) }()

	// Let the chapter request land, then cancel mid-pause.
	for deadline := time.Now().Add(5 * time.Second); len(seen()) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case res := <-done:
		if !errors.Is(res.Err, context.Canceled) {
			t.Errorf("Err = %v, want context.Canceled", res.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get did not return after cancellation")
	}
}

// The registered fetcher is the one real syncs use, so it is the one that has
// to carry robots.txt's delay.
func TestBevyFetcherIsRegisteredWithTheCrawlDelay(t *testing.T) {
	f, ok := fetchers[core.KindBevy].(bevyFetcher)
	if !ok || f.delay != 2*time.Second {
		t.Fatalf("fetchers[bevy] = %#v, want a bevyFetcher with a 2s delay", fetchers[core.KindBevy])
	}
}
