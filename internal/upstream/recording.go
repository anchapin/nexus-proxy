package upstream

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

// RecordingTransport is an http.RoundTripper that records every request and
// lets tests register canned responses. It is intentionally package-private
// helper code — production code uses *http.Client with the default
// transport.
type RecordingTransport struct {
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc // keyed by request URL string
	fallback http.HandlerFunc
	calls    []RecordedCall
}

type RecordedCall struct {
	URL string
	Req *http.Request
}

// NewRecordingTransport constructs an empty recorder.
func NewRecordingTransport() *RecordingTransport {
	return &RecordingTransport{handlers: map[string]http.HandlerFunc{}}
}

// On registers a handler for requests whose URL.String() matches key.
func (r *RecordingTransport) On(method, url string, h http.HandlerFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[method+" "+url] = h
}

// OnAny registers a fallback that fires for any unmatched request.
func (r *RecordingTransport) OnAny(h http.HandlerFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallback = h
}

// Calls returns a snapshot of recorded calls.
func (r *RecordingTransport) Calls() []RecordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecordedCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// RoundTrip implements http.RoundTripper.
func (r *RecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	key := req.Method + " " + req.URL.String()
	h := r.handlers[key]
	if h == nil {
		h = r.fallback
	}
	// Snapshot the body so the handler can still read it.
	if req.Body != nil {
		body, _ := readAndRestoreBody(req)
		_ = body
	}
	r.calls = append(r.calls, RecordedCall{URL: req.URL.String(), Req: req})
	r.mu.Unlock()
	if h == nil {
		return defaultResponse("no handler for " + key), nil
	}
	rec := newRecorder()
	h(rec, req)
	return rec.Result(), nil
}

func readAndRestoreBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	const max = 1 << 20
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := req.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil || len(buf) >= max {
			break
		}
	}
	req.Body = newReadCloser(buf)
	return buf, nil
}

func defaultResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       newReadCloser([]byte(body)),
	}
}

// Compile-time guard.
var _ http.RoundTripper = (*RecordingTransport)(nil)

// internal helpers shared by RecordingTransport and tests --------------------------------

type recorderRW struct {
	headers http.Header
	body    *bytes.Buffer
	status  int
}

func newRecorder() *recorderRW {
	return &recorderRW{headers: http.Header{}, body: &bytes.Buffer{}}
}

func (r *recorderRW) Header() http.Header         { return r.headers }
func (r *recorderRW) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorderRW) WriteHeader(s int)           { r.status = s }

func (r *recorderRW) Result() *http.Response {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return &http.Response{
		StatusCode: r.status,
		Header:     r.headers,
		Body:       io.NopCloser(r.body),
	}
}

type nopReadCloser struct{ b *bytes.Reader }

func (n nopReadCloser) Read(p []byte) (int, error) { return n.b.Read(p) }
func (n nopReadCloser) Close() error               { return nil }

func newReadCloser(b []byte) io.ReadCloser { return nopReadCloser{b: bytes.NewReader(b)} }
