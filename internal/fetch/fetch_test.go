package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

	res := getTribe(t.Context(), tribeSource(srv.URL), testNow)
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

	res := getTribe(t.Context(), tribeSource(srv.URL), testNow)
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

	res := getTribe(t.Context(), tribeSource(srv.URL), testNow)
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

	res := getTribe(t.Context(), tribeSource(srv.URL), testNow)
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

	res := getTribe(t.Context(), tribeSource(srv.URL), testNow)
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

	res := getTribe(t.Context(), tribeSource(srv.URL), testNow)
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

			res := getTribe(t.Context(), tribeSource(srv.URL), testNow)
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
	res := getTribe(t.Context(), s, testNow)
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

	res := getTribe(ctx, tribeSource(srv.URL), testNow)
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
