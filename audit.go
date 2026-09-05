package agentkit

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// This file is REQ-OBS-02..05: the audit trail and the tool span.

// ArgumentsHash is REQ-OBS-05's "arguments hash".
//
// A HASH, never the arguments. Tool arguments routinely carry file contents,
// credentials and personal data, and an audit trail is precisely the artifact
// that gets shipped to a log aggregator and retained for years. The hash gives
// correlation — the same call twice, the same call across sessions — without
// making the audit log the largest copy of the data it describes.
//
// It is taken over the RAW argument bytes, so two calls that differ only in
// the key order the model authored hash differently. That is deliberate and
// matches REQ-CACHE-01: on wires that carry arguments as a JSON string they
// are genuinely different calls.
func ArgumentsHash(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// MCPPrefix is the REQ-SEC-08 tool-name prefix for an MCP-qualified tool.
const MCPPrefix = "mcp__"

// MCPServerOf extracts the server name from a qualified tool name, or "" for a
// local tool.
//
// REQ-OBS-05 wants server_name on every MCP tool call. Deriving it from the
// name the prefixing convention already establishes means the field is right
// for any tool that follows the convention and EMPTY — rather than wrong — for
// one that does not, and it needs no separate plumbing through the batch
// executor for a subsystem that is not built yet.
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
