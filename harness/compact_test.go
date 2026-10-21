package harness

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Compaction tests (roadmap 1.1)
//
// The fake chat server pops scripted replies one per POST, and the
// compaction summary call IS one of those POSTs, so scripts list the
// summary reply at the point in call order where compact.go fires it.
// ---------------------------------------------------------------------------

// chatTextReplyUsage is chatTextReply plus a usage block, so tests can
// script the PromptTokens that the auto-trigger measures.
func chatTextReplyUsage(text string, usage Usage) []byte {
	return mustJSON(map[string]any{
		"choices": []map[string]any{
			{
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			},
		},
		"usage": usage,
	})
}

// seedHistory appends n plain alternating user/assistant messages tagged
// so tests can identify them by content.
func seedHistory(h *Harness, n int, tag string) {
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		h.History = append(h.History, Message{Role: role, Content: fmt.Sprintf("%s-%d", tag, i)})
	}
}

// TestCompactReplacesOldHistory: system + 13 old + 4 recent messages with
// CompactKeepTurns=4 -> Compact summarizes the 13 old ones, keeping the
// original system message at index 0, a summary system message at index 1,
// and the four recent messages verbatim at the tail.
func TestCompactReplacesOldHistory(t *testing.T) {
	srv := newFakeChatServer(t, okReply(chatTextReply("a compact summary")))
	h := newLoopHarness(srv.URL(), Config{SystemPrompt: "SYS PROMPT", CompactKeepTurns: 4}, NewRegistry())
	seedHistory(h, 13, "old")
	recent := []Message{
		{Role: "user", Content: "recent-1"},
		{Role: "assistant", Content: "recent-2"},
		{Role: "user", Content: "recent-3"},
		{Role: "assistant", Content: "recent-4"},
	}
	h.History = append(h.History, recent...)
	before := len(h.History) // 18

	changed, err := h.Compact(context.Background())
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !changed {
		t.Fatalf("Compact returned false, want true")
	}
	if len(h.History) >= before {
		t.Fatalf("History is %d messages after compaction, want fewer than %d", len(h.History), before)
	}

	if h.History[0].Role != "system" || ExtractText(h.History[0].Content) != "SYS PROMPT" {
		t.Errorf("History[0] = role %q content %q, want the original system message verbatim",
			h.History[0].Role, ExtractText(h.History[0].Content))
	}
	summary := ExtractText(h.History[1].Content)
	if h.History[1].Role != "system" || !strings.HasPrefix(summary, "Summary of earlier conversation:") {
		t.Errorf("History[1] = role %q content %q, want a system message starting with the summary prefix",
			h.History[1].Role, summary)
	}
	if !strings.Contains(summary, "a compact summary") {
		t.Errorf("summary message %q does not carry the model's summary text", summary)
	}

	tail := h.History[len(h.History)-4:]
	for i, want := range recent {
		if tail[i].Role != want.Role || ExtractText(tail[i].Content) != ExtractText(want.Content) {
			t.Errorf("kept tail[%d] = role %q content %q, want %q/%q verbatim",
				i, tail[i].Role, ExtractText(tail[i].Content), want.Role, ExtractText(want.Content))
		}
	}

	// Exactly one POST: the summary request itself, carrying the older
	// messages plus the compaction instruction.
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("server got %d requests, want 1 (the summary call)", len(reqs))
	}
	if !strings.Contains(string(reqs[0]), "Summarize the conversation above") {
		t.Errorf("summary request missing the compaction instruction: %s", reqs[0])
	}
	// The summary call's usage (absent here) is intentionally not folded
	// into LastUsage.
	if h.LastUsage != (Usage{}) {
		t.Errorf("LastUsage = %+v after Compact, want it untouched (summary usage ignored)", h.LastUsage)
	}
}

// TestCompactPreservesToolPairing: the naive keep-window boundary lands on
// a tool message, so Compact walks back to the announcing assistant
// message. Afterwards the whole conversation, summary included, still
// satisfies session.go's tool-pairing invariant.
func TestCompactPreservesToolPairing(t *testing.T) {
	srv := newFakeChatServer(t, okReply(chatTextReply("pairing summary")))
	h := newLoopHarness(srv.URL(), Config{SystemPrompt: "SYS PROMPT", CompactKeepTurns: 4}, NewRegistry())
	// len = 9 (incl. system): naive split at 9-4=5 lands exactly on the
	// tool message, which must instead move the boundary back to index 4.
	h.History = append(h.History,
		Message{Role: "user", Content: "old-1"},
		Message{Role: "assistant", Content: "old-2"},
		Message{Role: "user", Content: "old-3"},
		Message{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "tc1", Type: "function", Function: FunctionCall{Name: "noop", Arguments: `{}`}},
		}},
		Message{Role: "tool", Content: "tool-result-42", ToolCallID: "tc1"},
		Message{Role: "user", Content: "recent-1"},
		Message{Role: "assistant", Content: "recent-2"},
		Message{Role: "user", Content: "recent-3"},
	)

	changed, err := h.Compact(context.Background())
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !changed {
		t.Fatalf("Compact returned false, want true")
	}

	// The same invariant LoadSession enforces: every tool message has an
	// earlier assistant message that announced its tool_call_id.
	if err := validateSessionMessages(h.History); err != nil {
		t.Errorf("compacted History violates tool pairing: %v", err)
	}

	var assistantIdx, toolIdx = -1, -1
	for i, m := range h.History {
		if len(m.ToolCalls) > 0 {
			assistantIdx = i
		}
		if m.Role == "tool" {
			toolIdx = i
		}
	}
	if assistantIdx < 0 || toolIdx < 0 {
		t.Fatalf("tool-call group missing after compaction (assistant %d, tool %d): %+v",
			assistantIdx, toolIdx, h.History)
	}
	if assistantIdx >= toolIdx {
		t.Errorf("assistant tool_calls at %d is not before its tool result at %d", assistantIdx, toolIdx)
	}
	if h.History[assistantIdx].ToolCalls[0].ID != "tc1" ||
		h.History[toolIdx].ToolCallID != "tc1" ||
		ExtractText(h.History[toolIdx].Content) != "tool-result-42" {
		t.Errorf("tool-call group not kept verbatim: assistant %+v tool %+v",
			h.History[assistantIdx], h.History[toolIdx])
	}
}

// TestCompactDisabledByDefault: CompactThreshold 0 (the zero value) keeps
// askLoop byte-for-byte identical to a plain turn: even with LastUsage
// seeded far past any possible threshold, the turn is exactly one POST -
// no summary request is ever issued.
func TestCompactDisabledByDefault(t *testing.T) {
	srv := newFakeChatServer(t, okReply(chatTextReply("plain turn")))
	h := newLoopHarness(srv.URL(), Config{SystemPrompt: "SYS PROMPT", MaxContextTokens: 100}, NewRegistry())
	seedHistory(h, 12, "old")
	h.LastUsage = Usage{PromptTokens: 999999, TotalTokens: 999999}

	got, err := h.Ask(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "plain turn" {
		t.Errorf("Ask returned %q, want %q", got, "plain turn")
	}
	// Same request count as TestAskSimpleTextReply at the same sizes: 1.
	if n := len(srv.requests()); n != 1 {
		t.Errorf("server got %d requests, want exactly 1 (compaction disabled by default)", n)
	}
}

// TestCompactAutoTriggerFires: turn 1's response reports 90 of 100 prompt
// tokens (threshold 0.8), so the SECOND Ask starts by compacting: the
// summary POST goes out first, and the turn request that follows carries
// the "Summary of earlier conversation" system message.
func TestCompactAutoTriggerFires(t *testing.T) {
	srv := newFakeChatServer(t,
		okReply(chatTextReplyUsage("first turn", Usage{PromptTokens: 90, CompletionTokens: 5, TotalTokens: 95})),
		okReply(chatTextReply("auto compacted summary")),
		okReply(chatTextReply("second turn done")),
	)
	h := newLoopHarness(srv.URL(),
		Config{SystemPrompt: "SYS PROMPT", CompactThreshold: 0.8, MaxContextTokens: 100}, NewRegistry())
	seedHistory(h, 12, "old")

	if _, err := h.Ask(context.Background(), "first turn prompt"); err != nil {
		t.Fatalf("first Ask: %v", err)
	}
	// The trigger condition is now true: 90 >= 0.8*100.
	if h.LastUsage.PromptTokens < int(float64(100)*0.8) {
		t.Fatalf("fixture: LastUsage.PromptTokens = %d, want >= 80", h.LastUsage.PromptTokens)
	}

	got, err := h.Ask(context.Background(), "second turn prompt")
	if err != nil {
		t.Fatalf("second Ask: %v", err)
	}
	if got != "second turn done" {
		t.Errorf("second Ask returned %q, want %q", got, "second turn done")
	}

	reqs := srv.requests()
	if len(reqs) != 3 {
		t.Fatalf("server got %d requests, want 3 (turn 1 + summary + turn 2)", len(reqs))
	}
	// The second turn's first request is the compaction summary call.
	if !strings.Contains(string(reqs[1]), "Summarize the conversation above") {
		t.Errorf("summary request missing the compaction instruction: %s", reqs[1])
	}
	// The second turn's actual request (2nd body of that Ask) carries the
	// injected summary instead of the summarized old messages.
	if !strings.Contains(string(reqs[2]), "Summary of earlier conversation") {
		t.Errorf("turn-2 request does not carry the compaction summary: %s", reqs[2])
	}
	if strings.Contains(string(reqs[2]), "old-0") {
		t.Errorf("turn-2 request still carries the summarized old messages: %s", reqs[2])
	}

	if h.History[0].Role != "system" || ExtractText(h.History[0].Content) != "SYS PROMPT" {
		t.Errorf("History[0] = role %q content %q, want the original system prompt kept",
			h.History[0].Role, ExtractText(h.History[0].Content))
	}
	if !strings.HasPrefix(ExtractText(h.History[1].Content), "Summary of earlier conversation:") {
		t.Errorf("History[1] = %q, want the summary system message", ExtractText(h.History[1].Content))
	}
}

// TestCompactFailureKeepsHistory: a failing summary request must leave
// History untouched and return an error (Compact), and in the
// auto-trigger path the turn must still complete with only a stderr
// warning.
func TestCompactFailureKeepsHistory(t *testing.T) {
	// Direct Compact: the 500 surfaces as an error, History unchanged.
	srv := newFakeChatServer(t, statusReply(http.StatusInternalServerError, "boom", ""))
	h := newLoopHarness(srv.URL(), Config{SystemPrompt: "SYS PROMPT"}, NewRegistry())
	seedHistory(h, 12, "old")
	before := append([]Message(nil), h.History...)

	changed, err := h.Compact(context.Background())
	if err == nil {
		t.Fatalf("Compact returned %v, want the summary request error", changed)
	}
	if changed {
		t.Errorf("Compact reported changed=true after a failed summary")
	}
	if !reflect.DeepEqual(h.History, before) {
		t.Errorf("History changed after a failed compact:\nbefore %+v\nafter  %+v", before, h.History)
	}

	// Auto-trigger path: turn 1's usage crosses the threshold, the
	// summary POST 500s, and turn 2 must still finish with the
	// uncompacted history plus a warning on stderr.
	srv2 := newFakeChatServer(t,
		okReply(chatTextReplyUsage("first turn", Usage{PromptTokens: 90, TotalTokens: 90})),
		statusReply(http.StatusInternalServerError, "boom", ""),
		okReply(chatTextReply("turn proceeds anyway")),
	)
	h2 := newLoopHarness(srv2.URL(),
		Config{SystemPrompt: "SYS PROMPT", CompactThreshold: 0.8, MaxContextTokens: 100}, NewRegistry())
	seedHistory(h2, 12, "old")
	if _, err := h2.Ask(context.Background(), "first turn prompt"); err != nil {
		t.Fatalf("first Ask: %v", err)
	}

	origStderr := os.Stderr
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = pipeW
	got, askErr := h2.Ask(context.Background(), "second turn prompt")
	pipeW.Close()
	os.Stderr = origStderr
	warning, _ := io.ReadAll(pipeR)

	if askErr != nil {
		t.Fatalf("second Ask: %v (want the turn to proceed despite the failed compaction)", askErr)
	}
	if got != "turn proceeds anyway" {
		t.Errorf("second Ask returned %q, want %q", got, "turn proceeds anyway")
	}
	if !strings.Contains(string(warning), "warning: context compaction failed:") {
		t.Errorf("stderr = %q, want it to contain the compaction warning", warning)
	}
	if n := len(srv2.requests()); n != 3 {
		t.Errorf("server got %d requests, want 3 (turn 1 + failed summary + turn 2)", n)
	}
	// History survived the failed compaction with the old messages intact.
	if !strings.Contains(string(srv2.requests()[2]), "old-0") {
		t.Errorf("turn-2 request lost the old messages after the failed compaction: %s", srv2.requests()[2])
	}
}

// TestCompactTooSmallNoop: a history too small to yield anything before
// the keep window returns (false, nil) without touching the server (zero
// scripted responses: any POST at all would fail the test).
func TestCompactTooSmallNoop(t *testing.T) {
	srv := newFakeChatServer(t)
	h := newLoopHarness(srv.URL(), Config{SystemPrompt: "SYS PROMPT"}, NewRegistry())
	h.History = append(h.History,
		Message{Role: "user", Content: "only exchange"},
		Message{Role: "assistant", Content: "only reply"},
	)

	changed, err := h.Compact(context.Background())
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if changed {
		t.Errorf("Compact reported changed=true for a history too small to summarize")
	}
	if len(h.History) != 3 {
		t.Errorf("History is %d messages, want 3 untouched", len(h.History))
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("server got %d requests, want 0 (no summary call for a no-op compaction)", n)
	}
}
