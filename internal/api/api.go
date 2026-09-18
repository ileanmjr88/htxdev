package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
)

// EventStore is the read this package needs, and nothing else.
//
// D3 said to define it here rather than in internal/store, with the handler
// that consumes it. That is the Go convention and it has a concrete payoff:
// *store.Store already satisfies this without knowing the interface exists,
// because Go's interfaces are structural. There is no adapter, no registration
// and no import from store to api.
//
// Two methods rather than the one the plan sketched, because the handler
// genuinely needs both: an event carries a group slug and the wire format
// carries a group name, so something has to resolve one to the other. Doing it
// in the handler keeps the store returning domain types.
//
// The second implementation is the fake in api_test.go, which is the point of
// the interface rather than a side effect of it: handler tests stay offline and
// have no SQLite in them.
type EventStore interface {
	Upcoming(ctx context.Context, from time.Time) ([]core.Event, error)
	Groups(ctx context.Context) (map[string]core.Group, error)
	LastSynced(ctx context.Context) (time.Time, error)
}

// Server holds what the handlers need. Constructed once and read concurrently
// by every request goroutine, so nothing on it is mutated after New returns.
type Server struct {
	store EventStore
	log   *slog.Logger
	// now is injectable because "upcoming" is relative to it, and a test that
	// depended on the wall clock would have events falling out of its fixtures
	// as it ran.
	now func() time.Time
}

type Option func(*Server)

func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }
func WithLogger(l *slog.Logger) Option      { return func(s *Server) { s.log = l } }

func New(store EventStore, opts ...Option) *Server {
	s := &Server{store: store, log: slog.Default(), now: func() time.Time { return time.Now().UTC() }}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Handler returns the routed, wrapped handler.
//
// Returning an http.Handler rather than serving directly is what lets the
// tests drive the whole stack, middleware included, through httptest without
// opening a socket or picking a port.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Method-qualified patterns, which ServeMux has understood since Go 1.22.
	// Before that this needed a switch on r.Method inside every handler, or a
	// router dependency. A POST to this path now gets a 405 with an Allow
	// header from the standard library, which is behaviour nobody has to write.
	mux.HandleFunc("GET /api/v1/events.json", s.handleEvents)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Outermost first: recoverPanic has to wrap logRequests so that a panic
	// inside a handler is still logged as a request, and logRequests has to
	// wrap cors so the log records what was actually returned.
	return recoverPanic(s.log)(logRequests(s.log)(cors(mux)))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	now := s.now()

	events, err := s.store.Upcoming(r.Context(), now)
	if err != nil {
		s.fail(w, r, err, "read events")
		return
	}
	groups, err := s.store.Groups(r.Context())
	if err != nil {
		s.fail(w, r, err, "read groups")
		return
	}

	// generated_at is when the data was last current, not when this byte was
	// served. Using the clock made the body differ on every request, which
	// meant every response got its own ETag and no conditional request could
	// ever match one. It is also the more useful of the two facts: a caller
	// wants to know how stale this is, not what time it is on this machine.
	generated, err := s.store.LastSynced(r.Context())
	if err != nil {
		s.fail(w, r, err, "read last sync")
		return
	}
	if generated.IsZero() {
		generated = now
	}

	// Upcoming only ever returns published events, so this cannot serve a
	// pending one however it is called. The gate is in the store's query
	// rather than here, which is why an API bug cannot open it.
	writeJSON(w, r, http.StatusOK, NewFeed(events, groups, generated, false))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Liveness, not readiness. It answers "is this process up", and
	// deliberately does not touch the database: a health check that queries
	// SQLite turns a slow disk into an outage as far as any supervisor is
	// concerned, and restarting the process would not fix it.
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error, what string) {
	// The error is logged and not returned. A caller gets "something broke",
	// which is all they can act on, and the detail stays where it is useful.
	// Echoing a database error into a public response is how a schema ends up
	// in somebody's search index.
	s.log.ErrorContext(r.Context(), "request failed", "op", what, "err", err, "path", r.URL.Path)
	writeJSON(w, r, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

// writeJSON marshals first and writes second.
//
// Not json.NewEncoder(w).Encode(v), which is the obvious version and is wrong
// here: it writes the status and the first bytes before it can discover that
// marshalling failed, so a failure halfway through emits a 200 followed by
// truncated JSON. Buffering costs one allocation on a response that is tens of
// kilobytes and already came from a database.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// The data changes twice a day at most, so a minute of caching is
	// conservative and still takes the repeated hits off the process.
	// stale-while-revalidate lets a cache keep serving while it refreshes,
	// which matters for a single process behind no CDN.
	w.Header().Set("Cache-Control", "public, max-age=60, stale-while-revalidate=600")

	// A strong ETag over the exact bytes. Cheap here because the body is
	// already in memory, and it turns a repeated poll into a 304 with no body,
	// which is most of what a public feed gets asked for.
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)

	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	// A write failure here means the client went away mid-response. There is
	// nothing to do about it and the status is already sent.
	_, _ = w.Write(body)
}
