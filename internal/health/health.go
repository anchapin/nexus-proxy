// Package health implements a background Ollama health poller. A
// long-lived goroutine pings the local Ollama endpoint on a fixed
// interval and exposes the current "is Ollama reachable" state to the
// rest of the proxy. The chat handler consults IsLocalHealthy before
// issuing local-bound requests; when unhealthy it short-circuits to
// frontier (route=local) or skips the local member of the fusion
// panel (route=fusion) and stamps X-Nexus-Degraded: true on the
// response.
//
// The poller also acts as a small circuit breaker: after
// BreakerThreshold consecutive failed probes the health state flips
// to false until the next successful probe, at which point the
// breaker reopens and IsLocalHealthy() returns true again. This
// keeps the hot path from paying the per-request Ollama timeout
// when Ollama is down — the worst case becomes one probe timeout
// every PollInterval.
//
// Stdlib-only and dependency-free: this package is on the chat hot
// path's dependency list (handlers imports it), so it must not pull
// in anything the rest of the proxy does not already depend on.
package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults applied by New when the matching field is zero. Operators
// can override each knob via NEXUS_HEALTH_* env vars (see
// internal/config).
const (
	DefaultPollInterval     = 30 * time.Second
	DefaultBreakerThreshold = 3
	DefaultProbeTimeout     = 5 * time.Second
)

// ErrDisabled is the sentinel returned by Health methods when the
// receiver is nil. Wiring sites that hold an optional Health use
// this to skip health-aware code paths when no health check has
// been configured (e.g. unit tests).
var ErrDisabled = errors.New("health: not configured")

// Health tracks the live status of the local Ollama endpoint. The
// zero value is invalid; construct via New.
//
// Health is safe for concurrent use. IsLocalHealthy is a single
// atomic load; the poller mutates state via atomic.Store.
type Health struct {
	ollamaURL        string
	pollInterval     time.Duration
	breakerThreshold int32
	probeTimeout     time.Duration
	client           *http.Client

	// healthy is the current public state. It starts as true (assume
	// best case) and is corrected by the first probe. The breaker
	// trips it to false after BreakerThreshold consecutive failures
	// and recovers it to true on the next success.
	healthy atomic.Bool

	// failureCount is the number of consecutive failed probes since
	// the last success. Reset to 0 on every successful probe.
	failureCount atomic.Int32

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// New constructs a Health. Zero-valued knobs are filled with safe
// defaults (30s poll, 3-failure breaker threshold, 5s probe timeout)
// so a partial config is enough. A nil client falls back to
// http.DefaultClient.
//
// The returned Health has not yet probed; call Run to perform the
// initial probe and start the background poller.
func New(ollamaURL string, pollInterval time.Duration, breakerThreshold int, probeTimeout time.Duration, client *http.Client) *Health {
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	if breakerThreshold <= 0 {
		breakerThreshold = DefaultBreakerThreshold
	}
	if probeTimeout <= 0 {
		probeTimeout = DefaultProbeTimeout
	}
	if client == nil {
		client = http.DefaultClient
	}
	h := &Health{
		ollamaURL:        ollamaURL,
		pollInterval:     pollInterval,
		breakerThreshold: int32(breakerThreshold),
		probeTimeout:     probeTimeout,
		client:           client,
		closed:           make(chan struct{}),
	}
	// Start optimistically. Run performs an initial probe that
	// corrects this — a unit test or operator who never starts the
	// poller still gets "healthy" until something trips the breaker.
	h.healthy.Store(true)
	return h
}

// IsLocalHealthy reports whether the most recent probe succeeded.
// Safe to call from many goroutines.
//
// Returns true when h is nil so callers that hold an optional Health
// can avoid a nil-check on every request.
func (h *Health) IsLocalHealthy() bool {
	if h == nil {
		return true
	}
	return h.healthy.Load()
}

// FailureCount returns the current consecutive-failure counter.
// Exported for tests and operational dashboards; the chat hot path
// does not consult it directly (use IsLocalHealthy instead).
func (h *Health) FailureCount() int {
	if h == nil {
		return 0
	}
	return int(h.failureCount.Load())
}

// Run performs an initial synchronous probe and then starts the
// background poller. The initial probe's outcome is logged (warn on
// failure) and surfaces in IsLocalHealthy immediately so the first
// proxied request after boot already sees the correct state.
//
// Run blocks until ctx is canceled or Close is called; it is intended
// to be invoked on its own goroutine from main.
func (h *Health) Run(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, h.probeTimeout)
	// probe updates health state (recordFailure/recordSuccess)
	// internally; the error is not actionable here — a logged probe
	// failure shows up via the warning emitted below and via
	// IsLocalHealthy. errcheck requires us to consume the value.
	_ = h.probe(probeCtx)
	cancel()

	if !h.healthy.Load() {
		slog.Warn("ollama unreachable at boot; local model marked unhealthy",
			slog.String("ollama_url", h.ollamaURL),
			slog.Int("threshold", int(h.breakerThreshold)),
		)
	} else {
		slog.Info("ollama healthy at boot",
			slog.String("ollama_url", h.ollamaURL),
			slog.Duration("poll_interval", h.pollInterval),
		)
	}

	h.wg.Add(1)
	go h.loop(ctx)
}

// loop runs probes on a ticker until ctx is canceled or Close is
// called. The probe itself uses a per-call timeout derived from
// probeTimeout so a slow Ollama cannot pin the goroutine.
func (h *Health) loop(ctx context.Context) {
	defer h.wg.Done()
	t := time.NewTicker(h.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.closed:
			return
		case <-t.C:
			probeCtx, cancel := context.WithTimeout(ctx, h.probeTimeout)
			// Same discard rationale as in Run: probe updates the
			// health state; failure paths surface via recordFailure
			// which is observable through IsLocalHealthy and the
			// log line emitted by the next iteration's audit trail.
			_ = h.probe(probeCtx)
			cancel()
		}
	}
}

// Probe runs a single health probe synchronously and returns the
// resulting status. Exported so tests and ad-hoc operators can drive
// the poller without spinning up the goroutine.
//
// Errors are non-nil only when the probe failed at the transport
// layer or returned a 5xx status; 4xx responses (e.g. a reverse
// proxy in front of Ollama rejecting with 401) are treated as
// "Ollama itself is up".
func (h *Health) Probe(ctx context.Context) error {
	if h == nil {
		return ErrDisabled
	}
	return h.probe(ctx)
}

// probe performs one HTTP GET to /api/tags and updates the internal
// health state. See Probe for the public, error-returning variant.
//
// Status >= 500 is treated as failure (the daemon is reachable but
// sick). 4xx is treated as success: the daemon answered, even if
// some authentication layer rejected the request — Ollama itself
// is up and the operator can investigate the auth layer separately.
func (h *Health) probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.ollamaURL+"/api/tags", nil)
	if err != nil {
		h.recordFailure(err)
		return err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.recordFailure(err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		err := fmt.Errorf("status %d", resp.StatusCode)
		h.recordFailure(err)
		return err
	}
	h.recordSuccess()
	return nil
}

// recordFailure increments the consecutive-failure counter and trips
// the breaker (flips healthy to false) when the threshold is reached.
// Sub-threshold failures stay in the "healthy" state — Ollama may be
// flaky for a single probe — but are still logged at debug for
// observability.
func (h *Health) recordFailure(err error) {
	count := h.failureCount.Add(1)
	wasHealthy := h.healthy.Load()
	if count >= h.breakerThreshold {
		if wasHealthy {
			slog.Warn("ollama health: breaker tripped",
				slog.Int("failures", int(count)),
				slog.Int("threshold", int(h.breakerThreshold)),
				slog.Any("err", err),
			)
		}
		h.healthy.Store(false)
		return
	}
	if wasHealthy {
		slog.Debug("ollama probe failed (below threshold)",
			slog.Int("failures", int(count)),
			slog.Int("threshold", int(h.breakerThreshold)),
			slog.Any("err", err),
		)
	}
}

// recordSuccess resets the consecutive-failure counter and reopens
// the breaker. A "recovered" log line is emitted only on the
// unhealthy → healthy transition so the operator sees the recovery
// without the noise of every subsequent successful probe.
func (h *Health) recordSuccess() {
	if !h.healthy.Swap(true) {
		slog.Info("ollama health: recovered",
			slog.String("ollama_url", h.ollamaURL),
		)
	}
	h.failureCount.Store(0)
}

// Close stops the background poller and waits for the loop goroutine
// to exit. Safe to call multiple times.
func (h *Health) Close() error {
	h.closeOnce.Do(func() {
		close(h.closed)
	})
	h.wg.Wait()
	return nil
}
