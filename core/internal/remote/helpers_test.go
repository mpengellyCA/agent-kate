package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBackend is a scriptable stand-in for the core. Every field is guarded so
// a test can mutate it while an SSE stream is live.
type fakeBackend struct {
	mu sync.Mutex

	agents     []Agent
	transcript Transcript
	sendResult SendResult
	lastSend   SendRequest
	lastSender Principal
	diff       Diff
	gitStatus  any
	gitLog     any
	permDetail PermissionDetail

	listErr       error
	sendErr       error
	permErr       error
	permDetailErr error
	stopErr       error

	calls []string
}

func (f *fakeBackend) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
}

func (f *fakeBackend) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeBackend) ListAgents(context.Context) ([]Agent, error) {
	f.record("ListAgents")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents, f.listErr
}

func (f *fakeBackend) Transcript(context.Context, TranscriptRequest) (Transcript, error) {
	f.record("Transcript")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transcript, nil
}

func (f *fakeBackend) Send(_ context.Context, principal Principal, req SendRequest) (SendResult, error) {
	f.record("Send")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSender = principal
	f.lastSend = req
	return f.sendResult, f.sendErr
}

func (f *fakeBackend) Fork(_ context.Context, _ Principal, req ForkRequest) (ForkResult, error) {
	if req.ThreadID == "" {
		return ForkResult{}, ErrUnknownThread
	}
	return ForkResult{ThreadID: "fork-" + req.ThreadID}, nil
}
func (f *fakeBackend) ReadFile(_ context.Context, req FileRequest) (FileContent, error) {
	return FileContent{Path: req.Path, Text: "", Revision: "test"}, nil
}
func (f *fakeBackend) WriteFile(_ context.Context, _ Principal, req FileWriteRequest) (FileContent, error) {
	return FileContent{Path: req.Path, Text: req.Text, Revision: "test"}, nil
}

func (f *fakeBackend) PermissionDetail(context.Context, string) (PermissionDetail, error) {
	f.record("PermissionDetail")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.permDetail, f.permDetailErr
}

func (f *fakeBackend) RespondPermission(context.Context, Principal, PermissionAnswer) error {
	f.record("RespondPermission")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.permErr
}

func (f *fakeBackend) Interrupt(context.Context, Principal, string) error {
	f.record("Interrupt")
	return nil
}

func (f *fakeBackend) Stop(context.Context, Principal, string) error {
	f.record("Stop")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopErr
}

func (f *fakeBackend) Diff(context.Context, DiffRequest) (Diff, error) {
	f.record("Diff")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diff, nil
}

func (f *fakeBackend) GitStatus(context.Context, string) (any, error) {
	f.record("GitStatus")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gitStatus, nil
}

func (f *fakeBackend) GitLog(context.Context, string, int) (any, error) {
	f.record("GitLog")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gitLog, nil
}

// testEnv is a server, an HTTP test listener and one paired device.
type testEnv struct {
	t      *testing.T
	srv    *Server
	be     *fakeBackend
	http   *httptest.Server
	device Device
	cookie string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	be := &fakeBackend{}
	return newTestEnvWith(t, be)
}

func newTestEnvWith(t *testing.T, be *fakeBackend) *testEnv {
	t.Helper()
	srv, err := New(Config{
		BindAddr:    "127.0.0.1:0",
		DataDir:     t.TempDir(),
		CoreVersion: "0.test",
		WebUIBuild:  "test-build",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, be)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	// The harness serves the mux through httptest rather than the server's own
	// listener, so srv never binds and Addr() would read empty. Record the
	// address httptest actually took, because MintDevice refuses to pair while
	// nothing is listening — a pairing URL naming a port this process does not
	// own is worse than no URL at all.
	srv.mu.Lock()
	srv.addr = strings.TrimPrefix(ts.URL, "http://")
	srv.mu.Unlock()

	env := &testEnv{t: t, srv: srv, be: be, http: ts}
	env.pair("test phone")
	return env
}

// pair mints a device and exchanges its token for a session cookie through the
// real endpoint, so every other test rides on a genuinely authenticated flow.
func (e *testEnv) pair(name string) {
	e.t.Helper()
	token, _, dev, err := e.srv.MintDevice(name)
	if err != nil {
		e.t.Fatalf("MintDevice: %v", err)
	}
	e.device = dev
	resp := e.do("POST", "/api/v1/auth/exchange", `{"token":"`+token+`"}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("exchange: status %d body %s", resp.StatusCode, b)
	}
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			e.cookie = c.Value
		}
	}
	if e.cookie == "" {
		e.t.Fatal("exchange returned no session cookie")
	}
}

// do issues a request. When cookie is non-nil it is sent; otherwise the request
// is unauthenticated.
func (e *testEnv) do(method, path, body string, cookie *string) *http.Response {
	e.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.http.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	if cookie != nil {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: *cookie})
	}
	resp, err := e.http.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// auth issues an authenticated request with the paired device's cookie.
func (e *testEnv) auth(method, path, body string) *http.Response {
	e.t.Helper()
	return e.do(method, path, body, &e.cookie)
}

// authJSON issues an authenticated request and decodes the JSON body.
func (e *testEnv) authJSON(method, path, body string) (int, map[string]any) {
	e.t.Helper()
	resp := e.auth(method, path, body)
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			e.t.Fatalf("%s %s: body is not JSON (%v): %s", method, path, err, raw)
		}
	}
	return resp.StatusCode, out
}

// --- SSE client -------------------------------------------------------------

type sseFrame struct {
	id      string
	event   string
	data    string
	comment string
}

// sseConn is a live SSE reader for tests.
type sseConn struct {
	t    *testing.T
	resp *http.Response
	br   *bufio.Reader
}

// openSSE connects to the event stream. lastEventID < 0 sends no cursor.
func (e *testEnv) openSSE(query string, lastEventID int64) *sseConn {
	e.t.Helper()
	req, err := http.NewRequest("GET", e.http.URL+"/api/v1/events"+query, nil)
	if err != nil {
		e.t.Fatalf("new sse request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: cookieName, Value: e.cookie})
	if lastEventID >= 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(lastEventID, 10))
	}
	resp, err := e.http.Client().Do(req)
	if err != nil {
		e.t.Fatalf("sse connect: %v", err)
	}
	c := &sseConn{t: e.t, resp: resp, br: bufio.NewReader(resp.Body)}
	e.t.Cleanup(c.close)
	return c
}

func (c *sseConn) close() { _ = c.resp.Body.Close() }

// next reads one frame, failing the test if none arrives in time.
func (c *sseConn) next() sseFrame {
	c.t.Helper()
	type result struct {
		f   sseFrame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := c.readFrame()
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			c.t.Fatalf("sse read: %v", r.err)
		}
		return r.f
	case <-time.After(5 * time.Second):
		c.t.Fatal("timed out waiting for an SSE frame")
		return sseFrame{}
	}
}

// expectClosed asserts the stream ends (the server hung up).
func (c *sseConn) expectClosed() {
	c.t.Helper()
	ch := make(chan error, 1)
	go func() {
		for {
			if _, err := c.readFrame(); err != nil {
				ch <- err
				return
			}
		}
	}()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		c.t.Fatal("stream did not close")
	}
}

func (c *sseConn) readFrame() (sseFrame, error) {
	var f sseFrame
	var data []string
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return f, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if f.id == "" && f.event == "" && len(data) == 0 && f.comment == "" {
				continue // stray blank line
			}
			f.data = strings.Join(data, "\n")
			return f, nil
		case strings.HasPrefix(line, ":"):
			f.comment = strings.TrimSpace(line[1:])
		case strings.HasPrefix(line, "id: "):
			f.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			f.event = line[7:]
		case strings.HasPrefix(line, "data: "):
			data = append(data, line[6:])
		case strings.HasPrefix(line, "retry: "):
			f.comment = "retry"
		}
	}
}

func decodeData(t *testing.T, f sseFrame) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(f.data), &out); err != nil {
		t.Fatalf("frame %q data is not JSON: %v (%s)", f.event, err, f.data)
	}
	return out
}

// recordingWriter is a minimal http.ResponseWriter that satisfies everything
// http.ResponseController asks for, so the SSE frame encoder can be tested
// without a socket.
type recordingWriter struct {
	buf    strings.Builder
	header http.Header
}

func (r *recordingWriter) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}

func (r *recordingWriter) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *recordingWriter) WriteHeader(int)             {}
func (r *recordingWriter) Flush()                      {}
func (r *recordingWriter) SetWriteDeadline(time.Time) error {
	return nil
}
func (r *recordingWriter) String() string { return r.buf.String() }

// newCursorRequest builds a bare request carrying a Last-Event-ID header and/or
// a lastEventId query parameter, for eventCursor's table test.
func newCursorRequest(header, query string) *http.Request {
	path := "/api/v1/events"
	if query != "" {
		path += "?lastEventId=" + query
	}
	r := httptest.NewRequest("GET", path, nil)
	if header != "" {
		r.Header.Set("Last-Event-ID", header)
	}
	return r
}

// skipRetry drops the leading `retry:` frame every stream opens with.
func (c *sseConn) skipRetry() {
	c.t.Helper()
	f := c.next()
	if f.comment != "retry" {
		c.t.Fatalf("expected the retry preamble, got %+v", f)
	}
}
