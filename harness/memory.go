package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// Persistent memory (roadmap 2.4)
//
// A Memory is a plain-text file of one-entry-per-line notes ("- note") that
// survives across sessions. At startup (and after every remember call) the
// entries are rendered into a fenced block and appended to the Harness's
// LEADING system message, so the model sees them without the harness adding
// a second system message. That is the critical invariant: 1.1's compaction
// keeps History[0] verbatim and 1.3's sessions round-trip the system
// prompt, so memory injection must only ever rewrite History[0].Content
// (never insert messages, never touch History[1:]).
//
// Design deviation from the roadmap touch point: the roadmap listed
// harness/tools/memory.go, but the remember tool must mutate the Harness's
// system message, so both the store and the tool live in the harness
// package itself - the same precedent as the web tools avoiding an import
// of the core package in reverse (harness/tools must never import harness).
// ---------------------------------------------------------------------------

// defaultMemoryMaxBytes is the fallback cap on the rendered memory block
// when Memory.MaxBytes is zero or negative.
const defaultMemoryMaxBytes = 4096

// memoryOmittedMarker is prefixed into the block (right after the header
// line) when the MaxBytes cap forced the oldest entries to be dropped.
const memoryOmittedMarker = "[older memory entries omitted]"

// memoryFooter closes the injected block. The header is built per Memory
// because it embeds the file path: "=== PERSISTENT MEMORY (<path>) ===".
const memoryFooter = "=== END PERSISTENT MEMORY ==="

// Memory is a cross-session note store (roadmap 2.4). A nil *Memory on a
// Harness (the zero value) disables memory entirely, so embedders and
// workflows that never opt in are byte-for-byte unchanged.
type Memory struct {
	// Path is the memory file: plain text, one entry per line, each
	// starting with "- ". A missing file is an empty store, not an error.
	Path string

	// MaxBytes caps the size of the rendered injection block. Zero or
	// negative falls back to defaultMemoryMaxBytes. When the block would
	// exceed the cap, the OLDEST entries are dropped (with a marker line
	// noting it) until it fits.
	MaxBytes int
}

// Load reads the memory file and returns its entry lines (each kept
// verbatim, "- " prefix included). A missing file yields an empty slice
// and no error; blank lines are skipped.
func (m *Memory) Load() ([]string, error) {
	data, err := os.ReadFile(m.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read memory file %q: %w", m.Path, err)
	}

	var entries []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entries = append(entries, line)
	}
	return entries, nil
}

// Append adds one note to the store and reports whether it was written.
// The note is trimmed; an empty note is an error. Embedded newlines are
// flattened to spaces so one entry always stays on one line. The "- "
// prefix is added when missing, and an exact-match duplicate of an
// existing entry line is silently dropped (false, nil - no write). The
// write itself is atomic: the full file is rewritten through
// path+".tmp" and renamed over the target, with parent directories
// created as needed.
func (m *Memory) Append(note string) (bool, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return false, fmt.Errorf("memory note is empty")
	}
	note = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(note)
	note = strings.TrimSpace(note)
	if note == "" {
		return false, fmt.Errorf("memory note is empty")
	}
	if !strings.HasPrefix(note, "- ") {
		note = "- " + note
	}

	entries, err := m.Load()
	if err != nil {
		return false, err
	}
	for _, existing := range entries {
		if existing == note {
			return false, nil // dedupe: exact-match line already stored
		}
	}

	// Rebuild the whole file from its current raw bytes plus the new
	// line, so existing content (including any blank lines) is left
	// untouched and the entry lands at the end (newest-last).
	var content string
	if data, err := os.ReadFile(m.Path); err != nil {
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("read memory file %q: %w", m.Path, err)
		}
	} else {
		content = string(data)
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += note + "\n"

	if dir := filepath.Dir(m.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("create memory directory %q: %w", dir, err)
		}
	}
	tmp := m.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, fmt.Errorf("write memory temp file %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, m.Path); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("replace memory file %q: %w", m.Path, err)
	}
	return true, nil
}

// Render builds the block injected into the system message from the
// store's current entries, newest-last (file order). Format:
//
//	=== PERSISTENT MEMORY (<path>) ===
//	- entry one
//	- entry two
//	=== END PERSISTENT MEMORY ===
//
// With no entries it returns "" (nothing to inject). If the block would
// exceed MaxBytes (0 -> defaultMemoryMaxBytes), the OLDEST entries are
// dropped one at a time until it fits, and the marker line
// memoryOmittedMarker is inserted right after the header to admit it.
func (m *Memory) Render() (string, error) {
	entries, err := m.Load()
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", nil
	}

	limit := m.MaxBytes
	if limit <= 0 {
		limit = defaultMemoryMaxBytes
	}
	header := "=== PERSISTENT MEMORY (" + m.Path + ") ==="

	omitted := false
	for {
		var b strings.Builder
		b.WriteString(header)
		if omitted {
			b.WriteString("\n" + memoryOmittedMarker)
		}
		for _, e := range entries {
			b.WriteString("\n" + e)
		}
		b.WriteString("\n" + memoryFooter)
		block := b.String()

		if len(block) <= limit || len(entries) == 1 {
			// Fits, or only the newest entry is left: even that plus
			// the scaffolding cannot be shrunk further, so return it
			// whole rather than silently losing all memory.
			return block, nil
		}
		entries = entries[1:] // drop the oldest
		omitted = true
	}
}

// applyMemory rewrites the leading system message so it carries the
// current memory block on top of systemBase (the raw prompt WITHOUT any
// previously injected block). It is the single place that mutates
// History[0].Content for memory; History[1:] is never touched, and no
// second system message is ever inserted.
//
// With h.Memory == nil it is a no-op. A memory read failure is warning-
// only: the block becomes empty, History[0] is left as systemBase, and
// the turn proceeds - a broken memory file must never fail a conversation.
// The error return is always nil today; it exists so callers can stay
// forward-compatible if a future failure mode becomes fatal.
func (h *Harness) applyMemory() error {
	if h.Memory == nil {
		return nil
	}

	block, err := h.Memory.Render()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: persistent memory unavailable: %v\n", err)
		block = ""
	}

	content := h.systemBase
	if block != "" {
		if content != "" {
			content += "\n\n" + block
		} else {
			content = block
		}
	}

	if len(h.History) == 0 || h.History[0].Role != "system" {
		// Nothing leading to rewrite. Only prepend when there is
		// something to say: an all-empty content must not invent an
		// empty system message (New deliberately seeds none for an
		// empty SystemPrompt).
		if content == "" {
			return nil
		}
		h.History = append([]Message{{Role: "system", Content: content}}, h.History...)
		return nil
	}
	h.History[0].Content = content
	return nil
}

// ---------------------------------------------------------------------------
// remember tool
// ---------------------------------------------------------------------------

// RememberTool lets the model persist a note across sessions (roadmap
// 2.4). It lives in the harness package because it mutates the Harness's
// own system message; harness/tools must never import harness, so the
// roadmap's original home (harness/tools/memory.go) is not viable.
type RememberTool struct {
	H *Harness
}

// Name implements Tool.
func (RememberTool) Name() string { return "remember" }

// Description implements Tool.
func (RememberTool) Description() string {
	return "Record a short fact, decision, or preference to persistent memory that survives across sessions. One concise note per call."
}

// Parameters implements Tool.
func (RememberTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"note": map[string]any{
				"type":        "string",
				"description": "A single concise fact, decision, or preference to remember. Newlines are flattened to spaces; duplicate notes are ignored.",
			},
		},
		"required": []string{"note"},
	}
}

// Execute implements Tool: append the note to the store and re-inject the
// memory block so the change is visible to the model on the very next
// request of this session, not just the next session.
func (t RememberTool) Execute(args json.RawMessage) (string, error) {
	var in struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("remember: bad arguments: %w", err)
	}
	if t.H == nil || t.H.Memory == nil {
		return "", fmt.Errorf("memory is disabled")
	}

	appended, err := t.H.Memory.Append(in.Note)
	if err != nil {
		return "", fmt.Errorf("remember: %w", err)
	}
	if !appended {
		return "already recorded", nil
	}
	_ = t.H.applyMemory() // re-inject; read failures are warning-only
	return "recorded: " + strings.TrimSpace(in.Note), nil
}

// Serial implements SerialTyper: the tool writes the memory file, so it
// must not run concurrently with other side-effecting tools (roadmap 2.1).
func (RememberTool) Serial() bool { return true }
