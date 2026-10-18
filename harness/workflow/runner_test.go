package workflow

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"ai-harness/harness"
)

// fakeServer emulates an OpenAI-compatible chat endpoint plus /v1/models.
// Chat replies are scripted: each request answers with the matching entry,
// and everything is recorded so tests can assert on the request bodies.
type fakeServer struct {
	t         *testing.T
	server    *httptest.Server
	requests  []map[string]any
	responses []string // chat replies, consumed in order
}

func newFakeServer(t *testing.T, replies ...string) *fakeServer {
	t.Helper()
	f := &fakeServer{t: t, responses: replies}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("fake server: bad request body: %v", err)
		}
		f.requests = append(f.requests, body)

		var reply string
		if len(f.responses) > 0 {
			reply = f.responses[0]
			f.responses = f.responses[1:]
		} else {
			reply = "default-reply"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": reply},
					"finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "model-a"},
				{"id": "model-b"},
			},
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// baseConfigFor points a Config at the fake server (openai-compat path).
func baseConfigFor(f *fakeServer) harness.Config {
	return harness.Config{
		Provider:          "openai",
		BaseURL:           f.server.URL + "/v1",
		Model:             "test-model",
		SystemPrompt:      "SYS",
		MaxToolIterations: 5,
		MaxContextTokens:  1000,
	}
}

// pngDir creates a workflow dir containing a small valid PNG.
func pngFile(t *testing.T, dir string, rel string) string {
	t.Helper()
	// Minimal 1x1 transparent PNG.
	const b64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
	return full
}

func TestRunnerStepsChainAndTemplate(t *testing.T) {
	f := newFakeServer(t, "FIRST", "SECOND")
	wf, err := Load(writeWorkflow(t, `
name: chain
vars:
  topic: hi
steps:
  - name: one
    prompt: "first {{topic}}"
  - name: two
    prompt: "second refs {{steps.one}}"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	r, err := New(wf, baseConfigFor(f), &out)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	if err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(f.requests) != 2 {
		t.Fatalf("expected 2 chat requests, got %d", len(f.requests))
	}
	// Second request's user message must contain the templated first output.
	msgs, _ := json.Marshal(f.requests[1]["messages"])
	if !bytes.Contains(msgs, []byte("second refs FIRST")) {
		t.Errorf("step two prompt not templated from step one: %s", msgs)
	}
	// Conversation history carries across steps: second request includes
	// the system prompt and the first exchange.
	if !bytes.Contains(msgs, []byte(`"SYS"`)) || !bytes.Contains(msgs, []byte("first hi")) {
		t.Errorf("expected shared history in step two: %s", msgs)
	}
	if r.stepOutputs["one"] != "FIRST" || r.stepOutputs["two"] != "SECOND" {
		t.Errorf("step outputs: %v", r.stepOutputs)
	}
}

func TestRunnerStepSystemPromptResetsHistory(t *testing.T) {
	f := newFakeServer(t, "A", "B")
	wf, err := Load(writeWorkflow(t, `
name: reset
steps:
  - name: one
    prompt: "hello"
  - name: two
    prompt: "again"
    system_prompt: "FRESH"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	r, _ := mustRunner(t, wf, f, &out)
	if err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs, _ := json.Marshal(f.requests[1]["messages"])
	if !bytes.Contains(msgs, []byte("FRESH")) || bytes.Contains(msgs, []byte("hello")) {
		t.Errorf("expected fresh conversation for step two: %s", msgs)
	}
}

func TestRunnerMediaSentAsContentParts(t *testing.T) {
	f := newFakeServer(t, "saw it")
	dir := writeWorkflow(t, `
name: media
steps:
  - name: look
    prompt: "what is this?"
    media: [img.png]
`)
	pngFile(t, dir, "img.png")
	wf, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	r, _ := mustRunner(t, wf, f, &out)
	if err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs, _ := json.Marshal(f.requests[0]["messages"])
	if !bytes.Contains(msgs, []byte("image_url")) || !bytes.Contains(msgs, []byte("data:image/png;base64,")) {
		t.Errorf("expected multimodal content part: %s", msgs)
	}
}

func TestRunnerMediaUnsupportedExtensionFails(t *testing.T) {
	f := newFakeServer(t, "x")
	dir := writeWorkflow(t, `
name: badmedia
steps:
  - name: look
    prompt: "hi"
    media: [doc.txt]
`)
	if err := os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	r, _ := mustRunner(t, wf, f, &out)
	if err := r.Run(context.Background(), nil); err == nil {
		t.Fatal("expected unsupported media extension to fail the step")
	}
}

func TestRunnerOnErrorSkip(t *testing.T) {
	// Server that 500s on the first request (step "doomed") and then
	// succeeds, so the run must continue to step "survivor".
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "SURVIVED"},
					"finish_reason": "stop"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	wf, err := Load(writeWorkflow(t, `
name: skip
steps:
  - name: doomed
    prompt: "hi"
    on_error: skip
  - name: survivor
    prompt: "still here"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	base := baseConfigFor(&fakeServer{t: t, server: srv})
	var out bytes.Buffer
	r, err := New(wf, base, &out)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()
	if err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("skip mode should not fail the run: %v", err)
	}
	if _, ok := r.stepOutputs["doomed"]; ok {
		t.Error("failed step should have no output recorded")
	}
	if r.stepOutputs["survivor"] != "SURVIVED" {
		t.Errorf("step after a skipped failure did not run: %v", r.stepOutputs)
	}
}

func TestRunnerLuaMain(t *testing.T) {
	f := newFakeServer(t, "LUA-REPLY")
	dir := writeWorkflow(t, "name: luamain\n")
	script := `
local models, err = harness.list_models()
if err then error("list_models: " .. err) end
assert(#models == 2, "expected 2 models, got " .. #models)
assert(models[1].id == "model-a")

local n, cerr = harness.context_length("model-a")
-- fake server exposes no context endpoints; expect a (nil, err) pair
if not cerr then error("context_length should have failed") end

local answer, aerr = harness.ask("hello {{topic}}", {system = "CUSTOM"})
if aerr then error("ask: " .. aerr) end
assert(answer == "LUA-REPLY", "got " .. tostring(answer))

local t2, jerr = json.decode('{"k": [1, 2]}')
if jerr then error("json.decode: " .. jerr) end
assert(t2.k[1] == 1)

harness.log("from lua, arg1 =", harness.args[1])
`
	if err := os.WriteFile(filepath.Join(dir, LuaMainName), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Add a var so we can confirm templating inside harness.ask.
	wf.Vars = map[string]string{"topic": "templated"}

	var out bytes.Buffer
	r, err := New(wf, baseConfigFor(f), &out)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()
	if err := r.Run(context.Background(), []string{"cli-arg"}); err != nil {
		t.Fatalf("Run: %v\noutput: %s", err, out.String())
	}
	if len(f.requests) != 1 {
		t.Fatalf("expected 1 chat request, got %d", len(f.requests))
	}
	msgs, _ := json.Marshal(f.requests[0]["messages"])
	if !bytes.Contains(msgs, []byte("hello templated")) {
		t.Errorf("ask prompt not templated: %s", msgs)
	}
	if !bytes.Contains(msgs, []byte("CUSTOM")) {
		t.Errorf("ask system option not applied: %s", msgs)
	}
	if !bytes.Contains(out.Bytes(), []byte("from lua, arg1 = cli-arg")) {
		t.Errorf("harness.log/args not visible: %s", out.String())
	}
}

func TestRunnerListModels(t *testing.T) {
	f := newFakeServer(t)
	client := harness.NewClient(f.server.URL+"/v1", "")
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "model-a" {
		t.Errorf("unexpected models: %+v", models)
	}
}

func mustRunner(t *testing.T, wf *Workflow, f *fakeServer, out io.Writer) (*Runner, error) {
	t.Helper()
	r, err := New(wf, baseConfigFor(f), out)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(r.Close)
	return r, nil
}
