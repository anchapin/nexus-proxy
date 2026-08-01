// Package testutil provides shared test helpers that are safe to use from
// parallel tests across packages.
package testutil

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"
)

// FaultMode selects how a FaultableServer responds to the next request.
// Tests toggle the mode at runtime (via SetMode) to inject failures into an
// otherwise-healthy mock upstream — the foundation of the chaos/fault-injection
// harness (issue #1187).
type FaultMode int

const (
	// ModeHealthy serves a valid OpenAI-style completion on
	// /v1/chat/completions and valid Ollama probe responses on
	// /api/tags and /api/chat. This is the steady-state mode.
	ModeHealthy FaultMode = iota
	// ModeCrash responds with HTTP 502 Bad Gateway on every path. Used to
	// simulate a crashed upstream that the cascade must fall back from.
	ModeCrash
	// ModeSlow sleeps for Delay before serving the (otherwise-healthy)
	// response. When Delay exceeds the caller's context deadline the
	// request times out — used to verify cascade-timeout fallback.
	ModeSlow
	// ModePartial writes a partial response body then forcibly closes the
	// underlying TCP connection so the client observes a mid-stream reset.
	// GET /api/tags (which health.go closes without reading) falls back to
	// a 502 so the health probe still fails deterministically.
	ModePartial
	// Mode500 responds with HTTP 500 Internal Server Error on every path.
	// Used to drive repeated failures that trip the health circuit breaker.
	Mode500
)

// HealthyChatCompletion is the OpenAI-compatible non-streaming completion body
// served by ModeHealthy on the chat-completions path.
const HealthyChatCompletion = `{"model":"local-m","choices":[{"index":0,"message":{"role":"assistant","content":"ok from local"},"finish_reason":"stop"}]}`

// FaultableServer is an httptest.Server whose handler closure can be toggled
// between fault modes at runtime. It serves three paths so it can stand in for
// both the Ollama health-probe endpoints (/api/tags, /api/chat) and the chat
// completions endpoint (/v1/chat/completions or any other POST path).
//
// FaultableServer is safe for concurrent use: SetMode takes a write lock and
// the request handler takes a read lock, so a mode switch is observed
// atomically by in-flight and subsequent requests.
//
// It is intended for the chaos/fault-injection e2e harness (issue #1187) and
// is general enough to reuse from any package's tests.
type FaultableServer struct {
	*httptest.Server

	mu    sync.RWMutex
	mode  FaultMode
	delay time.Duration

	// requests counts every inbound request across all paths. Exposed so
	// tests can assert that an endpoint was (or was not) hit — e.g. verify
	// the cascade skipped local entirely after a cooldown armed.
	requests atomic.Int64
}

// NewFaultableServer starts a FaultableServer in ModeHealthy. The caller owns
// its lifetime: defer srv.Close() (or t.Cleanup) to release the listener.
func NewFaultableServer() *FaultableServer {
	f := &FaultableServer{mode: ModeHealthy}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// SetMode atomically switches the fault mode observed by subsequent requests.
func (f *FaultableServer) SetMode(m FaultMode) {
	f.mu.Lock()
	f.mode = m
	f.mu.Unlock()
}

// SetDelay sets the delay applied by ModeSlow. It is a no-op for other modes.
func (f *FaultableServer) SetDelay(d time.Duration) {
	f.mu.Lock()
	f.delay = d
	f.mu.Unlock()
}

// Mode returns the currently active fault mode.
func (f *FaultableServer) Mode() FaultMode {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.mode
}

// Requests returns the total number of inbound requests observed across all
// paths since the server started.
func (f *FaultableServer) Requests() int64 {
	return f.requests.Load()
}

// handle is the dispatch entry point installed as the httptest.Server handler.
func (f *FaultableServer) handle(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	f.mu.RLock()
	mode := f.mode
	delay := f.delay
	f.mu.RUnlock()

	// ModeSlow applies the delay up front for every path. When the delay
	// exceeds the request's context deadline the client observes a timeout
	// (the handler returns after ctx cancellation). The select on
	// r.Context().Done() lets in-flight requests unwind promptly when the
	// cascade cancels its per-attempt context.
	if mode == ModeSlow && delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	if mode == ModeHealthy {
		serveHealthy(w, r)
		return
	}

	// ModePartial truncates the body mid-stream for POST paths (chat +
	// completions) which both read the response body, forcing a read error.
	// /api/tags is probed via a body-less GET, so it falls back to 502 to
	// ensure the health check still fails deterministically.
	if mode == ModePartial {
		if r.Method == http.MethodPost {
			hijackAndReset(w)
			return
		}
		http.Error(w, "chaos: partial", http.StatusBadGateway)
		return
	}

	// ModeCrash and Mode500 map to deterministic 5xx status codes, both of
	// which health.go and the cascade treat as retryable failures.
	status := http.StatusBadGateway
	if mode == Mode500 {
		status = http.StatusInternalServerError
	}
	http.Error(w, "chaos: fault injected", status)
}

// serveHealthy writes the valid response for each known path. Unknown paths
// are treated as the chat-completions endpoint (the cascade POSTs to
// /v1/chat/completions; tests may also dial the bare base URL).
func serveHealthy(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/tags":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	case "/api/chat":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":{"content":"ok"}}`))
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(HealthyChatCompletion))
	}
}

// hijackAndReset takes over the underlying TCP connection, writes a partial
// HTTP response (declaring a large Content-Length but sending only a few
// bytes), then closes the connection. Any client that attempts to read the
// body observes an unexpected EOF / connection reset — the mid-stream crash
// the chaos harness needs to exercise.
func hijackAndReset(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		// Fallback for servers that don't support hijacking (none of the
		// stdlib httptest servers fall into this category, but be safe).
		http.Error(w, "chaos: hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, "chaos: hijack failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = conn.Close() }()
	// Declare a large Content-Length so the client expects more bytes than
	// we send; closing mid-body forces the read error.
	_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 512\r\n\r\n{\"choices\":[{\"message\":{\"role\":\"ass")
	_ = bufrw.Flush()
}
