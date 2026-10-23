// Package web is the browser front-end for ai-harness (web-server
// roadmap): a local HTTP UI that drives the same agent core the terminal
// REPL uses. main.go builds the core via setupAgent and hands the pieces
// to Serve through Config below.
//
// Dependency direction matters: web imports ai-harness/harness,
// ai-harness/harness/config and ai-harness/harness/mcp (none of which
// import web), and main.go passes the agent handles individually rather
// than a main-owned struct - that is why there is no "core" type in this
// package and no import cycle.
package web

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"ai-harness/harness"
	"ai-harness/harness/config"
	"ai-harness/harness/mcp"
)

// Config carries everything the web server needs, built by main.go.
//
// BindAddr/Port/Username/Password come from the --server/--port/
// --web-user/--web-pass flags (the HARNESS_WEB_USER / HARNESS_WEB_PASS
// env fallbacks land with the auth task). The remaining fields are the
// agent core setupAgent produced: Base is the harness Config as resolved
// at startup (the preset plus the context probe, memory file and
// stream-thinking default) BEFORE any web-settings overrides, so the
// settings UI can show and diff against it; Conf is the live viper
// Config, shared in place so hot-reload of tool policies behaves exactly
// like the REPL's; and Client/Registry/Harness/MemFile/CustomToolDir/
// MCPClients are the running handles the UI talks to and reconfigures
// (Harness.Reconfigure in harness/harness.go is the live
// provider/model/endpoint switch this package will call).
type Config struct {
	// BindAddr is the listen address ("0.0.0.0", "192.168.1.235", ...);
	// non-empty is what turns web mode on in main.go.
	BindAddr string

	// Port is the TCP port for the UI (default 8080, or whatever an
	// explicit --port asked for).
	Port int

	// Username and Password gate the UI login. Empty today; the auth
	// task fills them in (flag first, env fallback later).
	Username string
	Password string

	// Base is the startup harness.Config handed to the Harness, before
	// any web-settings overrides are applied.
	Base harness.Config

	// Conf is the viper-backed runtime config (tool permissions, MCP
	// servers, hot-reload); shared by pointer so config.yaml edits take
	// effect without a restart, same as the REPL's hot-reload block.
	Conf *config.Config

	// Client, Registry and Harness are the live agent handles: the HTTP
	// client to the model server, the tool registry with permission
	// policies applied, and the conversation-carrying Harness.
	Client   *harness.Client
	Registry *harness.Registry
	Harness  *harness.Harness

	// MemFile is the resolved persistent-memory path (memory-file in
	// config.yaml, default agent/memory.md) and CustomToolDir the
	// resolved custom Lua tool directory (custom-tool-dir, default
	// agent/custom_tools) - surfaced for the settings UI and the
	// hot-reload path.
	MemFile       string
	CustomToolDir string

	// MCPClients are the running MCP server clients (roadmap 2.2). The
	// web server does NOT close them - main.go's defer does, so the
	// server processes terminate on exit from either front-end.
	MCPClients []*mcp.Client

	// SettingsPath, SessionsDir and UploadsDir override where the
	// settings override layer persists (default agent/web-settings.json,
	// settings.DefaultSettingsPath), where session JSONs live (default
	// agent/sessions, DefaultSessionsDir) and where POST /api/upload
	// stores images (default agent/uploads, DefaultUploadsDir). Empty
	// means the default; the router subtask added these so tests can
	// point every writable path at a temp dir without touching the real
	// agent/ tree. main.go leaves them empty.
	SettingsPath string
	SessionsDir  string
	UploadsDir   string
}

// Serve starts the web UI on BindAddr:Port and blocks until the server
// stops, returning its error (main.go prints "web error: %v" and exits
// 1). The handler tree comes from newMux (web/server.go), the same
// function the package tests build handlers with; a settings-file load
// failure is a stderr warning there, never a boot failure.
//
// MCPClients in cfg are NOT closed here: main.go's defer terminates the
// MCP server processes on exit from either front-end, and closing them
// from Serve would double-close and kill tools mid-session.
func Serve(cfg Config) error {
	handler, err := newMux(cfg)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.Port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("web server listen on %s: %w", addr, err)
	}
	return nil
}
