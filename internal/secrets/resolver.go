// Package secrets provides pluggable credential resolution from external
// secret managers (HashiCorp Vault, AWS Secrets Manager) with transparent
// fallback to environment variables.
//
// The Resolver interface is the single abstraction used by config.Load to
// obtain credential values at boot. When NEXUS_SECRET_BACKEND is "env"
// (the default) the EnvResolver reproduces today's behaviour byte-for-byte.
// Setting it to "vault" or "awssm" routes credential lookups through the
// external store with fail-closed semantics: an unreachable configured
// backend is a hard error, never a silent fall-through to empty creds.
package secrets

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// Resolver resolves a named secret to its value. The name is the
// canonical NEXUS_* env-var key (e.g. "NEXUS_FRONTIER_API_KEY"). An
// empty string with a nil error means the secret is genuinely absent —
// callers decide whether that is fatal.
type Resolver interface {
	Resolve(name string) (string, error)
}

// BackendConfig holds the parameters for constructing a Resolver.
// Fields relevant only to a particular backend are ignored by others.
type BackendConfig struct {
	Backend     string // "env" | "vault" | "awssm"
	VaultAddr   string // e.g. "https://vault.example.com:8200"
	VaultToken  string // static token (mutually exclusive with VaultRole)
	VaultRole   string // Kubernetes auth role (requires in-cluster deployment)
	VaultPath   string // KV engine mount path (default "secret")
	AWSSMPrefix string // logical-name prefix for SM secret IDs
}

// NewResolver creates the Resolver selected by cfg.Backend.
func NewResolver(cfg BackendConfig) (Resolver, error) {
	switch cfg.Backend {
	case "", "env":
		return &EnvResolver{}, nil
	case "vault":
		return NewVaultResolver(cfg)
	case "awssm":
		return NewAWSSMSResolver(cfg)
	default:
		return nil, fmt.Errorf("secrets: unknown backend %q (want env|vault|awssm)", cfg.Backend)
	}
}

// EnvResolver reads secrets from the process environment. This is the
// default backend and reproduces pre-issue-#1173 behaviour exactly.
type EnvResolver struct{}

// Resolve returns os.Getenv(name). A missing var yields "" with no error,
// matching the historical contract of getEnv("NEXUS_*", "").
func (r *EnvResolver) Resolve(name string) (string, error) {
	return os.Getenv(name), nil
}

// secretNames is the ordered list of credential env vars the proxy resolves
// at boot. Each is tried through the active Resolver.
var secretNames = []string{
	"NEXUS_FRONTIER_API_KEY",
	"NEXUS_ZAI_API_KEY",
	"NEXUS_PROXY_API_KEY",
	"NEXUS_JUDGE_API_KEY",
	"NEXUS_COHERE_API_KEY",
}

// SecretNames returns the ordered list of credential env vars that the
// SecretStore manages. Callers can extend the list via SetSecretNames
// before calling Populate.
func SecretNames() []string {
	out := make([]string, len(secretNames))
	copy(out, secretNames)
	return out
}

// SetSecretNames replaces the managed secret name list (for testing or
// extensibility). Must be called before SecretStore.Populate.
func SetSecretNames(names []string) {
	secretNames = make([]string, len(names))
	copy(secretNames, names)
}

// SecretStore is a concurrency-safe container for resolved credentials.
// It supports periodic refresh: a background goroutine re-resolves all
// secrets and atomically swaps the live map, so rotated keys take effect
// without a restart (acceptance criterion: NEXUS_SECRET_REFRESH).
type SecretStore struct {
	resolver Resolver
	values   atomic.Pointer[map[string]string]
	stopCh   chan struct{}
	stopped  atomic.Bool
}

// NewSecretStore wraps a Resolver in a thread-safe store. The store is
// empty until Populate is called.
func NewSecretStore(r Resolver) *SecretStore {
	s := &SecretStore{
		resolver: r,
		stopCh:   make(chan struct{}),
	}
	empty := make(map[string]string)
	s.values.Store(&empty)
	return s
}

// Populate resolves every name in SecretNames() via the underlying Resolver
// and caches the results. Names that resolve to "" are stored as "" — the
// caller decides whether an empty credential is fatal.
func (s *SecretStore) Populate() error {
	return s.resolveAndSwap(SecretNames())
}

func (s *SecretStore) resolveAndSwap(names []string) error {
	next := make(map[string]string, len(names))
	for _, name := range names {
		val, err := s.resolver.Resolve(name)
		if err != nil {
			return fmt.Errorf("secrets: resolve %q: %w", name, err)
		}
		next[name] = val
	}
	s.values.Store(&next)
	return nil
}

// Get returns the cached value for name, or "" if the name was not resolved.
func (s *SecretStore) Get(name string) string {
	m := s.values.Load()
	if m == nil {
		return ""
	}
	return (*m)[name]
}

// All returns a copy of the current cached secrets.
func (s *SecretStore) All() map[string]string {
	m := s.values.Load()
	if m == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(*m))
	for k, v := range *m {
		out[k] = v
	}
	return out
}

// StartRefresh launches a background goroutine that re-resolves all secrets
// at the given interval. When the interval is zero or negative the method
// is a no-op (refresh disabled — the default). The goroutine stops on Close
// or context cancellation. Returns a cancel function that stops the loop.
//
// If a refresh cycle fails (e.g. transient Vault outage) the error is logged
// and the previous values remain in effect — a single failed poll never
// blanks the live credentials.
func (s *SecretStore) StartRefresh(interval time.Duration) context.CancelFunc {
	if interval <= 0 {
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				if err := s.resolveAndSwap(SecretNames()); err != nil {
					slog.Warn("secret refresh failed; previous values retained",
						slog.String("component", "secrets"),
						slog.String("error", err.Error()),
					)
				}
			}
		}
	}()
	return cancel
}

// Close signals the refresh goroutine to stop. Safe to call multiple times.
func (s *SecretStore) Close() {
	if s.stopped.CompareAndSwap(false, true) {
		close(s.stopCh)
	}
}
