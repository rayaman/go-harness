package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func shellArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal shell args: %v", err)
	}
	return b
}

// TestShellEcho: a trivial echo through the platform shell comes back
// with its output.
func TestShellEcho(t *testing.T) {
	tool := ShellTool{BaseDir: t.TempDir()}

	out, err := tool.Execute(shellArgs(t, map[string]any{"command": "echo hello"}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output = %q, want it to contain %q", out, "hello")
	}
}

// TestShellExitCodeInString: a non-zero exit is model-visible data, not a
// Go error: the result string carries the exit code and any output.
func TestShellExitCodeInString(t *testing.T) {
	tool := ShellTool{BaseDir: t.TempDir()}

	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "exit /b 3"
	} else {
		cmd = "echo oops; exit 3"
	}

	out, err := tool.Execute(shellArgs(t, map[string]any{"command": cmd}))
	if err != nil {
		t.Fatalf("Execute returned Go error %v, want exit folded into the result string", err)
	}
	if !strings.Contains(out, "command exited with code 3") {
		t.Errorf("output = %q, want it to start with the exit-code message", out)
	}
	if runtime.GOOS != "windows" && !strings.Contains(out, "oops") {
		t.Errorf("output = %q, want it to include the command's stdout", out)
	}
}

// TestShellCwdEscapeRejected: a cwd that resolves outside BaseDir (via
// ".." or an absolute path elsewhere) is rejected before anything runs.
func TestShellCwdEscapeRejected(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir() // a second temp dir, guaranteed outside base
	tool := ShellTool{BaseDir: base}

	cases := []string{
		"..",
		filepath.Join("..", ".."),
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, `C:\Windows`)
	} else {
		cases = append(cases, "/etc", outside)
	}

	for _, cwd := range cases {
		out, err := tool.Execute(shellArgs(t, map[string]any{
			"command": "echo pwned",
			"cwd":     cwd,
		}))
		if err == nil {
			t.Errorf("cwd %q: no error, output %q, want rejection", cwd, out)
			continue
		}
		if !strings.Contains(err.Error(), "outside") {
			t.Errorf("cwd %q: error = %v, want it to mention outside", cwd, err)
		}
		if strings.Contains(out, "pwned") {
			t.Errorf("cwd %q: command executed (%q) despite escaping", cwd, out)
		}
	}

	// A cwd legitimately under BaseDir still works.
	sub := filepath.Join(base, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("create subdir: %v", err)
	}
	out, err := tool.Execute(shellArgs(t, map[string]any{
		"command": "echo inside",
		"cwd":     "sub",
	}))
	if err != nil {
		t.Fatalf("in-base cwd: Execute: %v", err)
	}
	if !strings.Contains(out, "inside") {
		t.Errorf("in-base cwd output = %q, want it to contain %q", out, "inside")
	}
}

// TestShellTimeout: a command outlasting its timeout argument comes back
// as a Go error mentioning "timed out", well under the command's own
// runtime (the process must be killed, not awaited).
//
// The busy loops are SHELL BUILTINS, deliberately not ping/sleep: those
// spawn grandchild processes that keep running (and keep the temp-dir
// handle open) after the shell is killed, which would neither complete
// under the wall-time budget nor let t.TempDir clean up.
func TestShellTimeout(t *testing.T) {
	tool := ShellTool{BaseDir: t.TempDir()}

	var cmd string
	if runtime.GOOS == "windows" {
		// cmd built-in loop: ~forever iterations of `cd .`.
		cmd = `for /L %i in (1,1,100000000) do @cd .`
	} else {
		cmd = `while :; do :; done`
	}

	start := time.Now()
	_, err := tool.Execute(shellArgs(t, map[string]any{
		"command": cmd,
		"timeout": 1,
	}))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Execute succeeded, want a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want it to contain %q", err, "timed out")
	}
	// The kill must land near the 1s timeout, nowhere near the command's
	// ~30s runtime.
	if elapsed > 5*time.Second {
		t.Errorf("timeout took %v, want ~1s (command should have been killed)", elapsed)
	}
}
