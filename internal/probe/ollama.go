// OllamaProbe is the production Probe implementation (issue #6). It
// queries the local Ollama instance for the loaded model's
// context_length and the AMD GPU sysfs nodes for free VRAM, then
// combines the two into a per-request token budget = min(model
// context, free VRAM-derived safe budget).
//
// AMD-only. NVIDIA support is out of scope per the PRD; the package
// surface (Probe interface + Manager) is intentionally future-proof
// so a follow-up PR can drop in an NVIDIA implementation that
// shells out to `nvidia-smi` without touching the chat hot path.
//
// All side effects are gated behind a single HTTPSysfs+HTTP call; if
// either fails the probe still returns a Budget with whatever
// signal it did collect so a transient sysfs read never makes the
// proxy misbehave.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultSysfsRoot is the conventional /sys/class/dri mount point.
// Tests can override it via OllamaProbe.SysfsRoot to point at a
// synthesised directory.
const DefaultSysfsRoot = "/sys/class/dri"

// OllamaProbe combines an /api/ps call to Ollama with an AMDGPU
// sysfs read. Construct via NewOllamaProbe; the zero value is not
// usable (no OllamaURL).
type OllamaProbe struct {
	OllamaURL     string       // e.g. "http://localhost:11434"; no trailing slash
	Client        *http.Client // nil falls back to http.DefaultClient
	SysfsRoot     string       // root for /sys/class/dri lookups (default DefaultSysfsRoot)
	BytesPerToken int          // VRAM->token conversion factor; 0 falls back to DefaultBytesPerToken
}

// NewOllamaProbe constructs a probe with safe defaults. Pass nil for
// client to use http.DefaultClient; the per-call timeout lives on
// the client itself.
func NewOllamaProbe(ollamaURL string, client *http.Client) *OllamaProbe {
	return &OllamaProbe{
		OllamaURL:     strings.TrimRight(ollamaURL, "/"),
		Client:        client,
		SysfsRoot:     DefaultSysfsRoot,
		BytesPerToken: DefaultBytesPerToken,
	}
}

// Budget implements Probe. The two signals are merged into one
// snapshot: when both are available the budget is the minimum of
// (model context, free VRAM-derived safe tokens). A budget of 0
// with ErrNoSignal is returned when neither signal is reachable.
func (p *OllamaProbe) Budget(ctx context.Context) (Budget, error) {
	bpt := p.BytesPerToken
	if bpt <= 0 {
		bpt = DefaultBytesPerToken
	}
	sysfsRoot := p.SysfsRoot
	if sysfsRoot == "" {
		sysfsRoot = DefaultSysfsRoot
	}

	modelCtx, modelName, err := p.fetchModelContext(ctx)
	freeVRAMPerGPU, sysfsErr := readFreeVRAMBytesPerGPU(sysfsRoot)

	// Compute aggregate total for backward-compatible callers.
	var freeVRAM int64
	for _, v := range freeVRAMPerGPU {
		freeVRAM += v
	}

	switch {
	case err != nil && sysfsErr != nil:
		return Budget{Source: SourceStatic, BytesPerToken: bpt},
			fmt.Errorf("%w: ollama=%w sysfs=%w", ErrNoSignal, err, sysfsErr)
	case err != nil:
		// Ollama unreachable but VRAM is read; budget from VRAM only.
		toks := vramBytesToTokens(freeVRAM, bpt)
		return Budget{
			Tokens:              toks,
			FreeVRAMBytes:       freeVRAM,
			FreeVRAMBytesPerGPU: freeVRAMPerGPU,
			BytesPerToken:       bpt,
			Source:              SourceSysfs,
		}, nil
	case sysfsErr != nil:
		// No sysfs (e.g. macOS dev box) but Ollama answered; trust the model context.
		if modelCtx <= 0 {
			return Budget{Source: SourceStatic, BytesPerToken: bpt},
				fmt.Errorf("%w: no model context (model=%q)", ErrNoSignal, modelName)
		}
		return Budget{
			Tokens:        modelCtx,
			ModelContext:  modelCtx,
			BytesPerToken: bpt,
			Source:        SourceOllamaPS,
		}, nil
	default:
		// Both signals present; pick the more restrictive one.
		fromVRAM := vramBytesToTokens(freeVRAM, bpt)
		toks := modelCtx
		source := SourceOllamaPS
		if fromVRAM > 0 && (toks == 0 || fromVRAM < toks) {
			toks = fromVRAM
			source = SourceSysfs
		}
		if modelCtx > 0 && fromVRAM > 0 {
			source = SourceBoth
		}
		return Budget{
			Tokens:              toks,
			ModelContext:        modelCtx,
			FreeVRAMBytes:       freeVRAM,
			FreeVRAMBytesPerGPU: freeVRAMPerGPU,
			BytesPerToken:       bpt,
			Source:              source,
		}, nil
	}
}

// fetchModelContext calls Ollama /api/ps and returns the smallest
// context_length reported across all loaded models. The conservative
// "minimum across loaded models" rule keeps the guardrail honest when
// the operator has multiple models resident (KV cache is per-model and
// fragmenting VRAM across more than one model shrinks each one's safe
// headroom).
//
// Returns the context size and (0, "", nil) when no model is loaded,
// or (0, "", err) on transport / decode failure.
func (p *OllamaProbe) fetchModelContext(ctx context.Context) (int, string, error) {
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	if p.OllamaURL == "" {
		return 0, "", errors.New("probe: empty OllamaURL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.OllamaURL+"/api/ps", nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("ollama /api/ps: status %d", resp.StatusCode)
	}
	var raw struct {
		Models []struct {
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return 0, "", fmt.Errorf("ollama /api/ps: decode: %w", err)
	}
	if len(raw.Models) == 0 {
		return 0, "", nil
	}
	// Pick the smallest context_length so the budget is the worst case.
	minCtx := 0
	var minName string
	for _, m := range raw.Models {
		if m.ContextLength <= 0 {
			continue
		}
		if minCtx == 0 || m.ContextLength < minCtx {
			minCtx = m.ContextLength
			minName = m.Name
		}
	}
	return minCtx, minName, nil
}

// readFreeVRAMBytes walks the sysfs DRI tree for amdgpu nodes and
// sums free VRAM across every GPU it finds. The amdgpu driver
// exposes two naming schemes for backward compatibility:
//
//   - new (Linux 5.10+): /sys/class/dri/cardN/device/mem_info_vram_total,
//     mem_info_vram_used
//   - old (older kernels): /sys/class/dri/cardN/mem_total_vram,
//     mem_used_vram
//
// We try the new scheme first, then fall back to the legacy one,
// so the probe still works on long-lived kernel pins. NVIDIA's
// /sys/class/dri/cardN paths also exist on Tegra but the file names
// differ; that's why NVIDIA is out of scope (per PRD) for now.
func readFreeVRAMBytes(sysfsRoot string) (int64, error) {
	perGPU, err := readFreeVRAMBytesPerGPU(sysfsRoot)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, free := range perGPU {
		total += free
	}
	return total, nil
}

// readFreeVRAMBytesPerGPU walks the sysfs DRI tree for amdgpu nodes and
// returns free VRAM broken down by GPU (gpu_id -> free bytes). The gpu_id
// is the sysfs card name, e.g. "card0", "card1". This enables the
// observability collector to emit per-GPU Prometheus gauges with a gpu_id
// label for multi-GPU hosts (issue #394).
//
// The amdgpu driver exposes two naming schemes for backward compatibility:
//
//   - new (Linux 5.10+): /sys/class/dri/cardN/device/mem_info_vram_total,
//     mem_info_vram_used
//   - old (older kernels): /sys/class/dri/cardN/mem_total_vram,
//     mem_used_vram
//
// We try the new scheme first, then fall back to the legacy one,
// so the probe still works on long-lived kernel pins.
func readFreeVRAMBytesPerGPU(sysfsRoot string) (map[string]int64, error) {
	driPath := sysfsRoot
	if driPath == "" {
		driPath = DefaultSysfsRoot
	}
	entries, err := os.ReadDir(driPath)
	if err != nil {
		return nil, fmt.Errorf("probe: read %s: %w", driPath, err)
	}
	result := make(map[string]int64)
	var seen bool
	for _, e := range entries {
		name := e.Name()
		// Only consider render nodes (card0, card1, ...) — connectors
		// (card0-DP-1, ...) do not own VRAM.
		if !strings.HasPrefix(name, "card") || strings.Contains(name, "-") {
			continue
		}
		base := filepath.Join(driPath, name)
		// Probe the new layout under /device/ first.
		t, tu, ok := readAmdVramPair(filepath.Join(base, "device"))
		if !ok {
			t, tu, ok = readAmdVramPair(base)
		}
		if !ok {
			continue
		}
		if tu > t {
			tu = t // defensive; kernel counters briefly disagree under load
		}
		result[name] = t - tu
		seen = true
	}
	if !seen {
		return nil, fmt.Errorf("probe: no AMD sysfs nodes under %s", driPath)
	}
	return result, nil
}

// readAmdVramPair reads (total, used) VRAM in bytes from a sysfs
// base directory. Returns (0, 0, false) when neither the new nor
// the legacy filenames exist — a non-amdgpu node legitimately
// has no VRAM files (e.g. an Intel iGPU path) and should be
// skipped silently.
func readAmdVramPair(base string) (total, used int64, ok bool) {
	// Try modern names first, then legacy.
	t, terr := readIntFile(filepath.Join(base, "mem_info_vram_total"))
	u, uerr := readIntFile(filepath.Join(base, "mem_info_vram_used"))
	if terr != nil || uerr != nil {
		t, terr = readIntFile(filepath.Join(base, "mem_total_vram"))
		u, uerr = readIntFile(filepath.Join(base, "mem_used_vram"))
		if terr != nil || uerr != nil {
			return 0, 0, false
		}
	}
	return t, u, true
}

// readIntFile reads a single non-negative integer from a sysfs file.
// Returns the read error unmodified so callers can distinguish "file
// missing" from "file empty". Empty / whitespace-only files yield 0.
func readIntFile(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("probe: parse %s: %w", path, err)
	}
	if n < 0 {
		return 0, nil
	}
	return n, nil
}

// vramBytesToTokens converts a free-VRAM byte count into a token
// budget using the configured heuristic. The result is floored at
// zero so a nonsensical VRAM reading can never produce a negative
// budget.
func vramBytesToTokens(bytes int64, bytesPerToken int) int {
	if bytes <= 0 || bytesPerToken <= 0 {
		return 0
	}
	t := int(bytes) / bytesPerToken
	if t < 0 {
		return 0
	}
	return t
}
