package mcp

import (
	"fmt"
	"strings"
	"time"

	"github.com/agentfox/agentkit-go/internal/diag"
	"github.com/agentfox/agentkit-go/internal/toml"
)

// Diagnostic is the shared non-fatal report.
type Diagnostic = diag.Diagnostic

// Config is the `[mcp]` section (REQ-MCP-CLIENT-07) and `[mcp_server]`
// (REQ-MCP-SERVER-01).
type Config struct {
	Servers []ServerConfig
	// Server is the inbound half. Enabled defaults to FALSE and there is no
	// way for a missing key to turn it on (REQ-MCP-SERVER-01).
	Server ServerModeConfig
}

// ServerModeConfig is `[mcp_server]`.
type ServerModeConfig struct {
	Enabled bool
	// Transport is "stdio" or "http" (REQ-MCP-SERVER-02).
	Transport string
	Port      int
	// APIKeyEnv names the environment variable holding the HTTP API key. The
	// KEY itself is deliberately not a config field: a credential in a config
	// file is a credential in version control.
	APIKeyEnv string
}

// ParseConfig reads the `[mcp]` and `[mcp_server]` sections.
//
// Config is LOCALLY AUTHORED, so it decodes leniently (REQ-SEC-12.5): an
// unknown key is a diagnostic. The opposite of the wire package's rule, and
// the difference is who wrote the bytes.
func ParseConfig(path string, src []byte) (Config, []Diagnostic, error) {
	root, diags, err := toml.ParseTOML(src)
	if err != nil {
		return Config{}, diags, fmt.Errorf("mcp: %s: %w", path, err)
	}

	var cfg Config
	if mcpTbl, ok := root.Sub("mcp"); ok {
		servers, _ := mcpTbl.Array("servers")
		seen := map[string]bool{}
		for i, st := range servers {
			sc, sdiags := parseServer(path, i, st)
			diags = append(diags, sdiags...)
			if sc.Name == "" {
				continue
			}
			if seen[sc.Name] {
				diags = append(diags, Diagnostic{Path: path, Severity: diag.SeverityError,
					Message: fmt.Sprintf("two [[mcp.servers]] entries are named %q; the "+
						"name keys the pool, the tool prefix and every audit event", sc.Name)})
				continue
			}
			seen[sc.Name] = true
			cfg.Servers = append(cfg.Servers, sc)
		}
	}

	if srvTbl, ok := root.Sub("mcp_server"); ok {
		if v, ok := srvTbl.Get("enabled"); ok && v.Kind == toml.KindBool {
			cfg.Server.Enabled = v.Bool
		}
		if v, ok := srvTbl.Get("transport"); ok && v.Kind == toml.KindString {
			cfg.Server.Transport = v.Str
		}
		if v, ok := srvTbl.Get("port"); ok && v.Kind == toml.KindInt {
			cfg.Server.Port = int(v.Int)
		}
		if v, ok := srvTbl.Get("api_key_env"); ok && v.Kind == toml.KindString {
			cfg.Server.APIKeyEnv = v.Str
		}
	}
	if cfg.Server.Transport == "" {
		cfg.Server.Transport = "stdio"
	}
	if cfg.Server.Enabled && cfg.Server.Transport == "http" && cfg.Server.APIKeyEnv == "" {
		diags = append(diags, Diagnostic{Path: path, Severity: diag.SeverityError,
			Message: "[mcp_server] transport is http with no api_key_env; HTTP mode " +
				"requires authentication and will refuse to start (REQ-MCP-SERVER-07)"})
	}
	return cfg, diags, nil
}

func parseServer(path string, i int, t *toml.Table) (ServerConfig, []Diagnostic) {
	var (
		sc    ServerConfig
		diags []Diagnostic
	)
	where := fmt.Sprintf("[[mcp.servers]] #%d", i+1)

	str := func(key string) string {
		if v, ok := t.Get(key); ok && v.Kind == toml.KindString {
			return v.Str
		}
		return ""
	}

	sc.Name = strings.TrimSpace(str("name"))
	sc.Command = str("command")
	sc.URL = str("url")
	sc.Dir = str("dir")
	sc.ToolPrefix = str("tool_prefix")

	if v, ok := t.Get("args"); ok && v.Kind == toml.KindStringArray {
		sc.Args = append([]string(nil), v.Array...)
	}
	if v, ok := t.Get("allow_sampling"); ok && v.Kind == toml.KindBool {
		sc.AllowSampling = v.Bool
	}
	if v, ok := t.Get("per_session_call_limit"); ok && v.Kind == toml.KindInt {
		sc.PerSessionCallLimit = int(v.Int)
	}
	if v, ok := t.Get("timeout_s"); ok && v.Kind == toml.KindInt {
		sc.Timeout = time.Duration(v.Int) * time.Second
	}
	if hdrTbl, ok := t.Sub("headers"); ok {
		sc.Headers = map[string]string{}
		for _, k := range hdrTbl.Keys() {
			if v, ok := hdrTbl.Get(k); ok && v.Kind == toml.KindString {
				sc.Headers[k] = v.Str
			}
		}
	}
	if envTbl, ok := t.Sub("env"); ok {
		sc.Env = map[string]string{}
		for _, k := range envTbl.Keys() {
			if v, ok := envTbl.Get(k); ok && v.Kind == toml.KindString {
				sc.Env[k] = v.Str
			}
		}
	}

	switch {
	case sc.Name == "":
		diags = append(diags, Diagnostic{Path: path, Severity: diag.SeverityError,
			Message: where + ": name is required"})
	case sc.Command == "" && sc.URL == "":
		diags = append(diags, Diagnostic{Path: path, Severity: diag.SeverityError,
			Message: fmt.Sprintf("%s (%q): needs a command or a url", where, sc.Name)})
		sc.Name = "" // not usable
	case sc.Command != "" && sc.URL != "":
		diags = append(diags, Diagnostic{Path: path, Severity: diag.SeverityWarning,
			Message: fmt.Sprintf("%s (%q): both command and url are set; the command wins "+
				"and the url is ignored", where, sc.Name)})
	}

	if t := str("transport"); t != "" {
		// 2026-07-28 leaves exactly one remote transport, so the field has
		// nothing left to select. It is still PARSED rather than ignored: an
		// operator who wrote `transport = "sse"` configured a transport this
		// build removed, and silently serving them Streamable HTTP would hide
		// that their server is probably unreachable.
		sev := diag.SeverityWarning
		msg := fmt.Sprintf("%s (%q): transport %q is obsolete; MCP %s defines only "+
			"Streamable HTTP and this build implements only that",
			where, sc.Name, t, ProtocolVersion)
		if t != "streamable-http" {
			sev = diag.SeverityError
			msg = fmt.Sprintf("%s (%q): transport %q is not implemented; MCP %s removed "+
				"it and this build is modern-only", where, sc.Name, t, ProtocolVersion)
			sc.Name = "" // not usable
		}
		diags = append(diags, Diagnostic{Path: path, Severity: sev, Message: msg})
	}
	return sc, diags
}
