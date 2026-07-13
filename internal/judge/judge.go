// Package judge implements the asynchronous LLM-as-a-Judge evaluator
// described in issue #15.
//
// Design goals (per the issue and the AGENTS.md constraints):
//
//   - The chat hot path (internal/handlers, internal/upstream) must NOT
//     import this package. Handlers expose a tiny event surface — the
//     JudgeObserver hook on handlers.Deps — and the judge plugs into it
//     from cmd/nexus/main.go. This keeps the hot path lean and unit
//     testable without spinning up a worker pool.
//
//   - Bounded concurrency: at most N judge calls run at once, where N is
//     NEXUS_JUDGE_CONCURRENCY (default 2). Additional submissions go to
//     a buffered channel; overflow is dropped (non-blocking enqueue) and
//     logged so the operator can spot saturation.
//
//   - One attempt per sampled request. Parse failures produce a
//     JudgeScore with Score == 0 and Err != nil, which a future
//     telemetry/SQLite Storage (#16) persists as JudgeScore = NULL.
//
//   - Stdlib-only. The judge rides the same http.Client plumbing as a
//     normal frontier call, so tests can inject the same
//     upstream.RecordingTransport used elsewhere.
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anchapin/nexus-proxy/internal/budget"
)

// Sample is the input that triggers one judge attempt: the original
// user instruction and the model output we want scored. It is the
// "event payload" the handlers package hands to the evaluator; nothing
// outside this package needs to know how it is consumed.
type Sample struct {
	RequestID   string // optional caller-supplied correlation id
	Instruction string // latest user prompt (verbatim)
	Output      string // full streamed local-model response
	LocalModel  string // which local model produced Output
}

// JudgeScore is the structured record produced by one judge attempt.
// A Score of 0 with Err set means the parse failed; the row should be
// persisted as JudgeScore = NULL. Cost is a rough USD estimate derived
// from token counts * NEXUS_JUDGE_COST_PER_1K.
type JudgeScore struct {
	RequestID   string
	Score       int
	RawResponse string
	Cost        float64
	PromptTok   int
	OutputTok   int
	Err         error
	Timestamp   time.Time
}

// Storage persists JudgeScore records. A future PR will supply a
// SQLite-backed implementation (issue #16); for now the in-memory
// MemoryStorage satisfies the interface so the wiring is complete.
type Storage interface {
	Record(score JudgeScore) error
	Close() error
}

// HTTPClient is the minimal HTTP capability the judge needs. *http.Client
// satisfies it; tests can pass any compatible fake.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config tunes an Evaluator. Zero values get safe defaults applied by
// NewEvaluator so callers can construct an evaluator from a partial
// config without exploding.
type Config struct {
	URL         string        // frontier endpoint for judge calls
	Model       string        // judge model name
	APIKey      string        // bearer token; empty = no Authorization header
	SampleRate  float64       // 0..1; <=0 disables sampling
	Concurrency int           // max parallel judge calls (default 2)
	QueueDepth  int           // buffered channel size (default 64)
	Timeout     time.Duration // per-call judge timeout (default 30s)
	CostPer1K   float64       // USD per 1k tokens (input+output); default 0.002
	BudgetGuard *budget.Guard  // optional budget guard to record judge costs
}

// applyDefaults fills zero fields with sane values. It mutates cfg.
func (c *Config) applyDefaults() {
	if c.Concurrency <= 0 {
		c.Concurrency = 2
	}
	if c.QueueDepth <= 0 {
		c.QueueDepth = 64
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.CostPer1K <= 0 {
		c.CostPer1K = 0.002
	}
}

// Evaluator runs judge calls asynchronously with bounded concurrency.
// Construct with NewEvaluator; call Enqueue from request goroutines;
// call Close during graceful shutdown to drain.
type Evaluator struct {
	cfg     Config
	client  HTTPClient
	storage Storage

	queue     chan Sample
	wg        sync.WaitGroup
	rngMu     sync.Mutex
	rng       *rand.Rand
	closed    chan struct{}
	closeOnce sync.Once
}

// NewEvaluator wires the evaluator and starts its worker pool. The
// workers live until Close is called.
//
// If cfg.SampleRate <= 0 the evaluator is dormant: Sample returns
// false, Enqueue is a no-op, and Close returns immediately. This lets
// callers wire the judge unconditionally without checking config.
func NewEvaluator(cfg Config, client HTTPClient, storage Storage) *Evaluator {
	cfg.applyDefaults()
	if client == nil {
		client = http.DefaultClient
	}
	if storage == nil {
		storage = noopStorage{}
	}
	e := &Evaluator{
		cfg:     cfg,
		client:  client,
		storage: storage,
		queue:   make(chan Sample, cfg.QueueDepth),
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		closed:  make(chan struct{}),
	}
	if cfg.SampleRate <= 0 {
		// Dormant evaluator: do not start workers.
		close(e.closed)
		return e
	}
	for i := 0; i < cfg.Concurrency; i++ {
		e.wg.Add(1)
		go e.worker()
	}
	return e
}

// Enabled reports whether the evaluator will actually run judge calls.
// Useful for logging in main and for tests.
func (e *Evaluator) Enabled() bool {
	if e == nil {
		return false
	}
	return e.cfg.SampleRate > 0
}

// Sample returns true if a fresh request should be enqueued for judge
// evaluation, given the configured sample rate. It is the canonical
// "Sample" entry point listed in the issue's acceptance criteria.
//
// Sample is safe to call from many goroutines concurrently.
func (e *Evaluator) Sample() bool {
	if !e.Enabled() {
		return false
	}
	e.rngMu.Lock()
	r := e.rng.Float64()
	e.rngMu.Unlock()
	return r < e.cfg.SampleRate
}

// Enqueue is the non-blocking submit used by the chat handler. It is
// the canonical "Record (dispatch)" entry point: the row update
// happens asynchronously inside the worker pool.
//
// Returns false if the queue is full or the evaluator has been closed.
// Callers may log this; we deliberately do not block the request.
func (e *Evaluator) Enqueue(s Sample) bool {
	if !e.Enabled() {
		return false
	}
	select {
	case <-e.closed:
		return false
	default:
	}
	select {
	case e.queue <- s:
		return true
	default:
		slog.Warn("judge queue full, dropped request", slog.String("request_id", s.RequestID))
		return false
	}
}

// Close drains the queue and stops the worker pool. Safe to call
// multiple times.
func (e *Evaluator) Close() error {
	e.closeOnce.Do(func() {
		close(e.queue)
	})
	e.wg.Wait()
	return e.storage.Close()
}

// QueueDepth returns the current number of buffered, unjudged samples.
// Mostly useful for tests and operational dashboards.
func (e *Evaluator) QueueDepth() int { return len(e.queue) }

// Concurrency returns the configured worker count.
func (e *Evaluator) Concurrency() int { return e.cfg.Concurrency }

// worker pulls from the queue and runs Evaluate + Record until the
// queue is closed and drained.
func (e *Evaluator) worker() {
	defer e.wg.Done()
	for s := range e.queue {
		score := e.evaluate(s)
		if err := e.storage.Record(score); err != nil {
			slog.Error("judge storage record",
				slog.String("request_id", s.RequestID),
				slog.Any("err", err),
			)
		}
		// Wire judge cost into the budget guard (issue #240).
		if e.cfg.BudgetGuard != nil && score.Cost > 0 {
			e.cfg.BudgetGuard.Record(score.Cost, "judge")
		}
	}
}

// Evaluate runs one judge attempt synchronously and returns the
// resulting JudgeScore (recording into Storage is the caller's job).
// This is the canonical "Evaluate" entry point from the acceptance
// criteria and is exported so tests and future CLI tools can drive it
// without spinning up the worker pool.
func (e *Evaluator) Evaluate(ctx context.Context, s Sample) JudgeScore {
	if e == nil || !e.Enabled() {
		return JudgeScore{RequestID: s.RequestID, Err: errors.New("judge: disabled")}
	}
	return e.evaluateCtx(ctx, s)
}

func (e *Evaluator) evaluate(s Sample) JudgeScore {
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.Timeout)
	defer cancel()
	return e.evaluateCtx(ctx, s)
}

func (e *Evaluator) evaluateCtx(ctx context.Context, s Sample) JudgeScore {
	score := JudgeScore{RequestID: s.RequestID, Timestamp: time.Now().UTC()}

	prompt := PromptFor(s)
	// Use a struct so the JSON field order is deterministic — Go's
	// map encoder randomises keys, which makes the request harder
	// to snapshot in tests and harder to diff in logs.
	payload, _ := json.Marshal(judgeRequest{
		Model: e.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: judgeSystemPrompt},
			{Role: "user", Content: prompt},
		},
		Stream:  false,
		Options: judgeOptions{Temperature: 0.0},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.URL, bytes.NewReader(payload))
	if err != nil {
		score.Err = fmt.Errorf("judge: build request: %w", err)
		return score
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		score.Err = fmt.Errorf("judge: do: %w", err)
		return score
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		score.Err = fmt.Errorf("judge: read body: %w", err)
		return score
	}
	score.RawResponse = string(body)

	if resp.StatusCode != http.StatusOK {
		score.Err = fmt.Errorf("judge: status %d: %s", resp.StatusCode, body)
		return score
	}

	content, err := extractContent(body)
	if err != nil {
		score.Err = err
		return score
	}

	n, perr := ParseScore(content)
	if perr != nil {
		score.Err = perr
		return score
	}
	score.Score = n
	score.PromptTok = estimateTokens(s.Instruction + "\n" + s.Output)
	score.OutputTok = estimateTokens(content)
	score.Cost = EstimateCost(score.PromptTok, score.OutputTok, e.cfg.CostPer1K)
	return score
}

// Record is a public convenience wrapper around the Storage.Record
// call. It exists so the three acceptance-criteria entry points
// (Sample, Evaluate, Record) can be exercised independently — e.g. a
// one-shot CLI driver that calls Evaluate then Record.
func (e *Evaluator) Record(score JudgeScore) error {
	if e == nil || e.storage == nil {
		return errors.New("judge: no storage configured")
	}
	return e.storage.Record(score)
}

// judgeSystemPrompt instructs the frontier judge to return a single
// integer 1..5. Kept short and deterministic so a sloppy frontier
// model does not free-style an essay.
const judgeSystemPrompt = `You are an expert code reviewer. Read the user's instruction and the model's response, then output ONLY a single integer from 1 to 5 (inclusive) representing the overall quality. Do not output any other text, punctuation, or explanation. The integer must appear alone on a line.`

// PromptFor renders the user-message half of the judge prompt. It is
// exported so tests can assert "prompt contains the original
// instruction" without round-tripping through HTTP.
func PromptFor(s Sample) string {
	var b strings.Builder
	b.WriteString("User instruction:\n")
	b.WriteString(strings.TrimSpace(s.Instruction))
	b.WriteString("\n\nModel output")
	if s.LocalModel != "" {
		b.WriteString(" (from ")
		b.WriteString(s.LocalModel)
		b.WriteString(")")
	}
	b.WriteString(":\n")
	b.WriteString(s.Output)
	b.WriteString("\n\nGrade the correctness, efficiency, and idiomatic style of the code on a 1-5 scale. Reply with only the integer.")
	return b.String()
}

// ParseScore extracts an integer 1..5 from the judge's raw reply. It
// is forgiving: it scans the whole string for the first integer in
// range and ignores surrounding whitespace, punctuation, or chatty
// preambles. Returns an error if no such integer is found.
func ParseScore(raw string) (int, error) {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return 0, errors.New("judge: empty response")
	}
	// Strip a Markdown-style code fence if the model wrapped the
	// integer (some frontier models do this defensively).
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)
	// First try the entire string as a plain integer (the happy path).
	if n, err := strconv.Atoi(cleaned); err == nil {
		if n < 1 || n > 5 {
			return 0, fmt.Errorf("judge: score %d out of range 1..5", n)
		}
		return n, nil
	}
	// Otherwise scan tokens for the first integer in range.
	for _, field := range strings.Fields(cleaned) {
		field = strings.Trim(field, ".,:;!?\"'`")
		if field == "" {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		if n < 1 || n > 5 {
			continue
		}
		return n, nil
	}
	return 0, fmt.Errorf("judge: no integer 1..5 in %q", raw)
}

// EstimateCost computes a rough USD cost given prompt/output token
// counts and a flat per-1k rate. Operators override the rate via
// NEXUS_JUDGE_COST_PER_1K; the value is intentionally rough — the
// issue only requires "even a rough token-estimate × rate".
func EstimateCost(promptTokens, outputTokens int, costPer1K float64) float64 {
	total := promptTokens + outputTokens
	if total <= 0 || costPer1K <= 0 {
		return 0
	}
	return float64(total) / 1000.0 * costPer1K
}

// estimateTokens uses the same "4 chars per token" heuristic the
// routing guardrail uses. It is intentionally rough.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// judgeRequest and friends are typed mirrors of the OpenAI
// chat-completions request shape. We use them instead of map[string]
// any so the on-wire JSON is deterministic.
type judgeRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Options  judgeOptions  `json:"options"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type judgeOptions struct {
	Temperature float64 `json:"temperature"`
}

// extractContent pulls the assistant message content from an
// OpenAI-compatible chat-completions response. It tolerates minor
// shape variations (string content vs. array of parts) so the judge
// works against any frontier endpoint.
func extractContent(body []byte) (string, error) {
	var raw struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("judge: decode: %w", err)
	}
	if len(raw.Choices) == 0 {
		return "", errors.New("judge: empty choices")
	}
	c := raw.Choices[0].Message.Content
	if len(c) == 0 {
		return "", errors.New("judge: empty content")
	}
	// Most responses are bare strings; some endpoints wrap in an array.
	var s string
	if err := json.Unmarshal(c, &s); err == nil {
		return s, nil
	}
	// Fall back to a permissive string repr (strip surrounding quotes).
	return string(c), nil
}

// noopStorage is the default Storage used when callers pass nil —
// keeps NewEvaluator total but renders the evaluator harmless if the
// operator has not wired telemetry yet.
type noopStorage struct{}

func (noopStorage) Record(JudgeScore) error { return nil }
func (noopStorage) Close() error            { return nil }

// MemoryStorage is a thread-safe in-memory Storage for development
// and tests. Production code uses a SQLite-backed implementation
// (issue #16); the interface is identical so swapping is trivial.
type MemoryStorage struct {
	mu     sync.Mutex
	scores []JudgeScore
}

// NewMemoryStorage returns an empty in-memory store.
func NewMemoryStorage() *MemoryStorage { return &MemoryStorage{} }

// Record appends s to the in-memory log.
func (m *MemoryStorage) Record(s JudgeScore) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scores = append(m.scores, s)
	return nil
}

// Scores returns a copy of the recorded scores in insertion order.
func (m *MemoryStorage) Scores() []JudgeScore {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]JudgeScore, len(m.scores))
	copy(out, m.scores)
	return out
}

// Close is a no-op for the in-memory store; included to satisfy the
// Storage interface.
func (m *MemoryStorage) Close() error { return nil }
