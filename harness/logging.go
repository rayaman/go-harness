package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// This file implements the JSONL transcript logger from roadmap 3.1: one
// JSON object per line for every chat request sent, every assistant
// response received, and every tool call executed, into a per-session
// file under Config.LogDir. Logging is OFF unless LogDir is set, and when
// on it is strictly best-effort: a failed write never interrupts the
// agent loop.

// transcriptFieldBytes bounds the free-text fields written to the
// transcript (response content, tool arguments/results). Longer values
// are cut with the same head+tail "[truncated N of M bytes]" marker
// truncateToolOutput uses, so one huge tool dump can't bloat the log.
const transcriptFieldBytes = 4096

// transcriptEvent is one JSONL line. Kind selects which of the remaining
// fields apply; omitempty keeps every line small.
type transcriptEvent struct {
	TS   string `json:"ts"`   // RFC3339Nano, when the event happened
	Kind string `json:"kind"` // "request" | "response" | "tool_call"

	// request fields
	Model    string `json:"model,omitempty"`
	Messages int    `json:"messages,omitempty"`
	Tools    int    `json:"tools,omitempty"`

	// response fields
	Content   string `json:"content,omitempty"`
	ToolCalls int    `json:"tool_calls,omitempty"`
	Usage     *Usage `json:"usage,omitempty"` // only when the server reported it

	// tool_call fields
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
}

// transcriptLogger appends transcriptEvent lines to a per-session file
// opened by newTranscriptLogger. All writes are mutex-guarded (the
// parallel tool pool - roadmap 2.1 - calls runTool from goroutines) and
// best-effort: the first failure prints ONE stderr warning and every
// later write is a silent no-op, so logging can never crash or fail the
// agent loop.
type transcriptLogger struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	warned bool
}

// Close releases the transcript log file (roadmap 3.1). It is safe to
// call on a Harness with logging disabled (nil logger) and safe to call
// more than once; later log calls are silently dropped. Callers whose
// process outlives the Harness (long-running REPLs, tests that clean up
// temp dirs on Windows) should defer it.
func (h *Harness) Close() {
	h.transcript.close()
}

// newTranscriptLogger creates dir when needed and opens the per-session
// file transcript-<unixnano>.jsonl inside it. An empty dir means logging
// is disabled: it returns (nil, nil) without touching the disk.
func newTranscriptLogger(dir string) (*transcriptLogger, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, fmt.Sprintf("transcript-%d.jsonl", time.Now().UnixNano()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open transcript log %q: %w", path, err)
	}
	return &transcriptLogger{file: f, path: path}, nil
}

// log appends ev as one JSON line plus a newline. A nil receiver (logging
// disabled) returns immediately. Write failures warn once on stderr and
// are otherwise swallowed.
func (l *transcriptLogger) log(ev transcriptEvent) {
	if l == nil {
		return
	}
	line, err := json.Marshal(ev)
	if err != nil {
		// transcriptEvent only holds marshal-safe types, so this is
		// unreachable in practice; keep it silent rather than fatal.
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return // already closed
	}
	if _, err := l.file.Write(append(line, '\n')); err != nil {
		if !l.warned {
			l.warned = true
			fmt.Fprintf(os.Stderr, "warning: transcript logging disabled (%s): %v\n", l.path, err)
		}
	}
}

// close flushes nothing (writes are unbuffered) and closes the session
// file. Safe to call more than once and on a nil receiver; later log
// calls are silently dropped.
func (l *transcriptLogger) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// logRequest records one outbound chat request (roadmap 3.1): just the
// model, message count and tool-schema count - the full bodies are what
// History already holds, and the point is token accounting plus
// post-hoc debugging of askLoop, not duplicating every prompt.
func (h *Harness) logRequest(req ChatRequest) {
	h.transcript.log(transcriptEvent{
		TS:       time.Now().Format(time.RFC3339Nano),
		Kind:     "request",
		Model:    req.Model,
		Messages: len(req.Messages),
		Tools:    len(req.Tools),
	})
}

// logResponse records one assistant message off the wire, with its plain
// text (truncated), how many tool calls it requested, and the reported
// usage - omitted entirely when the server sent no usage at all.
func (h *Harness) logResponse(msg Message, usage Usage) {
	ev := transcriptEvent{
		TS:        time.Now().Format(time.RFC3339Nano),
		Kind:      "response",
		Content:   truncateToolOutput(ExtractText(msg.Content), transcriptFieldBytes),
		ToolCalls: len(msg.ToolCalls),
	}
	if usage.PromptTokens != 0 || usage.CompletionTokens != 0 || usage.TotalTokens != 0 {
		u := usage
		ev.Usage = &u
	}
	h.transcript.log(ev)
}

// logToolCall records one tool call execution: name, arguments and the
// (possibly truncated) result. Safe to call from the parallel pool.
func (h *Harness) logToolCall(call ToolCall, result string) {
	h.transcript.log(transcriptEvent{
		TS:        time.Now().Format(time.RFC3339Nano),
		Kind:      "tool_call",
		Name:      call.Function.Name,
		Arguments: truncateToolOutput(call.Function.Arguments, transcriptFieldBytes),
		Result:    truncateToolOutput(result, transcriptFieldBytes),
	})
}
