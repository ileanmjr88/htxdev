// Command htxdev collects Houston tech events from the feeds named in the
// source registry.
//
// Usage:
//
//	htxdev sync [flags]
//
// sync is read-only: it fetches every enabled source and prints what came
// back. It gains somewhere to write in Phase 4, and a serve subcommand in
// Phase 8.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Compile the timezone database into the binary. CGO_ENABLED=0 means no
	// cgo fallback to the host's zoneinfo, and a scratch container has none
	// at all, so without this LoadLocation("America/Chicago") fails at
	// runtime in exactly the environment Phase 7's cron will run in. Roughly
	// 450KB, paid once.
	_ "time/tzdata"
)

// houston is the zone every timestamp is rendered in. Feeds are normalized to
// UTC on the way in, so this is display only, but it is not cosmetic: an
// all-day event is stored as midnight Chicago, and rendering that instant in
// UTC shows the wrong date for part of the day.
var houston = mustLoadLocation("America/Chicago")

func main() {
	// Ctrl-C at a terminal and SIGTERM from a CI runner both cancel this
	// context, which fetch threads down into every in-flight request. A
	// signal.Notify channel would work too; NotifyContext is better here
	// because a context is the plumbing every layer below already speaks.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// flag.ErrHelp is a successful request for usage, which the flag package
	// has already printed. Reporting it as an error would make `htxdev sync -h`
	// exit 1 and print a second, confusing line.
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "htxdev: %v\n", err)
		os.Exit(1)
	}
}

// run is main's testable half. Everything that can fail returns an error
// rather than calling os.Exit, and both output streams are parameters, so a
// test can drive the real command end to end and read what it printed.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no subcommand given")
	}

	switch args[0] {
	case "sync":
		return runSync(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `htxdev collects Houston tech events from a curated registry of feeds.

usage:
  htxdev sync [flags]   fetch every enabled source and print what came back
  htxdev help           this message

run "htxdev sync -h" for sync's flags.
`)
}

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		// Unreachable: time/tzdata is imported above, so the database is in
		// the binary and cannot be missing at runtime.
		panic(err)
	}
	return loc
}
