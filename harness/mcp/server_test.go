package mcp

import (
	"context"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
)

// mcpTestServerFlag puts the test binary into "act as a stdio MCP
// server" mode instead of running tests (see TestMain). The child is
// re-executed with this flag plus MCP_TEST_SERVER=1, and serves the
// same fake MCP protocol code over its real os.Stdin/os.Stdout pipes -
// proving the genuine stdio path, not just in-process net.Pipe.
var mcpTestServerFlag = flag.Bool("mcp-test-server", false, "run as a stdio MCP fake server instead of tests")

func TestMain(m *testing.M) {
	// TestMain runs before testing flag parsing, so scan os.Args by
	// hand; the child never reaches m.Run and thus never parses flags.
	wantServer := os.Getenv("MCP_TEST_SERVER") == "1"
	for _, a := range os.Args[1:] {
		if a == "-mcp-test-server" || a == "--mcp-test-server" {
			wantServer = true
		}
	}
	if wantServer {
		_ = serveFake(os.Stdin, os.Stdout, fakeOpts{})
		os.Exit(0)
	}
	// -mcp-test-server is registered above so a stray m.Run never
	// trips on the unknown flag; normal runs proceed here.
	_ = mcpTestServerFlag
	os.Exit(m.Run())
}

// TestStartServerSubprocess exercises the full stdio spawn path: the
// test binary re-executed as a child MCP server, a real Client over
// its stdin/stdout pipes, ListTools/CallTool round-trips, and Close
// actually terminating the child (its process is reaped: ProcessState
// populated by the Wait inside the transport's Close).
func TestStartServerSubprocess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate test binary for re-exec: %v", err)
	}

	client, cmd, err := StartServer(exe,
		[]string{"-mcp-test-server"},
		append(os.Environ(), "MCP_TEST_SERVER=1"))
	if err != nil {
		t.Skipf("failed to re-exec test binary as MCP server: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := client.ListTools(ctx)
	if err != nil {
		client.Close()
		t.Fatalf("ListTools over real stdio: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" {
		client.Close()
		t.Fatalf("unexpected tool set: %+v", tools)
	}

	out, err := client.CallTool(ctx, "echo", []byte(`{"msg":"hi"}`))
	if err != nil {
		client.Close()
		t.Fatalf("CallTool over real stdio: %v", err)
	}
	if out != "first\nsecond" {
		client.Close()
		t.Fatalf("CallTool output = %q, want %q", out, "first\nsecond")
	}

	out, err = client.CallTool(ctx, "boom", nil)
	if err != nil {
		client.Close()
		t.Fatalf("CallTool boom: %v", err)
	}
	if !strings.HasPrefix(out, "error: ") {
		client.Close()
		t.Fatalf("boom output = %q, want error: prefix", out)
	}

	// Close must terminate the child: stdin gets EOF, the fake exits,
	// and the transport's Close Kills + Wait()s the process. Poll for
	// the populated ProcessState as proof cmd.Wait has returned.
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for cmd.ProcessState == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cmd.ProcessState == nil {
		t.Fatal("child MCP server process still running 5s after Close")
	}
	t.Logf("child process %d exited: %v", cmd.Process.Pid, cmd.ProcessState)
}
