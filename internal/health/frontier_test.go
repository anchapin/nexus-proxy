package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestFrontierHealth_NilSafe verifies the nil-receiver contract: a nil
// *FrontierHealth returns true for every name so callers can skip a nil
// check on every request.
func TestFrontierHealth_NilSafe(t *testing.T) {
	t.Parallel()
	var fh *FrontierHealth
	if !fh.IsHealthy("anything") {
		t.Fatal("nil FrontierHealth should report every provider as healthy")
	}
	if fh.UnhealthyProviders() != nil {
		t.Fatal("nil FrontierHealth should return nil for UnhealthyProviders")
	}
	if fh.States() != nil {
		t.Fatal("nil FrontierHealth should return nil for States")
	}
}

// TestFrontierHealth_InitialState verifies that a freshly constructed
// FrontierHealth starts every provider as healthy and returns the
// expected state snapshot.
func TestFrontierHealth_InitialState(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fh := NewFrontierHealth(
		[]FrontierProbeTarget{
			{Name: "alpha", BaseURL: srv.URL, APIKey: "k1"},
			{Name: "beta", BaseURL: srv.URL, APIKey: "k2"},
		},
		time.Hour, 3, 2*time.Second, srv.Client(),
	)
	if !fh.IsHealthy("alpha") {
		t.Fatal("alpha should be healthy at construction")
	}
	if !fh.IsHealthy("beta") {
		t.Fatal("beta should be healthy at construction")
	}
	if fh.IsHealthy("unknown") != true {
		t.Fatal("unknown provider should be healthy (defensive)")
	}

	states := fh.States()
	if len(states) != 2 {
		t.Fatalf("expected 2 states, got %d", len(states))
	}
}

// TestFrontierHealth_ProbeSuccess verifies that a successful probe
// keeps the provider healthy and invokes the probe callback.
func TestFrontierHealth_ProbeSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var probeCount atomic.Int32
	fh := NewFrontierHealth(
		[]FrontierProbeTarget{{Name: "ok", BaseURL: srv.URL}},
		time.Hour, 3, 2*time.Second, srv.Client(),
	)
	fh.SetProbeCallback(func(provider, result string) {
		if provider != "ok" {
			t.Errorf("unexpected provider %q", provider)
		}
		if result != "success" {
			t.Errorf("unexpected result %q", result)
		}
		probeCount.Add(1)
	})

	ctx := context.Background()
	fh.probeAll(ctx)

	if got := probeCount.Load(); got != 1 {
		t.Fatalf("expected 1 probe callback, got %d", got)
	}
	if !fh.IsHealthy("ok") {
		t.Fatal("provider should be healthy after successful probe")
	}
}

// TestFrontierHealth_BreakerTrips verifies that after BreakerThreshold
// consecutive failures the circuit opens (IsHealthy returns false) and
// the trip callback fires exactly once.
func TestFrontierHealth_BreakerTrips(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	var tripCount atomic.Int32
	fh := NewFrontierHealth(
		[]FrontierProbeTarget{{Name: "down", BaseURL: srv.URL}},
		time.Hour, 3, 2*time.Second, srv.Client(),
	)
	fh.SetTripCallback(func(provider string) {
		if provider != "down" {
			t.Errorf("unexpected provider %q", provider)
		}
		tripCount.Add(1)
	})

	ctx := context.Background()
	// Probe 1 and 2: below threshold, still healthy.
	fh.probeAll(ctx)
	if !fh.IsHealthy("down") {
		t.Fatal("should still be healthy after 1 failure")
	}
	fh.probeAll(ctx)
	if !fh.IsHealthy("down") {
		t.Fatal("should still be healthy after 2 failures")
	}
	// Probe 3: threshold reached, circuit opens.
	fh.probeAll(ctx)
	if fh.IsHealthy("down") {
		t.Fatal("should be unhealthy after 3 failures")
	}
	if got := tripCount.Load(); got != 1 {
		t.Fatalf("expected 1 trip callback, got %d", got)
	}
}

// TestFrontierHealth_BreakerRecloses verifies that after the circuit
// opens, a successful probe closes it again.
func TestFrontierHealth_BreakerRecloses(t *testing.T) {
	t.Parallel()
	statusCode := http.StatusBadGateway
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statusCode)
	}))
	defer srv.Close()

	fh := NewFrontierHealth(
		[]FrontierProbeTarget{{Name: "flaky", BaseURL: srv.URL}},
		time.Hour, 2, 2*time.Second, srv.Client(),
	)

	ctx := context.Background()
	// Trip the breaker.
	fh.probeAll(ctx)
	fh.probeAll(ctx)
	if fh.IsHealthy("flaky") {
		t.Fatal("should be unhealthy after 2 failures")
	}

	// Recover.
	statusCode = http.StatusOK
	fh.probeAll(ctx)
	if !fh.IsHealthy("flaky") {
		t.Fatal("should be healthy again after successful probe")
	}

	states := fh.States()
	if len(states) != 1 {
		t.Fatalf("expected 1 state, got %d", len(states))
	}
	if states[0].FailureCount != 0 {
		t.Fatalf("expected failure count 0 after recovery, got %d", states[0].FailureCount)
	}
}

// TestFrontierHealth_UnhealthyProviders verifies that UnhealthyProviders
// returns only the providers whose circuit is open.
func TestFrontierHealth_UnhealthyProviders(t *testing.T) {
	t.Parallel()
	healthySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthySrv.Close()

	downSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer downSrv.Close()

	fh := NewFrontierHealth(
		[]FrontierProbeTarget{
			{Name: "good", BaseURL: healthySrv.URL},
			{Name: "bad", BaseURL: downSrv.URL},
		},
		time.Hour, 1, 2*time.Second, healthySrv.Client(),
	)

	ctx := context.Background()
	fh.probeAll(ctx)

	unhealthy := fh.UnhealthyProviders()
	if len(unhealthy) != 1 {
		t.Fatalf("expected 1 unhealthy provider, got %d", len(unhealthy))
	}
	if unhealthy[0] != "bad" {
		t.Fatalf("expected 'bad', got %q", unhealthy[0])
	}
}

// TestFrontierHealth_ConnectionError verifies that a connection error
// (server down) counts as a probe failure.
func TestFrontierHealth_ConnectionError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close() // immediately close to force connection errors

	fh := NewFrontierHealth(
		[]FrontierProbeTarget{{Name: "dead", BaseURL: srv.URL}},
		time.Hour, 1, 2*time.Second, &http.Client{Timeout: 2 * time.Second},
	)

	ctx := context.Background()
	fh.probeAll(ctx)

	if fh.IsHealthy("dead") {
		t.Fatal("should be unhealthy after connection error")
	}
}

// TestFrontierHealth_FourOxIsSuccess verifies that a 4xx response is
// treated as success (the endpoint answered).
func TestFrontierHealth_FourOxIsSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	fh := NewFrontierHealth(
		[]FrontierProbeTarget{{Name: "auth", BaseURL: srv.URL}},
		time.Hour, 3, 2*time.Second, srv.Client(),
	)

	ctx := context.Background()
	fh.probeAll(ctx)

	if !fh.IsHealthy("auth") {
		t.Fatal("4xx should be treated as success (endpoint answered)")
	}
}

// TestFrontierHealth_EmptyTargets verifies that an empty target list
// is a valid no-op (IsHealthy returns true).
func TestFrontierHealth_EmptyTargets(t *testing.T) {
	t.Parallel()
	fh := NewFrontierHealth(nil, time.Hour, 3, 2*time.Second, nil)
	if !fh.IsHealthy("any") {
		t.Fatal("empty target list should report every provider as healthy")
	}
	if fh.UnhealthyProviders() != nil {
		t.Fatal("empty target list should have no unhealthy providers")
	}
}

// TestFrontierHealth_ProbeCallbackResult verifies that the probe
// callback receives "failure" for failed probes.
func TestFrontierHealth_ProbeCallbackResult(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var results []string
	var mu atomic.Int32
	fh := NewFrontierHealth(
		[]FrontierProbeTarget{{Name: "p", BaseURL: srv.URL}},
		time.Hour, 5, 2*time.Second, srv.Client(),
	)
	fh.SetProbeCallback(func(_, result string) {
		results = append(results, result)
		mu.Add(1)
	})

	ctx := context.Background()
	fh.probeAll(ctx)

	if len(results) != 1 || results[0] != "failure" {
		t.Fatalf("expected [failure], got %v", results)
	}
}

// TestFrontierHealth_Close verifies that Close stops the background
// poller without leaking goroutines.
func TestFrontierHealth_Close(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fh := NewFrontierHealth(
		[]FrontierProbeTarget{{Name: "x", BaseURL: srv.URL}},
		50*time.Millisecond, 3, 2*time.Second, srv.Client(),
	)

	ctx := context.Background()
	fh.Run(ctx)

	// Close should return promptly.
	done := make(chan struct{})
	go func() {
		_ = fh.Close()
		close(done)
	}()
	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s")
	}
}
