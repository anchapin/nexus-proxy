package judge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anchapin/nexus-proxy/internal/budget"
)

// rtFunc is a tiny test double that satisfies both http.RoundTripper
// and the HTTPClient interface. It mirrors the helper used in
// internal/upstream tests so the judge package doesn't need to import
// upstream.RecordingTransport (which would create an import cycle once
// handlers depends on judge, even if judge itself does not depend on
// upstream).
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (f rtFunc) Do(r *http.Request) (*http.Response, error)        { return f(r) }

// newTestEvaluator builds an evaluator wired to a fresh MemoryStorage
// and the given round-tripper. Tests that need custom timeouts set
// cfg.Timeout before calling. A zero SampleRate is interpreted as
// "all-or-nothing" so the happy-path tests do not have to thread
// 1.0 through every Config literal — the disabled-state test builds
// its own evaluator directly so this helper does not stomp on it.
func newTestEvaluator(t *testing.T, cfg Config, fn rtFunc) (*Evaluator, *MemoryStorage) {
	t.Helper()
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 1.0 // tests usually want all-or-nothing
	}
	if cfg.URL == "" {
		cfg.URL = "http://judge.local/v1/chat/completions"
	}
	if cfg.Model == "" {
		cfg.Model = "judge-model"
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 2
	}
	if cfg.QueueDepth == 0 {
		cfg.QueueDepth = 8
	}
	store := NewMemoryStorage()
	e := NewEvaluator(cfg, &http.Client{Transport: fn}, store)
	return e, store
}

// waitForScores polls until the in-memory store has at least n scores
// or the deadline elapses. Bounded to keep tests fast and to surface
// deadlocks quickly.
func waitForScores(t *testing.T, store *MemoryStorage, n int, d time.Duration) []JudgeScore {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		got := store.Scores()
		if len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	return store.Scores()
}

func TestPromptForContainsInstructionAndOutput(t *testing.T) {
	p := PromptFor(Sample{
		RequestID:   "req-1",
		Instruction: "Write fizzbuzz in Python",
		Output:      "for i in range(1, 16):\n    print('FizzBuzz')",
		LocalModel:  "qwen3-coder:8b",
	})
	for _, want := range []string{
		"Write fizzbuzz in Python",
		"FizzBuzz",
		"qwen3-coder:8b",
		"1-5 scale",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q in:\n%s", want, p)
		}
	}
}

func TestParseScore(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{"bare integer", "4", 4, false},
		{"with whitespace", "  3 \n", 3, false},
		{"with period", "5.", 5, false},
		{"inside sentence", "I'd give it a 2 overall.", 2, false},
		{"code fence", "```\n1\n```", 1, false},
		{"too low", "0", 0, true},
		{"too high", "6", 0, true},
		{"no integer", "no number here", 0, true},
		{"empty", "", 0, true},
		{"zero", "0", 0, true},
		{"negative ignored then 4 found", "-1 is bad but 4 is fair", 4, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseScore(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEstimateCost(t *testing.T) {
	if got := EstimateCost(1000, 500, 0.002); got < 0.0029 || got > 0.0031 {
		t.Errorf("EstimateCost(1000,500,0.002) = %v, want ~0.003", got)
	}
	if got := EstimateCost(0, 0, 0.002); got != 0 {
		t.Errorf("zero tokens should yield zero cost, got %v", got)
	}
	if got := EstimateCost(1000, 500, 0); got != 0 {
		t.Errorf("zero rate should yield zero cost, got %v", got)
	}
}

func TestExtractContent(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"4"}}]}`)
	got, err := extractContent(body)
	if err != nil {
		t.Fatalf("extractContent: %v", err)
	}
	if got != "4" {
		t.Errorf("got %q", got)
	}
	if _, err := extractContent([]byte(`not json`)); err == nil {
		t.Error("expected decode error")
	}
	if _, err := extractContent([]byte(`{"choices":[]}`)); err == nil {
		t.Error("expected empty choices error")
	}
}

func TestEvaluatorHappyPath(t *testing.T) {
	var (
		mu        sync.Mutex
		gotAuth   string
		gotURL    string
		gotUser   string // content of the "user" message in the request body
		gotModel  string
		streamOff bool
		tempSeen  bool
		callCount int
	)
	fn := rtFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		callCount++
		gotAuth = r.Header.Get("Authorization")
		gotURL = r.URL.String()
		// Pull the user-role message body out of the OpenAI payload so
		// assertions below check the judge prompt, not the system
		// prompt that precedes it.
		gotUser = extractUserContent(string(body))
		if strings.Contains(string(body), `"model":"judge-model"`) {
			gotModel = "judge-model"
		}
		if strings.Contains(string(body), `"stream":false`) {
			streamOff = true
		}
		if strings.Contains(string(body), `"temperature":0`) {
			tempSeen = true
		}
		resp := `{"choices":[{"message":{"content":"4"}}]}`
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(resp)),
		}, nil
	})

	e, store := newTestEvaluator(t, Config{
		APIKey: "sk-test",
	}, fn)
	defer e.Close()

	s := Sample{
		RequestID:   "req-1",
		Instruction: "fix the typo",
		Output:      "x = 1  # fixed",
		LocalModel:  "qwen3-coder:8b",
	}
	if !e.Enqueue(s) {
		t.Fatal("Enqueue should accept")
	}
	scores := waitForScores(t, store, 1, 2*time.Second)
	if len(scores) != 1 {
		t.Fatalf("got %d scores, want 1", len(scores))
	}
	got := scores[0]
	if got.RequestID != "req-1" {
		t.Errorf("RequestID = %q", got.RequestID)
	}
	if got.Score != 4 {
		t.Errorf("Score = %d, want 4", got.Score)
	}
	if got.Err != nil {
		t.Errorf("Err = %v", got.Err)
	}
	if got.Cost <= 0 {
		t.Error("Cost should be > 0")
	}

	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Errorf("expected 1 HTTP call, got %d", callCount)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if gotURL != "http://judge.local/v1/chat/completions" {
		t.Errorf("URL = %q", gotURL)
	}
	if gotModel != "judge-model" {
		t.Errorf("model field missing in request body")
	}
	if !streamOff {
		t.Errorf("expected stream=false in request")
	}
	if !tempSeen {
		t.Errorf("expected low temperature in request")
	}
	if !strings.Contains(gotUser, "fix the typo") {
		t.Errorf("prompt should contain original instruction, got %q", gotUser)
	}
	if !strings.Contains(gotUser, "x = 1  # fixed") {
		t.Errorf("prompt should contain model output, got %q", gotUser)
	}
}

// extractUserContent finds the "user"-role message body inside an
// OpenAI chat-completions request JSON. Returns "" if not found.
// Adequate for test assertions; production code uses a real JSON
// decoder.
func extractUserContent(body string) string {
	const marker = `"role":"user","content":"`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func TestEvaluatorParseFailureStoresErr(t *testing.T) {
	fn := rtFunc(func(_ *http.Request) (*http.Response, error) {
		resp := `{"choices":[{"message":{"content":"definitely not a number"}}]}`
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(resp)),
		}, nil
	})
	e, store := newTestEvaluator(t, Config{}, fn)
	defer e.Close()

	if !e.Enqueue(Sample{RequestID: "r-bad", Instruction: "x", Output: "y"}) {
		t.Fatal("Enqueue should accept")
	}
	scores := waitForScores(t, store, 1, 2*time.Second)
	if len(scores) != 1 {
		t.Fatalf("got %d scores, want 1", len(scores))
	}
	if scores[0].Err == nil {
		t.Error("expected Err on parse failure, got nil")
	}
	if scores[0].Score != 0 {
		t.Errorf("Score = %d, want 0 on parse failure", scores[0].Score)
	}
	if scores[0].RawResponse == "" {
		t.Error("RawResponse should preserve the bad payload for telemetry")
	}
}

func TestEvaluatorTransportErrorStoresErr(t *testing.T) {
	fn := rtFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("dial fail")
	})
	e, store := newTestEvaluator(t, Config{}, fn)
	defer e.Close()

	e.Enqueue(Sample{RequestID: "r-net", Instruction: "x", Output: "y"})
	scores := waitForScores(t, store, 1, 2*time.Second)
	if len(scores) != 1 {
		t.Fatalf("got %d scores", len(scores))
	}
	if scores[0].Err == nil || !strings.Contains(scores[0].Err.Error(), "dial fail") {
		t.Errorf("Err = %v", scores[0].Err)
	}
}

func TestEvaluatorNon200Status(t *testing.T) {
	fn := rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 502,
			Body:       io.NopCloser(strings.NewReader("bad gateway")),
		}, nil
	})
	e, store := newTestEvaluator(t, Config{}, fn)
	defer e.Close()

	e.Enqueue(Sample{Instruction: "x", Output: "y"})
	scores := waitForScores(t, store, 1, 2*time.Second)
	if scores[0].Err == nil || !strings.Contains(scores[0].Err.Error(), "502") {
		t.Errorf("Err = %v", scores[0].Err)
	}
}

// TestEvaluatorConcurrencyCap verifies that at most Concurrency
// in-flight judge calls run at any time, even when the queue is full.
// We use a deliberately slow stub so the workers actually overlap.
func TestEvaluatorConcurrencyCap(t *testing.T) {
	const (
		concurrency = 2
		totalJobs   = 6
	)
	var (
		inFlight int64
		maxSeen  int64
	)
	release := make(chan struct{})
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	defer releaseOnce()

	fn := rtFunc(func(_ *http.Request) (*http.Response, error) {
		now := atomic.AddInt64(&inFlight, 1)
		defer atomic.AddInt64(&inFlight, -1)
		// Track the high-water mark atomically.
		for {
			old := atomic.LoadInt64(&maxSeen)
			if now <= old || atomic.CompareAndSwapInt64(&maxSeen, old, now) {
				break
			}
		}
		<-release
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"3"}}]}`)),
		}, nil
	})

	e, store := newTestEvaluator(t, Config{
		Concurrency: concurrency,
		QueueDepth:  totalJobs,
	}, fn)
	defer e.Close()

	for i := 0; i < totalJobs; i++ {
		if !e.Enqueue(Sample{Instruction: "x", Output: "y"}) {
			t.Fatalf("Enqueue %d returned false", i)
		}
	}
	// Wait for the in-flight counter to reach the cap. That confirms
	// the workers have started; the rest of the queue is waiting
	// their turn.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&inFlight) >= int64(concurrency) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&inFlight); got > int64(concurrency) {
		t.Errorf("in-flight = %d, want <= %d (cap broken)", got, concurrency)
	}
	if got := atomic.LoadInt64(&inFlight); got < int64(concurrency) {
		t.Fatalf("only %d/%d workers started in time", got, concurrency)
	}

	// Release the blocked workers and let everything drain.
	releaseOnce()
	waitForScores(t, store, totalJobs, 3*time.Second)

	if got := atomic.LoadInt64(&maxSeen); got > int64(concurrency) {
		t.Errorf("max in-flight = %d, want <= %d (cap broken)", got, concurrency)
	}
	if got := atomic.LoadInt64(&maxSeen); got < 1 {
		t.Errorf("max in-flight = %d, expected at least 1", got)
	}
}

// TestEvaluatorQueueOverflowDrops confirms that an over-capacity
// Enqueue returns false and never blocks the caller — important for
// the chat hot path, which must stay sub-5ms.
func TestEvaluatorQueueOverflowDrops(t *testing.T) {
	// Bounded stub: the first call blocks (so the worker pool
	// cannot drain while the test runs) and subsequent calls
	// return immediately. That avoids re-entering the block when
	// the worker consumes the next queued item.
	release := make(chan struct{})
	var calls int32
	fn := rtFunc(func(_ *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			<-release
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"3"}}]}`)),
		}, nil
	})
	e, store := newTestEvaluator(t, Config{
		Concurrency: 1,
		QueueDepth:  1,
	}, fn)
	defer e.Close()
	defer close(release) // unblock the worker so Close can return

	// First enqueue occupies the single worker (which is now
	// blocked inside the stub). Wait for the worker to actually
	// pick the sample off the channel — otherwise the queue slot
	// is still occupied and the second Enqueue below would falsely
	// report "queue full".
	if !e.Enqueue(Sample{Instruction: "x", Output: "y"}) {
		t.Fatal("first Enqueue should succeed")
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&calls) >= 1 }, time.Second)
	// Second enqueue fills the queue.
	if !e.Enqueue(Sample{Instruction: "x", Output: "y"}) {
		t.Fatal("second Enqueue should fill the queue")
	}
	// Third enqueue must NOT block and must return false.
	done := make(chan bool, 1)
	go func() { done <- e.Enqueue(Sample{Instruction: "x", Output: "y"}) }()
	select {
	case ok := <-done:
		if ok {
			t.Error("third Enqueue should drop (queue full)")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Enqueue blocked when queue was full — hot path will stall")
	}
	// Sanity: nothing has been recorded yet because the worker is
	// still blocked inside the stub.
	if got := store.Scores(); len(got) != 0 {
		t.Errorf("worker should still be blocked, got %d scores", len(got))
	}
}

// waitFor polls cond until it returns true or d elapses. Bounded to
// keep tests fast and surface deadlocks quickly.
func waitFor(t *testing.T, cond func() bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestSampleRateDisabled makes sure an evaluator with SampleRate <= 0
// is dormant: Sample returns false, Enqueue is a no-op, and Close is
// instant.
func TestSampleRateDisabled(t *testing.T) {
	// Build the evaluator directly so newTestEvaluator's "zero rate
	// means 1.0" override does not interfere with this test's intent.
	store := NewMemoryStorage()
	e := NewEvaluator(Config{SampleRate: 0, URL: "http://x", Model: "m"}, &http.Client{Transport: rtFunc(func(_ *http.Request) (*http.Response, error) {
		t.Error("HTTP should not be called when sample rate is 0")
		return nil, nil
	})}, store)
	if e.Enabled() {
		t.Error("Enabled() = true, want false")
	}
	for i := 0; i < 100; i++ {
		if e.Sample() {
			t.Fatal("Sample() should always be false when disabled")
		}
	}
	if e.Enqueue(Sample{Instruction: "x"}) {
		t.Error("Enqueue should be a no-op when disabled")
	}
	// Close should return immediately even though no workers are running.
	closed := make(chan struct{})
	go func() { _ = e.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(1 * time.Second):
		t.Fatal("Close on disabled evaluator blocked")
	}
	if got := store.Scores(); len(got) != 0 {
		t.Errorf("disabled evaluator recorded %d scores, want 0", len(got))
	}
}

func TestSampleRateDistribution(t *testing.T) {
	// Sample ~10000 times at 30% — observed fraction must be within
	// ±3% of the configured rate. This catches bias / off-by-one
	// bugs in the random source.
	const (
		rate  = 0.3
		tries = 10000
	)
	e := NewEvaluator(Config{SampleRate: rate}, nil, nil)
	defer e.Close()
	hits := 0
	for i := 0; i < tries; i++ {
		if e.Sample() {
			hits++
		}
	}
	frac := float64(hits) / float64(tries)
	if frac < rate-0.03 || frac > rate+0.03 {
		t.Errorf("sample fraction %.3f, want ~%.3f", frac, rate)
	}
}

// TestEvaluateEntryPoint exercises the standalone Evaluate path so a
// future CLI tool (or test harness) can drive one judge call without
// spinning up the worker pool.
func TestEvaluateEntryPoint(t *testing.T) {
	fn := rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"5"}}]}`)),
		}, nil
	})
	e, _ := newTestEvaluator(t, Config{}, fn)
	defer e.Close()

	score := e.Evaluate(context.Background(), Sample{
		Instruction: "implement quicksort",
		Output:      "def qs(xs): return xs",
	})
	if score.Err != nil {
		t.Fatalf("Err = %v", score.Err)
	}
	if score.Score != 5 {
		t.Errorf("Score = %d, want 5", score.Score)
	}
}

func TestEvaluateNilEvaluator(t *testing.T) {
	var e *Evaluator
	score := e.Evaluate(context.Background(), Sample{})
	if score.Err == nil {
		t.Error("nil evaluator should return Err")
	}
}

// TestRecordEntryPoint confirms the public Record wrapper forwards to
// the underlying Storage and surfaces errors.
func TestRecordEntryPoint(t *testing.T) {
	e, store := newTestEvaluator(t, Config{}, rtFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, nil
	}))
	defer e.Close()

	if err := e.Record(JudgeScore{RequestID: "r1", Score: 4}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := store.Scores(); len(got) != 1 || got[0].RequestID != "r1" || got[0].Score != 4 {
		t.Errorf("store = %+v", got)
	}
}

// TestMemoryStorageConcurrency stresses the storage under concurrent
// Record calls — the worker pool can call Record from N goroutines
// simultaneously, so the storage's locking must hold.
func TestMemoryStorageConcurrency(t *testing.T) {
	store := NewMemoryStorage()
	const writers = 8
	const perWriter = 50
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				_ = store.Record(JudgeScore{RequestID: "x", Score: id + j})
			}
		}(i)
	}
	wg.Wait()
	if got := len(store.Scores()); got != writers*perWriter {
		t.Errorf("got %d scores, want %d", got, writers*perWriter)
	}
}

// TestEvaluatorCloseDrains makes sure Close waits for in-flight work
// before returning — important so a graceful shutdown does not lose
// pending JudgeScore records.
func TestEvaluatorCloseDrains(t *testing.T) {
	fn := rtFunc(func(_ *http.Request) (*http.Response, error) {
		time.Sleep(30 * time.Millisecond)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"2"}}]}`)),
		}, nil
	})
	e, store := newTestEvaluator(t, Config{}, fn)
	for i := 0; i < 5; i++ {
		e.Enqueue(Sample{Instruction: "x", Output: "y"})
	}
	_ = e.Close()
	if got := len(store.Scores()); got != 5 {
		t.Errorf("Close did not drain: got %d/5 scores", got)
	}
}

// TestBudgetGuardIntegration verifies that a configured BudgetGuard receives
// Record calls with source="judge" after each successful evaluation (issue #240).
func TestBudgetGuardIntegration(t *testing.T) {
	var (
		mu           sync.Mutex
		recordedCost float64
		recordedSrc  string
	)
	fn := rtFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"3"}}]}`)),
		}, nil
	})

	bg := newBudgetGuardForTest(t)
	bg.SetAlerter(budgetAlerterFunc(func(_ interface{}, cost float64, src string) {
		mu.Lock()
		defer mu.Unlock()
		recordedCost = cost
		recordedSrc = src
	}))

	e, store := newTestEvaluator(t, Config{
		BudgetGuard: bg,
	}, fn)
	defer e.Close()

	if !e.Enqueue(Sample{RequestID: "r-bg", Instruction: "x", Output: "y"}) {
		t.Fatal("Enqueue should accept")
	}
	waitForScores(t, store, 1, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if recordedCost <= 0 {
		t.Errorf("BudgetGuard cost = %v, want > 0", recordedCost)
	}
	if recordedSrc != "judge" {
		t.Errorf("BudgetGuard source = %q, want judge", recordedSrc)
	}
}

// budgetAlerterFunc is a thin adapter so tests can use a plain function
// as a budget.Alerter without defining an interface implementation.
type budgetAlerterFunc func(interface{}, float64, string)

func (f budgetAlerterFunc) OnExceed(budget.State) {}
func (f budgetAlerterFunc) OnSpend(state budget.State, cost float64, src string) {
	f(state, cost, src)
}
func (f budgetAlerterFunc) OnApproaching(budget.State) {}

func newBudgetGuardForTest(t *testing.T) *budget.Guard {
	t.Helper()
	return budget.NewGuard(1000.0)
}

// Compile-time guard: judge.HTTPClient must accept *http.Client.
var _ HTTPClient = (*http.Client)(nil)

// TestEvaluatorDroppedCounter verifies the dropped atomic counter increments
// when the queue is full and Enqueue returns false (issue #226).
func TestEvaluatorDroppedCounter(t *testing.T) {
	release := make(chan struct{})
	fn := rtFunc(func(_ *http.Request) (*http.Response, error) {
		<-release
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"3"}}]}`)),
		}, nil
	})
	e, _ := newTestEvaluator(t, Config{
		Concurrency: 1,
		QueueDepth:  1,
	}, fn)
	defer e.Close()
	defer close(release)

	if e.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0 initially", e.Dropped())
	}

	// First enqueue occupies the worker.
	if !e.Enqueue(Sample{Instruction: "x", Output: "y"}) {
		t.Fatal("first Enqueue should succeed")
	}
	// Wait for the worker to pick it up so the queue slot is freed.
	waitFor(t, func() bool { return len(e.queue) == 0 }, time.Second)
	// Second enqueue fills the queue (worker still blocked in stub).
	if !e.Enqueue(Sample{Instruction: "x", Output: "y"}) {
		t.Fatal("second Enqueue should fill the queue")
	}
	// Third enqueue must drop.
	done := make(chan bool, 1)
	go func() { done <- e.Enqueue(Sample{Instruction: "x", Output: "y"}) }()
	select {
	case ok := <-done:
		if ok {
			t.Error("third Enqueue should drop")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Enqueue blocked when queue was full")
	}
	if got := e.Dropped(); got != 1 {
		t.Errorf("Dropped = %d, want 1", got)
	}
}

// TestEvaluatorDroppedCounterConcurrent exercises the dropped counter from
// many goroutines; the race detector is the primary assertion.
func TestEvaluatorDroppedCounterConcurrent(t *testing.T) {
	// Slow stub so the queue stays full.
	blocked := make(chan struct{})
	fn := rtFunc(func(_ *http.Request) (*http.Response, error) {
		<-blocked
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"3"}}]}`)),
		}, nil
	})
	e, _ := newTestEvaluator(t, Config{
		Concurrency: 1,
		QueueDepth:  1,
	}, fn)
	defer close(blocked) // release blocked worker

	// Fill the queue first.
	if !e.Enqueue(Sample{Instruction: "x", Output: "y"}) {
		t.Fatal("first Enqueue should succeed")
	}
	waitFor(t, func() bool { return len(e.queue) == 0 }, time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Enqueue(Sample{Instruction: "x", Output: "y"})
		}()
	}
	wg.Wait()

	if got := e.Dropped(); got == 0 {
		t.Errorf("Dropped = 0 after 50 overflow enqueues, want > 0")
	}
}

// TestEvaluatorDroppedNilSafe confirms nil evaluator returns 0 from Dropped.
func TestEvaluatorDroppedNilSafe(t *testing.T) {
	var e *Evaluator
	if got := e.Dropped(); got != 0 {
		t.Errorf("nil Dropped = %d, want 0", got)
	}
}
