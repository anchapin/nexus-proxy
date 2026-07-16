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
	"bytes"
	"context"
	"encoding/json"
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
	localModel       string
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

	// currentInterval is the active polling interval in nanoseconds.
	// It starts as pollInterval and increases via exponential backoff
	// after consecutive failures (capped at 15x pollInterval).
	currentInterval atomic.Int64

	// lastTier tracks the backoff tier that was last logged so we
	// only log on tier transitions, not every failed probe.
	lastTier atomic.Int32

	// mu serializes recordSuccess to ensure that Swap(true) and
	// Store(0) are observable together — without this mutex a
	// goroutine could see healthy==true but a stale failureCount
	// due to CPU cache ordering between the two atomics.
	mu sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// New constructs a Health. Zero-valued knobs are filled with safe
// defaults (30s poll, 3-failure breaker threshold, 5s probe timeout)
// so a partial config is enough. A nil client falls back to
// http.DefaultClient. localModel is the model name used for the
// model-availability probe; it is stored verbatim and passed to
// the chat completions endpoint.
//
// The returned Health has not yet probed; call Run to perform the
// initial probe and start the background poller.
func New(ollamaURL string, localModel string, pollInterval time.Duration, breakerThreshold int, probeTimeout time.Duration, client *http.Client) *Health {
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
		localModel:       localModel,
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
	h.currentInterval.Store(int64(pollInterval))
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
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.healthy.Load()
}

// FailureCount returns the current consecutive-failure counter.
// Exported for tests and operational dashboards; the chat hot path
// does not consult it directly (use IsLocalHealthy instead).
func (h *Health) FailureCount() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return int(h.failureCount.Load())
}

// PollingInterval returns the current polling interval.
// Exported for tests to verify backoff behavior.
func (h *Health) PollingInterval() time.Duration {
	if h == nil {
		return 0
	}
	return time.Duration(h.currentInterval.Load())
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

	if !h.IsLocalHealthy() {
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

// maxBackoffMultiplier caps the backoff at 15x the poll interval.
const maxBackoffMultiplier = 15

// calcBackoffInterval computes the backoff interval based on consecutive
// failure count. Tier 0 (failures 1-2): 1x, Tier 1 (failures 3-4): 2x,
// Tier 2 (failures 5-6): 4x, Tier 3 (failures 7-8): 8x, Tier 4+: 15x.
func (h *Health) calcBackoffInterval(count int) time.Duration {
	// tier 0: count 1-2, tier 1: count 3-4, tier 2: count 5-6, ...
	tier := (count - 1) >> 1
	if tier < 0 {
		tier = 0
	}
	multiplier := int64(1 << tier)
	if multiplier > maxBackoffMultiplier {
		multiplier = maxBackoffMultiplier
	}
	return h.pollInterval * time.Duration(multiplier)
}

// loop runs probes on a ticker until ctx is canceled or Close is
// called. The probe itself uses a per-call timeout derived from
// probeTimeout so a slow Ollama cannot pin the goroutine.
// After the breaker trips, the ticker interval is increased via
// exponential backoff to reduce log noise during extended outages.
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

			// After the probe, check if we need to adjust the
			// backoff interval (only when breaker is open).
			if !h.healthy.Load() {
				h.mu.Lock()
				count := int(h.failureCount.Load())
				// failureCount < breakerThreshold shouldn't happen
				// (breaker is only open when count >= threshold),
				// but handle it gracefully.
				if count >= int(h.breakerThreshold) {
					newInterval := h.calcBackoffInterval(count)
					curr := time.Duration(h.currentInterval.Load())
					if newInterval != curr {
						// Drain any pending ticks from the old ticker
						// before stopping it, to prevent stale ticks
						// from firing at the wrong interval.
						oldTicker := t
						select {
						case <-oldTicker.C:
						default:
						}
						oldTicker.Stop()
						t = time.NewTicker(newInterval)
						slog.Info("ollama health: entering backoff mode",
							slog.Float64("interval_seconds", newInterval.Seconds()),
						)
					}
				}
				h.mu.Unlock()
			}
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

// probe performs one HTTP GET to /api/tags to check server reachability
// and one HTTP POST to /api/chat with a trivial request to verify the
// model is loaded and functional. See Probe for the public, error-returning
// variant.
//
// Status >= 500 on either call is treated as failure (the daemon is
// reachable but sick). 4xx on the tags check is treated as success: the
// daemon answered, even if some authentication layer rejected the request.
// For the model check, 4xx means the model is not available.
func (h *Health) probe(ctx context.Context) error {
	// First check server reachability via /api/tags.
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
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		err := fmt.Errorf("tags status %d", resp.StatusCode)
		h.recordFailure(err)
		return err
	}

	// Server is up; now verify the model is actually loaded by making
	// a trivial chat request. This catches the case where Ollama is running
	// but the model failed to load (e.g. VRAM exhaustion).
	chatReq := struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}{
		Model: h.localModel,
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{
			{Role: "user", Content: "hi"},
		},
	}
	payload, err := json.Marshal(chatReq)
	if err != nil {
		h.recordFailure(err)
		return err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, h.ollamaURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		h.recordFailure(err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = h.client.Do(req)
	if err != nil {
		h.recordFailure(err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		err := fmt.Errorf("chat status %d", resp.StatusCode)
		h.recordFailure(err)
		return err
	}
	// 4xx on the chat endpoint means the model is not available.
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("model %s unavailable: status %d", h.localModel, resp.StatusCode)
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
	h.mu.Lock()
	defer h.mu.Unlock()
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
		// Update currentInterval based on failure count tier.
		// Tier 0 (count 1-2): 1x, Tier 1 (count 3-4): 2x,
		// Tier 2 (count 5-6): 4x, Tier 3 (count 7-8): 8x, Tier 4+: 15x.
		tier := (int(count) - 1) >> 1
		if tier < 0 {
			tier = 0
		}
		multiplier := int64(1 << tier)
		if multiplier > maxBackoffMultiplier {
			multiplier = maxBackoffMultiplier
		}
		h.currentInterval.Store(int64(h.pollInterval * time.Duration(multiplier)))
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
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.healthy.Swap(true) {
		slog.Info("ollama health: recovered",
			slog.String("ollama_url", h.ollamaURL),
		)
	}
	h.failureCount.Store(0)
	h.currentInterval.Store(int64(h.pollInterval))
	h.lastTier.Store(0)
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
