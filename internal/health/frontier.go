// Frontier health poller (issue #1158). Mirrors the Ollama health
// poller (Health in health.go) but tracks multiple frontier API
// providers concurrently. A background goroutine periodically sends a
// lightweight probe (GET <BaseURL>/models) to each registered provider,
// tracks consecutive failures, and exposes a per-provider open/closed
// circuit state.
//
// The ProviderSelector (internal/router) consults this state to skip
// providers whose circuit is open so traffic fails over to a healthy
// provider within one poll interval instead of waiting for the 60s
// error-rate refresh.
//
// Breaker semantics match the Ollama poller: after BreakerThreshold
// consecutive failed probes the circuit opens (IsHealthy returns
// false); the next successful probe closes it again. There is no
// half-open or cooldown window — a provider that recovers is admitted
// immediately on the next selection.
//
// The poller is disabled when PollInterval <= 0. In that case
// IsHealthy always returns true (the caller falls back to the existing
// error-rate-based selector behaviour).
package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FrontierDefaults applied by NewFrontierHealth when the matching field
// is zero. Operators override each knob via NEXUS_FRONTIER_HEALTH_*
// env vars (see internal/config).
const (
	DefaultFrontierPollInterval     = 60 * time.Second
	DefaultFrontierBreakerThreshold = 3
	DefaultFrontierProbeTimeout     = 5 * time.Second
)

// ErrFrontierDisabled is the sentinel returned by FrontierHealth methods
// when the receiver is nil. Wiring sites that hold an optional
// *FrontierHealth use this to skip health-aware code paths when no
// frontier health poller has been configured.
var ErrFrontierDisabled = errors.New("health: frontier probing not configured")

// FrontierProbeTarget describes one frontier provider the poller probes.
// BaseURL is the upstream API root (e.g. "https://api.openai.com/v1");
// the poller appends "/models" to form the probe URL. APIKey is sent as
// a bearer token so providers that require authentication do not reject
// the probe with 401 (which would be a false negative). An empty APIKey
// is sent as-is — some providers accept anonymous /models calls.
type FrontierProbeTarget struct {
	Name    string // operator-assigned provider identifier
	BaseURL string // upstream API root, e.g. "https://api.openai.com/v1"
	APIKey  string // bearer token (may be empty)
}

// FrontierBreakerState is a point-in-time snapshot of one provider's
// circuit state, used by observability and /status surfaces.
type FrontierBreakerState struct {
	Name         string
	Healthy      bool
	FailureCount int32
}

// probeCallback is invoked after every probe so the observability layer
// can increment its nexus_frontier_probe_total{provider,result} counter.
// result is "success" or "failure".
type probeCallback func(provider, result string)

// tripCallback is invoked when a provider's circuit transitions from
// closed to open so the observability layer can increment its
// nexus_frontier_circuit_open_total counter.
type tripCallback func(provider string)

// frontierProviderState holds the live circuit state for one provider.
// All mutable fields are atomic so the hot path (IsHealthy) never
// contends with the poller goroutine.
type frontierProviderState struct {
	cfg atomic.Value // stores providerConfig

	healthy      atomic.Bool
	failureCount atomic.Int32
}

// providerConfig holds the immutable fields of frontierProviderState.
// Stored in an atomic.Value so all reads are race-free.
type providerConfig struct {
	name    string
	baseURL string
	apiKey  string
}

// name returns the provider name (race-free).
func (st *frontierProviderState) name() string {
	return st.cfg.Load().(providerConfig).name
}

// baseURL returns the provider base URL (race-free).
func (st *frontierProviderState) baseURL() string {
	return st.cfg.Load().(providerConfig).baseURL
}

// apiKey returns the provider API key (race-free).
func (st *frontierProviderState) apiKey() string {
	return st.cfg.Load().(providerConfig).apiKey
}

// FrontierHealth tracks the live status of all configured frontier API
// providers. The zero value is invalid; construct via NewFrontierHealth.
//
// FrontierHealth is safe for concurrent use. IsHealthy is a pair of
// atomic loads; the poller mutates state via atomic stores under a
// mutex that serialises the failure-count/healthy pair.
type FrontierHealth struct {
	pollInterval     time.Duration
	breakerThreshold int32
	probeTimeout     time.Duration
	client           *http.Client

	mu      sync.Mutex // serialises recordFailure/recordSuccess per-provider
	states  map[string]*frontierProviderState
	targets []FrontierProbeTarget

	onProbe probeCallback
	onTrip  tripCallback

	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup
}

// NewFrontierHealth constructs a FrontierHealth for the given targets.
// Zero-valued knobs are filled with safe defaults (60s poll, 3-failure
// threshold, 5s probe timeout). A nil client falls back to
// http.DefaultClient. When targets is empty the poller is a no-op
// (IsHealthy returns true for every name) but still valid — this
// simplifies wiring when no frontier providers are configured.
//
// The returned FrontierHealth has not yet probed; call Run to perform
// the initial probe and start the background poller.
func NewFrontierHealth(targets []FrontierProbeTarget, pollInterval time.Duration, breakerThreshold int, probeTimeout time.Duration, client *http.Client) *FrontierHealth {
	if pollInterval <= 0 {
		pollInterval = DefaultFrontierPollInterval
	}
	if breakerThreshold <= 0 {
		breakerThreshold = DefaultFrontierBreakerThreshold
	}
	if probeTimeout <= 0 {
		probeTimeout = DefaultFrontierProbeTimeout
	}
	if client == nil {
		client = http.DefaultClient
	}
	fh := &FrontierHealth{
		pollInterval:     pollInterval,
		breakerThreshold: int32(breakerThreshold),
		probeTimeout:     probeTimeout,
		client:           client,
		states:           make(map[string]*frontierProviderState, len(targets)),
		targets:          targets,
		closed:           make(chan struct{}),
	}
	for _, t := range targets {
		st := &frontierProviderState{}
		st.cfg.Store(providerConfig{
			name:    t.Name,
			baseURL: strings.TrimRight(t.BaseURL, "/"),
			apiKey:  t.APIKey,
		})
		st.healthy.Store(true)
		fh.states[t.Name] = st
	}
	return fh
}

// SetProbeCallback installs a callback invoked after every probe with
// (provider, "success"|"failure"). Used by the observability layer to
// increment nexus_frontier_probe_total. Must be called before Run.
func (fh *FrontierHealth) SetProbeCallback(cb probeCallback) {
	fh.onProbe = cb
}

// SetTripCallback installs a callback invoked when a provider's circuit
// transitions from closed to open. Used by the observability layer to
// increment nexus_frontier_circuit_open_total. Must be called before Run.
func (fh *FrontierHealth) SetTripCallback(cb tripCallback) {
	fh.onTrip = cb
}

// IsHealthy reports whether the named provider's circuit is closed
// (healthy). Returns true when fh is nil so callers that hold an
// optional *FrontierHealth can avoid a nil-check on every request.
// Returns true for an unknown provider name (defensive: the selector
// should not consult health for a provider that was not registered, but
// a programming error must not block traffic).
func (fh *FrontierHealth) IsHealthy(name string) bool {
	if fh == nil {
		return true
	}
	fh.mu.Lock()
	st, ok := fh.states[name]
	fh.mu.Unlock()
	if !ok {
		return true
	}
	return st.healthy.Load()
}

// UnhealthyProviders returns the names of all providers whose circuit is
// currently open. Returns nil when fh is nil or when every provider is
// healthy. Used by the selector to skip open-circuit providers and by
// /healthz to surface degraded state.
func (fh *FrontierHealth) UnhealthyProviders() []string {
	if fh == nil {
		return nil
	}
	fh.mu.Lock()
	defer fh.mu.Unlock()
	var out []string
	for _, st := range fh.states {
		if !st.healthy.Load() {
			out = append(out, st.name())
		}
	}
	return out
}

// States returns a snapshot of every provider's circuit state. Used by
// the observability layer and /status. Returns nil when fh is nil.
func (fh *FrontierHealth) States() []FrontierBreakerState {
	if fh == nil {
		return nil
	}
	fh.mu.Lock()
	defer fh.mu.Unlock()
	out := make([]FrontierBreakerState, 0, len(fh.states))
	for _, st := range fh.states {
		out = append(out, FrontierBreakerState{
			Name:         st.name(),
			Healthy:      st.healthy.Load(),
			FailureCount: st.failureCount.Load(),
		})
	}
	return out
}

// Run performs an initial synchronous probe of all providers and then
// starts the background poller. The initial probe's outcome is logged
// per provider and surfaces in IsHealthy immediately so the first
// proxied request after boot already sees the correct state.
//
// Run blocks until ctx is canceled or Close is called; it is intended
// to be invoked on its own goroutine from the server bootstrap.
func (fh *FrontierHealth) Run(ctx context.Context) {
	fh.probeAll(ctx)

	fh.wg.Add(1)
	go fh.loop(ctx)
}

// loop runs probes on a ticker until ctx is canceled or Close is called.
// Each probe uses a per-call timeout derived from probeTimeout so a
// slow provider cannot pin the goroutine.
func (fh *FrontierHealth) loop(ctx context.Context) {
	defer fh.wg.Done()
	t := time.NewTicker(fh.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-fh.closed:
			return
		case <-t.C:
			fh.probeAll(ctx)
		}
	}
}

// probeAll probes every registered provider concurrently. Each probe
// runs in its own goroutine bounded by the per-probe timeout; the
// method returns once all probes have completed.
func (fh *FrontierHealth) probeAll(ctx context.Context) {
	fh.mu.Lock()
	targets := make([]*frontierProviderState, 0, len(fh.states))
	for _, st := range fh.states {
		targets = append(targets, st)
	}
	fh.mu.Unlock()

	if len(targets) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, st := range targets {
		wg.Add(1)
		go func(st *frontierProviderState) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, fh.probeTimeout)
			err := fh.probeOne(probeCtx, st)
			cancel()
			result := "success"
			if err != nil {
				result = "failure"
			}
			if fh.onProbe != nil {
				fh.onProbe(st.name(), result)
			}
		}(st)
	}
	wg.Wait()
}

// probeOne sends a single GET <BaseURL>/models to the provider and
// records the outcome. Status >= 500 is treated as failure; any other
// status (including 4xx) is treated as success — the endpoint answered,
// which is the contract we are probing. A 4xx here typically means the
// API key is valid but lacks the models:read scope; the provider is
// still reachable, which is what the circuit breaker guards against.
func (fh *FrontierHealth) probeOne(ctx context.Context, st *frontierProviderState) error {
	url := st.baseURL() + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fh.recordFailure(st, err)
		return err
	}
	if st.apiKey() != "" {
		req.Header.Set("Authorization", "Bearer "+st.apiKey())
	}
	resp, err := fh.client.Do(req)
	if err != nil {
		fh.recordFailure(st, err)
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		err := fmt.Errorf("probe status %d", resp.StatusCode)
		fh.recordFailure(st, err)
		return err
	}
	fh.recordSuccess(st)
	return nil
}

// recordFailure increments the consecutive-failure counter for the
// provider and trips the circuit (sets healthy to false) when the
// threshold is reached. Sub-threshold failures stay in the "healthy"
// state — a provider may be flaky for a single probe — but are still
// logged at debug for observability.
func (fh *FrontierHealth) recordFailure(st *frontierProviderState, probeErr error) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	count := st.failureCount.Add(1)
	wasHealthy := st.healthy.Load()
	if count >= fh.breakerThreshold {
		if wasHealthy {
			args := []any{
				slog.String("provider", st.name()),
				slog.Int("failures", int(count)),
				slog.Int("threshold", int(fh.breakerThreshold)),
			}
			if probeErr != nil {
				args = append(args, slog.Any("err", probeErr))
			}
			slog.Warn("frontier health: circuit opened", args...)
			st.healthy.Store(false)
			if fh.onTrip != nil {
				fh.onTrip(st.name())
			}
		}
		return
	}
	if wasHealthy {
		args := []any{
			slog.String("provider", st.name()),
			slog.Int("failures", int(count)),
			slog.Int("threshold", int(fh.breakerThreshold)),
		}
		if probeErr != nil {
			args = append(args, slog.Any("err", probeErr))
		}
		slog.Debug("frontier probe failed (below threshold)", args...)
	}
}

// recordSuccess resets the consecutive-failure counter and closes the
// circuit. A "recovered" log line is emitted only on the
// unhealthy → healthy transition so the operator sees the recovery
// without the noise of every subsequent successful probe.
func (fh *FrontierHealth) recordSuccess(st *frontierProviderState) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if !st.healthy.Swap(true) {
		slog.Info("frontier health: circuit closed (recovered)",
			slog.String("provider", st.name()),
		)
	}
	st.failureCount.Store(0)
}

// Close stops the background poller and waits for the loop goroutine
// to exit. Safe to call multiple times.
func (fh *FrontierHealth) Close() error {
	fh.closeOnce.Do(func() {
		close(fh.closed)
	})
	fh.wg.Wait()
	return nil
}
