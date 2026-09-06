package core

import (
	"context"
	"encoding/json"
)

// The four plugin category interfaces of REQ-PLUGIN-01/02 live in core for the
// same reason Tracer does: AgentConfig has to hold a registry, and every one of
// these interfaces is already expressed in core's own vocabulary —
// APIProvider, Tool, SessionStore, AuditEvent. Declaring them in the plugins
// package and referring to them from AgentConfig would be a cycle; declaring a
// parallel set would be two vocabularies for one concept.
//
// core holds declarations and interface seams. The machinery — the registry,
// manifest discovery, the import lint, the conformance report — is the
// `plugins` package, which aliases these.

// Plugin is what every category shares.
//
// PluginName rather than Name: a plugin is very often also a Tool or a
// provider that already has a Name, and a collision there would force an
// embedder to wrap rather than implement.
type Plugin interface {
	PluginName() string
}

// BackendPlugin supplies a new wire API (REQ-PROV-09).
type BackendPlugin interface {
	Plugin
	Backend() APIProvider
}

// ToolProviderPlugin supplies tools.
//
// It takes a context and returns an error because a real one enumerates
// something — a directory, a socket, a remote catalogue — and a signature that
// cannot fail forces that work into an init() where it cannot be reported.
type ToolProviderPlugin interface {
	Plugin
	Tools(ctx context.Context) ([]Tool, error)
}

// StoragePlugin supplies a session store.
type StoragePlugin interface {
	Plugin
	OpenSession(ctx context.Context, sessionID string) (SessionStore, error)
}

// PluginDecision is REQ-PLUGIN-02's tri-state.
type PluginDecision string

const (
	// PluginNoOpinion is the empty string, so a hook that has not implemented
	// OnToolUse abstains rather than voting.
	PluginNoOpinion PluginDecision = ""
	PluginAllow     PluginDecision = "allow"
	PluginBlock     PluginDecision = "block"
)

// EventHookPlugin observes the run and may NARROW tool authorization.
//
// SIGNATURE NOTE: REQ-PLUGIN-02 writes OnToolUse with the context LAST. It is
// first here. Go's convention is not decoration — a context in any other
// position is invisible to every linter and to every reader scanning for
// cancellation — and the requirement's ordering reads as transcription rather
// than intent.
type EventHookPlugin interface {
	Plugin
	OnSessionStart(e AuditEvent)
	OnSessionEnd(e AuditEvent)
	OnToolUse(ctx context.Context, toolName string, toolInput json.RawMessage) PluginDecision
}

// PluginRegistry is the seam AgentConfig holds (REQ-PLUGIN-11).
//
// It is deliberately narrow: the LOOP only ever needs the event hooks. A
// backend, a tool provider and a storage plugin are applied by the embedder at
// construction — they decide what the agent IS — so widening this interface
// would let the loop reach for them at a point where changing any of them is
// already too late.
type PluginRegistry interface {
	EventHooks() []EventHookPlugin
}
