package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/ileanmjr88/htxdev/internal/api"
	"github.com/ileanmjr88/htxdev/internal/store"
)

// Server timeouts. net/http's zero value has NONE of these, which means a
// default http.Server will hold a connection open forever for a client that
// opens one and never finishes a request. That is the whole of Slowloris and
// it needs no tooling.
//
// Each one covers a different stall, which is why there are four rather than
// one big number:
const (
	// The request line and headers. The shortest, because a client with
	// nothing to say yet is the cheapest thing to hang up on, and this is the
	// one that closes Slowloris specifically.
	readHeaderTimeout = 5 * time.Second
	// Headers plus body. Generous by comparison because a body can be large
	// and slow legitimately, though nothing here reads one yet.
	readTimeout = 10 * time.Second
	// From the end of the request to the end of the response, so it bounds
	// how long a handler can take. Every response here comes from one SQLite
	// query against a local file.
	writeTimeout = 15 * time.Second
	// How long a kept-alive connection may idle before it is closed. Without
	// it, idle connections are bounded by readTimeout instead, which throws
	// away keep-alive for exactly the repeat clients a public feed has.
	idleTimeout = 60 * time.Second

	maxHeaderBytes = 1 << 20 // 1MB, the net/http default, stated rather than implied

	// In-flight requests get this long to finish after a signal arrives.
	// Shorter than most container stop grace periods, so the process exits on
	// its own terms rather than being killed mid-response.
	shutdownTimeout = 10 * time.Second
)

const defaultAddr = ":8080"

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("htxdev serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultAddr, "address to listen on")
	dbPath := fs.String("db", defaultDBPath, "path to the SQLite database")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("serve takes no arguments, got %q", fs.Arg(0))
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// *store.Store satisfies api.EventStore without either package knowing
	// about the other's interface. Structural typing is doing real work here:
	// internal/store has no import of internal/api and never will.
	srv := &http.Server{
		Handler:           api.New(st, api.WithLogger(log)).Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),

		// BaseContext is deliberately not set to ctx. Deriving every request's
		// context from the signal context looks correct and destroys graceful
		// shutdown: the first Ctrl-C would cancel every in-flight request
		// instead of letting it finish, which is the opposite of what Shutdown
		// is for.
	}

	// Listening separately from serving, so a bind failure is reported here
	// rather than arriving down a channel after the address has been printed.
	// It also means :0 resolves to a real port that a test can read back.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	fmt.Fprintf(stdout, "htxdev serving on http://%s\n", ln.Addr())
	fmt.Fprintf(stdout, "  GET /api/v1/events.json\n  GET /healthz\n")

	// Buffered, so this goroutine can always send and exit even if nobody is
	// selecting any more. An unbuffered channel here leaks the goroutine on
	// every shutdown.
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		// Serve returned on its own, which means something went wrong;
		// ErrServerClosed only arrives after a Shutdown we did not call.
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)

	case <-ctx.Done():
		fmt.Fprintln(stdout, "\nshutting down; finishing in-flight requests")
	}

	// context.Background, not ctx. ctx is already cancelled by the time we get
	// here, so a context derived from it would be born expired and Shutdown
	// would close every live connection immediately, which is exactly the
	// thing this is trying to avoid.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Shutdown returns the context's error when it runs out of time,
		// having already closed what was left. Worth saying so plainly,
		// because it means a client somewhere got a truncated response.
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
