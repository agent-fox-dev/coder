// Command mcp-server is REQ-MCP-SERVER-02's reference driver.
//
// The requirement names `nightshift --mcp-server`; nightshift is a daemon
// built ON this SDK, so the SDK ships the mechanism (mcp.Server, Server.Run)
// and this binary as the smallest complete host. A real host does exactly what
// main does here — construct a Server, register its own tools and resources,
// and call Run — with its own inventory in place of the demonstration one.
//
// The tools registered below exist to make the binary runnable end to end
// against a real MCP client; they are not part of the SDK's surface.
//
//	mcp-server                                # stdio
//	mcp-server -config agentkit.toml          # whatever [mcp_server] selects
//	mcp-server -transport http -port 8722 -api-key-env MCP_API_KEY
//
// Exit 0 on a clean shutdown, 1 on a startup or transport failure.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentfox/agentkit-go/mcp"
)

func main() {
	var (
		configPath = flag.String("config", "", "TOML file whose [mcp_server] table selects the mode")
		transport  = flag.String("transport", "", `"stdio" or "http" (overrides the config)`)
		port       = flag.Int("port", 0, "TCP port for http mode (overrides the config)")
		apiKeyEnv  = flag.String("api-key-env", "", "environment variable holding the http API key")
	)
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	// Flags win over the file: an operator debugging a deployment should not
	// have to edit the config they are trying to reproduce.
	if *transport != "" {
		cfg.Transport, cfg.Enabled = *transport, true
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if *apiKeyEnv != "" {
		cfg.APIKeyEnv = *apiKeyEnv
	}
	if !cfg.Enabled && *configPath == "" {
		// Invoked with no config and no flags. Server mode is off by default
		// (REQ-MCP-SERVER-01), but a binary whose entire purpose is to serve
		// and that silently exits 0 looks like a crash; stdio is the safe mode
		// to assume, since it needs no port and no credential.
		cfg.Enabled, cfg.Transport = true, "stdio"
	}

	// Warnings go to STDERR, always. In stdio mode stdout carries the protocol
	// and a stray line there is a frame the client's decoder is poisoned by.
	warnf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "mcp-server: "+format+"\n", args...)
	}

	srv := mcp.NewServer(mcp.ServerOptions{
		Info:   mcp.Implementation{Name: "agentkit-mcp-server", Version: "0.1.0"},
		Warnf:  warnf,
		Limits: mcp.DefaultLimits(),
		Instructions: "A reference AgentKit MCP server. Tools here are for " +
			"demonstration; a host registers its own.",
	})
	if err := registerDemo(srv); err != nil {
		fail(err)
	}

	// SIGINT/SIGTERM cancel the context, which stops the listener and cancels
	// every in-flight handler.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx, cfg, os.Getenv); err != nil {
		fail(err)
	}
}

func loadConfig(path string) (mcp.ServerModeConfig, error) {
	if path == "" {
		return mcp.ServerModeConfig{}, nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return mcp.ServerModeConfig{}, err
	}
	cfg, diags, err := mcp.ParseConfig(path, src)
	for _, d := range diags {
		fmt.Fprintln(os.Stderr, "mcp-server: "+d.String())
	}
	if err != nil {
		return mcp.ServerModeConfig{}, err
	}
	return cfg.Server, nil
}

// registerDemo is the part a real host replaces.
func registerDemo(s *mcp.Server) error {
	if err := s.RegisterTool(mcp.ToolDefinition{
		Name:        "echo",
		Description: "Return the supplied message unchanged.",
		InputSchema: json.RawMessage(
			`{"type":"object","properties":{"message":{"type":"string","description":"text to return"}},"required":["message"]}`),
	}, func(_ context.Context, args map[string]any) (mcp.ToolsCallResult, error) {
		msg, ok := args["message"].(string)
		if !ok {
			return mcp.ToolsCallResult{}, fmt.Errorf("message must be a string")
		}
		return mcp.ToolsCallResult{Content: []mcp.Content{{Type: "text", Text: msg}}}, nil
	}); err != nil {
		return err
	}

	if err := s.RegisterResource(mcp.Resource{
		URI: "agentkit://server/info", Name: "server info", MimeType: "application/json",
	}, func(context.Context, string) (mcp.ResourcesReadResult, error) {
		body, err := json.Marshal(map[string]any{
			"protocol": mcp.ProtocolVersion,
			"tools":    mcp.SortedToolNames(s),
			"started":  time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			return mcp.ResourcesReadResult{}, err
		}
		return mcp.ResourcesReadResult{Contents: []mcp.ResourceContents{{
			URI: "agentkit://server/info", MimeType: "application/json", Text: string(body),
		}}}, nil
	}); err != nil {
		return err
	}

	// A template, so the reference host exercises the parameterised path too.
	return s.RegisterResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "agentkit://echo/{message}",
		Name:        "echoed message",
		Description: "Reads back whatever is in the URI.",
	}, func(_ context.Context, uri string, vars map[string]string) (mcp.ResourcesReadResult, error) {
		return mcp.ResourcesReadResult{Contents: []mcp.ResourceContents{{
			URI: uri, MimeType: "text/plain", Text: vars["message"],
		}}}, nil
	})
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mcp-server:", err)
	os.Exit(1)
}
