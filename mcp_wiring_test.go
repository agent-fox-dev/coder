package agentkit

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/mcp"
	"github.com/agentfox/agentkit-go/plugins"
	"github.com/agentfox/agentkit-go/wire"
)

// mcpTools spins up an in-process MCP server, connects a pool to it, and
// returns the adapted core.Tools.
//
// The client and server here are the SHIPPED implementations talking over a
// pipe, so what this exercises is the real qualified-name path rather than a
// hand-built tool that merely has a name with two underscores in it.
func mcpTools(t *testing.T, serverName string) []core.Tool {
	t.Helper()
	srv := mcp.NewServer(mcp.ServerOptions{})
	if err := srv.RegisterTool(
		mcp.ToolDefinition{Name: "create_issue", Description: "open an issue"},
		func(_ context.Context, args map[string]any) (mcp.ToolsCallResult, error) {
			title, _ := args["title"].(string)
			return mcp.ToolsCallResult{Content: []mcp.Content{
				{Type: "text", Text: "created: " + title}}}, nil
		}); err != nil {
		t.Fatal(err)
	}

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	serverSide := mcp.NewPipeTransport(c2sR, s2cW, wire.Limits{})
	clientSide := mcp.NewPipeTransport(s2cR, c2sW, wire.Limits{})

	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(context.Background(), serverSide) }()

	conn := mcp.NewConnection(mcp.ServerConfig{Name: serverName}, clientSide, mcp.ConnectionOptions{})
	if err := conn.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	pool := mcp.NewPool(mcp.ConnectionOptions{})
	if err := pool.Add(conn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pool.Close()
		_ = serverSide.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("the MCP server loop did not stop")
		}
	})

	tools, err := pool.Tools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

// TestMCPToolsAreGatedByQualifiedNameEverywhere is REQ-MCP-CLIENT-11.
//
// The allowlist, the permission callback and the plugin hooks all have to see
// the SAME name — the qualified one. A gate that matches on the unqualified
// name is a gate that does not apply, and it fails open: the tool runs and the
// policy that was meant to stop it never fired.
func TestMCPToolsAreGatedByQualifiedNameEverywhere(t *testing.T) {
	tools := mcpTools(t, "github")
	if len(tools) != 1 || tools[0].Name != "github__create_issue" {
		t.Fatalf("tools = %+v, want one github__create_issue", tools)
	}

	t.Run("the allowlist matches the qualified name", func(t *testing.T) {
		s := &scripted{turns: []core.AssistantMessage{
			assistantWithTools(core.StopReasonToolUse,
				toolUse(t, "c1", "github__create_issue", `{"title":"bug"}`)),
			{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
		}}
		a := newTestAgent(t, s, func(c *core.AgentConfig) {
			c.ToolPolicy.CustomTools = tools
			// The UNQUALIFIED name is deliberately not in the list.
			c.ToolPolicy.ToolNames = []string{"create_issue"}
		})
		res, err := a.Run(context.Background(), "go")
		if err != nil {
			t.Fatal(err)
		}
		msg := findToolResult(t, res.Messages, "c1")
		if !msg.IsError {
			t.Fatal("an allowlist naming the UNQUALIFIED tool must not admit the " +
				"qualified one; matching loosely here fails open")
		}
	})

	t.Run("the permission callback sees the qualified name", func(t *testing.T) {
		var seen []string
		s := &scripted{turns: []core.AssistantMessage{
			assistantWithTools(core.StopReasonToolUse,
				toolUse(t, "c1", "github__create_issue", `{"title":"bug"}`)),
			{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
		}}
		a := newTestAgent(t, s, func(c *core.AgentConfig) {
			c.ToolPolicy.CustomTools = tools
			c.BeforeToolCall = func(_ context.Context, in core.BeforeToolCallContext) core.BeforeToolCallDecision {
				seen = append(seen, in.ToolName)
				return core.BeforeToolCallDecision{}
			}
		})
		if _, err := a.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		if strings.Join(seen, ",") != "github__create_issue" {
			t.Fatalf("interceptor saw %v, want the qualified name", seen)
		}
	})

	t.Run("plugin hooks see the qualified name and can block", func(t *testing.T) {
		var seen []string
		s := &scripted{turns: []core.AssistantMessage{
			assistantWithTools(core.StopReasonToolUse,
				toolUse(t, "c1", "github__create_issue", `{"title":"bug"}`)),
			{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
		}}
		a := newTestAgent(t, s, func(c *core.AgentConfig) {
			c.ToolPolicy.CustomTools = tools
			c.Plugins = registryWith(&recordingHook{name: "gate", seen: &seen,
				blockPrefix: "github__"})
		})
		res, err := a.Run(context.Background(), "go")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(seen, ",") != "github__create_issue" {
			t.Fatalf("hook saw %v, want the qualified name (REQ-MCP-CLIENT-11)", seen)
		}
		if !findToolResult(t, res.Messages, "c1").IsError {
			t.Fatal("the hook blocked the call and the result must say so")
		}
	})
}

type recordingHook struct {
	plugins.BaseEventHook
	name        string
	seen        *[]string
	blockPrefix string
}

func (h *recordingHook) PluginName() string { return h.name }
func (h *recordingHook) OnToolUse(_ context.Context, tool string, _ json.RawMessage) core.PluginDecision {
	*h.seen = append(*h.seen, tool)
	if h.blockPrefix != "" && strings.HasPrefix(tool, h.blockPrefix) {
		return core.PluginBlock
	}
	return core.PluginNoOpinion
}

// TestAnMCPToolCallIsAuditedWithItsServerName closes REQ-OBS-05 against a real
// MCP tool, rather than against a name that merely looks like one.
func TestAnMCPToolCallIsAuditedWithItsServerName(t *testing.T) {
	tools := mcpTools(t, "github")
	log := &auditLog{}
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "github__create_issue", `{"title":"bug"}`)),
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.ToolPolicy.CustomTools = tools
		c.Hooks.OnAudit = log.add
	})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	calls := log.of(core.AuditToolCall)
	if len(calls) != 1 {
		t.Fatalf("%d tool-call audit events, want 1", len(calls))
	}
	if calls[0].ServerName != "github" {
		t.Fatalf("server_name = %q, want github. It comes from the tool's own MCPServer "+
			"field, not from parsing a name whose prefix is configurable.",
			calls[0].ServerName)
	}
	if calls[0].ToolName != "github__create_issue" {
		t.Fatalf("tool_name = %q, want the qualified name", calls[0].ToolName)
	}
}
