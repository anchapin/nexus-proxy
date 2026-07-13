// Package transport constructs the shared *http.Client instances that the
// proxy uses for every outbound call: chat upstream, SLM routing, RAG
// embedding, health polling, VRAM probing, and the judge evaluator.
//
// Why a dedicated factory (issue #34): every call site previously fell
// back to http.DefaultClient, whose default Transport caps idle
// connections per host at 2. On a multi-agent local-dev workload that
// keeps two upstream connections permanently warm — which means the
// chat handler and the SLM/RAG/health/probe goroutines serialize on a
// pool of two and pay handshake latency on every call. Building a
// custom Transport sized for this workload removes that bottleneck
// without changing any caller's contract (*http.Client satisfies the
// upstream.Client and judge.HTTPClient interfaces already).
//
// Two clients are exposed:
//
//   - New()         — primary pool sized for chat-class traffic.
//   - NewProbe()    — lighter pool with MaxIdleConnsPerHost=1, suitable
//     for background pollers (health, VRAM) that hit one host on a
//     slow cadence and should not reserve idle slots on the main pool.
//
// All knobs are env-tunable; see internal/config for the env-var
// surface and the defaults.
package transport

import (
	"net/http"
	"time"
)

// Defaults applied when the corresponding Config field is zero. Tests
// and operators can override each knob via NEXUS_HTTP_* env vars
// (see internal/config).
const (
	DefaultMaxIdleConns        = 100
	DefaultMaxIdleConnsPerHost = 16
	DefaultMaxConnsPerHost     = 0 // 0 == unlimited
	DefaultIdleConnTimeout     = 90 * time.Second
	// DefaultDisableKeepAlives is false: keep-alive is the whole point of
	// having a connection pool.

	// probePerHost caps idle conns reserved by background pollers so they
	// do not crowd the primary pool when several pollers (health + VRAM)
	// share a single host.
	probePerHost = 1
)

// Config carries the knobs for building a pooled *http.Client. The
// zero value produces a transport sized to the package defaults; a
// fully zero-valued Config is the recommended way to construct a
// "give me the defaults" client in tests.
type Config struct {
	// MaxIdleConns caps the total number of idle connections across
	// all hosts. http.DefaultClient's underlying transport uses 100.
	MaxIdleConns int

	// MaxIdleConnsPerHost caps idle connections retained per host. The
	// Go stdlib default is 2 — far too few for the multi-agent local
	// dev workload (chat + SLM + RAG all hit the local Ollama).
	MaxIdleConnsPerHost int

	// MaxConnsPerHost limits the total (in-flight + idle) connections
	// per host. Zero means no limit, which matches the stdlib default.
	MaxConnsPerHost int

	// IdleConnTimeout is how long an idle connection stays in the
	// pool before being closed. The stdlib default is 90s; we keep it
	// for predictability.
	IdleConnTimeout time.Duration

	// DisableKeepAlives disables HTTP keep-alive. False (the default)
	// is what makes connection reuse possible; operators flip this on
	// for one-off debugging runs.
	DisableKeepAlives bool
}

// applyDefaults fills zero-valued fields with the package defaults so
// callers can pass a partially-populated Config.
func (c Config) applyDefaults() Config {
	if c.MaxIdleConns == 0 {
		c.MaxIdleConns = DefaultMaxIdleConns
	}
	if c.MaxIdleConnsPerHost == 0 {
		c.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost
	}
	if c.IdleConnTimeout == 0 {
		c.IdleConnTimeout = DefaultIdleConnTimeout
	}
	return c
}

// transport builds the underlying *http.Transport. Exposed unexported
// so the test file in this package can assert the configured values
// without exposing the field to outside callers.
func (c Config) transport() *http.Transport {
	c = c.applyDefaults()
	return &http.Transport{
		MaxIdleConns:        c.MaxIdleConns,
		MaxIdleConnsPerHost: c.MaxIdleConnsPerHost,
		MaxConnsPerHost:     c.MaxConnsPerHost,
		IdleConnTimeout:     c.IdleConnTimeout,
		DisableKeepAlives:   c.DisableKeepAlives,
	}
}

// New builds the primary shared *http.Client. The returned client's
// Transport is a freshly allocated *http.Transport so two callers
// cannot accidentally share idle-conn state via the package-level
// http.DefaultTransport.
//
// The client has no per-call timeout; callers that need one (the SLM
// router, the fusion arbiter, etc.) wrap each call with
// http.NewRequestWithContext(ctx, ...). Keeping the timeout at the
// caller avoids the stdlib http.Client default of "no timeout" being
// silently overridden by a too-large global value.
func New(c Config) *http.Client {
	return &http.Client{Transport: c.transport()}
}

// NewProbe builds a lighter client sized for background pollers
// (health poller, VRAM probe) that hit a single host on a slow
// cadence. It keeps MaxIdleConnsPerHost=1 so several pollers hitting
// the same Ollama host do not collectively reserve N idle slots in
// the main pool — they share at most one.
//
// All other knobs come from the supplied Config (with defaults
// applied). The per-call timeout, as with New, is left to the caller.
func NewProbe(c Config) *http.Client {
	if c.MaxIdleConnsPerHost == 0 || c.MaxIdleConnsPerHost > probePerHost {
		c.MaxIdleConnsPerHost = probePerHost
	}
	return &http.Client{Transport: c.transport()}
}
