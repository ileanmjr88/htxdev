package api

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// middleware is the shape every wrapper here takes. Naming it makes the chain
// in Handler readable as a list rather than as nested calls.
type middleware func(http.Handler) http.Handler

// cors allows any origin to read the feed.
//
// Deliberately wide open, and only because of what this serves: one public
// GET of public event listings, with no cookies, no credentials and no state.
// The point of publishing an API at all is that somebody else's Houston site
// can use it. Access-Control-Allow-Credentials is absent, so a browser will
// not attach cookies to a cross-origin request here whatever an origin asks
// for, and "*" stays safe.
//
// The moment this API grows anything authenticated, this stops being
// acceptable and has to become an allowlist.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Max-Age", "86400")

		// Vary on the request header a cache would otherwise ignore. Without
		// it a shared cache can serve one origin's preflight answer to
		// another, which is only harmless while the answer is "*".
		w.Header().Add("Vary", "Origin")
		w.Header().Add("Vary", "Access-Control-Request-Headers")

		// A preflight is answered here and never reaches the mux, which would
		// return 405 for it: OPTIONS is not a route.
		if r.Method == http.MethodOptions {
			if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				w.Header().Set("Access-Control-Allow-Headers", req)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// logRequests records one line per request, after it finishes.
func logRequests(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			log.InfoContext(r.Context(), "request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.written,
				"duration", time.Since(start),
				// Trimmed, because a User-Agent is attacker-controlled and
				// unbounded, and a log line is not the place to find that out.
				"ua", truncate(r.UserAgent(), 120),
			)
		})
	}
}

// recoverPanic turns a panic in a handler into a 500 and a log line.
//
// Without it, net/http still catches the panic and closes the connection, so
// the process survives either way. What it does not do is answer: the client
// sees a dropped connection rather than a status, and a load balancer reads
// that as the whole backend being unhealthy rather than as one bad request.
func recoverPanic(log *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is how a handler says "stop, silently",
				// and net/http treats it specially. Logging it as a crash would
				// make a client hanging up look like a bug in this program.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				log.ErrorContext(r.Context(), "panic in handler",
					"path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// recorder remembers what a handler did, because http.ResponseWriter will not
// tell you afterwards.
type recorder struct {
	http.ResponseWriter
	status  int
	written int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// Unwrap lets the standard library reach the real ResponseWriter through this
// one, which is what keeps http.ResponseController working: without it,
// wrapping silently removes Flush and SetWriteDeadline from anything upstream.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// etagMatches implements the If-None-Match comparison, which is a list and not
// a string: a client may send several, or "*".
func etagMatches(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		// A cache may return a strong tag weakened. The comparison here is
		// allowed to be weak because this response is a whole document rather
		// than a range.
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
