package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ai-harness/harness"
)

// ---------------------------------------------------------------------------
// Settings store tests (web/settings.go)
//
// Run via the compiled-binary workaround (go test -c -o web_pkg_test.exe
// ./web, then .\web_pkg_test.exe from the repo root): the pre-existing
// harness/config init panic on a missing config.yaml kills `go test
// ./...` from package dirs, not these tests.
// ---------------------------------------------------------------------------

// presetConfig is the startup harness.Config the tests layer overrides
// onto. Values are distinctive so a clobber is obvious in failures.
func presetConfig() harness.Config {
	return harness.Config{
		Provider:          "ollama",
		BaseURL:           "http://127.0.0.1:11434/v1",
		APIKey:            "sk-live-9999",
		Model:             "preset-model",
		SystemPrompt:      "preset persona",
		Temperature:       0.7,
		MaxToolIterations: 100,
		MaxContextTokens:  32768,
		MaxRetries:        2,
		RequestTimeout:    10 * time.Minute,
		CompactThreshold:  0.8,
		CompactKeepTurns:  6,
	}
}

// tempSettingsPath points a settings file inside t.TempDir().
func tempSettingsPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "web-settings.json")
}

func TestSettingsSaveLoadRoundTrip(t *testing.T) {
	path := tempSettingsPath(t)
	temp := 0.3
	threshold := 0.55
	streamTrue := true
	in := WebSettings{
		Model:              "round-trip-model",
		APIKey:             "sk-round-trip-key",
		Temperature:        &temp,
		MaxContextTokens:   12345,
		RequestTimeout:     "45s",
		StreamThinking:     &streamTrue,
		CompactThreshold:   &threshold,
		CompactKeepTurns:   3,
		LogDir:             filepath.Join("agent", "logs"),
		MaxImageDimension:  2048,
		MaxParallelTools:   6,
		MaxToolOutputBytes: 40000,
		MaxToolIterations:  20,
		MaxRetries:         4,
		Provider:           "openai",
		BaseURL:            "http://example.test/v1",
		SystemPrompt:       "round trip persona",
	}
	if err := in.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestLoadMissingFileIsZeroValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load of missing file: %v, want nil error", err)
	}
	if !reflect.DeepEqual(s, WebSettings{}) {
		t.Errorf("missing file gave %+v, want zero value", s)
	}
}

func TestApplyUnsetOverridesKeepPreset(t *testing.T) {
	base := presetConfig()
	if got := (WebSettings{}).Apply(base); !reflect.DeepEqual(got, base) {
		t.Errorf("empty overrides changed the preset:\n got %+v\nwant %+v", got, base)
	}

	// A partial layer changes only its own fields, and a *float64 at 0
	// (the meaningful zero) DOES override - the pointer convention.
	zero := 0.0
	s := WebSettings{Model: "swapped", Temperature: &zero}
	got := s.Apply(base)
	if got.Model != "swapped" {
		t.Errorf("Model = %q, want the override", got.Model)
	}
	if got.Temperature != 0 {
		t.Errorf("Temperature = %v, want the explicit 0 override", got.Temperature)
	}
	if got.Model == base.Model || got.BaseURL != base.BaseURL || got.MaxToolIterations != base.MaxToolIterations {
		t.Errorf("non-overridden fields changed: %+v", got)
	}
}

func TestApplyNeverTouchesMemory(t *testing.T) {
	base := presetConfig()
	base.Memory = &harness.Memory{Path: filepath.Join(t.TempDir(), "memory.md")}
	got := WebSettings{Model: "x"}.Apply(base)
	if got.Memory != base.Memory {
		t.Error("Apply replaced or cleared the preset's Memory pointer; memory stays owned by main.go")
	}
}

func TestApplyAndValidateRequestTimeout(t *testing.T) {
	base := presetConfig()

	got := WebSettings{RequestTimeout: "45s"}.Apply(base)
	if got.RequestTimeout != 45*time.Second {
		t.Errorf("RequestTimeout = %v, want 45s", got.RequestTimeout)
	}

	// Unparseable: Apply keeps the preset (a corrupt file must not wedge
	// the config) and Validate surfaces the problem instead.
	got = WebSettings{RequestTimeout: "bogus"}.Apply(base)
	if got.RequestTimeout != base.RequestTimeout {
		t.Errorf("unparseable timeout changed the preset: %v", got.RequestTimeout)
	}
	if err := (WebSettings{RequestTimeout: "bogus"}).Validate(); err == nil {
		t.Error("Validate accepted an unparseable request_timeout")
	}
	if err := (WebSettings{RequestTimeout: "45s"}).Validate(); err != nil {
		t.Errorf("Validate rejected a good duration: %v", err)
	}
}

func TestRedactionMasksKey(t *testing.T) {
	if got := (WebSettings{APIKey: "sk-secret-abcd"}).Redacted().APIKey; got != "****abcd" {
		t.Errorf("long key masked to %q, want **** + last 4", got)
	}
	if got := (WebSettings{APIKey: "abc"}).Redacted().APIKey; got != "****" {
		t.Errorf("short key masked to %q, want bare sentinel", got)
	}
	if got := (WebSettings{}).Redacted().APIKey; got != "" {
		t.Errorf("unset key masked to %q, want empty (must not collide with the sentinel)", got)
	}
}

// storeFor builds a SettingsStore over a temp file and a stub mux with
// the three routes mounted the way the server task will mount them.
func storeFor(t *testing.T, base harness.Config, h *harness.Harness) (*SettingsStore, *http.ServeMux) {
	t.Helper()
	st, err := NewSettingsStore(tempSettingsPath(t), base, h)
	if err != nil {
		t.Fatalf("NewSettingsStore: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+RouteSettings, st.HandleGet)
	mux.HandleFunc("PUT "+RouteSettings, st.HandlePut)
	mux.HandleFunc("POST "+RouteSettingsTest, st.HandleTest)
	return st, mux
}

func TestGetSettingsMasksStoredKey(t *testing.T) {
	_, mux := storeFor(t, presetConfig(), nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RouteSettings, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-live") {
		t.Errorf("GET leaked the API key: %s", body)
	}
	var got WebSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if got.APIKey != "****9999" {
		t.Errorf("APIKey = %q, want the masked sentinel + last 4", got.APIKey)
	}
	if got.Model != "preset-model" {
		t.Errorf("GET should report the EFFECTIVE config: Model = %q, want the preset value", got.Model)
	}
}

func TestPutSavesReconfiguresAndSentinelKeepsKey(t *testing.T) {
	path := tempSettingsPath(t)
	base := presetConfig()
	h := harness.New(base, harness.NewClient(base.BaseURL, base.APIKey), harness.NewRegistry())

	st, mux := func() (*SettingsStore, *http.ServeMux) {
		s, err := NewSettingsStore(path, base, h)
		if err != nil {
			t.Fatalf("NewSettingsStore: %v", err)
		}
		m := http.NewServeMux()
		m.HandleFunc("PUT "+RouteSettings, s.HandlePut)
		return s, m
	}()

	// PUT 1: the browser echoes GET's mask back as api_key -> the stored
	// key must survive untouched, while model/max_context_tokens apply.
	body := `{"model":"browser-model","max_context_tokens":8192,"api_key":"****9999"}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, RouteSettings, strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if h.Config.Model != "browser-model" || h.Config.MaxContextTokens != 8192 {
		t.Errorf("live harness not reconfigured: model=%q ctx=%d", h.Config.Model, h.Config.MaxContextTokens)
	}
	if eff := st.Effective(); eff.APIKey != "sk-live-9999" {
		t.Errorf("masked sentinel clobbered the key: effective APIKey = %q", eff.APIKey)
	}
	if saved, err := Load(path); err != nil {
		t.Fatalf("Load after PUT: %v", err)
	} else if saved.Model != "browser-model" {
		t.Errorf("saved settings = %+v, want the override persisted", saved)
	}
	if resp := rec.Body.String(); strings.Contains(resp, "sk-live") || !strings.Contains(resp, "****9999") {
		t.Errorf("PUT response leaked or mis-masked the key: %s", resp)
	}

	// PUT 2: a real key change goes through. PUT REPLACES the whole
	// override layer (documented HandlePut contract), so this body pins
	// only the key and model falls back to the preset again.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, RouteSettings,
		strings.NewReader(`{"api_key":"sk-brand-new-777"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 2 status = %d: %s", rec.Code, rec.Body.String())
	}
	if eff := st.Effective(); eff.APIKey != "sk-brand-new-777" {
		t.Errorf("effective APIKey = %q, want the submitted one", eff.APIKey)
	}
	if eff := st.Effective(); eff.Model != "preset-model" {
		t.Errorf("effective Model = %q, want the preset after the replacing PUT", eff.Model)
	}
	if resp := rec.Body.String(); strings.Contains(resp, "brand") {
		t.Errorf("PUT 2 response leaked the new key: %s", resp)
	}

	// PUT with an invalid timeout: 400, nothing saved, harness untouched
	// (state is exactly what PUT 2 left: preset model, new key).
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, RouteSettings,
		strings.NewReader(`{"model":"should-not-stick","request_timeout":"bogus"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid-timeout PUT status = %d, want 400", rec.Code)
	}
	if h.Config.Model != "preset-model" {
		t.Errorf("rejected PUT reconfigured the harness model to %q", h.Config.Model)
	}
	if saved, _ := Load(path); saved.Model != "" || saved.APIKey != "sk-brand-new-777" {
		t.Errorf("rejected PUT changed the saved file: %+v", saved)
	}
}

func TestSettingsTestEndpoint(t *testing.T) {
	// Stand-in model server: OpenAI-style /v1/models with n_ctx, the
	// first probe Client.ContextLength tries (harness/client.go).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"probe-model","meta":{"n_ctx":4096}}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, mux := storeFor(t, presetConfig(), nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteSettingsTest,
		strings.NewReader(`{"base_url":"`+srv.URL+`/v1","model":"probe-model"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("test endpoint status = %d: %s", rec.Code, rec.Body.String())
	}
	var res testResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode test response: %v", err)
	}
	if !res.OK || res.ContextLength != 4096 || res.Error != "" {
		t.Errorf("probe against live stub = %+v, want ok with context_length 4096", res)
	}

	// Dead endpoint: refused instantly, reported as ok:false with an
	// error, still HTTP 200 so the browser can render the reason.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteSettingsTest,
		strings.NewReader(`{"base_url":"http://127.0.0.1:1/v1","model":"probe-model"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("dead-endpoint status = %d, want 200", rec.Code)
	}
	res = testResult{}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode dead-endpoint response: %v", err)
	}
	if res.OK || res.Error == "" {
		t.Errorf("dead endpoint gave %+v, want ok:false with an error", res)
	}
}

func TestNewSettingsStoreDefaultPathAndCorruptFile(t *testing.T) {
	// DefaultSettingsPath only matters as the server task's argument;
	// prove the constant is where the roadmap says, without touching it.
	// Compared slash-separated (the repo writes agent/ paths with "/" in
	// constants; filepath.Join would produce backslashes on Windows).
	if DefaultSettingsPath != "agent/web-settings.json" {
		t.Errorf("DefaultSettingsPath = %q", DefaultSettingsPath)
	}

	path := tempSettingsPath(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSettingsStore(path, presetConfig(), nil); err == nil {
		t.Error("NewSettingsStore accepted a corrupt settings file")
	}
}
