package agentkit

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// This file is REQ-OBS-02..05: the audit trail and the tool span.

// ArgumentsHash is core.HashArguments, kept here because it was part of this
// package's surface before the MCP client needed it too.
func ArgumentsHash(raw []byte) string { return core.HashArguments(raw) }

// MCPPrefix is the REQ-SEC-08 tool-name prefix for an MCP-qualified tool.
const MCPPrefix = "mcp__"

// serverNameOf resolves REQ-OBS-05's server_name.
//
// The TOOL's own field wins. Deriving it from the qualified name was the
// original approach and it was wrong: REQ-MCP-CLIENT-05's convention is
// `server_name__tool_name` with a CONFIGURABLE prefix, so a local tool named
// `a__b` is indistinguishable from server `a`'s tool `b`, and a server
// configured with an empty prefix carries no server in the name at all. The
// layer that opened the connection is the only one that knows.
//
// MCPServerOf remains as a fallback for a tool assembled without the field.
func serverNameOf(t core.Tool, name string) string {
	if t.MCPServer != "" {
		return t.MCPServer
	}
	return MCPServerOf(name)
}

// MCPServerOf extracts a server name from a tool name carrying the `mcp__`
// prefix. It is a FALLBACK: prefer core.Tool.MCPServer, which is authoritative.
func MCPServerOf(toolName string) string {
	rest, ok := strings.CutPrefix(toolName, MCPPrefix)
	if !ok {
		return ""
	}
	server, _, ok := strings.Cut(rest, "__")
	if !ok {
		return ""
	}
	return server
}

func (a *Agent) sessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.SessionID
}

// audit stamps and dispatches one event.
//
// Both the kind-specific hook and OnAudit fire, so a single sink registers
// once while a caller who wants only session boundaries is not handed every
// tool call. Every hook goes through the same recovering wrapper the turn
// hooks use: hooks are Axis 2 — observation, never interception (REQ-OBS-07) —
// so a panicking one must not be able to change the outcome, and a panic is
// the loudest way to change one.
func (a *Agent) audit(e core.AuditEvent) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	h := a.hooks()
	if e.SessionID == "" {
		e.SessionID = a.sessionID()
	}

	switch e.Kind {
	case core.AuditSessionStart:
		safely(h.OnError, "OnSessionStart", func() {
			if h.OnSessionStart != nil {
				h.OnSessionStart(e)
			}
		})
	case core.AuditSessionEnd:
		safely(h.OnError, "OnSessionEnd", func() {
			if h.OnSessionEnd != nil {
				h.OnSessionEnd(e)
			}
		})
	}
	safely(h.OnError, "OnAudit", func() {
		if h.OnAudit != nil {
			h.OnAudit(e)
		}
	})
}

// AuditSkills records the loaded skill set (REQ-OBS-04).
//
// It is exported because skill loading is the embedder's call — the skills
// package discovers and merges, and the agent is handed the result — so the
// agent cannot observe it without being told.
func (a *Agent) AuditSkills(names []string) {
	a.audit(core.AuditEvent{Kind: core.AuditSkillsLoaded,
		Skills: append([]string(nil), names...)})
}

// pluginVeto runs REQ-PLUGIN-04's event hooks over one tool call.
//
// Hooks run in REGISTRATION ORDER and the first "block" WINS — the scan stops
// there, so a later hook cannot un-block what an earlier one refused. A
// panicking hook is contained: a plugin is third-party code in this process,
// and letting it take the run down would make every plugin a liveness risk for
// the host.
func pluginVeto(ctx context.Context, reg core.PluginRegistry, toolName string,
	input json.RawMessage) (core.PluginDecision, core.EventHookPlugin) {

	if reg == nil {
		return core.PluginNoOpinion, nil
	}
	for _, h := range reg.EventHooks() {
		d := core.PluginNoOpinion
		func() {
			defer func() { _ = recover() }()
			d = h.OnToolUse(ctx, toolName, input)
		}()
		if d == core.PluginBlock {
			return core.PluginBlock, h
		}
	}
	return core.PluginNoOpinion, nil
}
