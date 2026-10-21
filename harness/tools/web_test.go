package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// webArgs marshals a tool argument map for Execute.
func webArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal web args: %v", err)
	}
	return b
}

// htmlEntities are written as Go \x26 escapes (and reassembled here) so the
// entity literals survive verbatim in this source file.
var (
	ampEntity  = "\x26amp;"
	aposEntity = "\x26#39;"
)

// TestWebFetchSchemeRejected: non-http(s) schemes are refused with an error
// naming the scheme, before any name resolution or request happens.
func TestWebFetchSchemeRejected(t *testing.T) {
	tool := WebFetchTool{}

	for _, tc := range []struct{ url, scheme string }{
		{"file:///etc/passwd", "file"},
		{"ftp://x/", "ftp"},
	} {
		_, err := tool.Execute(webArgs(t, map[string]any{"url": tc.url}))
		if err == nil {
			t.Fatalf("Execute(%q): want scheme error, got nil", tc.url)
		}
		if !strings.Contains(err.Error(), tc.scheme) {
			t.Errorf("Execute(%q) error = %q, want it to name scheme %q", tc.url, err, tc.scheme)
		}
	}
}

// TestWebFetchSSRFBlocked: with AllowLocal false (the default), a host that
// resolves to a loopback/private address is refused; with AllowLocal true
// the same server is fetched (that is exactly what the flag is for).
func TestWebFetchSSRFBlocked(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("hello from local"))
	}))
	defer srv.Close()

	blocked := WebFetchTool{}
	_, err := blocked.Execute(webArgs(t, map[string]any{"url": srv.URL}))
	if err == nil {
		t.Fatal("AllowLocal=false: want SSRF error, got nil")
	}
	if !strings.Contains(err.Error(), "local/private address") {
		t.Errorf("error = %q, want it to mention local/private address", err)
	}
	if hits != 0 {
		t.Errorf("SSRF-blocked fetch made %d requests, want 0", hits)
	}

	// A literal "localhost" hostname is refused the same way (no DNS).
	if _, err := blocked.Execute(webArgs(t, map[string]any{"url": "http://localhost:1/"})); err == nil ||
		!strings.Contains(err.Error(), "local/private address") {
		t.Errorf("localhost fetch error = %v, want local/private address rejection", err)
	}

	// AllowLocal opts out - the same URL then fetches fine.
	allowed := WebFetchTool{AllowLocal: true}
	out, err := allowed.Execute(webArgs(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("AllowLocal=true: Execute: %v", err)
	}
	if !strings.Contains(out, "hello from local") || hits == 0 {
		t.Errorf("AllowLocal=true: output = %q, hits = %d, want the body fetched", out, hits)
	}
}

// TestWebFetchExtractsText: HTML is flattened to visible text - script and
// style contents vanish, block tags become newlines, entities unescape, and
// the provenance header leads the output.
func TestWebFetchExtractsText(t *testing.T) {
	// "&" + "amp;" and "&" + "#39;" are assembled from the entity vars so
	// the literals survive verbatim in this source.
	page := "<!DOCTYPE html>\n<html>\n<head><title>Page Title</title></head>\n<body>\n" +
		"<script>var secretScriptToken = \"SHOULD_NOT_APPEAR\";</script>\n" +
		"<style>.secretStyleToken { color: red; }</style>\n" +
		"<h1>Welcome</h1>\n" +
		"<p>Alpha line one " + ampEntity + " continued.</p>\n" +
		"<br>\n" +
		"<div>Second div \x26quot;quoted\x26quot;</div>\n" +
		"<ul><li>Item A</li><li>Item B</li></ul>\n" +
		"<p>It's fine</p>\n" +
		"</body>\n</html>\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	}))
	defer srv.Close()

	tool := WebFetchTool{AllowLocal: true}
	out, err := tool.Execute(webArgs(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(out, "fetched "+srv.URL+" (text/html, ") {
		t.Errorf("output = %q, want it to start with the provenance header", out)
	}
	for _, want := range []string{"Welcome", "Alpha line one & continued.", "Second div \"quoted\"", "Item A", "Item B"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"SHOULD_NOT_APPEAR", "secretStyleToken", "Page Title", "<"} {
		if strings.Contains(out, notWant) {
			t.Errorf("output contains %q, want it stripped; got:\n%s", notWant, out)
		}
	}
}

// TestWebFetchRawMode: extract_text:false returns the raw body, markup and all.
func TestWebFetchRawMode(t *testing.T) {
	body := "<html><body><p>RAW_TOKEN <b>bold</b></p></body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	tool := WebFetchTool{AllowLocal: true}
	out, err := tool.Execute(webArgs(t, map[string]any{"url": srv.URL, "extract_text": false}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, body) {
		t.Errorf("output = %q, want the raw HTML body %q", out, body)
	}
}

// TestWebFetchNon2xx: a 404 comes back as an error string naming the status.
func TestWebFetchNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	tool := WebFetchTool{AllowLocal: true}
	_, err := tool.Execute(webArgs(t, map[string]any{"url": srv.URL + "/missing"}))
	if err == nil {
		t.Fatal("want error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, want it to contain 404", err)
	}
}

// TestWebFetchBinaryGuard: a non-text content type yields a descriptive
// line instead of binary garbage.
func TestWebFetchBinaryGuard(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	}))
	defer srv.Close()

	tool := WebFetchTool{AllowLocal: true}
	out, err := tool.Execute(webArgs(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "not fetched as text") || !strings.Contains(out, "image/png") {
		t.Errorf("output = %q, want the binary guard line", out)
	}
	if strings.Contains(out, string(png)) {
		t.Errorf("output leaked raw binary bytes: %q", out)
	}
}

// TestWebFetchTruncates: a body larger than MaxBytes comes back cut at the
// cap with the truncation marker, and MaxOutputBytes agrees with MaxBytes.
func TestWebFetchTruncates(t *testing.T) {
	body := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	tool := WebFetchTool{AllowLocal: true, MaxBytes: 128}
	if tool.MaxOutputBytes() != 128 {
		t.Errorf("MaxOutputBytes() = %d, want 128", tool.MaxOutputBytes())
	}
	out, err := tool.Execute(webArgs(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "...[truncated]") {
		t.Errorf("output missing truncation marker: %q", out)
	}
	// The header line stays whole; the text part stays near the cap.
	_, textPart, found := strings.Cut(out, "\n")
	if !found {
		t.Fatalf("output has no header line: %q", out)
	}
	if len(textPart) > 128+len("...[truncated]") {
		t.Errorf("text part is %d bytes, want it capped near 128", len(textPart))
	}
}

// fakeBackend is a test-local SearchBackend recording its invocations.
type fakeBackend struct {
	results  []SearchResult
	calls    int
	gotQuery string
	gotMax   int
}

func (f *fakeBackend) Search(ctx context.Context, client *http.Client, query string, maxResults int, allowLocal bool) ([]SearchResult, error) {
	f.calls++
	f.gotQuery = query
	f.gotMax = maxResults
	return f.results, nil
}

// TestWebSearchFormatsResults: results render as a numbered list with title,
// URL and snippet; MaxResults both passes through to the backend and trims
// an overlong result set.
func TestWebSearchFormatsResults(t *testing.T) {
	fake := &fakeBackend{results: []SearchResult{
		{Title: "Alpha", URL: "https://a.example/1", Snippet: "first result"},
		{Title: "Beta", URL: "https://b.example/2", Snippet: "second result"},
		{Title: "Gamma", URL: "https://c.example/3", Snippet: "third result"},
		{Title: "Delta", URL: "https://d.example/4", Snippet: "fourth result"},
		{Title: "Eps", URL: "https://e.example/5", Snippet: "fifth result"},
	}}
	tool := WebSearchTool{Backend: fake, MaxResults: 2, AllowLocal: true}
	if tool.MaxOutputBytes() != 4096 {
		t.Errorf("MaxOutputBytes() = %d, want 4096", tool.MaxOutputBytes())
	}

	out, err := tool.Execute(webArgs(t, map[string]any{"query": "golang build tags"}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.calls != 1 || fake.gotQuery != "golang build tags" || fake.gotMax != 2 {
		t.Errorf("backend saw calls=%d query=%q max=%d, want 1 / golang build tags / 2",
			fake.calls, fake.gotQuery, fake.gotMax)
	}
	for _, want := range []string{"1. Alpha", "https://a.example/1", "first result", "2. Beta", "https://b.example/2", "second result"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"Gamma", "Delta", "Eps"} {
		if strings.Contains(out, notWant) {
			t.Errorf("output contains %q past MaxResults=2; got:\n%s", notWant, out)
		}
	}

	// The per-call max_results argument overrides the tool default.
	if _, err := tool.Execute(webArgs(t, map[string]any{"query": "x", "max_results": 3})); err != nil {
		t.Fatalf("Execute(max_results=3): %v", err)
	}
	if fake.gotMax != 3 {
		t.Errorf("max_results arg: backend got max=%d, want 3", fake.gotMax)
	}
}

// TestWebSearchEmptyQuery: a blank query errors without calling the backend.
func TestWebSearchEmptyQuery(t *testing.T) {
	fake := &fakeBackend{}
	tool := WebSearchTool{Backend: fake}

	if _, err := tool.Execute(webArgs(t, map[string]any{"query": "   "})); err == nil {
		t.Fatal("blank query: want error, got nil")
	}
	if _, err := tool.Execute(webArgs(t, map[string]any{"query": ""})); err == nil {
		t.Fatal("empty query: want error, got nil")
	}
	if fake.calls != 0 {
		t.Errorf("backend called %d times for empty queries, want 0", fake.calls)
	}
}

// ddgFixtureSrc mimics DuckDuckGo's html endpoint: two result__a anchors
// (the second wrapped in a uddg= redirect) paired with result__snippet
// anchors. The HTML entity literals are written as @@AMP@@ / @@APOS@@
// placeholders because raw entities would not survive verbatim in this
// source file; they are substituted below.
const ddgFixtureSrc = `<!DOCTYPE html><html><body>
<div class="result">
<a rel="nofollow" class="result__a" href="https://golang.org/">The <b>Go</b> Programming Language</a>
<a class="result__snippet" href="https://golang.org/">Go is statically typed @@AMP@@ compiled.</a>
</div>
<div class="result">
<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpage@@AMP@@rut=abcdef1234">Example Domain</a>
<a class="result__snippet" href="https://example.com/page">A page for testing; it@@APOS@@s canonical.</a>
</div>
</body></html>`

// TestWebSearchDDGParsesFixture: DuckDuckGoBackend pointed at a saved
// DDG-HTML fixture parses two results, cleans the markup/entities out of
// title and snippet, and unwraps the uddg redirect parameter to the real URL.
func TestWebSearchDDGParsesFixture(t *testing.T) {
	fixture := strings.NewReplacer("@@AMP@@", ampEntity, "@@APOS@@", aposEntity).Replace(ddgFixtureSrc)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("q"); q != "golang" {
			t.Errorf("fixture server got q=%q, want golang (query must reach the endpoint)", q)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	// endpoint is unexported by design: only tests in this package
	// repoint the backend at a fixture server.
	backend := DuckDuckGoBackend{endpoint: srv.URL}
	results, err := backend.Search(context.Background(), srv.Client(), "golang", 8, true)
	if err != nil {
		t.Fatalf("backend.Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("parsed %d results, want 2: %+v", len(results), results)
	}
	if results[0].Title != "The Go Programming Language" {
		t.Errorf("result 0 title = %q, want markup stripped", results[0].Title)
	}
	if results[0].URL != "https://golang.org/" {
		t.Errorf("result 0 url = %q, want the plain href", results[0].URL)
	}
	if !strings.Contains(results[0].Snippet, "statically typed & compiled") {
		t.Errorf("result 0 snippet = %q, want the amp entity decoded", results[0].Snippet)
	}
	// The uddg wrapper must decode back to the real target URL.
	if results[1].URL != "https://example.com/page" {
		t.Errorf("result 1 url = %q, want the uddg parameter decoded", results[1].URL)
	}
	if !strings.Contains(results[1].Snippet, "it's canonical") {
		t.Errorf("result 1 snippet = %q, want the ' entity decoded", results[1].Snippet)
	}
}
