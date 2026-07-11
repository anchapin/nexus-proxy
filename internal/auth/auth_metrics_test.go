package auth

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// counter is a tiny thread-safe tally that doubles as the observer
// for issue #70 tests. It records every outcome the middleware
// reports so tests can assert the observer fires on the expected
// decision path.
type counter struct {
	accepted        atomic.Uint32
	rejectedInvalid atomic.Uint32
	rejectedMissing atomic.Uint32
	exempt          atomic.Uint32
}

func (c *counter) observe(outcome string) {
	switch outcome {
	case "accepted":
		c.accepted.Add(1)
	case "rejected_invalid":
		c.rejectedInvalid.Add(1)
	case "rejected_missing":
		c.rejectedMissing.Add(1)
	case "exempt":
		c.exempt.Add(1)
	}
}

// TestMiddlewareObserverAccepted (issue #70) verifies the observer
// hook fires exactly once per accepted request with outcome="accepted".
func TestMiddlewareObserverAccepted(t *testing.T) {
	c := &counter{}
	h := MiddlewareWithObserver([]string{"sk-secret"}, nil, c.observe)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if got := c.accepted.Load(); got != 1 {
		t.Errorf("accepted = %d, want 1", got)
	}
	if got := c.rejectedInvalid.Load() + c.rejectedMissing.Load(); got != 0 {
		t.Errorf("rejected incremented on accept path: %d", got)
	}
	if got := c.exempt.Load(); got != 0 {
		t.Errorf("exempt fired on non-exempt path: %d", got)
	}
}

// TestMiddlewareObserverRejectedInvalid (issue #70) verifies a
// request that presents a wrong key bumps the "rejected_invalid"
// outcome (not "rejected_missing").
func TestMiddlewareObserverRejectedInvalid(t *testing.T) {
	c := &counter{}
	h := MiddlewareWithObserver([]string{"sk-secret"}, nil, c.observe)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if got := c.rejectedInvalid.Load(); got != 1 {
		t.Errorf("rejected_invalid = %d, want 1", got)
	}
	if got := c.rejectedMissing.Load(); got != 0 {
		t.Errorf("rejected_missing should not fire on a presented credential: %d", got)
	}
}

// TestMiddlewareObserverRejectedMissing (issue #70) verifies a
// request with no credential at all bumps "rejected_missing"
// (distinguishing "presented the wrong key" from "presented no key").
func TestMiddlewareObserverRejectedMissing(t *testing.T) {
	c := &counter{}
	h := MiddlewareWithObserver([]string{"sk-secret"}, nil, c.observe)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	if got := c.rejectedMissing.Load(); got != 1 {
		t.Errorf("rejected_missing = %d, want 1", got)
	}
	if got := c.rejectedInvalid.Load(); got != 0 {
		t.Errorf("rejected_invalid should not fire on a missing credential: %d", got)
	}
}

// TestMiddlewareObserverExempt (issue #70) verifies an exempt path
// bumps the exempt outcome and never increments accept/reject.
func TestMiddlewareObserverExempt(t *testing.T) {
	c := &counter{}
	exempt := func(r *http.Request) bool { return r.URL.Path == "/healthz" }
	h := MiddlewareWithObserver([]string{"sk-secret"}, exempt, c.observe)(okHandler())

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if got := c.exempt.Load(); got != 1 {
		t.Errorf("exempt = %d, want 1", got)
	}
	if got := c.accepted.Load() + c.rejectedInvalid.Load() + c.rejectedMissing.Load(); got != 0 {
		t.Errorf("non-exempt outcomes fired on exempt path: %d", got)
	}
}

// TestMiddlewareObserverNilSafe (issue #70) verifies a nil observer
// does not panic and the middleware still enforces auth — the
// convenience Middleware wrapper is exercised in main.go via the no-
// observer overload.
func TestMiddlewareObserverNilSafe(t *testing.T) {
	// Should not panic with a nil observer.
	h := MiddlewareWithObserver([]string{"sk-secret"}, nil, nil)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("nil observer path: status = %d, want 200", rec.Code)
	}

	// Wrong key still rejected.
	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("nil observer path: wrong-key status = %d, want 401", rec.Code)
	}
}

// TestMiddlewareDisabledKeysSkipsObserver (issue #70) ensures the
// no-keys-configured identity pass-through does NOT invoke the
// observer: an observability hook must not record a decision when
// there is no decision to record.
func TestMiddlewareDisabledKeysSkipsObserver(t *testing.T) {
	c := &counter{}
	h := MiddlewareWithObserver(nil, nil, c.observe)(okHandler())

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no keys = pass-through)", rec.Code)
	}

	total := c.accepted.Load() + c.rejectedInvalid.Load() + c.rejectedMissing.Load() + c.exempt.Load()
	if total != 0 {
		t.Errorf("observer fired on pass-through middleware: total = %d, want 0", total)
	}
}
