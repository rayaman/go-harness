package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Auth tests (web/auth.go)
//
// Same compiled-binary caveat as settings_test.go: run through
// go test -c -o web_pkg_test.exe ./web + .\web_pkg_test.exe from the repo
// root, not `go test ./web` from a package dir (harness/config's init
// panic, pre-existing).
// ---------------------------------------------------------------------------

// authedMux mounts the auth handlers + middleware over a stub /api/ping
// and a stub / page, the way the server task's router will.
func authedMux(a *auth) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+RouteLogin, a.HandleLoginPage)
	mux.HandleFunc("POST "+RouteLogin, a.HandleLogin)
	mux.HandleFunc("POST "+RouteLogout, a.HandleLogout)
	mux.Handle("GET /api/ping", a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"pong": "yes"})
	})))
	mux.Handle("GET /", a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "app page", http.StatusOK)
	})))
	return mux
}

// loginForm POSTs the classic form body and returns the recorder for
// cookie/status assertions.
func loginForm(mux http.Handler, user, pass string) *httptest.ResponseRecorder {
	form := url.Values{"username": {user}, "password": {pass}}
	req := httptest.NewRequest(http.MethodPost, RouteLogin, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// sessionCookie extracts the session cookie value from a Set-Cookie
// header list (empty string when absent).
func sessionCookie(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	return ""
}

func TestLoginWrongPasswordNoCookie(t *testing.T) {
	a := newAuth(Config{Username: "ryan", Password: "s3cret"})
	mux := authedMux(a)

	rec := loginForm(mux, "ryan", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password status = %d, want 401", rec.Code)
	}
	if sessionCookie(rec) != "" {
		t.Error("wrong password issued a session cookie")
	}
	if !strings.Contains(rec.Body.String(), "<form") {
		t.Error("failed form login did not re-render the login page")
	}
}

func TestLoginSuccessCookiePassesMiddleware(t *testing.T) {
	a := newAuth(Config{Username: "ryan", Password: "s3cret"})
	mux := authedMux(a)

	rec := loginForm(mux, "ryan", "s3cret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303 redirect to /", rec.Code)
	}
	token := sessionCookie(rec)
	if token == "" {
		t.Fatal("no session cookie after successful login")
	}
	if len(token) < 32 {
		t.Errorf("session token %q is shorter than 32 chars", token)
	}
	// Cookie attributes per the spec: HttpOnly, Lax, Path=/, Max-Age=24h.
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge != 86400 {
		t.Errorf("cookie attributes = %+v, want HttpOnly/Lax/Path=//Max-Age=86400", cookie)
	}

	// The token now passes the middleware on /api/ routes.
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("authenticated /api/ping = %d, want 200: %s", rec2.Code, rec2.Body.String())
	}
}

func TestLogoutKillsSession(t *testing.T) {
	a := newAuth(Config{Username: "ryan", Password: "s3cret"})
	mux := authedMux(a)

	token := sessionCookie(loginForm(mux, "ryan", "s3cret"))
	if token == "" {
		t.Fatal("login produced no cookie")
	}

	logout := httptest.NewRequest(http.MethodPost, RouteLogout, nil)
	logout.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, logout)

	// Cookie expired (Max-Age<0) AND the token deleted server-side: the
	// old value must fail the middleware even replayed by hand.
	var expired bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			expired = true
		}
	}
	if !expired {
		t.Error("logout did not send an expiring session cookie")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("replayed post-logout token = %d, want 401", rec2.Code)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	a := newAuth(Config{Username: "ryan", Password: "s3cret"})
	mux := authedMux(a)

	token := sessionCookie(loginForm(mux, "ryan", "s3cret"))
	// Backdate the server-side expiry past sessionTTL's window.
	a.mu.Lock()
	a.sessions[token] = time.Now().Add(-time.Second)
	a.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expired token = %d, want 401", rec.Code)
	}
	// Lazy deletion: the entry must be gone, not just time-expired.
	a.mu.Lock()
	_, present := a.sessions[token]
	a.mu.Unlock()
	if present {
		t.Error("expired session token was not lazily deleted")
	}
}

func TestUnauthenticatedRouting(t *testing.T) {
	a := newAuth(Config{Username: "ryan", Password: "s3cret"})
	mux := authedMux(a)

	// API without cookie: 401 JSON, not a redirect.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/ping = %d, want 401", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("unauthenticated API Content-Type = %q, want JSON", ct)
	}

	// Page without cookie: redirect to /login.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != RouteLogin {
		t.Errorf("unauthenticated page = %d %q, want 303 -> /login", rec.Code, rec.Header().Get("Location"))
	}

	// Public paths load unauthenticated.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RouteLogin, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<form") {
		t.Errorf("GET /login = %d, want 200 with the login form", rec.Code)
	}
	public := http.NewServeMux()
	public.Handle("/static/app.js", a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	public.Handle("/favicon.ico", a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	for _, p := range []string{"/static/app.js", "/favicon.ico"} {
		rec = httptest.NewRecorder()
		public.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("public path %s = %d, want 200 unauthenticated", p, rec.Code)
		}
	}
}

func TestJSONLoginShape(t *testing.T) {
	a := newAuth(Config{Username: "ryan", Password: "s3cret"})
	mux := authedMux(a)

	req := httptest.NewRequest(http.MethodPost, RouteLogin,
		strings.NewReader(`{"username":"ryan","password":"s3cret"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON login = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if sessionCookie(rec) == "" {
		t.Error("JSON login set no cookie")
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Errorf("JSON login body is not JSON: %v", err)
	}

	// Wrong JSON password: 401 JSON (fetch clients read the error).
	req = httptest.NewRequest(http.MethodPost, RouteLogin,
		strings.NewReader(`{"username":"ryan","password":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong JSON password = %d, want 401", rec.Code)
	}
}

func TestCredentialPrecedence(t *testing.T) {
	// Flags win over env.
	t.Setenv("HARNESS_WEB_USER", "envuser")
	t.Setenv("HARNESS_WEB_PASS", "envpass")
	a := newAuth(Config{Username: "flaguser", Password: "flagpass"})
	if a.username != "flaguser" || a.password != "flagpass" {
		t.Errorf("flags ignored: got %q/%q", a.username, a.password)
	}

	// Env used when flags are empty.
	a = newAuth(Config{})
	if a.username != "envuser" || a.password != "envpass" {
		t.Errorf("env fallback failed: got %q/%q", a.username, a.password)
	}

	// admin/admin last, per-field composition (flag user + env pass).
	t.Setenv("HARNESS_WEB_USER", "")
	t.Setenv("HARNESS_WEB_PASS", "envpass")
	a = newAuth(Config{Username: "flaguser"})
	if a.username != "flaguser" || a.password != "envpass" {
		t.Errorf("per-field composition broke: got %q/%q", a.username, a.password)
	}

	// Nothing set at all -> the default pair.
	t.Setenv("HARNESS_WEB_PASS", "")
	a = newAuth(Config{})
	if a.username != defaultWebUser || a.password != defaultWebPassword {
		t.Errorf("default fallback failed: got %q/%q", a.username, a.password)
	}
}

func TestLoopbackBindDetection(t *testing.T) {
	for _, local := range []string{"", "localhost", "127.0.0.1", "127.0.0.1:8080", "::1", "[::1]:8080"} {
		if !loopbackBind(local) {
			t.Errorf("loopbackBind(%q) = false, want true", local)
		}
	}
	for _, remote := range []string{"0.0.0.0", "0.0.0.0:8080", "192.168.1.235", "example.com"} {
		if loopbackBind(remote) {
			t.Errorf("loopbackBind(%q) = true, want false", remote)
		}
	}
}
