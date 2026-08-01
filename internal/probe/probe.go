// Package probe implements dynamic VRAM-aware token budgeting for the
// chat router (issue #6). It replaces the original static
// `NEXUS_TOKEN_GUARDRAIL` heuristic with a live measurement of the
// local Ollama instance and the host's free VRAM, while preserving
// the static value as a last-ditch fallback when neither probe
// source is reachable.
//
// The package is intentionally small: a Probe interface, a single
// implementation (OllamaProbe) that queries the live Ollama
// /api/ps endpoint plus the AMD GPU sysfs nodes, and a Manager
// that runs the probe on boot and on a ticker and surfaces the
// latest snapshot through an atomic.Pointer so the chat hot path
// never blocks on I/O.
//
// Stdlib-only by design — the rest of the proxy already uses slog +
// net/http + os.Open; pulling in extra dependencies just to read
// sysfs would not earn its keep.
package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Source identifies where a Budget's tokens value came from. The
// handler logs the source alongside the budget so operators can
// see whether the dynamic probe is doing real work or whether the
// static fallback is in play.
type Source string

const (
	// SourceOllamaPS means the budget was bounded by the loaded
	// model's context_length from /api/ps.
	SourceOllamaPS Source = "ollama-ps"
	// SourceSysfs means the budget was bounded by free VRAM
	// reported by the AMD GPU sysfs nodes.
	SourceSysfs Source = "amd-sysfs"
	// SourceBoth means both probes contributed and the budget is
	// min(model-context, free-vram-tokens). This is the common
	// production case.
	SourceBoth Source = "ollama-ps+amd-sysfs"
	// SourceNVIDIA means the budget's free-VRAM figure was last
	// updated by a periodic nvidia-smi refresh (issue #1178). The
	// AMD sysfs path does not run on NVIDIA-only hosts, so without
	// the refresh FreeVRAMBytes would be frozen at boot.
	SourceNVIDIA Source = "nvidia-smi"
	// SourceStatic means the dynamic probe could not produce a
	// budget and the caller is falling back to
	// NEXUS_TOKEN_GUARDRAIL.
	SourceStatic Source = "static-fallback"
)

// GPUInfo holds per-GPU VRAM data reported by nvidia-smi (issue #775).
type GPUInfo struct {
	Index       int // GPU index from nvidia-smi (0, 1, ...)
	Name        string
	MemoryFree  int64 // bytes free on this GPU
	MemoryTotal int64 // bytes total on this GPU
}

// Budget is one snapshot of the per-request token budget the chat
// router should use to decide whether a prompt is too large for the
// local model.
//
// Tokens is the union of the two signals: min(loaded model context,
// free VRAM-derived safe budget). When neither signal is available
// Tokens is zero and Source is SourceStatic so the caller can fall
// back to the static NEXUS_TOKEN_GUARDRAIL.
type Budget struct {
	Tokens        int   // the budget the router should use
	ModelContext  int   // context_length from /api/ps, 0 if unknown
	FreeVRAMBytes int64 // free VRAM in bytes, 0 if unknown
	BytesPerToken int   // bytes-per-token heuristic used to convert VRAM -> tokens
	Source        Source
	// PerGPU holds per-GPU VRAM readings (issue #775). nil when
	// no NVIDIA GPU is detected; a nil/free slice element means
	// that GPU's free VRAM could not be determined.
	PerGPU []GPUInfo
}

// Disabled reports whether the probe could not produce a budget.
// Callers should treat a disabled budget as "use the static
// fallback configured by the operator".
func (b Budget) Disabled() bool { return b.Tokens <= 0 }

// Probe is the minimal interface a probe implementation must satisfy.
// OllamaProbe is the only production implementation today, but the
// interface lets tests substitute deterministic stubs (or future
// NVIDIA / llama.cpp backends) without touching the manager.
type Probe interface {
	// Budget returns the current budget snapshot. Implementations
	// must be safe to call from many goroutines; the Manager
	// invokes Budget from a single goroutine but tests may share
	// a Probe across goroutines.
	Budget(ctx context.Context) (Budget, error)
}

// ErrNoSignal is returned when the probe ran but produced no budget
// because every signal it relies on was unavailable (e.g. Ollama
// returned no loaded models AND the sysfs nodes do not exist). The
// Manager logs this at warn level and keeps the previous budget in
// place — operators want the proxy to keep serving traffic with the
// last-known value rather than reject every request after a transient
// probe failure.
var ErrNoSignal = errors.New("probe: no usable signal")

// Defaults applied by NewManager when the matching field is zero.
// Operators can override each knob via NEXUS_PROBE_* env vars (see
// internal/config).
const (
	DefaultPollInterval = 60 * time.Second
	DefaultProbeTimeout = 5 * time.Second
	// DefaultBytesPerToken is the heuristic used to convert a
	// free-VRAM byte count into a safe context budget. 256 KiB
	// per token is conservative for Q4-quantised 7B-8B models
	// on the AMD Vulkan backend; operators with different
	// quantisation profiles can raise or lower the value via
	// NEXUS_PROBE_BYTES_PER_TOKEN. The PRD's target hardware is
	// 8-12 GiB VRAM, where the formula yields ~32k-48k tokens
	// of safe headroom.
	DefaultBytesPerToken = 256 * 1024
	// DefaultThermalThreshold is the GPU junction temperature
	// (degrees Celsius) above which the probe treats the GPU as
	// VRAM-starved (issue #597). 90 °C is the typical AMD Radeon
	// thermal-throttle ceiling; better-cooled cards can raise the
	// knob via NEXUS_PROBE_THERMAL_THRESHOLD, and 0 disables the
	// check entirely.
	DefaultThermalThreshold = 90
)

// Manager owns the lifetime of the periodic probe. It performs an
// initial blocking probe so the first proxied request after boot
// already sees the dynamic budget, then re-polls on a ticker to
// catch thermal-throttle or model-swap events.
//
// Manager is safe for concurrent use. Get returns an atomic snapshot
// and never blocks on I/O.
type Manager struct {
	probe     Probe
	interval  time.Duration
	timeout   time.Duration
	now       func() time.Time // overridable for tests
	closeOnce sync.Once
	closed    chan struct{}
	wg        sync.WaitGroup

	// tickCh receives one signal after every successful
	// latest.Store(&b) inside doProbe. It exists so tests can
	// deterministically wait for the Nth repoll to land before
	// reading Get() — waiting on the stub's call count alone is
	// not enough because doProbe publishes the budget *after* the
	// stub returns. Buffered with slack so the production probe
	// hot path never blocks; production code does not read from
	// this channel. WaitForProbes is the public accessor.
	tickCh chan struct{}

	// nvidiaInterval is the cadence of the periodic NVIDIA VRAM
	// refresh (issue #1178). Zero (the default) means the NVIDIA
	// path is boot-only and no goroutine is started. Set via
	// EnableNVIDIARefresh before Run.
	nvidiaInterval time.Duration
	// nvidiaRefresh returns per-GPU NVIDIA VRAM. It defaults to
	// the package-level ReadPerGPUVRAM; tests override it with a
	// deterministic stub. Guarded so a nil function is treated as
	// "no NVIDIA" rather than panicking.
	nvidiaRefresh func() ([]GPUInfo, error)

	latest atomic.Pointer[Budget]
}

// NewManager constructs a Manager. Zero-valued knobs are filled with
// safe defaults (60s poll, 5s probe timeout). A nil probe is rejected
// because the manager would have nothing to call — callers that want
// "no probe at all" should leave the manager unset on Deps instead.
func NewManager(p Probe, interval, timeout time.Duration) *Manager {
	if p == nil {
		panic("probe: NewManager requires a non-nil Probe")
	}
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	return &Manager{
		probe:    p,
		interval: interval,
		timeout:  timeout,
		now:      time.Now,
		closed:   make(chan struct{}),
		tickCh:   make(chan struct{}, 64),
	}
}

// Get returns the most recently observed budget. It is safe to call
// from any goroutine. The zero Budget is returned before the first
// probe completes — callers should treat Disabled() as "fall back to
// the static guardrail".
func (m *Manager) Get() Budget {
	if m == nil {
		return Budget{Source: SourceStatic}
	}
	if p := m.latest.Load(); p != nil {
		return *p
	}
	return Budget{Source: SourceStatic}
}

// Run performs an initial synchronous probe and then starts the
// background ticker. The initial probe is deliberately blocking so
// the first request after boot already sees the dynamic budget —
// the alternative (lazy load on first request) would inject the
// probe latency into a user's request and surface confusing TTFTs.
//
// Run blocks until ctx is canceled or Close is called; it is
// intended to be invoked on its own goroutine from main, matching
// the pattern already used by internal/health.
//
// Disabled probing: when interval <= 0 the manager performs the
// initial probe and returns without starting the ticker. The
// latest value is still available via Get so operators can inspect
// what the boot probe found.
func (m *Manager) Run(ctx context.Context) {
	m.doProbe(ctx)

	if m.interval <= 0 {
		slog.Info("probe: polling disabled (NEXUS_PROBE_INTERVAL=0); boot snapshot only",
			slog.Any("budget", m.Get()),
		)
	} else {
		slog.Info("probe: running",
			slog.Duration("interval", m.interval),
			slog.Duration("timeout", m.timeout),
		)
		m.wg.Add(1)
		go m.loop(ctx)
	}

	// Periodic NVIDIA VRAM refresh (issue #1178). On NVIDIA-only
	// hosts the AMD sysfs path returns nothing, so the budget's
	// FreeVRAMBytes would otherwise be frozen at boot for the whole
	// process lifetime. When enabled, a goroutine shells out to
	// nvidia-smi on this cadence and republishes the budget so the
	// VRAM-aware limiter (internal/concurrencylimit) sees model-swap
	// and co-tenant VRAM-grab events. Boot-only when the interval is
	// zero — no goroutine, no extra fork cost.
	if m.nvidiaInterval > 0 {
		slog.Info("probe: periodic NVIDIA VRAM refresh enabled",
			slog.Duration("interval", m.nvidiaInterval),
		)
		m.wg.Add(1)
		go m.nvidiaLoop(ctx)
	}
}

// loop runs probes on a ticker until ctx is canceled or Close is
// called.
func (m *Manager) loop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.closed:
			return
		case <-t.C:
			m.doProbe(ctx)
		}
	}
}

// EnableNVIDIARefresh arms a periodic NVIDIA VRAM refresh (issue
// #1178). It must be called before Run. A non-positive interval is a
// no-op so the boot-only behaviour is preserved when the operator
// leaves NEXUS_PROBE_NVIDIA_INTERVAL unset. The refresh function
// defaults to the package-level ReadPerGPUVRAM; callers in the same
// package can override Manager.nvidiaRefresh with a deterministic stub.
func (m *Manager) EnableNVIDIARefresh(interval time.Duration) {
	if m == nil || interval <= 0 {
		return
	}
	m.nvidiaInterval = interval
	if m.nvidiaRefresh == nil {
		m.nvidiaRefresh = ReadPerGPUVRAM
	}
}

// nvidiaLoop runs mergeNVIDIA on a ticker until ctx is canceled or
// Close is called. It is the NVIDIA-side companion to loop; the two
// run independently because the AMD sysfs poll and the nvidia-smi
// fork have very different cost profiles and cadences.
func (m *Manager) nvidiaLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(m.nvidiaInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.closed:
			return
		case <-t.C:
			m.mergeNVIDIA()
		}
	}
}

// mergeNVIDIA reads the current NVIDIA VRAM snapshot and republishes
// the budget with the NVIDIA contribution merged in (issue #1178). On
// NVIDIA-only hosts this is the only source of free VRAM, so the
// published FreeVRAMBytes (read on every Acquire by the concurrency
// limiter) would otherwise stay frozen at boot. Missing nvidia-smi or
// an empty GPU list is a silent no-op so AMD-only and headless hosts
// are unaffected.
//
// The NVIDIA sum is authoritative: when nvidia-smi reports GPUs the
// published FreeVRAMBytes is overwritten. This is correct on
// NVIDIA-only hosts (the AMD sysfs value is 0) and on hosts where
// Ollama runs against CUDA; mixed AMD+NVIDIA hosts are out of scope
// and the hard ceiling (NEXUS_LOCAL_MAX_CONCURRENT) remains the
// ultimate safety bound regardless of the VRAM reading.
func (m *Manager) mergeNVIDIA() {
	refresh := m.nvidiaRefresh
	if refresh == nil {
		refresh = ReadPerGPUVRAM
	}
	gpus, err := refresh()
	if err != nil || len(gpus) == 0 {
		// Missing nvidia-smi / no NVIDIA GPU — nothing to merge.
		return
	}
	var nvidiaFree int64
	for _, g := range gpus {
		if g.MemoryFree > 0 {
			nvidiaFree += g.MemoryFree
		}
	}
	if nvidiaFree <= 0 {
		return
	}

	cur := m.Get()
	merged := cur
	merged.PerGPU = gpus
	merged.FreeVRAMBytes = nvidiaFree
	merged.BytesPerToken = cur.BytesPerToken
	if merged.BytesPerToken <= 0 {
		merged.BytesPerToken = DefaultBytesPerToken
	}
	// Keep the budget internally consistent: re-derive Tokens from
	// the (now NVIDIA-driven) free VRAM and the retained model
	// context, mirroring the min() rule in OllamaProbe.Budget.
	fromVRAM := vramBytesToTokens(merged.FreeVRAMBytes, merged.BytesPerToken)
	toks := merged.ModelContext
	if fromVRAM > 0 && (toks == 0 || fromVRAM < toks) {
		toks = fromVRAM
	}
	merged.Tokens = toks
	merged.Source = SourceNVIDIA

	m.latest.Store(&merged)
	m.signalTick()
	slog.Info("probe updated",
		slog.String("source", string(merged.Source)),
		slog.Int("budget_tokens", merged.Tokens),
		slog.Int("model_context", merged.ModelContext),
		slog.Int64("free_vram_bytes", merged.FreeVRAMBytes),
		slog.Int("nvidia_gpus", len(gpus)),
	)
}

// Probe runs a single probe synchronously and returns the resulting
// budget. Exported so tests and ad-hoc operators can drive the
// manager without spinning up the goroutine.
func (m *Manager) Probe(ctx context.Context) (Budget, error) {
	if m == nil {
		return Budget{Source: SourceStatic}, errors.New("probe: manager not configured")
	}
	return m.probe.Budget(ctx)
}

// doProbe is the unexported, side-effecting probe used by Run and
// loop. It copies the result via the atomic pointer; probe errors
// keep the previous budget in place so a transient failure doesn't
// make the handler over- or under-estimate.
func (m *Manager) doProbe(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	b, err := m.probe.Budget(pctx)
	if err != nil {
		slog.Warn("probe failed", slog.Any("err", err))
		return
	}
	if b.Disabled() {
		slog.Warn("probe produced no budget; keeping previous value",
			slog.Any("err", ErrNoSignal),
			slog.Int("model_context", b.ModelContext),
			slog.Int64("free_vram", b.FreeVRAMBytes),
		)
		return
	}
	m.latest.Store(&b)
	m.signalTick()
	slog.Info("probe updated",
		slog.String("source", string(b.Source)),
		slog.Int("budget_tokens", b.Tokens),
		slog.Int("model_context", b.ModelContext),
		slog.Int64("free_vram_bytes", b.FreeVRAMBytes),
	)
}

// signalTick delivers a non-blocking signal on tickCh so test
// observers (WaitForProbes) can confirm the latest budget is now
// published. The buffer is sized so production code never blocks;
// when the buffer is full the signal is dropped, which only
// affects tests that fell behind by more than 64 ticks.
func (m *Manager) signalTick() {
	select {
	case m.tickCh <- struct{}{}:
	default:
	}
}

// WaitForProbes returns a channel that closes once at least n
// successful probes have been published through latest.Store. It
// is the test-side companion to doProbe and replaces time.Sleep
// polling in tests that need to wait for a repoll before reading
// Get(). Returns a closed channel when n <= 0.
func (m *Manager) WaitForProbes(n int) <-chan struct{} {
	out := make(chan struct{})
	if n <= 0 {
		close(out)
		return out
	}
	go func() {
		defer close(out)
		for i := 0; i < n; i++ {
			<-m.tickCh
		}
	}()
	return out
}

// Close stops the background ticker and waits for the loop goroutine
// to exit. Safe to call multiple times. Always-noop on a nil receiver
// so wiring sites that hold an optional Manager do not need a guard.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		close(m.closed)
	})
	m.wg.Wait()
	return nil
}

// StaticBudget returns a Budget whose Source is SourceStatic so the
// caller can log a coherent "fell back to NEXUS_TOKEN_GUARDRAIL"
// line even when no dynamic probe was configured. Exposed because
// the chat handler uses it on the "no BudgetObserver wired" path.
func StaticBudget(tokens int) Budget {
	return Budget{Tokens: tokens, Source: SourceStatic}
}

// ReadPerGPUVRAM shells out to nvidia-smi and returns per-GPU VRAM
// info (issue #775). The command is:
//
//	nvidia-smi --query-gpu=index,name,memory.free,memory.total
//	  --format=csv,noheader,nounits
//
// Returns nil when nvidia-smi is unavailable or returns a non-zero
// exit code (e.g. no NVIDIA GPU present). The error is only non-nil
// when the command could not be executed at all; a successful command
// that finds no GPUs returns (nil, nil).
func ReadPerGPUVRAM() ([]GPUInfo, error) {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.free,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, nil // no NVIDIA GPU or nvidia-smi not installed
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	gpus := make([]GPUInfo, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		// CSV fields: index, name, memory.free (MiB), memory.total (MiB)
		fields := strings.Split(line, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) < 4 {
			continue
		}
		idx, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		free, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			free = 0
		}
		total, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			total = 0
		}
		gpus = append(gpus, GPUInfo{
			Index:       idx,
			Name:        fields[1],
			MemoryFree:  free * 1024 * 1024, // nvidia-smi reports MiB
			MemoryTotal: total * 1024 * 1024,
		})
	}
	return gpus, nil
}

// GPUCount returns the number of NVIDIA GPUs detected via nvidia-smi.
// Returns 0 when nvidia-smi is unavailable or reports no GPUs.
func GPUCount() int {
	gpus, err := ReadPerGPUVRAM()
	if err != nil || len(gpus) == 0 {
		return 0
	}
	return len(gpus)
}

// String renders the budget for log/JSON use. Kept tiny so the
// /healthz handler can include it without pulling in encoding/json.
func (b Budget) String() string {
	if b.Disabled() {
		return fmt.Sprintf("disabled(source=%s)", b.Source)
	}
	return fmt.Sprintf("tokens=%d source=%s model_context=%d free_vram=%d bytes_per_token=%d",
		b.Tokens, b.Source, b.ModelContext, b.FreeVRAMBytes, b.BytesPerToken)
}
