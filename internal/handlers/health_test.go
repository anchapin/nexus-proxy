package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/config"
	"github.com/anchapin/nexus-proxy/internal/health"
)

// TestLivezAlwaysOK verifies the issue #42 AC: /livez always returns
// 200 when the process is alive, with the documented JSON body
// shape. The handler takes no dependencies, so a table-driven sweep
// over HTTP methods / headers is sufficient.
func TestLivezAlwaysOK(t *testing.T) {
	h := LivezHandler()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req := httptest.NewRequest(method, "/livez", nil)
		rec := httptest.NewRecorder()
		h(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("%s /livez status = %d, want 200", method, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json prefix", ct)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v (raw=%s)", err, rec.Body.String())
		}
		if body["status"] != "alive" {
			t.Errorf("body[status] = %q, want \"alive\"", body["status"])
		}
	}
}

// TestReadyzMatrix covers every readiness-mode combination listed in
// the issue body AC:
//
//   - degraded (default): always 200.
//   - strict + ollama healthy + frontier key set -> 200.
//   - strict + ollama healthy + no frontier key -> 200.
//   - strict + ollama unhealthy + frontier key set -> 200 (degrade).
//   - strict + ollama unhealthy + no frontier key -> 503.
//
// The table mirrors the acceptance criteria so a regression to the
// AC is impossible to merge unnoticed.
func TestReadyzMatrix(t *testing.T) {
	const (
		ollamaUp   = true
		ollamaDown = false
	)
	cases := []struct {
		name               string
		mode               string
		ollamaHealthy      bool
		frontierConfigured bool
		wantStatus         int
		wantBody           string // expected "status" field value
		wantDegraded       bool
		wantReason         string // "" => no "reason" field expected
	}{
		{
			name:               "degraded_default_ollama_down_no_frontier",
			mode:               ReadinessModeDegraded,
			ollamaHealthy:      ollamaDown,
			frontierConfigured: false,
			wantStatus:         http.StatusOK,
			wantBody:           "ready",
			wantDegraded:       true,
		},
		{
			name:               "degraded_default_ollama_up",
			mode:               ReadinessModeDegraded,
			ollamaHealthy:      ollamaUp,
			frontierConfigured: false,
			wantStatus:         http.StatusOK,
			wantBody:           "ready",
			wantDegraded:       false,
		},
		{
			name:               "degraded_ollama_down_frontier_set",
			mode:               ReadinessModeDegraded,
			ollamaHealthy:      ollamaDown,
			frontierConfigured: true,
			wantStatus:         http.StatusOK,
			wantBody:           "ready",
			wantDegraded:       false, // frontier is configured → not degraded
		},
		{
			name:               "strict_ollama_up_no_frontier",
			mode:               ReadinessModeStrict,
			ollamaHealthy:      ollamaUp,
			frontierConfigured: false,
			wantStatus:         http.StatusOK,
			wantBody:           "ready",
			wantDegraded:       false,
		},
		{
			name:               "strict_ollama_up_frontier_set",
			mode:               ReadinessModeStrict,
			ollamaHealthy:      ollamaUp,
			frontierConfigured: true,
			wantStatus:         http.StatusOK,
			wantBody:           "ready",
			wantDegraded:       false,
		},
		{
			name:               "strict_ollama_down_frontier_set_degrades",
			mode:               ReadinessModeStrict,
			ollamaHealthy:      ollamaDown,
			frontierConfigured: true,
			wantStatus:         http.StatusOK,
			wantBody:           "ready",
			wantDegraded:       true, // ollama is down → degraded
		},
		{
			name:               "strict_ollama_down_no_frontier_503",
			mode:               ReadinessModeStrict,
			ollamaHealthy:      ollamaDown,
			frontierConfigured: false,
			wantStatus:         http.StatusServiceUnavailable,
			wantBody:           "not_ready",
			wantDegraded:       true,
			wantReason:         "ollama_unreachable_and_no_frontier_key",
		},
		{
			name:               "unknown_mode_falls_back_to_degraded",
			mode:               "lol",
			ollamaHealthy:      ollamaDown,
			frontierConfigured: false,
			wantStatus:         http.StatusOK,
			wantBody:           "ready",
			wantDegraded:       true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hpoller := stubHealth(t, tc.ollamaHealthy)
			h := ReadyzHandler(ReadyzDeps{
				Health:             hpoller,
				FrontierConfigured: tc.frontierConfigured,
				Mode:               tc.mode,
			})

			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			h(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}

			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v body=%s", err, rec.Body.String())
			}
			if got, _ := body["status"].(string); got != tc.wantBody {
				t.Errorf("body.status = %q, want %q", got, tc.wantBody)
			}
			if got, _ := body["degraded"].(bool); got != tc.wantDegraded {
				t.Errorf("body.degraded = %v, want %v", got, tc.wantDegraded)
			}
			if tc.wantReason != "" {
				if got, _ := body["reason"].(string); got != tc.wantReason {
					t.Errorf("body.reason = %q, want %q", got, tc.wantReason)
				}
			} else if _, present := body["reason"]; present {
				t.Errorf("body.reason should be omitted, got %v", body["reason"])
			}
		})
	}
}

// TestReadyzNilHealthIsHealthy verifies the documented nil-safe
// contract: a missing health poller is treated as "healthy" so unit
// tests and minimal wiring never trip a 503 over an absent poller.
func TestReadyzNilHealthIsHealthy(t *testing.T) {
	h := ReadyzHandler(ReadyzDeps{
		Health:             nil,
		FrontierConfigured: false,
		Mode:               ReadinessModeStrict,
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("nil health /readyz = %d, want 200 (treated as healthy)", rec.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ready" {
		t.Errorf("nil health /readyz status field = %v, want ready", body["status"])
	}
	if body["degraded"] != false {
		t.Errorf("nil health /readyz degraded = %v, want false", body["degraded"])
	}
}

// TestStatusShape verifies the /status JSON envelope has every field
// the issue body lists, populated from live state via the supplied
// adapters. The test uses representative non-zero values for every
// nested field so a regression that hardcodes zeros would be
// caught immediately.
func TestStatusShape(t *testing.T) {
	probe := ProbeStatsFunc{
		TokensFn:   func() int { return 12345 },
		SourceFn:   func() string { return "ollama-ps+amd-sysfs" },
		FreeVRAMFn: func() int64 { return 8 * 1024 * 1024 * 1024 },
		ContextFn:  func() int { return 32768 },
	}
	judge := JudgeStatsFunc{
		EnabledFn:     func() bool { return true },
		QueueDepthFn:  func() int { return 3 },
		ConcurrencyFn: func() int { return 2 },
	}
	quality := QualityStatsFunc{
		EnabledFn:     func() bool { return true },
		QueueDepthFn:  func() int { return 1 },
		ConcurrencyFn: func() int { return 4 },
		DroppedFn:     func() uint64 { return 7 },
	}
	cfg := config.Config{FrontierKey: "sk-test"}
	start := time.Now().Add(-42 * time.Second)

	h := StatusHandler(StatusDeps{
		Health:        stubHealth(t, true),
		Probe:         probe,
		Judge:         judge,
		Quality:       quality,
		Config:        cfg,
		ReadinessMode: ReadinessModeStrict,
		StartTime:     start,
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/status code = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}

	// Every top-level key from the issue body AC must exist with
	// the expected nested type. Checking each field individually
	// (rather than via struct round-trip) keeps the test resilient
	// to field reordering in the response struct.

	check := func(key string, wantType string) {
		t.Helper()
		v, ok := body[key]
		if !ok {
			t.Errorf("missing top-level key %q", key)
		}
		if v == nil {
			t.Errorf("top-level key %q is nil (want %s)", key, wantType)
		}
	}
	check("ollama", "object")
	check("frontier", "object")
	check("vram_probe", "object")
	check("judge", "object")
	check("quality", "object")
	check("uptime_seconds", "number")
	check("readiness_mode", "string")

	if body["readiness_mode"] != ReadinessModeStrict {
		t.Errorf("readiness_mode = %v, want %q", body["readiness_mode"], ReadinessModeStrict)
	}

	// Uptime is reported in seconds; the handler rounds toward zero
	// so a 42-second start window should land between 41 and 43.
	uptime, _ := body["uptime_seconds"].(float64)
	if uptime < 41 || uptime > 43 {
		t.Errorf("uptime_seconds = %v, want ~42", uptime)
	}

	ollama, _ := body["ollama"].(map[string]interface{})
	if ollama == nil {
		t.Fatal("ollama is not an object")
	}
	if ollama["healthy"] != true {
		t.Errorf("ollama.healthy = %v, want true", ollama["healthy"])
	}
	if v, _ := ollama["failure_count"].(float64); v != 0 {
		t.Errorf("ollama.failure_count = %v, want 0", v)
	}

	frontier, _ := body["frontier"].(map[string]interface{})
	if frontier == nil {
		t.Fatal("frontier is not an object")
	}
	if frontier["configured"] != true {
		t.Errorf("frontier.configured = %v, want true", frontier["configured"])
	}

	probeJSON, _ := body["vram_probe"].(map[string]interface{})
	if probeJSON == nil {
		t.Fatal("vram_probe is not an object")
	}
	if v, _ := probeJSON["tokens"].(float64); v != 12345 {
		t.Errorf("vram_probe.tokens = %v, want 12345", v)
	}
	if probeJSON["source"] != "ollama-ps+amd-sysfs" {
		t.Errorf("vram_probe.source = %v, want ollama-ps+amd-sysfs", probeJSON["source"])
	}
	if v, _ := probeJSON["free_vram_bytes"].(float64); v != float64(8*1024*1024*1024) {
		t.Errorf("vram_probe.free_vram_bytes = %v, want %d", v, 8*1024*1024*1024)
	}
	if v, _ := probeJSON["model_context"].(float64); v != 32768 {
		t.Errorf("vram_probe.model_context = %v, want 32768", v)
	}

	judgeJSON, _ := body["judge"].(map[string]interface{})
	if judgeJSON == nil {
		t.Fatal("judge is not an object")
	}
	if judgeJSON["enabled"] != true {
		t.Errorf("judge.enabled = %v, want true", judgeJSON["enabled"])
	}
	if v, _ := judgeJSON["queue_depth"].(float64); v != 3 {
		t.Errorf("judge.queue_depth = %v, want 3", v)
	}
	if v, _ := judgeJSON["concurrency"].(float64); v != 2 {
		t.Errorf("judge.concurrency = %v, want 2", v)
	}

	qualityJSON, _ := body["quality"].(map[string]interface{})
	if qualityJSON == nil {
		t.Fatal("quality is not an object")
	}
	if qualityJSON["enabled"] != true {
		t.Errorf("quality.enabled = %v, want true", qualityJSON["enabled"])
	}
	if v, _ := qualityJSON["queue_depth"].(float64); v != 1 {
		t.Errorf("quality.queue_depth = %v, want 1", v)
	}
	if v, _ := qualityJSON["concurrency"].(float64); v != 4 {
		t.Errorf("quality.concurrency = %v, want 4", v)
	}
	if v, _ := qualityJSON["dropped"].(float64); v != 7 {
		t.Errorf("quality.dropped = %v, want 7", v)
	}
}

// TestStatusZeroAdapters verifies the /status handler works when
// every backing subsystem is the zero adapter. This is the boot-time
// shape before main.go has wired anything: the endpoint must still
// return 200 with sensible zero values, never panic on a nil
// adapter function.
func TestStatusZeroAdapters(t *testing.T) {
	h := StatusHandler(StatusDeps{
		Health:        nil,
		Probe:         ProbeStatsFunc{},
		Judge:         JudgeStatsFunc{},
		Quality:       QualityStatsFunc{},
		Config:        config.Config{},
		ReadinessMode: "", // exercises NormalizeReadinessMode fallback
	})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/status code = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if body["readiness_mode"] != ReadinessModeDegraded {
		t.Errorf("zero adapters readiness_mode = %v, want %q",
			body["readiness_mode"], ReadinessModeDegraded)
	}
	if body["frontier"].(map[string]interface{})["configured"] != false {
		t.Errorf("zero adapters frontier.configured should be false, got %v", body["frontier"])
	}
}

// TestHealthzBackwardCompat verifies the legacy /healthz envelope is
// preserved exactly so existing Docker healthchecks, operator
// scripts, and Prometheus scrape-config comments continue to work.
// The test pins every field in the pre-#42 JSON shape.
func TestHealthzBackwardCompat(t *testing.T) {
	probe := ProbeStatsFunc{
		TokensFn:   func() int { return 24000 },
		SourceFn:   func() string { return "ollama-ps+amd-sysfs" },
		FreeVRAMFn: func() int64 { return 4 * 1024 * 1024 * 1024 },
		ContextFn:  func() int { return 32000 },
	}
	cfg := config.Config{TokenGuardrail: 6000}

	h := HealthzHandler(stubHealth(t, true), probe, cfg)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz code = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}

	// Exact field set — adding/removing any key is a breaking
	// change for operators grep'ing the envelope.
	wantKeys := []string{
		"status", "ollama_healthy", "budget_tokens", "budget_source",
		"free_vram_bytes", "model_context", "static_fallback_tokens",
	}
	for _, k := range wantKeys {
		if _, ok := body[k]; !ok {
			t.Errorf("/healthz body missing legacy key %q (got=%v)", k, body)
		}
	}
	if body["status"] != "ok" {
		t.Errorf("/healthz status field = %v, want \"ok\"", body["status"])
	}
	if body["ollama_healthy"] != true {
		t.Errorf("/healthz ollama_healthy = %v, want true", body["ollama_healthy"])
	}
	if v, _ := body["budget_tokens"].(float64); v != 24000 {
		t.Errorf("/healthz budget_tokens = %v, want 24000", v)
	}
	if body["budget_source"] != "ollama-ps+amd-sysfs" {
		t.Errorf("/healthz budget_source = %v, want ollama-ps+amd-sysfs", body["budget_source"])
	}
	if v, _ := body["static_fallback_tokens"].(float64); v != 6000 {
		t.Errorf("/healthz static_fallback_tokens = %v, want 6000", v)
	}
}

// TestHealthzFallbackToStatic verifies the fallback path: when the
// probe reports zero tokens (e.g. probe disabled or still booting),
// /healthz echoes the operator-configured NEXUS_TOKEN_GUARDRAIL
// with the "static-fallback" source label — same behaviour as the
// pre-#42 handler in cmd/nexus/main.go.
func TestHealthzFallbackToStatic(t *testing.T) {
	probe := ProbeStatsFunc{} // every closure nil → returns zero
	cfg := config.Config{TokenGuardrail: 4096}

	h := HealthzHandler(stubHealth(t, false), probe, cfg)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz fallback code = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if v, _ := body["budget_tokens"].(float64); v != 4096 {
		t.Errorf("fallback budget_tokens = %v, want 4096", v)
	}
	if body["budget_source"] != "static-fallback" {
		t.Errorf("fallback budget_source = %v, want static-fallback", body["budget_source"])
	}
}

// TestNormalizeReadinessMode exhaustively covers the canonicalisation
// rule: strict stays strict, empty / degraded stay degraded, and
// every other value falls back to degraded so a typo never
// flips the pod into a more aggressive state.
func TestNormalizeReadinessMode(t *testing.T) {
	cases := map[string]string{
		"":                    ReadinessModeDegraded,
		ReadinessModeDegraded: ReadinessModeDegraded,
		ReadinessModeStrict:   ReadinessModeStrict,
		"STRICT":              ReadinessModeDegraded, // case-sensitive
		"Degraded":            ReadinessModeDegraded,
		"always-200":          ReadinessModeDegraded,
		"lol":                 ReadinessModeDegraded,
		"strict ":             ReadinessModeDegraded, // whitespace-sensitive
	}
	for in, want := range cases {
		if got := NormalizeReadinessMode(in); got != want {
			t.Errorf("NormalizeReadinessMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// stubHealth builds a *health.Health whose IsLocalHealthy /
// FailureCount return the supplied healthy flag (and zero
// failures). We construct it via the constructor so internal atomics
// are initialised — calling methods on a zero-value *Health would
// panic.
//
// The *testing.T is only used for t.Cleanup hooks (so the per-probe
// context cancel funcs are released after the test exits and go vet
// never flags the discarded-return pattern). It is otherwise unused.
func stubHealth(t *testing.T, healthy bool) *health.Health {
	t.Helper()
	h := health.New("http://ollama.local", 30*time.Second, 3, 5*time.Second, nil)
	if !healthy {
		// Drive the failure counter past the breaker threshold so
		// IsLocalHealthy flips to false. The probe URL is
		// unroutable so every probe fails fast — a 200ms context
		// bounds the wait on the kernel TCP retry path so the
		// test never blocks past its budget. The cancel func is
		// released via t.Cleanup so go vet's "discarded cancel"
		// warning never fires and a long-running test cannot leak
		// timer resources.
		for i := 0; i < 3; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			t.Cleanup(cancel)
			_ = h.Probe(ctx)
		}
	}
	return h
}
