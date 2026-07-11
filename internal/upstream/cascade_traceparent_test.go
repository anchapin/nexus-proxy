package upstream

import (
	"net/http"
	"strings"
	"testing"
)

// TestCascadeForwardsTraceparent confirms the cascade's per-step POST
// carries the W3C traceparent header when WithTraceparent is passed.
// Distributed traces need every retry attempt on the same trace id
// so the receiving collector shows the full failure fan-out.
func TestCascadeForwardsTraceparent(t *testing.T) {
	const wantTP = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	var seenTP string
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		seenTP = r.Header.Get("traceparent")
		w.WriteHeader(200)
		_, _ = strings.NewReader(chatBody200).WriteTo(w)
	})
	rw := newSSERW()
	_, err := twoStepCascade().Run(rw, &http.Client{Transport: ft}, nil, WithTraceparent(wantTP))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seenTP != wantTP {
		t.Errorf("traceparent = %q, want %q", seenTP, wantTP)
	}
}

// TestCascadeOmitsTraceparentByDefault confirms no header is set on
// existing call sites that pass no options — backward-compat with
// every pre-issue-#41 test.
func TestCascadeOmitsTraceparentByDefault(t *testing.T) {
	var seenTP string
	ft := newFakeTransport()
	ft.on("http://primary.local/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		seenTP = r.Header.Get("traceparent")
		w.WriteHeader(200)
		_, _ = strings.NewReader(chatBody200).WriteTo(w)
	})
	rw := newSSERW()
	if _, err := twoStepCascade().Run(rw, &http.Client{Transport: ft}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seenTP != "" {
		t.Errorf("expected empty traceparent, got %q", seenTP)
	}
}
