package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Username/password login and session middleware (web-server roadmap,
// auth subtask)
//
// The UI drives the agent core, so it is unauthenticated RCE-by-proxy if
// left open: every route except the login page, /static/* and
// /favicon.ico goes through Middleware below. Sessions are in-memory
// random tokens (restart = everyone logs back in - fine for a local
// tool, and it keeps the key material off disk next to web-settings.json
// which already stores an API key).
//
// newAuth resolves the credentials and is the exported-to-the-package
// entry point the server task calls from web/config.go's Serve (still a
// placeholder stub today - the server task wires routes; the handlers
// here are ready to mount).
// ---------------------------------------------------------------------------

// Route paths the auth handlers below serve, for the server task's
// router:
//
//	GET  /login  - the login page
//	POST /login  - submit credentials, set the session cookie
//	POST /logout - drop the session server-side and expire the cookie
const (
	RouteLogin  = "/login"
	RouteLogout = "/logout"
)

// sessionCookieName is the single cookie name this package uses. HttpOnly
// so page JS can never read the token, SameSite=Lax so a cross-site POST
// never carries it (GET navigations still do, which is what makes the
// redirect-to-login flow work after a restart).
const sessionCookieName = "harness_session"

// sessionTTL is both the cookie Max-Age and the server-side expiry
// window for a session token. checkSession enforces the server-side half
// independently of the cookie, so a stolen/kept-alive cookie cannot
// outlive its token entry.
const sessionTTL = 24 * time.Hour

// defaultWebUser/defaultWebPassword are the last-resort credentials when
// neither the --web-user/--web-pass flags (main.go) nor the
// HARNESS_WEB_USER/HARNESS_WEB_PASS env vars are set. newAuth warns
// loudly whenever they are used, and harder when the bind address is
// non-loopback (see the note in resolveCredentials).
const (
	defaultWebUser     = "admin"
	defaultWebPassword = "admin"
)

// auth holds the resolved credentials and the live session tokens.
// sessions maps token hex -> expiry; mu guards every read/write of the
// map. Constructed via newAuth.
type auth struct {
	username string
	password string

	mu       sync.Mutex
	sessions map[string]time.Time
}

// newAuth resolves the credentials from cfg (the flags main.go already
// parses, see web/config.go) with the env fallback, warns per the rules
// in resolveCredentials, and returns a ready auth. The serve task calls
// this from Serve; until Serve is real, this constructor is the entry
// point.
func newAuth(cfg Config) *auth {
	user, pass, defaulted := resolveCredentials(cfg)
	if defaulted {
		fmt.Fprintf(os.Stderr, "warning: web login credentials default to %q/%q; set the --web-user/--web-pass flags or the HARNESS_WEB_USER/HARNESS_WEB_PASS environment variables\n", defaultWebUser, defaultWebPassword)
		if !loopbackBind(cfg.BindAddr) {
			fmt.Fprintf(os.Stderr, "warning: web UI binds to %q, which is reachable from the network, with the default password - change it before exposing this port\n", cfg.BindAddr)
		}
	}
	return &auth{username: user, password: pass, sessions: make(map[string]time.Time)}
}

// resolveCredentials applies the precedence: --web-user/--web-pass flags
// first, then HARNESS_WEB_USER/HARNESS_WEB_PASS, then admin/admin. The
// bool return says whether the defaults were used (the caller warns). A
// half-set pair (flag user + env pass, say) composes per-field, which
// keeps the flag/env mix predictable.
func resolveCredentials(cfg Config) (user, pass string, defaulted bool) {
	user, pass = cfg.Username, cfg.Password
	if user == "" {
		user = os.Getenv("HARNESS_WEB_USER")
	}
	if pass == "" {
		pass = os.Getenv("HARNESS_WEB_PASS")
	}
	if user == "" || pass == "" {
		return defaultWebUser, defaultWebPassword, true
	}
	return user, pass, false
}

// loopbackBind reports whether a bind address is local-only. Empty (web
// mode off / default), "localhost", and any loopback IP (127.0.0.0/8,
// ::1) are local; "0.0.0.0", "" is treated per the rule above, and a
// routable IP like 192.168.x.x is not. A bare host name that resolves
// nowhere parseable is treated as non-loopback (warn rather than stay
// quiet).
func loopbackBind(addr string) bool {
	if addr == "" || addr == "localhost" {
		return true
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}
	if addr == "localhost" {
		return true
	}
	ip := net.ParseIP(addr)
	return ip != nil && ip.IsLoopback()
}

// newSessionToken returns 32 random bytes hex-encoded (64 chars) from
// crypto/rand. A rand failure is fatal to the login attempt (a
// predictable session id is worse than an error page), surfaced as an
// error the handler turns into a 500.
func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// credentialsMatch compares both fields with
// subtle.ConstantTimeCompare so response timing leaks neither the
// username nor the password length/content progressively.
func (a *auth) credentialsMatch(user, pass string) bool {
	uOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.username)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(pass), []byte(a.password)) == 1
	return uOK && pOK
}

// checkSession reports whether token names a live session, lazily
// deleting the entry once expired (the cookie may outlive the token if
// the client ignored Max-Age, so the server-side expiry is authoritative
// and checked on every request).
func (a *auth) checkSession(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, token)
		return false
	}
	return true
}

// HandleLoginPage serves GET RouteLogin: already-authenticated visitors
// skip straight to "/" so a logged-in browser never sees the form;
// everyone else gets the inline page below.
func (a *auth) HandleLoginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && a.checkSession(cookie.Value) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	writeLoginPage(w, http.StatusOK, "")
}

// HandleLogin serves POST RouteLogin. The body may be a classic form
// (urlencoded username/password fields) or a JSON object, so the UI task
// can wire either without changing this handler. On success a fresh
// token is stored with a sessionTTL expiry and set as the session
// cookie (HttpOnly, SameSite=Lax, Path=/, Max-Age=sessionTTL). On
// failure: 401 plus the re-rendered login page (API-style JSON clients
// that sent JSON get the 401 body as JSON instead, so fetch()-based UIs
// can read the error).
func (a *auth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var user, pass string
	if jsonRequest(r) {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parse login body"})
			return
		}
		user, pass = body.Username, body.Password
	} else {
		if err := r.ParseForm(); err != nil {
			writeLoginPage(w, http.StatusBadRequest, "malformed form body")
			return
		}
		user, pass = r.FormValue("username"), r.FormValue("password")
	}

	if !a.credentialsMatch(user, pass) {
		if jsonRequest(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
			return
		}
		writeLoginPage(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, err := newSessionToken()
	if err != nil {
		writeLoginPage(w, http.StatusInternalServerError, "internal error generating session")
		return
	}
	a.mu.Lock()
	a.sessions[token] = time.Now().Add(sessionTTL)
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	// Browsers following the POST get redirected to the app; API/JSON
	// clients get a 200 with just the status so they can decide.
	if jsonRequest(r) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// HandleLogout serves POST RouteLogout: the token is deleted server-side
// (so a replayed cookie is dead even before its Max-Age) and the cookie
// is re-sent with Max-Age=-1 to expire it in the browser.
func (a *auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	if jsonRequest(r) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	http.Redirect(w, r, RouteLogin, http.StatusSeeOther)
}

// Middleware gates every route except the public set: the login/logout
// endpoints themselves, /static/* (the UI assets must load on the login
// page), and /favicon.ico (browsers request it unprompted). Protected
// requests without a live session cookie get 401 JSON for /api/* (the
// UI's fetch calls should branch on status, not follow a redirect) and a
// 303 redirect to /login for pages. Expiry is enforced per request via
// checkSession.
func (a *auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(sessionCookieName)
		if err == nil && a.checkSession(cookie.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
			return
		}
		http.Redirect(w, r, RouteLogin, http.StatusSeeOther)
	})
}

// publicPath is the auth-free allowlist (see Middleware).
func publicPath(p string) bool {
	if p == RouteLogin || p == RouteLogout || p == "/favicon.ico" {
		return true
	}
	return strings.HasPrefix(p, "/static/")
}

// jsonRequest distinguishes the fetch()/JSON clients from classic form
// posts so the same handler can answer both in their expected shape.
func jsonRequest(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
}

// writeLoginPage renders the minimal standalone login form (inline HTML,
// no template files, no JS dependency) so the UI task can replace it
// later. message is the optional error banner shown above the form. All
// interpolated text is escaped with html.EscapeString - the error text
// here is static, but the page also echoes the submitted username back
// into the form, which is user input.
func writeLoginPage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>ai-harness login</title></head>
<body style="font-family: system-ui, sans-serif; max-width: 24rem; margin: 4rem auto;">
<h1>ai-harness</h1>
%s<form method="post" action="%s">
<p><label for="u">Username</label><br><input id="u" name="username" autocomplete="username"></p>
<p><label for="p">Password</label><br><input id="p" name="password" type="password" autocomplete="current-password"></p>
<p><button type="submit">Log in</button></p>
</form>
</body>
</html>
`, loginBanner(message), RouteLogin)
}

// loginBanner renders the optional error banner (escaped) or the empty
// string.
func loginBanner(message string) string {
	if message == "" {
		return ""
	}
	return fmt.Sprintf("<p style=\"color:#b00020\">%s</p>\n", html.EscapeString(message))
}
