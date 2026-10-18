package luatool

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// HTTPConfig defines the security boundaries for Lua network requests.
type HTTPConfig struct {
	Timeout             time.Duration
	MaxResponseBodySize int64 // In bytes (e.g., 2 MB)
	AllowPrivateIPs     bool  // Should be false in production
}

// DefaultHTTPConfig provides secure default settings.
var DefaultHTTPConfig = HTTPConfig{
	Timeout:             10 * time.Second,
	MaxResponseBodySize: 2 * 1024 * 1024, // 2MB max response
	AllowPrivateIPs:     false,           // SSRF protection enabled
}

// RegisterHTTPModule attaches the custom 'http' module to the Gopher-Lua state.
func RegisterHTTPModule(L *lua.LState, cfg HTTPConfig) {
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultHTTPConfig.Timeout
	}
	if cfg.MaxResponseBodySize == 0 {
		cfg.MaxResponseBodySize = DefaultHTTPConfig.MaxResponseBodySize
	}

	client := &http.Client{
		Timeout: cfg.Timeout,
	}

	// Define module functions
	exports := map[string]lua.LGFunction{
		"request": makeHTTPRequestFunc(client, cfg),
	}

	// Register module so scripts can use `local http = require("http")`.
	// Push the module table explicitly: SetFuncs only populates it, and
	// without a push require() returns the module-name string instead.
	L.PreloadModule("http", func(L *lua.LState) int {
		tbl := L.NewTable()
		L.SetFuncs(tbl, exports)
		L.Push(tbl)
		return 1
	})
}

func makeHTTPRequestFunc(client *http.Client, cfg HTTPConfig) lua.LGFunction {
	return func(L *lua.LState) int {
		tbl := L.CheckTable(1)

		// Parse input parameters from Lua table
		rawURL := tbl.RawGetString("url").String()
		method := strings.ToUpper(tbl.RawGetString("method").String())
		if method == "" {
			method = "GET"
		}

		bodyStr := tbl.RawGetString("body").String()

		// 1. Validate URL & Prevent SSRF
		parsedURL, err := url.Parse(rawURL)
		if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
			L.Push(lua.LNil)
			L.Push(lua.LString("invalid URL or unsupported scheme"))
			return 2
		}

		if !cfg.AllowPrivateIPs {
			if err := validatePublicURL(parsedURL); err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString(fmt.Sprintf("SSRF blocked: %v", err)))
				return 2
			}
		}

		// 2. Prepare HTTP Context and Request
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
		defer cancel()

		var bodyReader io.Reader
		if bodyStr != "" {
			bodyReader = bytes.NewBufferString(bodyStr)
		}

		req, err := http.NewRequestWithContext(ctx, method, parsedURL.String(), bodyReader)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("failed to create request: %v", err)))
			return 2
		}

		// 3. Set Headers
		headersTable := tbl.RawGetString("headers")
		if hTbl, ok := headersTable.(*lua.LTable); ok {
			hTbl.ForEach(func(key, val lua.LValue) {
				req.Header.Set(key.String(), val.String())
			})
		}

		// Default User-Agent if not specified
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", "Go-LuaTool-Client/1.0")
		}

		// 4. Execute Request
		resp, err := client.Do(req)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("http request failed: %v", err)))
			return 2
		}
		defer resp.Body.Close()

		// 5. Read Response with Size Limit
		limitedReader := io.LimitReader(resp.Body, cfg.MaxResponseBodySize)
		respBytes, err := io.ReadAll(limitedReader)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("error reading response body: %v", err)))
			return 2
		}

		// 6. Return Lua Response Table { status = 200, body = "...", headers = {...} }
		resTbl := L.NewTable()
		resTbl.RawSetString("status", lua.LNumber(resp.StatusCode))
		resTbl.RawSetString("body", lua.LString(string(respBytes)))

		respHeadersTbl := L.NewTable()
		for k, v := range resp.Header {
			if len(v) > 0 {
				respHeadersTbl.RawSetString(k, lua.LString(v[0]))
			}
		}
		resTbl.RawSetString("headers", respHeadersTbl)

		L.Push(resTbl)
		L.Push(lua.LNil)
		return 2
	}
}

// validatePublicURL ensures the host does not resolve to local or private networks.
func validatePublicURL(u *url.URL) error {
	host := u.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("could not resolve host: %w", err)
	}

	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("ip %s is restricted (private/loopback)", ip.String())
		}
	}
	return nil
}
