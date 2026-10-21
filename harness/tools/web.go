package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// =============================================================================
// Web tools (roadmap 2.3): web_fetch and web_search. Stdlib only, like every
// other builtin here - no new module dependencies. Both are read-only, so
// neither implements Serial(): under the 2.1 parallel pool they may run
// concurrently with other tool calls. Both implement MaxOutputTyper so the
// harness 1.5 output cap agrees with the tool's own cap.
// =============================================================================

const (
	// webUserAgent is sent on every outbound request so remote servers can
	// identify the agent.
	webUserAgent = "ai-harness/1.0 (+local agent)"

	// webRawBodyCap bounds how much of a response body is ever read,
	// regardless of the per-tool text cap applied afterwards.
	webRawBodyCap = 2 * 1024 * 1024

	// webDefaultTimeout applies when a tool's Timeout field is zero.
	webDefaultTimeout = 30 * time.Second

	// webFetchDefaultMaxBytes is the default text cap for web_fetch when
	// MaxBytes is zero.
	webFetchDefaultMaxBytes = 8192

	// webSearchDefaultResults is the default result count for web_search
	// when neither the tool field nor the call argument sets one.
	webSearchDefaultResults = 8

	// webSearchMaxOutput caps web_search's formatted result list; returned
	// from MaxOutputBytes so the harness cap matches.
	webSearchMaxOutput = 4096
)

// truncateWebText keeps the first maxBytes bytes of s, cut at a rune
// boundary, and flags the cut. Deliberately the same simple head-only style
// as capShellOutput (shell.go): this package must stay stdlib-only and
// independent of the harness package.
func truncateWebText(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "...[truncated]"
}

// trimRunes cuts s to at most max runes (adding an ellipsis when cut).
func trimRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}

// webGuardLocal refuses a host that resolves to a loopback, private,
// link-local, or otherwise non-routable address (SSRF guard, roadmap 2.3).
// Callers skip it entirely when their AllowLocal opt-out is set - that flag
// exists for self-hosted endpoints and for tests pointing the tools at
// httptest servers on 127.0.0.1.
func webGuardLocal(ctx context.Context, displayURL, host string) error {
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return fmt.Errorf("refusing to fetch %q: resolves to a local/private address (%s)", displayURL, host)
	}
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", host, err)
	}
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		// IsPrivate covers 10/8, 172.16/12, 192.168/16 and IPv6 ULA
		// (fc00::/7); IsLoopback covers 127/8 and ::1;
		// IsLinkLocalUnicast covers 169.254/16 and fe80::/10.
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return fmt.Errorf("refusing to fetch %q: resolves to a local/private address (%s)", displayURL, s)
		}
	}
	return nil
}

// webGet performs a GET for target with the shared client settings: custom
// User-Agent, default redirect following, and a body read capped at
// webRawBodyCap bytes. Redirects are followed by the client's default
// policy; resp.Request.URL carries the final URL.
func webGet(ctx context.Context, client *http.Client, target string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s: %w", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, webRawBodyCap))
	if err != nil {
		return nil, nil, fmt.Errorf("read response body: %w", err)
	}
	return resp, body, nil
}

// -----------------------------------------------------------------------------
// HTML -> plain text (stdlib-only small pass, no golang.org/x/net/html)
// -----------------------------------------------------------------------------

var (
	// htmlDropBlockREs remove entire script/style/head blocks (RE2 has no
	// backreferences, so one regex per tag).
	htmlScriptRE = regexp.MustCompile(`(?is)<script\b.*?</script\s*>`)
	htmlStyleRE  = regexp.MustCompile(`(?is)<style\b.*?</style\s*>`)
	htmlHeadRE   = regexp.MustCompile(`(?is)<head\b.*?</head\s*>`)

	// htmlBlockTagRE converts block-level boundaries to newlines.
	htmlBlockTagRE = regexp.MustCompile(`(?i)<br\s*/?>|</?(?:p|div|li|ul|ol|dl|dt|dd|tr|table|thead|tbody|tfoot|h[1-6]|blockquote|pre|hr|section|article|aside|nav|header|footer|main|form|figure|figcaption|details|summary)\b[^>]*>`)

	// htmlAnyTagRE strips any leftover tags.
	htmlAnyTagRE = regexp.MustCompile(`(?s)<[^>]*>`)

	// htmlEntityReplacer unescapes the handful of entities worth handling.
	// A single-pass Replacer so "&lt;" decodes once, not twice.
	// The '&' bytes are written as \x26 so the entity literals survive
	// verbatim in this source. A single-pass Replacer means "&" + "amp;lt;"
	// decodes once, not twice.
	htmlEntityReplacer = strings.NewReplacer(
		"\x26lt;", "<",
		"\x26gt;", ">",
		"\x26quot;", `"`,
		"\x26#39;", "'",
		"\x26apos;", "'",
		"\x26nbsp;", " ",
		"\x26amp;", "&",
	)

	htmlWSRunRE    = regexp.MustCompile(`[ \t]+\n`)
	htmlBlankRunRE = regexp.MustCompile(`\n{3,}`)
	htmlSpaceRunRE = regexp.MustCompile(`[ \t]{2,}`)
)

// htmlToText strips an HTML document down to readable plain text: script /
// style / head blocks vanish, block-level tags become newlines, remaining
// tags are dropped, a handful of entities are unescaped, and runs of blank
// lines collapse.
func htmlToText(s string) string {
	s = htmlScriptRE.ReplaceAllString(s, "")
	s = htmlStyleRE.ReplaceAllString(s, "")
	s = htmlHeadRE.ReplaceAllString(s, "")
	s = htmlBlockTagRE.ReplaceAllString(s, "\n")
	s = htmlAnyTagRE.ReplaceAllString(s, "")
	s = htmlEntityReplacer.Replace(s)
	s = htmlSpaceRunRE.ReplaceAllString(s, " ")
	s = htmlWSRunRE.ReplaceAllString(s, "\n")
	s = htmlBlankRunRE.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// isTextualContentType reports whether a response's media type should be
// treated as text. Unknown (empty) types are treated as text so raw-mode
// fetches of mislabeled pages still surface content instead of a guard line.
func isTextualContentType(mt string) bool {
	if mt == "" || strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case "application/json", "application/xml", "application/xhtml+xml",
		"application/javascript", "application/x-javascript",
		"application/sql", "application/graphql":
		return true
	}
	return strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml")
}

// mediaTypeOf extracts the media type from a Content-Type header value.
func mediaTypeOf(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// =============================================================================
// web_fetch
// =============================================================================

// WebFetchTool fetches a URL over http/https and returns its content as
// text (HTML is converted to plain text by default). Safety rails owned
// here: the scheme is restricted to http/https (so file://, ftp://, ... are
// refused), hosts resolving to local/private addresses are refused unless
// AllowLocal is set, the raw body read is capped at 2 MB, and the returned
// text is capped at MaxBytes with a "...[truncated]" marker.
type WebFetchTool struct {
	// MaxBytes caps the returned text. Zero means webFetchDefaultMaxBytes.
	MaxBytes int

	// AllowLocal opts out of the SSRF guard. It exists for self-hosted
	// or test endpoints (e.g. an httptest server on 127.0.0.1); leave it
	// false for anything an untrusted model can steer.
	AllowLocal bool

	// Timeout bounds the whole fetch (DNS, redirects, body read). Zero
	// means webDefaultTimeout (30s).
	Timeout time.Duration

	// Client overrides the HTTP client; nil builds a default one with the
	// tool's Timeout. Tests inject one pointed at an httptest server.
	Client *http.Client
}

func (t WebFetchTool) Name() string { return "web_fetch" }

// MaxOutputBytes implements harness.MaxOutputTyper (roadmap 1.5) so the
// harness-side cap agrees with the tool's own text cap.
func (t WebFetchTool) MaxOutputBytes() int { return t.maxBytes() }

func (t WebFetchTool) maxBytes() int {
	if t.MaxBytes > 0 {
		return t.MaxBytes
	}
	return webFetchDefaultMaxBytes
}

func (t WebFetchTool) Description() string {
	return fmt.Sprintf("Fetch a web page over http/https and return its content as text (HTML is converted to plain text unless extract_text is false). Local and private addresses are refused. Output is capped at %d bytes.", t.maxBytes())
}

func (t WebFetchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The URL to fetch (http or https only).",
			},
			"extract_text": map[string]any{
				"type":        "boolean",
				"description": "Convert HTML to plain text (default true). Set false to get the raw body.",
			},
		},
		"required": []string{"url"},
	}
}

func (t WebFetchTool) Execute(args json.RawMessage) (string, error) {
	var params struct {
		URL         string `json:"url"`
		ExtractText *bool  `json:"extract_text"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	target := strings.TrimSpace(params.URL)
	if target == "" {
		return "", fmt.Errorf("url is required")
	}
	extract := true
	if params.ExtractText != nil {
		extract = *params.ExtractText
	}

	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", target, err)
	}
	// Scheme restriction: only http/https. This rejects file://, ftp://,
	// gopher://, data:, javascript:, etc. before any resolution or request.
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("refusing to fetch %q: unsupported scheme %q (only http and https are allowed)", target, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("url %q has no host", target)
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = webDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if !t.AllowLocal {
		if err := webGuardLocal(ctx, target, host); err != nil {
			return "", err
		}
	}

	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, body, err := webGet(ctx, client, target)
	if err != nil {
		return "", err
	}

	finalURL := resp.Request.URL.String()
	mt := mediaTypeOf(resp.Header.Get("Content-Type"))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("web_fetch: HTTP %d (%s) for %s", resp.StatusCode, resp.Status, finalURL)
	}

	header := fmt.Sprintf("fetched %s (%s, %d bytes)\n", finalURL, orUnknown(mt), len(body))

	// Binary guard: never hand image/octet-stream bytes to the model.
	if !isTextualContentType(mt) {
		return header + fmt.Sprintf("[content-type %s, %d bytes, not fetched as text]", orUnknown(mt), len(body)), nil
	}

	text := string(body)
	if extract && strings.Contains(mt, "html") {
		text = htmlToText(text)
	}
	return header + truncateWebText(text, t.maxBytes()), nil
}

func orUnknown(mt string) string {
	if mt == "" {
		return "unknown"
	}
	return mt
}

// =============================================================================
// web_search
// =============================================================================

// SearchResult is one hit from a search backend.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// SearchBackend is the pluggable search provider seam (roadmap 2.3). A
// backend does its own HTTP via the shared client (honoring ctx) and returns
// parsed results; allowLocal lets backends aimed at internal search services
// skip the SSRF guard, mirroring WebFetchTool.AllowLocal.
type SearchBackend interface {
	Search(ctx context.Context, client *http.Client, query string, maxResults int, allowLocal bool) ([]SearchResult, error)
}

// WebSearchTool searches the web and returns a numbered list of results
// (title / URL / snippet). The backend is pluggable; the default is
// DuckDuckGo's keyless HTML endpoint.
type WebSearchTool struct {
	// Backend does the actual querying. nil means DuckDuckGoBackend{}.
	Backend SearchBackend

	// MaxResults is the default result count. Zero means
	// webSearchDefaultResults; the per-call max_results argument wins.
	MaxResults int

	// Client overrides the HTTP client handed to the backend; nil builds
	// a default one with the tool's Timeout.
	Client *http.Client

	// AllowLocal opts the backend out of the SSRF guard (same purpose as
	// WebFetchTool.AllowLocal: self-hosted search endpoints and tests).
	AllowLocal bool

	// Timeout bounds the search request. Zero means webDefaultTimeout.
	Timeout time.Duration
}

func (t WebSearchTool) Name() string { return "web_search" }

// MaxOutputBytes implements harness.MaxOutputTyper (roadmap 1.5).
func (t WebSearchTool) MaxOutputBytes() int { return webSearchMaxOutput }

func (t WebSearchTool) Description() string {
	return fmt.Sprintf("Search the web and return up to %d results as a numbered list of title, URL, and snippet.", t.maxResults(0))
}

func (t WebSearchTool) maxResults(param int) int {
	if param > 0 {
		return param
	}
	if t.MaxResults > 0 {
		return t.MaxResults
	}
	return webSearchDefaultResults
}

func (t WebSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query.",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Optional maximum number of results to return.",
			},
		},
		"required": []string{"query"},
	}
}

func (t WebSearchTool) Execute(args json.RawMessage) (string, error) {
	var params struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	query := strings.TrimSpace(params.Query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	maxResults := t.maxResults(params.MaxResults)

	backend := t.Backend
	if backend == nil {
		backend = DuckDuckGoBackend{}
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = webDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	results, err := backend.Search(ctx, client, query, maxResults, t.AllowLocal)
	if err != nil {
		return "", fmt.Errorf("web_search: %w", err)
	}
	if len(results) > maxResults {
		results = results[:maxResults]
	}
	if len(results) == 0 {
		return fmt.Sprintf("no results for %q", query), nil
	}

	var b strings.Builder
	for i, r := range results {
		title := strings.TrimSpace(htmlEntityReplacer.Replace(r.Title))
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, title, r.URL)
		if snip := strings.TrimSpace(htmlEntityReplacer.Replace(r.Snippet)); snip != "" {
			b.WriteString("   " + trimRunes(snip, 200) + "\n")
		}
	}
	return truncateWebText(b.String(), webSearchMaxOutput), nil
}

// =============================================================================
// DuckDuckGoBackend (default web_search backend; keyless HTML endpoint)
// =============================================================================

// ddgHTMLEndpoint is DuckDuckGo's no-API-key HTML search endpoint.
const ddgHTMLEndpoint = "https://html.duckduckgo.com/html/"

// DuckDuckGoBackend queries DuckDuckGo's HTML endpoint and scrapes the
// result anchors. No API key required.
type DuckDuckGoBackend struct {
	// endpoint overrides the DDG HTML endpoint. Unexported on purpose:
	// production callers use the default; only tests in this package
	// repoint it at an httptest server.
	endpoint string
}

var (
	// ddgAnchorRE grabs every <a>...</a> so class/href/inner text can be
	// pulled regardless of attribute order.
	ddgAnchorRE = regexp.MustCompile(`(?is)<a\b[^>]*>.*?</a>`)

	ddgClassRE = regexp.MustCompile(`(?i)\bclass="([^"]*)"`)
	ddgHrefRE  = regexp.MustCompile(`(?i)\bhref="([^"]*)"`)

	ddgInlineTagRE = regexp.MustCompile(`(?s)<[^>]*>`)
	ddgSpaceRE     = regexp.MustCompile(`\s{2,}`)
)

func (b DuckDuckGoBackend) Search(ctx context.Context, client *http.Client, query string, maxResults int, allowLocal bool) ([]SearchResult, error) {
	endpoint := b.endpoint
	if endpoint == "" {
		endpoint = ddgHTMLEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse search endpoint: %w", err)
	}
	if !allowLocal {
		if err := webGuardLocal(ctx, endpoint, u.Hostname()); err != nil {
			return nil, err
		}
	}
	q := url.Values{}
	q.Set("q", query)
	u.RawQuery = q.Encode()

	if client == nil {
		client = &http.Client{Timeout: webDefaultTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, webRawBodyCap))
	if err != nil {
		return nil, fmt.Errorf("read search response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("search endpoint returned HTTP %d", resp.StatusCode)
	}
	// Defensive by design: an unexpected page shape yields an empty result
	// set (Execute then reports "no results"), not a parse error.
	return parseDDGResults(string(body), maxResults), nil
}

// parseDDGResults pairs the result__a title anchors with the
// result__snippet anchors of DDG's HTML page, in document order.
func parseDDGResults(html string, maxResults int) []SearchResult {
	type anchor struct {
		href string
		text string
	}
	var links, snippets []anchor
	for _, a := range ddgAnchorRE.FindAllString(html, -1) {
		cls := ""
		if m := ddgClassRE.FindStringSubmatch(a); m != nil {
			cls = strings.ToLower(m[1])
		}
		if !strings.Contains(cls, "result__a") && !strings.Contains(cls, "result__snippet") {
			continue
		}
		href := ""
		if m := ddgHrefRE.FindStringSubmatch(a); m != nil {
			href = htmlEntityReplacer.Replace(m[1])
		}
		inner := a
		if gt := strings.IndexByte(inner, '>'); gt >= 0 {
			inner = inner[gt+1:]
		}
		if end := strings.LastIndex(strings.ToLower(inner), "</a>"); end >= 0 {
			inner = inner[:end]
		}
		if strings.Contains(cls, "result__a") {
			links = append(links, anchor{href: href, text: inner})
		} else {
			snippets = append(snippets, anchor{href: href, text: inner})
		}
	}

	out := make([]SearchResult, 0, len(links))
	for i, l := range links {
		if maxResults > 0 && len(out) >= maxResults {
			break
		}
		res := SearchResult{
			Title: cleanDDGText(l.text),
			URL:   ddgResolveHref(l.href),
		}
		if i < len(snippets) {
			res.Snippet = cleanDDGText(snippets[i].text)
		}
		out = append(out, res)
	}
	return out
}

// ddgResolveHref unwraps DDG's redirect wrapper: result hrefs often look
// like //duckduckgo.com/l/?uddg=<escaped real URL>&rut=... . The uddg
// parameter carries the target, already percent-encoded by url.Values.
func ddgResolveHref(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if target := u.Query().Get("uddg"); target != "" {
		return target
	}
	return href
}

// cleanDDGText strips inline markup (DDG bolds query matches with <b>),
// unescapes entities, and flattens whitespace.
func cleanDDGText(s string) string {
	s = ddgInlineTagRE.ReplaceAllString(s, "")
	s = htmlEntityReplacer.Replace(s)
	s = ddgSpaceRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
