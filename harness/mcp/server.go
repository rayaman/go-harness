package mcp

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// transport bundles the child process's stdio pipes so closing the MCP
// Client also terminates the server process. Client.Close is safe to
// call twice; killOnce keeps the Kill idempotent too.
type transport struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	cmd    *exec.Cmd
}

func (t *transport) Read(p []byte) (int, error)  { return t.stdout.Read(p) }
func (t *transport) Write(p []byte) (int, error) { return t.stdin.Write(p) }

func (t *transport) Close() error {
	// Close stdin first (a well-behaved server exits on EOF), then
	// hard-kill in case it does not, then reap the process so Wait
	// callers never block forever.
	_ = t.stdin.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = t.stdout.Close()
	// Wait is safe to call once per cmd; errors (already reaped or
	// killed exit status) are expected and ignored here.
	_ = t.cmd.Wait()
	return nil
}

// StartServer spawns an MCP server as a child process wired to stdin/
// stdout pipes (stderr passes straight through to os.Stderr so server
// logs stay visible) and returns a handshaked Client over those pipes
// plus the exec.Cmd, so callers can inspect process state. The cmd is
// kept rather than using exec.CommandContext because Close() must be
// able to Kill() it independently of any request context.
//
// Note: on Windows the command may be a `.cmd`/`.bat` shim (e.g. npx-
// installed MCP servers). Go's exec.Command resolves those through
// cmd.exe for us, so no special-casing is needed here.
func StartServer(command string, args []string, env []string) (*Client, *exec.Cmd, error) {
	cmd := exec.Command(command, args...)
	if len(env) > 0 {
		cmd.Env = env
	}
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("mcp server %s stdin pipe: %w", command, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("mcp server %s stdout pipe: %w", command, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("mcp server %s: %w", command, err)
	}

	c, err := NewClient(&transport{stdin: stdin, stdout: stdout, cmd: cmd}, "ai-harness")
	if err != nil {
		// NewClient failed the handshake: make sure the child dies.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, cmd, fmt.Errorf("mcp server %s handshake: %w", command, err)
	}
	return c, cmd, nil
}
