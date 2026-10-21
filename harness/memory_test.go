package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Persistent memory tests (roadmap 2.4)
//
// The fixtures from harness_test.go are reused: newLoopHarness wires a
// Harness through New (which now captures systemBase and calls
// applyMemory), and "http://127.0.0.1:0" is the throwaway server URL for
// tests that never hit the network.
// ---------------------------------------------------------------------------

// newMemoryHarness builds a Harness with a Memory store over a fresh
// file in dir, plus the given system prompt. No requests are made.
func newMemoryHarness(t *testing.T, systemPrompt string, maxBytes int) (*Harness, *Memory, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.md")
	mem := &Memory{Path: path, MaxBytes: maxBytes}
	h := newLoopHarness("http://127.0.0.1:0", Config{
		SystemPrompt: systemPrompt,
		Memory:       mem,
	}, NewRegistry())
	return h, mem, path
}

// remember calls RememberTool.Execute with the given note.
func remember(t *testing.T, h *Harness, note string) string {
	t.Helper()
	args, err := json.Marshal(map[string]string{"note": note})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	out, err := RememberTool{H: h}.Execute(json.RawMessage(args))
	if err != nil {
		t.Fatalf("remember(%q): %v", note, err)
	}
	return out
}

// TestRememberAppendsAndInjects: remember writes "- <note>" to the file
// and re-injects the block into History[0]; a second (different) note
// shows up in both the file and the block.
func TestRememberAppendsAndInjects(t *testing.T) {
	h, _, path := newMemoryHarness(t, "you are a test persona", 0)

	if out := remember(t, h, "the deploy target is prod-eu"); out != "recorded: the deploy target is prod-eu" {
		t.Errorf("first remember returned %q, want the recorded: message", out)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read memory file: %v", err)
	}
	if !strings.Contains(string(data), "- the deploy target is prod-eu\n") {
		t.Errorf("memory file = %q, want it to contain the prefixed entry line", data)
	}

	sys := ExtractText(h.History[0].Content)
	if !strings.Contains(sys, "=== PERSISTENT MEMORY") ||
		!strings.Contains(sys, "- the deploy target is prod-eu") {
		t.Errorf("History[0] = %q, want the memory block with the entry", sys)
	}
	if h.History[0].Role != "system" {
		t.Fatalf("History[0].Role = %q, want system", h.History[0].Role)
	}
	if len(h.History) != 1 {
		t.Fatalf("History has %d messages, want just the leading system message", len(h.History))
	}

	if out := remember(t, h, "user prefers terse answers"); out != "recorded: user prefers terse answers" {
		t.Errorf("second remember returned %q, want the recorded: message", out)
	}

	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read memory file: %v", err)
	}
	fileStr := string(data)
	for _, want := range []string{"- the deploy target is prod-eu", "- user prefers terse answers"} {
		if !strings.Contains(fileStr, want) {
			t.Errorf("memory file = %q, want it to contain %q", fileStr, want)
		}
	}

	sys = ExtractText(h.History[0].Content)
	for _, want := range []string{"- the deploy target is prod-eu", "- user prefers terse answers"} {
		if !strings.Contains(sys, want) {
			t.Errorf("History[0] = %q, want the injected block to contain %q", sys, want)
		}
	}
	// Newest-last ordering inside the block.
	if strings.Index(sys, "prod-eu") > strings.Index(sys, "terse answers") {
		t.Errorf("block is not newest-last:\n%s", sys)
	}
	// Exactly one header line: re-injection replaced, never stacked.
	if n := strings.Count(sys, "=== PERSISTENT MEMORY ("); n != 1 {
		t.Errorf("History[0] has %d memory headers, want exactly 1:\n%s", n, sys)
	}
}

// TestRememberDeduplicates: the same note twice -> second call answers
// "already recorded" and the file keeps a single line for it.
func TestRememberDeduplicates(t *testing.T) {
	h, _, path := newMemoryHarness(t, "persona", 0)

	remember(t, h, "one source of truth per fact")
	out := remember(t, h, "one source of truth per fact")
	if out != "already recorded" {
		t.Errorf("duplicate remember returned %q, want %q", out, "already recorded")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read memory file: %v", err)
	}
	if n := strings.Count(string(data), "one source of truth per fact"); n != 1 {
		t.Errorf("memory file has %d copies of the note, want 1:\n%s", n, data)
	}
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 1 {
		t.Errorf("memory file has %d lines, want 1:\n%s", len(lines), data)
	}
}

// TestMemoryInjectedAtStartup: a pre-populated memory file is injected
// into the seeded system message by New itself - no remember call needed.
func TestMemoryInjectedAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.md")
	pre := "- remember the wifi password\n- meetings are wednesdays\n"
	if err := os.WriteFile(path, []byte(pre), 0o644); err != nil {
		t.Fatalf("write memory file: %v", err)
	}

	h := newLoopHarness("http://127.0.0.1:0", Config{
		SystemPrompt: "you are a test persona",
		Memory:       &Memory{Path: path},
	}, NewRegistry())

	if len(h.History) != 1 || h.History[0].Role != "system" {
		t.Fatalf("History = %+v, want a single leading system message", h.History)
	}
	sys := ExtractText(h.History[0].Content)
	if !strings.HasPrefix(sys, "you are a test persona\n\n=== PERSISTENT MEMORY (") {
		t.Errorf("History[0] does not start with the base prompt + block header:\n%s", sys)
	}
	if !strings.Contains(sys, "- remember the wifi password") ||
		!strings.Contains(sys, "- meetings are wednesdays") {
		t.Errorf("History[0] missing pre-populated entries:\n%s", sys)
	}
	if !strings.Contains(sys, "=== END PERSISTENT MEMORY ===") {
		t.Errorf("History[0] missing the block footer:\n%s", sys)
	}
	// The base prompt itself must precede the block untouched.
	if h.systemBase != "you are a test persona" {
		t.Errorf("systemBase = %q, want the raw prompt", h.systemBase)
	}
}

// TestResetKeepsMemory: /reset re-seeds History but the memory block is
// re-injected from the (unchanged) store, and systemBase survives.
func TestResetKeepsMemory(t *testing.T) {
	h, _, _ := newMemoryHarness(t, "you are a test persona", 0)
	remember(t, h, "the release train leaves mondays")

	// Fake some chat turns (no server round trip needed to reset).
	h.History = append(h.History,
		Message{Role: "user", Content: "hi"},
		Message{Role: "assistant", Content: "hello"},
	)

	h.Reset()

	if len(h.History) != 1 || h.History[0].Role != "system" {
		t.Fatalf("History after Reset = %+v, want a single leading system message", h.History)
	}
	sys := ExtractText(h.History[0].Content)
	if !strings.Contains(sys, "=== PERSISTENT MEMORY") ||
		!strings.Contains(sys, "- the release train leaves mondays") {
		t.Errorf("History[0] after Reset lost the memory block:\n%s", sys)
	}
	if !strings.HasPrefix(sys, "you are a test persona") {
		t.Errorf("History[0] after Reset lost the base prompt:\n%s", sys)
	}
	if h.systemBase != "you are a test persona" {
		t.Errorf("systemBase after Reset = %q, want the raw prompt preserved", h.systemBase)
	}
}

// TestMemoryCapDropsOldest: with MaxBytes set, the rendered block keeps
// the NEWEST entries, admits the drop with the marker line, loses the
// very first entry, and stays within the cap plus header slack.
func TestMemoryCapDropsOldest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.md")

	var b strings.Builder
	for i := 1; i <= 40; i++ {
		// 40-byte entry lines ("- entry 01 " = 11 chars + 29 filler).
		fmt.Fprintf(&b, "- entry %02d %s\n", i, strings.Repeat("x", 29))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write memory file: %v", err)
	}

	block, err := (&Memory{Path: path, MaxBytes: 300}).Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	first := fmt.Sprintf("- entry %02d ", 1) + strings.Repeat("x", 29)
	last := fmt.Sprintf("- entry %02d ", 40) + strings.Repeat("x", 29)
	if !strings.Contains(block, last) {
		t.Errorf("block missing the newest entry:\n%s", block)
	}
	if strings.Contains(block, first) {
		t.Errorf("block still contains the very first entry:\n%s", block)
	}
	if !strings.Contains(block, "[older memory entries omitted]") {
		t.Errorf("block missing the omitted-entries marker:\n%s", block)
	}
	// Marker goes right after the header line.
	headerEnd := strings.Index(block, "\n")
	if headerEnd < 0 || !strings.HasPrefix(block[headerEnd+1:], "[older memory entries omitted]") {
		t.Errorf("marker is not directly after the header line:\n%s", block)
	}
	// Newest-last order is preserved among the kept entries.
	if strings.Index(block, "- entry 39") > strings.Index(block, "- entry 40") {
		t.Errorf("kept entries are not newest-last:\n%s", block)
	}
	// Over the cap only by the fixed marker/header slack.
	if len(block) > 360 {
		t.Errorf("block length %d exceeds cap+slack (300+60):\n%s", len(block), block)
	}
}

// TestNewlineNoteSanitized: an embedded newline is flattened to spaces
// so one note never spans two entry lines.
func TestNewlineNoteSanitized(t *testing.T) {
	h, _, path := newMemoryHarness(t, "persona", 0)

	remember(t, h, "first part\nsecond part")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read memory file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("memory file has %d lines, want 1:\n%s", len(lines), data)
	}
	if lines[0] != "- first part second part" {
		t.Errorf("stored line = %q, want %q", lines[0], "- first part second part")
	}
}

// TestLoadSessionRefreshesMemory: a session saved while memory had
// entry A is loaded after entry B was appended to the file - History[0]
// must carry the CURRENT block (A and B, one header) rather than the
// stale block embedded in the saved History.
func TestLoadSessionRefreshesMemory(t *testing.T) {
	dir := t.TempDir()
	memPath := filepath.Join(dir, "memory.md")

	h1 := newLoopHarness("http://127.0.0.1:0", Config{
		SystemPrompt: "you are a test persona",
		Memory:       &Memory{Path: memPath},
	}, NewRegistry())
	remember(t, h1, "entry alpha")
	// History[0] now carries the injected block; the session file must
	// store the RAW base prompt instead (SaveSession contract).
	sessPath := filepath.Join(dir, "session.json")
	if err := h1.SaveSession(sessPath); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	// Entry B lands in the memory file between save and load.
	if _, err := (&Memory{Path: memPath}).Append("entry bravo"); err != nil {
		t.Fatalf("append entry B: %v", err)
	}

	h2 := newLoopHarness("http://127.0.0.1:0", Config{
		Memory: &Memory{Path: memPath},
	}, NewRegistry())
	if err := h2.LoadSession(sessPath); err != nil {
		t.Fatalf("LoadSession: %v", err)
	}

	if h2.History[0].Role != "system" {
		t.Fatalf("loaded History[0].Role = %q, want system", h2.History[0].Role)
	}
	sys := ExtractText(h2.History[0].Content)
	if !strings.Contains(sys, "- entry bravo") {
		t.Errorf("loaded History[0] missing the newly appended entry B:\n%s", sys)
	}
	if !strings.Contains(sys, "- entry alpha") {
		t.Errorf("loaded History[0] missing entry A from the current file:\n%s", sys)
	}
	if n := strings.Count(sys, "=== PERSISTENT MEMORY ("); n != 1 {
		t.Errorf("loaded History[0] has %d memory headers, want exactly 1 (no stale duplicate):\n%s", n, sys)
	}
	if h2.systemBase != "you are a test persona" {
		t.Errorf("systemBase = %q, want the restored raw prompt", h2.systemBase)
	}
	// h1's History was only the system message, so the loaded History
	// is exactly one message, with the stale injected block replaced
	// wholesale by the current one.
	if len(h2.History) != 1 {
		t.Fatalf("loaded History has %d messages, want 1: %+v", len(h2.History), h2.History)
	}
	// SaveSession deliberately records the RAW base prompt in
	// system_prompt (the messages snapshot may still carry the stale
	// injected block - LoadSession replaces it wholesale).
	sessData, err := os.ReadFile(sessPath)
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	var saved Session
	if err := json.Unmarshal(sessData, &saved); err != nil {
		t.Fatalf("unmarshal session file: %v", err)
	}
	if saved.SystemPrompt != "you are a test persona" {
		t.Errorf("session system_prompt = %q, want the raw base prompt", saved.SystemPrompt)
	}
	if strings.Contains(ExtractText(saved.Messages[0].Content), "=== PERSISTENT MEMORY") {
		// Not an error by contract, but documents what LoadSession must
		// strip; assert the refresh really happened instead.
		t.Log("saved messages carry the stale block (expected); load must replace it")
	}
}

// TestMemoryDisabledByDefault: Memory nil -> New/Reset leave History[0]
// byte-for-byte at the raw system prompt, and remember reports the
// store is disabled.
func TestMemoryDisabledByDefault(t *testing.T) {
	const prompt = "you are a test persona"
	h := newLoopHarness("http://127.0.0.1:0", Config{SystemPrompt: prompt}, NewRegistry())

	if h.Memory != nil {
		t.Fatalf("h.Memory = %+v, want nil for a zero-value Config", h.Memory)
	}
	if len(h.History) != 1 || ExtractText(h.History[0].Content) != prompt {
		t.Fatalf("History[0] = %+v, want exactly the raw prompt", h.History)
	}
	if h.systemBase != prompt {
		t.Errorf("systemBase = %q, want the raw prompt", h.systemBase)
	}

	h.Reset()
	if len(h.History) != 1 || ExtractText(h.History[0].Content) != prompt {
		t.Errorf("History[0] after Reset = %+v, want exactly the raw prompt", h.History)
	}

	args := json.RawMessage(`{"note":"anything"}`)
	if _, err := (RememberTool{H: h}).Execute(args); err == nil || err.Error() != "memory is disabled" {
		t.Errorf("remember with nil Memory: err = %v, want the memory-is-disabled error", err)
	}
}
