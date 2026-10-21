package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Session persistence tests (roadmap 1.3)
//
// Reuse the fake chat server fixture from harness_test.go for the
// save -> load -> Ask round trip.
// ---------------------------------------------------------------------------

// writeSessionFile marshals a Session and writes it to dir/name,
// returning the path. Tests use it to hand LoadSession crafted files.
func writeSessionFile(t *testing.T, dir, name string, sess Session) string {
	t.Helper()
	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	return path
}

// loadedHistory builds a valid conversation exercising the shapes a real
// session carries: system prompt, plain user/assistant turns, and an
// assistant tool_calls + tool result pair.
func loadedHistory() []Message {
	return []Message{
		{Role: "system", Content: "you are a test persona"},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{
			ID: "call_rt", Type: "function",
			Function: FunctionCall{Name: "fake_tool", Arguments: `{"q":"first"}`},
		}}},
		{Role: "tool", Content: "tool said hi", ToolCallID: "call_rt", Name: "fake_tool"},
		{Role: "assistant", Content: "answer from the loaded context"},
	}
}

// TestSaveLoadRoundTrip: save a seeded conversation into a nested temp
// path (exercising MkdirAll), load it into a fresh Harness, and check
// History matches message-for-message. Then Ask against a fake server and
// assert the recorded request body carries the loaded conversation - the
// loaded context really goes out on the wire.
func TestSaveLoadRoundTrip(t *testing.T) {
	seed := Config{SystemPrompt: "you are a test persona"}
	h1 := newLoopHarness("http://127.0.0.1:0", seed, NewRegistry())
	h1.History = loadedHistory()

	path := filepath.Join(t.TempDir(), "nested", "dir", "session.json")
	if err := h1.SaveSession(path); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	srv := newFakeChatServer(t, okReply(chatTextReply("reply built on loaded history")))
	h2 := newLoopHarness(srv.URL(), Config{}, NewRegistry())
	if err := h2.LoadSession(path); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	if len(h2.History) != len(h1.History) {
		t.Fatalf("loaded History has %d messages, want %d: %+v",
			len(h2.History), len(h1.History), h2.History)
	}
	for i, want := range h1.History {
		got := h2.History[i]
		if got.Role != want.Role {
			t.Errorf("History[%d].Role = %q, want %q", i, got.Role, want.Role)
		}
		if gotText, wantText := ExtractText(got.Content), ExtractText(want.Content); gotText != wantText {
			t.Errorf("History[%d] content = %q, want %q", i, gotText, wantText)
		}
	}
	// The tool-call pair survived the round trip intact.
	asst := h2.History[2]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_rt" {
		t.Errorf("History[2] tool_calls = %+v, want one call with id call_rt", asst.ToolCalls)
	}
	if h2.History[3].Role != "tool" || h2.History[3].ToolCallID != "call_rt" {
		t.Errorf("History[3] = role %q tool_call_id %q, want tool/call_rt",
			h2.History[3].Role, h2.History[3].ToolCallID)
	}
	// LoadSession restored the saved persona and model into Config.
	if h2.Config.SystemPrompt != "you are a test persona" {
		t.Errorf("Config.SystemPrompt = %q, want the saved persona restored", h2.Config.SystemPrompt)
	}
	if h2.Config.Model != "test-model" {
		t.Errorf("Config.Model = %q, want the saved model restored", h2.Config.Model)
	}

	// A subsequent Ask must ride on the loaded conversation.
	if _, err := h2.Ask(context.Background(), "follow-up question"); err != nil {
		t.Fatalf("Ask after load: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("server got %d requests, want 1", len(reqs))
	}
	body := string(reqs[0])
	for _, want := range []string{"answer from the loaded context", "tool said hi", "you are a test persona"} {
		if !strings.Contains(body, want) {
			t.Errorf("request body missing loaded message %q:\n%s", want, body)
		}
	}
}

// TestLoadSessionRejectsOrphanToolMessage: a "tool" message whose
// tool_call_id was never announced by an earlier assistant tool_calls is
// a conversation no provider will accept. LoadSession must error and
// leave History untouched.
func TestLoadSessionRejectsOrphanToolMessage(t *testing.T) {
	path := writeSessionFile(t, t.TempDir(), "orphan.json", Session{
		Model: "test-model",
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "tool", Content: "result of a call nobody made", ToolCallID: "call_ghost", Name: "fake_tool"},
			{Role: "assistant", Content: "done"},
		},
	})

	h := newLoopHarness("http://127.0.0.1:0", Config{SystemPrompt: "persona"}, NewRegistry())
	before := len(h.History)

	err := h.LoadSession(path)
	if err == nil {
		t.Fatalf("LoadSession accepted an orphan tool message; History = %+v", h.History)
	}
	if !strings.Contains(err.Error(), "call_ghost") {
		t.Errorf("error = %q, want it to name the orphan tool_call_id call_ghost", err)
	}
	if len(h.History) != before {
		t.Errorf("History changed to %d messages after rejected load, want unchanged (%d)",
			len(h.History), before)
	}
}

// TestLoadSessionRejectsEmpty: an empty Messages list is rejected.
func TestLoadSessionRejectsEmpty(t *testing.T) {
	path := writeSessionFile(t, t.TempDir(), "empty.json", Session{
		Model:        "test-model",
		SystemPrompt: "persona",
		Messages:     []Message{},
	})

	h := newLoopHarness("http://127.0.0.1:0", Config{SystemPrompt: "persona"}, NewRegistry())
	before := len(h.History)

	err := h.LoadSession(path)
	if err == nil {
		t.Fatalf("LoadSession accepted an empty session; History = %+v", h.History)
	}
	if !strings.Contains(err.Error(), "no messages") {
		t.Errorf("error = %q, want it to mention that the session has no messages", err)
	}
	if len(h.History) != before {
		t.Errorf("History changed to %d messages after rejected load, want unchanged (%d)",
			len(h.History), before)
	}
}

// TestSaveSessionAtomic: two saves to the same path leave no *.tmp file
// behind and the second save's content wins.
func TestSaveSessionAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	h := newLoopHarness("http://127.0.0.1:0", Config{SystemPrompt: "v1 persona"}, NewRegistry())
	h.History = []Message{{Role: "user", Content: "first save"}}
	if err := h.SaveSession(path); err != nil {
		t.Fatalf("first SaveSession: %v", err)
	}

	h.History = []Message{{Role: "user", Content: "second save wins"}}
	if err := h.SaveSession(path); err != nil {
		t.Fatalf("second SaveSession: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read session dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %q after atomic save", e.Name())
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		t.Fatalf("unmarshal session file: %v", err)
	}
	if len(sess.Messages) != 1 || ExtractText(sess.Messages[0].Content) != "second save wins" {
		t.Errorf("session file messages = %+v, want the second save's content to win", sess.Messages)
	}
}

// TestResetKeepsSystemPrompt: Reset mirrors New's seeding - exactly the
// system message with a SystemPrompt set, exactly zero messages without.
func TestResetKeepsSystemPrompt(t *testing.T) {
	h := newLoopHarness("http://127.0.0.1:0", Config{SystemPrompt: "keep me"}, NewRegistry())
	h.History = append(h.History,
		Message{Role: "user", Content: "old turn"},
		Message{Role: "assistant", Content: "old reply"},
	)

	h.Reset()

	if len(h.History) != 1 {
		t.Fatalf("History has %d messages after Reset, want exactly the system message: %+v",
			len(h.History), h.History)
	}
	if h.History[0].Role != "system" || ExtractText(h.History[0].Content) != "keep me" {
		t.Errorf("History[0] = role %q content %q, want system/%q",
			h.History[0].Role, ExtractText(h.History[0].Content), "keep me")
	}

	// No system prompt: Reset leaves an entirely empty conversation.
	h2 := newLoopHarness("http://127.0.0.1:0", Config{}, NewRegistry())
	h2.History = append(h2.History, Message{Role: "user", Content: "turn"})
	h2.Reset()
	if len(h2.History) != 0 {
		t.Errorf("History has %d messages after Reset with empty SystemPrompt, want 0: %+v",
			len(h2.History), h2.History)
	}
}

// TestLoadSessionRestoresModel: a session saved under a different model
// switches h.Config.Model on load, so /load also restores the model.
func TestLoadSessionRestoresModel(t *testing.T) {
	path := writeSessionFile(t, t.TempDir(), "model.json", Session{
		Model:        "other",
		SystemPrompt: "persona",
		Messages:     []Message{{Role: "user", Content: "hi"}},
	})

	h := newLoopHarness("http://127.0.0.1:0", Config{SystemPrompt: "persona"}, NewRegistry())
	if h.Config.Model != "test-model" {
		t.Fatalf("precondition: Config.Model = %q, want test-model", h.Config.Model)
	}

	if err := h.LoadSession(path); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if h.Config.Model != "other" {
		t.Errorf("Config.Model = %q after load, want %q", h.Config.Model, "other")
	}
}
