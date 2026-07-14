package transport

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew_Defaults(t *testing.T) {
	client := New(Config{})
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.Transport)
	}

	if tr.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != DefaultIdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, DefaultIdleConnTimeout)
	}
}

func TestNew_CustomConfig(t *testing.T) {
	cfg := Config{
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     200,
		IdleConnTimeout:     60 * time.Second,
		DialContextTimeout:  15 * time.Second,
	}
	client := New(cfg)
	tr := client.Transport.(*http.Transport)

	if tr.MaxIdleConnsPerHost != 50 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 50", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != 200 {
		t.Errorf("MaxConnsPerHost = %d, want 200", tr.MaxConnsPerHost)
	}
	if tr.IdleConnTimeout != 60*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 60s", tr.IdleConnTimeout)
	}
}

func TestNewFromEnv_Overrides(t *testing.T) {
	restore := func() {
		os.Unsetenv("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST")
		os.Unsetenv("NEXUS_HTTP_MAX_CONNS_PER_HOST")
		os.Unsetenv("NEXUS_HTTP_IDLE_CONN_TIMEOUT")
		os.Unsetenv("NEXUS_HTTP_DIAL_CONTEXT_TIMEOUT")
	}
	restore()
	defer restore()

	os.Setenv("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST", "25")
	os.Setenv("NEXUS_HTTP_MAX_CONNS_PER_HOST", "100")
	os.Setenv("NEXUS_HTTP_IDLE_CONN_TIMEOUT", "45s")
	os.Setenv("NEXUS_HTTP_DIAL_CONTEXT_TIMEOUT", "10s")

	client := NewFromEnv()
	tr := client.Transport.(*http.Transport)

	if tr.MaxIdleConnsPerHost != 25 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 25", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != 100 {
		t.Errorf("MaxConnsPerHost = %d, want 100", tr.MaxConnsPerHost)
	}
	if tr.IdleConnTimeout != 45*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 45s", tr.IdleConnTimeout)
	}
}

func TestNewFromEnv_InvalidValuesFallBackToDefaults(t *testing.T) {
	restore := func() {
		os.Unsetenv("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST")
		os.Unsetenv("NEXUS_HTTP_IDLE_CONN_TIMEOUT")
	}
	restore()
	defer restore()

	os.Setenv("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST", "not-an-int")
	os.Setenv("NEXUS_HTTP_IDLE_CONN_TIMEOUT", "not-a-duration")

	client := NewFromEnv()
	tr := client.Transport.(*http.Transport)

	if tr.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want default %d", tr.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != DefaultIdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want default %v", tr.IdleConnTimeout, DefaultIdleConnTimeout)
	}
}

func TestNew_ZeroValuesGetDefaults(t *testing.T) {
	cfg := Config{
		MaxIdleConnsPerHost: 0,
		IdleConnTimeout:     0,
		DialContextTimeout:  0,
	}
	client := New(cfg)
	tr := client.Transport.(*http.Transport)

	if tr.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want default %d", tr.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != DefaultIdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want default %v", tr.IdleConnTimeout, DefaultIdleConnTimeout)
	}
}

func TestConfig_ApplyDefaults(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want Config
	}{
		{
			name: "zero values get defaults",
			cfg:  Config{},
			want: Config{
				MaxIdleConnsPerHost: DefaultMaxIdleConnsPerHost,
				IdleConnTimeout:     DefaultIdleConnTimeout,
				DialContextTimeout:  DefaultDialContextTimeout,
			},
		},
		{
			name: "negative values get defaults",
			cfg: Config{
				MaxIdleConnsPerHost: -1,
				IdleConnTimeout:     -1,
				DialContextTimeout:  -1,
			},
			want: Config{
				MaxIdleConnsPerHost: DefaultMaxIdleConnsPerHost,
				IdleConnTimeout:     DefaultIdleConnTimeout,
				DialContextTimeout:  DefaultDialContextTimeout,
			},
		},
		{
			name: "positive values are preserved",
			cfg: Config{
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     30 * time.Second,
				DialContextTimeout:  5 * time.Second,
			},
			want: Config{
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     30 * time.Second,
				DialContextTimeout:  5 * time.Second,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.applyDefaults()
			if tt.cfg.MaxIdleConnsPerHost != tt.want.MaxIdleConnsPerHost {
				t.Errorf("MaxIdleConnsPerHost = %d, want %d", tt.cfg.MaxIdleConnsPerHost, tt.want.MaxIdleConnsPerHost)
			}
			if tt.cfg.IdleConnTimeout != tt.want.IdleConnTimeout {
				t.Errorf("IdleConnTimeout = %v, want %v", tt.cfg.IdleConnTimeout, tt.want.IdleConnTimeout)
			}
			if tt.cfg.DialContextTimeout != tt.want.DialContextTimeout {
				t.Errorf("DialContextTimeout = %v, want %v", tt.cfg.DialContextTimeout, tt.want.DialContextTimeout)
			}
		})
	}
}

func TestParseEnvInt(t *testing.T) {
	restore := func() { os.Unsetenv("TEST_VAR") }
	restore()
	defer restore()

	os.Setenv("TEST_VAR", "42")
	if got := parseEnvInt("TEST_VAR", 99); got != 42 {
		t.Errorf("parseEnvInt = %d, want 42", got)
	}

	os.Setenv("TEST_VAR", "not-an-int")
	if got := parseEnvInt("TEST_VAR", 99); got != 99 {
		t.Errorf("parseEnvInt = %d, want fallback 99", got)
	}

	os.Unsetenv("TEST_VAR")
	if got := parseEnvInt("TEST_VAR", 99); got != 99 {
		t.Errorf("parseEnvInt = %d, want fallback 99", got)
	}
}

func TestParseEnvDuration(t *testing.T) {
	restore := func() { os.Unsetenv("TEST_VAR") }
	restore()
	defer restore()

	os.Setenv("TEST_VAR", "30s")
	if got := parseEnvDuration("TEST_VAR", time.Second); got != 30*time.Second {
		t.Errorf("parseEnvDuration = %v, want 30s", got)
	}

	os.Setenv("TEST_VAR", "not-a-duration")
	if got := parseEnvDuration("TEST_VAR", time.Second); got != time.Second {
		t.Errorf("parseEnvDuration = %v, want fallback 1s", got)
	}

	os.Unsetenv("TEST_VAR")
	if got := parseEnvDuration("TEST_VAR", time.Second); got != time.Second {
		t.Errorf("parseEnvDuration = %v, want fallback 1s", got)
	}
}

func TestLoadConfigFromEnv_AllDefaults(t *testing.T) {
	restore := func() {
		os.Unsetenv("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST")
		os.Unsetenv("NEXUS_HTTP_MAX_CONNS_PER_HOST")
		os.Unsetenv("NEXUS_HTTP_IDLE_CONN_TIMEOUT")
		os.Unsetenv("NEXUS_HTTP_DIAL_CONTEXT_TIMEOUT")
	}
	restore()
	defer restore()

	cfg := loadConfigFromEnv()
	if cfg.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", cfg.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost)
	}
	if cfg.MaxConnsPerHost != 0 {
		t.Errorf("MaxConnsPerHost = %d, want 0", cfg.MaxConnsPerHost)
	}
	if cfg.IdleConnTimeout != DefaultIdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", cfg.IdleConnTimeout, DefaultIdleConnTimeout)
	}
	if cfg.DialContextTimeout != DefaultDialContextTimeout {
		t.Errorf("DialContextTimeout = %v, want %v", cfg.DialContextTimeout, DefaultDialContextTimeout)
	}
}

func TestLoadConfigFromEnv_AllSet(t *testing.T) {
	restore := func() {
		os.Unsetenv("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST")
		os.Unsetenv("NEXUS_HTTP_MAX_CONNS_PER_HOST")
		os.Unsetenv("NEXUS_HTTP_IDLE_CONN_TIMEOUT")
		os.Unsetenv("NEXUS_HTTP_DIAL_CONTEXT_TIMEOUT")
	}
	restore()
	defer restore()

	os.Setenv("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST", "150")
	os.Setenv("NEXUS_HTTP_MAX_CONNS_PER_HOST", "300")
	os.Setenv("NEXUS_HTTP_IDLE_CONN_TIMEOUT", "120s")
	os.Setenv("NEXUS_HTTP_DIAL_CONTEXT_TIMEOUT", "20s")

	cfg := loadConfigFromEnv()
	if cfg.MaxIdleConnsPerHost != 150 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 150", cfg.MaxIdleConnsPerHost)
	}
	if cfg.MaxConnsPerHost != 300 {
		t.Errorf("MaxConnsPerHost = %d, want 300", cfg.MaxConnsPerHost)
	}
	if cfg.IdleConnTimeout != 120*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 120s", cfg.IdleConnTimeout)
	}
	if cfg.DialContextTimeout != 20*time.Second {
		t.Errorf("DialContextTimeout = %v, want 20s", cfg.DialContextTimeout)
	}
}

func TestNew_ClientHasNoTimeout(t *testing.T) {
	client := New(Config{})
	if client.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 (context deadlines are used)", client.Timeout)
	}
}

func TestNew_DialContextIsWired(t *testing.T) {
	client := New(Config{
		DialContextTimeout: 5 * time.Second,
	})
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.Transport)
	}

	// Verify DialContext is set (not nil)
	if tr.DialContext == nil {
		t.Error("DialContext is nil")
	}
}

func TestNewFromEnv_ReadsEnvVars(t *testing.T) {
	restore := func() {
		os.Unsetenv("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST")
		os.Unsetenv("NEXUS_HTTP_MAX_CONNS_PER_HOST")
		os.Unsetenv("NEXUS_HTTP_IDLE_CONN_TIMEOUT")
		os.Unsetenv("NEXUS_HTTP_DIAL_CONTEXT_TIMEOUT")
	}
	restore()
	defer restore()

	os.Setenv("NEXUS_HTTP_MAX_IDLE_CONNS_PER_HOST", "200")
	os.Setenv("NEXUS_HTTP_MAX_CONNS_PER_HOST", "400")

	client := NewFromEnv()
	tr := client.Transport.(*http.Transport)

	if tr.MaxIdleConnsPerHost != 200 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 200", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != 400 {
		t.Errorf("MaxConnsPerHost = %d, want 400", tr.MaxConnsPerHost)
	}
}

// --- mTLS tests ---

func TestHasClientCert(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{
			name: "both cert and key set",
			cfg:  Config{ClientCertFile: "cert.pem", ClientKeyFile: "key.pem"},
			want: true,
		},
		{
			name: "only cert set",
			cfg:  Config{ClientCertFile: "cert.pem", ClientKeyFile: ""},
			want: false,
		},
		{
			name: "only key set",
			cfg:  Config{ClientCertFile: "", ClientKeyFile: "key.pem"},
			want: false,
		},
		{
			name: "neither set",
			cfg:  Config{ClientCertFile: "", ClientKeyFile: ""},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.hasClientCert(); got != tt.want {
				t.Errorf("hasClientCert() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildTLSConfig(t *testing.T) {
	// Create a temporary self-signed certificate for testing.
	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "client.crt")
	keyFile := filepath.Join(tmpDir, "client.key")
	caFile := filepath.Join(tmpDir, "ca.crt")

	// Generate a self-signed certificate using openssl if available,
	// otherwise skip this test. For CI, certificates are pre-generated.
	generateTestCert(t, certFile, keyFile, caFile)

	t.Run("valid cert and key", func(t *testing.T) {
		cfg := Config{
			ClientCertFile: certFile,
			ClientKeyFile:  keyFile,
		}
		tlsConfig, err := buildTLSConfig(cfg)
		if err != nil {
			t.Fatalf("buildTLSConfig() error = %v", err)
		}
		if tlsConfig == nil {
			t.Fatal("buildTLSConfig() returned nil")
		}
		if len(tlsConfig.Certificates) != 1 {
			t.Errorf("len(Certificates) = %d, want 1", len(tlsConfig.Certificates))
		}
		if tlsConfig.GetClientCertificate == nil {
			t.Error("GetClientCertificate is nil")
		}
	})

	t.Run("with CAFile", func(t *testing.T) {
		cfg := Config{
			ClientCertFile: certFile,
			ClientKeyFile:  keyFile,
			CAFile:         caFile,
		}
		tlsConfig, err := buildTLSConfig(cfg)
		if err != nil {
			t.Fatalf("buildTLSConfig() error = %v", err)
		}
		if tlsConfig.RootCAs == nil {
			t.Error("RootCAs is nil")
		}
	})

	t.Run("invalid cert path", func(t *testing.T) {
		cfg := Config{
			ClientCertFile: "nonexistent.crt",
			ClientKeyFile:  keyFile,
		}
		_, err := buildTLSConfig(cfg)
		if err == nil {
			t.Error("buildTLSConfig() expected error for invalid cert path")
		}
	})

	t.Run("invalid key path", func(t *testing.T) {
		cfg := Config{
			ClientCertFile: certFile,
			ClientKeyFile:  "nonexistent.key",
		}
		_, err := buildTLSConfig(cfg)
		if err == nil {
			t.Error("buildTLSConfig() expected error for invalid key path")
		}
	})
}

func TestNew_WithMTLS(t *testing.T) {
	tmpDir := t.TempDir()
	certFile := filepath.Join(tmpDir, "client.crt")
	keyFile := filepath.Join(tmpDir, "client.key")
	caFile := filepath.Join(tmpDir, "ca.crt")
	generateTestCert(t, certFile, keyFile, caFile)

	cfg := Config{
		ClientCertFile: certFile,
		ClientKeyFile:  keyFile,
		CAFile:         caFile,
	}
	client := New(cfg)
	tr := client.Transport.(*http.Transport)

	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil")
	}
	if tr.TLSClientConfig.GetClientCertificate == nil {
		t.Error("GetClientCertificate is nil")
	}
}

func TestNew_WithMTLS_FallbackOnError(t *testing.T) {
	cfg := Config{
		ClientCertFile: "nonexistent.crt",
		ClientKeyFile:  "nonexistent.key",
	}
	client := New(cfg)
	tr := client.Transport.(*http.Transport)

	// Should fall back to nil TLSClientConfig rather than panicking
	if tr.TLSClientConfig != nil {
		t.Error("TLSClientConfig should be nil on error")
	}
}

func TestNew_NoMTLS(t *testing.T) {
	cfg := Config{}
	client := New(cfg)
	tr := client.Transport.(*http.Transport)

	if tr.TLSClientConfig != nil {
		t.Error("TLSClientConfig should be nil when no certs configured")
	}
}

func TestLoadConfigFromEnv_MTLS(t *testing.T) {
	restore := func() {
		os.Unsetenv("NEXUS_HTTP_CLIENT_CERT_FILE")
		os.Unsetenv("NEXUS_HTTP_CLIENT_KEY_FILE")
		os.Unsetenv("NEXUS_HTTP_CA_FILE")
	}
	restore()
	defer restore()

	os.Setenv("NEXUS_HTTP_CLIENT_CERT_FILE", "/path/to/cert.pem")
	os.Setenv("NEXUS_HTTP_CLIENT_KEY_FILE", "/path/to/key.pem")
	os.Setenv("NEXUS_HTTP_CA_FILE", "/path/to/ca.pem")

	cfg := loadConfigFromEnv()
	if cfg.ClientCertFile != "/path/to/cert.pem" {
		t.Errorf("ClientCertFile = %q, want %q", cfg.ClientCertFile, "/path/to/cert.pem")
	}
	if cfg.ClientKeyFile != "/path/to/key.pem" {
		t.Errorf("ClientKeyFile = %q, want %q", cfg.ClientKeyFile, "/path/to/key.pem")
	}
	if cfg.CAFile != "/path/to/ca.pem" {
		t.Errorf("CAFile = %q, want %q", cfg.CAFile, "/path/to/ca.pem")
	}
}

// generateTestCert creates a self-signed certificate for testing.
func generateTestCert(t *testing.T, certFile, keyFile, caFile string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate private key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "testca",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatalf("failed to write cert file: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatalf("failed to write key file: %v", err)
	}
	if err := os.WriteFile(caFile, certPEM, 0600); err != nil {
		t.Fatalf("failed to write ca file: %v", err)
	}
}
