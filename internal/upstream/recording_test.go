package upstream

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestRecordingTransportRoundTripBodyRestored verifies that RoundTrip restores
// req.Body after snapshotting it, so a registered handler can still read the
// original request bytes. Regression guard for the discard-pattern hazard
// described in issue #602.
func TestRecordingTransportRoundTripBodyRestored(t *testing.T) {
	rt := NewRecordingTransport()

	const payload = `{"prompt":"hello nexus"}`
	var seen string
	rt.On(http.MethodPost, "http://test.local/echo", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("handler ReadAll: %v", err)
		}
		seen = string(b)
		w.WriteHeader(http.StatusOK)
	})

	req, err := http.NewRequest(http.MethodPost, "http://test.local/echo", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.ContentLength = int64(len(payload))

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if seen != payload {
		t.Fatalf("handler saw body %q, want %q (body was not restored)", seen, payload)
	}
}

// TestRecordingTransportOnRegistersHandler exercises the On/OnAny/Calls
// surface with a real round-trip: a matched handler runs, an unmatched
// request falls through to OnAny, and Calls records both.
func TestRecordingTransportOnRegistersHandler(t *testing.T) {
	rt := NewRecordingTransport()

	matched := false
	rt.On(http.MethodGet, "http://match.local/x", func(w http.ResponseWriter, _ *http.Request) {
		matched = true
		w.WriteHeader(http.StatusTeapot)
	})

	fallbackHit := false
	rt.OnAny(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHit = true
		w.WriteHeader(http.StatusNotFound)
	})

	// Matched request.
	reqMatch, _ := http.NewRequest(http.MethodGet, "http://match.local/x", nil)
	resp, err := rt.RoundTrip(reqMatch)
	if err != nil {
		t.Fatalf("RoundTrip match: %v", err)
	}
	resp.Body.Close()
	if !matched {
		t.Fatal("matched handler did not run")
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("matched status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}

	// Unmatched request → fallback.
	reqOther, _ := http.NewRequest(http.MethodGet, "http://other.local/y", nil)
	resp2, err := rt.RoundTrip(reqOther)
	if err != nil {
		t.Fatalf("RoundTrip fallback: %v", err)
	}
	resp2.Body.Close()
	if !fallbackHit {
		t.Fatal("fallback handler did not run")
	}
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("fallback status = %d, want %d", resp2.StatusCode, http.StatusNotFound)
	}

	calls := rt.Calls()
	if len(calls) != 2 {
		t.Fatalf("Calls len = %d, want 2", len(calls))
	}
	if calls[0].URL != "http://match.local/x" || calls[1].URL != "http://other.local/y" {
		t.Fatalf("Calls URLs = %q, %q", calls[0].URL, calls[1].URL)
	}

	// Calls must return a defensive copy: mutating it must not affect the
	// transport's internal state.
	calls[0].URL = "mutated"
	again := rt.Calls()
	if again[0].URL != "http://match.local/x" {
		t.Fatalf("Calls returned a shared slice; got %q after mutation", again[0].URL)
	}
}

// TestRecordingTransportNoHandlerDefaultResponse covers the path where no
// handler and no fallback are registered: RoundTrip must return a synthetic
// 200 response rather than panicking.
func TestRecordingTransportNoHandlerDefaultResponse(t *testing.T) {
	rt := NewRecordingTransport()
	req, _ := http.NewRequest(http.MethodGet, "http://ghost.local/z", nil)

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "no handler for") {
		t.Fatalf("default body = %q, want it to mention missing handler", string(body))
	}
}

// TestReadAndRestoreBody covers the three branches of readAndRestoreBody:
// nil body, normal body, and a body larger than the 1 MiB cap.
func TestReadAndRestoreBody(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "http://x/", nil)
		req.Body = nil
		out, err := readAndRestoreBody(req)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if out != nil {
			t.Fatalf("out = %v, want nil", out)
		}
	})

	t.Run("restores small body", func(t *testing.T) {
		const payload = "snap me"
		req, _ := http.NewRequest(http.MethodPost, "http://x/", strings.NewReader(payload))
		out, err := readAndRestoreBody(req)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if string(out) != payload {
			t.Fatalf("snapshot = %q, want %q", string(out), payload)
		}
		rest, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("ReadAll after restore: %v", err)
		}
		if string(rest) != payload {
			t.Fatalf("restored body = %q, want %q", string(rest), payload)
		}
	})

	t.Run("caps at 1MiB", func(t *testing.T) {
		// 2 MiB payload: readAndRestoreBody must stop reading at 1 MiB.
		big := bytes.Repeat([]byte{'a'}, 2<<20)
		req, _ := http.NewRequest(http.MethodPost, "http://x/", bytes.NewReader(big))
		out, err := readAndRestoreBody(req)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		const max = 1 << 20
		if len(out) != max {
			t.Fatalf("snapshot len = %d, want capped at %d", len(out), max)
		}
	})
}
