package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"ai-harness/harness"
	"ai-harness/harness/config"
	luatool "ai-harness/harness/lua-tool"
)

// ---------------------------------------------------------------------------
// Session manager (web-server roadmap, session-manager subtask)
//
// Multiple named chat sessions share ONE harness.Client and ONE
// harness.Registry (the tools and the model connection are process-wide),
// while each session owns its own harness.Harness and therefore its own
// conversation History. Sessions persist through Harness.SaveSession /
// LoadSession (harness/session.go) as agent/sessions/<slug>.json.
//
// Two goroutine-safety constraints shape this file:
//
//  1. harness.Harness is NOT goroutine-safe: History is a plain slice
//     mutated by Ask/Reset/Compact/LoadSession. Every session therefore
//     carries a mutex (Session.mu) that serializes turns AND any state
//     read that walks History (History(), Info(), Save()).
//
//  2. The shared harness.Registry has no internal mutex either, and a
//     turn reads it (Registry.Schemas() per request, PolicyFor per tool
//     call). A policy change or Lua re-registration racing with another
//     session's in-flight turn is a real data race. All registry
//     mutations this package performs (SetPolicy, ReloadTools,
//     ReconfigureAll's client swap) serialize through the manager-level
//     regMu, which keeps concurrent WEB mutations from racing each other.
//     A mutation still cannot be made atomic with another session's
//     in-flight turn without a registry-wide read lock the harness does
//     not offer - the same caveat the REPL's per-prompt hot-reload
//     accepts (main.go reloads between prompts, never mid-turn).
// ---------------------------------------------------------------------------

// DefaultSessionsDir is where session JSON files live, following the
// same working-directory convention as agent/config.yaml and
// agent/memory.md. The manager's sessionsDir field (defaulting to this)
// is package-private so tests can point it at a temp dir instead of the
// real agent/ tree.
const DefaultSessionsDir = "agent/sessions"

// ---------------------------------------------------------------------------
// Event: the single SSE payload type for the whole UI
// ---------------------------------------------------------------------------

// Event Type constants. The UI is built against these exact strings.
const (
	EventHello            = "hello"             // sent to a fresh subscriber
	EventStatus           = "status"            // turn lifecycle marker (Message carries the state)
	EventThinking         = "thinking"          // reasoning-trace delta (Text)
	EventContent          = "content"           // reply-text delta (Text)
	EventToolCall         = "tool_call"         // model requested a tool (ID/Name/Args)
	EventToolResult       = "tool_result"       // tool finished (CallID/Name/Result/OK)
	EventApprovalRequest  = "approval_request"  // PolicyAsk tool awaiting the browser's answer
	EventApprovalResolved = "approval_resolved" // answer landed: approved/denied/timeout/cancelled
	EventUsage            = "usage"             // token counts of the last model call
	EventError            = "error"             // turn failure (Message)
	EventDone             = "done"              // turn finished, answer in Text
	EventLog              = "log"               // server-side log line (reserved for the router)
)

// Event is one SSE payload. Every field except Type is optional
// (omitempty); which fields carry values depends on Type:
//
//	hello            Session
//	status           Session, Message ("running")
//	thinking         Text
//	content          Text
//	tool_call        ID, Name, Args
//	tool_result      CallID, Name, Result, OK
//	approval_request ID, Name, Args
//	approval_resolved ID, OK, Resolved ("approved"|"denied"|"timeout"|"cancelled")
//	usage            Usage
//	error            Message
//	done             Session, Text (the full final answer)
//	log              Message
type Event struct {
	Type string `json:"type"`

	Text     string         `json:"text,omitempty"`
	Name     string         `json:"name,omitempty"`
	Args     string         `json:"args,omitempty"`
	Result   string         `json:"result,omitempty"`
	OK       bool           `json:"ok,omitempty"`
	ID       string         `json:"id,omitempty"`
	CallID   string         `json:"call_id,omitempty"`
	Resolved string         `json:"resolved,omitempty"`
	Usage    *harness.Usage `json:"usage,omitempty"`
	Message  string         `json:"message,omitempty"`
	Session  string         `json:"session,omitempty"`
}

// eventBufferSize is the per-subscriber channel capacity. Slow consumers
// never block a turn: broadcast uses a non-blocking send and DROPS the
// event for any subscriber whose buffer is full (documented policy - a
// wedged browser tab loses events, not the agent loop).
const eventBufferSize = 256

// approvalTimeout is how long a PolicyAsk tool waits for the browser's
// answer before the turn resolves it as DENY. Two minutes: long enough
// for a human to notice the prompt, short enough that a closed tab
// cannot wedge a session's turn indefinitely. Tests shrink it via the
// manager's approvalTimeout field.
const approvalTimeout = 2 * time.Minute

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// Session is one chat conversation: its own Harness (own History), turn
// mutex, event bus, cancellable turn context, pending-approval map and
// last Usage. Created and looked up through SessionManager.
type Session struct {
	ID        string
	Name      string
	CreatedAt time.Time

	// mu serializes turns and every read that walks harness.History
	// (History, Info, Save). Harness is not goroutine-safe.
	mu      sync.Mutex
	harness *harness.Harness

	// Event bus: subscribers get broadcast events on a buffered channel;
	// a full buffer drops the event (see eventBufferSize).
	busMu sync.Mutex
	subs  map[chan Event]struct{}

	// The in-flight turn's cancel func (nil when idle), guarded by
	// turnMu because Cancel runs on the HTTP handler's goroutine while
	// Post owns the turn.
	turnMu     sync.Mutex
	turnCancel context.CancelFunc

	// Pending approvals: tool call ID -> the channel Confirm blocks on.
	// ResolveApproval (router/browser) answers it.
	approvalMu      sync.Mutex
	pending         map[string]chan bool
	approvalTimeout time.Duration

	// lastUsage mirrors the harness's LastUsage after each completed
	// turn (surfaced through the usage event).
	lastUsage harness.Usage
}

// Subscribe registers a new event listener and immediately queues the
// hello event. The caller MUST Unsubscribe when done (an SSE handler
// that returns without unsubscribing leaks a channel).
func (s *Session) Subscribe() chan Event {
	ch := make(chan Event, eventBufferSize)
	s.busMu.Lock()
	s.subs[ch] = struct{}{}
	s.busMu.Unlock()
	select {
	case ch <- Event{Type: EventHello, Session: s.ID}:
	default: // buffer full on a brand-new subscriber: unreachable in practice
	}
	return ch
}

// Unsubscribe removes a listener and closes its channel.
func (s *Session) Unsubscribe(ch chan Event) {
	s.busMu.Lock()
	defer s.busMu.Unlock()
	if _, ok := s.subs[ch]; ok {
		delete(s.subs, ch)
		close(ch)
	}
}

// broadcast fans an event out to every subscriber. Non-blocking send: a
// subscriber with a full buffer simply misses this event (slow-consumer
// drop policy - see eventBufferSize).
func (s *Session) broadcast(ev Event) {
	s.busMu.Lock()
	defer s.busMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// ResolveApproval answers a pending approval_request for callID (true =
// allow). It reports whether the call ID was actually pending, so the
// router can 404 a stale or bogus answer.
func (s *Session) ResolveApproval(callID string, approved bool) bool {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	ch, ok := s.pending[callID]
	if !ok {
		return false
	}
	select {
	case ch <- approved: // buffered size 1, never blocks; Confirm may have timed out first
	default:
	}
	return true
}

// Cancel aborts the in-flight turn (roadmap 3.3 semantics: the harness
// rolls History back to its pre-Ask state). It reports whether a turn
// was actually running.
func (s *Session) Cancel() bool {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.turnCancel == nil {
		return false
	}
	s.turnCancel()
	return true
}

// awaitApproval is the Confirm bridge: it publishes an approval_request
// event, blocks until the browser answers, the approvalTimeout expires
// (resolved as DENY with the "timeout" marker so a closed tab cannot
// wedge the turn), or the turn is cancelled. The pending entry is always
// removed (defer).
func (s *Session) awaitApproval(ctx context.Context, call harness.ToolCall) bool {
	ch := make(chan bool, 1)
	s.approvalMu.Lock()
	s.pending[call.ID] = ch
	s.approvalMu.Unlock()
	defer func() {
		s.approvalMu.Lock()
		delete(s.pending, call.ID)
		s.approvalMu.Unlock()
	}()

	s.broadcast(Event{
		Type: EventApprovalRequest,
		ID:   call.ID,
		Name: call.Function.Name,
		Args: call.Function.Arguments,
	})

	timer := time.NewTimer(s.approvalTimeout)
	defer timer.Stop()

	select {
	case ok := <-ch:
		resolved := "denied"
		if ok {
			resolved = "approved"
		}
		s.broadcast(Event{Type: EventApprovalResolved, ID: call.ID, Name: call.Function.Name, OK: ok, Resolved: resolved})
		return ok
	case <-timer.C:
		s.broadcast(Event{Type: EventApprovalResolved, ID: call.ID, Name: call.Function.Name, OK: false, Resolved: "timeout"})
		return false
	case <-ctx.Done():
		s.broadcast(Event{Type: EventApprovalResolved, ID: call.ID, Name: call.Function.Name, OK: false, Resolved: "cancelled"})
		return false
	}
}

// Post runs one agent turn: it takes the session turn mutex, wires
// Confirm (approval bridge) and OnDelta (streaming deltas) for the
// duration of the turn, runs Ask/AskWithImages under a cancellable
// context, and emits Events for deltas, tool activity, usage and
// completion. The final answer is returned alongside the done event.
//
// Tool activity is DERIVED, not hooked: harness.Harness exposes no
// tool-execution callback, so after the turn Post diffs History from
// the index captured before Ask - every assistant message's tool_calls
// become tool_call events and every "tool" message becomes a
// tool_result event. What this CAN show: the complete ordered list of
// calls, their arguments, results and success (a result is marked OK
// unless it starts with the harness's "error: " prefix, the same string
// runTool produces for failures and denials). What it CANNOT show:
// live per-tool progress while the turn is still running - the events
// land when the turn's tool round completes, in History order. The
// approval_request events for PolicyAsk tools ARE live, because they
// ride the Confirm hook.
//
// With Config.StreamThinking on, thinking/content deltas arrive as they
// stream; with it off, exactly one content event carrying
// the whole answer is emitted at completion, so the UI sees the reply
// either way. done always carries the full answer in Text.
func (s *Session) Post(input string, images []string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	s.turnMu.Lock()
	s.turnCancel = cancel
	s.turnMu.Unlock()
	defer func() {
		s.turnMu.Lock()
		s.turnCancel = nil
		s.turnMu.Unlock()
		cancel() // release the context's timer regardless of how the turn ended
	}()

	s.broadcast(Event{Type: EventStatus, Session: s.ID, Message: "running"})

	contentDeltas := 0
	s.harness.OnDelta = func(kind, text string) {
		switch kind {
		case "thinking":
			s.broadcast(Event{Type: EventThinking, Text: text})
		case "content":
			contentDeltas++
			s.broadcast(Event{Type: EventContent, Text: text})
		}
	}
	s.harness.Confirm = func(call harness.ToolCall) bool {
		return s.awaitApproval(ctx, call)
	}
	defer func() {
		s.harness.OnDelta = nil
		s.harness.Confirm = nil
	}()

	before := len(s.harness.History)

	var answer string
	var err error
	if len(images) > 0 {
		answer, err = s.harness.AskWithImages(ctx, input, images)
	} else {
		answer, err = s.harness.Ask(ctx, input)
	}

	// History diff -> tool events. On cancellation askLoop rolls History
	// back to `before`, so a cancelled turn emits no tool events.
	for i := before; i < len(s.harness.History); i++ {
		m := s.harness.History[i]
		for _, tc := range m.ToolCalls {
			s.broadcast(Event{Type: EventToolCall, ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
		}
		if m.Role == "tool" {
			result := harness.ExtractText(m.Content)
			s.broadcast(Event{
				Type:   EventToolResult,
				CallID: m.ToolCallID,
				Name:   m.Name,
				Result: result,
				OK:     !strings.HasPrefix(result, "error:"),
			})
		}
	}

	if ctx.Err() != nil {
		s.broadcast(Event{Type: EventError, Message: "turn cancelled"})
		if err != nil {
			return "", err
		}
		return "", ctx.Err()
	}
	if err != nil {
		s.broadcast(Event{Type: EventError, Message: err.Error()})
		return "", err
	}

	usage := s.harness.LastUsage
	s.lastUsage = usage
	if usage.PromptTokens != 0 || usage.CompletionTokens != 0 || usage.TotalTokens != 0 {
		u := usage
		s.broadcast(Event{Type: EventUsage, Usage: &u})
	}
	// Non-streaming turns never fire OnDelta: emit the whole reply as a
	// single content event so every turn delivers exactly one logical
	// answer stream (delta sequence when streaming, one event when not).
	if contentDeltas == 0 && answer != "" {
		s.broadcast(Event{Type: EventContent, Text: answer})
	}
	s.broadcast(Event{Type: EventDone, Session: s.ID, Text: answer})
	return answer, nil
}

// MessageView is one renderable History row. Text and Reasoning carry
// the COMPLETE message content: the SPA renders its transcript boxes
// from History() and replaces the optimistic live view with it after
// every done event, so preview-style truncation here (what main.go's
// /history uses for the terminal) would visibly cut long replies off.
type MessageView struct {
	Index     int    `json:"index"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	ToolCalls int    `json:"tool_calls,omitempty"`
	Images    int    `json:"images,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

// History returns a renderable view of the conversation. It takes the
// session mutex, so it never walks History while a turn mutates it.
func (s *Session) History() []MessageView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MessageView, 0, len(s.harness.History))
	for i, m := range s.harness.History {
		v := MessageView{
			Index:     i,
			Role:      m.Role,
			Text:      harness.ExtractText(m.Content),
			ToolCalls: len(m.ToolCalls),
			Images:    len(m.Images),
			Reasoning: m.ReasoningContent,
		}
		out = append(out, v)
	}
	return out
}

// Info snapshots the session's listing row (message count read under the
// session mutex).
func (s *Session) Info(active bool) SessionInfo {
	s.mu.Lock()
	messages := len(s.harness.History)
	s.mu.Unlock()
	return SessionInfo{
		ID:        s.ID,
		Name:      s.Name,
		CreatedAt: s.CreatedAt,
		Active:    active,
		Messages:  messages,
	}
}

// previewText flattens newlines and truncates to max runes with an
// ellipsis (same rendering rule as main.go's /history).
func previewText(s string, max int) string {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// SessionManager
// ---------------------------------------------------------------------------

// SessionInfo is one row of the session list.
type SessionInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Active    bool      `json:"active"`
	Messages  int       `json:"messages"`
}

// SessionManager owns the session map, the active pointer, the shared
// Client/Registry handles, and the persistence dir. mu guards the map,
// order and active id; regMu serializes registry mutations (see the
// caveat at the top of this file).
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	order    []string // creation order, for stable listings
	active   string
	nextSeq  int

	regMu    sync.Mutex
	registry *harness.Registry
	client   *harness.Client

	store           *SettingsStore
	conf            *config.Config
	memFile         string
	customToolDir   string
	sessionsDir     string
	approvalTimeout time.Duration

	// luaModTimes tracks custom-tool mtimes for the hot-reload pass
	// (mirrors main.go's package-level luaModTimes map).
	luaModTimes map[string]time.Time
}

// NewSessionManager builds the manager from the web.Config hand-off and
// the settings store. The initial session ADOPTS cfg.Harness so the
// conversation main.go's setupAgent already seeded (if any) stays
// reachable through the UI; with no Harness in the config a fresh one is
// built from store.Effective(). Every session created AFTER this one is
// built from the CURRENT effective config (store.Effective()), so a
// settings change applies to new sessions immediately; existing sessions
// pick the change up through ReconfigureAll, which the router calls
// after a successful settings PUT.
func NewSessionManager(cfg Config, store *SettingsStore) *SessionManager {
	m := &SessionManager{
		sessions:        make(map[string]*Session),
		registry:        cfg.Registry,
		client:          cfg.Client,
		store:           store,
		conf:            cfg.Conf,
		memFile:         cfg.MemFile,
		customToolDir:   cfg.CustomToolDir,
		sessionsDir:     DefaultSessionsDir,
		approvalTimeout: approvalTimeout,
		luaModTimes:     make(map[string]time.Time),
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if cfg.Harness != nil {
		s := m.newSessionLocked("default")
		s.harness = cfg.Harness
	} else {
		m.newSessionLocked("")
	}
	return m
}

// newSessionLocked builds and registers a session. name auto-numbers
// when blank. Callers hold m.mu.
func (m *SessionManager) newSessionLocked(name string) *Session {
	m.nextSeq++
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("Session %d", m.nextSeq)
	}
	id := fmt.Sprintf("s%d", m.nextSeq)

	cfg := m.sessionHarnessConfigLocked()
	s := &Session{
		ID:              id,
		Name:            name,
		CreatedAt:       time.Now(),
		harness:         harness.New(cfg, m.client, m.registry),
		subs:            make(map[chan Event]struct{}),
		pending:         make(map[string]chan bool),
		approvalTimeout: m.approvalTimeout,
	}
	m.sessions[id] = s
	m.order = append(m.order, id)
	m.active = id
	return s
}

// sessionHarnessConfigLocked merges the current effective settings with
// the manager's memory hand-off (empty MemFile leaves memory disabled,
// unlike the REPL's agent/memory.md default - the web layer never wants
// to write into the repo tree implicitly). Callers hold m.mu.
func (m *SessionManager) sessionHarnessConfigLocked() harness.Config {
	cfg := m.store.Effective()
	if m.memFile != "" {
		cfg.Memory = &harness.Memory{Path: m.memFile}
	}
	return cfg
}

// Create adds a new session built from the current effective config and
// makes it active (the UI switches to what it just created).
func (m *SessionManager) Create(name string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.newSessionLocked(name)
}

// List returns every session in creation order, flagging the active one.
func (m *SessionManager) List() []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionInfo, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.sessions[id].Info(id == m.active))
	}
	return out
}

// Get returns the session with that id, or nil.
func (m *SessionManager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// Active returns the active session (never nil while the manager exists:
// Delete refuses to remove the last one).
func (m *SessionManager) Active() *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[m.active]
}

// ActiveID returns the active session's id.
func (m *SessionManager) ActiveID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// Switch makes id the active session; unknown ids error.
func (m *SessionManager) Switch(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; !ok {
		return fmt.Errorf("no such session %q", id)
	}
	m.active = id
	return nil
}

// Rename updates a session's display name (the persistence slug is
// recomputed from it at the next Save).
func (m *SessionManager) Rename(id, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("no such session %q", id)
	}
	if strings.TrimSpace(name) != "" {
		s.Name = name
	}
	return nil
}

// Delete removes a session. The last session cannot be deleted (the UI
// always has something to show); deleting the active session moves the
// active pointer to the first remaining one. The on-disk JSON file is
// left in place so Load can bring the conversation back.
func (m *SessionManager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; !ok {
		return fmt.Errorf("no such session %q", id)
	}
	if len(m.sessions) == 1 {
		return fmt.Errorf("refusing to delete the last session")
	}
	delete(m.sessions, id)
	for i, oid := range m.order {
		if oid == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	if m.active == id {
		m.active = m.order[0]
	}
	return nil
}

// Reset clears a session's conversation (Harness.Reset: config and tools
// kept) under the session mutex.
func (m *SessionManager) Reset(id string) error {
	s := m.Get(id)
	if s == nil {
		return fmt.Errorf("no such session %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.harness.Reset()
	return nil
}

// Post runs a turn on the active session (the router resolves which
// session the browser is talking to before calling, but this mirrors the
// single-conversation shape of the REPL).
func (m *SessionManager) Post(input string, images []string) (string, error) {
	return m.Active().Post(input, images)
}

// Cancel aborts the in-flight turn of the named session; it reports
// whether a turn was actually running.
func (m *SessionManager) Cancel(id string) bool {
	s := m.Get(id)
	if s == nil {
		return false
	}
	return s.Cancel()
}

// ---------------------------------------------------------------------------
// Persistence: agent/sessions/<slug>.json
// ---------------------------------------------------------------------------

// slugUnsafe matches anything that is not a-z, 0-9 (names are
// lower-cased first). Collapsing runs of unsafe characters to "-" kills
// path separators, dots, and control junk, so a crafted name can never
// traverse out of the sessions dir.
var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify turns a session name into a safe file stem; an all-unsafe name
// falls back to "session".
func slugify(name string) string {
	s := slugUnsafe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "session"
	}
	return s
}

// uniqueSlugLocked picks a slug for sess that no OTHER session's name
// would produce, so two sessions never share a file. Callers hold m.mu.
func (m *SessionManager) uniqueSlugLocked(sess *Session) string {
	taken := make(map[string]bool, len(m.order))
	for _, id := range m.order {
		if id != sess.ID {
			taken[slugify(m.sessions[id].Name)] = true
		}
	}
	base := slugify(sess.Name)
	slug := base
	for i := 2; taken[slug]; i++ {
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	return slug
}

// sessionPathLocked resolves the persistence file for a session.
// Callers hold m.mu.
func (m *SessionManager) sessionPathLocked(sess *Session) string {
	return filepath.Join(m.sessionsDir, m.uniqueSlugLocked(sess)+".json")
}

// Save writes the session's conversation to agent/sessions/<slug>.json
// (atomic write inside SaveSession; the dir is created on demand).
func (m *SessionManager) Save(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no such session %q", id)
	}
	path := m.sessionPathLocked(s)
	m.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.harness.SaveSession(path)
}

// Load reads a session file into an existing session, or into a NEW
// session when id is blank (the new session is named after the file and
// becomes active). The file may live anywhere the caller points; only
// the auto-created path is under the sessions dir.
func (m *SessionManager) Load(id, path string) (*Session, error) {
	m.mu.Lock()
	var s *Session
	if id == "" {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		s = m.newSessionLocked(name)
	} else {
		s = m.sessions[id]
	}
	m.mu.Unlock()

	if s == nil {
		return nil, fmt.Errorf("no such session %q", id)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.harness.LoadSession(path); err != nil {
		return nil, err
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Config propagation and registry surfaces
// ---------------------------------------------------------------------------

// ReconfigureAll pushes the current effective settings through every
// session's Harness.Reconfigure (conversation kept), building ONE fresh
// Client and handing the same pointer to all sessions so the
// single-shared-client invariant survives a settings change. The router
// calls this after a successful settings PUT; SettingsStore.HandlePut
// itself only reconfigures the startup harness it holds.
//
// Caveat (constraint 2 in the header): the fresh client swap and the
// MaxRetries push inside Reconfigure are not atomic with another
// session's in-flight turn; the router should call this when no turn is
// running, the same way the REPL only hot-reloads between prompts.
func (m *SessionManager) ReconfigureAll() {
	m.mu.Lock()
	eff := m.sessionHarnessConfigLocked()
	sessions := make([]*Session, 0, len(m.order))
	for _, id := range m.order {
		sessions = append(sessions, m.sessions[id])
	}
	m.mu.Unlock()

	client := harness.NewClient(eff.BaseURL, eff.APIKey)
	client.SetRequestTimeout(eff.RequestTimeout)

	m.regMu.Lock()
	defer m.regMu.Unlock()
	m.client = client
	for _, s := range sessions {
		s.mu.Lock()
		s.harness.Reconfigure(eff, client)
		s.mu.Unlock()
	}
}

// Tools lists every registered tool with its effective policy (the
// /tools equivalent). Read under regMu so it cannot observe a half
// applied ReloadTools.
func (m *SessionManager) Tools() []harness.ToolPolicy {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	return m.registry.Policies()
}

// SetPolicy assigns a tool's permission policy on the SHARED registry
// (every session sees it on its next tool call), serialized through
// regMu.
func (m *SessionManager) SetPolicy(name string, p harness.Policy) {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	m.registry.SetPolicy(name, p)
}

// ReloadTools re-reads config.yaml's tool-permissions lists and the
// custom Lua tool dir, applying both to the shared registry under regMu
// (the web equivalent of the REPL's per-prompt hot-reload block).
func (m *SessionManager) ReloadTools() {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	if m.conf != nil {
		applyWebToolPolicies(m.conf, m.registry)
	}
	if m.customToolDir != "" {
		reloadWebLuaTools(m.customToolDir, m.registry, m.luaModTimes)
	}
}

// LogDir returns the transcript log directory from the LIVE effective
// config (empty = logging disabled). This is what feeds the log viewer
// in web/logs.go.
func (m *SessionManager) LogDir() string {
	return m.store.Effective().LogDir
}

// applyWebToolPolicies is package web's copy of main.go's unexported
// applyToolPolicies (main.go is package main, not importable, and
// harness cannot import harness/config): config.yaml's allow/ask/deny
// lists plus the run_shell_command ask default unless explicitly listed
// under allow or deny. ApplyPolicies replaces the whole mapping, so
// names removed from config revert to allow on the next reload - and a
// UI SetPolicy change is overwritten by a reload, exactly like the REPL.
func applyWebToolPolicies(conf *config.Config, registry *harness.Registry) {
	allow, ask, deny := conf.ToolPermissions.Allow, conf.ToolPermissions.Ask, conf.ToolPermissions.Deny
	explicit := false
	for _, n := range append(append([]string{}, allow...), deny...) {
		if n == "run_shell_command" {
			explicit = true
			break
		}
	}
	if !explicit {
		ask = append(ask, "run_shell_command")
	}
	registry.ApplyPolicies(allow, ask, deny)
}

// reloadWebLuaTools is package web's copy of main.go's unexported
// reloadLuaTools: skip dirs and non-.lua files, re-register only files
// that are new or whose mtime moved, treat every failure as a stderr
// warning, never fatal. modTimes is the manager's map (main.go's is a
// package-level var there; here it hangs off the manager so two
// managers, e.g. tests, stay independent).
func reloadWebLuaTools(dir string, registry *harness.Registry, modTimes map[string]time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".lua" {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		lastMod, known := modTimes[path]
		if !known || info.ModTime().After(lastMod) {
			data, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[hot-reload error reading %s]: %v\n", entry.Name(), err)
				continue
			}

			tool, err := luatool.NewLuaToolFromScriptWithConfig(string(data), luatool.FilesystemConfig{BaseDir: "."})
			if err != nil {
				fmt.Fprintf(os.Stderr, "[hot-reload syntax error in %s]: %v\n", entry.Name(), err)
				continue
			}

			registry.Register(tool)
			modTimes[path] = info.ModTime()
		}
	}
}
