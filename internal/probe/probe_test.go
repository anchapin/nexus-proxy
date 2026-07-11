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
	if _, err := readFreeVRAMBytes(dir); err == nil {
		t.Fatal("expected error when no AMD nodes are present")
	}
}

func TestReadFreeVRAMBytesIgnoresConnectors(t *testing.T) {
	dir := t.TempDir()
	// card0-DP-1 should be ignored (it is a connector node, not a GPU).
	if err := os.MkdirAll(filepath.Join(dir, "card0-DP-1", "device"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readFreeVRAMBytes(dir); err == nil {
		t.Fatal("expected error: connector paths must not be counted as GPUs")
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

func TestOllamaProbeBothDownReturnsErrNoSignal(t *testing.T) {
	p := NewOllamaProbe("http://127.0.0.1:1", &http.Client{Timeout: 100 * time.Millisecond})
	p.SysfsRoot = t.TempDir()

	_, err := p.Budget(context.Background())
	if !errors.Is(err, ErrNoSignal) {
		t.Errorf("got %v, want ErrNoSignal", err)
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
