package registry

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Reject is one event the project has decided not to publish.
type Reject struct {
	Fingerprint string
	Reason      string
	// Blank means proposed rather than applied, exactly like Group.VerifiedBy.
	// Filling it in is the act, and the commit author is the provenance.
	RejectedBy string
	RejectedAt time.Time
}

// Rejects is the parsed contents of rejects.yaml, indexed for lookup.
type Rejects struct {
	byFingerprint map[string]Reject
}

type wireRejectFile struct {
	Rejects []wireReject `yaml:"rejects"`
}

type wireReject struct {
	Fingerprint string `yaml:"fingerprint"`
	Reason      string `yaml:"reason"`
	RejectedBy  string `yaml:"rejected_by"`
	RejectedAt  string `yaml:"rejected_at"`
}

// LoadRejectsFile reads rejects.yaml at path.
//
// A missing file is not an error. A project with nothing to reject is a
// legitimate state and so is a fresh clone, and failing the whole sync over an
// absent filter would be worse than running without it. The count is reported
// on every run instead, so "zero rejects loaded" is visible rather than silent.
func LoadRejectsFile(path string) (*Rejects, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Rejects{byFingerprint: map[string]Reject{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rejects: %w", err)
	}
	defer func() { _ = f.Close() }()

	rej, err := LoadRejects(f)
	if err != nil {
		return nil, fmt.Errorf("rejects %s: %w", path, err)
	}
	return rej, nil
}

// LoadRejects parses and validates a reject list. Split from LoadRejectsFile so
// tests can hand it a string and stay off the filesystem, matching Load.
func LoadRejects(r io.Reader) (*Rejects, error) {
	var wf wireRejectFile
	// Strict, for the same reason the registry is: this file is edited by pull
	// request by people who never run the code, and a misspelled key that
	// silently does nothing is worse than an error with a line number.
	if err := yaml.NewDecoder(r, yaml.Strict()).Decode(&wf); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	out := &Rejects{byFingerprint: make(map[string]Reject, len(wf.Rejects))}
	var problems []string

	for i, w := range wf.Rejects {
		where := fmt.Sprintf("reject %d", i)
		if w.Fingerprint == "" {
			problems = append(problems, where+": missing fingerprint")
			continue
		}
		where = "reject " + w.Fingerprint
		if _, dup := out.byFingerprint[w.Fingerprint]; dup {
			problems = append(problems, where+": listed twice")
			continue
		}
		// A reject with no reason is a decision nobody can review later, which
		// defeats the point of putting it in a file under version control.
		if w.Reason == "" {
			problems = append(problems, where+": missing reason")
		}

		var at time.Time
		if w.RejectedAt != "" {
			t, err := time.Parse(time.DateOnly, w.RejectedAt)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: rejected_at %q is not YYYY-MM-DD", where, w.RejectedAt))
			} else {
				at = t
			}
		}
		if w.RejectedBy != "" && w.RejectedAt == "" {
			problems = append(problems, where+": rejected_by is set but rejected_at is blank")
		}

		out.byFingerprint[w.Fingerprint] = Reject{
			Fingerprint: w.Fingerprint,
			Reason:      w.Reason,
			RejectedBy:  w.RejectedBy,
			RejectedAt:  at,
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("%d problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	return out, nil
}

// Rejected reports whether this fingerprint is on the list.
//
// A nil *Rejects answers no, so a caller with nothing loaded needs no special
// case. Every fingerprint is checked, not only applied ones: see Applied.
func (r *Rejects) Rejected(fingerprint string) (Reject, bool) {
	if r == nil {
		return Reject{}, false
	}
	rj, ok := r.byFingerprint[fingerprint]
	return rj, ok
}

// Len is how many entries loaded, reported on every sync so an absent or empty
// file is visible rather than silent.
func (r *Rejects) Len() int {
	if r == nil {
		return 0
	}
	return len(r.byFingerprint)
}
