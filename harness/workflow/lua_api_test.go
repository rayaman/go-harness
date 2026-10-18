package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// runLuaMainWith runs a workflow whose main.lua is the given script, with
// args passed through to harness.args. Returns captured stdout.
func runLuaMainWith(t *testing.T, f *fakeServer, script string, args []string) string {
	t.Helper()
	dir := writeWorkflow(t, "name: luaext\n")
	if err := os.WriteFile(filepath.Join(dir, LuaMainName), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out bytes.Buffer
	r, _ := mustRunner(t, wf, f, &out)
	if err := r.Run(context.Background(), args); err != nil {
		t.Fatalf("Run: %v\noutput: %s", err, out.String())
	}
	return out.String()
}

func TestExternalWriteReadOutsideSandbox(t *testing.T) {
	f := newFakeServer(t)
	outside := t.TempDir() // deliberately NOT inside the workflow dir

	script := `
local out, aerr = harness.external.abspath(harness.args[1])
if not out then error("abspath: " .. aerr) end

local msg, werr = harness.external.write_file(out .. "/nested/deep/hello.md", "hi from main.lua")
if not msg then error("write_file: " .. werr) end

local text, rerr = harness.external.read_file(out .. "/nested/deep/hello.md")
if not text then error("read_file: " .. rerr) end
assert(text == "hi from main.lua", "read back: " .. tostring(text))

local ok, eerr = harness.external.exists(out .. "/nested/deep/hello.md")
if not ok then error("exists said false: " .. tostring(eerr)) end
local missing, merr = harness.external.exists(out .. "/nope.md")
if missing or merr then error("missing path should report (false, nil)") end

local isdir, derr = harness.external.is_dir(out .. "/nested/deep")
if not isdir or derr then error("is_dir should be true") end
local isfile, ferr = harness.external.is_dir(out .. "/nested/deep/hello.md")
if isfile or ferr then error("is_dir on a file should be false") end

harness.log("external ok")
`
	out := runLuaMainWith(t, f, script, []string{outside})
	if !bytes.Contains([]byte(out), []byte("external ok")) {
		t.Fatalf("script did not finish: %s", out)
	}
	data, err := os.ReadFile(filepath.Join(outside, "nested", "deep", "hello.md"))
	if err != nil {
		t.Fatalf("file outside sandbox not written: %v", err)
	}
	if string(data) != "hi from main.lua" {
		t.Errorf("content = %q", data)
	}
}

func TestExternalListImagesFiltersAndSorts(t *testing.T) {
	f := newFakeServer(t)
	dir := t.TempDir()
	pngFile(t, dir, "b.png")
	pngFile(t, dir, "a.JPG") // uppercase extension must count
	pngFile(t, dir, filepath.Join("sub", "c.jpeg"))
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := `
local imgs, err = harness.external.list_images(harness.args[1])
if not imgs then error("list_images: " .. err) end
assert(#imgs == 3, "expected 3 images, got " .. #imgs)
for _, p in ipairs(imgs) do
   assert(p == p:gsub("^%s+", ""), "path should be absolute/clean: " .. p)
end
harness.log("count=" .. #imgs)
`
	out := runLuaMainWith(t, f, script, []string{dir})
	if !bytes.Contains([]byte(out), []byte("count=3")) {
		t.Fatalf("unexpected output: %s", out)
	}

	// Sorted order check (absolute paths, lexicographic).
	script2 := `
local imgs, err = harness.external.list_images(harness.args[1])
if not imgs then error("list_images: " .. err) end
for i = 2, #imgs do
   assert(imgs[i-1] < imgs[i], "not sorted: " .. imgs[i-1] .. " !< " .. imgs[i])
end
local sorted, serr = harness.external.list_images(harness.args[1] .. "/missing-dir")
assert(not sorted and serr, "missing dir should return (nil, err)")
harness.log("sorted ok")
`
	out2 := runLuaMainWith(t, f, script2, []string{dir})
	if !bytes.Contains([]byte(out2), []byte("sorted ok")) {
		t.Fatalf("sort assertions failed: %s", out2)
	}
}

func TestExternalSleep(t *testing.T) {
	f := newFakeServer(t)
	script := `
local _, serr = harness.external.sleep(0)
assert(not serr, "sleep(0) errored: " .. tostring(serr))
local _, nerr = harness.external.sleep(-1)
assert(nerr, "sleep(-1) should return an error")
harness.log("sleep ok: " .. tostring(nerr))
`
	out := runLuaMainWith(t, f, script, nil)
	if !bytes.Contains([]byte(out), []byte("sleep ok")) {
		t.Fatalf("sleep test did not finish: %s", out)
	}
}

func TestAskMediaAbsolutePathOutsideWorkflowDir(t *testing.T) {
	f := newFakeServer(t, "SEEN")
	outside := t.TempDir()
	img := pngFile(t, outside, "page.png") // absolute path, outside the workflow dir

	script := `
local text, err = harness.ask_media("what is on this page?", { harness.args[1] }, { reset = true })
if not text then error("ask_media: " .. err) end
assert(text == "SEEN", "got " .. tostring(text))
harness.log("media ok")
`
	out := runLuaMainWith(t, f, script, []string{img})
	if !bytes.Contains([]byte(out), []byte("media ok")) {
		t.Fatalf("script did not finish: %s", out)
	}
	if len(f.requests) != 1 {
		t.Fatalf("expected 1 chat request, got %d", len(f.requests))
	}
	msgs, _ := json.Marshal(f.requests[0]["messages"])
	if !bytes.Contains(msgs, []byte("data:image/png;base64,")) {
		t.Errorf("absolute out-of-dir image not attached: %s", msgs)
	}
}

// TestShippedMangaTranslatorWorkflow runs the real workflows/manga-translator
// against the fake server: two pages get one .md each outside the sandbox,
// the running summary.md is updated and injected into later prompts, and a
// re-run resumes by skipping already-translated pages.
func TestShippedMangaTranslatorWorkflow(t *testing.T) {
	wfDir := filepath.Join("..", "..", "workflows", "manga-translator")

	inDir := t.TempDir()
	pngFile(t, inDir, "page1.png")
	pngFile(t, inDir, "page2.png")
	outDir := t.TempDir()

	f := newFakeServer(t,
		"TRANS-PAGE-ONE", "SUMMARY-ONE", // page 1: translation, then summary update
		"TRANS-PAGE-TWO", "SUMMARY-TWO", // page 2: translation, then summary update
	)
	wf, err := Load(wfDir)
	if err != nil {
		t.Fatalf("Load shipped workflow: %v", err)
	}
	var out bytes.Buffer
	r, _ := mustRunner(t, wf, f, &out)
	if err := r.Run(context.Background(), []string{inDir, outDir, "Japanese", "English"}); err != nil {
		t.Fatalf("Run: %v\noutput: %s", err, out.String())
	}
	if len(f.requests) != 4 {
		t.Fatalf("expected 4 chat requests (2 pages x translate+summary), got %d\noutput: %s", len(f.requests), out.String())
	}

	// One .md per page, written outside the workflow sandbox.
	for name, want := range map[string]string{"page1.md": "TRANS-PAGE-ONE", "page2.md": "TRANS-PAGE-TWO"} {
		data, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("%s content = %q, want %q", name, data, want)
		}
	}
	// summary.md reflects the final summary update.
	if data, err := os.ReadFile(filepath.Join(outDir, "summary.md")); err != nil || !bytes.Contains(data, []byte("SUMMARY-TWO")) {
		t.Errorf("summary.md not updated: %q, %v", data, err)
	}
	// The second page's prompt must carry page 1's summary as context, and
	// carry an image part.
	msgs, _ := json.Marshal(f.requests[2]["messages"])
	if !bytes.Contains(msgs, []byte("SUMMARY-ONE")) {
		t.Errorf("page 2 prompt missing running summary context: %s", msgs)
	}
	if !bytes.Contains(msgs, []byte("image_url")) {
		t.Errorf("page 2 request missing image part: %s", msgs)
	}
	// First page's prompt must say there is no summary yet.
	msgs1, _ := json.Marshal(f.requests[0]["messages"])
	if !bytes.Contains(msgs1, []byte("no previous summary")) {
		t.Errorf("page 1 prompt should note empty summary: %s", msgs1)
	}

	// Resume: re-running with the same dirs makes zero model calls.
	f2 := newFakeServer(t)
	wf2, err := Load(wfDir)
	if err != nil {
		t.Fatalf("Load (resume): %v", err)
	}
	var out2 bytes.Buffer
	r2, _ := mustRunner(t, wf2, f2, &out2)
	if err := r2.Run(context.Background(), []string{inDir, outDir, "Japanese", "English"}); err != nil {
		t.Fatalf("Run (resume): %v\noutput: %s", err, out2.String())
	}
	if len(f2.requests) != 0 {
		t.Errorf("resume should skip translated pages, got %d requests", len(f2.requests))
	}
	if !bytes.Contains(out2.Bytes(), []byte("2 skipped")) {
		t.Errorf("resume output missing skip report: %s", out2.String())
	}
}
