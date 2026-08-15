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
	"testing"
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
