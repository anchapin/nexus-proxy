package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubProbe is a deterministic Probe used by the Manager tests.
// Each call dequeues one pre-canned (Budget, error) pair, so the
// test can interleave success and failure calls in a known order
// without races against the Manager's internal serialisation.
type stubProbe struct {
	mu    sync.Mutex
	queue []stubResult
	calls int32
}

// stubResult is one pre-canned Probe return value.
type stubResult struct {
	budget Budget
	err    error
}

func (s *stubProbe) Budget(_ context.Context) (Budget, error) {
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return Budget{}, nil
	}
	r := s.queue[0]
	s.queue = s.queue[1:]
	return r.budget, r.err
}

func (s *stubProbe) push(b Budget, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, stubResult{budget: b, err: err})
}

func (s *stubProbe) callCount() int32 { return atomic.LoadInt32(&s.calls) }

// ---------------------------------------------------------------------
// vramBytesToTokens + readFreeVRAMBytes (pure unit tests)
// ---------------------------------------------------------------------

func TestVramBytesToTokens(t *testing.T) {
	cases := []struct {
		name          string
		bytes         int64
		bytesPerToken int
		want          int
	}{
		{"zero bytes", 0, 256 * 1024, 0},
		{"negative bytes (defensive)", -1, 256 * 1024, 0},
		{"256 KiB per token; 1 GiB free ~ 4096 tokens",
			int64(1) << 30, 256 * 1024, 4096},
		{"8 GiB free at 256 KiB ~ 32768 tokens",
			int64(8) << 30, 256 * 1024, 32768},
		{"zero bytesPerToken returns 0", int64(1) << 30, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := vramBytesToTokens(tc.bytes, tc.bytesPerToken)
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// writeAMDNode synthesises a sysfs card0 device tree under dir.
// Returns the directory it created.
func writeAMDNode(t *testing.T, dir string, total, used int64) {
	t.Helper()
	card := filepath.Join(dir, "card0")
	device := filepath.Join(card, "device")
	if err := os.MkdirAll(device, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite := func(p string, v int64) {
		if err := os.WriteFile(p, []byte(fmt.Sprintf("%d\n", v)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(device, "mem_info_vram_total"), total)
	mustWrite(filepath.Join(device, "mem_info_vram_used"), used)
}

func TestReadFreeVRAMBytesHappyPath(t *testing.T) {
	dir := t.TempDir()
	writeAMDNode(t, dir, int64(16)<<30, int64(2)<<30) // 16 GiB total, 14 GiB free
	free, err := readFreeVRAMBytes(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(14) << 30
	if free != want {
		t.Errorf("got %d free, want %d", free, want)
	}
}

func TestReadFreeVRAMBytesLegacyNames(t *testing.T) {
	dir := t.TempDir()
	card := filepath.Join(dir, "card0")
	if err := os.MkdirAll(card, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite := func(name string, v int64) {
		p := filepath.Join(card, name)
		if err := os.WriteFile(p, []byte(fmt.Sprintf("%d\n", v)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("mem_total_vram", int64(8)<<30)
	mustWrite("mem_used_vram", int64(3)<<30)
	free, err := readFreeVRAMBytes(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(5) << 30
	if free != want {
		t.Errorf("got %d free, want %d", free, want)
	}
}

func TestReadFreeVRAMBytesNoGPUs(t *testing.T) {
	dir := t.TempDir()
	free, err := readFreeVRAMBytes(dir)
	if err != nil {
		t.Fatalf("expected no error when no AMD nodes are present, got: %v", err)
	}
	if free != 0 {
		t.Errorf("got %d free, want 0", free)
	}
}

func TestReadFreeVRAMBytesIgnoresConnectors(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "card0-DP-1", "device"), 0o755); err != nil {
		t.Fatal(err)
	}
	// card0-DP-1 is a connector (not a GPU) and has no VRAM files,
	// so it is skipped and no AMD GPU is found.
	free, err := readFreeVRAMBytes(dir)
	if err != nil {
		t.Fatalf("expected no error when no AMD nodes are present, got: %v", err)
	}
	if free != 0 {
		t.Errorf("got %d free, want 0", free)
	}
}

// TestReadFreeVRAMBytesNoAmdNodes synthesises a sysfs tree with card0
// present but having no VRAM files — the Intel iGPU path (issue #608).
// The function should return (0, nil) and log an info-level message so
// operators can distinguish "no AMD GPU" from "sysfs probe failed".
func TestReadFreeVRAMBytesNoAmdNodes(t *testing.T) {
	dir := t.TempDir()
	card := filepath.Join(dir, "card0")
	// card0 exists but has no VRAM files (Intel iGPU or unrecognised GPU).
	if err := os.MkdirAll(card, 0o755); err != nil {
		t.Fatal(err)
	}
	free, err := readFreeVRAMBytes(dir)
	if err != nil {
		t.Fatalf("expected no error when card exists but has no AMD VRAM files, got: %v", err)
	}
	if free != 0 {
		t.Errorf("got %d free, want 0", free)
	}
}

func TestReadFreeVRAMBytesSumAcrossCards(t *testing.T) {
	dir := t.TempDir()
	// Two cards, each with their own (total, used). The function should
	// sum free across both.
	for _, name := range []string{"card0", "card1"} {
		card := filepath.Join(dir, name, "device")
		if err := os.MkdirAll(card, 0o755); err != nil {
			t.Fatal(err)
		}
		total := int64(8) << 30
		used := int64(1) << 30
		_ = os.WriteFile(filepath.Join(card, "mem_info_vram_total"),
			[]byte(fmt.Sprintf("%d\n", total)), 0o644)
		_ = os.WriteFile(filepath.Join(card, "mem_info_vram_used"),
			[]byte(fmt.Sprintf("%d\n", used)), 0o644)
	}
	free, err := readFreeVRAMBytes(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(14) << 30
	if free != want {
		t.Errorf("got %d free, want %d (sum across cards)", free, want)
	}
}

// ---------------------------------------------------------------------
// readThermalDegrees (issue #597)
// ---------------------------------------------------------------------

// writeHwmonTemp synthesises an AMD hwmon temperature node under a DRI
// card device tree. millidegrees is the raw value written to
// temp1_input following the Linux hwmon convention (90000 == 90 °C).
// The modern amdgpu layout nests the numbered hwmon dir under a hwmon
// parent: cardN/device/hwmon/hwmonM/temp1_input.
func writeHwmonTemp(t *testing.T, dir, card string, millidegrees int64) {
	t.Helper()
	hwmon := filepath.Join(dir, card, "device", "hwmon", "hwmon0")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hwmon, "temp1_input"),
		[]byte(fmt.Sprintf("%d\n", millidegrees)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadThermalDegreesHappyPath(t *testing.T) {
	dir := t.TempDir()
	// 75000 millidegrees == 75 °C.
	writeHwmonTemp(t, dir, "card0", 75000)
	temp, err := readThermalDegrees(dir)
	if err != nil {
		t.Fatal(err)
	}
	if temp != 75 {
		t.Errorf("got %d °C, want 75", temp)
	}
}

func TestReadThermalDegreesMaxAcrossMultipleCards(t *testing.T) {
	dir := t.TempDir()
	// Two cards at different temperatures. The probe must report the
	// hottest card (the one that governs throttling), not a sum: adding
	// temperatures across GPUs has no physical meaning and would
	// false-positive whenever several warm-but-healthy cards coexist.
	writeHwmonTemp(t, dir, "card0", 45000) // 45 °C
	writeHwmonTemp(t, dir, "card1", 95000) // 95 °C
	temp, err := readThermalDegrees(dir)
	if err != nil {
		t.Fatal(err)
	}
	if temp != 95 {
		t.Errorf("got %d °C, want 95 (max across card0=45, card1=95)", temp)
	}
}

func TestReadThermalDegreesParsingAcrossMultipleCards(t *testing.T) {
	dir := t.TempDir()
	// Verify correct parsing of each card's millidegree value and that
	// the aggregation across multiple cards is deterministic.
	writeHwmonTemp(t, dir, "card0", 60000) // 60 °C
	writeHwmonTemp(t, dir, "card1", 88000) // 88 °C
	writeHwmonTemp(t, dir, "card2", 72000) // 72 °C
	temp, err := readThermalDegrees(dir)
	if err != nil {
		t.Fatal(err)
	}
	if temp != 88 {
		t.Errorf("got %d °C, want 88 (max of 60/88/72)", temp)
	}
}

func TestReadThermalDegreesLegacyFlatLayout(t *testing.T) {
	dir := t.TempDir()
	// Some older kernels expose hwmonM directly under device/ rather
	// than nesting it under a hwmon parent. readCardThermal must fall
	// back to that flat layout.
	hwmon := filepath.Join(dir, "card0", "device", "hwmon0")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hwmon, "temp1_input"),
		[]byte("82000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	temp, err := readThermalDegrees(dir)
	if err != nil {
		t.Fatal(err)
	}
	if temp != 82 {
		t.Errorf("got %d °C, want 82 (legacy flat layout)", temp)
	}
}

func TestReadThermalDegreesNoHwmonNodes(t *testing.T) {
	dir := t.TempDir()
	// A VRAM-capable card without a thermal sensor should yield an
	// error so the probe treats the temperature as "unknown" and does
	// not throttle.
	writeAMDNode(t, dir, int64(8)<<30, int64(2)<<30)
	if _, err := readThermalDegrees(dir); err == nil {
		t.Fatal("expected error when no hwmon temp nodes are present")
	}
}

func TestReadThermalDegreesIgnoresConnectors(t *testing.T) {
	dir := t.TempDir()
	// A connector path (card0-DP-1) must never be counted as a GPU.
	if err := os.MkdirAll(filepath.Join(dir, "card0-DP-1", "device", "hwmon", "hwmon0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readThermalDegrees(dir); err == nil {
		t.Fatal("expected error: connector paths must not be counted as thermal sources")
	}
}

// ---------------------------------------------------------------------
// OllamaProbe with httptest stub for /api/ps
// ---------------------------------------------------------------------

// psServer returns canned JSON for /api/ps; models is a list of
// (name, context_length) pairs and a possible /api/ps HTTP status
// override.
func psServer(t *testing.T, status int, models []psModel) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		if status != 0 && status != 200 {
			http.Error(w, "boom", status)
			return
		}
		arr := make([]map[string]any, 0, len(models))
		for _, m := range models {
			arr = append(arr, map[string]any{
				"name":           m.name,
				"context_length": m.contextLength,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"models": arr})
	}))
}

type psModel struct {
	name          string
	contextLength int
}

func TestOllamaProbeApisPSAloneYieldsModelContext(t *testing.T) {
	srv := psServer(t, 0, []psModel{{name: "qwen3-coder:8b", contextLength: 8192}})
	defer srv.Close()
	dir := t.TempDir() // no AMD sysfs nodes; signal degenerates to PS only
	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Source != SourceOllamaPS {
		t.Errorf("source = %q, want %q", b.Source, SourceOllamaPS)
	}
	if b.Tokens != 8192 {
		t.Errorf("tokens = %d, want 8192", b.Tokens)
	}
	if b.ModelContext != 8192 {
		t.Errorf("model context = %d, want 8192", b.ModelContext)
	}
}

func TestOllamaProbePSAndSysfsPicksMinimum(t *testing.T) {
	srv := psServer(t, 0, []psModel{{name: "qwen3-coder:8b", contextLength: 8192}})
	defer srv.Close()

	dir := t.TempDir()
	// 8 GiB total / 4 GiB used -> 4 GiB free -> at 256 KiB/tok = 16384 tokens.
	// Model context is 8192 < 16384, so PS wins.
	writeAMDNode(t, dir, int64(8)<<30, int64(4)<<30)

	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Source != SourceBoth {
		t.Errorf("source = %q, want %q", b.Source, SourceBoth)
	}
	if b.Tokens != 8192 {
		t.Errorf("tokens = %d, want min(8192, 16384)=8192", b.Tokens)
	}

	// Now flip the ratio: model ctx 16384, free VRAM only enough for 4096 tokens.
	srv2 := psServer(t, 0, []psModel{{name: "qwen3-coder:8b", contextLength: 16384}})
	defer srv2.Close()
	// 1 GiB free -> 4096 tokens at 256 KiB/tok.
	writeAMDNode(t, dir, int64(4)<<30, int64(3)<<30)

	p = NewOllamaProbe(srv2.URL, srv2.Client())
	p.SysfsRoot = dir

	b, err = p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Tokens != 4096 {
		t.Errorf("tokens = %d, want min(16384, 4096)=4096", b.Tokens)
	}
}

func TestOllamaProbeMultipleLoadedModelsPicksSmallest(t *testing.T) {
	srv := psServer(t, 0, []psModel{
		{name: "big", contextLength: 32768},
		{name: "small", contextLength: 4096},
		{name: "middle", contextLength: 8192},
	})
	defer srv.Close()
	dir := t.TempDir()
	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Tokens != 4096 {
		t.Errorf("tokens = %d, want 4096 (smallest loaded model)", b.Tokens)
	}
}

func TestOllamaProbeNoLoadedModelFallsBackToSysfs(t *testing.T) {
	srv := psServer(t, 0, nil) // empty models list
	defer srv.Close()
	dir := t.TempDir()
	writeAMDNode(t, dir, int64(8)<<30, int64(2)<<30) // 6 GiB free -> ~24576 tokens

	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Source != SourceSysfs {
		t.Errorf("source = %q, want %q (no model loaded)", b.Source, SourceSysfs)
	}
	if b.Tokens != 6*(1<<30)/(256*1024) {
		t.Errorf("tokens = %d, want %d", b.Tokens, 6*(1<<30)/(256*1024))
	}
}

func TestOllamaProbeOllamaDownFallsBackToSysfs(t *testing.T) {
	dir := t.TempDir()
	writeAMDNode(t, dir, int64(8)<<30, int64(2)<<30)

	p := NewOllamaProbe("http://127.0.0.1:1", &http.Client{Timeout: 100 * time.Millisecond})
	p.SysfsRoot = dir

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Source != SourceSysfs {
		t.Errorf("source = %q, want %q", b.Source, SourceSysfs)
	}
}

func TestOllamaProbeBothDownButSysfsAccessibleNoAmdNodesReturnsNil(t *testing.T) {
	// When sysfs is accessible (empty temp dir) but no AMD nodes found,
	// readFreeVRAMBytes returns (0, nil) and logs an info message.
	// Budget() then falls through to the Ollama-only path and returns
	// nil error (issue #608).
	p := NewOllamaProbe("http://127.0.0.1:1", &http.Client{Timeout: 100 * time.Millisecond})
	p.SysfsRoot = t.TempDir()

	_, err := p.Budget(context.Background())
	if err != nil {
		t.Errorf("expected nil error when sysfs accessible but no AMD nodes, got: %v", err)
	}
}

func TestOllamaProbeBothDownButConfigZeroSysfsReturnsStat(t *testing.T) {
	// When sysfsRoot is empty the probe sees no cards and falls back
	// to SourceStatic + ErrNoSignal — operators on macOS would see this.
	p := NewOllamaProbe("http://127.0.0.1:1", &http.Client{Timeout: 100 * time.Millisecond})
	p.SysfsRoot = ""

	_, err := p.Budget(context.Background())
	if !errors.Is(err, ErrNoSignal) {
		t.Errorf("got %v, want ErrNoSignal", err)
	}
}

func TestOllamaProbeAPIsPSNon200(t *testing.T) {
	srv := psServer(t, 500, []psModel{{name: "x", contextLength: 4096}})
	defer srv.Close()
	dir := t.TempDir()
	// 4 GiB free -> ~16384 tokens at 256 KiB/tok.
	writeAMDNode(t, dir, int64(8)<<30, int64(4)<<30)

	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.Source != SourceSysfs {
		t.Errorf("source = %q, want %q", b.Source, SourceSysfs)
	}
	if b.Tokens <= 0 {
		t.Errorf("tokens = %d, want positive", b.Tokens)
	}
}

func TestOllamaProbeEmptyOllamaURL(t *testing.T) {
	p := &OllamaProbe{OllamaURL: ""}
	_, err := p.Budget(context.Background())
	if err == nil {
		t.Fatal("expected error when OllamaURL is empty")
	}
}

func TestOllamaProbeCustomBytesPerToken(t *testing.T) {
	srv := psServer(t, 0, []psModel{{name: "x", contextLength: 4096}})
	defer srv.Close()
	dir := t.TempDir()
	writeAMDNode(t, dir, int64(2)<<30, int64(1)<<30) // 1 GiB free

	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir
	p.BytesPerToken = 1024 // 1 KiB per token, very generous

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	// 1 GiB free / 1 KiB per token = ~1M tokens, dwarfing the 4096 model ctx.
	if b.Tokens != 4096 || b.Source != SourceBoth {
		t.Errorf("got tokens=%d source=%q, want min(4096, large)=4096 source=ollama-ps+amd-sysfs",
			b.Tokens, b.Source)
	}
}

func TestOllamaProbeNewManagerPanicsOnNilProbe(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewManager(nil) must panic")
		}
	}()
	_ = NewManager(nil, 0, 0)
}

// ---------------------------------------------------------------------
// Manager behaviour
// ---------------------------------------------------------------------

func TestManagerGetBeforeFirstProbeReturnsStatic(t *testing.T) {
	stub := &stubProbe{}
	m := NewManager(stub, time.Hour, time.Second)
	if got := m.Get(); got.Source != SourceStatic || !got.Disabled() {
		t.Errorf("before first probe Get = %+v, want static-disabled", got)
	}
	if stub.callCount() != 0 {
		t.Errorf("probe called %d times before Run", stub.callCount())
	}
}

func TestManagerRunPerformsInitialProbe(t *testing.T) {
	stub := &stubProbe{}
	stub.push(Budget{Tokens: 9000, ModelContext: 9000, Source: SourceOllamaPS}, nil)
	m := NewManager(stub, time.Hour, time.Second)
	m.Run(context.Background())
	defer m.Close()

	if stub.callCount() != 1 {
		t.Errorf("probe called %d times, want exactly 1 (initial probe only)", stub.callCount())
	}
	if got := m.Get(); got.Tokens != 9000 {
		t.Errorf("Get.Tokens = %d, want 9000", got.Tokens)
	}
}

func TestManagerRunWithZeroIntervalDoesNotPoll(t *testing.T) {
	stub := &stubProbe{}
	stub.push(Budget{Tokens: 1, Source: SourceSysfs}, nil)
	stub.push(Budget{Tokens: 2, Source: SourceSysfs}, nil)
	stub.push(Budget{Tokens: 3, Source: SourceSysfs}, nil)
	m := NewManager(stub, 0, time.Second) // interval=0 = polling disabled
	m.Run(context.Background())
	defer m.Close()

	if n := stub.callCount(); n != 1 {
		t.Errorf("probe called %d times, want 1 (no polling on interval=0)", n)
	}
	if got := m.Get(); got.Tokens != 1 {
		t.Errorf("Get.Tokens = %d, want first queued value (1)", got.Tokens)
	}
}

func TestManagerRepollsOnTicker(t *testing.T) {
	stub := &stubProbe{}
	stub.push(Budget{Tokens: 100, Source: SourceSysfs}, nil)
	stub.push(Budget{Tokens: 200, Source: SourceSysfs}, nil)
	stub.push(Budget{Tokens: 300, Source: SourceSysfs}, nil)

	m := NewManager(stub, 10*time.Millisecond, time.Second)
	m.Run(context.Background())
	defer m.Close()

	// Wait deterministically for the third successful probe
	// publication (initial probe + 2 ticker repolls). WaitForProbes
	// fires *after* the Manager has stored the budget in
	// latest, so the subsequent Get() is race-free against the
	// Manager's goroutine — unlike a wait on the stub's call
	// count, which would race Store vs Get(). A watchdog timeout
	// keeps the test from hanging forever if the Manager is
	// genuinely broken; it is a safety net, not the wait.
	select {
	case <-m.WaitForProbes(3):
	case <-time.After(5 * time.Second):
		t.Fatalf("probe only stored %d times within deadline, want >=3 (initial + 2 ticks)", stub.callCount())
	}
	if got := m.Get(); got.Tokens != 300 {
		t.Errorf("final Get.Tokens = %d, want 300 (last queued value)", got.Tokens)
	}
}

func TestWaitForProbesZeroOrNegativeClosesImmediately(t *testing.T) {
	stub := &stubProbe{}
	stub.push(Budget{Tokens: 100, Source: SourceSysfs}, nil)

	m := NewManager(stub, 10*time.Millisecond, time.Second)
	m.Run(context.Background())
	defer m.Close()

	for _, n := range []int{0, -1} {
		ch := m.WaitForProbes(n)
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("WaitForProbes(%d) returned open channel, want closed", n)
			}
		default:
			t.Errorf("WaitForProbes(%d) returned open channel, want closed", n)
		}
	}
}

func TestManagerProbeFailureKeepsPreviousBudget(t *testing.T) {
	stub := &stubProbe{}
	stub.push(Budget{Tokens: 4096, Source: SourceOllamaPS}, nil)
	stub.push(Budget{}, errors.New("simulated transport down"))

	m := NewManager(stub, 5*time.Minute, time.Second)
	m.Run(context.Background())
	defer m.Close()

	if got := m.Get(); got.Tokens != 4096 {
		t.Errorf("after error Get.Tokens = %d, want 4096 (previous value retained)", got.Tokens)
	}
}

func TestManagerProbeErrNoSignalKeepsPreviousBudget(t *testing.T) {
	stub := &stubProbe{}
	stub.push(Budget{Tokens: 4096, Source: SourceOllamaPS}, nil)
	stub.push(Budget{}, fmt.Errorf("%w: simulated", ErrNoSignal))

	m := NewManager(stub, 5*time.Minute, time.Second)
	m.Run(context.Background())
	defer m.Close()

	if got := m.Get(); got.Tokens != 4096 {
		t.Errorf("after ErrNoSignal Get.Tokens = %d, want 4096 (previous retained)", got.Tokens)
	}
}

func TestManagerNilIsNoop(t *testing.T) {
	var m *Manager
	if err := m.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
	if got := m.Get(); got.Source != SourceStatic {
		t.Errorf("nil Get.Source = %q, want static", got.Source)
	}
}

func TestManagerProbeSingleShot(t *testing.T) {
	stub := &stubProbe{}
	stub.push(Budget{Tokens: 8192, ModelContext: 8192, Source: SourceOllamaPS}, nil)
	m := NewManager(stub, time.Hour, time.Second)

	ctx := context.Background()
	b, err := m.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if b.Tokens != 8192 {
		t.Errorf("tokens = %d, want 8192", b.Tokens)
	}
	if b.Source != SourceOllamaPS {
		t.Errorf("source = %q, want %q", b.Source, SourceOllamaPS)
	}
	if stub.callCount() != 1 {
		t.Errorf("stub called %d times, want 1", stub.callCount())
	}
}

func TestManagerProbeReturnsError(t *testing.T) {
	stub := &stubProbe{}
	stub.push(Budget{}, errors.New("simulated upstream failure"))
	m := NewManager(stub, time.Hour, time.Second)

	ctx := context.Background()
	_, err := m.Probe(ctx)
	if err == nil {
		t.Fatal("expected error from Probe, got nil")
	}
	if stub.callCount() != 1 {
		t.Errorf("stub called %d times, want 1", stub.callCount())
	}
}

func TestManagerProbeOnNilManager(t *testing.T) {
	var m *Manager
	ctx := context.Background()
	b, err := m.Probe(ctx)
	if err == nil {
		t.Fatal("expected error from nil.Probe, got nil")
	}
	if b.Source != SourceStatic {
		t.Errorf("source = %q, want %q", b.Source, SourceStatic)
	}
}

func TestStaticBudgetHelper(t *testing.T) {
	b := StaticBudget(6000)
	if b.Tokens != 6000 || b.Source != SourceStatic {
		t.Errorf("StaticBudget(6000) = %+v, want tokens=6000 source=static", b)
	}
	if !b.Disabled() == false {
		t.Error("StaticBudget with positive tokens must NOT be Disabled()")
	}
}

func TestBudgetString(t *testing.T) {
	if got := (Budget{}).String(); !strings.Contains(got, "disabled") {
		t.Errorf("disabled budget string = %q, want substring \"disabled\"", got)
	}
	nonEmpty := Budget{Tokens: 2048, ModelContext: 4096, FreeVRAMBytes: 5 << 30, BytesPerToken: 256 * 1024, Source: SourceBoth}
	got := nonEmpty.String()
	if !strings.Contains(got, "tokens=2048") || !strings.Contains(got, "source=ollama-ps+amd-sysfs") {
		t.Errorf("budget string = %q, want tokens+source substrings", got)
	}
}

// ---------------------------------------------------------------------
// ChatModel filtering (issue #490)
// ---------------------------------------------------------------------

// TestOllamaProbeChatModelFiltersOutEmbedder verifies the core fix:
// when an embedding model (nomic-embed-text, 8192) is resident
// alongside the chat model (qwen3-coder:4b, 32768), scoping the probe
// to the chat model yields the chat model's real context window
// instead of the embedder's smaller one.
func TestOllamaProbeChatModelFiltersOutEmbedder(t *testing.T) {
	srv := psServer(t, 0, []psModel{
		{name: "nomic-embed-text", contextLength: 8192},
		{name: "qwen3-coder:4b", contextLength: 32768},
	})
	defer srv.Close()
	dir := t.TempDir() // no sysfs → signal degenerates to PS only
	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir
	p.ChatModel = "qwen3-coder:4b"

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Tokens != 32768 {
		t.Errorf("tokens = %d, want 32768 (chat model context, not embedder's 8192)", b.Tokens)
	}
	if b.ModelContext != 32768 {
		t.Errorf("model context = %d, want 32768", b.ModelContext)
	}
}

// TestOllamaProbeChatModelEmptyKeepsLegacyMin verifies the
// backwards-compat path: with ChatModel unset the probe still picks
// the smallest context across every loaded model.
func TestOllamaProbeChatModelEmptyKeepsLegacyMin(t *testing.T) {
	srv := psServer(t, 0, []psModel{
		{name: "nomic-embed-text", contextLength: 8192},
		{name: "qwen3-coder:4b", contextLength: 32768},
	})
	defer srv.Close()
	dir := t.TempDir()
	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir
	// ChatModel intentionally left empty → legacy min-across-all.

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Tokens != 8192 {
		t.Errorf("tokens = %d, want 8192 (legacy smallest-across-all)", b.Tokens)
	}
	if b.ModelContext != 8192 {
		t.Errorf("model context = %d, want 8192", b.ModelContext)
	}
}

// TestOllamaProbeChatModelNotResidentReturnsError verifies that when
// ChatModel is set but no matching model is resident,
// fetchModelContext surfaces an error so Budget falls through to the
// static fallback rather than silently adopting an embedder context.
func TestOllamaProbeChatModelNotResidentReturnsError(t *testing.T) {
	srv := psServer(t, 0, []psModel{
		{name: "nomic-embed-text", contextLength: 8192},
	})
	defer srv.Close()
	dir := t.TempDir()
	// Provide sysfs so Budget does not fail with ErrNoSignal — instead
	// it should produce a sysfs-only budget while the model-context
	// signal is treated as unavailable.
	writeAMDNode(t, dir, int64(8)<<30, int64(2)<<30)

	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir
	p.ChatModel = "qwen3-coder:4b" // not in the resident list

	b, err := p.Budget(context.Background())
	// sysfs is available, so Budget succeeds with a sysfs-only budget
	// and the chat-context error is consumed internally.
	if err != nil {
		t.Fatalf("Budget: %v (expected sysfs-only fallback)", err)
	}
	if b.Source != SourceSysfs {
		t.Errorf("source = %q, want %q (sysfs-only fallback)", b.Source, SourceSysfs)
	}
	if b.ModelContext != 0 {
		t.Errorf("model context = %d, want 0 (chat model not resident)", b.ModelContext)
	}
}

// TestOllamaProbeChatModelNotResidentSysfsNoAmdNodesReturnsNil
// confirms that when ChatModel is set, no matching model is resident,
// but sysfs is accessible (empty, no AMD nodes), Budget returns nil
// (issue #608 — operators see the info log instead of a cryptic error).
func TestOllamaProbeChatModelNotResidentSysfsNoAmdNodesReturnsNil(t *testing.T) {
	srv := psServer(t, 0, []psModel{
		{name: "nomic-embed-text", contextLength: 8192},
	})
	defer srv.Close()
	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = t.TempDir()
	p.ChatModel = "qwen3-coder:4b"

	_, err := p.Budget(context.Background())
	if err != nil {
		t.Errorf("expected nil error when sysfs accessible but no AMD nodes, got: %v", err)
	}
}

// TestModelMatchesChat covers the matcher helper directly.
func TestModelMatchesChat(t *testing.T) {
	cases := []struct {
		loaded, chat string
		want         bool
	}{
		{"qwen3-coder:4b", "qwen3-coder:4b", true},
		{"qwen3-coder:4b-q8_0", "qwen3-coder:4b", false}, // strict equality; variant must set LocalModel exactly
		{"nomic-embed-text", "qwen3-coder:4b", false},
		{"qwen3-coder", "qwen3-coder:4b", false},
		{"qwen3-coder:8b", "qwen3-coder:4b", false},
		{"qwen3", "qwen3-coder:4b", false},
		{"", "qwen3-coder:4b", false},
		{"qwen3-coder:4b", "", false},
	}
	for _, tc := range cases {
		got := modelMatchesChat(tc.loaded, tc.chat)
		if got != tc.want {
			t.Errorf("modelMatchesChat(%q,%q) = %v, want %v", tc.loaded, tc.chat, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------
// Thermal throttle integration (issue #597)
// ---------------------------------------------------------------------

// TestOllamaProbeThermalThrottleReducesBudget verifies the core fix:
// when the GPU junction temperature exceeds the configured threshold,
// the budget collapses to zero (Disabled) so the router falls back to
// the static guardrail — even though free VRAM would otherwise report
// ample headroom and a model is resident with a large context window.
func TestOllamaProbeThermalThrottleReducesBudget(t *testing.T) {
	srv := psServer(t, 0, []psModel{{name: "qwen3-coder:8b", contextLength: 32768}})
	defer srv.Close()
	dir := t.TempDir()
	// Plenty of free VRAM (14 GiB) and a large model context — without
	// the thermal check the budget would be 32768 tokens.
	writeAMDNode(t, dir, int64(16)<<30, int64(2)<<30)
	// 95000 millidegrees == 95 °C, above the 80 °C threshold.
	writeHwmonTemp(t, dir, "card0", 95000)

	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir
	p.ThermalThreshold = 80

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if !b.Disabled() {
		t.Errorf("budget not disabled: tokens=%d source=%q, want tokens=0 (thermal clamp)",
			b.Tokens, b.Source)
	}
	if b.Tokens != 0 {
		t.Errorf("tokens = %d, want 0 (budget must collapse under thermal throttle)", b.Tokens)
	}
	if b.FreeVRAMBytes != 0 {
		t.Errorf("free_vram = %d, want 0 (treated as starved under thermal throttle)", b.FreeVRAMBytes)
	}
}

// TestOllamaProbeThermalUnderThresholdKeepsBudget confirms that a
// temperature below the threshold leaves the normal budget untouched.
func TestOllamaProbeThermalUnderThresholdKeepsBudget(t *testing.T) {
	srv := psServer(t, 0, []psModel{{name: "qwen3-coder:8b", contextLength: 8192}})
	defer srv.Close()
	dir := t.TempDir()
	writeAMDNode(t, dir, int64(8)<<30, int64(4)<<30) // 4 GiB free -> 16384 tokens
	// 60000 millidegrees == 60 °C, comfortably below the 80 °C threshold.
	writeHwmonTemp(t, dir, "card0", 60000)

	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir
	p.ThermalThreshold = 80

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	// min(8192 model ctx, 16384 vram) = 8192; thermal check must not fire.
	if b.Tokens != 8192 {
		t.Errorf("tokens = %d, want 8192 (no throttle below threshold)", b.Tokens)
	}
	if b.Source != SourceBoth {
		t.Errorf("source = %q, want %q", b.Source, SourceBoth)
	}
}

// TestOllamaProbeThermalThresholdZeroDisabled verifies that a zero
// threshold disables the check entirely: even a scorching GPU must not
// throttle when the operator opted out. This also documents the
// backwards-compat guarantee — OllamaProbe values constructed without
// setting ThermalThreshold (e.g. the legacy unit tests above) are
// unaffected.
func TestOllamaProbeThermalThresholdZeroDisabled(t *testing.T) {
	srv := psServer(t, 0, []psModel{{name: "x", contextLength: 4096}})
	defer srv.Close()
	dir := t.TempDir()
	writeAMDNode(t, dir, int64(8)<<30, int64(2)<<30)
	writeHwmonTemp(t, dir, "card0", 120000) // 120 °C — would throttle any positive threshold

	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir
	p.ThermalThreshold = 0 // disabled

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Disabled() {
		t.Errorf("budget disabled with ThermalThreshold=0: %+v, want normal budget", b)
	}
	if b.Tokens != 4096 {
		t.Errorf("tokens = %d, want 4096 (thermal check disabled)", b.Tokens)
	}
}

// TestOllamaProbeThermalNoHwmonDoesNotThrottle confirms that a missing
// thermal node (e.g. an Intel iGPU or a VM without hwmon) never trips
// the throttle, even when a threshold is configured. The probe must
// treat "temperature unknown" as "do not throttle".
func TestOllamaProbeThermalNoHwmonDoesNotThrottle(t *testing.T) {
	srv := psServer(t, 0, []psModel{{name: "x", contextLength: 4096}})
	defer srv.Close()
	dir := t.TempDir()
	writeAMDNode(t, dir, int64(8)<<30, int64(2)<<30)
	// No hwmon temp node written.

	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir
	p.ThermalThreshold = 50 // low threshold, but no temp to read

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Disabled() {
		t.Errorf("budget disabled despite unknown temperature: %+v", b)
	}
	if b.Tokens != 4096 {
		t.Errorf("tokens = %d, want 4096 (no throttle when temp unreadable)", b.Tokens)
	}
}

// TestOllamaProbeThermalBoundaryExactThreshold verifies the "exceeds"
// semantics: a temperature exactly at the threshold must NOT throttle
// (the issue says "exceeds", i.e. strictly greater than).
func TestOllamaProbeThermalBoundaryExactThreshold(t *testing.T) {
	srv := psServer(t, 0, []psModel{{name: "x", contextLength: 4096}})
	defer srv.Close()
	dir := t.TempDir()
	writeAMDNode(t, dir, int64(8)<<30, int64(2)<<30)
	// Exactly 80 °C == threshold 80; "exceeds" is strict, so no throttle.
	writeHwmonTemp(t, dir, "card0", 80000)

	p := NewOllamaProbe(srv.URL, srv.Client())
	p.SysfsRoot = dir
	p.ThermalThreshold = 80

	b, err := p.Budget(context.Background())
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	if b.Disabled() {
		t.Errorf("budget disabled at exactly the threshold: %+v, want no throttle (strict exceeds)", b)
	}
}
