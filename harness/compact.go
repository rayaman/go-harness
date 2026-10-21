package harness

import (
	"context"
	"fmt"
)

// defaultCompactKeepTurns is the fallback when Config.CompactKeepTurns is
// zero or negative: the number of most-recent History MESSAGES (not turn
// pairs) that Compact keeps verbatim.
const defaultCompactKeepTurns = 8

// compactSummaryInstruction is the user message appended after the older
// History slice on the compaction summary request (roadmap 1.1).
const compactSummaryInstruction = "Summarize the conversation above compactly. Preserve: decisions made, file paths and names touched, tool results that matter, unresolved questions. Write in third person."

// Compact summarizes the older part of History into a single system
// message and reports whether History actually changed (roadmap 1.1).
//
// Layout after a successful compaction:
//
//	[original leading system message, if any] +
//	[system message "Summary of earlier conversation:\n" + summary] +
//	[the last Config.CompactKeepTurns messages, verbatim]
//
// It returns (false, nil) - without touching History or the server - when
// there is nothing to summarize: everything before the keep window is just
// the seeded system message. Compact itself ignores CompactThreshold; that
// field only gates the automatic trigger in askLoop, so the manual
// /compact path always attempts compaction.
//
// The split boundary never cuts an assistant tool_calls message away from
// its tool results: askLoop always appends tool results right after their
// announcement, so if the boundary lands on a "tool" message it walks back
// to the announcing assistant message, keeping the whole group in the
// verbatim window (the pairing invariant session.go's
// validateSessionMessages enforces).
//
// A failed summary request returns (false, err) and leaves History
// exactly as it was: compaction must never corrupt the conversation. The
// summary call's Usage is intentionally NOT recorded in LastUsage - the
// regular request right after (or the warning path) overwrites it anyway.
func (h *Harness) Compact(ctx context.Context) (bool, error) {
	keep := h.Config.CompactKeepTurns
	if keep <= 0 {
		keep = defaultCompactKeepTurns
	}

	// A leading system message (seeded by New/Reset) always stays
	// verbatim at index 0 and is never part of what gets summarized.
	offset := 0
	if len(h.History) > 0 && h.History[0].Role == "system" {
		offset = 1
	}

	// Nothing before the keep window besides the system prompt.
	if len(h.History)-keep <= offset {
		return false, nil
	}

	split := len(h.History) - keep
	// The keep window must not begin on a tool result: that would orphan
	// it from the assistant message that announced its tool_call_id. Walk
	// the boundary back over the contiguous tool messages to the
	// announcing assistant.
	for split > offset && h.History[split].Role == "tool" {
		split--
	}
	if split <= offset {
		return false, nil
	}

	// Copy the older slice before appending the instruction: it aliases
	// History's backing array, and appending in place could clobber the
	// kept messages.
	reqMsgs := append([]Message(nil), h.History[offset:split]...)
	reqMsgs = append(reqMsgs, Message{Role: "user", Content: compactSummaryInstruction})

	// Same provider dispatch as askLoop's non-streaming path (the summary
	// call never streams), Tools: nil and Temperature: 0 so the model can
	// only answer with a plain deterministic summary.
	resp, err := h.chatCompletionNonStream(ctx, ChatRequest{
		Model:       h.Config.Model,
		Messages:    reqMsgs,
		Tools:       nil,
		Temperature: 0,
		Stream:      false,
	})
	if err != nil {
		return false, fmt.Errorf("compaction summary request: %w", err)
	}
	if len(resp.Choices) == 0 {
		return false, fmt.Errorf("compaction summary request: model server returned no choices")
	}

	summary := ExtractText(resp.Choices[0].Message.Content)

	newHistory := make([]Message, 0, len(h.History)-(split-offset)+2)
	if offset == 1 {
		newHistory = append(newHistory, h.History[0])
	}
	newHistory = append(newHistory, Message{
		Role:    "system",
		Content: "Summary of earlier conversation:\n" + summary,
	})
	newHistory = append(newHistory, h.History[split:]...)
	h.History = newHistory
	return true, nil
}

// chatCompletionNonStream is the buffered provider dispatch shared by the
// compaction summary call: provider "ollama" goes through the native
// /api/chat (passing MaxContextTokens as num_ctx), everything else
// through the OpenAI-compatible endpoint - exactly the non-streaming
// branch of askLoop, factored out without restructuring askLoop itself.
func (h *Harness) chatCompletionNonStream(ctx context.Context, chatReq ChatRequest) (*ChatResponse, error) {
	if h.Config.Provider == "ollama" {
		resp, err := h.Client.OllamaChat(ctx, chatReq, h.Config.MaxContextTokens)
		if err != nil {
			return nil, fmt.Errorf("ollama chat: %w", err)
		}
		return resp, nil
	}
	resp, err := h.Client.ChatCompletion(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("chat completion: %w", err)
	}
	return resp, nil
}
