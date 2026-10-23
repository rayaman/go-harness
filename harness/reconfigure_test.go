package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Reconfigure tests (web settings hand-off, web/config.go)
//
// Reconfigure swaps Config + Client on a live Harness without dropping the
// conversation. These tests pin the three invariants the web UI relies on:
// History survives, the new model/endpoint is visible on h.Config (and the
// Client pointer is actually rebound), and memory re-injection rebuilds
// History[0] over the new systemBase instead of stacking blocks - the same
// invariant Reset/LoadSession maintain per harness/memory.go.
// ---------------------------------------------------------------------------

// TestReconfigureSwapsConfigKeepsHistory: model/baseURL changes are
// visible afterwards, History (including mid-conversation messages) and
// the Confirm/OnDelta hooks survive, and the Client is rebound to the new
// one.
func TestReconfigureSwapsConfigKeepsHistory(t *testing.T) {
	oldClient := NewClient("http://127.0.0.1:1/v1", "")
	h := newLoopHarness("http://127.0.0.1:1/v1", Config{
		SystemPrompt: "old persona",
	}, NewRegistry())
	h.Client = oldClient

	// Seed a fake conversation: system + user + assistant.
	h.History = append(h.History,
		Message{Role: "user", Content: "keep me"},
		Message{Role: "assistant", Content: "and me"},
	)
	confirmCalled := false
	h.Confirm = func(call ToolCall) bool { confirmCalled = true; return true }
	h.OnDelta = func(kind, text string) {}

	newClient := NewClient("http://127.0.0.1:2/v1", "newkey")
	newCfg := h.Config
	newCfg.Model = "swapped-model"
	newCfg.BaseURL = "http://127.0.0.1:2/v1"
	newCfg.SystemPrompt = "new persona"
	newCfg.MaxRetries = 5

	h.Reconfigure(newCfg, newClient)

	if h.Config.Model != "swapped-model" {
		t.Errorf("Config.Model = %q, want %q", h.Config.Model, "swapped-model")
	}
	if h.Config.BaseURL != "http://127.0.0.1:2/v1" {
		t.Errorf("Config.BaseURL = %q, want the new endpoint", h.Config.BaseURL)
	}
	if h.Client != newClient {
		t.Error("Client was not rebound to the client passed to Reconfigure")
	}

	// History survives: original length (1 system + 2 seeded) and the
	// seeded messages come first; only History[0].Content is rewritten.
	if len(h.History) != 3 {
		t.Fatalf("History has %d messages after Reconfigure, want 3: %+v", len(h.History), h.History)
	}
	if h.History[0].Role != "system" {
		t.Errorf("History[0].Role = %q, want system", h.History[0].Role)
	}
	// With no Memory store on either Config, applyMemory is a no-op, so
	// History[0] keeps its original content: Reconfigure touches History
	// ONLY through applyMemory (see TestReconfigureMemoryReInjectionNoStack
	// for the store-attached rebuild path).
	if ExtractText(h.History[0].Content) != "old persona" {
		t.Errorf("History[0].Content = %q, want the original prompt (memory disabled means applyMemory is a no-op)", ExtractText(h.History[0].Content))
	}
	if h.systemBase != "new persona" {
		t.Errorf("systemBase = %q, want the new prompt captured for future re-injection", h.systemBase)
	}
	if h.History[1].Role != "user" || ExtractText(h.History[1].Content) != "keep me" {
		t.Errorf("History[1] = %+v, want the seeded user message untouched", h.History[1])
	}
	if h.History[2].Role != "assistant" || ExtractText(h.History[2].Content) != "and me" {
		t.Errorf("History[2] = %+v, want the seeded assistant message untouched", h.History[2])
	}

	// Hooks stay attached (plain mutable state, like New-era assignment).
	if h.Confirm == nil || h.OnDelta == nil {
		t.Error("Confirm/OnDelta hooks were dropped by Reconfigure")
	}
	if !h.Confirm(ToolCall{}) || !confirmCalled {
		t.Error("Confirm hook no longer runs the assigned closure")
	}
}

// TestReconfigureNilClientKeepsExisting: passing a nil Client keeps the
// current one and still applies cfg.MaxRetries to it, so a settings-only
// change can hand off without rebuilding the HTTP client.
func TestReconfigureNilClientKeepsExisting(t *testing.T) {
	h := newLoopHarness("http://127.0.0.1:1/v1", Config{}, NewRegistry())
	before := h.Client

	newCfg := h.Config
	newCfg.Model = "model-only-change"
	newCfg.MaxRetries = 2

	h.Reconfigure(newCfg, nil)

	if h.Client != before {
		t.Error("nil client replaced the existing Client; it must be tolerated")
	}
	if h.Config.Model != "model-only-change" {
		t.Errorf("Config.Model = %q, want %q", h.Config.Model, "model-only-change")
	}
	if before.maxRetries != 2 {
		t.Errorf("existing client maxRetries = %d, want 2 from the new Config", before.maxRetries)
	}
}

// TestReconfigureMemoryReInjectionNoStack: with a Memory store set on the
// new Config, calling Reconfigure twice must leave exactly ONE
// "=== PERSISTENT MEMORY" block in History[0], rebuilt over the LATEST
// systemBase (the applyMemory invariant from harness/memory.go), and the
// memory block must reflect the store's entries.
func TestReconfigureMemoryReInjectionNoStack(t *testing.T) {
	memPath := filepath.Join(t.TempDir(), "memory.md")
	if err := os.WriteFile(memPath, []byte("- the deploy target is prod-eu\n"), 0o644); err != nil {
		t.Fatalf("seed memory file: %v", err)
	}

	// Start WITHOUT memory (New-era: no store on Config).
	h := newLoopHarness("http://127.0.0.1:1/v1", Config{
		SystemPrompt: "base persona",
	}, NewRegistry())
	h.History = append(h.History, Message{Role: "user", Content: "mid-conversation"})

	// First Reconfigure attaches the store: block appears on top of the
	// (unchanged) systemBase.
	cfg1 := h.Config
	cfg1.Model = "first-swap"
	cfg1.Memory = &Memory{Path: memPath}
	h.Reconfigure(cfg1, nil)

	sys := ExtractText(h.History[0].Content)
	if n := strings.Count(sys, "=== PERSISTENT MEMORY"); n != 1 {
		t.Fatalf("History[0] has %d memory headers after one Reconfigure, want 1:\n%s", n, sys)
	}
	if !strings.Contains(sys, "- the deploy target is prod-eu") {
		t.Errorf("History[0] = %q, want the memory entry", sys)
	}
	if !strings.HasPrefix(sys, "base persona") {
		t.Errorf("History[0] = %q, want it to start with the systemBase prompt", sys)
	}

	// Second Reconfigure changes the system prompt AND re-attaches the
	// store: exactly one block, now over the NEW base, not stacked.
	cfg2 := h.Config
	cfg2.Model = "second-swap"
	cfg2.SystemPrompt = "re-persona"
	cfg2.Memory = &Memory{Path: memPath}
	h.Reconfigure(cfg2, nil)

	sys = ExtractText(h.History[0].Content)
	if n := strings.Count(sys, "=== PERSISTENT MEMORY"); n != 1 {
		t.Fatalf("History[0] has %d memory headers after two Reconfigures, want 1 (no stacking):\n%s", n, sys)
	}
	if n := strings.Count(sys, memoryFooter); n != 1 {
		t.Fatalf("History[0] has %d memory footers, want 1:\n%s", n, sys)
	}
	if !strings.HasPrefix(sys, "re-persona") {
		t.Errorf("History[0] = %q, want it rebuilt over the new systemBase", sys)
	}

	// Conversation and History length still intact: system + user only.
	if len(h.History) != 2 {
		t.Fatalf("History has %d messages, want 2: %+v", len(h.History), h.History)
	}
	if ExtractText(h.History[1].Content) != "mid-conversation" {
		t.Errorf("History[1] = %q, want the mid-conversation message untouched", ExtractText(h.History[1].Content))
	}
}
