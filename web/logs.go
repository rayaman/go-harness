package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"ai-harness/harness"
)

// ---------------------------------------------------------------------------
// JSONL transcript viewer / tailer (web-server roadmap, log-viewer
// subtask)
//
// harness/logging.go (roadmap 3.1) writes one JSON object per line into
// transcript-<unixnano>.jsonl under harness.Config.LogDir: kind
// "request" (model, message count, tool-schema count), "response"
// (content preview, tool-call count, usage when the server reported it)
// and "tool_call" (name, arguments, result). This file lists those
// files, parses them into a typed LogLine with a human-friendly summary,
// pages big files by byte offset, and tails a growing file for SSE.
//
// The log dir comes from SessionManager.LogDir (the LIVE effective
// config). An unset or missing dir is not an error: the list comes back
// empty with Disabled/Reason set, so the UI can show "logging disabled -
// set log-dir in config.yaml" instead of a broken panel.
// ---------------------------------------------------------------------------

// transcriptNamePattern is the exact naming rule newTranscriptLogger in
// harness/logging.go uses; anything else in the log dir is skipped.
var transcriptNamePattern = regexp.MustCompile(`^transcript-\d+\.jsonl$`)

// maxLogPageBytes caps how much of a transcript one read request
// returns, so a multi-hundred-MB log cannot be slurped into the browser
// in one page (maxLogPageLines caps the line count too).
const (
	maxLogPageBytes = 256 * 1024
	maxLogPageLines = 500
)

// logPollInterval is how often Follow re-stats and re-reads the
// transcript. Polling (rather than fsnotify) is deliberate: it is simple,
// needs no extra dependency, and is Windows-safe - Windows file-change
// notifications are unreliable for a file another process holds open for
// appending, which is exactly the transcript's situation.
const logPollInterval = 500 * time.Millisecond

// loggingDisabledReason is the indicator string the UI shows when there
// is nothing to tail.
const loggingDisabledReason = "logging disabled - set log-dir in config.yaml"

// LogFileInfo is one transcript file in the log dir.
type LogFileInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// TranscriptList is the log-dir listing: the files (newest first) plus
// the disabled indicator. Disabled is true when dir was empty (log-dir
// unset) or the directory does not exist yet; Reason then carries the
// human explanation. An empty-but-present dir lists zero files with
// Disabled false (logging is on, nothing logged yet).
type TranscriptList struct {
	Dir      string        `json:"dir,omitempty"`
	Disabled bool          `json:"disabled"`
	Reason   string        `json:"reason,omitempty"`
	Files    []LogFileInfo `json:"files"`
}

// ListTranscripts returns every transcript-*.jsonl in dir, newest first
// (mtime, falling back to name order for identical mtimes). Non-matching
// names are skipped. An empty or missing dir yields an empty list plus
// the disabled indicator rather than an error.
func ListTranscripts(dir string) (TranscriptList, error) {
	if dir == "" {
		return TranscriptList{Disabled: true, Reason: loggingDisabledReason, Files: []LogFileInfo{}}, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TranscriptList{Dir: dir, Disabled: true, Reason: loggingDisabledReason, Files: []LogFileInfo{}}, nil
		}
		return TranscriptList{}, fmt.Errorf("read log dir %q: %w", dir, err)
	}

	files := make([]LogFileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !transcriptNamePattern.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished mid-listing: skip rather than fail the whole list
		}
		files = append(files, LogFileInfo{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].ModTime.Equal(files[j].ModTime) {
			return files[i].ModTime.After(files[j].ModTime)
		}
		return files[i].Name > files[j].Name
	})
	return TranscriptList{Dir: dir, Files: files}, nil
}

// LogLine is one parsed transcript record, plus the raw line so the UI
// can always show exactly what was logged even for record shapes this
// package does not know.
type LogLine struct {
	Seq    int   `json:"seq"`
	Offset int64 `json:"offset"` // byte offset of the line, the paging cursor

	TS   string `json:"ts,omitempty"`
	Kind string `json:"kind,omitempty"`

	// request fields
	Model    string `json:"model,omitempty"`
	Messages int    `json:"messages,omitempty"`
	Tools    int    `json:"tools,omitempty"`

	// response fields
	Content   string         `json:"content,omitempty"`
	ToolCalls int            `json:"tool_calls,omitempty"`
	Usage     *harness.Usage `json:"usage,omitempty"`

	// tool_call fields
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	OK        *bool  `json:"ok,omitempty"` // tool_call only: result did not start with "error:"

	Summary string `json:"summary"`
	Raw     string `json:"raw"`
}

// transcriptRecord mirrors the transcriptEvent schema in
// harness/logging.go (field-for-field, so a schema change there shows
// up as unknown/zero fields here rather than a parse failure).
type transcriptRecord struct {
	TS   string `json:"ts"`
	Kind string `json:"kind"`

	Model    string `json:"model"`
	Messages int    `json:"messages"`
	Tools    int    `json:"tools"`

	Content   string         `json:"content"`
	ToolCalls int            `json:"tool_calls"`
	Usage     *harness.Usage `json:"usage"`

	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
}

// parseLogLine decodes one JSONL line. An unparseable line is NOT an
// error: it comes back with Raw set, Kind "" and the raw text as the
// summary, so one corrupt line never blanks a page.
func parseLogLine(raw string, seq int, offset int64) LogLine {
	line := LogLine{Seq: seq, Offset: offset, Raw: raw}

	var rec transcriptRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		line.Summary = "unparseable line: " + previewText(raw, 120)
		return line
	}

	line.TS = rec.TS
	line.Kind = rec.Kind
	line.Model = rec.Model
	line.Messages = rec.Messages
	line.Tools = rec.Tools
	line.Content = rec.Content
	line.ToolCalls = rec.ToolCalls
	line.Usage = rec.Usage
	line.Name = rec.Name
	line.Arguments = rec.Arguments
	line.Result = rec.Result

	switch rec.Kind {
	case "request":
		line.Summary = fmt.Sprintf("request: model=%s messages=%d tools=%d", rec.Model, rec.Messages, rec.Tools)
	case "response":
		if rec.Usage != nil {
			line.Summary = fmt.Sprintf("response: tokens=%d prompt=%d completion=%d tool_calls=%d",
				rec.Usage.TotalTokens, rec.Usage.PromptTokens, rec.Usage.CompletionTokens, rec.ToolCalls)
		} else {
			line.Summary = fmt.Sprintf("response: tool_calls=%d (no usage reported)", rec.ToolCalls)
		}
	case "tool_call":
		ok := !strings.HasPrefix(rec.Result, "error:")
		line.OK = &ok
		verdict := "ok"
		if !ok {
			verdict = "err"
		}
		line.Summary = fmt.Sprintf("tool: name=%s %s", rec.Name, verdict)
	default:
		// Unknown record type: fall back to the raw JSON rather than
		// failing the page (the UI can render Raw verbatim).
		line.Summary = "raw: " + previewText(raw, 160)
	}
	return line
}

// TranscriptPage is one page of a parsed transcript.
type TranscriptPage struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	Lines      []LogLine `json:"lines"`
	NextOffset int64     `json:"next_offset"` // cursor to pass back for the next page
	HasMore    bool      `json:"has_more"`
	Disabled   bool      `json:"disabled,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

// safeTranscriptName rejects anything that is not a bare transcript file
// name, so a request can never traverse out of the log dir.
func safeTranscriptName(name string) error {
	if !transcriptNamePattern.MatchString(name) {
		return fmt.Errorf("not a transcript file name: %q", name)
	}
	return nil
}

// ReadTranscript parses transcript `name` in dir starting at byte
// `offset`, returning at most maxLogPageLines lines and maxLogPageBytes
// of raw text. NextOffset is the byte offset just past the last line
// returned, so the browser pages by feeding it back as offset; HasMore
// says whether more complete lines remain. offset past EOF yields an
// empty page, not an error.
func ReadTranscript(dir, name string, offset int64, limit int) (TranscriptPage, error) {
	if dir == "" {
		return TranscriptPage{Name: name, Disabled: true, Reason: loggingDisabledReason}, nil
	}
	if err := safeTranscriptName(name); err != nil {
		return TranscriptPage{}, err
	}
	if limit <= 0 || limit > maxLogPageLines {
		limit = maxLogPageLines
	}

	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return TranscriptPage{}, fmt.Errorf("open transcript %q: %w", name, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return TranscriptPage{}, fmt.Errorf("stat transcript %q: %w", name, err)
	}
	page := TranscriptPage{Name: name, Size: info.Size(), Lines: []LogLine{}}

	if offset < 0 {
		offset = 0
	}
	if offset > info.Size() {
		return page, nil // beyond EOF: nothing to show
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return page, nil
	}

	br := bufio.NewReader(f)
	cursor := offset
	consumed := int64(0)
	seq := 0
	for {
		text, readErr := br.ReadString('\n')
		if len(text) > 0 && text[len(text)-1] == '\n' {
			// Only complete lines count: a trailing partial line (the
			// logger may be mid-write) stays unread for the next page.
			trimmed := strings.TrimRight(text, "\r\n")
			if trimmed != "" {
				page.Lines = append(page.Lines, parseLogLine(trimmed, seq, cursor))
				seq++
			}
			cursor += int64(len(text))
			consumed += int64(len(text))
			if seq >= limit || consumed >= maxLogPageBytes {
				page.HasMore = true // there may be more; the caller re-asks at NextOffset
				break
			}
		}
		if readErr != nil {
			break // io.EOF: the readable region is exhausted
		}
	}
	page.NextOffset = cursor
	return page, nil
}

// Follow tails transcript `name` in dir, emitting parsed lines as they
// are appended. It polls every logPollInterval (see the note above on
// why polling), reads only complete lines, and handles the two ways a
// transcript moves under a tail: GROWTH (the normal append) and
// TRUNCATION/ROTATION (the file shrank below the read cursor, or was
// replaced wholesale - the cursor resets to 0 and the tail re-reads from
// the top).
//
// The returned channel is closed when ctx is cancelled or the file
// permanently disappears (deleted and not recreated within one poll
// interval after the first miss). A file that is missing at the moment
// Follow is called is an error.
func Follow(ctx context.Context, dir, name string) (<-chan LogLine, error) {
	if dir == "" {
		return nil, errors.New(loggingDisabledReason)
	}
	if err := safeTranscriptName(name); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open transcript %q: %w", name, err)
	}

	out := make(chan LogLine, 64)
	go func() {
		defer close(out)

		var offset int64
		seq := 0
		misses := 0
		ticker := time.NewTicker(logPollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			newOffset, emitErr, missing := followOnce(ctx, path, name, offset, &seq, out)
			if emitErr != nil {
				return // ctx cancelled mid-send
			}
			offset = newOffset
			if missing {
				misses++
				if misses >= 2 {
					return // gone for two consecutive polls: rotation away, stop
				}
			} else {
				misses = 0
			}
		}
	}()
	return out, nil
}

// followOnce reads everything complete at path past offset, sending
// parsed lines to out. It returns the new cursor, whether the send loop
// was cut short by ctx, and whether the file was missing this poll (in
// which case the cursor is kept so a recreated file resumes where it
// left off, and a shrunken file resets the cursor to 0).
func followOnce(ctx context.Context, path, name string, offset int64, seq *int, out chan<- LogLine) (newOffset int64, ctxErr error, missing bool) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return offset, nil, true
		}
		return offset, nil, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return offset, nil, false
	}
	if info.Size() < offset {
		offset = 0 // truncated or rotated: start over from the top
	}
	if info.Size() == offset {
		return offset, nil, false
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, nil, false
	}

	br := bufio.NewReader(f)
	cursor := offset
	for {
		text, readErr := br.ReadString('\n')
		if len(text) > 0 && text[len(text)-1] == '\n' {
			trimmed := strings.TrimRight(text, "\r\n")
			if trimmed != "" {
				select {
				case out <- parseLogLine(trimmed, *seq, cursor):
					*seq++
				case <-ctx.Done():
					return cursor, ctx.Err(), false
				}
			}
			cursor += int64(len(text))
		}
		if readErr != nil {
			break
		}
	}
	return cursor, nil, false
}
