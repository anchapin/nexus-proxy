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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	// ChatModel, when non-empty, restricts fetchModelContext to the
	// loaded model matching this name (typically cfg.LocalModel). This
	// prevents a resident embedding model (e.g. nomic-embed-text with an
	// 8192 context) from shrinking the chat-route guardrail below the
	// chat model's real window. When empty, fetchModelContext keeps the
	// legacy "smallest context across all loaded models" behaviour.
	ChatModel string

	// chatModelWarnOnce guards the one-shot "chat model not resident"
	// warning so a missing chat model does not spam the log on every
	// poll cycle. The zero value is ready to use.
	chatModelWarnOnce sync.Once
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
	freeVRAM, sysfsErr := readFreeVRAMBytes(sysfsRoot)

	switch {
	case err != nil && sysfsErr != nil:
		return Budget{Source: SourceStatic, BytesPerToken: bpt},
			fmt.Errorf("%w: ollama=%w sysfs=%w", ErrNoSignal, err, sysfsErr)
	case err != nil:
		// Ollama unreachable but VRAM is read; budget from VRAM only.
		toks := vramBytesToTokens(freeVRAM, bpt)
		return Budget{
			Tokens:        toks,
			FreeVRAMBytes: freeVRAM,
			BytesPerToken: bpt,
			Source:        SourceSysfs,
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
			Tokens:        toks,
			ModelContext:  modelCtx,
			FreeVRAMBytes: freeVRAM,
			BytesPerToken: bpt,
			Source:        source,
		}, nil
	}
}

// fetchModelContext calls Ollama /api/ps and returns the context
// window of the loaded chat model.
//
// When OllamaProbe.ChatModel is non-empty the result is restricted to
// loaded models whose name matches ChatModel (issue #490). This avoids
// a resident embedding model (nomic-embed-text, 8192 context) shrinking
// the chat-route guardrail below the chat model's real window. When no
// resident model matches ChatModel, (0, "", err) is returned so Budget
// falls through to the static fallback rather than silently adopting an
// embedder's context.
//
// When ChatModel is empty the legacy "smallest context across all loaded
// models" rule applies — the conservative minimum keeps the guardrail
// honest when the operator has multiple chat models resident (KV cache
// is per-model and fragmenting VRAM across more than one model shrinks
// each one's safe headroom).
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
	// When a chat model is pinned, only consider loaded models that
	// match it; embedding models are intentionally excluded so they
	// cannot shrink the chat guardrail (issue #490). Matching is a
	// strict full-name equality — broader family matching is
	// intentionally out of scope to keep the filter predictable.
	if p.ChatModel != "" {
		minCtx := 0
		var minName string
		for _, m := range raw.Models {
			if !modelMatchesChat(m.Name, p.ChatModel) {
				continue
			}
			if m.ContextLength <= 0 {
				continue
			}
			if minCtx == 0 || m.ContextLength < minCtx {
				minCtx = m.ContextLength
				minName = m.Name
			}
		}
		if minCtx == 0 {
			p.chatModelWarnOnce.Do(func() {
				slog.Warn("vram probe: configured chat model not resident; budget falls back to static guardrail",
					slog.String("chat_model", p.ChatModel),
				)
			})
			return 0, "", fmt.Errorf("probe: chat model %q not resident in /api/ps", p.ChatModel)
		}
		return minCtx, minName, nil
	}
	// Legacy path: pick the smallest context_length so the budget is
	// the worst case across every loaded model.
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

// modelMatchesChat reports whether a resident model name corresponds
// to the configured chat model. The match is strict full equality:
// the configured NEXUS_LOCAL_MODEL must exactly equal the name
// Ollama reports in /api/ps. Broader prefix, base-name, or family
// matching is intentionally out of scope (issue #490) to keep the
// filter predictable and avoid silently adopting a sibling model's
// context. Operators running a quantisation variant should set
// NEXUS_LOCAL_MODEL to the exact resident tag.
func modelMatchesChat(loaded, chat string) bool {
	return loaded != "" && chat != "" && loaded == chat
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
	driPath := sysfsRoot
	if driPath == "" {
		driPath = DefaultSysfsRoot
	}
	entries, err := os.ReadDir(driPath)
	if err != nil {
		return 0, fmt.Errorf("probe: read %s: %w", driPath, err)
	}
	var total, used int64
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
		total += t
		used += tu
		seen = true
	}
	if !seen {
		slog.Info("probe: no AMD GPU nodes found under /sys/class/dri — is an AMD GPU present?")
		return 0, nil
	}
	return total - used, nil
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
