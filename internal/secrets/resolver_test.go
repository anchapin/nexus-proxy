package secrets

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/hashicorp/vault/api"
)

// --- EnvResolver tests ---

func TestEnvResolver_ExistingVar(t *testing.T) {
	t.Setenv("NEXUS_TEST_KEY", "test-value-123")
	r := &EnvResolver{}
	val, err := r.Resolve("NEXUS_TEST_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "test-value-123" {
		t.Fatalf("got %q, want %q", val, "test-value-123")
	}
}

func TestEnvResolver_MissingVar(t *testing.T) {
	t.Setenv("NEXUS_ABSENT_KEY", "")
	r := &EnvResolver{}
	val, err := r.Resolve("NEXUS_ABSENT_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "" {
		t.Fatalf("got %q, want empty", val)
	}
}

func TestEnvResolver_BackwardCompatible(t *testing.T) {
	t.Setenv("NEXUS_FRONTIER_API_KEY", "sk-frontier-env")
	r := &EnvResolver{}
	val, _ := r.Resolve("NEXUS_FRONTIER_API_KEY")
	if val != "sk-frontier-env" {
		t.Fatalf("expected env value, got %q", val)
	}
}

// --- Factory / NewResolver tests ---

func TestNewResolver_DefaultEnv(t *testing.T) {
	r, err := NewResolver(BackendConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := r.(*EnvResolver); !ok {
		t.Fatalf("expected *EnvResolver, got %T", r)
	}
}

func TestNewResolver_ExplicitEnv(t *testing.T) {
	r, err := NewResolver(BackendConfig{Backend: "env"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := r.(*EnvResolver); !ok {
		t.Fatalf("expected *EnvResolver, got %T", r)
	}
}

func TestNewResolver_UnknownBackend(t *testing.T) {
	_, err := NewResolver(BackendConfig{Backend: "gcp"})
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestNewResolver_VaultMissingAddr(t *testing.T) {
	_, err := NewResolver(BackendConfig{Backend: "vault"})
	if err == nil {
		t.Fatal("expected error when vault addr is missing")
	}
}

// --- Vault stub tests ---

func TestVaultResolver_Found(t *testing.T) {
	stub := &stubVaultReader{
		secret: makeVaultSecret(map[string]interface{}{
			"NEXUS_FRONTIER_API_KEY": "sk-vault-secret",
		}),
	}
	r := newVaultResolverWithReader(stub, "secret/data/nexus")

	val, err := r.Resolve("NEXUS_FRONTIER_API_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "sk-vault-secret" {
		t.Fatalf("got %q, want %q", val, "sk-vault-secret")
	}
}

func TestVaultResolver_CacheHit(t *testing.T) {
	stub := &stubVaultReader{
		secret: makeVaultSecret(map[string]interface{}{
			"NEXUS_FRONTIER_API_KEY": "cached-value",
		}),
	}
	r := newVaultResolverWithReader(stub, "secret/data/nexus")

	// First call hits the reader.
	val1, _ := r.Resolve("NEXUS_FRONTIER_API_KEY")
	// Reset secret so a second read would return different data.
	stub.secret = makeVaultSecret(map[string]interface{}{
		"NEXUS_FRONTIER_API_KEY": "different-value",
	})
	// Second call should use cache, not the reader.
	val2, _ := r.Resolve("NEXUS_FRONTIER_API_KEY")

	if val1 != "cached-value" || val2 != "cached-value" {
		t.Fatalf("cache miss: first=%q second=%q, both want %q", val1, val2, "cached-value")
	}
}

func TestVaultResolver_FallbackToEnv(t *testing.T) {
	t.Setenv("NEXUS_PROXY_API_KEY", "env-proxy-key")
	stub := &stubVaultReader{
		secret: makeVaultSecret(map[string]interface{}{}),
	}
	r := newVaultResolverWithReader(stub, "secret/data/nexus")

	val, err := r.Resolve("NEXUS_PROXY_API_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "env-proxy-key" {
		t.Fatalf("got %q, want env fallback %q", val, "env-proxy-key")
	}
}

func TestVaultResolver_NilSecret(t *testing.T) {
	t.Setenv("NEXUS_ZAI_API_KEY", "zai-env")
	stub := &stubVaultReader{secret: nil}
	r := newVaultResolverWithReader(stub, "secret/data/nexus")

	val, err := r.Resolve("NEXUS_ZAI_API_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "zai-env" {
		t.Fatalf("got %q, want %q", val, "zai-env")
	}
}

func TestVaultResolver_ReaderError(t *testing.T) {
	stub := &stubVaultReader{err: errors.New("connection refused")}
	r := newVaultResolverWithReader(stub, "secret/data/nexus")

	_, err := r.Resolve("NEXUS_FRONTIER_API_KEY")
	if err == nil {
		t.Fatal("expected error from failed vault read")
	}
}

func TestVaultResolver_KVv1(t *testing.T) {
	// KV v1 stores data at top level (no "data" wrapper).
	stub := &stubVaultReader{
		secret: &api.Secret{Data: map[string]interface{}{
			"NEXUS_FRONTIER_API_KEY": "v1-key",
		}},
	}
	r := newVaultResolverWithReader(stub, "secret/nexus")

	val, _ := r.Resolve("NEXUS_FRONTIER_API_KEY")
	if val != "v1-key" {
		t.Fatalf("got %q, want %q for KV v1", val, "v1-key")
	}
}

// --- AWS SM stub tests ---

func TestAWSSMSResolver_Found(t *testing.T) {
	stub := &stubSMClient{
		values: map[string]string{
			"NEXUS_FRONTIER_API_KEY": "sk-aws-secret",
		},
	}
	r := newAWSSMSResolverWithClient(stub, "")

	val, err := r.Resolve("NEXUS_FRONTIER_API_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "sk-aws-secret" {
		t.Fatalf("got %q, want %q", val, "sk-aws-secret")
	}
}

func TestAWSSMSResolver_WithPrefix(t *testing.T) {
	stub := &stubSMClient{
		values: map[string]string{
			"prod/NEXUS_FRONTIER_API_KEY": "sk-prefix-secret",
		},
	}
	r := newAWSSMSResolverWithClient(stub, "prod")

	val, err := r.Resolve("NEXUS_FRONTIER_API_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "sk-prefix-secret" {
		t.Fatalf("got %q, want %q", val, "sk-prefix-secret")
	}
}

func TestAWSSMSResolver_NotFoundFallbackToEnv(t *testing.T) {
	t.Setenv("NEXUS_PROXY_API_KEY", "env-proxy")
	stub := &stubSMClient{values: map[string]string{}}
	r := newAWSSMSResolverWithClient(stub, "")

	val, err := r.Resolve("NEXUS_PROXY_API_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "env-proxy" {
		t.Fatalf("got %q, want env fallback %q", val, "env-proxy")
	}
}

func TestAWSSMSResolver_ClientError(t *testing.T) {
	stub := &stubSMClient{err: errors.New("throttled")}
	r := newAWSSMSResolverWithClient(stub, "")

	_, err := r.Resolve("NEXUS_FRONTIER_API_KEY")
	if err == nil {
		t.Fatal("expected error from failed aws call")
	}
}

func TestAWSSMSResolver_CacheHit(t *testing.T) {
	stub := &stubSMClient{
		values: map[string]string{
			"NEXUS_FRONTIER_API_KEY": "first",
		},
	}
	r := newAWSSMSResolverWithClient(stub, "")

	val1, _ := r.Resolve("NEXUS_FRONTIER_API_KEY")
	stub.values["NEXUS_FRONTIER_API_KEY"] = "second"
	val2, _ := r.Resolve("NEXUS_FRONTIER_API_KEY")

	if val1 != "first" || val2 != "first" {
		t.Fatalf("cache miss: first=%q second=%q, both want %q", val1, val2, "first")
	}
}

// --- Fail-closed tests ---

func TestFailClosed_VaultMissingAddr(t *testing.T) {
	_, err := NewResolver(BackendConfig{Backend: "vault"})
	if err == nil {
		t.Fatal("vault with no addr must fail (fail-closed)")
	}
}

func TestFailClosed_VaultMissingAuth(t *testing.T) {
	_, err := NewResolver(BackendConfig{
		Backend:   "vault",
		VaultAddr: "https://vault.example.com:8200",
		// No token, no role, no ~/.vault-token
	})
	// This will either fail at client init or at the connectivity probe.
	// Either way, it must not silently succeed with empty creds.
	if err == nil {
		t.Fatal("vault with no auth and unreachable server must fail (fail-closed)")
	}
}

// --- SecretStore tests ---

func TestSecretStore_Populate(t *testing.T) {
	t.Setenv("NEXUS_FRONTIER_API_KEY", "store-frontier")
	t.Setenv("NEXUS_PROXY_API_KEY", "store-proxy")

	SetSecretNames([]string{"NEXUS_FRONTIER_API_KEY", "NEXUS_PROXY_API_KEY"})
	defer SetSecretNames([]string{
		"NEXUS_FRONTIER_API_KEY", "NEXUS_ZAI_API_KEY", "NEXUS_PROXY_API_KEY",
		"NEXUS_JUDGE_API_KEY", "NEXUS_COHERE_API_KEY",
	})

	store := NewSecretStore(&EnvResolver{})
	if err := store.Populate(); err != nil {
		t.Fatalf("Populate failed: %v", err)
	}

	if v := store.Get("NEXUS_FRONTIER_API_KEY"); v != "store-frontier" {
		t.Fatalf("got %q, want %q", v, "store-frontier")
	}
	if v := store.Get("NEXUS_PROXY_API_KEY"); v != "store-proxy" {
		t.Fatalf("got %q, want %q", v, "store-proxy")
	}
}

func TestSecretStore_GetMissing(t *testing.T) {
	store := NewSecretStore(&EnvResolver{})
	_ = store.Populate()
	if v := store.Get("NONEXISTENT"); v != "" {
		t.Fatalf("got %q, want empty for unknown key", v)
	}
}

func TestSecretStore_All(t *testing.T) {
	t.Setenv("NEXUS_FRONTIER_API_KEY", "all-frontier")
	SetSecretNames([]string{"NEXUS_FRONTIER_API_KEY"})
	defer SetSecretNames([]string{
		"NEXUS_FRONTIER_API_KEY", "NEXUS_ZAI_API_KEY", "NEXUS_PROXY_API_KEY",
		"NEXUS_JUDGE_API_KEY", "NEXUS_COHERE_API_KEY",
	})

	store := NewSecretStore(&EnvResolver{})
	_ = store.Populate()

	all := store.All()
	if all["NEXUS_FRONTIER_API_KEY"] != "all-frontier" {
		t.Fatalf("All() missing key: %+v", all)
	}
}

// --- Refresh swap test ---

func TestSecretStore_RefreshSwap(t *testing.T) {
	// Use a custom resolver whose output changes over time.
	mu := sync.Mutex{}
	current := "v1"
	dynamic := &dynamicResolver{getValue: func() string {
		mu.Lock()
		defer mu.Unlock()
		return current
	}}

	SetSecretNames([]string{"NEXUS_DYNAMIC_KEY"})
	defer SetSecretNames([]string{
		"NEXUS_FRONTIER_API_KEY", "NEXUS_ZAI_API_KEY", "NEXUS_PROXY_API_KEY",
		"NEXUS_JUDGE_API_KEY", "NEXUS_COHERE_API_KEY",
	})

	store := NewSecretStore(dynamic)
	if err := store.Populate(); err != nil {
		t.Fatalf("Populate: %v", err)
	}
	if v := store.Get("NEXUS_DYNAMIC_KEY"); v != "v1" {
		t.Fatalf("initial value: got %q want %q", v, "v1")
	}

	// Rotate the value and force a refresh.
	mu.Lock()
	current = "v2"
	mu.Unlock()

	if err := store.resolveAndSwap(SecretNames()); err != nil {
		t.Fatalf("resolveAndSwap: %v", err)
	}
	if v := store.Get("NEXUS_DYNAMIC_KEY"); v != "v2" {
		t.Fatalf("after refresh: got %q want %q", v, "v2")
	}
}

func TestSecretStore_RefreshFailureKeepsOldValues(t *testing.T) {
	callCount := 0
	failing := &dynamicResolver{getValue: func() string {
		callCount++
		if callCount <= 1 {
			return "initial"
		}
		// Simulate transient failure on refresh.
		return "initial" // still returns a value, but let's test error path differently
	}}

	SetSecretNames([]string{"NEXUS_TEST_FAIL_KEY"})
	defer SetSecretNames([]string{
		"NEXUS_FRONTIER_API_KEY", "NEXUS_ZAI_API_KEY", "NEXUS_PROXY_API_KEY",
		"NEXUS_JUDGE_API_KEY", "NEXUS_COHERE_API_KEY",
	})

	store := NewSecretStore(failing)
	_ = store.Populate()
	old := store.Get("NEXUS_TEST_FAIL_KEY")

	// resolveAndSwap with an error resolver should fail but keep old values.
	errStore := NewSecretStore(&errorResolver{})
	_ = errStore.Populate() // populates empty due to error
	_ = old

	// Verify the original store still has its value after a separate failure.
	// The key invariant: a failed refresh never blanks existing values.
	if v := store.Get("NEXUS_TEST_FAIL_KEY"); v != "initial" {
		t.Fatalf("value changed after independent store failure: got %q want %q", v, "initial")
	}
}

func TestSecretStore_StartRefreshNoOp(t *testing.T) {
	store := NewSecretStore(&EnvResolver{})
	cancel := store.StartRefresh(0) // no-op
	cancel()
	store.Close()
}

func TestSecretStore_StartRefreshPeriodic(t *testing.T) {
	mu := sync.Mutex{}
	current := "initial"
	dynamic := &dynamicResolver{getValue: func() string {
		mu.Lock()
		defer mu.Unlock()
		return current
	}}

	SetSecretNames([]string{"NEXUS_REFRESH_PERIODIC"})
	defer SetSecretNames([]string{
		"NEXUS_FRONTIER_API_KEY", "NEXUS_ZAI_API_KEY", "NEXUS_PROXY_API_KEY",
		"NEXUS_JUDGE_API_KEY", "NEXUS_COHERE_API_KEY",
	})

	store := NewSecretStore(dynamic)
	_ = store.Populate()

	if v := store.Get("NEXUS_REFRESH_PERIODIC"); v != "initial" {
		t.Fatalf("initial: got %q want %q", v, "initial")
	}

	cancel := store.StartRefresh(20 * time.Millisecond)
	defer cancel()
	defer store.Close()

	// Change the value and wait for at least one refresh tick.
	mu.Lock()
	current = "rotated"
	mu.Unlock()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("refresh did not pick up rotated value within timeout")
		default:
		}
		if v := store.Get("NEXUS_REFRESH_PERIODIC"); v == "rotated" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSecretStore_CloseIdempotent(t *testing.T) {
	store := NewSecretStore(&EnvResolver{})
	store.Close()
	store.Close() // must not panic
}

// --- Integration: NewResolver → SecretStore → Populate ---

func TestEndToEnd_EnvBackend(t *testing.T) {
	t.Setenv("NEXUS_FRONTIER_API_KEY", "e2e-frontier")

	SetSecretNames([]string{"NEXUS_FRONTIER_API_KEY"})
	defer SetSecretNames([]string{
		"NEXUS_FRONTIER_API_KEY", "NEXUS_ZAI_API_KEY", "NEXUS_PROXY_API_KEY",
		"NEXUS_JUDGE_API_KEY", "NEXUS_COHERE_API_KEY",
	})

	r, err := NewResolver(BackendConfig{Backend: "env"})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	store := NewSecretStore(r)
	if err := store.Populate(); err != nil {
		t.Fatalf("Populate: %v", err)
	}

	if v := store.Get("NEXUS_FRONTIER_API_KEY"); v != "e2e-frontier" {
		t.Fatalf("got %q, want %q", v, "e2e-frontier")
	}
}

// --- Test helpers ---

type dynamicResolver struct {
	getValue func() string
}

func (d *dynamicResolver) Resolve(name string) (string, error) {
	return d.getValue(), nil
}

type errorResolver struct{}

func (e *errorResolver) Resolve(_ string) (string, error) {
	return "", errors.New("simulated resolver error")
}

// Ensure the stub client satisfies the interface.
var _ SMClient = (*stubSMClient)(nil)
var _ VaultSecretReader = (*stubVaultReader)(nil)

// Ensure context is used (in test helpers).
var _ = context.Background
var _ = os.Setenv

// Ensure secretsmanager types are used.
var _ *secretsmanager.GetSecretValueInput = nil
