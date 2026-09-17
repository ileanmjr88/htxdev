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
	"github.com/ileanmjr88/htxdev/internal/store"
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
        url: https://127.0.0.1:1/feed.ics
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
// is different and must exit non-zero.
//
// The source points at port 1 on loopback, which refuses the connection
// immediately. That keeps the test offline and fast, and it stopped being
// possible to lean on "ics has no decoder yet" when Phase 3 gave it one.
func TestSyncFailsWhenEverySourceFails(t *testing.T) {
	path := writeRegistry(t, `
groups:
  - slug: only-ics
    name: Only ICS
    sources:
      - kind: ics
        url: https://127.0.0.1:1/feed.ics
        enabled: true
`)
	// -db into a temp directory, not the default. Without it this test writes
	// an htxdev.db into the package directory and leaves it in the repo.
	var stdout, stderr bytes.Buffer
	err := runSync(t.Context(),
		[]string{"-sources", path, "-db", filepath.Join(t.TempDir(), "htxdev.db")}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "all 1 sources failed") {
		t.Fatalf("runSync() = %v, want an all-sources-failed error", err)
	}
	// The report is still the useful output. Failing must not swallow it, and
	// the row has to name the feed that failed rather than just saying one did.
	out := stdout.String()
	if !strings.Contains(out, "fail") || !strings.Contains(out, "127.0.0.1:1") {
		t.Errorf("stdout = %q, want a failing row naming the feed", out)
	}
}

func TestSyncHonoursACancelledContext(t *testing.T) {
	path := writeRegistry(t, `
groups:
  - slug: only-ics
    name: Only ICS
    sources:
      - kind: ics
        url: https://127.0.0.1:1/feed.ics
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

// The registry used by the database tests below. Every source refuses the
// connection immediately, so the run is offline and every source fails, which
// is enough to exercise persist: the registry still gets mirrored and the
// event save is a well-formed no-op.
const failingRegistry = `
groups:
  - slug: only-ics
    name: Only ICS
    url: https://example.test
    sources:
      - kind: ics
        url: https://127.0.0.1:1/feed.ics
        enabled: true
`

// The database is a committed git artifact, so "show me what this would do"
// has to be answerable without doing it.
func TestSyncDryRunWritesNothing(t *testing.T) {
	path := writeRegistry(t, failingRegistry)
	dbPath := filepath.Join(t.TempDir(), "htxdev.db")

	var stdout, stderr bytes.Buffer
	err := runSync(t.Context(), []string{"-sources", path, "-db", dbPath, "-n"}, &stdout, &stderr)
	// Every source failed, so the run still reports failure. The point is what
	// it did not write.
	if err == nil {
		t.Fatal("runSync() = nil, want the all-sources-failed error")
	}

	if _, statErr := os.Stat(dbPath); statErr == nil {
		t.Errorf("%s exists after a dry run", dbPath)
	}
	if !strings.Contains(stdout.String(), "Dry run") {
		t.Errorf("stdout = %q, want it to say the run was dry", stdout.String())
	}
}

func TestSyncCreatesAndMirrorsTheDatabase(t *testing.T) {
	path := writeRegistry(t, failingRegistry)
	dbPath := filepath.Join(t.TempDir(), "htxdev.db")

	var stdout, stderr bytes.Buffer
	// The error is expected: the one source refuses the connection. Persisting
	// still has to happen, because a run where every feed was down is exactly
	// when you want the registry mirror and the existing rows left alone.
	if err := runSync(t.Context(), []string{"-sources", path, "-db", dbPath}, &stdout, &stderr); err == nil {
		t.Fatal("runSync() = nil, want the all-sources-failed error")
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database was not created: %v", err)
	}
	// One file. No -wal, no -shm; neither is in .gitignore.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			t.Errorf("%s exists beside the database", dbPath+suffix)
		}
	}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st.Close() }()

	counts, err := st.Counts(t.Context())
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	// A failed source contributes no events, which is not the same as
	// contributing an empty list: nothing is written and nothing is cancelled.
	if counts.Total != 0 {
		t.Errorf("total = %d, want 0 events from a source that never answered", counts.Total)
	}

	if !strings.Contains(stdout.String(), "0 new, 0 updated") {
		t.Errorf("stdout = %q, want the store line", stdout.String())
	}
}

func TestReportStore(t *testing.T) {
	cases := []struct {
		name string
		in   stored
		want []string
		omit string
	}{
		{
			name: "first run, with a merge",
			in: stored{records: 76, events: 71,
				saved:  store.SaveResult{Inserted: 71},
				counts: store.Counts{Total: 71, Published: 0, Pending: 71}},
			want: []string{"76 records normalized into 71 events (5 merged)", "71 new, 0 updated"},
			omit: "promoted",
		},
		{
			name: "after a verification",
			in: stored{records: 76, events: 76,
				saved:  store.SaveResult{Updated: 76, Promoted: 19, Merged: 2},
				counts: store.Counts{Total: 76, Published: 19, Pending: 57}},
			want: []string{"0 new, 76 updated", "19 promoted out of pending", "2 rows joined"},
			// Nothing collapsed, so the parenthetical is noise.
			omit: "merged)",
		},
		{
			name: "an unattributable record is named",
			in: stored{records: 2, events: 1,
				problems: []error{errString("event \"x\": no source registered for \"https://nowhere.test/feed\"")},
				counts:   store.Counts{Total: 1, Pending: 1}},
			want: []string{"unattributable:", "nowhere.test"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			reportStore(&out, "htxdev.db", tc.in)
			got := out.String()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("output missing %q:\n%s", w, got)
				}
			}
			if tc.omit != "" && strings.Contains(got, tc.omit) {
				t.Errorf("output mentions %q, want it left out:\n%s", tc.omit, got)
			}
		})
	}
}
