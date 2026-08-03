package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDispatchNoArgs verifies that dispatch returns (0, false) when no
// subcommand is present, signalling the caller to start the proxy server.
func TestDispatchNoArgs(t *testing.T) {
	code, handled := dispatch([]string{"nexus"}, &bytes.Buffer{}, &bytes.Buffer{})
	if handled {
		t.Error("expected handled=false for no subcommand")
	}
	if code != 0 {
		t.Errorf("expected code 0, got %d", code)
	}
}

// TestDispatchVersion verifies all version flag variants dispatch to
// printVersion and return exit code 0.
func TestDispatchVersion(t *testing.T) {
	for _, flag := range []string{"-v", "--version", "version"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, handled := dispatch([]string{"nexus", flag}, &stdout, &stderr)
			if !handled {
				t.Fatal("expected handled=true")
			}
			if code != 0 {
				t.Errorf("expected exit 0, got %d", code)
			}
			out := stdout.String()
			if !strings.Contains(out, "nexus") {
				t.Errorf("stdout %q does not contain 'nexus'", out)
			}
		})
	}
}

// TestDispatchHelp verifies that -h/--help/help print usage and exit 0.
func TestDispatchHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help", "help"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, handled := dispatch([]string{"nexus", flag}, &stdout, &stderr)
			if !handled {
				t.Fatal("expected handled=true")
			}
			if code != 0 {
				t.Errorf("expected exit 0, got %d", code)
			}
			errStr := stderr.String()
			if !strings.Contains(errStr, "Usage:") {
				t.Errorf("stderr %q does not contain 'Usage:'", errStr)
			}
		})
	}
}

// TestDispatchUnknownSubcommand verifies unknown verbs exit 2.
func TestDispatchUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, handled := dispatch([]string{"nexus", "foobar"}, &stdout, &stderr)
	if !handled {
		t.Fatal("expected handled=true")
	}
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr should mention unknown subcommand, got %q", stderr.String())
	}
}

// TestDispatchCheckAlias verifies both "check" and "doctor" dispatch to
// runCheck (the diagnostic suite).
func TestDispatchCheckAlias(t *testing.T) {
	for _, verb := range []string{"check", "doctor"} {
		t.Run(verb, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			// runCheck reads config from env, so it may return non-zero
			// in a test environment. We only verify dispatch routes here.
			code, handled := dispatch([]string{"nexus", verb}, &stdout, &stderr)
			if !handled {
				t.Fatal("expected handled=true")
			}
			// runCheck returns 0 or 1 depending on environment
			if code != 0 && code != 1 {
				t.Errorf("expected exit 0 or 1, got %d", code)
			}
		})
	}
}

// TestDispatchConfig routes to runConfig.
func TestDispatchConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// "config" with no sub-args prints usage and exits 0
	code, handled := dispatch([]string{"nexus", "config"}, &stdout, &stderr)
	if !handled {
		t.Fatal("expected handled=true")
	}
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
}

// TestDispatchDashboard routes to runDashboard.
func TestDispatchDashboard(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, handled := dispatch([]string{"nexus", "dashboard"}, &stdout, &stderr)
	if !handled {
		t.Fatal("expected handled=true")
	}
	// dashboard may return 0 or 1 depending on whether metrics DB exists
	_ = code
}

// TestDispatchRoutingPreview verifies routing-preview dispatches and
// exits 1 when no prompts are given.
func TestDispatchRoutingPreview(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, handled := dispatch([]string{"nexus", "routing-preview"}, &stdout, &stderr)
	if !handled {
		t.Fatal("expected handled=true")
	}
	if code != 1 {
		t.Errorf("expected exit 1 (no prompts), got %d", code)
	}
}
