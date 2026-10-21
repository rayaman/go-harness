package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Permission gate tests (roadmap 1.2)
//
// Reuse the fake chat server + fake tools from harness_test.go: scripted
// tool-call round trips let each test assert exactly what string the gate
// fed back to the model on the NEXT request.
// ---------------------------------------------------------------------------

// toolMessages pulls the "tool"-role messages out of a recorded request
// body (the results the loop fed back after the previous turn).
func toolMessages(t *testing.T, body []byte) []Message {
	t.Helper()
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal recorded request body: %v", err)
	}
	var out []Message
	for _, m := range req.Messages {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

// TestPolicyDenyBlocksTool: a tool at PolicyDeny must never Execute; the
// model-visible string fed back must be the "not permitted" message, and
// the loop must continue to the model's final answer.
func TestPolicyDenyBlocksTool(t *testing.T) {
	tool := &fakeTool{name: "dangerous", output: "should never run"}
	reg := NewRegistry()
	reg.Register(tool)
	reg.SetPolicy("dangerous", PolicyDeny)

	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("c1", "dangerous", `{"q":"go"}`)),
		okReply(chatTextReply("understood")),
	)
	h := newLoopHarness(srv.URL(), Config{}, reg)

	got, err := h.Ask(context.Background(), "call the dangerous tool")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "understood" {
		t.Errorf("Ask returned %q, want %q (loop should continue past the denial)", got, "understood")
	}

	if recorded := tool.recordedArgs(); len(recorded) != 0 {
		t.Errorf("denied tool executed %d time(s) with args %q, want 0 executions", len(recorded), recorded)
	}

	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("server got %d requests, want 2", len(reqs))
	}
	msgs := toolMessages(t, reqs[1])
	if len(msgs) != 1 {
		t.Fatalf("second request has %d tool messages, want 1", len(msgs))
	}
	if want := `error: tool "dangerous" is not permitted by configuration`; msgs[0].ToolCallID != "c1" ||
		ExtractText(msgs[0].Content) != want {
		t.Errorf("tool message = tool_call_id %q content %q, want c1/%q",
			msgs[0].ToolCallID, ExtractText(msgs[0].Content), want)
	}
}

// TestPolicyAskDeclined: PolicyAsk + a Confirm hook that returns false
// must skip Execute and feed the "user declined" string back to the
// model; Confirm receives the pending ToolCall (name + raw args).
func TestPolicyAskDeclined(t *testing.T) {
	tool := &fakeTool{name: "risky", output: "should not run"}
	reg := NewRegistry()
	reg.Register(tool)
	reg.SetPolicy("risky", PolicyAsk)

	var gotCall ToolCall
	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("c2", "risky", `{"q":"please"}`)),
		okReply(chatTextReply("moving on")),
	)
	h := newLoopHarness(srv.URL(), Config{}, reg)
	h.Confirm = func(call ToolCall) bool {
		gotCall = call
		return false
	}

	got, err := h.Ask(context.Background(), "call the risky tool")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "moving on" {
		t.Errorf("Ask returned %q, want %q", got, "moving on")
	}

	if gotCall.ID != "c2" || gotCall.Function.Name != "risky" ||
		gotCall.Function.Arguments != `{"q":"please"}` {
		t.Errorf("Confirm got call %+v, want c2/risky/{\"q\":\"please\"}", gotCall)
	}
	if recorded := tool.recordedArgs(); len(recorded) != 0 {
		t.Errorf("declined tool executed %d time(s) with args %q, want 0 executions", len(recorded), recorded)
	}

	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("server got %d requests, want 2", len(reqs))
	}
	msgs := toolMessages(t, reqs[1])
	if len(msgs) != 1 {
		t.Fatalf("second request has %d tool messages, want 1", len(msgs))
	}
	if want := `user declined to run tool "risky"`; !strings.Contains(ExtractText(msgs[0].Content), want) {
		t.Errorf("tool message content = %q, want it to contain %q", ExtractText(msgs[0].Content), want)
	}
}

// TestPolicyAskConfirmed: Confirm returning true runs the tool exactly as
// if no policy were configured (output fed back, loop continues).
func TestPolicyAskConfirmed(t *testing.T) {
	tool := &fakeTool{name: "risky", output: "tool-output-77"}
	reg := NewRegistry()
	reg.Register(tool)
	reg.SetPolicy("risky", PolicyAsk)

	confirmCalls := 0
	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("c3", "risky", `{}`)),
		okReply(chatTextReply("done")),
	)
	h := newLoopHarness(srv.URL(), Config{}, reg)
	h.Confirm = func(call ToolCall) bool {
		confirmCalls++
		return true
	}

	got, err := h.Ask(context.Background(), "call the risky tool")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "done" {
		t.Errorf("Ask returned %q, want %q", got, "done")
	}
	if confirmCalls != 1 {
		t.Errorf("Confirm called %d times, want 1", confirmCalls)
	}
	if recorded := tool.recordedArgs(); len(recorded) != 1 {
		t.Errorf("tool executed %d time(s), want exactly 1 (confirmed call must run)", len(recorded))
	}

	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("server got %d requests, want 2", len(reqs))
	}
	msgs := toolMessages(t, reqs[1])
	if len(msgs) != 1 || ExtractText(msgs[0].Content) != "tool-output-77" {
		t.Errorf("tool message content = %v, want the tool's normal output", msgs)
	}
}

// TestPolicyNilConfirmAllows: PolicyAsk with no Confirm hook set
// auto-allows (documented embedder-compatibility behavior); the tool
// runs normally.
func TestPolicyNilConfirmAllows(t *testing.T) {
	tool := &fakeTool{name: "risky", output: "ran-anyway"}
	reg := NewRegistry()
	reg.Register(tool)
	reg.SetPolicy("risky", PolicyAsk)

	srv := newFakeChatServer(t,
		okReply(chatToolCallReply("c4", "risky", `{}`)),
		okReply(chatTextReply("fine")),
	)
	h := newLoopHarness(srv.URL(), Config{}, reg) // h.Confirm stays nil

	got, err := h.Ask(context.Background(), "call the risky tool")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "fine" {
		t.Errorf("Ask returned %q, want %q", got, "fine")
	}
	if recorded := tool.recordedArgs(); len(recorded) != 1 {
		t.Errorf("tool executed %d time(s), want 1 (nil Confirm must auto-allow)", len(recorded))
	}
}

// TestRegisterKeepsPolicy: re-registering a tool (the hot-reload path)
// must not clobber a policy set for that name; unconfigured names stay
// PolicyAllow. Also pins ApplyPolicies' precedence and full-replacement
// semantics, which the REPL and workflow runner rely on.
func TestRegisterKeepsPolicy(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeTool{name: "hot", output: "v1"})
	reg.SetPolicy("hot", PolicyDeny)
	reg.Register(&fakeTool{name: "hot", output: "v2"})

	if p := reg.PolicyFor("hot"); p != PolicyDeny {
		t.Errorf("policy after re-Register = %v, want PolicyDeny to survive", p)
	}
	if p := reg.PolicyFor("never-configured"); p != PolicyAllow {
		t.Errorf("unconfigured policy = %v, want PolicyAllow (zero value)", p)
	}

	// Precedence: deny beats ask beats allow on name collision. x is in
	// all three lists, y in allow+ask, z in ask only, w nowhere (allow).
	reg.ApplyPolicies([]string{"x", "y"}, []string{"x", "y", "z"}, []string{"x"})
	if reg.PolicyFor("x") != PolicyDeny || reg.PolicyFor("y") != PolicyAsk ||
		reg.PolicyFor("z") != PolicyAsk || reg.PolicyFor("w") != PolicyAllow {
		t.Errorf("ApplyPolicies precedence wrong: x=%v y=%v z=%v w=%v, want deny/ask/ask/allow",
			reg.PolicyFor("x"), reg.PolicyFor("y"), reg.PolicyFor("z"), reg.PolicyFor("w"))
	}
	// Full replacement: a later call with empty lists reverts to allow.
	reg.ApplyPolicies(nil, nil, nil)
	if reg.PolicyFor("x") != PolicyAllow {
		t.Errorf("policy after empty ApplyPolicies = %v, want PolicyAllow", reg.PolicyFor("x"))
	}
}
