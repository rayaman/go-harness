package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Fake OpenAI-compatible chat server (roadmap 3.4)
//
// newFakeChatServer hands back a scripted queue of raw JSON response bodies,
// one per POST, and records every request body it received so tests can
// assert on exactly what the agent loop fed back on the next turn. Later
// roadmap items (1.4 retry, 1.1 compaction) can reuse this fixture.
// ---------------------------------------------------------------------------

type fakeChatServer struct {
	t      *testing.T
	server *httptest.Server

	mu        sync.Mutex
	responses []fakeReply
	received  [][]byte
}

// fakeReply is one scripted HTTP reply: a status code, the body to send
// with it, and an optional Retry-After header value (roadmap 1.4 retry
// tests). Status 0 means 200.
type fakeReply struct {
	status     int
	body       []byte
	retryAfter string
}

// okReply wraps a raw ChatResponse JSON body as a 200 reply.
func okReply(body []byte) fakeReply { return fakeReply{body: body} }

// statusReply scripts a non-200 reply with the given status, error body,
// and optional Retry-After header ("" = no header).
func statusReply(status int, body string, retryAfter string) fakeReply {
	return fakeReply{status: status, body: []byte(body), retryAfter: retryAfter}
}

// newFakeChatServer starts an httptest server scripted with the given
// fakeReply entries (see okReply/statusReply), popped one per request in
// order. A request arriving after the script is exhausted fails the test
// (that would mean the loop kept asking when it shouldn't have).
func newFakeChatServer(t *testing.T, responses ...fakeReply) *fakeChatServer {
	f := &fakeChatServer{t: t, responses: responses}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("fake chat server: read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.received = append(f.received, body)
		idx := len(f.received) - 1
		if idx >= len(f.responses) {
			f.t.Errorf("fake chat server: got request %d but only scripted %d response(s)", idx+1, len(f.responses))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reply := f.responses[idx]
		if reply.status != 0 {
			w.Header().Set("Content-Type", "text/plain")
			if reply.retryAfter != "" {
				w.Header().Set("Retry-After", reply.retryAfter)
			}
			w.WriteHeader(reply.status)
			w.Write(reply.body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(reply.body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// URL is the base URL to hand to NewClient (the client appends
// "/chat/completions").
func (f *fakeChatServer) URL() string { return f.server.URL }

// requests returns the request bodies received so far, in order.
func (f *fakeChatServer) requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.received))
	copy(out, f.received)
	return out
}

// chatTextReply scripts a plain assistant text reply (no tool calls).
func chatTextReply(text string) []byte {
	return mustJSON(map[string]any{
		"choices": []map[string]any{
			{
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			},
		},
	})
}

// chatToolCallReply scripts an assistant reply carrying exactly one tool
// call: id/type "function"/function.name plus function.arguments as a raw
// JSON string, exactly as the wire format in types.go defines it.
func chatToolCallReply(id, toolName, argumentsJSON string) []byte {
	return mustJSON(map[string]any{
		"choices": []map[string]any{
			{
				"message": map[string]any{
					"role":    "assistant",
					"content": "",
					"tool_calls": []map[string]any{
						{
							"id":   id,
							"type": "function",
							"function": map[string]any{
								"name":      toolName,
								"arguments": argumentsJSON,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("fake chat server: marshal scripted response: " + err.Error())
	}
	return b
}

// ---------------------------------------------------------------------------
// Fake tools
// ---------------------------------------------------------------------------

// fakeTool returns a canned output and records the raw arguments JSON of
// every Execute call. Execute rejects invalid JSON with an error, which is
// what the malformed-arguments test relies on.
type fakeTool struct {
	name   string
	output string

	mu   sync.Mutex
	args []string
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "fake test tool" }
func (f *fakeTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string"}},
	}
}

func (f *fakeTool) Execute(args json.RawMessage) (string, error) {
	f.mu.Lock()
	f.args = append(f.args, string(args))
	f.mu.Unlock()

	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return f.output, nil
}

func (f *fakeTool) recordedArgs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.args...)
}

// cappedTool is a fakeTool that also implements MaxOutputTyper, so its
// MaxOutputBytes() overrides Config.MaxToolOutputBytes (roadmap 1.5).
// The fakeTool is embedded by pointer to keep the mutex un-copied.
type cappedTool struct {
	*fakeTool
	limit int
}

func (c *cappedTool) MaxOutputBytes() int { return c.limit }

// ---------------------------------------------------------------------------
// Harness fixture
// ---------------------------------------------------------------------------

// newLoopHarness wires a Harness against the fake server on the
// OpenAI-compatible path (Provider "openai", NOT the ollama native path).
// The per-test Config only needs to override what the test cares about.
func newLoopHarness(serverURL string, cfg Config, registry *Registry) *Harness {
	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}
	if cfg.Model == "" {
		cfg.Model = "test-model"
	}
	if cfg.MaxToolIterations == 0 {
		cfg.MaxToolIterations = 10
	}
	return New(cfg, NewClient(serverURL, ""), registry)
}

// ---------------------------------------------------------------------------
// Agent-loop tests (roadmap 3.4)
// ---------------------------------------------------------------------------

// TestAskSimpleTextReply: one assistant text reply, no tool calls -> Ask
// returns it and History is exactly user + assistant (empty SystemPrompt
// means no seeded system message).
func TestAskSimpleTextReply(t *testing.T) {
	srv := newFakeChatServer(t, okReply(chatTextReply("hello there")))
	h := newLoopHarness(srv.URL(), Config{}, NewRegistry())

	got, err := h.Ask(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "hello there" {
		t.Errorf("Ask returned %q, want %q", got, "hello there")
	}

	if len(h.History) != 2 {
		t.Fatalf("History has %d messages, want 2: %+v", len(h.History), h.History)
	}
	if h.History[0].Role != "user" || ExtractText(h.History[0].Content) != "hi" {
		t.Errorf("History[0] = role %q content %q, want user/%q",
			h.History[0].Role, ExtractText(h.History[0].Content), "hi")
	}
	if h.History[1].Role != "assistant" || ExtractText(h.History[1].Content) != "hello there" {
		t.Errorf("History[1] = role %q content %q, want assistant/%q",
			h.History[1].Role, ExtractText(h.History[1].Content), "hello there")
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("server got %d requests, want 1", n)
	}
}

// TestAskToolCallRoundTrip: assistant tool_call -> fake tool runs with the
// exact arguments JSON -> its output is fed back as a "tool" message
// linked by tool_call_id, sitting between the assistant tool_calls message
// and the final assistant reply.
func TestAskToolCallRoundTrip(t *testing.T) {
	const argsJSON = `{"q":"hello"}`
	tool := &fakeTool{name: "fake_tool", output: "tool-output-42"}
	reg := NewRegistry()
	reg.Register(tool)

	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("call_1", "fake_tool", argsJSON)),
		okReply(chatTextReply("final answer")),
	)
	h := newLoopHarness(srv.URL(), Config{}, reg)

	got, err := h.Ask(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "final answer" {
		t.Errorf("Ask returned %q, want %q", got, "final answer")
	}

	// History: user, assistant(tool_calls), tool(result), assistant(final).
	if len(h.History) != 4 {
		t.Fatalf("History has %d messages, want 4: %+v", len(h.History), h.History)
	}
	assistant := h.History[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("History[1] = role %q tool_calls %d, want assistant with 1 tool call",
			assistant.Role, len(assistant.ToolCalls))
	}
	call := assistant.ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" ||
		call.Function.Name != "fake_tool" || call.Function.Arguments != argsJSON {
		t.Errorf("tool call = %+v, want id call_1/type function/fake_tool/%s", call, argsJSON)
	}
	toolMsg := h.History[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" ||
		ExtractText(toolMsg.Content) != "tool-output-42" {
		t.Errorf("History[2] = role %q tool_call_id %q content %q, want tool/call_1/tool-output-42",
			toolMsg.Role, toolMsg.ToolCallID, ExtractText(toolMsg.Content))
	}
	final := h.History[3]
	if final.Role != "assistant" || ExtractText(final.Content) != "final answer" ||
		len(final.ToolCalls) != 0 {
		t.Errorf("History[3] = role %q content %q tool_calls %d, want assistant/final answer/0",
			final.Role, ExtractText(final.Content), len(final.ToolCalls))
	}

	// The fake tool must have received the exact arguments JSON off the wire.
	recorded := tool.recordedArgs()
	if len(recorded) != 1 || recorded[0] != argsJSON {
		t.Errorf("tool received args %q, want exactly [%s]", recorded, argsJSON)
	}
	// And exactly two model requests went out (tool round trip + final).
	if n := len(srv.requests()); n != 2 {
		t.Errorf("server got %d requests, want 2", n)
	}
}

// TestAskMaxToolIterationsCap: the server always answers with tool calls,
// so after MaxToolIterations rounds the forced no-tools final answer also
// comes back with tool calls, and Ask must give up with an error naming
// the limit. (Roadmap 1.4 added that one extra forced request, hence the
// 4th scripted reply and 4-request expectation.)
func TestAskMaxToolIterationsCap(t *testing.T) {
	noop := &fakeTool{name: "noop", output: "ok"}
	reg := NewRegistry()
	reg.Register(noop)

	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("c1", "noop", `{}`)),
		okReply(chatToolCallReply("c2", "noop", `{}`)),
		okReply(chatToolCallReply("c3", "noop", `{}`)),
		okReply(chatToolCallReply("c4", "noop", `{}`)), // forced final: still tool calls
	)
	// MaxRetries must stay 0 here: the retry-disabled path is what keeps
	// the request count exactly MaxToolIterations+1.
	h := newLoopHarness(srv.URL(), Config{MaxToolIterations: 3}, reg)

	got, err := h.Ask(context.Background(), "loop forever")
	if err == nil {
		t.Fatalf("Ask returned %q, want a max-iterations error", got)
	}
	if got != "" {
		t.Errorf("Ask returned %q alongside error, want empty text", got)
	}
	if !strings.Contains(err.Error(), "max tool iterations") {
		t.Errorf("error = %q, want it to mention the max tool iterations limit", err)
	}
	if n := len(srv.requests()); n != 4 {
		t.Errorf("server got %d requests, want MaxToolIterations=3 + 1 forced final", n)
	}
}

// TestAskMaxToolIterationsZeroSkipsLoop: the guard around roadmap 1.4's
// graceful fallback. With MaxToolIterations == 0 the loop body never ran,
// so there is nothing to summarize: askLoop must return the cap error
// immediately and NOT fire the forced no-tools final request. Built with
// New directly because newLoopHarness would default a zero cap to 10.
func TestAskMaxToolIterationsZeroSkipsLoop(t *testing.T) {
	// Zero scripted responses: any request at all fails the test.
	srv := newFakeChatServer(t)
	h := New(Config{Provider: "openai", Model: "test-model", MaxToolIterations: 0},
		NewClient(srv.URL(), ""), NewRegistry())

	got, err := h.Ask(context.Background(), "loop forever")
	if err == nil {
		t.Fatalf("Ask returned %q, want a max-iterations error", got)
	}
	if got != "" {
		t.Errorf("Ask returned %q alongside error, want empty text", got)
	}
	if !strings.Contains(err.Error(), "max tool iterations") {
		t.Errorf("error = %q, want it to mention the max tool iterations limit", err)
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("server got %d requests, want 0 (cap 0 skips both the loop and the fallback)", n)
	}
}

// TestAskUnknownTool: the model calls a tool that isn't in the Registry;
// the tool message fed back on the NEXT request must tell the model
// "no such tool" (asserted against the captured request body).
func TestAskUnknownTool(t *testing.T) {
	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("call_x", "ghost_tool", `{}`)),
		okReply(chatTextReply("recovered")),
	)
	// Deliberately empty registry: "ghost_tool" does not exist.
	h := newLoopHarness(srv.URL(), Config{}, NewRegistry())

	got, err := h.Ask(context.Background(), "call something missing")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "recovered" {
		t.Errorf("Ask returned %q, want %q", got, "recovered")
	}

	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("server got %d requests, want 2", len(reqs))
	}
	var second ChatRequest
	if err := json.Unmarshal(reqs[1], &second); err != nil {
		t.Fatalf("unmarshal second request body: %v", err)
	}
	var toolMsgs []Message
	for _, m := range second.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 1 {
		t.Fatalf("second request has %d tool messages, want 1", len(toolMsgs))
	}
	if toolMsgs[0].ToolCallID != "call_x" {
		t.Errorf("tool message tool_call_id = %q, want call_x", toolMsgs[0].ToolCallID)
	}
	if content := ExtractText(toolMsgs[0].Content); !strings.Contains(content, "no such tool") {
		t.Errorf("tool message content = %q, want it to contain %q", content, "no such tool")
	}
}

// TestAskMalformedToolArguments: the model sends invalid JSON in
// function.arguments; the tool still gets the raw bad JSON, and its error
// (stringified by runTool) must reach the model with an "error:" prefix.
func TestAskMalformedToolArguments(t *testing.T) {
	tool := &fakeTool{name: "fake_tool", output: "unused"}
	reg := NewRegistry()
	reg.Register(tool)

	const badArgs = `{not valid json`
	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("call_bad", "fake_tool", badArgs)),
		okReply(chatTextReply("understood")),
	)
	h := newLoopHarness(srv.URL(), Config{}, reg)

	got, err := h.Ask(context.Background(), "send bad args")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "understood" {
		t.Errorf("Ask returned %q, want %q", got, "understood")
	}

	// The tool was invoked with the raw malformed JSON.
	recorded := tool.recordedArgs()
	if len(recorded) != 1 || recorded[0] != badArgs {
		t.Errorf("tool received args %q, want exactly [%s]", recorded, badArgs)
	}

	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("server got %d requests, want 2", len(reqs))
	}
	var second ChatRequest
	if err := json.Unmarshal(reqs[1], &second); err != nil {
		t.Fatalf("unmarshal second request body: %v", err)
	}
	var toolMsgs []Message
	for _, m := range second.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 1 {
		t.Fatalf("second request has %d tool messages, want 1", len(toolMsgs))
	}
	if content := ExtractText(toolMsgs[0].Content); !strings.Contains(content, "error:") {
		t.Errorf("tool message content = %q, want it to contain %q", content, "error:")
	}
}

// ---------------------------------------------------------------------------
// Retry + graceful final answer tests (roadmap 1.4)
// ---------------------------------------------------------------------------

// shrinkRetryBackoff makes the retry sleeps negligible for tests and
// restores the real 1s schedule afterwards.
func shrinkRetryBackoff(t *testing.T) {
	t.Helper()
	orig := retryBaseDelay
	retryBaseDelay = 10 * time.Millisecond
	t.Cleanup(func() { retryBaseDelay = orig })
}

// TestRetryServer5xxThenSuccess: the server 500s once, then answers
// normally; with MaxRetries=2 Ask must succeed on the retry, and the
// helper must have recorded exactly the failed attempt + the retry.
func TestRetryServer5xxThenSuccess(t *testing.T) {
	shrinkRetryBackoff(t)
	srv := newFakeChatServer(t,
		statusReply(http.StatusInternalServerError, "boom", ""),
		okReply(chatTextReply("second time's the charm")),
	)
	h := newLoopHarness(srv.URL(), Config{MaxRetries: 2}, NewRegistry())

	got, err := h.Ask(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "second time's the charm" {
		t.Errorf("Ask returned %q, want %q", got, "second time's the charm")
	}
	if n := len(srv.requests()); n != 2 {
		t.Errorf("server got %d requests, want 2 (one 500 + one retry)", n)
	}
}

// TestRetryNoRetryOn400: a 4xx (other than 408/429) is a permanent
// rejection - the request must be sent exactly once and the error must
// surface verbatim.
func TestRetryNoRetryOn400(t *testing.T) {
	shrinkRetryBackoff(t)
	srv := newFakeChatServer(t,
		statusReply(http.StatusBadRequest, "bad request", ""),
		okReply(chatTextReply("must never be asked")),
	)
	h := newLoopHarness(srv.URL(), Config{MaxRetries: 2}, NewRegistry())

	got, err := h.Ask(context.Background(), "hi")
	if err == nil {
		t.Fatalf("Ask returned %q, want the 400 error", got)
	}
	if !strings.Contains(err.Error(), "returned 400") {
		t.Errorf("error = %q, want it to mention %q", err, "returned 400")
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("server got %d requests, want exactly 1 (no retry on 400)", n)
	}
}

// TestRetryDisabledByDefault: MaxRetries unset (0) keeps the original
// single-attempt behavior: one 500 fails the turn immediately. Only one
// scripted response, so any retry would trip the fake server too.
func TestRetryDisabledByDefault(t *testing.T) {
	srv := newFakeChatServer(t, statusReply(http.StatusInternalServerError, "boom", ""))
	h := newLoopHarness(srv.URL(), Config{}, NewRegistry())

	got, err := h.Ask(context.Background(), "hi")
	if err == nil {
		t.Fatalf("Ask returned %q, want the 500 error", got)
	}
	if !strings.Contains(err.Error(), "returned 500") {
		t.Errorf("error = %q, want it to mention %q", err, "returned 500")
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("server got %d requests, want exactly 1 (retries disabled)", n)
	}
}

// TestIterationCapForcesFinalAnswer: with MaxToolIterations=2 and the
// model calling tools both rounds, askLoop must not error - it sends ONE
// more request with the tools schemas stripped (no "tools" key in the
// body) plus the stop instruction, and returns that plain-text reply.
func TestIterationCapForcesFinalAnswer(t *testing.T) {
	noop := &fakeTool{name: "noop", output: "ok"}
	reg := NewRegistry()
	reg.Register(noop)

	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("c1", "noop", `{}`)),
		okReply(chatToolCallReply("c2", "noop", `{}`)),
		okReply(chatTextReply("graceful final answer")),
	)
	h := newLoopHarness(srv.URL(), Config{MaxToolIterations: 2}, reg)

	got, err := h.Ask(context.Background(), "loop until capped")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "graceful final answer" {
		t.Errorf("Ask returned %q, want %q", got, "graceful final answer")
	}

	reqs := srv.requests()
	if len(reqs) != 3 {
		t.Fatalf("server got %d requests, want 3 (2 tool rounds + forced final)", len(reqs))
	}
	last := string(reqs[len(reqs)-1])
	// The forced final request must advertise no tools at all...
	if strings.Contains(last, `"tools"`) {
		t.Errorf("forced final request still carries a tools key: %s", last)
	}
	// ...and must carry the stop-calling-tools instruction.
	if !strings.Contains(last, "tool-call limit") {
		t.Errorf("forced final request missing the stop instruction: %s", last)
	}

	// The graceful answer landed in History like a normal final reply.
	final := h.History[len(h.History)-1]
	if final.Role != "assistant" || ExtractText(final.Content) != "graceful final answer" {
		t.Errorf("last History entry = role %q content %q, want assistant/graceful final answer",
			final.Role, ExtractText(final.Content))
	}
}

// TestIterationCapStillFails: if even the forced no-tools final request
// comes back with tool calls, Ask must still fail with the original
// "max tool iterations" wording (plus the secondary detail).
func TestIterationCapStillFails(t *testing.T) {
	noop := &fakeTool{name: "noop", output: "ok"}
	reg := NewRegistry()
	reg.Register(noop)

	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("c1", "noop", `{}`)),
		okReply(chatToolCallReply("c2", "noop", `{}`)),
		okReply(chatToolCallReply("c3", "noop", `{}`)),
	)
	h := newLoopHarness(srv.URL(), Config{MaxToolIterations: 2}, reg)

	got, err := h.Ask(context.Background(), "loop forever")
	if err == nil {
		t.Fatalf("Ask returned %q, want a max-iterations error", got)
	}
	if got != "" {
		t.Errorf("Ask returned %q alongside error, want empty text", got)
	}
	if !strings.Contains(err.Error(), "max tool iterations") {
		t.Errorf("error = %q, want it to mention the max tool iterations limit", err)
	}
	if n := len(srv.requests()); n != 3 {
		t.Errorf("server got %d requests, want 3 (2 tool rounds + forced final)", n)
	}
}

// ---------------------------------------------------------------------------
// Tool output size cap tests (roadmap 1.5)
// ---------------------------------------------------------------------------

// makeTenKOutput builds a 10 000-byte tool result: 99 'a' bytes, then a
// 4-byte rune starting at byte 99 so it straddles the head cut at
// cap/2 = 100, then 'b' bytes to the end. A naive byte slice at 100 would
// cut that rune mid-character.
func makeTenKOutput(t *testing.T) string {
	t.Helper()
	head := strings.Repeat("a", 99)
	runeStr := "🯰" // U+1FAF0, 4 bytes (F0 9F AF B0)
	tail := strings.Repeat("b", 10000-len(head)-len(runeStr))
	s := head + runeStr + tail
	if len(s) != 10000 {
		t.Fatalf("fixture is %d bytes, want 10000", len(s))
	}
	return s
}

// TestToolOutputTruncation: a 10 KB result with MaxToolOutputBytes=200
// comes back head + marker + tail, cut on rune boundaries.
func TestToolOutputTruncation(t *testing.T) {
	big := makeTenKOutput(t)

	h := New(Config{MaxToolIterations: 1, MaxToolOutputBytes: 200},
		NewClient("http://127.0.0.1:0", ""), NewRegistry())
	h.Registry.Register(&fakeTool{name: "big", output: big})

	out := h.runTool(ToolCall{
		ID: "x", Type: "function",
		Function: FunctionCall{Name: "big", Arguments: `{}`},
	})

	// Head is the 99 'a' bytes up to the straddling rune's start byte:
	// the cut at 100 snapped back to 99 so the 4-byte rune was never split.
	if !strings.HasPrefix(out, strings.Repeat("a", 99)) {
		t.Errorf("truncated output does not start with the 99-byte head")
	}
	// Tail is the last 100 bytes (cap - cap/2), all ASCII here.
	if !strings.HasSuffix(out, strings.Repeat("b", 100)) {
		t.Errorf("truncated output does not end with the 100-byte tail")
	}

	// The middle marker reports exactly how much was dropped of the total.
	wantMarker := fmt.Sprintf("...[truncated %d of %d bytes - narrow your request]...",
		9900-99, 10000)
	if !strings.Contains(out, wantMarker) {
		t.Errorf("truncated output missing marker %q; got:\n%s", wantMarker, out)
	}

	// Rune-safe slicing: the whole result is valid UTF-8 and the straddling
	// rune is gone entirely (a raw byte slice would have left broken bytes).
	if !utf8.ValidString(out) {
		t.Errorf("truncated output is not valid UTF-8 (rune was split at the cut)")
	}
	if strings.Contains(out, "🯰") {
		t.Errorf("straddling rune survived truncation, expected it fully dropped")
	}

	// head(99) + marker + tail(100): the cap budgets head+tail, the marker
	// is documented fixed overhead on top.
	if wantLen := 99 + len(wantMarker) + 100; len(out) != wantLen {
		t.Errorf("truncated output is %d bytes, want %d", len(out), wantLen)
	}
}

// TestToolOutputCapDisabled: MaxToolOutputBytes=0 (the default) leaves a
// huge result byte-for-byte untouched.
func TestToolOutputCapDisabled(t *testing.T) {
	big := makeTenKOutput(t)

	h := New(Config{MaxToolIterations: 1, MaxToolOutputBytes: 0},
		NewClient("http://127.0.0.1:0", ""), NewRegistry())
	h.Registry.Register(&fakeTool{name: "big", output: big})

	out := h.runTool(ToolCall{
		ID: "x", Type: "function",
		Function: FunctionCall{Name: "big", Arguments: `{}`},
	})
	if out != big {
		t.Errorf("output changed with cap 0: got %d bytes, want %d untouched", len(out), len(big))
	}
}

// TestToolOutputPerToolOverride: a tool implementing MaxOutputTyper has its
// value replace Config.MaxToolOutputBytes - both to tighten (config 0,
// tool 50) and to opt out (config 200, tool 0).
func TestToolOutputPerToolOverride(t *testing.T) {
	big := makeTenKOutput(t)

	// Tool cap tightens where the config cap is unlimited.
	h := New(Config{MaxToolIterations: 1, MaxToolOutputBytes: 0},
		NewClient("http://127.0.0.1:0", ""), NewRegistry())
	tight := &cappedTool{fakeTool: &fakeTool{name: "big", output: big}, limit: 50}
	h.Registry.Register(tight)

	out := h.runTool(ToolCall{
		ID: "x", Type: "function",
		Function: FunctionCall{Name: "big", Arguments: `{}`},
	})
	if !strings.Contains(out, "[truncated") {
		t.Errorf("tool cap 50 was ignored: output is %d bytes with no marker", len(out))
	}
	if !utf8.ValidString(out) {
		t.Errorf("tool-capped output is not valid UTF-8")
	}

	// Tool cap 0 opts the tool out of a global cap.
	h2 := New(Config{MaxToolIterations: 1, MaxToolOutputBytes: 200},
		NewClient("http://127.0.0.1:0", ""), NewRegistry())
	unlimited := &cappedTool{fakeTool: &fakeTool{name: "big", output: big}, limit: 0}
	h2.Registry.Register(unlimited)

	out2 := h2.runTool(ToolCall{
		ID: "x", Type: "function",
		Function: FunctionCall{Name: "big", Arguments: `{}`},
	})
	if out2 != big {
		t.Errorf("tool cap 0 did not opt out of the global cap: got %d bytes, want %d", len(out2), len(big))
	}
}

// ---------------------------------------------------------------------------
// JSONL transcript logging tests (roadmap 3.1)
// ---------------------------------------------------------------------------

// readTranscriptLines reads the single transcript-*.jsonl file in dir and
// returns its lines. Exactly one file is expected (per-session logging);
// zero files or a read failure fails the test.
func readTranscriptLines(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "transcript-*.jsonl"))
	if err != nil {
		t.Fatalf("glob transcript logs: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d transcript files in %s, want exactly 1", len(matches), dir)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read transcript log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// parseTranscriptEvents decodes every JSONL line into a map; each line
// must be one valid JSON object (that IS the JSONL contract).
func parseTranscriptEvents(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range readTranscriptLines(t, dir) {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("transcript line is not valid JSON (%v): %s", err, line)
		}
		events = append(events, ev)
	}
	return events
}

// TestTranscriptLoggingToolRoundTrip: with Config.LogDir set, a full
// tool-call round trip (request -> tool_calls -> tool result -> final
// answer) appends one JSON line per event kind in order: request,
// response, tool_call, request, response - with the tool name/args/result
// and the server-reported usage recorded, under a per-session
// transcript-<unixnano>.jsonl file.
func TestTranscriptLoggingToolRoundTrip(t *testing.T) {
	const argsJSON = `{"q":"hello"}`
	tool := &fakeTool{name: "fake_tool", output: "tool-output-42"}
	reg := NewRegistry()
	reg.Register(tool)

	// Usage-carrying scripted replies so the usage field is exercised.
	toolReply := mustJSON(map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []map[string]any{{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "fake_tool", "arguments": argsJSON},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 5, "total_tokens": 16},
	})
	finalReply := mustJSON(map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]any{"role": "assistant", "content": "final answer"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 3, "total_tokens": 23},
	})

	srv := newFakeChatServer(t, okReply(toolReply), okReply(finalReply))
	logDir := filepath.Join(t.TempDir(), "logs") // does not exist yet: New must create it

	h := newLoopHarness(srv.URL(), Config{LogDir: logDir}, reg)
	if _, err := h.Ask(context.Background(), "do the thing"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	// Release the transcript file so t.TempDir() cleanup can delete it
	// (Windows refuses to unlink open files).
	h.Close()

	events := parseTranscriptEvents(t, logDir)
	if len(events) != 5 {
		t.Fatalf("transcript has %d events, want 5 (req/resp/tool_call/req/resp): %+v", len(events), events)
	}

	wantKinds := []string{"request", "response", "tool_call", "request", "response"}
	for i, want := range wantKinds {
		if got := events[i]["kind"]; got != want {
			t.Errorf("event %d kind = %v, want %s", i, got, want)
		}
		if ts, ok := events[i]["ts"].(string); !ok || ts == "" {
			t.Errorf("event %d has no timestamp: %+v", i, events[i])
		}
	}

	// Request events carry model + counts, not full bodies.
	if events[0]["model"] != "test-model" || events[0]["messages"].(float64) != 1 || events[0]["tools"].(float64) != 1 {
		t.Errorf("first request event = %+v, want model test-model / 1 message / 1 tool", events[0])
	}

	// The tool_calls response event records usage from the server.
	if _, ok := events[1]["usage"]; !ok {
		t.Errorf("first response event has no usage: %+v", events[1])
	} else if u, ok := events[1]["usage"].(map[string]any); !ok || u["total_tokens"].(float64) != 16 {
		t.Errorf("first response usage = %+v, want total_tokens 16", events[1]["usage"])
	}
	if events[1]["tool_calls"].(float64) != 1 {
		t.Errorf("first response tool_calls = %v, want 1", events[1]["tool_calls"])
	}

	// The tool_call event carries name, arguments and the result.
	tc := events[2]
	if tc["name"] != "fake_tool" || tc["arguments"] != argsJSON || tc["result"] != "tool-output-42" {
		t.Errorf("tool_call event = %+v, want fake_tool/%s/tool-output-42", tc, argsJSON)
	}

	// The final response event carries the reply text and its usage.
	if events[4]["content"] != "final answer" {
		t.Errorf("final response content = %v, want %q", events[4]["content"], "final answer")
	}
	if u, ok := events[4]["usage"].(map[string]any); !ok || u["total_tokens"].(float64) != 23 {
		t.Errorf("final response usage = %+v, want total_tokens 23", events[4]["usage"])
	}
}

// ---------------------------------------------------------------------------
// Ctrl+C / context cancellation tests (roadmap 3.3)
// ---------------------------------------------------------------------------

// cancelTool cancels the turn's context from inside Execute, simulating
// Ctrl+C landing mid tool round trip (the REPL's signal wiring itself
// can't be unit tested, so the loop-level contract is tested here).
type cancelTool struct {
	name   string
	cancel context.CancelFunc

	mu  sync.Mutex
	ran int
}

func (c *cancelTool) Name() string        { return c.name }
func (c *cancelTool) Description() string { return "cancels the turn when run" }
func (c *cancelTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (c *cancelTool) Execute(args json.RawMessage) (string, error) {
	c.mu.Lock()
	c.ran++
	c.mu.Unlock()
	c.cancel()
	return "cancelling now", nil
}

// TestAskCancelMidTurn: the ctx is cancelled from inside a tool's
// Execute, so the NEXT request in the loop dies with context.Canceled.
// Ask must surface that error and askLoop must roll History back to
// exactly its pre-Ask state: no half-finished user/assistant turn, and
// above all no orphan tool message (the assistant tool_calls message is
// gone with the rollback, so nothing dangles).
func TestAskCancelMidTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tool := &cancelTool{name: "boom", cancel: cancel}
	reg := NewRegistry()
	reg.Register(tool)

	// Only the tool_calls reply is scripted: after the tool cancels, the
	// second request must fail in the transport without reaching the
	// fake server (an extra request would trip the fake server anyway).
	srv := newFakeChatServer(t, okReply(chatToolCallReply("call_1", "boom", `{}`)))
	h := newLoopHarness(srv.URL(), Config{SystemPrompt: "sys prompt"}, reg)

	before := len(h.History) // 1: just the seeded system message
	got, err := h.Ask(ctx, "turn that cancels itself")
	if err == nil {
		t.Fatalf("Ask returned %q, want a context.Canceled error", got)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if got != "" {
		t.Errorf("Ask returned %q alongside error, want empty text", got)
	}

	// History is exactly what it was before Ask: no orphan tool message,
	// no dangling tool_calls assistant turn.
	if len(h.History) != before {
		t.Fatalf("History has %d messages after cancel, want %d (pre-Ask): %+v",
			len(h.History), before, h.History)
	}
	if h.History[0].Role != "system" {
		t.Errorf("History[0] = role %q, want system", h.History[0].Role)
	}

	// The tool ran exactly once, and the cancelled second request never
	// reached the server (transport short-circuits on a dead ctx).
	tool.mu.Lock()
	ran := tool.ran
	tool.mu.Unlock()
	if ran != 1 {
		t.Errorf("cancel tool ran %d times, want 1", ran)
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("server got %d requests, want 1 (the cancelled follow-up must not arrive)", n)
	}
}

// TestAskCancelPreCancelledCtx: a context that is already done when Ask
// is called must fail immediately with context.Canceled, touch neither
// the server nor History, and leave the session exactly as it was.
func TestAskCancelPreCancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Zero scripted responses: any request at all fails the test.
	srv := newFakeChatServer(t)
	h := newLoopHarness(srv.URL(), Config{SystemPrompt: "sys prompt"}, NewRegistry())

	before := len(h.History)
	got, err := h.Ask(ctx, "never sent")
	if err == nil {
		t.Fatalf("Ask returned %q, want a context.Canceled error", got)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if got != "" {
		t.Errorf("Ask returned %q alongside error, want empty text", got)
	}
	if len(h.History) != before {
		t.Errorf("History has %d messages after cancel, want %d untouched: %+v",
			len(h.History), before, h.History)
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("server got %d requests, want 0", n)
	}
}

// TestTranscriptLoggingDisabledByDefault: an empty Config.LogDir (the
// zero value) writes nothing anywhere - the logger stays nil, every hook
// is a no-op, and the round trip behaves exactly as the unlogged tests
// above assert.
func TestTranscriptLoggingDisabledByDefault(t *testing.T) {
	tool := &fakeTool{name: "fake_tool", output: "tool-output-42"}
	reg := NewRegistry()
	reg.Register(tool)

	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("call_1", "fake_tool", `{"q":"hello"}`)),
		okReply(chatTextReply("final answer")),
	)
	cwdBefore, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	h := newLoopHarness(srv.URL(), Config{}, reg) // LogDir unset
	if _, err := h.Ask(context.Background(), "do the thing"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// No transcript file appears in the working directory...
	if matches, err := filepath.Glob(filepath.Join(cwdBefore, "transcript-*.jsonl")); err != nil || len(matches) != 0 {
		t.Errorf("found transcript files %v (err %v) with logging disabled, want none", matches, err)
	}
	// ...and none in the test's temp root either (belt and braces: the
	// logger must never open a file when LogDir is empty).
	if matches, err := filepath.Glob(filepath.Join(t.TempDir(), "transcript-*.jsonl")); err != nil || len(matches) != 0 {
		t.Errorf("found transcript files %v (err %v) with logging disabled, want none", matches, err)
	}
}
