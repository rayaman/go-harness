package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Session is the on-disk snapshot of a conversation (roadmap 1.3): the
// message History plus the config bits worth restoring (model, system
// prompt). Messages is exactly Harness.History, so the file carries the
// same wire format the provider already sees.
//
// Multimodal round-trip note: Message.Content is `any`. A []ContentPart
// (the OpenAI-compat image shape) marshals fine, but json.Unmarshal reads
// it back as []any / map[string]any. That is intentional and safe:
// ExtractText already handles those shapes and the agent loop re-marshals
// them into the identical wire format, so a saved image session
// round-trips into the generic []any shape and still works on the request
// path. Do not try to force it back into []ContentPart.
type Session struct {
	Model        string    `json:"model"`
	SystemPrompt string    `json:"system_prompt,omitempty"`
	Messages     []Message `json:"messages"`
}

// SaveSession marshals the current History together with Config.Model and
// Config.SystemPrompt and writes the result to path; parent directories
// are created as needed. The write is atomic: the JSON goes to
// path+".tmp" first and is renamed over the target, so an interrupted
// save can never corrupt an existing session file (a failed rename also
// removes the temp file).
//
// With persistent memory enabled (roadmap 2.4), the recorded
// SystemPrompt is deliberately Config.SystemPrompt - the RAW base prompt,
// never the memory-injected History[0] content: injected blocks are
// rendered from the CURRENT memory file at load time, so a saved session
// can never freeze a stale memory block into the JSON. The Messages
// snapshot may still carry an injected block in History[0]; LoadSession
// replaces that content wholesale, so it never compounds.
func (h *Harness) SaveSession(path string) error {
	sess := Session{
		Model:        h.Config.Model,
		SystemPrompt: h.Config.SystemPrompt,
		Messages:     h.History,
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create session directory %q: %w", dir, err)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write session temp file %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace session file %q: %w", path, err)
	}
	return nil
}

// LoadSession reads a session file from path and replaces History with
// its messages (a fresh copy). When the file recorded a non-empty
// SystemPrompt or Model, Config is updated too, so /load restores the
// persona and model that were in effect at save time - and a later
// /reset re-seeds the restored system prompt.
//
// With persistent memory enabled (roadmap 2.4), the effective system
// base (the restored Config.SystemPrompt, or the loaded History[0]
// content when the file recorded no prompt) is captured into systemBase
// and applyMemory re-injects the CURRENT memory file's block: a block
// embedded in the saved History[0] is replaced wholesale rather than
// stacked, so a loaded session never shows a stale or duplicated memory
// snapshot.
//
// Sessions that violate the tool-pairing invariant - a "tool" message
// with no earlier assistant message carrying a matching tool_call_id -
// are rejected with a descriptive error rather than loaded, because the
// provider refuses such a conversation. History and Config are left
// untouched on any error.
func (h *Harness) LoadSession(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read session file %q: %w", path, err)
	}

	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return fmt.Errorf("parse session file %q: %w", path, err)
	}
	if err := validateSessionMessages(sess.Messages); err != nil {
		return fmt.Errorf("session file %q: %w", path, err)
	}

	h.History = append([]Message(nil), sess.Messages...)
	if sess.SystemPrompt != "" {
		h.Config.SystemPrompt = sess.SystemPrompt
	}
	if sess.Model != "" {
		h.Config.Model = sess.Model
	}

	// Capture the memory-free system base for applyMemory. Prefer the
	// recorded prompt; fall back to the loaded leading system message,
	// stripping any block a memory-enabled save had embedded (the
	// footer line ends every injected block).
	h.systemBase = h.Config.SystemPrompt
	if h.systemBase == "" && len(h.History) > 0 && h.History[0].Role == "system" {
		h.systemBase = stripMemoryBlock(ExtractText(h.History[0].Content))
	}
	// Re-inject the current memory file's block over the base (no-op
	// when Memory is nil, which also keeps 1.3's behavior exact).
	return h.applyMemory()
}

// stripMemoryBlock removes a trailing injected persistent-memory block
// from a system message's content, returning the raw base prompt. It
// looks for the memory header line at a line start and cuts there,
// dropping the separator blank line before it. Content without a block
// is returned untouched.
func stripMemoryBlock(content string) string {
	idx := strings.Index(content, "\n=== PERSISTENT MEMORY (")
	if idx < 0 {
		if strings.HasPrefix(content, "=== PERSISTENT MEMORY (") {
			return ""
		}
		return content
	}
	// Also drop the "\n\n" separator applyMemory inserted before the
	// header; if the base itself ended with a lone newline, keep exactly
	// what preceded the separator.
	cut := idx
	for cut > 0 && content[cut-1] == '\n' {
		cut--
	}
	return content[:cut]
}

// validateSessionMessages checks the invariants that make a message list
// acceptable to a provider: it must not be empty, and every "tool"
// message's tool_call_id must have been announced by an earlier assistant
// message's tool_calls (matching anywhere earlier, not necessarily the
// immediately preceding message).
func validateSessionMessages(msgs []Message) error {
	if len(msgs) == 0 {
		return fmt.Errorf("session contains no messages")
	}

	announced := make(map[string]bool)
	for i, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				announced[tc.ID] = true
			}
		}
		if m.Role == "tool" && !announced[m.ToolCallID] {
			return fmt.Errorf(
				"message %d is a tool result with tool_call_id %q but no earlier assistant message made that tool call",
				i, m.ToolCallID)
		}
	}
	return nil
}
