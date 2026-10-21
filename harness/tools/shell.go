package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ShellTool runs a shell command for the model (roadmap 1.2). It is the
// riskiest builtin, so the REPL registers it at PolicyAsk by default:
// Harness.runTool gates it through the Confirm hook before this Execute
// ever runs.
//
// Safety rails owned here:
//   - cwd is confined under BaseDir (same containment idiom as the file
//     tools: resolve, then filepath.Rel and reject anything escaping).
//   - combined stdout+stderr output is capped at shellMaxOutput bytes.
//   - the command is killed via exec.CommandContext after its timeout.
type ShellTool struct {
	// BaseDir is the directory commands run in and the root that any
	// cwd argument must stay under.
	BaseDir string

	// DefaultTimeout caps commands that don't ask for their own. Zero
	// falls back to shellDefaultTimeout.
	DefaultTimeout time.Duration
}

const (
	shellDefaultTimeout = 60 * time.Second
	shellMaxTimeout     = 300 * time.Second
	shellMaxOutput      = 64 * 1024
)

func (t ShellTool) Name() string { return "run_shell_command" }

// Serial reports true: shell commands have arbitrary side effects, so
// they never run concurrently with other tool calls in a turn
// (harness.SerialTyper, roadmap 2.1).
func (t ShellTool) Serial() bool { return true }

func (t ShellTool) Description() string {
	return fmt.Sprintf("Run a shell command and return its combined stdout+stderr output. The command runs with %s as the working directory; the optional cwd argument must stay under it. Non-zero exits are reported in the output, not as failures.", t.BaseDir)
}

func (t ShellTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command line to execute.",
			},
			"cwd": map[string]any{
				"type":        "string",
				"description": "Optional working directory, relative to (or under) the base directory.",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Optional timeout in seconds (capped at 300).",
			},
		},
		"required": []string{"command"},
	}
}

func (t ShellTool) Execute(args json.RawMessage) (string, error) {
	var params struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(params.Command) == "" {
		return "", fmt.Errorf("command is required")
	}

	baseAbs, err := filepath.Abs(t.BaseDir)
	if err != nil {
		return "", fmt.Errorf("resolve base directory: %w", err)
	}

	// Resolve the working directory, confining it to BaseDir exactly the
	// way the file tools clamp paths (see readfile.go): absolute cwd
	// arguments are kept absolute so the Rel check below can reject them,
	// relative ones are joined under the base first.
	cwd := baseAbs
	if params.Cwd != "" {
		if filepath.IsAbs(params.Cwd) {
			cwd = filepath.Clean(params.Cwd)
		} else {
			cwd, err = filepath.Abs(filepath.Join(baseAbs, params.Cwd))
			if err != nil {
				return "", fmt.Errorf("resolve cwd: %w", err)
			}
		}
		rel, err := filepath.Rel(baseAbs, cwd)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("cwd %q is outside the allowed base directory %q", params.Cwd, t.BaseDir)
		}
	}

	// Timeout: argument (seconds, hard-capped) wins, else the tool's
	// DefaultTimeout, else the package default.
	timeout := t.DefaultTimeout
	if timeout <= 0 {
		timeout = shellDefaultTimeout
	}
	if params.Timeout > 0 {
		timeout = time.Duration(params.Timeout) * time.Second
		if timeout > shellMaxTimeout {
			timeout = shellMaxTimeout
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Platform shell dispatch: cmd /C on Windows, /bin/sh -c elsewhere.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", params.Command)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", params.Command)
	}
	cmd.Dir = cwd
	// WaitDelay bounds how long the I/O wait can outlive the shell
	// process itself. Without it, `cmd /C ping ...` on Windows would
	// keep CombinedOutput blocked on the inherited stdout pipe until
	// the grandchild ping.exe finished, even though cmd.exe was killed
	// at the deadline.
	cmd.WaitDelay = 2 * time.Second

	// CombinedOutput keeps stdout+stderr interleaved in one buffer with
	// no extra goroutines of our own; the cap is applied to the result.
	output, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("command timed out after %s", timeout)
	}
	if err != nil {
		// A non-zero exit is data for the model, not a harness failure:
		// report code + output in the result string so the model can
		// react. Anything else (shell binary unspawnable, etc.) is a
		// genuine execution error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Sprintf("command exited with code %d:\n%s",
				exitErr.ExitCode(), capShellOutput(string(output))), nil
		}
		return "", fmt.Errorf("executing command: %w", err)
	}
	return capShellOutput(string(output)), nil
}

// capShellOutput keeps the first shellMaxOutput bytes of command output
// and flags the cut. Deliberately simpler than harness.truncateToolOutput
// (head+tail + byte counts): this package must stay stdlib-only and
// independent of the harness package.
func capShellOutput(s string) string {
	if len(s) <= shellMaxOutput {
		return s
	}
	return s[:shellMaxOutput] + "...[truncated]"
}
