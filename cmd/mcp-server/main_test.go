package main_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/mcp"
)

// TestTheReferenceBinaryServesOverRealStdio drives the SHIPPED binary as a
// subprocess with the SHIPPED client.
//
// Everything else exercises Serve over in-memory pipes, which cannot catch the
// failures specific to a real process: a stray write to stdout corrupting the
// frame stream, ServeStdio wiring the wrong file descriptors, or a signal
// handler that never lets the process exit. This is the only test that runs
// the binary the way an MCP host would launch it.
func TestTheReferenceBinaryServesOverRealStdio(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := build(t)

	var stderr []string
	tr, err := mcp.StartStdio(context.Background(), mcp.StdioOptions{
		Command: bin,
		Stderr:  func(line string) { stderr = append(stderr, line) },
	})
	if err != nil {
		t.Fatalf("start: %v (stderr: %v)", err, stderr)
	}
	conn := mcp.NewConnection(mcp.ServerConfig{Name: "ref"}, tr, mcp.ConnectionOptions{})
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := conn.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v (stderr: %v)", err, stderr)
	}
	if got := conn.Info().ProtocolVersion; got != mcp.ProtocolVersion {
		t.Fatalf("negotiated %q, want %q", got, mcp.ProtocolVersion)
	}

	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("the reference binary registers at least one tool")
	}

	res, err := conn.Call(ctx, "echo", map[string]any{"message": "over a real pipe"})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError || len(res.Content) != 1 || res.Content[0].Text != "over a real pipe" {
		t.Fatalf("echo returned %+v", res)
	}

	// The templated resource: the only path that proves URI variables survive
	// a real transport.
	read, err := conn.ReadResource(ctx, "agentkit://echo/hello")
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	if len(read.Contents) != 1 || read.Contents[0].Text != "hello" {
		t.Fatalf("the template variable did not reach the handler: %+v", read.Contents)
	}
}

func build(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "mcp-server")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Env = os.Environ()
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, msg)
	}
	return out
}
