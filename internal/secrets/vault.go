package secrets

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/vault/api"
)

// VaultSecretReader is the minimal interface for reading a KV secret from
// Vault. The production implementation uses vault/api.Logical; tests inject
// a stub to avoid a live Vault dependency.
type VaultSecretReader interface {
	Read(path string) (*api.Secret, error)
}

// VaultResolver reads credentials from HashiCorp Vault's KV engine.
// It maps each NEXUS_* env-var name to a field inside a Vault secret path,
// then falls back to the process environment if the field is absent.
//
// Path mapping: {VaultPath}/data/nexus (KV v2) or {VaultPath}/nexus (KV v1).
// The secret object is expected to contain keys matching the env-var name,
// e.g. {"NEXUS_FRONTIER_API_KEY": "sk-..."}. A key that is not present in
// Vault falls back to os.Getenv so operators can mix Vault-sourced and
// env-sourced credentials during migration.
type VaultResolver struct {
	reader VaultSecretReader
	path   string // full logical path, e.g. "secret/data/nexus"
	cache  map[string]string
}

// NewVaultResolver creates a Resolver backed by HashiCorp Vault.
// It requires either VaultToken (static token) or VaultRole (Kubernetes
// auth, which reads the service-account JWT from the standard file path).
// Fail-closed: if the Vault client cannot be initialised or the initial
// connection probe fails, NewVaultResolver returns an error.
func NewVaultResolver(cfg BackendConfig) (Resolver, error) {
	if cfg.VaultAddr == "" {
		return nil, fmt.Errorf("secrets: NEXUS_VAULT_ADDR is required when backend=vault")
	}

	client, err := api.NewClient(&api.Config{
		Address: cfg.VaultAddr,
		HttpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("secrets: vault client init: %w", err)
	}

	if cfg.VaultToken != "" {
		client.SetToken(cfg.VaultToken)
	} else if cfg.VaultRole != "" {
		if err := vaultK8sAuth(client, cfg.VaultRole); err != nil {
			return nil, fmt.Errorf("secrets: vault k8s auth: %w", err)
		}
	} else {
		// Fall back to token from env or ~/.vault-token.
		if client.Token() == "" {
			return nil, fmt.Errorf("secrets: vault backend requires NEXUS_VAULT_TOKEN or NEXUS_VAULT_ROLE")
		}
	}

	mount := cfg.VaultPath
	if mount == "" {
		mount = "secret"
	}

	vr := &VaultResolver{
		reader: client.Logical(),
		path:   mount + "/data/nexus",
		cache:  make(map[string]string),
	}

	// Fail-closed probe: verify connectivity by attempting a read. An
	// unreachable Vault at boot is a hard error, never silent.
	if _, err := vr.reader.Read(vr.path); err != nil {
		return nil, fmt.Errorf("secrets: vault unreachable at %s: %w", cfg.VaultAddr, err)
	}

	return vr, nil
}

// vaultK8sAuth authenticates to Vault using the Kubernetes auth method.
// It reads the service-account JWT from the standard mount path and
// exchanges it for a Vault token using the given role.
func vaultK8sAuth(client *api.Client, role string) error {
	jwtBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return fmt.Errorf("read k8s service account token: %w", err)
	}

	resp, err := client.Logical().Write("auth/kubernetes/login", map[string]interface{}{
		"jwt":  strings.TrimSpace(string(jwtBytes)),
		"role": role,
	})
	if err != nil {
		return err
	}
	if resp == nil || resp.Auth == nil || resp.Auth.ClientToken == "" {
		return fmt.Errorf("vault k8s auth returned empty token for role %q", role)
	}
	client.SetToken(resp.Auth.ClientToken)
	return nil
}

// Resolve looks up name in Vault. If the field is absent it falls back to
// os.Getenv so operators can mix Vault and env credentials.
func (r *VaultResolver) Resolve(name string) (string, error) {
	if v, ok := r.cache[name]; ok {
		return v, nil
	}

	sec, err := r.reader.Read(r.path)
	if err != nil {
		return "", fmt.Errorf("vault read %s: %w", r.path, err)
	}

	if sec != nil && sec.Data != nil {
		data := sec.Data
		// KV v2 nests values under "data"; KV v1 stores them at top level.
		if inner, ok := data["data"].(map[string]interface{}); ok {
			data = inner
		}
		if v, ok := data[name].(string); ok && v != "" {
			r.cache[name] = v
			return v, nil
		}
	}

	return os.Getenv(name), nil
}

// --- testing helpers ---

// newVaultResolverWithReader constructs a VaultResolver with a custom reader.
// This is used by tests to inject a stub without a live Vault instance.
func newVaultResolverWithReader(reader VaultSecretReader, path string) *VaultResolver {
	return &VaultResolver{
		reader: reader,
		path:   path,
		cache:  make(map[string]string),
	}
}

// stubVaultReader is a test double for VaultSecretReader.
type stubVaultReader struct {
	secret *api.Secret
	err    error
	calls  int
}

func (s *stubVaultReader) Read(_ string) (*api.Secret, error) {
	s.calls++
	return s.secret, s.err
}

// makeVaultSecret builds an api.Secret with KV-v2-shaped data.
func makeVaultSecret(kv map[string]interface{}) *api.Secret {
	return &api.Secret{Data: map[string]interface{}{"data": kv}}
}
