// Package auth — tenant attribution (issue #1154).
//
// Multi-key inbound auth maps each API key to a tenant identifier that
// flows into metrics and audit logs, enabling per-team usage tracking on
// a shared proxy instance. The tenant is resolved in the auth middleware
// and placed on the request context so downstream handlers can read it
// without re-parsing the Authorization header.

package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
)

// tenantKey is the unexported context key for the resolved tenant
// identifier. Using a dedicated type avoids collisions with other
// context values.
type tenantKey struct{}

// WithTenant returns a new context carrying the tenant identifier so
// downstream handlers and metrics recorders can attribute the request.
// An empty tenant is stored as-is (legacy single-key path).
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenant)
}

// TenantFromContext extracts the tenant identifier placed on the context
// by the auth middleware. Returns "" when no tenant is set (legacy
// single-key path or auth disabled).
func TenantFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tenantKey{}).(string); ok {
		return v
	}
	return ""
}

// TenantFromRequest is a convenience wrapper that extracts the tenant
// from the request's context.
func TenantFromRequest(r *http.Request) string {
	return TenantFromContext(r.Context())
}

// CredentialEntry is a single key→tenant mapping loaded from the JSON
// keys file.
type CredentialEntry struct {
	Key    string `json:"key"`
	Tenant string `json:"tenant"`
}

// credentialItem pairs a raw key with its tenant for constant-time
// comparison. The raw key is kept so subtle.ConstantTimeCompare can
// run directly against the presented token without a pre-hash step.
type credentialItem struct {
	key    string
	tenant string
}

// CredentialSet is the runtime lookup table of API keys → tenants.
// It is safe for concurrent reads after construction. The CredentialSet
// is rebuilt on hot-reload (SIGHUP) so rotating one key does not
// invalidate others.
type CredentialSet struct {
	mu    sync.RWMutex
	items []credentialItem
	// keyHashes provides O(1) lookup by SHA-256(key) → index so the
	// hot path (valid token) is a single map probe. The constant-time
	// comparison loop runs over all items on every request regardless
	// of whether the hash matched, preventing timing side-channels.
	keyHashes map[string]int
}

// NewCredentialSet builds a CredentialSet from a slice of CredentialEntry.
// Duplicate keys keep the last entry. Empty keys are silently skipped.
func NewCredentialSet(entries []CredentialEntry) *CredentialSet {
	cs := &CredentialSet{
		items:     make([]credentialItem, 0, len(entries)),
		keyHashes: make(map[string]int, len(entries)),
	}
	for _, e := range entries {
		if strings.TrimSpace(e.Key) == "" {
			continue
		}
		h := hashKey(e.Key)
		if _, exists := cs.keyHashes[h]; exists {
			// Duplicate key — overwrite the tenant.
			cs.items[cs.keyHashes[h]] = credentialItem{key: e.Key, tenant: e.Tenant}
			continue
		}
		cs.keyHashes[h] = len(cs.items)
		cs.items = append(cs.items, credentialItem{key: e.Key, tenant: e.Tenant})
	}
	return cs
}

// Match performs a constant-time comparison of the presented token
// against every candidate key and returns the matching tenant and true
// if any candidate matched. Every candidate is compared using
// crypto/subtle.ConstantTimeCompare to avoid timing side-channels —
// there is no early-return shortcut.
func (cs *CredentialSet) Match(token string) (string, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if len(cs.items) == 0 {
		return "", false
	}
	var matchedTenant string
	matched := false
	for _, item := range cs.items {
		if subtle.ConstantTimeCompare([]byte(token), []byte(item.key)) == 1 {
			matchedTenant = item.tenant
			matched = true
			// Do NOT break — continue all comparisons so timing
			// is uniform regardless of position in the list.
		}
	}
	return matchedTenant, matched
}

// Len returns the number of registered credentials.
func (cs *CredentialSet) Len() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.items)
}

// Replace swaps the entire credential set atomically. Used during
// SIGHUP hot-reload so rotating one key does not invalidate others.
func (cs *CredentialSet) Replace(entries []CredentialEntry) {
	next := NewCredentialSet(entries)
	cs.mu.Lock()
	cs.items = next.items
	cs.keyHashes = next.keyHashes
	cs.mu.Unlock()
}

// hashKey returns the lowercase hex SHA-256 of a key string.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// LoadAPIKeysFile reads and parses the JSON keys file referenced by
// NEXUS_API_KEYS_FILE. The file must be a JSON array of
// {"key":"...","tenant":"..."} objects. An empty array returns an
// empty (zero-length) slice and no error so callers can distinguish
// "file present but empty" from "file missing". A missing file returns
// an error so the operator sees the problem at boot rather than
// silently falling back to no-auth.
func LoadAPIKeysFile(path string) ([]CredentialEntry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("auth: empty NEXUS_API_KEYS_FILE path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: cannot read NEXUS_API_KEYS_FILE %q: %w", path, err)
	}
	var entries []CredentialEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("auth: cannot unmarshal NEXUS_API_KEYS_FILE %q: %w", path, err)
	}
	// Validate: every entry must have a non-empty key. Tenant may be
	// empty (anonymous attribution).
	for i, e := range entries {
		if strings.TrimSpace(e.Key) == "" {
			return nil, fmt.Errorf("auth: NEXUS_API_KEYS_FILE entry %d has empty key", i)
		}
	}
	return entries, nil
}
