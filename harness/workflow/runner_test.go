package workflow

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// sseFakeServer emulates an OpenAI-compatible chat endpoint that answers
// with an SSE stream: reasoning deltas first, then content deltas, then
// [DONE]. It records each request's "stream" flag so tests can assert the
// client actually opted into streaming.
func sseFakeServer(t *testing.T) (*httptest.Server, *[]bool) {
	t.Helper()
	var streamFlags []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("sse fake: bad request body: %v", err)
			}
			streamFlags = append(streamFlags, body["stream"] == true)

			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			chunks := []map[string]any{
				{"choices": []map[string]any{{"delta": map[string]any{"role": "assistant"}}}},
				{"choices": []map[string]any{{"delta": map[string]any{"reasoning_content": "Hmm, "}}}},
				{"choices": []map[string]any{{"delta": map[string]any{"reasoning_content": "let me think.\nSecond thought."}}}},
				{"choices": []map[string]any{{"delta": map[string]any{"content": "Hello "}}}},
				{"choices": []map[string]any{{"delta": map[string]any{"content": "world"}, "finish_reason": "stop"}}},
				{"usage": map[string]int{"prompt_tokens": 7, "completion_tokens": 4, "total_tokens": 11}},
			}
			for _, c := range chunks {
				b, _ := json.Marshal(c)
				fmt.Fprintf(w, "data: %s\n\n", b)
				if flusher != nil {
					flusher.Flush()
				}
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "model-a"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &streamFlags
}

func TestRunnerStreamThinking(t *testing.T) {
	srv, streamFlags := sseFakeServer(t)
	wf, err := Load(writeWorkflow(t, `
name: st
config:
  stream_thinking: true
steps:
  - name: think
    prompt: "ponder this"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	base := harness.Config{
		Provider:          "openai",
		BaseURL:           srv.URL + "/v1",
		Model:             "test-model",
		SystemPrompt:      "SYS",
		MaxToolIterations: 5,
		MaxContextTokens:  1000,
	}
	var out bytes.Buffer
	r, err := New(wf, base, &out)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	if err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v\noutput: %s", err, out.String())
	}
	if len(*streamFlags) != 1 || !(*streamFlags)[0] {
		t.Errorf("expected one chat request with stream=true, got %v", *streamFlags)
	}
	got := out.String()
	// Thinking deltas must appear live, line-prefixed, split on newlines.
	if !strings.Contains(got, "[thinking] Hmm, let me think.") ||
		!strings.Contains(got, "[thinking] Second thought.") {
		t.Errorf("thinking deltas missing from output:\n%s", got)
	}
	// The step result is still the full accumulated content, printed once
	// at end-of-step (streamed content must not double-print).
	if r.stepOutputs["think"] != "Hello world" {
		t.Errorf("step result: got %q, want %q", r.stepOutputs["think"], "Hello world")
	}
	if strings.Count(got, "Hello world") != 1 {
		t.Errorf("streamed content should print exactly once, output:\n%s", got)
	}
	// Thinking lines must precede the result line.
	iThink := strings.Index(got, "[thinking] Hmm,")
	iResult := strings.Index(got, "Hello world\n")
	if iThink < 0 || iResult < 0 || iThink > iResult {
		t.Errorf("expected thinking before result, output:\n%s", got)
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

// -----------------------------------------------------------------------------
// Structured output (roadmap 2.5)
// -----------------------------------------------------------------------------

// lastUserContent returns the content of the last message in request i of
// the fake server, as a string (suitable for substring assertions).
func lastUserContent(f *fakeServer, i int) string {
	msgs, _ := f.requests[i]["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	s, _ := last["content"].(string)
	return s
}

func TestStepSchemaSuccess(t *testing.T) {
	f := newFakeServer(t, `{"title": "Hello"}`, "done")
	wf, err := Load(writeWorkflow(t, `
name: structok
steps:
  - name: s1
    prompt: Extract
    save_as: extr
    schema:
      type: object
      required: [title]
      properties:
        title:
          type: string
  - name: s2
    prompt: "title is {{steps.s1.title}}"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	r, _ := mustRunner(t, wf, f, &out)
	if err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v\noutput: %s", err, out.String())
	}
	if len(f.requests) != 2 {
		t.Fatalf("expected 2 chat requests, got %d", len(f.requests))
	}
	// First prompt must carry the schema instructions suffix.
	if !strings.Contains(lastUserContent(f, 0), "Respond with ONLY a single valid JSON value") {
		t.Errorf("schema instructions missing from step prompt: %s", lastUserContent(f, 0))
	}
	// Raw text recorded as today, decoded value recorded alongside (plus
	// the save_as alias).
	if r.stepOutputs["s1"] != `{"title": "Hello"}` {
		t.Errorf("raw step output: %q", r.stepOutputs["s1"])
	}
	decoded, ok := r.stepJSON["s1"].(map[string]any)
	if !ok || decoded["title"] != "Hello" {
		t.Errorf("decoded step JSON: %#v", r.stepJSON["s1"])
	}
	if d2, ok := r.stepJSON["extr"].(map[string]any); !ok || d2["title"] != "Hello" {
		t.Errorf("save_as alias decoded JSON: %#v", r.stepJSON["extr"])
	}
	// Later step's server-side prompt resolves the nested field.
	if !strings.Contains(lastUserContent(f, 1), "title is Hello") {
		t.Errorf("{{steps.s1.title}} not resolved: %s", lastUserContent(f, 1))
	}
}

func TestStepSchemaRetry(t *testing.T) {
	f := newFakeServer(t, "not json at all", `{"title": "Hello"}`)
	wf, err := Load(writeWorkflow(t, `
name: retry
steps:
  - name: s1
    prompt: Extract
    schema:
      type: object
      required: [title]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	r, _ := mustRunner(t, wf, f, &out)
	if err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v\noutput: %s", err, out.String())
	}
	if len(f.requests) != 2 {
		t.Fatalf("expected exactly 2 model requests for the step, got %d", len(f.requests))
	}
	retry := lastUserContent(f, 1)
	if !strings.Contains(retry, "not valid") {
		t.Errorf("retry prompt missing \"not valid\": %s", retry)
	}
	if !strings.Contains(retry, "Reply again with ONLY the JSON value.") {
		t.Errorf("retry prompt missing final instruction: %s", retry)
	}
	// First reply isn't JSON at all, so the problem text reports the parse failure.
	if !strings.Contains(retry, "reply was not valid JSON") {
		t.Errorf("retry prompt missing parse problem text: %s", retry)
	}
	if r.stepOutputs["s1"] != `{"title": "Hello"}` {
		t.Errorf("step output after retry: %q", r.stepOutputs["s1"])
	}
}

func TestStepSchemaFailsAfterRetry(t *testing.T) {
	f := newFakeServer(t, "not json", "also not json", "OK")
	wf, err := Load(writeWorkflow(t, `
name: structfail
steps:
  - name: s1
    prompt: Extract
    on_error: skip
    schema:
      type: object
      required: [title]
  - name: s2
    prompt: continue
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	r, _ := mustRunner(t, wf, f, &out)
	// on_error: skip -> the run keeps going past the failing step.
	if err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("on_error: skip should not fail the run: %v", err)
	}
	if len(f.requests) != 3 {
		t.Fatalf("expected 2 failed requests + 1 for step two, got %d", len(f.requests))
	}
	if !strings.Contains(out.String(), "reply failed schema validation after one retry") {
		t.Errorf("skip message missing validation error: %s", out.String())
	}
	if _, ok := r.stepOutputs["s1"]; ok {
		t.Error("failed step should record no output")
	}
	if _, ok := r.stepJSON["s1"]; ok {
		t.Error("failed step should record no decoded value")
	}
	if r.stepOutputs["s2"] != "OK" {
		t.Errorf("workflow did not continue after skipped schema failure: %v", r.stepOutputs)
	}
}

func TestStepJSONFieldTemplating(t *testing.T) {
	f := newFakeServer(t, `{"a": {"b": 42}, "meta": {"x": 1, "y": "z"}}`)
	wf, err := Load(writeWorkflow(t, `
name: tmpl
steps:
  - name: s
    prompt: Extract
    schema:
      type: object
      properties:
        a:
          type: object
          properties:
            b:
              type: integer
        meta:
          type: object
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	r, _ := mustRunner(t, wf, f, &out)
	if err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v\noutput: %s", err, out.String())
	}

	got := r.render("{{steps.s.a.b}}|{{s.a.b}}|{{steps.s.meta}}|{{steps.s}}|{{steps.s.a.nope}}|{{steps.s.bogus.deep}}")
	want := `42|42|{"x":1,"y":"z"}|{"a": {"b": 42}, "meta": {"x": 1, "y": "z"}}|{{steps.s.a.nope}}|{{steps.s.bogus.deep}}`
	if got != want {
		t.Errorf("render:\n got %q\nwant %q", got, want)
	}

	// Scalar rendering helper: integral float64 prints plain (42, not
	// 4.2e+01), non-integral keeps shortest form.
	if renderJSONValue(42.0) != "42" {
		t.Errorf("renderJSONValue(42.0) = %q", renderJSONValue(42.0))
	}
	if renderJSONValue(4.25) != "4.25" {
		t.Errorf("renderJSONValue(4.25) = %q", renderJSONValue(4.25))
	}
}
