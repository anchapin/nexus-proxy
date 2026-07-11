package quality

import (
	"strings"
	"testing"
)

func TestDetectEditsFindsToolCalls(t *testing.T) {
	body := `model response preamble... ` +
		`"tool_calls":[{"id":"call_1","type":"function","function":` +
		`{"name":"write_file","arguments":"{\"path\":\"/tmp/foo.rs\",\"content\":\"x\"}"}}]` +
		` some trailing noise...`
	events := DetectEdits(body)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Path != "/tmp/foo.rs" {
		t.Errorf("Path = %q, want /tmp/foo.rs", events[0].Path)
	}
}

// TestDetectEditsAllCommonToolNames walks the supported name list to
// confirm each variant triggers a positive match.
func TestDetectEditsAllCommonToolNames(t *testing.T) {
	for _, name := range editToolNames {
		t.Run(name, func(t *testing.T) {
			body := `{"name":"` + name + `","arguments":"{\"path\":\"/tmp/x\"}"}`
			ev := DetectEdits(body)
			if len(ev) != 1 {
				t.Fatalf("name=%s: got %d events, want 1", name, len(ev))
			}
			if ev[0].Path != "/tmp/x" {
				t.Errorf("name=%s: Path=%q, want /tmp/x", name, ev[0].Path)
			}
		})
	}
}

// TestDetectEditsFilePathAlias verifies the snake/camel-case fallback
// fields ("filePath", "file_path", "filepath") are honoured when the
// canonical "path" key is absent.
func TestDetectEditsFilePathAlias(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"name":"write_file","arguments":"{\"filePath\":\"/tmp/a.go\",\"content\":\"x\"}"}`, "/tmp/a.go"},
		{`{"name":"edit_file","arguments":"{\"file_path\":\"/tmp/b.py\",\"diff\":\"+\"}"}`, "/tmp/b.py"},
		{`{"name":"apply_patch","arguments":"{\"filepath\":\"/tmp/c.c\",\"patch\":\"+\"}"}`, "/tmp/c.c"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := DetectEdits(tc.body); len(got) != 1 || got[0].Path != tc.want {
				t.Fatalf("got %+v, want exactly 1 event with Path=%q", got, tc.want)
			}
		})
	}
}

// TestDetectEditsDeduplicatesPerPath covers the de-dup contract: the
// same path appearing twice in a long body yields a single event.
func TestDetectEditsDeduplicatesPerPath(t *testing.T) {
	body := strings.Repeat(
		`{"name":"write_file","arguments":"{\"path\":\"/tmp/x.rs\"}"} `, 5)
	ev := DetectEdits(body)
	if len(ev) != 1 {
		t.Fatalf("got %d events, want 1 (de-dup)", len(ev))
	}
}

// TestDetectEditsEmptyBody verifies the cheap early-exit branch.
func TestDetectEditsEmptyBody(t *testing.T) {
	if got := DetectEdits(""); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

// TestDetectEditsReturnsNothingForUnknownTool confirms we do not flag
// benign tool calls (read_file, bash, etc.).
func TestDetectEditsReturnsNothingForUnknownTool(t *testing.T) {
	body := `{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}`
	if got := DetectEdits(body); len(got) != 0 {
		t.Errorf("bash should not trigger; got %d events", len(got))
	}
}

// TestDetectEditsHandlesSSEFraming ensures detection works against
// the data: {...}\n\n chunks OpenCode-style streams emit. We don't
// actually parse SSE — we just feed the body of one chunk and expect
// the same result.
func TestDetectEditsHandlesSSEFraming(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[` +
		`{"function":{"name":"edit_file","arguments":"{\"path\":\"/tmp/sse.go\",\"diff\":\"+\"}"}}` +
		`]}}]}` + "\n\n"
	ev := DetectEdits(body)
	if len(ev) != 1 || ev[0].Path != "/tmp/sse.go" {
		t.Fatalf("got %+v, want one event at /tmp/sse.go", ev)
	}
}

// TestDetectEditsHandlesCascadeChunkSSE matches the realistic shape
// the cascade emits after JSON-encoding the assistant `content`
// field — the path field appears inside the inner
// `function.arguments` JSON blob (string-encoded), giving us the
// `{\"path\":\"...\"}` escape pattern detection must unescape.
func TestDetectEditsHandlesCascadeChunkSSE(t *testing.T) {
	// As emitted by writeSSEResponse after the cascade's
	// json.Marshal. Single backslash + quote per escape.
	body := `data: {"choices":[{"delta":{"content":"applied edit {\"name\":\"edit_file\",\"arguments\":\"{\"path\":\"/tmp/foo.rs\",\"diff\":\"+x\"}\"}"},"finish_reason":"stop","index":0}]}` + "\n\n"
	ev := DetectEdits(body)
	if len(ev) != 1 || ev[0].Path != "/tmp/foo.rs" {
		t.Fatalf("got %+v, want one event at /tmp/foo.rs", ev)
	}
}
