package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadRejects(t *testing.T, body string) *Rejects {
	t.Helper()
	r, err := LoadRejects(strings.NewReader(body))
	if err != nil {
		t.Fatalf("LoadRejects: %v", err)
	}
	return r
}

func TestLoadRejects(t *testing.T) {
	r := loadRejects(t, `
rejects:
  - fingerprint: "ics:abc@google.com"
    reason: Personal appointment on a shared group calendar
    rejected_by: ileanmjr88
    rejected_at: 2026-09-17
  - fingerprint: "tribe:iondistrict.com?id=1"
    reason: Investor programming
`)
	if r.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", r.Len())
	}

	got, ok := r.Rejected("ics:abc@google.com")
	if !ok {
		t.Fatal("signed reject not found")
	}
	if got.RejectedBy != "ileanmjr88" {
		t.Errorf("rejectedBy = %q", got.RejectedBy)
	}
	if want := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC); !got.RejectedAt.Equal(want) {
		t.Errorf("rejectedAt = %s, want %s", got.RejectedAt, want)
	}

	// The unsigned one is still a reject. Both blank fields fail closed,
	// because both point at "do not publish": erring the other way would mean
	// a pull request that forgot a name silently republished what it was
	// written to hide.
	if _, ok := r.Rejected("tribe:iondistrict.com?id=1"); !ok {
		t.Error("an unsigned reject was not applied; blank rejected_by must still reject")
	}

	if _, ok := r.Rejected("ics:never-listed@google.com"); ok {
		t.Error("an unlisted fingerprint was reported as rejected")
	}
}

// A project with nothing to reject is a legitimate state, and so is a fresh
// clone. Failing the whole sync over an absent filter would be worse than
// running without it; the count is reported on every run instead.
func TestLoadRejectsFileMissingIsNotAnError(t *testing.T) {
	r, err := LoadRejectsFile(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("LoadRejectsFile: %v", err)
	}
	if r.Len() != 0 {
		t.Errorf("Len() = %d, want 0", r.Len())
	}
	if _, ok := r.Rejected("anything"); ok {
		t.Error("an empty list rejected something")
	}
}

// A nil *Rejects answers no, so a caller with nothing loaded needs no special
// case and cannot panic on one.
func TestNilRejectsAnswersNo(t *testing.T) {
	var r *Rejects
	if r.Len() != 0 {
		t.Errorf("Len() = %d, want 0", r.Len())
	}
	if _, ok := r.Rejected("anything"); ok {
		t.Error("a nil list rejected something")
	}
}

func TestLoadRejectsProblems(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "no fingerprint",
			body:    "rejects:\n  - reason: Something\n",
			wantErr: "missing fingerprint",
		},
		{
			// A reject with no reason is a decision nobody can review later,
			// which defeats the point of putting it under version control.
			name:    "no reason",
			body:    "rejects:\n  - fingerprint: \"ics:a@b\"\n",
			wantErr: "missing reason",
		},
		{
			name:    "listed twice",
			body:    "rejects:\n  - fingerprint: \"ics:a@b\"\n    reason: One\n  - fingerprint: \"ics:a@b\"\n    reason: Two\n",
			wantErr: "listed twice",
		},
		{
			name:    "bad date",
			body:    "rejects:\n  - fingerprint: \"ics:a@b\"\n    reason: One\n    rejected_at: 17-09-2026\n",
			wantErr: "not YYYY-MM-DD",
		},
		{
			name:    "signed but undated",
			body:    "rejects:\n  - fingerprint: \"ics:a@b\"\n    reason: One\n    rejected_by: ileanmjr88\n",
			wantErr: "rejected_at is blank",
		},
		{
			// Strict, for the same reason the registry is: this file is edited
			// by pull request by people who never run the code, and a
			// misspelled key that silently does nothing is worse than an error.
			name:    "misspelled key",
			body:    "rejects:\n  - fingerprint: \"ics:a@b\"\n    resaon: typo\n",
			wantErr: "resaon",
		},
		{
			name:    "every problem at once",
			body:    "rejects:\n  - reason: One\n  - fingerprint: \"ics:a@b\"\n",
			wantErr: "2 problem(s)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRejects(strings.NewReader(tc.body))
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadRejectsEmptyFile(t *testing.T) {
	for _, body := range []string{"", "rejects:\n", "# only a comment\n"} {
		r, err := LoadRejects(strings.NewReader(body))
		if err != nil {
			t.Fatalf("LoadRejects(%q): %v", body, err)
		}
		if r.Len() != 0 {
			t.Errorf("LoadRejects(%q).Len() = %d, want 0", body, r.Len())
		}
	}
}

// The committed file has to stay valid, the same way TestRealSourcesYAMLIsValid
// guards the registry.
func TestRealRejectsYAMLIsValid(t *testing.T) {
	r, err := LoadRejectsFile("../../data/rejects.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if r.Len() == 0 {
		t.Skip("no rejects listed yet")
	}
	for _, fp := range []string{"ics:6kqj0pb66kqmab9pc8s66b9k6gpm4b9p71ij2bb269hmad1n6ks3gdj368@google.com"} {
		rj, ok := r.Rejected(fp)
		if !ok {
			t.Errorf("%q is not in the reject list", fp)
			continue
		}
		if rj.Reason == "" {
			t.Errorf("%q has no reason", fp)
		}
		// The reasons are generic on purpose. A title-keyed list would contain
		// the titles being rejected, which for this entry means writing
		// somebody's private calendar entry into a file in order to stop
		// publishing somebody's private calendar entry.
		for _, leak := range []string{"Family", "Dental", "visit"} {
			if strings.Contains(rj.Reason, leak) {
				t.Errorf("reason %q repeats the private text it exists to suppress", rj.Reason)
			}
		}
	}

	// And the file itself must not contain them either.
	body, err := os.ReadFile("../../data/rejects.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"Family visit", "Dental"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("data/rejects.yaml contains %q; reject by fingerprint, not by title", leak)
		}
	}
}
