package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ai-harness/harness"
	"ai-harness/harness/config"
)

// ---------------------------------------------------------------------------
// Router tests (web/server.go)
//
// Handlers are built with newMux (the same tree Serve binds) and fed to
// httptest.NewServer - no real port, and the real listener is needed for
// the SSE tests so a client disconnect cancels the streaming handler.
// Settings/sessions/uploads all point at t.TempDir(); the real agent/
// tree is never written. Run via the compiled-binary workaround (see
// settings_test.go's header note).
// ---------------------------------------------------------------------------

// routerFixture is a live httptest.Server over newMux plus the session
// cookie from a successful login and the temp dirs the router writes to.
type routerFixture struct {
	t           *testing.T
	srv         *httptest.Server
	client      *http.Client
	cookie      *http.Cookie
	uploadsDir  string
	sessionsDir string
}

// noRedirectClient never follows 3xx so tests see the raw redirect (and
// its Set-Cookie) instead of the followed target.
var noRedirectClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// newRouterFixture wires the full handler tree against the stub model
// server at baseURL (pass "" for no reachable model server), logs in
// once, and hands back an authenticated fixture.
func newRouterFixture(t *testing.T, baseURL string, registry *harness.Registry) *routerFixture {
	t.Helper()
	tmp := t.TempDir()

	base := harness.Config{
		Provider:          "openai",
		BaseURL:           baseURL,
		Model:             "stub-model",
		MaxToolIterations: 10,
		MaxContextTokens:  1000,
	}
	if registry == nil {
		registry = harness.NewRegistry()
	}

	cfg := Config{
		BindAddr:     "127.0.0.1",
		Port:         0,
		Username:     "tester",
		Password:     "pw12345",
		Base:         base,
		Conf:         &config.Config{},
		Client:       harness.NewClient(baseURL, ""),
		Registry:     registry,
		SettingsPath: filepath.Join(tmp, "web-settings.json"),
		SessionsDir:  filepath.Join(tmp, "sessions"),
		UploadsDir:   filepath.Join(tmp, "uploads"),
	}

	handler, err := newMux(cfg)
	if err != nil {
		t.Fatalf("newMux: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Log in with the classic form post and capture the session cookie
	// (no-redirect client: the 303 itself carries the Set-Cookie).
	form := url.Values{"username": {cfg.Username}, "password": {cfg.Password}}
	resp, err := noRedirectClient.Post(srv.URL+RouteLogin, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("login POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login did not set the session cookie")
	}
	return &routerFixture{t: t, srv: srv, client: noRedirectClient, cookie: cookie, uploadsDir: cfg.UploadsDir, sessionsDir: cfg.SessionsDir}
}

// authedRequest builds a request carrying the session cookie.
func (f *routerFixture) authedRequest(method, path string, body io.Reader, contentType string) *http.Request {
	req, err := http.NewRequest(method, f.srv.URL+path, body)
	if err != nil {
		panic(err)
	}
	req.AddCookie(f.cookie)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

// do performs an authenticated request and returns the response (the
// caller must close the body).
func (f *routerFixture) do(method, path string, body io.Reader, contentType string) *http.Response {
	resp, err := f.client.Do(f.authedRequest(method, path, body, contentType))
	if err != nil {
		panic(err)
	}
	return resp
}

// postJSON sends v (nil = empty body) as application/json and returns
// the response.
func (f *routerFixture) postJSON(path string, v any) *http.Response {
	var body io.Reader
	if v != nil {
		body = bytes.NewReader(mustJSON(v))
	} else {
		body = strings.NewReader("")
	}
	return f.do(http.MethodPost, path, body, "application/json")
}

// getJSON performs GET path and decodes the JSON body into v, failing
// the test on any transport, status or decode problem.
func (f *routerFixture) getJSON(path string, v any) {
	f.t.Helper()
	resp := f.do(http.MethodGet, path, nil, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		f.t.Fatalf("GET %s decode: %v", path, err)
	}
}

// expectStatus runs method path (JSON body v or nil) and fails unless
// the status matches; returns the decoded body map for assertions.
func (f *routerFixture) expectStatus(method, path string, v any, want int) map[string]any {
	f.t.Helper()
	var body io.Reader
	if v != nil {
		body = bytes.NewReader(mustJSON(v))
	}
	resp := f.do(method, path, body, "application/json")
	defer resp.Body.Close()
	if resp.StatusCode != want {
		f.t.Fatalf("%s %s status = %d, want %d", method, path, resp.StatusCode, want)
	}
	var out map[string]any
	data, _ := io.ReadAll(resp.Body)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			f.t.Fatalf("%s %s decode body %q: %v", method, path, data, err)
		}
	}
	return out
}

func TestUnauthenticatedStatusIs401(t *testing.T) {
	f := newRouterFixture(t, "http://127.0.0.1:1/v1", nil)

	resp, err := noRedirectClient.Get(f.srv.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/status status = %d, want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("401 Content-Type = %q, want JSON", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode 401 body: %v", err)
	}
	if body["error"] == "" {
		t.Errorf("401 body missing error field: %v", body)
	}
}

func TestStaticAssetsServed(t *testing.T) {
	f := newRouterFixture(t, "http://127.0.0.1:1/v1", nil)

	// /static/* is public (login page needs it): no cookie.
	resp, err := noRedirectClient.Get(f.srv.URL + "/static/app.js")
	if err != nil {
		t.Fatalf("GET /static/app.js: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/app.js status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type = %q, want a javascript type", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("app.js body is empty")
	}
	if !bytes.Contains(body, []byte("attachSessionStream")) {
		t.Errorf("app.js body does not look like the real SPA: %q", body)
	}

	// "/" serves the embedded index.html, authenticated.
	resp2 := f.do(http.MethodGet, "/", nil, "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp2.StatusCode)
	}
	if ct := resp2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("/ Content-Type = %q, want text/html", ct)
	}
	body2, _ := io.ReadAll(resp2.Body)
	if !bytes.Contains(body2, []byte("<title>ai-harness</title>")) {
		t.Errorf("/ body is not the embedded index: %q", body2)
	}
	if !bytes.Contains(body2, []byte("shell: harness-spa rev1")) {
		t.Errorf("/ body is missing the SPA shell marker: %q", body2)
	}

	// Unknown paths under the "/" catch-all are explicit 404s.
	resp3 := f.do(http.MethodGet, "/definitely-not-a-page", nil, "")
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("GET /definitely-not-a-page status = %d, want 404", resp3.StatusCode)
	}
}

func TestStatusRouteShape(t *testing.T) {
	registry := harness.NewRegistry()
	registry.Register(&fakeTool{name: "fake_tool", output: "x"})

	f := newRouterFixture(t, "http://127.0.0.1:1/v1", registry)

	var status map[string]any
	f.getJSON("/api/status", &status)

	for _, key := range []string{
		"model", "provider", "base_url", "context_tokens", "prompt_tokens",
		"context_percent", "sessions", "active_session", "tools_count",
		"mcp_servers_count", "memory_file", "log_dir", "logging_enabled",
	} {
		if _, ok := status[key]; !ok {
			t.Errorf("status body missing key %q: %v", key, status)
		}
	}
	if status["model"] != "stub-model" {
		t.Errorf("model = %v, want stub-model", status["model"])
	}
	if status["provider"] != "openai" {
		t.Errorf("provider = %v, want openai", status["provider"])
	}
	if status["context_tokens"].(float64) != 1000 {
		t.Errorf("context_tokens = %v, want 1000", status["context_tokens"])
	}
	if status["tools_count"].(float64) != 1 {
		t.Errorf("tools_count = %v, want 1", status["tools_count"])
	}
	if status["mcp_servers_count"].(float64) != 0 {
		t.Errorf("mcp_servers_count = %v, want 0", status["mcp_servers_count"])
	}
	if status["logging_enabled"].(bool) != false {
		t.Errorf("logging_enabled = %v, want false", status["logging_enabled"])
	}
	sessions, ok := status["sessions"].([]any)
	if !ok || len(sessions) != 1 {
		t.Fatalf("sessions = %v, want one entry", status["sessions"])
	}
	if status["active_session"] != "s1" {
		t.Errorf("active_session = %v, want s1", status["active_session"])
	}
}

func TestSessionCreateListSwitchDeleteHTTP(t *testing.T) {
	f := newRouterFixture(t, "http://127.0.0.1:1/v1", nil)

	// Create makes the new session active.
	created := f.expectStatus(http.MethodPost, "/api/sessions", map[string]string{"name": "beta"}, http.StatusCreated)
	sess, ok := created["session"].(map[string]any)
	if !ok {
		t.Fatalf("create response missing session: %v", created)
	}
	newID, _ := sess["id"].(string)
	if newID == "" || sess["name"] != "beta" {
		t.Fatalf("created session = %v", sess)
	}

	var list struct {
		Sessions []SessionInfo `json:"sessions"`
		Active   string        `json:"active"`
	}
	f.getJSON("/api/sessions", &list)
	if len(list.Sessions) != 2 || list.Active != newID {
		t.Fatalf("after create: sessions=%d active=%q", len(list.Sessions), list.Active)
	}

	// Switch back to the initial session.
	f.expectStatus(http.MethodPost, "/api/sessions/s1/switch", nil, http.StatusOK)
	f.getJSON("/api/sessions", &list)
	if list.Active != "s1" {
		t.Errorf("after switch: active = %q, want s1", list.Active)
	}

	// Rename and unknown-id handling.
	f.expectStatus(http.MethodPost, "/api/sessions/s1/rename", map[string]string{"name": "alpha"}, http.StatusOK)
	f.expectStatus(http.MethodPost, "/api/sessions/nope/switch", nil, http.StatusNotFound)

	// Single-session history fetch.
	var detail struct {
		Session SessionInfo   `json:"session"`
		History []MessageView `json:"history"`
	}
	f.getJSON("/api/sessions/s1", &detail)
	if detail.Session.ID != "s1" || detail.Session.Name != "alpha" {
		t.Errorf("detail session = %+v", detail.Session)
	}

	// Delete the extra, then the last one is refused with 400.
	f.expectStatus(http.MethodDelete, "/api/sessions/"+newID, nil, http.StatusOK)
	body := f.expectStatus(http.MethodDelete, "/api/sessions/s1", nil, http.StatusBadRequest)
	if body["error"] == nil {
		t.Errorf("delete-last response missing error: %v", body)
	}
}

// TestChatSSEStreamContract drives the documented SSE contract: POST
// chat returns 202 immediately and the turn output (content, usage,
// done) arrives as data: <Event JSON> frames on the events stream,
// which opens with the queued hello Event.
func TestChatSSEStreamContract(t *testing.T) {
	fake := newFakeChatServer(t, chatUsageReply("hello world", 12, 7, 19))
	f := newRouterFixture(t, fake.URL(), nil)

	// Subscribe FIRST so no turn event can be missed, then start the turn.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req := f.authedRequest(http.MethodGet, "/api/sessions/s1/events", nil, "")
	req = req.WithContext(ctx)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("events Content-Type = %q, want text/event-stream", ct)
	}

	ack := f.expectStatus(http.MethodPost, "/api/sessions/s1/chat", map[string]any{"text": "hi"}, http.StatusAccepted)
	if ack["ok"] != true {
		t.Errorf("chat 202 body = %v, want ok:true", ack)
	}

	events := readSSEEvents(t, resp.Body, EventDone)
	if len(events) == 0 {
		t.Fatal("no SSE events received")
	}
	if events[0].Type != EventHello || events[0].Session != "s1" {
		t.Errorf("first event = %+v, want hello for s1", events[0])
	}

	var contentText, doneText string
	var sawUsage, sawStatus bool
	for _, ev := range events {
		switch ev.Type {
		case EventStatus:
			sawStatus = true
		case EventContent:
			contentText = ev.Text
		case EventUsage:
			sawUsage = true
			if ev.Usage == nil || ev.Usage.PromptTokens != 12 || ev.Usage.CompletionTokens != 7 || ev.Usage.TotalTokens != 19 {
				t.Errorf("usage event = %+v", ev.Usage)
			}
		case EventDone:
			doneText = ev.Text
		}
	}
	if !sawStatus {
		t.Error("no status event seen")
	}
	if contentText != "hello world" {
		t.Errorf("content text = %q, want %q", contentText, "hello world")
	}
	if !sawUsage {
		t.Error("no usage event seen")
	}
	if doneText != "hello world" {
		t.Errorf("done text = %q, want %q", doneText, "hello world")
	}
}

// readSSEEvents parses "data: <json>" frames until wantType is seen
// (or the stream/timeout fails).
func readSSEEvents(t *testing.T, body io.Reader, wantType string) []Event {
	t.Helper()
	var out []Event
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // comment pings, blank separators
		}
		var ev Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Errorf("unparseable SSE frame %q: %v", line, err)
			continue
		}
		out = append(out, ev)
		if ev.Type == wantType {
			return out
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading SSE stream: %v", err)
	}
	t.Fatalf("stream ended before %q event; got %d events", wantType, len(out))
	return out
}

func TestApproveUnknownCallIDIs404(t *testing.T) {
	f := newRouterFixture(t, "http://127.0.0.1:1/v1", nil)

	body := f.expectStatus(http.MethodPost, "/api/sessions/s1/approve",
		map[string]any{"call_id": "no-such-call", "approved": true}, http.StatusNotFound)
	if body["error"] == nil {
		t.Errorf("404 approve body missing error: %v", body)
	}
}

func TestToolPolicyPUTReflectedInGET(t *testing.T) {
	registry := harness.NewRegistry()
	registry.Register(&fakeTool{name: "fake_tool", output: "x"})
	f := newRouterFixture(t, "http://127.0.0.1:1/v1", registry)

	var before struct {
		Tools []toolPolicyView `json:"tools"`
	}
	f.getJSON("/api/tools", &before)
	if len(before.Tools) != 1 || before.Tools[0].Name != "fake_tool" || before.Tools[0].Policy != "allow" {
		t.Fatalf("initial tools = %+v", before.Tools)
	}

	f.expectStatus(http.MethodPut, "/api/tools/policy", map[string]string{"name": "fake_tool", "policy": "ask"}, http.StatusOK)

	var after struct {
		Tools []toolPolicyView `json:"tools"`
	}
	f.getJSON("/api/tools", &after)
	if len(after.Tools) != 1 || after.Tools[0].Policy != "ask" {
		t.Errorf("after PUT tools = %+v, want fake_tool ask", after.Tools)
	}

	// A bogus policy string is a 400.
	f.expectStatus(http.MethodPut, "/api/tools/policy", map[string]string{"name": "fake_tool", "policy": "sometimes"}, http.StatusBadRequest)
}

func TestUploadGuards(t *testing.T) {
	f := newRouterFixture(t, "http://127.0.0.1:1/v1", nil)

	// Happy path: a png lands under the temp uploads dir.
	contentType, body := uploadFileBody(t, "cat.png", []byte("fake-png-bytes"))
	resp := f.do(http.MethodPost, "/api/upload", body, contentType)
	var up struct {
		Path string `json:"path"`
	}
	decodeInto(t, resp, http.StatusOK, &up)
	if up.Path == "" {
		t.Fatal("upload response missing path")
	}
	if filepath.Dir(up.Path) != f.uploadsDir {
		t.Errorf("upload path %q outside uploads dir %q", up.Path, f.uploadsDir)
	}
	if _, err := os.Stat(up.Path); err != nil {
		t.Errorf("uploaded file not on disk: %v", err)
	}

	// Extension whitelist: .txt rejected.
	contentType, body = uploadFileBody(t, "notes.txt", []byte("nope"))
	resp = f.do(http.MethodPost, "/api/upload", body, contentType)
	drainExpectStatus(t, resp, http.StatusBadRequest)

	// Traversal defense is layered: Go's multipart reader already
	// strips path components from the filename parameter
	// (Part.FileName() = filepath.Base), and the handler additionally
	// rejects any name containing a separator, "..", or that is "." /
	// "..". On this (Windows) build the stdlib sanitizes `..\evil.png`
	// to `evil.png` before the handler runs, so the request succeeds -
	// what matters is that the stored file never escapes the uploads
	// dir. Bare "." / ".." survive Base() and hit the handler's guard.
	for _, bad := range []string{"..", "."} {
		contentType, body = uploadFileBody(t, bad, []byte("x"))
		resp = f.do(http.MethodPost, "/api/upload", body, contentType)
		drainExpectStatus(t, resp, http.StatusBadRequest)
	}

	contentType, body = uploadFileBody(t, `..\evil.png`, []byte("x"))
	resp = f.do(http.MethodPost, "/api/upload", body, contentType)
	if resp.StatusCode == http.StatusOK {
		var up struct {
			Path string `json:"path"`
		}
		decodeInto(t, resp, http.StatusOK, &up)
		if filepath.Dir(up.Path) != f.uploadsDir {
			t.Errorf("sanitized upload escaped uploads dir: %q", up.Path)
		}
	} else {
		drainExpectStatus(t, resp, http.StatusBadRequest)
	}

	// Size cap: ~21 MB exceeds the ~20 MB limit.
	big := bytes.Repeat([]byte("z"), 21<<20)
	contentType, body = uploadFileBody(t, "big.png", big)
	resp = f.do(http.MethodPost, "/api/upload", body, contentType)
	drainExpectStatus(t, resp, http.StatusRequestEntityTooLarge)
}

// uploadFileBody builds a multipart body with a single "file" field.
func uploadFileBody(t *testing.T, filename string, content []byte) (contentType string, body io.Reader) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return w.FormDataContentType(), &buf
}

// decodeInto closes resp, asserts the status and decodes the JSON body.
func decodeInto(t *testing.T, resp *http.Response, want int, v any) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

// drainExpectStatus asserts the status of a response whose body is not
// needed.
func drainExpectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
	io.Copy(io.Discard, resp.Body)
}

// TestSettingsPUTReconfiguresLiveSessions pins the router-level wiring
// the settings subtask left for us: after a successful PUT, the manager
// rebuilds every session's client from the new effective config, so a
// new turn talks to the NEW endpoint.
func TestSettingsPUTReconfiguresLiveSessions(t *testing.T) {
	original := newFakeChatServer(t, chatTextReply("first"))
	f := newRouterFixture(t, original.URL(), nil)

	// Point the settings at a second stub server and PUT.
	replacement := newFakeChatServer(t, chatTextReply("second"))
	resp := f.do(http.MethodPut, RouteSettings,
		bytes.NewReader(mustJSON(map[string]string{"base_url": replacement.URL()})), "application/json")
	drainExpectStatus(t, resp, http.StatusOK)

	// The next turn must hit the replacement server.
	f.expectStatus(http.MethodPost, "/api/sessions/s1/chat", map[string]any{"text": "go"}, http.StatusAccepted)
	deadline := time.Now().Add(10 * time.Second)
	for replacement.requestCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if replacement.requestCount() == 0 {
		t.Error("after settings PUT, turn still hit the original server")
	}
	if original.requestCount() != 0 {
		t.Errorf("original stub received %d request(s), want 0", original.requestCount())
	}
}

// TestMCPStatusEmptyNoServers keeps the MCP panel shape stable with no
// servers configured.
func TestMCPStatusEmpty(t *testing.T) {
	f := newRouterFixture(t, "http://127.0.0.1:1/v1", nil)
	var out struct {
		Servers  []mcpServerStatus `json:"servers"`
		Warnings []string          `json:"warnings"`
	}
	f.getJSON("/api/mcp", &out)
	if len(out.Servers) != 0 {
		t.Errorf("servers = %+v, want empty", out.Servers)
	}
}

// TestLogsRoutesDisabled checks the disabled-log-dir JSON shapes for the
// list route (the follow stream rides on logs_test.go's Follow tests).
func TestLogsRoutesDisabled(t *testing.T) {
	f := newRouterFixture(t, "http://127.0.0.1:1/v1", nil)
	var list TranscriptList
	f.getJSON("/api/logs", &list)
	if !list.Disabled {
		t.Errorf("logs list = %+v, want disabled", list)
	}
	resp := f.do(http.MethodGet, "/api/logs/transcript-1.jsonl", nil, "")
	drainExpectStatus(t, resp, http.StatusBadRequest)
}
