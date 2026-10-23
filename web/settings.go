package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ai-harness/harness"
)

// ---------------------------------------------------------------------------
// Browser-editable settings (web-server roadmap, settings subtask)
//
// The startup preset (main.go's setupAgent -> web.Config.Base, see
// web/config.go) stays the source of truth for everything the browser
// never touches; WebSettings below is a thin override layer on top of it,
// persisted to DefaultSettingsPath so edits survive restarts. Apply merges
// the two, and a live PUT pushes the merged result through
// Harness.Reconfigure (harness/harness.go) without dropping the
// conversation - the same swap harness/reconfigure_test.go pins.
//
// SettingsStore is the dependency struct the server task constructs (its
// Serve in web/config.go is still the placeholder stub): it owns the
// persisted overrides, the startup base, the live Harness pointer, and a
// mutex guarding every mutation.
// ---------------------------------------------------------------------------

// DefaultSettingsPath is where the override layer persists, relative to
// the working directory (same convention as agent/config.yaml and
// agent/memory.md). Load/Save and NewSettingsStore take an explicit path
// so tests can point at a temp dir; this constant is only the default the
// server task passes.
const DefaultSettingsPath = "agent/web-settings.json"

// Route paths the settings handlers below serve. The server task mounts
// them (Serve in web/config.go is still the stub):
//
//	GET  /api/settings       - effective settings, API key masked
//	PUT  /api/settings       - replace the override layer, save, reconfigure
//	POST /api/settings/test  - probe a submitted endpoint without saving
const (
	RouteSettings     = "/api/settings"
	RouteSettingsTest = "/api/settings/test"
)

// WebSettings is the browser-editable override layer over the startup
// harness.Config.
//
// Convention (deliberate): the ZERO VALUE of every field means "no
// override - keep the startup preset value". Strings are unset at "",
// ints at 0, bools at false; the two fields whose meaningful range
// includes zero (Temperature, CompactThreshold) are *float64 so a user
// can legitimately set 0 through the UI. Apply, Redacted and the PUT
// handler all rely on this convention, so do not change a field's type
// without revisiting them. Two known consequences of the plain-zero
// convention: an override can never blank a preset's string ("" reads as
// unset, not "empty"), and MaxRetries cannot be forced to a preset's
// non-zero opposite from the UI - the startup preset owns it until the
// field model grows explicit presence bits. (StreamThinking is *bool
// precisely so the UI CAN force web-mode streaming back off again.)
//
// RequestTimeout is a Go duration string ("10m", "45s") rather than a
// time.Duration so the JSON round-trips human-editably; Apply parses it
// with time.ParseDuration and Validate reports an unparseable value.
// Memory is deliberately absent: persistent memory stays owned by
// main.go (see harness/memory.go), and Apply never touches the preset's
// Memory pointer.
type WebSettings struct {
	Provider         string   `json:"provider,omitempty"`
	BaseURL          string   `json:"base_url,omitempty"`
	APIKey           string   `json:"api_key,omitempty"`
	Model            string   `json:"model,omitempty"`
	SystemPrompt     string   `json:"system_prompt,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	MaxContextTokens int      `json:"max_context_tokens,omitempty"`

	MaxToolIterations  int    `json:"max_tool_iterations,omitempty"`
	MaxParallelTools   int    `json:"max_parallel_tools,omitempty"`
	MaxToolOutputBytes int    `json:"max_tool_output_bytes,omitempty"`
	MaxRetries         int    `json:"max_retries,omitempty"`
	MaxImageDimension  int    `json:"max_image_dimension,omitempty"`
	RequestTimeout     string `json:"request_timeout,omitempty"`
	StreamThinking     *bool  `json:"stream_thinking,omitempty"`

	CompactThreshold *float64 `json:"compact_threshold,omitempty"`
	CompactKeepTurns int      `json:"compact_keep_turns,omitempty"`
	LogDir           string   `json:"log_dir,omitempty"`
}

// Load reads the settings file from path. A missing file is NOT an error:
// no overrides saved yet is the normal first-run state, so the caller
// gets the zero value (which Apply treats as "preset unchanged"). A
// present-but-corrupt file does error, so the server can surface it
// instead of silently dropping the user's overrides.
func Load(path string) (WebSettings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return WebSettings{}, nil
		}
		return WebSettings{}, fmt.Errorf("read settings file %q: %w", path, err)
	}
	var s WebSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return WebSettings{}, fmt.Errorf("parse settings file %q: %w", path, err)
	}
	return s, nil
}

// Save writes the settings to path atomically, mirroring
// harness/session.go:SaveSession: the JSON goes to path+".tmp" first and
// is renamed over the target (parent directories created as needed), so
// an interrupted save can never corrupt the existing file and a failed
// rename removes the temp file.
func (s WebSettings) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create settings directory %q: %w", dir, err)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write settings temp file %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace settings file %q: %w", path, err)
	}
	return nil
}

// Apply returns a copy of base with ONLY the set overrides applied (see
// the zero-value convention on WebSettings). An unparseable
// RequestTimeout is ignored here so a corrupt file can never wedge the
// effective config; Validate reports it instead, and the PUT handler
// rejects it before saving.
//
// Memory is never touched: the merged config carries whatever Memory the
// preset had, so Reconfigure re-attaches the main.go-owned store (see
// harness/harness.go and harness/memory.go).
func (s WebSettings) Apply(base harness.Config) harness.Config {
	out := base
	if s.Provider != "" {
		out.Provider = s.Provider
	}
	if s.BaseURL != "" {
		out.BaseURL = s.BaseURL
	}
	if s.APIKey != "" {
		out.APIKey = s.APIKey
	}
	if s.Model != "" {
		out.Model = s.Model
	}
	if s.SystemPrompt != "" {
		out.SystemPrompt = s.SystemPrompt
	}
	if s.Temperature != nil {
		out.Temperature = *s.Temperature
	}
	if s.MaxContextTokens != 0 {
		out.MaxContextTokens = s.MaxContextTokens
	}
	if s.MaxToolIterations != 0 {
		out.MaxToolIterations = s.MaxToolIterations
	}
	if s.MaxParallelTools != 0 {
		out.MaxParallelTools = s.MaxParallelTools
	}
	if s.MaxToolOutputBytes != 0 {
		out.MaxToolOutputBytes = s.MaxToolOutputBytes
	}
	if s.MaxRetries != 0 {
		out.MaxRetries = s.MaxRetries
	}
	if s.MaxImageDimension != 0 {
		out.MaxImageDimension = s.MaxImageDimension
	}
	if s.RequestTimeout != "" {
		if d, err := time.ParseDuration(s.RequestTimeout); err == nil {
			out.RequestTimeout = d
		}
	}
	if s.StreamThinking != nil {
		out.StreamThinking = *s.StreamThinking
	}
	if s.CompactThreshold != nil {
		out.CompactThreshold = *s.CompactThreshold
	}
	if s.CompactKeepTurns != 0 {
		out.CompactKeepTurns = s.CompactKeepTurns
	}
	if s.LogDir != "" {
		out.LogDir = s.LogDir
	}
	return out
}

// Validate reports the settings the handlers refuse to save. Today that
// is exactly one thing: RequestTimeout must parse as a Go duration.
// Apply silently keeps the preset value for an unparseable timeout, so
// this is where the user learns about it (PUT returns 400).
func (s WebSettings) Validate() error {
	if s.RequestTimeout != "" {
		if _, err := time.ParseDuration(s.RequestTimeout); err != nil {
			return fmt.Errorf("invalid request_timeout %q: %w", s.RequestTimeout, err)
		}
	}
	return nil
}

// apiKeyMaskPrefix replaces the API key in every GET response. The mask
// keeps the last 4 characters as a fingerprint so the user can tell which
// key is stored without the value ever leaving the server.
const apiKeyMaskPrefix = "****"

// maskAPIKey renders the redacted form: "****" plus the last 4 chars of
// a long key, bare "****" for a short one, and "" for an unset key (an
// empty key must not mask to the sentinel, or "keep existing" would be
// ambiguous with "clear it").
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return apiKeyMaskPrefix
	}
	return apiKeyMaskPrefix + key[len(key)-4:]
}

// Redacted returns a copy with APIKey replaced by its masked form, for
// any response body that leaves the server. The PUT handler compares an
// incoming value against maskAPIKey of the CURRENT effective key: an
// exact match (or the bare sentinel for short keys) means the browser
// echoed GET's mask back, i.e. "keep the existing key", not "the key
// literally starts with ****".
func (s WebSettings) Redacted() WebSettings {
	s.APIKey = maskAPIKey(s.APIKey)
	return s
}

// mergedSettings renders the effective config (base + overrides) back
// into the flat WebSettings shape so GET/PUT responses show the values
// actually in effect, not just the overrides - the settings UI renders
// one form, and a later PUT of that form re-pins exactly what the user
// saw.
func mergedSettings(base harness.Config, s WebSettings) WebSettings {
	cfg := s.Apply(base)
	temperature := cfg.Temperature
	compactThreshold := cfg.CompactThreshold
	streamThinking := cfg.StreamThinking
	return WebSettings{
		Provider:           cfg.Provider,
		BaseURL:            cfg.BaseURL,
		APIKey:             cfg.APIKey,
		Model:              cfg.Model,
		SystemPrompt:       cfg.SystemPrompt,
		Temperature:        &temperature,
		MaxContextTokens:   cfg.MaxContextTokens,
		MaxToolIterations:  cfg.MaxToolIterations,
		MaxParallelTools:   cfg.MaxParallelTools,
		MaxToolOutputBytes: cfg.MaxToolOutputBytes,
		MaxRetries:         cfg.MaxRetries,
		MaxImageDimension:  cfg.MaxImageDimension,
		RequestTimeout:     cfg.RequestTimeout.String(),
		StreamThinking:     &streamThinking,
		CompactThreshold:   &compactThreshold,
		CompactKeepTurns:   cfg.CompactKeepTurns,
		LogDir:             cfg.LogDir,
	}
}

// settingsTestTimeout caps the reachability probe so a dead or
// firewalled endpoint can never hang the browser's settings page.
const settingsTestTimeout = 10 * time.Second

// SettingsStore owns the persisted override layer, the startup base it
// layers on, and the live Harness to reconfigure. Every mutation runs
// under mu, so concurrent PUTs can never interleave a save with a
// reconfigure. The server task constructs one via NewSettingsStore and
// mounts the Handle* methods on RouteSettings/RouteSettingsTest; h may be
// nil (unit tests), in which case PUT saves but skips Reconfigure.
type SettingsStore struct {
	mu       sync.Mutex
	path     string
	settings WebSettings
	base     harness.Config
	h        *harness.Harness
}

// NewSettingsStore loads the override layer from path (DefaultSettingsPath
// if empty) against the startup base and the live harness. The web.Config
// Serve stub in web/config.go is still the placeholder: the server task
// wires this constructor plus the handlers below into the router.
func NewSettingsStore(path string, base harness.Config, h *harness.Harness) (*SettingsStore, error) {
	if path == "" {
		path = DefaultSettingsPath
	}
	s, err := Load(path)
	if err != nil {
		return nil, err
	}
	return &SettingsStore{path: path, settings: s, base: base, h: h}, nil
}

// Effective returns the live merged config (preset + overrides) for the
// status route to report. It reads through Apply, so it always matches
// what the Harness was last reconfigured with.
func (st *SettingsStore) Effective() harness.Config {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.settings.Apply(st.base)
}

// HandleGet serves GET RouteSettings: the effective settings with the
// API key masked (Redacted), so the UI can render the form without the
// key ever crossing the wire in the clear.
func (st *SettingsStore) HandleGet(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	redacted := mergedSettings(st.base, st.settings).Redacted()
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, redacted)
}

// HandlePut serves PUT RouteSettings. The body REPLACES the whole
// override layer (zero fields = back to preset defaults), is validated
// before anything is written, then saved to disk and pushed through
// Harness.Reconfigure with a freshly built client for the new endpoint
// (harness.NewClient plus SetRequestTimeout; Reconfigure itself applies
// MaxRetries - see harness/harness.go). An incoming APIKey equal to the
// current mask means "keep the stored key". The response is the updated
// effective settings, masked, same shape as HandleGet.
func (st *SettingsStore) HandlePut(w http.ResponseWriter, r *http.Request) {
	var incoming WebSettings
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("parse settings body: %v", err)})
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	// Echoed mask -> keep the stored override value (which may itself be
	// empty, meaning "no override, use the preset's key").
	currentKey := st.settings.Apply(st.base).APIKey
	if incoming.APIKey != "" && incoming.APIKey == maskAPIKey(currentKey) {
		incoming.APIKey = st.settings.APIKey
	}

	if err := incoming.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := incoming.Save(st.path); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	st.settings = incoming

	eff := st.settings.Apply(st.base)
	client := harness.NewClient(eff.BaseURL, eff.APIKey)
	// Reconfigure handles MaxRetries but not the request timeout (that
	// is a client-level knob, see harness/client.go).
	client.SetRequestTimeout(eff.RequestTimeout)
	if st.h != nil {
		st.h.Reconfigure(eff, client)
	}
	writeJSON(w, http.StatusOK, mergedSettings(st.base, st.settings).Redacted())
}

// testResult is the POST RouteSettingsTest response: ok plus latency,
// with context_length on success and error on failure. HTTP status is
// 200 either way when the probe ran; the body is the result.
type testResult struct {
	OK            bool   `json:"ok"`
	LatencyMS     int64  `json:"latency_ms"`
	ContextLength int    `json:"context_length,omitempty"`
	Error         string `json:"error,omitempty"`
}

// HandleTest serves POST RouteSettingsTest: a throwaway reachability
// probe against the SUBMITTED provider/base_url/api_key/model, without
// saving or reconfiguring anything. It builds a scratch harness.Client
// and calls ContextLength (harness/client.go, which tries the
// models/props/Ollama native probes) under a settingsTestTimeout context
// so an unreachable endpoint fails the request in seconds, not minutes.
// Empty submitted fields fall back to the current effective values; the
// masked-sentinel rule for api_key applies here too so the UI's "test
// before saving" flow can reuse the GET response verbatim.
func (st *SettingsStore) HandleTest(w http.ResponseWriter, r *http.Request) {
	var submitted WebSettings
	if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("parse settings body: %v", err)})
		return
	}

	st.mu.Lock()
	eff := st.settings.Apply(st.base)
	if submitted.APIKey != "" && submitted.APIKey == maskAPIKey(eff.APIKey) {
		submitted.APIKey = st.settings.APIKey
	}
	st.mu.Unlock()

	merged := submitted.Apply(eff)
	client := harness.NewClient(merged.BaseURL, merged.APIKey)

	ctx, cancel := context.WithTimeout(r.Context(), settingsTestTimeout)
	defer cancel()

	start := time.Now()
	n, err := client.ContextLength(ctx, merged.Model)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		writeJSON(w, http.StatusOK, testResult{OK: false, LatencyMS: latency, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, testResult{OK: true, LatencyMS: latency, ContextLength: n})
}

// writeJSON is the package-wide response helper (auth.go's 401s use it
// too): fixed content type, marshaled body, error on marshal failure left
// to the net/http logger rather than half-writing a body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "warning: encode JSON response: %v\n", err)
	}
}
