package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ai-harness/harness"
	"ai-harness/harness/config"
)

// ---------------------------------------------------------------------------
// HTTP router (web-server roadmap, router subtask)
//
// Go 1.22+ net/http method+wildcard patterns only - no third-party
// router. newMux assembles the whole handler tree (public auth routes,
// the JSON API, the embedded SPA) and wraps it in auth.Middleware;
// Serve in web/config.go calls it and binds the listener, and tests call
// it directly with httptest so no real port is involved.
//
// Everything under /api/ answers JSON via the package-wide writeJSON
// helper (settings.go). Errors are {"error": "..."}; success bodies are
// the exact shapes the SPA is built against (see the per-route comments).
//
// SSE contract (both event streams):
//
//	Content-Type: text/event-stream, Cache-Control: no-cache,
//	Connection: keep-alive (+ X-Accel-Buffering: no so nginx-style
//	proxies do not buffer the stream away). Every frame is a single
//	"data: <JSON>\n\n" line followed by an immediate flush; a comment
//	frame ": ping\n\n" is sent every ssePingInterval so intermediaries
//	keep the connection open. The stream ends when the request context
//	is cancelled (client disconnect) or the source closes.
//
//	GET /api/sessions/{id}/events streams Event objects (sessions.go)
//	verbatim as data frames. Session.Subscribe queues a hello Event the
//	moment the stream opens, so the client learns the session id before
//	any turn output. A turn's output arrives on this stream after
//	POST /api/sessions/{id}/chat (which returns 202 immediately); on
//	"done" the client should re-fetch GET /api/sessions/{id} to
//	reconcile history against the documented slow-consumer drop policy
//	(sessions.go: a subscriber with a full buffer silently misses
//	events).
//
//	GET /api/logs/{name}/follow streams transcript lines WRAPPED as
//	{"type":"log","line":<LogLine>} frames - one wrapper shape for the
//	whole UI, so a single EventSource-style reader can distinguish log
//	frames from anything else a future stream might carry. LogLine is
//	the same struct the paged GET /api/logs/{name} returns.
// ---------------------------------------------------------------------------

// ssePingInterval is how often both SSE streams emit a ": ping" comment
// frame, comfortably below common proxy idle timeouts (and below the
// http.Server IdleTimeout in web/config.go).
const ssePingInterval = 20 * time.Second

// DefaultUploadsDir is where POST /api/upload stores images, following
// the same working-directory convention as agent/config.yaml.
// Config.UploadsDir (or this default) is what the handler uses; tests
// point it at a temp dir.
const DefaultUploadsDir = "agent/uploads"

// maxUploadBytes caps one uploaded image at ~20 MB; the request body is
// additionally capped with slack for the multipart framing itself.
const maxUploadBytes = 20 << 20

// mcpProbeTimeout bounds the tools/list liveness probe in the MCP
// status route so a wedged MCP server cannot hang the browser panel.
const mcpProbeTimeout = 5 * time.Second

// uploadExts is the image-extension whitelist POST /api/upload accepts.
var uploadExts = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".gif":  true,
	".webp": true,
}

// staticFS embeds the SPA assets. A LATER subtask replaces the file
// contents; these placeholders exist so the directive compiles and the
// static route is testable now.
//
//go:embed static
var staticFS embed.FS

// server bundles the state every handler needs. Constructed by newMux.
type server struct {
	cfg        Config
	auth       *auth
	store      *SettingsStore
	manager    *SessionManager
	uploadsDir string
}

// newMux builds the settings store, auth, session manager and the fully
// wrapped handler tree, WITHOUT binding a port. Serve (web/config.go)
// puts the returned handler on an http.Server; tests feed it to
// httptest directly. A corrupt saved-settings file is a stderr warning
// plus a zero-settings store, matching the repo's non-fatal-warning
// style - the UI should still start so the user can fix settings through
// the settings page.
func newMux(cfg Config) (http.Handler, error) {
	settingsPath := cfg.SettingsPath
	if settingsPath == "" {
		settingsPath = DefaultSettingsPath
	}
	store, err := NewSettingsStore(settingsPath, cfg.Base, cfg.Harness)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; continuing with no saved settings overrides\n", err)
		store = &SettingsStore{path: settingsPath, base: cfg.Base, h: cfg.Harness}
	}

	manager := NewSessionManager(cfg, store)
	if cfg.SessionsDir != "" {
		manager.sessionsDir = cfg.SessionsDir
	}

	uploadsDir := cfg.UploadsDir
	if uploadsDir == "" {
		uploadsDir = DefaultUploadsDir
	}

	s := &server{cfg: cfg, auth: newAuth(cfg), store: store, manager: manager, uploadsDir: uploadsDir}

	mux := http.NewServeMux()

	// Public routes (auth.Middleware's allowlist covers these paths).
	mux.HandleFunc("GET "+RouteLogin, s.auth.HandleLoginPage)
	mux.HandleFunc("POST "+RouteLogin, s.auth.HandleLogin)
	mux.HandleFunc("POST "+RouteLogout, s.auth.HandleLogout)
	mux.HandleFunc("GET /static/", s.handleStatic)

	// The SPA shell. "/" is the mux catch-all, so anything unmatched
	// (including unknown GETs) lands here and gets an explicit 404
	// unless the path is exactly "/".
	mux.HandleFunc("GET /", s.handleIndex)

	// Status.
	mux.HandleFunc("GET /api/status", s.handleStatus)

	// Sessions.
	mux.HandleFunc("GET /api/sessions", s.handleSessionsList)
	mux.HandleFunc("POST /api/sessions", s.handleSessionCreate)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSessionGet)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleSessionDelete)
	mux.HandleFunc("POST /api/sessions/{id}/switch", s.handleSessionSwitch)
	mux.HandleFunc("POST /api/sessions/{id}/rename", s.handleSessionRename)
	mux.HandleFunc("POST /api/sessions/{id}/chat", s.handleSessionChat)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleSessionEvents)
	mux.HandleFunc("POST /api/sessions/{id}/approve", s.handleSessionApprove)
	mux.HandleFunc("POST /api/sessions/{id}/cancel", s.handleSessionCancel)
	mux.HandleFunc("POST /api/sessions/{id}/reset", s.handleSessionReset)
	mux.HandleFunc("POST /api/sessions/{id}/compact", s.handleSessionCompact)
	mux.HandleFunc("POST /api/sessions/{id}/save", s.handleSessionSave)
	mux.HandleFunc("POST /api/sessions/{id}/load", s.handleSessionLoad)

	// Tools.
	mux.HandleFunc("GET /api/tools", s.handleToolsList)
	mux.HandleFunc("PUT /api/tools/policy", s.handleToolPolicy)
	mux.HandleFunc("POST /api/tools/reload", s.handleToolsReload)

	// MCP status.
	mux.HandleFunc("GET /api/mcp", s.handleMCPStatus)

	// Persistent memory.
	mux.HandleFunc("GET /api/memory", s.handleMemoryGet)
	mux.HandleFunc("POST /api/memory", s.handleMemoryPost)

	// Transcript logs.
	mux.HandleFunc("GET /api/logs", s.handleLogsList)
	mux.HandleFunc("GET /api/logs/{name}", s.handleLogsPage)
	mux.HandleFunc("GET /api/logs/{name}/follow", s.handleLogsFollow)

	// Settings (delegated to SettingsStore; PUT additionally pushes the
	// new config through every live session).
	mux.HandleFunc("GET "+RouteSettings, s.store.HandleGet)
	mux.HandleFunc("PUT "+RouteSettings, s.handleSettingsPut)
	mux.HandleFunc("POST "+RouteSettingsTest, s.store.HandleTest)

	// Image uploads for chat attachments.
	mux.HandleFunc("POST /api/upload", s.handleUpload)

	return s.auth.Middleware(mux), nil
}

// ---------------------------------------------------------------------------
// Static assets and the SPA shell
// ---------------------------------------------------------------------------

// handleIndex serves the embedded index.html for "/" only; anything else
// that fell through to this catch-all pattern is a 404.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedded index.html missing: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// handleStatic serves /static/* from the embed with content types from
// the file extension. no-cache (revalidate) rather than immutable: the
// SPA assets are replaced by a later subtask and dev iteration should
// never serve stale bytes. embed.FS.Open rejects non-clean paths, so a
// traversal attempt simply fails to open.
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || name == "/" {
		http.NotFound(w, r)
		return
	}
	f, err := staticFS.Open("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(strings.ToLower(pathpkg.Ext(name)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, info.ModTime(), rs)
		return
	}
	io.Copy(w, f)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// statusResponse is the GET /api/status body.
type statusResponse struct {
	Model           string        `json:"model"`
	Provider        string        `json:"provider"`
	BaseURL         string        `json:"base_url"`
	ContextTokens   int           `json:"context_tokens"`
	PromptTokens    int           `json:"prompt_tokens"`
	ContextPercent  float64       `json:"context_percent"`
	Sessions        []SessionInfo `json:"sessions"`
	ActiveSession   string        `json:"active_session"`
	ToolsCount      int           `json:"tools_count"`
	MCPServersCount int           `json:"mcp_servers_count"`
	MemoryFile      string        `json:"memory_file"`
	LogDir          string        `json:"log_dir"`
	LoggingEnabled  bool          `json:"logging_enabled"`
}

// handleStatus reports the effective config (store.Effective), the
// active session's last turn usage, and the session/tool/MCP counts.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	eff := s.store.Effective()
	usage := s.activeUsage()

	percent := 0.0
	if eff.MaxContextTokens > 0 {
		percent = float64(usage.PromptTokens) / float64(eff.MaxContextTokens) * 100
	}

	mcpCount := len(s.cfg.MCPClients)
	if s.cfg.Conf != nil && len(s.cfg.Conf.MCPServers) > 0 {
		mcpCount = len(s.cfg.Conf.MCPServers)
	}

	logDir := s.manager.LogDir()
	writeJSON(w, http.StatusOK, statusResponse{
		Model:           eff.Model,
		Provider:        eff.Provider,
		BaseURL:         eff.BaseURL,
		ContextTokens:   eff.MaxContextTokens,
		PromptTokens:    usage.PromptTokens,
		ContextPercent:  percent,
		Sessions:        s.manager.List(),
		ActiveSession:   s.manager.ActiveID(),
		ToolsCount:      len(s.manager.Tools()),
		MCPServersCount: mcpCount,
		MemoryFile:      s.cfg.MemFile,
		LogDir:          logDir,
		LoggingEnabled:  logDir != "",
	})
}

// activeUsage snapshots the active session's last turn Usage under the
// session mutex (Post writes lastUsage under that same lock).
func (s *server) activeUsage() harness.Usage {
	sess := s.manager.Active()
	if sess == nil {
		return harness.Usage{}
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.lastUsage
}

// ---------------------------------------------------------------------------
// Session routes
// ---------------------------------------------------------------------------

// handleSessionsList: GET /api/sessions -> {sessions, active}.
func (s *server) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": s.manager.List(),
		"active":   s.manager.ActiveID(),
	})
}

// handleSessionCreate: POST /api/sessions {name} -> {session}. The new
// session becomes active (manager.Create semantics).
func (s *server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse body: " + err.Error()})
		return
	}
	sess := s.manager.Create(body.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"session": sess.Info(true)})
}

// handleSessionGet: GET /api/sessions/{id} -> {session, history}.
func (s *server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.manager.Get(id)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no such session %q", id)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session": sess.Info(id == s.manager.ActiveID()),
		"history": sess.History(),
	})
}

// handleSessionDelete: DELETE /api/sessions/{id} -> {ok}. The manager
// refuses to remove the last session -> 400.
func (s *server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.manager.Get(id) == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no such session %q", id)})
		return
	}
	if err := s.manager.Delete(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSessionSwitch: POST /api/sessions/{id}/switch -> {ok}.
func (s *server) handleSessionSwitch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.manager.Switch(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSessionRename: POST /api/sessions/{id}/rename {name} -> {ok}.
func (s *server) handleSessionRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse body: " + err.Error()})
		return
	}
	if err := s.manager.Rename(id, body.Name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSessionChat: POST /api/sessions/{id}/chat {text, images} ->
// 202 {ok:true} immediately. The turn runs in a goroutine; every output
// arrives as Events on the session's SSE stream. Session.Post already
// broadcasts an error Event when the turn fails, so the handler must
// NOT emit one a second time.
func (s *server) handleSessionChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.manager.Get(id)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no such session %q", id)})
		return
	}
	var body struct {
		Text   string   `json:"text"`
		Images []string `json:"images"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse body: " + err.Error()})
		return
	}
	if body.Text == "" && len(body.Images) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text or images required"})
		return
	}

	go func() {
		// Post broadcasts the error Event itself on failure; there is
		// deliberately nothing else to report here.
		if _, err := sess.Post(body.Text, body.Images); err != nil {
			return
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// handleSessionEvents: GET /api/sessions/{id}/events -> the SSE stream
// of Event JSON objects (framing: see the contract block at the top of
// this file). Session.Subscribe queues the hello Event, so the client
// receives it as the first frame without any extra write here. The
// subscription is always removed when the request context is done.
func (s *server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.manager.Get(id)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no such session %q", id)})
		return
	}
	flusher, ok := writeSSEHeaders(w)
	if !ok {
		return
	}

	ch := sess.Subscribe()
	defer sess.Unsubscribe(ch)

	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return // unsubscribed elsewhere
			}
			data, err := json.Marshal(ev)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: marshal session event: %v\n", err)
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handleSessionApprove: POST /api/sessions/{id}/approve {call_id,
// approved} -> {ok:true}; 404 when that call id is not pending (stale
// prompt, already answered, or bogus id).
func (s *server) handleSessionApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.manager.Get(id)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no such session %q", id)})
		return
	}
	var body struct {
		CallID   string `json:"call_id"`
		Approved bool   `json:"approved"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse body: " + err.Error()})
		return
	}
	if !sess.ResolveApproval(body.CallID, body.Approved) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no pending approval for call id %q", body.CallID)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSessionCancel: POST /api/sessions/{id}/cancel -> {ok:true} when
// a turn was in flight, {ok:false} when the session was idle.
func (s *server) handleSessionCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.manager.Get(id)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no such session %q", id)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": sess.Cancel()})
}

// handleSessionReset: POST /api/sessions/{id}/reset -> {ok:true}.
func (s *server) handleSessionReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.manager.Reset(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSessionCompact: POST /api/sessions/{id}/compact -> 202. The
// summary turn runs in a goroutine that takes the session's own turn
// mutex (the same lock Post holds for a chat turn), so compaction can
// never interleave with a running conversation turn - the manager's
// turn-lock semantics are exactly what this needs, no 501 fallback.
// Progress rides the session's SSE stream: a "compacting" status Event
// first, then done (with the outcome in Text) or error.
func (s *server) handleSessionCompact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.manager.Get(id)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no such session %q", id)})
		return
	}

	go func() {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		sess.broadcast(Event{Type: EventStatus, Session: sess.ID, Message: "compacting"})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		changed, err := sess.harness.Compact(ctx)
		if err != nil {
			sess.broadcast(Event{Type: EventError, Message: fmt.Sprintf("compact failed: %v", err)})
			return
		}
		if !changed {
			sess.broadcast(Event{Type: EventDone, Session: sess.ID, Text: "nothing to compact"})
			return
		}
		sess.broadcast(Event{Type: EventDone, Session: sess.ID, Text: fmt.Sprintf("compacted history to %d messages", len(sess.harness.History))})
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// handleSessionSave: POST /api/sessions/{id}/save -> {ok:true, path}.
// savedPath resolves the same persistence file manager.Save writes to,
// so the UI learns where the conversation landed.
func (s *server) handleSessionSave(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, err := s.manager.savedPath(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err := s.manager.Save(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path})
}

// handleSessionLoad: POST /api/sessions/{id}/load {path} -> {session}.
// An unknown route id is a 404; a blank manager id would create a new
// active session, but every routed id is concrete, so this always loads
// into the named session.
func (s *server) handleSessionLoad(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Path string `json:"path"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse body: " + err.Error()})
		return
	}
	if body.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path required"})
		return
	}
	sess, err := s.manager.Load(id, body.Path)
	if err != nil {
		if s.manager.Get(id) == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess.Info(sess.ID == s.manager.ActiveID())})
}

// savedPath resolves the persistence file id's session saves to (the
// exact path Save writes, via the manager's own slug logic). Lives on
// the manager rather than the handler so it shares the sessions map
// lock.
func (m *SessionManager) savedPath(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return "", fmt.Errorf("no such session %q", id)
	}
	return m.sessionPathLocked(s), nil
}

// ---------------------------------------------------------------------------
// Tool routes
// ---------------------------------------------------------------------------

// toolPolicyView is one GET /api/tools row.
type toolPolicyView struct {
	Name   string `json:"name"`
	Policy string `json:"policy"`
}

// policyName renders a harness.Policy as the string the SPA uses
// (mirrors main.go's /tools output).
func policyName(p harness.Policy) string {
	switch p {
	case harness.PolicyAsk:
		return "ask"
	case harness.PolicyDeny:
		return "deny"
	default:
		return "allow"
	}
}

// policyForName parses the PUT /api/tools/policy policy string.
func policyForName(name string) (harness.Policy, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "allow":
		return harness.PolicyAllow, true
	case "ask":
		return harness.PolicyAsk, true
	case "deny":
		return harness.PolicyDeny, true
	default:
		return harness.PolicyAllow, false
	}
}

// handleToolsList: GET /api/tools -> {tools:[{name, policy}]}.
func (s *server) handleToolsList(w http.ResponseWriter, r *http.Request) {
	policies := s.manager.Tools()
	views := make([]toolPolicyView, 0, len(policies))
	for _, p := range policies {
		views = append(views, toolPolicyView{Name: p.Name, Policy: policyName(p.Policy)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": views})
}

// handleToolPolicy: PUT /api/tools/policy {name, policy} -> {ok:true}.
// The change lands on the SHARED registry (every session sees it on its
// next tool call); a later reload reverts it if config.yaml does not
// list the name, exactly like the REPL's hot-reload.
func (s *server) handleToolPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Policy string `json:"policy"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse body: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required"})
		return
	}
	p, ok := policyForName(body.Policy)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown policy %q (use allow, ask or deny)", body.Policy)})
		return
	}
	s.manager.SetPolicy(body.Name, p)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleToolsReload: POST /api/tools/reload -> {ok:true}. Lua
// hot-reload plus config.yaml policy reapply via the manager.
func (s *server) handleToolsReload(w http.ResponseWriter, r *http.Request) {
	s.manager.ReloadTools()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------
// MCP status
// ---------------------------------------------------------------------------

// mcpServerStatus is one GET /api/mcp row.
type mcpServerStatus struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Running bool     `json:"running"`
	Tools   []string `json:"tools"`
}

// handleMCPStatus derives the panel from cfg.Conf.MCPServers (the
// configured list), cfg.MCPClients (the live stdio clients main.go
// started) and, when pairing is unreliable, the registry attribution
// mcp.LoadTools performs (tools are registered under "<server>_<name>"
// only when the plain name collides, so only prefixed names are
// unambiguously attributable - documented below).
//
// Pairing caveat: setupAgent appends a client ONLY for servers that
// started, so when a server failed at boot the index correspondence
// between Conf.MCPServers and MCPClients breaks. The handler pairs by
// index only when the counts match, probes each client with a bounded
// tools/list for liveness, and otherwise degrades to the registry
// prefix heuristic plus a warning. MCPClients are closed by main.go,
// never here.
func (s *server) handleMCPStatus(w http.ResponseWriter, r *http.Request) {
	var configured []config.MCPServerConfig
	if s.cfg.Conf != nil {
		configured = s.cfg.Conf.MCPServers
	}
	clients := s.cfg.MCPClients
	paired := len(clients) == len(configured)

	warnings := []string{}
	if !paired && (len(clients) > 0 || len(configured) > 0) {
		warnings = append(warnings, fmt.Sprintf("%d configured MCP server(s) but %d live client(s): servers that failed to start at boot are skipped in both lists, so name pairing is unreliable", len(configured), len(clients)))
	}

	servers := make([]mcpServerStatus, 0, len(configured))
	ctx, cancel := context.WithTimeout(r.Context(), mcpProbeTimeout)
	defer cancel()

	for i, srv := range configured {
		st := mcpServerStatus{Name: srv.Name, Command: srv.Command, Tools: []string{}}
		if paired && i < len(clients) {
			infos, err := clients[i].ListTools(ctx)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("MCP server %q: tools/list probe failed: %v", srv.Name, err))
			} else {
				st.Running = true
				for _, info := range infos {
					st.Tools = append(st.Tools, info.Name)
				}
			}
		} else {
			// Fallback attribution: LoadTools prefixes a tool with
			// "<server>_" ONLY when its plain name collides, so this
			// sees collision-prefixed tools at best. Running is then
			// "at least one attributable tool is registered".
			prefix := srv.Name + "_"
			if s.cfg.Registry != nil {
				for _, p := range s.cfg.Registry.Policies() {
					if strings.HasPrefix(p.Name, prefix) {
						st.Tools = append(st.Tools, strings.TrimPrefix(p.Name, prefix))
					}
				}
			}
			st.Running = len(st.Tools) > 0
			if !st.Running {
				warnings = append(warnings, fmt.Sprintf("status unavailable for MCP server %q: live client pairing is ambiguous and only collision-prefixed registry tools are attributable", srv.Name))
			}
		}
		servers = append(servers, st)
	}

	writeJSON(w, http.StatusOK, map[string]any{"servers": servers, "warnings": warnings})
}

// ---------------------------------------------------------------------------
// Persistent memory
// ---------------------------------------------------------------------------

// handleMemoryGet: GET /api/memory -> {file, entries}. An unset memory
// file yields an empty listing rather than an error.
func (s *server) handleMemoryGet(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MemFile == "" {
		writeJSON(w, http.StatusOK, map[string]any{"file": "", "entries": []string{}})
		return
	}
	entries, err := (&harness.Memory{Path: s.cfg.MemFile}).Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if entries == nil {
		entries = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": s.cfg.MemFile, "entries": entries})
}

// handleMemoryPost: POST /api/memory {text} -> {ok:true, count}. The
// harness store dedupes exact-match entries; count is the entry total
// after the append attempt.
func (s *server) handleMemoryPost(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MemFile == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "memory disabled - set memory-file in config.yaml"})
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse body: " + err.Error()})
		return
	}
	mem := &harness.Memory{Path: s.cfg.MemFile}
	written, err := mem.Append(body.Text)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	entries, err := mem.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(entries), "written": written})
}

// ---------------------------------------------------------------------------
// Transcript log routes
// ---------------------------------------------------------------------------

// handleLogsList: GET /api/logs -> the TranscriptList from the LIVE
// effective log dir (manager.LogDir).
func (s *server) handleLogsList(w http.ResponseWriter, r *http.Request) {
	list, err := ListTranscripts(s.manager.LogDir())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleLogsPage: GET /api/logs/{name}?offset=N&limit=M -> a
// TranscriptPage. Unsafe names are 400, a missing file 404.
func (s *server) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	dir := s.manager.LogDir()
	if dir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": loggingDisabledReason})
		return
	}
	name := r.PathValue("name")
	if err := safeTranscriptName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil && r.URL.Query().Get("offset") != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid offset"})
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil && r.URL.Query().Get("limit") != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit"})
		return
	}
	page, err := ReadTranscript(dir, name, offset, limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// logSSEFrame is the follow-stream wrapper: every LogLine rides as
// {"type":"log","line":{...}} (the framing decision - see the SSE
// contract block at the top of this file).
type logSSEFrame struct {
	Type string  `json:"type"`
	Line LogLine `json:"line"`
}

// handleLogsFollow: GET /api/logs/{name}/follow -> SSE of logSSEFrame.
// The Follow tailer's own context is the request context, so a client
// disconnect stops the poller; the goroutine Follow started exits when
// the channel closes.
func (s *server) handleLogsFollow(w http.ResponseWriter, r *http.Request) {
	dir := s.manager.LogDir()
	if dir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": loggingDisabledReason})
		return
	}
	name := r.PathValue("name")
	if err := safeTranscriptName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	lines, err := Follow(r.Context(), dir, name)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}

	flusher, ok := writeSSEHeaders(w)
	if !ok {
		return
	}

	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case line, open := <-lines:
			if !open {
				return
			}
			data, err := json.Marshal(logSSEFrame{Type: EventLog, Line: line})
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: marshal log frame: %v\n", err)
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// Settings delegation
// ---------------------------------------------------------------------------

// handleSettingsPut delegates to SettingsStore.HandlePut and, on
// success, pushes the new effective config through every live session
// (manager.ReconfigureAll) - HandlePut itself only reconfigures the
// startup harness it holds. The status is captured through a thin
// wrapper so "successful PUT" means "the store wrote a 2xx".
func (s *server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	s.store.HandlePut(rec, r)
	if rec.status == 0 || rec.status < 300 {
		s.manager.ReconfigureAll()
	}
}

// statusRecorder remembers the first WriteHeader code so a delegated
// handler's outcome can drive follow-up work.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	if rec.status == 0 {
		rec.status = code
	}
	rec.ResponseWriter.WriteHeader(code)
}

// ---------------------------------------------------------------------------
// Uploads
// ---------------------------------------------------------------------------

// handleUpload: POST /api/upload multipart "file" field -> {path}. The
// file is saved under the uploads dir (<uploadsDir>/<unixnano>-<name>).
// Guards: the extension whitelist, a ~20 MB size cap enforced with
// http.MaxBytesReader (plus a header.Size check), and rejection of any
// filename containing a path separator, "..", or that is empty/dot -
// traversal never reaches the filesystem.
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("upload exceeds the %d MB limit", maxUploadBytes>>20)})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse multipart form: " + err.Error()})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `missing "file" form field`})
		return
	}
	defer file.Close()

	name := header.Filename
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid file name %q", name)})
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	if !uploadExts[ext] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file type not allowed (png, jpg, jpeg, gif or webp only)"})
		return
	}
	if header.Size > maxUploadBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("upload exceeds the %d MB limit", maxUploadBytes>>20)})
		return
	}

	if err := os.MkdirAll(s.uploadsDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	dst := filepath.Join(s.uploadsDir, fmt.Sprintf("%d-%s", time.Now().UnixNano(), name))
	out, err := os.Create(dst)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		os.Remove(dst)
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("upload exceeds the %d MB limit", maxUploadBytes>>20)})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write upload: " + err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": dst})
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// writeSSEHeaders sets the SSE headers (contract block at the top of
// this file), writes the 200 and flushes the headers. It reports the
// Flusher the caller needs for frame flushing, or false when the
// ResponseWriter cannot flush (then a 500 has been written).
func writeSSEHeaders(w http.ResponseWriter) (http.Flusher, bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return nil, false
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, true
}

// decodeJSONBody decodes r's JSON body into v. An empty body is NOT an
// error (all fields keep their zero values), so the UI may POST {} where
// every field is optional.
func decodeJSONBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}
