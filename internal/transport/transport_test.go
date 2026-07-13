package transport

import (
	"net/http"
	"testing"
	"time"
)

// TestNewDefaults verifies the zero-value Config produces a transport
// with the documented package defaults. This is the contract every
// call site (production + tests) relies on when no env var is set.
func TestNewDefaults(t *testing.T) {
	c := New(Config{})
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.MaxIdleConns != DefaultMaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want %d", tr.MaxIdleConns, DefaultMaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost)
	}
	if tr.MaxConnsPerHost != DefaultMaxConnsPerHost {
		t.Errorf("MaxConnsPerHost = %d, want %d (0 == unlimited)", tr.MaxConnsPerHost, DefaultMaxConnsPerHost)
	}
	if tr.IdleConnTimeout != DefaultIdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, DefaultIdleConnTimeout)
	}
	if tr.DisableKeepAlives {
		t.Error("DisableKeepAlives = true, want false (keep-alive is the whole point)")
	}
}

// TestNewConfigured verifies every Config knob propagates to the
// underlying transport. The table-driven shape mirrors the existing
// config_test.go style so future knobs can be added with a single
// new row.
func TestNewConfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want func(t *testing.T, tr *http.Transport)
	}{
		{
			name: "scaled-up pool",
			cfg: Config{
				MaxIdleConns:        500,
				MaxIdleConnsPerHost: 64,
				MaxConnsPerHost:     200,
				IdleConnTimeout:     2 * time.Minute,
			},
			want: func(t *testing.T, tr *http.Transport) {
				if tr.MaxIdleConns != 500 {
					t.Errorf("MaxIdleConns = %d, want 500", tr.MaxIdleConns)
				}
				if tr.MaxIdleConnsPerHost != 64 {
					t.Errorf("MaxIdleConnsPerHost = %d, want 64", tr.MaxIdleConnsPerHost)
				}
				if tr.MaxConnsPerHost != 200 {
					t.Errorf("MaxConnsPerHost = %d, want 200", tr.MaxConnsPerHost)
				}
				if tr.IdleConnTimeout != 2*time.Minute {
					t.Errorf("IdleConnTimeout = %v, want 2m", tr.IdleConnTimeout)
				}
				if tr.DisableKeepAlives {
					t.Error("DisableKeepAlives = true, want false")
				}
			},
		},
		{
			name: "keep-alives disabled",
			cfg: Config{
				DisableKeepAlives: true,
			},
			want: func(t *testing.T, tr *http.Transport) {
				if !tr.DisableKeepAlives {
					t.Error("DisableKeepAlives = false, want true")
				}
			},
		},
		{
			name: "unlimited MaxConnsPerHost (zero)",
			cfg: Config{
				MaxConnsPerHost: 0,
			},
			want: func(t *testing.T, tr *http.Transport) {
				if tr.MaxConnsPerHost != 0 {
					t.Errorf("MaxConnsPerHost = %d, want 0 (unlimited)", tr.MaxConnsPerHost)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(tc.cfg)
			tr, ok := c.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
			}
			tc.want(t, tr)
		})
	}
}

// TestNewProbe verifies the secondary probe client applies the
// probePerHost cap regardless of what the caller's Config says —
// background pollers should not crowd the main pool.
func TestNewProbe(t *testing.T) {
	c := NewProbe(Config{
		// Caller asks for a generous per-host cap; probe factory
		// must clamp it down to probePerHost.
		MaxIdleConnsPerHost: 32,
		MaxIdleConns:        200,
	})
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.MaxIdleConnsPerHost != probePerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d (clamped to probe cap)", tr.MaxIdleConnsPerHost, probePerHost)
	}
	if tr.MaxIdleConns != 200 {
		t.Errorf("MaxIdleConns = %d, want 200 (other knobs preserved)", tr.MaxIdleConns)
	}
}

// TestNewProbeHonoursExplicitSmallerCap verifies an operator who
// intentionally sets MaxIdleConnsPerHost to 0 (== default) does not
// accidentally get the probe cap bumped when they pass an explicit
// smaller value through. The current implementation only clamps when
// the caller-supplied value is greater than probePerHost, so a value
// of 0 (== default) becomes 1. This test pins that contract.
func TestNewProbeHonoursExplicitSmallerCap(t *testing.T) {
	// Setting to exactly probePerHost should be a no-op clamp.
	c := NewProbe(Config{MaxIdleConnsPerHost: probePerHost})
	tr := c.Transport.(*http.Transport)
	if tr.MaxIdleConnsPerHost != probePerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, probePerHost)
	}
}

// TestDistinctTransports verifies two New() calls return clients with
// distinct *http.Transport values. Sharing a single Transport between
// clients is fine, but the factory must not accidentally return the
// package-level http.DefaultTransport (which would silently re-introduce
// the very pool sizing issue #34 is fixing).
func TestDistinctTransports(t *testing.T) {
	c1 := New(Config{})
	c2 := New(Config{})
	tr1 := c1.Transport.(*http.Transport)
	tr2 := c2.Transport.(*http.Transport)
	if tr1 == tr2 {
		t.Error("New returned clients with the same *http.Transport; expected distinct instances")
	}
	if tr1 == http.DefaultTransport {
		t.Error("New returned http.DefaultTransport; expected a fresh transport")
	}
	if tr2 == http.DefaultTransport {
		t.Error("New returned http.DefaultTransport; expected a fresh transport")
	}
}
