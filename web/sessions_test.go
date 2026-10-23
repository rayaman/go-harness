package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-harness/harness"
	"ai-harness/harness/config"
)

// ---------------------------------------------------------------------------
// Session manager tests (web/sessions.go)
//
// Same fake OpenAI-compatible chat server pattern as
// harness/harness_test.go (stub /v1/chat/completions with a scripted
// queue), re-implemented here because that fixture lives in package
// harness's test binary. All session dirs point at t.TempDir() - these
// tests never touch the real agent/ tree.
// ---------------------------------------------------------------------------

type fakeChatServer struct {
	t      *testing.T
	server *httptest.Server

	mu        sync.Mutex
	responses [][]byte
	received  [][]byte
	// block, when set, makes the handler wait on it before answering
	// (cancel test).
	block chan struct{}
}

func newFakeChatServer(t *testing.T, responses ...[]byte) *fakeChatServer {
	f := &fakeChatServer{t: t, responses: responses}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("fake chat server: read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		idx := len(f.received)
		f.received = append(f.received, body)
		block := f.block
		f.mu.Unlock()

		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}

		if idx >= len(f.responses) {
			t.Errorf("fake chat server: got request %d but only scripted %d response(s)", idx+1, len(f.responses))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(f.responses[idx])
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeChatServer) URL() string { return f.server.URL }

func (f *fakeChatServer) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

func chatTextReply(text string) []byte {
	return mustJSON(map[string]any{"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
}

func chatUsageReply(text string, prompt, completion, total int) []byte {
	return mustJSON(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": total},
	})
}

func chatToolCallReply(id, toolName, argumentsJSON string) []byte {
	return mustJSON(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{
			"role":       "assistant",
			"content":    "",
			"tool_calls": []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": toolName, "arguments": argumentsJSON}}},
		}, "finish_reason": "tool_calls"}},
	})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("fake chat server: marshal scripted response: " + err.Error())
	}
	return b
}

// fakeTool returns a canned output and records every Execute.
type fakeTool struct {
	name   string
	output string

	mu   sync.Mutex
	args []string
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "fake test tool" }
func (f *fakeTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
}
func (f *fakeTool) Execute(args json.RawMessage) (string, error) {
	f.mu.Lock()
	f.args = append(f.args, string(args))
	f.mu.Unlock()
	return f.output, nil
}
func (f *fakeTool) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.args)
}

// newTestManager wires a SessionManager against the stub server with a
// temp settings file and a temp sessions dir (never the real agent/
// tree). Harness is nil in the web.Config, so the manager builds its
// own initial session.
func newTestManager(t *testing.T, baseURL string) (*SessionManager, string) {
	t.Helper()
	tmp := t.TempDir()
	sessionsDir := filepath.Join(tmp, "sessions")

	base := harness.Config{
		Provider:          "openai",
		BaseURL:           baseURL,
		Model:             "stub-model",
		MaxToolIterations: 10,
	}
	store, err := NewSettingsStore(filepath.Join(tmp, "web-settings.json"), base, nil)
	if err != nil {
		t.Fatalf("NewSettingsStore: %v", err)
	}
	m := NewSessionManager(Config{
		Base:     base,
		Conf:     &config.Config{},
		Client:   harness.NewClient(baseURL, ""),
		Registry: harness.NewRegistry(),
	}, store)
	m.sessionsDir = sessionsDir
	return m, sessionsDir
}

// collectEvents subscribes to s and records events until stop is called
// (or timeout). Returns a thread-safe recorder.
type eventRecorder struct {
	mu     sync.Mutex
	events []Event
	sub    chan Event
	done   chan struct{}
}

func startRecording(s *Session) *eventRecorder {
	rec := &eventRecorder{sub: s.Subscribe(), done: make(chan struct{})}
	go func() {
		for ev := range rec.sub {
			rec.mu.Lock()
			rec.events = append(rec.events, ev)
			rec.mu.Unlock()
		}
		close(rec.done)
	}()
	return rec
}

func (r *eventRecorder) stop(s *Session) {
	s.Unsubscribe(r.sub)
	<-r.done
}

func (r *eventRecorder) list() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func (r *eventRecorder) has(typ string) bool {
	for _, ev := range r.list() {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// CRUD, switching, delete rules
// ---------------------------------------------------------------------------

func TestSessionCreateListSwitchDelete(t *testing.T) {
	m, _ := newTestManager(t, "http://127.0.0.1:1/v1")

	initial := m.List()
	if len(initial) != 1 {
		t.Fatalf("fresh manager has %d sessions, want 1", len(initial))
	}
	firstID := initial[0].ID
	if !initial[0].Active {
		t.Errorf("initial session should be active")
	}

	b := m.Create("Work")
	c := m.Create("")
	if b.Name != "Work" {
		t.Errorf("Create(\"Work\").Name = %q", b.Name)
	}
	if !strings.HasPrefix(c.Name, "Session ") {
		t.Errorf("Create(\"\").Name = %q, want an auto-generated name", c.Name)
	}
	if m.ActiveID() != c.ID {
		t.Errorf("active = %q after Create, want the new session %q", m.ActiveID(), c.ID)
	}

	if err := m.Switch(firstID); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if m.ActiveID() != firstID {
		t.Errorf("active = %q after Switch, want %q", m.ActiveID(), firstID)
	}
	if err := m.Switch("nope"); err == nil {
		t.Errorf("Switch(unknown) returned nil error")
	}
	if m.Get("nope") != nil {
		t.Errorf("Get(unknown) returned non-nil")
	}

	list := m.List()
	if len(list) != 3 {
		t.Fatalf("List has %d sessions, want 3", len(list))
	}
	if !list[0].Active || list[1].Active || list[2].Active {
		t.Errorf("only the first session should be active: %+v", list)
	}

	if err := m.Rename(firstID, "Renamed"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := m.Get(firstID).Name; got != "Renamed" {
		t.Errorf("Name after Rename = %q", got)
	}

	// Deleting the active session moves the pointer; deleting the last
	// one is refused.
	if err := m.Delete(list[0].ID); err != nil {
		t.Fatalf("Delete active session: %v", err)
	}
	if m.ActiveID() == firstID {
		t.Errorf("active still points at the deleted session")
	}
	if err := m.Delete(list[1].ID); err != nil {
		t.Fatalf("Delete second session: %v", err)
	}
	if m.ActiveID() == list[1].ID {
		t.Errorf("active still points at the second deleted session")
	}
	if len(m.List()) != 1 {
		t.Fatalf("%d sessions left, want 1", len(m.List()))
	}
	if !m.List()[0].Active {
		t.Errorf("the sole remaining session should be active")
	}
	last := m.List()[0]
	if err := m.Delete(last.ID); err == nil || !strings.Contains(err.Error(), "last session") {
		t.Errorf("Delete of last session = %v, want a \"last session\" refusal", err)
	}
	if err := m.Delete("nope"); err == nil {
		t.Errorf("Delete(unknown) returned nil error")
	}
}

func TestSlugifySanitization(t *testing.T) {
	cases := map[string]string{
		"Work Chat":           "work-chat",
		"../../etc/passwd":    "etc-passwd",
		`weird\name:foo`:      "weird-name-foo",
		"!!!":                 "session",
		"":                    "session",
		"  Mixed CASE 42  ":   "mixed-case-42",
		"café tool":           "caf-tool",
		"transcript-12.jsonl": "transcript-12-jsonl",
		"a/../b":              "a-b",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Save / Load round-trip
// ---------------------------------------------------------------------------

func TestSessionSaveLoadRoundTrip(t *testing.T) {
	srv := newFakeChatServer(t, okUsageReply("saved reply"))
	m, sessionsDir := newTestManager(t, srv.URL())

	s := m.Active()
	if _, err := s.Post("hello", nil); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := m.Save(s.ID); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The file must land inside the sessions dir with a sanitized name.
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatalf("ReadDir(sessions dir): %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("sessions dir has %d entries, want 1: %+v", len(entries), entries)
	}
	path := filepath.Join(sessionsDir, entries[0].Name())

	loaded, err := m.Load("", path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatalf("Load returned nil session")
	}
	// The loaded session's conversation must match the saved one's
	// visible turns (user + assistant, no system prompt in the test base).
	if got := loaded.History(); len(got) != 2 || got[1].Text != "saved reply" {
		t.Errorf("loaded history = %+v, want user + assistant \"saved reply\"", got)
	}
	if m.ActiveID() != loaded.ID {
		t.Errorf("Load(id=\"\") should make the new session active, active=%q loaded=%q", m.ActiveID(), loaded.ID)
	}

	// Path traversal through the name must stay inside the dir: a name
	// full of traversal junk gets a safe slug.
	trav := m.Create("../../evil")
	if err := m.Save(trav.ID); err != nil {
		t.Fatalf("Save traversal-named session: %v", err)
	}
	entries, _ = os.ReadDir(sessionsDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "..") || strings.ContainsRune(e.Name(), os.PathSeparator) {
			t.Errorf("unsafe session file name %q", e.Name())
		}
	}
}

func okUsageReply(text string) []byte {
	return chatUsageReply(text, 10, 5, 15)
}

// ---------------------------------------------------------------------------
// Chat turn events
// ---------------------------------------------------------------------------

func TestChatTurnEmitsContentUsageDone(t *testing.T) {
	srv := newFakeChatServer(t, chatUsageReply("hello there", 12, 7, 19))
	m, _ := newTestManager(t, srv.URL())
	s := m.Active()

	rec := startRecording(s)

	answer, err := s.Post("hi", nil)
	// Stop BEFORE asserting: Unsubscribe drains the buffered channel
	// into the recorder, so list() sees the whole turn.
	rec.stop(s)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if answer != "hello there" {
		t.Errorf("Post answer = %q", answer)
	}

	events := rec.list()
	var content, done Event
	contentSeen, doneSeen, usageSeen := false, false, false
	for _, ev := range events {
		switch ev.Type {
		case EventContent:
			content, contentSeen = ev, true
		case EventUsage:
			usageSeen = true
			if ev.Usage == nil || ev.Usage.TotalTokens != 19 {
				t.Errorf("usage event = %+v, want total 19", ev.Usage)
			}
		case EventDone:
			done, doneSeen = ev, true
		}
	}
	if !contentSeen {
		t.Fatalf("no content event in %+v", events)
	}
	if content.Text != "hello there" {
		t.Errorf("content event text = %q", content.Text)
	}
	if !usageSeen {
		t.Errorf("no usage event in %+v", events)
	}
	if !doneSeen {
		t.Fatalf("no done event in %+v", events)
	}
	if done.Text != "hello there" || done.Session != s.ID {
		t.Errorf("done event = %+v, want text + session id", done)
	}
	if !rec.has(EventHello) || !rec.has(EventStatus) {
		t.Errorf("expected hello and status events, got %+v", events)
	}
}

func TestChatTurnEmitsToolCallAndResultEvents(t *testing.T) {
	tool := &fakeTool{name: "fake_tool", output: "tool-output-42"}
	srv := newFakeChatServer(t,
		chatToolCallReply("call_1", "fake_tool", `{"q":"x"}`),
		chatTextReply("final answer"),
	)
	m, _ := newTestManager(t, srv.URL())
	m.registry.Register(tool)
	s := m.Active()

	rec := startRecording(s)

	_, err := s.Post("use the tool", nil)
	rec.stop(s)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	var call, result Event
	callSeen, resultSeen := false, false
	for _, ev := range rec.list() {
		switch ev.Type {
		case EventToolCall:
			call, callSeen = ev, true
		case EventToolResult:
			result, resultSeen = ev, true
		}
	}
	if !callSeen || call.ID != "call_1" || call.Name != "fake_tool" || call.Args != `{"q":"x"}` {
		t.Errorf("tool_call event = %+v, want call_1/fake_tool/args", call)
	}
	if !resultSeen || result.CallID != "call_1" || result.Name != "fake_tool" ||
		result.Result != "tool-output-42" || !result.OK {
		t.Errorf("tool_result event = %+v, want call_1 output ok", result)
	}
	if tool.callCount() != 1 {
		t.Errorf("tool executed %d times, want 1", tool.callCount())
	}
}

// ---------------------------------------------------------------------------
// Approval bridge
// ---------------------------------------------------------------------------

func TestApprovalGrantedRunsTool(t *testing.T) {
	tool := &fakeTool{name: "fake_tool", output: "ran"}
	srv := newFakeChatServer(t,
		chatToolCallReply("call_ask", "fake_tool", `{}`),
		chatTextReply("after tool"),
	)
	m, _ := newTestManager(t, srv.URL())
	m.registry.Register(tool)
	m.registry.SetPolicy("fake_tool", harness.PolicyAsk)
	s := m.Active()

	rec := startRecording(s)

	var callID string
	var postErr error
	var postAnswer string
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		postAnswer, postErr = s.Post("go", nil)
	}()

	// Wait for the approval_request event, then answer true the way the
	// browser does.
	deadline := time.After(5 * time.Second)
	for callID == "" {
		select {
		case <-deadline:
			t.Fatalf("no approval_request event arrived; events=%+v", rec.list())
		default:
		}
		for _, ev := range rec.list() {
			if ev.Type == EventApprovalRequest && ev.ID == "call_ask" {
				callID = ev.ID
				if ev.Name != "fake_tool" {
					t.Errorf("approval_request name = %q", ev.Name)
				}
			}
		}
		if callID == "" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !s.ResolveApproval(callID, true) {
		t.Fatalf("ResolveApproval(%q, true) reported not pending", callID)
	}
	if s.ResolveApproval("bogus", true) {
		t.Errorf("ResolveApproval(unknown id) reported pending")
	}

	wg.Wait()
	rec.stop(s)
	if postErr != nil {
		t.Fatalf("Post after approval: %v", postErr)
	}
	if postAnswer != "after tool" {
		t.Errorf("answer = %q, want \"after tool\"", postAnswer)
	}
	if tool.callCount() != 1 {
		t.Errorf("tool ran %d times after approval, want 1", tool.callCount())
	}
	if !rec.has(EventApprovalResolved) {
		t.Fatalf("no approval_resolved event after answering")
	}
	for _, ev := range rec.list() {
		if ev.Type == EventApprovalResolved {
			if !ev.OK || ev.Resolved != "approved" {
				t.Errorf("approval_resolved = %+v, want ok=true resolved=approved", ev)
			}
		}
	}
}

func TestApprovalTimeoutDenies(t *testing.T) {
	tool := &fakeTool{name: "fake_tool", output: "should not run"}
	srv := newFakeChatServer(t,
		chatToolCallReply("call_timeout", "fake_tool", `{}`),
		chatTextReply("after denial"),
	)
	m, _ := newTestManager(t, srv.URL())
	m.registry.Register(tool)
	m.registry.SetPolicy("fake_tool", harness.PolicyAsk)
	s := m.Active()
	// Shrink the injected timeout field rather than the package const.
	s.approvalTimeout = 50 * time.Millisecond

	rec := startRecording(s)

	answer, err := s.Post("go", nil)
	rec.stop(s)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if answer != "after denial" {
		t.Errorf("answer = %q", answer)
	}
	if tool.callCount() != 0 {
		t.Errorf("tool ran %d times despite approval timeout, want 0", tool.callCount())
	}

	var resolved Event
	seen := false
	for _, ev := range rec.list() {
		if ev.Type == EventApprovalResolved {
			resolved, seen = ev, true
		}
	}
	if !seen {
		t.Fatalf("no approval_resolved event; events=%+v", rec.list())
	}
	if resolved.OK || resolved.Resolved != "timeout" {
		t.Errorf("approval_resolved = %+v, want ok=false resolved=timeout", resolved)
	}
	// The model must have been told the tool was declined: the tool
	// result message in History carries the harness's decline string.
	found := false
	for _, mv := range s.History() {
		if mv.Role == "tool" && strings.Contains(mv.Text, "user declined") {
			found = true
		}
	}
	if !found {
		t.Errorf("no \"user declined\" tool result in history: %+v", s.History())
	}
}

// ---------------------------------------------------------------------------
// Cancel
// ---------------------------------------------------------------------------

func TestCancelAbortsTurn(t *testing.T) {
	srv := newFakeChatServer(t) // no scripted replies: the handler blocks anyway
	srv.block = make(chan struct{})

	m, _ := newTestManager(t, srv.URL())
	s := m.Active()

	rec := startRecording(s)

	var postErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, postErr = s.Post("hang forever", nil)
	}()

	// Wait until the stub has actually received the request, then cancel.
	deadline := time.After(5 * time.Second)
	for srv.requestCount() == 0 {
		select {
		case <-deadline:
			t.Fatalf("stub server never received the turn's request")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !m.Cancel(s.ID) {
		t.Fatalf("Cancel reported no in-flight turn while one was running")
	}
	if m.Cancel("nope") {
		t.Errorf("Cancel(unknown session) = true, want false")
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Post did not return after Cancel")
	}
	rec.stop(s)
	if postErr == nil {
		t.Errorf("Post returned nil error after Cancel, want a cancellation error")
	}
	if !rec.has(EventError) {
		t.Errorf("no error event after cancellation; events=%+v", rec.list())
	}
	// askLoop rolls History back on cancellation: the session must be
	// pristine for the next turn.
	if len(s.History()) != 0 {
		t.Errorf("history not rolled back after cancel: %+v", s.History())
	}
	// Turn mutex released: a second Cancel finds nothing running.
	if m.Cancel(s.ID) {
		t.Errorf("Cancel after the turn ended = true, want false")
	}
}

// ---------------------------------------------------------------------------
// ReconfigureAll and registry surfaces
// ---------------------------------------------------------------------------

func TestReconfigureAllAppliesEffectiveSettings(t *testing.T) {
	m, tmp := newTestManager(t, "http://127.0.0.1:1/v1")
	_ = tmp

	// PUT a model override through the settings store's file layer the
	// same way the router's PUT handler will, then push it live.
	path := m.store.path
	eff := m.store.Effective()
	if eff.Model != "stub-model" {
		t.Fatalf("effective model = %q", eff.Model)
	}
	override := WebSettings{Model: "overridden-model"}
	if err := override.Save(path); err != nil {
		t.Fatalf("Save overrides: %v", err)
	}
	reloaded, err := NewSettingsStore(path, m.store.base, nil)
	if err != nil {
		t.Fatalf("reload store: %v", err)
	}
	m.store = reloaded

	m.ReconfigureAll()
	for _, info := range m.List() {
		if got := m.Get(info.ID).harness.Config.Model; got != "overridden-model" {
			t.Errorf("session %s model = %q after ReconfigureAll, want the override", info.ID, got)
		}
	}
	// A session created AFTER the change builds from the new config too.
	if got := m.Create("later").harness.Config.Model; got != "overridden-model" {
		t.Errorf("new session model = %q, want the override", got)
	}
}

func TestSetPolicyToolsAndReload(t *testing.T) {
	m, _ := newTestManager(t, "http://127.0.0.1:1/v1")
	m.registry.Register(&fakeTool{name: "fake_tool"})
	m.registry.Register(&fakeTool{name: "run_shell_command"})

	m.SetPolicy("fake_tool", harness.PolicyAsk)
	if got := m.registry.PolicyFor("fake_tool"); got != harness.PolicyAsk {
		t.Errorf("PolicyFor after SetPolicy = %v, want ask", got)
	}

	tools := m.Tools()
	names := make(map[string]harness.Policy)
	for _, tp := range tools {
		names[tp.Name] = tp.Policy
	}
	if names["fake_tool"] != harness.PolicyAsk {
		t.Errorf("Tools() = %+v, want fake_tool ask", tools)
	}

	// ReloadTools re-applies config.yaml's lists (the UI SetPolicy is
	// overwritten, exactly like the REPL) AND re-registers Lua tools
	// from the custom dir.
	customDir := filepath.Join(t.TempDir(), "custom_tools")
	if err := os.MkdirAll(customDir, 0o755); err != nil {
		t.Fatal(err)
	}
	luaSrc := `
function name() return "lua_echo" end
function description() return "echo tool" end
function parameters() return {type="object", properties={}} end
function execute(args) return "echoed" end
`
	if err := os.WriteFile(filepath.Join(customDir, "echo.lua"), []byte(luaSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-.lua file and a subdirectory must be skipped without noise.
	if err := os.WriteFile(filepath.Join(customDir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(customDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	m.customToolDir = customDir
	m.conf = &config.Config{ToolPermissions: struct {
		Allow []string
		Ask   []string
		Deny  []string
	}{Ask: []string{"fake_tool"}, Deny: []string{"denied_tool"}}}

	m.ReloadTools()

	if got := m.registry.PolicyFor("fake_tool"); got != harness.PolicyAsk {
		t.Errorf("fake_tool policy after reload = %v, want ask (config-listed)", got)
	}
	if got := m.registry.PolicyFor("run_shell_command"); got != harness.PolicyAsk {
		t.Errorf("run_shell_command policy after reload = %v, want the ask default", got)
	}
	if got := m.registry.PolicyFor("denied_tool"); got != harness.PolicyDeny {
		t.Errorf("denied_tool policy after reload = %v, want deny", got)
	}
	if _, ok := m.registry.Get("lua_echo"); !ok {
		t.Errorf("lua_echo tool not registered after ReloadTools; tools=%+v", m.Tools())
	}

	// Second reload with no change must not re-register (mtime tracked):
	// deleting the file then reloading keeps the registration (the
	// harness never unregisters), but a MODIFIED file re-registers.
	m.ReloadTools()
	if _, ok := m.registry.Get("lua_echo"); !ok {
		t.Errorf("lua_echo lost after second ReloadTools")
	}

	// LogDir comes from the live effective config.
	if m.LogDir() != "" {
		t.Errorf("LogDir() = %q, want empty when log_dir unset", m.LogDir())
	}
	override := WebSettings{LogDir: filepath.Join("agent", "test-logs")}
	if err := override.Save(m.store.path); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewSettingsStore(m.store.path, m.store.base, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.store = reloaded
	if m.LogDir() != filepath.Join("agent", "test-logs") {
		t.Errorf("LogDir() = %q, want the override", m.LogDir())
	}
}
