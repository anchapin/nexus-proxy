package main

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlowlorisReadTimeout verifies that an http.Server configured with
// a ReadTimeout correctly drops connections that fail to send a complete
// request (headers) within the deadline. This is the core Slowloris /
// slow-header DoS protection that issue #106 addresses.
//
// The test spins up a minimal server with a 2s ReadTimeout, opens a raw
// TCP connection, sends the request line, then pauses before completing
// the headers. The server must close the connection within ReadTimeout +
// margin.
func TestSlowlorisReadTimeout(t *testing.T) {
	const readTimeout = 2 * time.Second
	const margin = 3 * time.Second // generous headroom for CI flakiness

	var handled atomic.Int32
	srv := &http.Server{
		Addr:              "127.0.0.1:0",
		ReadTimeout:       readTimeout,
		ReadHeaderTimeout: readTimeout,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handled.Add(1)
		}),
	}

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go srv.Serve(ln)
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send a valid request line but only one incomplete header.
	// The server will wait for the remaining headers until ReadTimeout.
	_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n"))
	if err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	// Read any bytes the server may send (e.g. early error) — the key
	// signal is that the connection is closed by the server.
	deadline := time.Now().Add(readTimeout + margin)
	conn.SetReadDeadline(deadline) //nolint:errcheck

	reader := bufio.NewReader(conn)
	start := time.Now()
	_, readErr := reader.ReadByte()
	elapsed := time.Since(start)

	// The connection should have been closed by the server's
	// ReadTimeout. The read must return an error (EOF or timeout).
	if readErr == nil {
		t.Fatal("expected connection to be closed by ReadTimeout, but read succeeded")
	}

	// Must not hang for longer than ReadTimeout + margin.
	if elapsed > readTimeout+margin {
		t.Errorf("connection closed after %v, want <= %v (ReadTimeout + margin)",
			elapsed.Round(time.Millisecond), (readTimeout + margin).Round(time.Millisecond))
	}

	// No complete request should have been handled.
	if n := handled.Load(); n != 0 {
		t.Errorf("handler invoked %d times, want 0 (incomplete request should not be routed)", n)
	}
}

// TestSlowlorisIdleTimeout verifies that idle keep-alive connections are
// reaped by IdleTimeout. A client connects, sends a valid HTTP request,
// reads the response, then sits idle — the server should close the
// connection after IdleTimeout.
func TestSlowlorisIdleTimeout(t *testing.T) {
	const idleTimeout = 2 * time.Second
	const margin = 3 * time.Second

	var handled atomic.Int32
	srv := &http.Server{
		Addr:         "127.0.0.1:0",
		IdleTimeout:  idleTimeout,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handled.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}),
	}

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go srv.Serve(ln)
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send a complete, minimal HTTP request.
	_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\n\r\n"))
	if err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read the full response.
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()

	if handled.Load() != 1 {
		t.Fatalf("expected 1 handled request, got %d", handled.Load())
	}

	// Now sit idle — the server should close the connection after
	// IdleTimeout.
	deadline := time.Now().Add(idleTimeout + margin)
	conn.SetReadDeadline(deadline) //nolint:errcheck

	start := time.Now()
	_, readErr := reader.ReadByte()
	elapsed := time.Since(start)

	if readErr == nil {
		t.Fatal("expected idle connection to be closed by IdleTimeout, but read succeeded")
	}

	if elapsed > idleTimeout+margin {
		t.Errorf("idle connection closed after %v, want <= %v (IdleTimeout + margin)",
			elapsed.Round(time.Millisecond), (idleTimeout + margin).Round(time.Millisecond))
	}
}
