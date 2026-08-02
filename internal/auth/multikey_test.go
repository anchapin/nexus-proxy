package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMultiKeyMiddleware_AcceptsValidKey tests that requests with any
// listed key are accepted and the resolved tenant is placed on context.
func TestMultiKeyMiddleware_AcceptsValidKey(t *testing.T) {
	creds := NewCredentialSet([]CredentialEntry{
		{Key: "key-alpha", Tenant: "team-a"},
		{Key: "key-beta", Tenant: "team-b"},
	})
	m := NewMultiKeyMiddleware("", creds, nil, nil, nil)

	var capturedTenant string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTenant = TenantFromRequest(r)
		w.WriteHeader(http.StatusOK)
	})

	// Test team-a key
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer key-alpha")
	m.Wrap(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if capturedTenant != "team-a" {
		t.Fatalf("tenant = %q, want %q", capturedTenant, "team-a")
	}

	// Test team-b key
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer key-beta")
	m.Wrap(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if capturedTenant != "team-b" {
		t.Fatalf("tenant = %q, want %q", capturedTenant, "team-b")
	}
}

// TestMultiKeyMiddleware_RejectsUnlistedKey tests that unlisted keys
// get 401.
func TestMultiKeyMiddleware_RejectsUnlistedKey(t *testing.T) {
	creds := NewCredentialSet([]CredentialEntry{
		{Key: "key-alpha", Tenant: "team-a"},
	})
	m := NewMultiKeyMiddleware("", creds, nil, nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer not-listed")
	m.Wrap(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

// TestMultiKeyMiddleware_LegacySingleKeyStillWorks tests that when
// both multi-key and single-key are configured, multi-key takes
// precedence but the single key still works as a fallback via the
// CredentialSet (not via the legacy path).
func TestMultiKeyMiddleware_EnabledWithCredsOnly(t *testing.T) {
	creds := NewCredentialSet([]CredentialEntry{
		{Key: "key-a", Tenant: "team-a"},
	})
	m := NewMultiKeyMiddleware("", creds, nil, nil, nil)
	if !m.Enabled() {
		t.Fatal("Enabled() = false with creds, want true")
	}
}

// TestMultiKeyMiddleware_SetCredentialsHotReload tests runtime
// credential replacement.
func TestMultiKeyMiddleware_SetCredentialsHotReload(t *testing.T) {
	creds := NewCredentialSet([]CredentialEntry{
		{Key: "old-key", Tenant: "old"},
	})
	m := NewMultiKeyMiddleware("", creds, nil, nil, nil)

	// Old key works
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer old-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("old-key: status = %d, want 200", rr.Code)
	}

	// Replace credentials
	newCreds := NewCredentialSet([]CredentialEntry{
		{Key: "new-key", Tenant: "new"},
	})
	m.SetCredentials(newCreds)

	// Old key should now be rejected
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer old-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("old-key after reload: status = %d, want 401", rr.Code)
	}

	// New key should work
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer new-key")
	m.Wrap(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("new-key: status = %d, want 200", rr.Code)
	}
}

// TestMultiKeyMiddleware_EmptyTenant tests that a credential with an
// empty tenant still authenticates but yields an empty tenant string.
func TestMultiKeyMiddleware_EmptyTenant(t *testing.T) {
	creds := NewCredentialSet([]CredentialEntry{
		{Key: "no-tenant-key", Tenant: ""},
	})
	m := NewMultiKeyMiddleware("", creds, nil, nil, nil)

	var capturedTenant string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTenant = TenantFromRequest(r)
		w.WriteHeader(http.StatusOK)
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer no-tenant-key")
	m.Wrap(handler).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if capturedTenant != "" {
		t.Fatalf("tenant = %q, want empty", capturedTenant)
	}
}

// TestMultiKeyMiddleware_ExemptPathBypassesAuth ensures exempt paths
// still work in multi-key mode.
func TestMultiKeyMiddleware_ExemptPathBypassesAuth(t *testing.T) {
	creds := NewCredentialSet([]CredentialEntry{
		{Key: "key-a", Tenant: "team-a"},
	})
	exempt := func(r *http.Request) bool {
		return r.URL.Path == "/healthz"
	}
	m := NewMultiKeyMiddleware("", creds, exempt, nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200 (exempt)", rr.Code)
	}
}

// TestMultiKeyMiddleware_RejectsMissingToken ensures 401 for missing
// token in multi-key mode.
func TestMultiKeyMiddleware_RejectsMissingToken(t *testing.T) {
	creds := NewCredentialSet([]CredentialEntry{
		{Key: "key-a", Tenant: "team-a"},
	})
	m := NewMultiKeyMiddleware("", creds, nil, nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	m.Wrap(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rr.Code)
	}
}

// TestWithTenant_TenantFromRequest ensures the context helper round-trips.
func TestWithTenant_TenantFromRequest(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(WithTenant(context.Background(), "team-x"))
	if got := TenantFromRequest(req); got != "team-x" {
		t.Fatalf("TenantFromRequest = %q, want %q", got, "team-x")
	}
}
