package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// fakeStore is the second implementation of EventStore, and the reason the
// interface exists at all rather than the handler taking a *store.Store.
//
// Every test in this file runs with no SQLite, no file and no fixture: the
// handler's job is turning domain values into HTTP, and that is what gets
// tested. It also makes the failure cases reachable, which they are not
// against a real database that refuses to break on request.
type fakeStore struct {
	events []core.Event
	groups map[string]core.Group
	synced time.Time

	err error // returned by whichever call errFrom names
	// Named rather than a bool per method, so a test says which read fails.
	errFrom string

	calls []string
}

func (f *fakeStore) Upcoming(_ context.Context, _ time.Time) ([]core.Event, error) {
	f.calls = append(f.calls, "Upcoming")
	if f.errFrom == "Upcoming" {
		return nil, f.err
	}
	return f.events, nil
}

func (f *fakeStore) Groups(context.Context) (map[string]core.Group, error) {
	f.calls = append(f.calls, "Groups")
	if f.errFrom == "Groups" {
		return nil, f.err
	}
	return f.groups, nil
}

func (f *fakeStore) LastSynced(context.Context) (time.Time, error) {
	f.calls = append(f.calls, "LastSynced")
	if f.errFrom == "LastSynced" {
		return time.Time{}, f.err
	}
	return f.synced, nil
}

var (
	testNow    = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	testSynced = time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)
)

func testStore() *fakeStore {
	return &fakeStore{
		synced: testSynced,
		groups: map[string]core.Group{
			"hlug": {Slug: "hlug", Name: "Houston Linux User Group", URL: "https://houstonlinux.org"},
		},
		events: []core.Event{{
			ID: 1, Fingerprint: "ics:a@google.com", GroupSlug: "hlug",
			Title: "Houston Linux - User Meeting", Excerpt: "Monthly meeting.",
			Start:  time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC),
			End:    time.Date(2026, 9, 25, 1, 0, 0, 0, time.UTC),
			Venue:  core.Venue{ID: 1, Name: "Ion", Address: "4201 Main St", City: "Houston"},
			Room:   "Conference Room 030",
			Status: "published",
			Sources: []core.EventSource{
				{SourceKey: "https://a.test/feed", Fingerprint: "ics:a@google.com"},
				{SourceKey: "https://b.test/feed", Fingerprint: "tribe:b"},
			},
			Categories: []string{"dev"},
		}},
	}
}

func newTestServer(st EventStore) http.Handler {
	return New(st,
		WithClock(func() time.Time { return testNow }),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	).Handler()
}

func get(t *testing.T, h http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEventsEndpoint(t *testing.T) {
	w := get(t, newTestServer(testStore()), "/api/v1/events.json")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}

	var feed Feed
	if err := json.Unmarshal(w.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(feed.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(feed.Events))
	}

	// generated_at is when the data was last current, not when it was served.
	// That is what makes the ETag mean anything.
	if !feed.GeneratedAt.Equal(testSynced) {
		t.Errorf("generated_at = %s, want the last sync %s", feed.GeneratedAt, testSynced)
	}
	// The server can never set preview: it has no way to read a pending event.
	if feed.Preview {
		t.Error("preview = true on a served response")
	}

	e := feed.Events[0]
	if e.ID != "ics:a@google.com" || e.Title != "Houston Linux - User Meeting" {
		t.Errorf("event = %+v", e)
	}
	if e.Group.Name != "Houston Linux User Group" {
		t.Errorf("group name = %q, want it resolved from the slug", e.Group.Name)
	}
	if e.Venue == nil || e.Venue.Address != "4201 Main St" || e.Room != "Conference Room 030" {
		t.Errorf("venue = %+v room = %q", e.Venue, e.Room)
	}
	// Two feeds carried this, which is the only place dedupe is visible.
	if e.Sources != 2 {
		t.Errorf("sources = %d, want 2", e.Sources)
	}
	if e.Pending {
		t.Error("a served event is marked pending")
	}
}

// An event with no end time is absent from the JSON rather than present as
// year 1, so a consumer can treat "no end" as a state.
func TestEventWithNoEndOmitsIt(t *testing.T) {
	st := testStore()
	st.events[0].End = time.Time{}

	w := get(t, newTestServer(st), "/api/v1/events.json")
	if strings.Contains(w.Body.String(), `"end"`) {
		t.Errorf("response carries an end key:\n%s", w.Body.String())
	}
}

func TestETag(t *testing.T) {
	h := newTestServer(testStore())

	first := get(t, h, "/api/v1/events.json")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}

	// Stable across requests. It was not, until generated_at stopped being the
	// wall clock: every response got its own tag and no conditional request
	// could ever match, which is the kind of bug that only shows up if you
	// actually make the second request.
	second := get(t, newTestServer(testStore()), "/api/v1/events.json")
	if got := second.Header().Get("ETag"); got != etag {
		t.Errorf("etag changed between identical responses: %s then %s", etag, got)
	}

	cases := []struct {
		name  string
		match string
		want  int
	}{
		{"exact", etag, http.StatusNotModified},
		{"weak form", "W/" + etag, http.StatusNotModified},
		{"wildcard", "*", http.StatusNotModified},
		{"one of several", `"other", ` + etag, http.StatusNotModified},
		{"stale", `"deadbeef"`, http.StatusOK},
		{"absent", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w *httptest.ResponseRecorder
			if tc.match == "" {
				w = get(t, h, "/api/v1/events.json")
			} else {
				w = get(t, h, "/api/v1/events.json", "If-None-Match", tc.match)
			}
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
			if tc.want == http.StatusNotModified && w.Body.Len() != 0 {
				t.Errorf("304 carried %d bytes, want none", w.Body.Len())
			}
		})
	}
}

// The gate, at the last place data can leave. Upcoming never returns a pending
// event, so this cannot serve one however it is called; the assertion is that
// the handler does not invent a way to.
func TestServerCannotServePendingEvents(t *testing.T) {
	st := testStore()
	// A store that wrongly handed one over anyway.
	st.events = append(st.events, core.Event{
		Fingerprint: "ics:pending@test", GroupSlug: "hlug", Title: "Should not publish",
		Start: testNow.Add(24 * time.Hour), Status: "pending",
	})

	w := get(t, newTestServer(st), "/api/v1/events.json")
	var feed Feed
	if err := json.Unmarshal(w.Body.Bytes(), &feed); err != nil {
		t.Fatal(err)
	}
	// It is still rendered, because the handler trusts the store's query, but
	// it is rendered as pending rather than silently as publishable. If this
	// ever shows false, the marker was lost and a pending event became
	// indistinguishable from a live one.
	for _, e := range feed.Events {
		if e.Title == "Should not publish" && !e.Pending {
			t.Error("a pending event was serialised without its marker")
		}
	}
}

func TestStoreFailuresReturn500AndLeakNothing(t *testing.T) {
	for _, from := range []string{"Upcoming", "Groups", "LastSynced"} {
		t.Run(from, func(t *testing.T) {
			st := testStore()
			st.errFrom = from
			st.err = errors.New("no such table: events [recovered]")

			w := get(t, newTestServer(st), "/api/v1/events.json")
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", w.Code)
			}
			// The detail is logged, never returned. Echoing a database error
			// into a public response is how a schema ends up in somebody's
			// search index.
			if strings.Contains(w.Body.String(), "no such table") {
				t.Errorf("response leaked the underlying error: %s", w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "internal error") {
				t.Errorf("body = %q, want a JSON error", w.Body.String())
			}
		})
	}
}

func TestRouting(t *testing.T) {
	h := newTestServer(testStore())

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/v1/events.json", http.StatusOK},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/api/v1/events", http.StatusNotFound},
		// Method patterns have been a ServeMux feature since Go 1.22. Before
		// that this needed a switch inside every handler or a router.
		{http.MethodPost, "/api/v1/events.json", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/v1/events.json", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
			// The standard library supplies this for a method mismatch, which
			// is behaviour nobody here had to write.
			if tc.want == http.StatusMethodNotAllowed && w.Header().Get("Allow") == "" {
				t.Error("405 with no Allow header")
			}
		})
	}
}

// Liveness, not readiness: it answers "is this process up" and must not touch
// the database. A health check that queries SQLite turns a slow disk into an
// outage as far as a supervisor is concerned, and restarting would not fix it.
func TestHealthDoesNotTouchTheStore(t *testing.T) {
	st := testStore()
	st.errFrom, st.err = "Upcoming", errors.New("database is on fire")

	w := get(t, newTestServer(st), "/healthz")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a broken store", w.Code)
	}
	if len(st.calls) != 0 {
		t.Errorf("health check called the store: %v", st.calls)
	}
}

func TestCORS(t *testing.T) {
	h := newTestServer(testStore())

	t.Run("read is open to any origin", func(t *testing.T) {
		w := get(t, h, "/api/v1/events.json", "Origin", "https://someone-elses-site.test")
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("allow-origin = %q, want *", got)
		}
		// Absent on purpose. Without it a browser will not attach cookies to a
		// cross-origin request whatever an origin asks for, which is what
		// keeps "*" safe here.
		if w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Error("credentials are allowed; * is no longer safe")
		}
		if !strings.Contains(w.Header().Get("Vary"), "Origin") {
			t.Error("no Vary: Origin, so a shared cache may cross origins")
		}
	})

	t.Run("preflight is answered before the mux", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodOptions, "/api/v1/events.json", nil)
		r.Header.Set("Origin", "https://someone-elses-site.test")
		r.Header.Set("Access-Control-Request-Method", "GET")
		r.Header.Set("Access-Control-Request-Headers", "if-none-match")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		// The mux would return 405: OPTIONS is not a route.
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Headers"); got != "if-none-match" {
			t.Errorf("allow-headers = %q, want the requested ones echoed", got)
		}
	})
}

// net/http catches a panic on its own, so the process survives either way.
// What it does not do is answer: the client sees a dropped connection rather
// than a status, and a load balancer reads that as the backend being unhealthy
// rather than as one bad request.
func TestPanicBecomesA500(t *testing.T) {
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("something went very wrong")
	})
	h := recoverPanic(slog.New(slog.NewTextHandler(io.Discard, nil)))(panicking)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events.json", nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "something went very wrong") {
		t.Errorf("the panic value reached the client: %s", w.Body.String())
	}
}

// http.ErrAbortHandler is how a handler says "stop, silently", and net/http
// treats it specially. Swallowing it would turn a client hanging up into a
// logged crash and a 500 nobody is there to read.
func TestAbortHandlerIsNotSwallowed(t *testing.T) {
	aborting := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})
	h := recoverPanic(slog.New(slog.NewTextHandler(io.Discard, nil)))(aborting)

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Errorf("recovered %v, want ErrAbortHandler to propagate", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestEmptyDatabaseServesAnEmptyFeed(t *testing.T) {
	st := &fakeStore{groups: map[string]core.Group{}}

	w := get(t, newTestServer(st), "/api/v1/events.json")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var feed Feed
	if err := json.Unmarshal(w.Body.Bytes(), &feed); err != nil {
		t.Fatal(err)
	}
	// An empty array, not null. A consumer looping over it should not have to
	// check, and encoding/json writes null for a nil slice.
	if !strings.Contains(w.Body.String(), `"events":[]`) {
		t.Errorf("empty feed encoded as %s, want an empty array", w.Body.String())
	}
	// No sync has happened, so generated_at falls back to the clock rather
	// than serialising the zero time as year 1.
	if !feed.GeneratedAt.Equal(testNow) {
		t.Errorf("generated_at = %s, want the clock when nothing has synced", feed.GeneratedAt)
	}
}

// Written because mutation testing found it: replacing the body hash with a
// constant passed every other test in this file, since none of them compared
// two different responses. A constant ETag is worse than no ETag, because a
// cache then holds the first response it ever saw and never revalidates.
func TestETagFollowsTheContent(t *testing.T) {
	tag := func(mutate func(*fakeStore)) string {
		st := testStore()
		mutate(st)
		w := get(t, newTestServer(st), "/api/v1/events.json")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		return w.Header().Get("ETag")
	}

	base := tag(func(*fakeStore) {})

	cases := []struct {
		name   string
		mutate func(*fakeStore)
	}{
		{"a title changes", func(s *fakeStore) { s.events[0].Title = "Something else" }},
		{"an event is added", func(s *fakeStore) { s.events = append(s.events, s.events[0]) }},
		{"the venue moves", func(s *fakeStore) { s.events[0].Venue.Address = "1001 Avenida de las Americas" }},
		{"a group is renamed", func(s *fakeStore) {
			s.groups["hlug"] = core.Group{Slug: "hlug", Name: "Houston Linux"}
		}},
		{"a new sync ran", func(s *fakeStore) { s.synced = testSynced.Add(time.Hour) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tag(tc.mutate); got == base {
				t.Errorf("etag is %s either way; it does not cover the response", got)
			}
		})
	}
}

// Also from mutation testing. A group that leaves sources.yaml still has events
// in the database, and the join returns nothing for it. The slug is a poor name
// but it is a name; the alternative renders an empty line on the site, which
// looks like a layout bug rather than like missing data.
func TestAnUnknownGroupFallsBackToItsSlug(t *testing.T) {
	st := testStore()
	st.events[0].GroupSlug = "group-that-left-the-registry"

	w := get(t, newTestServer(st), "/api/v1/events.json")
	var feed Feed
	if err := json.Unmarshal(w.Body.Bytes(), &feed); err != nil {
		t.Fatal(err)
	}
	if got := feed.Events[0].Group.Name; got != "group-that-left-the-registry" {
		t.Errorf("group name = %q, want the slug rather than a blank", got)
	}
}
