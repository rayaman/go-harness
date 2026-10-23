package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// JSONL transcript viewer tests (web/logs.go)
// ---------------------------------------------------------------------------

// writeTranscript seeds a synthetic JSONL file matching the
// transcriptEvent schema in harness/logging.go and returns its path.
func writeTranscript(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript %q: %v", name, err)
	}
	return path
}

func synthLine(model string, msgs int) string {
	b, _ := json.Marshal(map[string]any{
		"ts": "2026-01-01T00:00:00Z", "kind": "request",
		"model": model, "messages": msgs, "tools": 2,
	})
	return string(b)
}

func synthResponse(content string, prompt, completion, total int) string {
	b, _ := json.Marshal(map[string]any{
		"ts": "2026-01-01T00:00:01Z", "kind": "response",
		"content": content, "tool_calls": 0,
		"usage": map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": total},
	})
	return string(b)
}

func synthToolCall(name, result string) string {
	b, _ := json.Marshal(map[string]any{
		"ts": "2026-01-01T00:00:02Z", "kind": "tool_call",
		"name": name, "arguments": `{"q":"x"}`, "result": result,
	})
	return string(b)
}

func TestListTranscriptsSkipsNonMatchingAndSortsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "transcript-100.jsonl", []string{synthLine("m", 1)})
	writeTranscript(t, dir, "transcript-200.jsonl", []string{synthLine("m", 2)})
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript-abc.jsonl"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "transcript-999.jsonl"), 0o755); err != nil {
		t.Fatal(err) // a directory with a matching name must be skipped too
	}

	// Distinct mtimes: make transcript-100 the newer one.
	newer := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "transcript-100.jsonl"), newer, newer); err != nil {
		t.Fatal(err)
	}

	list, err := ListTranscripts(dir)
	if err != nil {
		t.Fatalf("ListTranscripts: %v", err)
	}
	if list.Disabled {
		t.Errorf("present dir should not report disabled: %+v", list)
	}
	if len(list.Files) != 2 {
		t.Fatalf("listed %d files, want 2: %+v", len(list.Files), list.Files)
	}
	if list.Files[0].Name != "transcript-100.jsonl" || list.Files[1].Name != "transcript-200.jsonl" {
		t.Errorf("order = %+v, want transcript-100 (newer mtime) first", list.Files)
	}
	if list.Files[0].Size == 0 || list.Files[0].ModTime.IsZero() {
		t.Errorf("file info incomplete: %+v", list.Files[0])
	}
}

func TestListTranscriptsDisabledCases(t *testing.T) {
	// Unset dir.
	list, err := ListTranscripts("")
	if err != nil {
		t.Fatalf("ListTranscripts(\"\") error: %v", err)
	}
	if !list.Disabled || list.Reason == "" || len(list.Files) != 0 {
		t.Errorf("unset dir gave %+v, want disabled + reason + empty list", list)
	}
	if !strings.Contains(list.Reason, "log-dir") {
		t.Errorf("reason %q should mention log-dir", list.Reason)
	}

	// Missing dir.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	list, err = ListTranscripts(missing)
	if err != nil {
		t.Fatalf("ListTranscripts(missing) error: %v", err)
	}
	if !list.Disabled || len(list.Files) != 0 {
		t.Errorf("missing dir gave %+v, want disabled + empty list", list)
	}

	// Present but empty: enabled, zero files.
	empty := t.TempDir()
	list, err = ListTranscripts(empty)
	if err != nil {
		t.Fatalf("ListTranscripts(empty): %v", err)
	}
	if list.Disabled || len(list.Files) != 0 {
		t.Errorf("empty dir gave %+v, want enabled + empty list", list)
	}
}

func TestReadTranscriptParseSummariesAndPagination(t *testing.T) {
	dir := t.TempDir()
	lines := []string{
		synthLine("first-model", 3),
		synthResponse("hello", 120, 30, 150),
		synthToolCall("fake_tool", "ok output"),
		synthToolCall("fake_tool", "error: boom"),
		`{"ts":"x","kind":"weird_kind","whatever":42}`,
		`not json at all`,
	}
	writeTranscript(t, dir, "transcript-1.jsonl", lines)

	page, err := ReadTranscript(dir, "transcript-1.jsonl", 0, 0)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(page.Lines) != 6 {
		t.Fatalf("parsed %d lines, want 6: %+v", len(page.Lines), page.Lines)
	}
	if page.HasMore {
		t.Errorf("HasMore true after consuming the whole file")
	}

	first := page.Lines[0]
	if first.Kind != "request" || first.Model != "first-model" || first.Messages != 3 {
		t.Errorf("request line = %+v", first)
	}
	if first.Summary != "request: model=first-model messages=3 tools=2" {
		t.Errorf("request summary = %q", first.Summary)
	}

	resp := page.Lines[1]
	if resp.Usage == nil || resp.Usage.TotalTokens != 150 {
		t.Errorf("response usage = %+v", resp.Usage)
	}
	if !strings.HasPrefix(resp.Summary, "response: tokens=150") {
		t.Errorf("response summary = %q", resp.Summary)
	}

	toolOK := page.Lines[2]
	if toolOK.OK == nil || !*toolOK.OK {
		t.Errorf("tool_call ok line = %+v", toolOK)
	}
	if toolOK.Summary != "tool: name=fake_tool ok" {
		t.Errorf("tool summary = %q", toolOK.Summary)
	}
	toolErr := page.Lines[3]
	if toolErr.OK == nil || *toolErr.OK {
		t.Errorf("tool_call error line = %+v, want ok=false", toolErr)
	}
	if !strings.HasSuffix(toolErr.Summary, "err") {
		t.Errorf("tool error summary = %q", toolErr.Summary)
	}

	unknown := page.Lines[4]
	if unknown.Kind != "weird_kind" {
		t.Errorf("unknown kind not carried through: %+v", unknown)
	}
	if !strings.HasPrefix(unknown.Summary, "raw:") || unknown.Raw == "" {
		t.Errorf("unknown line should fall back to raw: %+v", unknown)
	}
	corrupt := page.Lines[5]
	if corrupt.Kind != "" || !strings.HasPrefix(corrupt.Summary, "unparseable") || corrupt.Raw != "not json at all" {
		t.Errorf("corrupt line = %+v, want raw fallback", corrupt)
	}

	// Pagination: a limit of 2 pages the file by cursor.
	var all []LogLine
	offset := int64(0)
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatalf("pagination did not terminate")
		}
		p, err := ReadTranscript(dir, "transcript-1.jsonl", offset, 2)
		if err != nil {
			t.Fatalf("paged ReadTranscript: %v", err)
		}
		all = append(all, p.Lines...)
		if !p.HasMore || len(p.Lines) == 0 {
			break
		}
		if p.NextOffset <= offset {
			t.Fatalf("cursor did not advance: %d -> %d", offset, p.NextOffset)
		}
		offset = p.NextOffset
	}
	if len(all) != 6 {
		t.Errorf("paged read got %d lines total, want 6", len(all))
	}
	// Offsets must be distinct and ordered.
	for i := 1; i < len(all); i++ {
		if all[i].Offset <= all[i-1].Offset {
			t.Errorf("offsets not ordered at %d: %+v", i, all[i])
		}
	}

	// Offset past EOF: empty page, no error.
	p, err := ReadTranscript(dir, "transcript-1.jsonl", page.Size+1000, 0)
	if err != nil {
		t.Fatalf("ReadTranscript past EOF: %v", err)
	}
	if len(p.Lines) != 0 || p.HasMore {
		t.Errorf("past-EOF page = %+v, want empty", p)
	}
}

func TestReadTranscriptRejectsUnsafeNames(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", "..", "../secret.jsonl", "notes.txt", "transcript-1.json", "transcript-1.jsonl;rm"} {
		if _, err := ReadTranscript(dir, bad, 0, 0); err == nil {
			t.Errorf("ReadTranscript(%q) accepted an unsafe name", bad)
		}
	}
	if _, err := ListTranscripts(filepath.Join(t.TempDir(), "missing-deeper")); err != nil {
		t.Errorf("ListTranscripts of a missing nested dir: %v, want nil error", err)
	}
}

func TestFollowDeliversAppendedLines(t *testing.T) {
	dir := t.TempDir()
	name := fmt.Sprintf("transcript-%d.jsonl", time.Now().UnixNano())
	path := writeTranscript(t, dir, name, []string{synthLine("m", 1)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := Follow(ctx, dir, name)
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}

	// Existing content arrives first.
	first := <-ch
	if first.Kind != "request" || first.Model != "m" {
		t.Fatalf("first followed line = %+v", first)
	}

	// Append two lines while following (the way the harness's logger
	// appends mid-run).
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(synthResponse("appended", 10, 2, 12) + "\n"); err != nil {
		t.Fatal(err)
	}
	// A deliberately partial line: Follow must NOT emit it until the
	// newline lands.
	if _, err := f.WriteString(`{"kind":"tool_call","name":"partial"`); err != nil {
		t.Fatal(err)
	}

	second := <-ch
	if second.Kind != "response" || second.Content != "appended" {
		t.Errorf("second followed line = %+v, want appended response", second)
	}

	// Give the poller a couple of intervals: the partial line must not
	// show up.
	select {
	case extra := <-ch:
		t.Fatalf("follow emitted a partial line before it was complete: %+v", extra)
	case <-time.After(3 * logPollInterval):
	}

	if _, err := f.WriteString("}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	third := <-ch
	if third.Kind != "tool_call" || third.Name != "partial" {
		t.Errorf("third followed line = %+v, want completed partial tool_call", third)
	}

	// Truncation/rotation: shrink the file under the cursor; Follow
	// resets to 0 and re-reads from the top.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	f2, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.WriteString(synthLine("after-rotation", 7) + "\n"); err != nil {
		t.Fatal(err)
	}
	f2.Close()

	fourth := <-ch
	if fourth.Kind != "request" || fourth.Model != "after-rotation" {
		t.Errorf("post-rotation followed line = %+v", fourth)
	}

	// Cancelling the context closes the channel.
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed: Follow shut down cleanly
			}
			// drain stragglers until close
		case <-deadline:
			t.Fatalf("Follow channel not closed after ctx cancel")
		}
	}
}

func TestFollowRejectsBadInputs(t *testing.T) {
	if _, err := Follow(context.Background(), "", "transcript-1.jsonl"); err == nil {
		t.Errorf("Follow with empty dir returned nil error")
	}
	dir := t.TempDir()
	if _, err := Follow(context.Background(), dir, "evil"); err == nil {
		t.Errorf("Follow with unsafe name returned nil error")
	}
	if _, err := Follow(context.Background(), dir, "transcript-1.jsonl"); err == nil {
		t.Errorf("Follow on a missing file returned nil error")
	}
}
