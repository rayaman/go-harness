package web

// Regression tests for the two web-mode bugs reported after the first
// browser session:
//
//  1. "Streaming isn't working" - web turns ran with StreamThinking off
//     (the REPL's opt-in default), so the harness took the
//     non-streaming path and the chat received one content event at
//     turn end. Pinning: WebSettings.StreamThinking is a *bool (the UI
//     can force it either way), the flag propagates through Apply and
//     the store, and a streaming turn really does emit MULTIPLE content
//     deltas with "stream": true on the wire.
//
//  2. "Boxes cut off responses" - Session.History() returned main.go's
//     /history-style 120-rune flattened previews, and the SPA rebuilds
//     its transcript boxes from that endpoint after every done event,
//     so every long reply visibly shrank. Pinning: History() carries
//     the complete untruncated text with newlines intact.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ai-harness/harness"
	"ai-harness/harness/config"
)

// ---------------------------------------------------------------------------
// Bug 1 pins: StreamThinking tri-state
// ---------------------------------------------------------------------------

func TestApplyStreamThinkingTriState(t *testing.T) {
	base := presetConfig()
	base.StreamThinking = true

	// nil override keeps the preset (true here, and false below).
	if got := (WebSettings{}).Apply(base).StreamThinking; !got {
		t.Error("nil StreamThinking override clobbered the preset's true")
	}
	baseOff := base
	baseOff.StreamThinking = false
	if got := (WebSettings{}).Apply(baseOff).StreamThinking; got {
		t.Error("nil StreamThinking override flipped the preset's false to true")
	}

	// Explicit false now wins over a true preset - this is exactly what
	// the old plain-bool field could not express.
	off := false
	if got := (WebSettings{StreamThinking: &off}).Apply(base).StreamThinking; got {
		t.Error("*false override did not disable streaming against a true preset")
	}
	on := true
	baseOff2 := base
	baseOff2.StreamThinking = false
	if got := (WebSettings{StreamThinking: &on}).Apply(baseOff2).StreamThinking; !got {
		t.Error("*true override did not enable streaming against a false preset")
	}
}

func TestSettingsPUTCanDisableStreaming(t *testing.T) {
	base := presetConfig()
	base.StreamThinking = true // web-mode default after main.go's flip
	st, err := NewSettingsStore(tempSettingsPath(t), base, nil)
	if err != nil {
		t.Fatalf("NewSettingsStore: %v", err)
	}
	if eff := st.Effective(); !eff.StreamThinking {
		t.Fatal("no overrides should keep the base's streaming on")
	}

	// The UI's full-form PUT with the stream_thinking checkbox unchecked.
	req := httptest.NewRequest(http.MethodPut, RouteSettings, strings.NewReader(`{"stream_thinking":false}`))
	rec := httptest.NewRecorder()
	st.HandlePut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body %s", rec.Code, rec.Body.String())
	}
	if eff := st.Effective(); eff.StreamThinking {
		t.Error("PUT stream_thinking:false did not disable streaming in Effective()")
	}

	// GET echoes the false back (not omitted), so the form re-renders off.
	rec2 := httptest.NewRecorder()
	st.HandleGet(rec2, httptest.NewRequest(http.MethodGet, RouteSettings, nil))
	var resp map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	v, ok := resp["stream_thinking"]
	if !ok {
		t.Fatal("GET omitted stream_thinking; the form cannot render the saved false")
	}
	if v != false {
		t.Errorf("GET stream_thinking = %v, want false", v)
	}
}

// newStreamingTestManager mirrors newTestManager but with the base preset
// already streaming (what main.go now does for web mode).
func newStreamingTestManager(t *testing.T, baseURL string) *SessionManager {
	t.Helper()
	tmp := t.TempDir()
	base := harness.Config{
		Provider:          "openai",
		BaseURL:           baseURL,
		Model:             "stub-model",
		MaxToolIterations: 10,
		StreamThinking:    true,
	}
	store, err := NewSettingsStore(filepath.Join(tmp, "web-settings.json"), base, nil)
	if err != nil {
		t.Fatalf("NewSettingsStore: %v", err)
	}
	m := NewSessionManager(Config{
		Base:     base,
		Conf:     &config.Config{},
		Client:   harness.NewClient(baseURL, ""),
		Registry: harness.NewRegistry(),
	}, store)
	m.sessionsDir = filepath.Join(tmp, "sessions")
	return m
}

func TestStreamThinkingReachesSessionsAndReconfigure(t *testing.T) {
	srv := newFakeChatServer(t, chatTextReply("ok"))
	m := newStreamingTestManager(t, srv.URL())
	s := m.Active()
	if s == nil {
		t.Fatal("manager has no initial session")
	}
	if !s.harness.Config.StreamThinking {
		t.Error("initial session lost the streaming base config")
	}
	if !m.Create("extra").harness.Config.StreamThinking {
		t.Error("a newly created session lost the streaming base config")
	}

	// Turning streaming off through the store + ReconfigureAll must reach
	// the EXISTING sessions (the switch has to work live, both ways).
	req := httptest.NewRequest(http.MethodPut, RouteSettings, strings.NewReader(`{"stream_thinking":false}`))
	rec := httptest.NewRecorder()
	// Reuse the manager's own store so Effective()/ReconfigureAll agree.
	m.store.HandlePut(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body %s", rec.Code, rec.Body.String())
	}
	m.ReconfigureAll()
	for _, info := range m.List() {
		if got := m.Get(info.ID).harness.Config.StreamThinking; got {
			t.Errorf("session %s still streaming after stream_thinking:false + ReconfigureAll", info.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Bug 1 pin: a streaming turn really streams (multiple deltas, stream:true)
// ---------------------------------------------------------------------------

func TestStreamingTurnEmitsMultipleContentDeltas(t *testing.T) {
	var gotStream *bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream *bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotStream = body.Stream

		w.Header().Set("Content-Type", "text/event-stream")
		// harness/client.go SSE shape: "data: {chunk}\n\n" ... "data: [DONE]".
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"wor\",\"reasoning_content\":\"think \"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ld!\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	m := newStreamingTestManager(t, srv.URL)
	s := m.Active()
	rec := startRecording(s)

	answer, err := s.Post("greet", nil)
	rec.stop(s)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if gotStream == nil || !*gotStream {
		t.Errorf("model server request had stream=%v, want true (web turns must request streaming)", gotStream)
	}

	var contentEvents, thinkingEvents int
	var joined strings.Builder
	for _, ev := range rec.list() {
		switch ev.Type {
		case EventContent:
			contentEvents++
			joined.WriteString(ev.Text)
		case EventThinking:
			thinkingEvents++
		}
	}
	if contentEvents < 2 {
		t.Errorf("streaming turn emitted %d content events, want >= 2 deltas", contentEvents)
	}
	if joined.String() != "Hello world!" {
		t.Errorf("joined content deltas = %q, want %q", joined.String(), "Hello world!")
	}
	if thinkingEvents < 1 {
		t.Errorf("reasoning_content deltas not forwarded as thinking events (got %d)", thinkingEvents)
	}
	if answer != "Hello world!" {
		t.Errorf("Post answer = %q, want %q", answer, "Hello world!")
	}
}

// ---------------------------------------------------------------------------
// Bug 2 pins: History() carries full untruncated text
// ---------------------------------------------------------------------------

func TestHistoryCarriesFullUntruncatedText(t *testing.T) {
	long := "first line\nsecond line\n" + strings.Repeat("detail ", 80) // >500 runes, multi-line
	if len([]rune(long)) <= historyFullTextMinRunes {
		t.Fatalf("test reply too short to prove non-truncation")
	}
	srv := newFakeChatServer(t, chatUsageReply(long, 10, 20, 30))
	m, _ := newTestManager(t, srv.URL())
	s := m.Active()
	if _, err := s.Post("long reply please", nil); err != nil {
		t.Fatalf("Post: %v", err)
	}

	hist := s.History()
	if len(hist) != 2 {
		t.Fatalf("history = %+v, want user + assistant", hist)
	}
	last := hist[1]
	if last.Text != long {
		t.Errorf("assistant history text was cut: len=%d hasEllipsis=%v\n got: %.160q\nwant: %.160q",
			len([]rune(last.Text)), strings.HasSuffix(last.Text, "..."), last.Text, long)
	}
	if strings.Contains(last.Text, "...") || !strings.Contains(last.Text, "\n") {
		t.Errorf("history text looks preview-formatted (ellipsised or newline-flattened): %.120q", last.Text)
	}
}

// historyFullTextMinRunes documents the threshold the truncation bug used
// (historyPreviewRunes was 120); the test reply must comfortably exceed it.
const historyFullTextMinRunes = 120
