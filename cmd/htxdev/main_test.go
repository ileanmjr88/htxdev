package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ileanmjr88/htxdev/internal/core"
	"github.com/ileanmjr88/htxdev/internal/fetch"
	"github.com/ileanmjr88/htxdev/internal/registry"
)

// There is no end-to-end happy-path test here, deliberately. Reaching one
// would mean either relaxing the registry's https-only rule or exporting a
// client seam out of fetch, and both are worse than the gap: the first is a
// real check against a plaintext feed URL landing in sources.yaml, and the
// second exists only for tests. fetch's own tests already drive every HTTP
// path against httptest. What is left for this package is dispatch, flag
// handling, rendering and the exit-code policy, and those are what is below.

func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantErr  string // substring; empty means no error
		wantOut  string // substring of stdout
		wantHelp bool   // usage lands on stderr
	}{
		{
			name:     "no subcommand",
			args:     nil,
			wantErr:  "no subcommand",
			wantHelp: true,
		},
		{
			name:     "unknown subcommand names the offender",
			args:     []string{"snyc"},
			wantErr:  `unknown subcommand "snyc"`,
			wantHelp: true,
		},
		{
			name:    "help succeeds and prints to stdout",
			args:    []string{"help"},
			wantOut: "htxdev sync",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(t.Context(), tt.args, &stdout, &stderr)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("run() = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("run() = %v, want error containing %q", err, tt.wantErr)
			}

			if tt.wantOut != "" && !strings.Contains(stdout.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantOut)
			}
			// Usage on a bad invocation belongs on stderr, so piping stdout
			// somewhere useful is not polluted by an error message.
			if tt.wantHelp && !strings.Contains(stderr.String(), "usage:") {
				t.Errorf("stderr = %q, want usage", stderr.String())
			}
		})
	}
}

func TestSyncRejectsBadInvocations(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown flag", []string{"-nope"}, "not defined"},
		{"positional argument", []string{"data/sources.yaml"}, "takes no arguments"},
		{"missing registry file", []string{"-sources", "does/not/exist.yaml"}, "registry"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runSync(t.Context(), tt.args, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("runSync() = %v, want error containing %q", err, tt.wantErr)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing written before the run starts", stdout.String())
			}
		})
	}
}

// A malformed registry must fail before any feed is touched, and it must say
// what is wrong. The loader reports every problem at once precisely because
// the file is edited by pull request from people who never run the code.
func TestSyncReportsRegistryProblems(t *testing.T) {
	path := writeRegistry(t, `
groups:
  - slug: broken
    name: Broken
    sources:
      - kind: rss
        url: https://example.invalid/feed
        enabled: true
`)
	var stdout, stderr bytes.Buffer
	err := runSync(t.Context(), []string{"-sources", path}, &stdout, &stderr)
	if err == nil {
		t.Fatal("runSync() = nil, want an error")
	}
	if !strings.Contains(err.Error(), `unknown kind "rss"`) {
		t.Errorf("error = %v, want it to name the unknown kind", err)
	}
}

func TestSyncFailsWhenNoRegistrySourceIsEnabled(t *testing.T) {
	path := writeRegistry(t, `
groups:
  - slug: quiet
    name: Quiet
    sources:
      - kind: ics
        url: https://example.invalid/feed.ics
        enabled: false
`)
	var stdout, stderr bytes.Buffer
	err := runSync(t.Context(), []string{"-sources", path}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "no enabled sources") {
		t.Fatalf("runSync() = %v, want a no-enabled-sources error", err)
	}
}

// The exit-code policy. One dead feed must not fail a run, because Phase 7's
// cron would go red on somebody else's outage. A run where nothing came back
// is different and must exit non-zero. Until Phase 3 an ics source fails
// without touching the network, which makes it a free way to prove it.
func TestSyncFailsWhenEverySourceFails(t *testing.T) {
	path := writeRegistry(t, `
groups:
  - slug: only-ics
    name: Only ICS
    sources:
      - kind: ics
        url: https://example.invalid/feed.ics
        enabled: true
`)
	var stdout, stderr bytes.Buffer
	err := runSync(t.Context(), []string{"-sources", path}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "all 1 sources failed") {
		t.Fatalf("runSync() = %v, want an all-sources-failed error", err)
	}
	// The report is still the useful output. Failing must not swallow it.
	if !strings.Contains(stdout.String(), "no decoder yet") {
		t.Errorf("stdout = %q, want the per-source reason", stdout.String())
	}
}

func TestSyncHonoursACancelledContext(t *testing.T) {
	path := writeRegistry(t, `
groups:
  - slug: only-ics
    name: Only ICS
    sources:
      - kind: ics
        url: https://example.invalid/feed.ics
        enabled: true
`)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var stdout, stderr bytes.Buffer
	err := runSync(ctx, []string{"-sources", path}, &stdout, &stderr)
	// Not "all 1 sources failed": an interrupted run is reported as an
	// interruption, not as a feed that happens to be broken.
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("runSync() = %v, want an interrupted error", err)
	}
}

func TestReportTallies(t *testing.T) {
	reg := &registry.Registry{
		Groups: []core.Group{
			{Slug: "verified-group", Name: "Verified", VerifiedBy: "ileanmjr88"},
			{Slug: "pending-group", Name: "Pending"},
			{Slug: "broken-group", Name: "Broken"},
		},
	}
	windowEnd := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	results := []fetch.Result{
		{
			Source:    core.Source{GroupSlug: "verified-group", Kind: core.KindTribe},
			Events:    []core.RawEvent{{Title: "One"}, {Title: "Two"}},
			WindowEnd: windowEnd,
		},
		{
			Source:    core.Source{GroupSlug: "pending-group", Kind: core.KindTribe},
			Events:    []core.RawEvent{{Title: "Three"}},
			Skipped:   []error{errString("missing global_id")},
			WindowEnd: windowEnd,
		},
		{
			Source: core.Source{GroupSlug: "broken-group", Kind: core.KindICS},
			Err:    errString("no decoder yet"),
		},
	}

	var out bytes.Buffer
	sum := report(&out, reg, results, false)

	want := syncSummary{sources: 3, ok: 2, events: 3, skipped: 1, verified: 1, windowEnd: windowEnd}
	if sum != want {
		t.Errorf("summary = %+v, want %+v", sum, want)
	}

	got := out.String()
	for _, substr := range []string{
		"unverified, would stay pending", // only the pending group earns this
		"1 skipped",
		"no decoder yet",
		"3 events from 2 of 3 sources",
		"window ends Mon 2026-11-16",
		"cancellation defers this cycle",
		"1 of 3 groups verified",
	} {
		if !strings.Contains(got, substr) {
			t.Errorf("report output missing %q\n---\n%s", substr, got)
		}
	}
	// The note has to land on the right row. Counting occurrences is not
	// enough: inverting the check to reg.IsVerified still prints exactly one
	// note, just against the wrong group. That mutation survived until this
	// assertion looked at the line rather than the document.
	if line := lineWith(t, got, "pending-group"); !strings.Contains(line, "unverified") {
		t.Errorf("pending group row = %q, want the unverified note", line)
	}
	if line := lineWith(t, got, "verified-group"); strings.Contains(line, "unverified") {
		t.Errorf("verified group row = %q, want no unverified note", line)
	}
}

// lineWith returns the single output line containing substr. It fails the test
// on zero or several, so an ambiguous match never quietly becomes a passing
// assertion about the wrong row.
func lineWith(t *testing.T, out, substr string) string {
	t.Helper()
	var found []string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, substr) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d lines containing %q, want exactly 1\n---\n%s", len(found), substr, out)
	}
	return found[0]
}

// With nothing fetched there is no window to report, and printing a zero time
// as "Mon 0001-01-01" is worse than printing nothing.
func TestReportOmitsTheWindowWhenNothingSucceeded(t *testing.T) {
	reg := &registry.Registry{Groups: []core.Group{{Slug: "broken-group", Name: "Broken"}}}
	results := []fetch.Result{{
		Source: core.Source{GroupSlug: "broken-group", Kind: core.KindICS},
		Err:    errString("no decoder yet"),
	}}

	var out bytes.Buffer
	report(&out, reg, results, false)

	if got := out.String(); strings.Contains(got, "window ends") {
		t.Errorf("report output = %q, want no window line", got)
	}
}

// Verbose output renders in America/Chicago, not UTC. An event at 01:00 UTC
// is the previous evening in Houston, so a UTC render shows the wrong day to
// the only people who will read this.
func TestReportVerboseRendersHoustonTime(t *testing.T) {
	reg := &registry.Registry{Groups: []core.Group{{Slug: "g", Name: "G"}}}
	results := []fetch.Result{{
		Source: core.Source{GroupSlug: "g", Kind: core.KindTribe, URL: "https://example.invalid/feed"},
		Events: []core.RawEvent{
			{Title: "Late Night Talk", Start: time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)},
			{Title: "Block Party", Start: time.Date(2026, 9, 19, 5, 0, 0, 0, time.UTC), AllDay: true},
		},
	}}

	var out bytes.Buffer
	report(&out, reg, results, true)
	got := out.String()

	// 2026-09-18 01:00 UTC is 2026-09-17 20:00 in Houston (CDT).
	if !strings.Contains(got, "Thu 2026-09-17 20:00") {
		t.Errorf("want the Houston rendering of 2026-09-18T01:00Z\n---\n%s", got)
	}
	if !strings.Contains(got, "Sat 2026-09-19 all day") {
		t.Errorf("want an all-day event rendered without a clock time\n---\n%s", got)
	}
	if !strings.Contains(got, "Late Night Talk") || !strings.Contains(got, "example.invalid/feed") {
		t.Errorf("verbose output missing the title or the source URL\n---\n%s", got)
	}
}

func writeRegistry(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sources.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing temp registry: %v", err)
	}
	return path
}

// errString is a stand-in for whatever fetch actually returned. The report
// only ever calls Error() on these, so there is nothing to gain from a real
// wrapped error here.
type errString string

func (e errString) Error() string { return string(e) }

// The load-bearing half of the exit policy, and the half no end-to-end test in
// this package can reach: a run where most sources failed still succeeds. Six
// of seven failing is the normal state of this repo until Phase 3, and it is
// also what Phase 7's cron will see the first time a Meetup feed 500s.
func TestVerdict(t *testing.T) {
	tests := []struct {
		name    string
		sum     syncSummary
		wantErr bool
	}{
		{"every source worked", syncSummary{sources: 7, ok: 7}, false},
		{"one of seven worked", syncSummary{sources: 7, ok: 1}, false},
		{"nothing came back", syncSummary{sources: 7, ok: 0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verdict(tt.sum)
			if (err != nil) != tt.wantErr {
				t.Fatalf("verdict(%+v) = %v, wantErr %v", tt.sum, err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "all 7 sources failed") {
				t.Errorf("verdict() = %v, want it to name the count", err)
			}
		})
	}
}
