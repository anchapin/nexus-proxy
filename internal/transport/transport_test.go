package transport

import (
	"net/http"
	"os"
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
		name    string
		cfg     Config
		want    Config
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
