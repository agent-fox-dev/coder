// Package plugins is §6.6: the four plugin categories, their registry,
// manifest discovery, and the load-time checks.
//
// # What "plugin" means here, and what it does not
//
// REQ-PLUGIN-08 settles the mechanism: dependencies are resolved at BUILD time
// through the ordinary Go module system. There is no runtime loader, no
// `plugin.Open`, no dlopen. A plugin is a Go type the embedder compiled in and
// registered.
//
// That makes the manifest a DECLARATION rather than a loader input, and the
// distinction is the whole shape of this package. A plugin.toml says "a plugin
// named X, from module M, providing these categories, is expected here". The
// registry says what was actually linked and registered. Discovery reconciles
// the two: a manifest with nothing registered behind it is a graceful skip
// with a warning (REQ-PLUGIN-08), and a registration whose manifest declares
// categories it does not implement is a conformance violation (REQ-PLUGIN-10).
//
// REQ-SEC-07 states the honest limit and it is worth repeating at the top of
// the file rather than burying it: with build-time linkage, REQ-PLUGIN-09's
// import restriction is an IMPORT-PATH LINT, not a sandbox. Plugin code runs
// in the same process with the same privileges as everything else. The lint
// catches a plugin that reaches for agentkit/internal; it does not stop one
// that decides to read your filesystem.
package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/internal/diag"
)

// Diagnostic is the shared non-fatal report.
type Diagnostic = diag.Diagnostic

// Severity is the shared severity.
type Severity = diag.Severity

const (
	SeverityWarning = diag.SeverityWarning
	SeverityError   = diag.SeverityError
)

// Kind names one of REQ-PLUGIN-01's four categories.
type Kind string

const (
	KindBackend      Kind = "backend"
	KindToolProvider Kind = "tool_provider"
	KindStorage      Kind = "storage"
	KindEventHook    Kind = "event_hook"
)

// AllKinds is the closed set, for validation and for error messages.
var AllKinds = []Kind{KindBackend, KindToolProvider, KindStorage, KindEventHook}

func validKind(k Kind) bool {
	for _, v := range AllKinds {
		if v == k {
			return true
		}
	}
	return false
}

// The category interfaces are declared in core, because AgentConfig has to
// hold a registry and every one of them is already expressed in core's
// vocabulary. They are aliased here so a plugin author imports one package.
type (
	Plugin             = core.Plugin
	BackendPlugin      = core.BackendPlugin
	ToolProviderPlugin = core.ToolProviderPlugin
	StoragePlugin      = core.StoragePlugin
	EventHookPlugin    = core.EventHookPlugin
	Decision           = core.PluginDecision
)

const (
	// DecisionNone is "no opinion" — the empty string, so the zero value of a
	// hook that has not been implemented abstains rather than voting.
	DecisionNone  = core.PluginNoOpinion
	DecisionAllow = core.PluginAllow
	DecisionBlock = core.PluginBlock
)

// BaseEventHook is REQ-PLUGIN-03: default no-op implementations, embedded.
//
// It exists so that adding a method to EventHookPlugin does not break every
// plugin in existence — which is the difference between an interface an
// ecosystem can live with and one that pins the SDK's version to its least
// maintained consumer.
type BaseEventHook struct{}

func (BaseEventHook) OnSessionStart(core.AuditEvent) {}
func (BaseEventHook) OnSessionEnd(core.AuditEvent)   {}
func (BaseEventHook) OnToolUse(context.Context, string, json.RawMessage) Decision {
	return DecisionNone
}

// ---------------------------------------------------------------- registry

// Registry holds registered plugins in REGISTRATION ORDER (REQ-PLUGIN-04).
//
// It is a value held on the config, never a package-level global
// (REQ-PLUGIN-11): a global would have to be frozen against late registration,
// and two agents in one process could not carry different plugin sets — which
// is exactly what a test injecting a mock needs.
type Registry struct {
	order []Plugin
	index map[string]int
	diags []Diagnostic
}

func NewRegistry() *Registry { return &Registry{index: map[string]int{}} }

// Register adds a plugin. REQ-PLUGIN-06: on a name collision the LATER
// registration wins, with a warning.
//
// Later-wins is what makes the load order in REQ-PLUGIN-06 meaningful:
// built-ins first, manifest plugins next, local plugins last, so a local
// override is possible at all. Silently keeping the first would make the
// ordering decorative.
func (r *Registry) Register(p Plugin) {
	if r.index == nil {
		r.index = map[string]int{}
	}
	name := ""
	if p != nil {
		name = p.PluginName()
	}
	if name == "" {
		r.diags = append(r.diags, Diagnostic{Severity: SeverityError,
			Message: "plugin registered with an empty name; it can never be disabled or overridden"})
		return
	}
	if i, exists := r.index[name]; exists {
		r.diags = append(r.diags, Diagnostic{Severity: SeverityWarning,
			Message: fmt.Sprintf("plugin %q re-registered; the later registration wins", name)})
		r.order[i] = p
		return
	}
	r.index[name] = len(r.order)
	r.order = append(r.order, p)
}

// Remove drops a plugin by name. It is how REQ-PLUGIN-07's disabled list is
// applied to a registry that was populated before the config was read.
func (r *Registry) Remove(name string) bool {
	i, ok := r.index[name]
	if !ok {
		return false
	}
	r.order = append(r.order[:i], r.order[i+1:]...)
	delete(r.index, name)
	for n, j := range r.index {
		if j > i {
			r.index[n] = j - 1
		}
	}
	return true
}

// Plugins returns everything, in registration order.
func (r *Registry) Plugins() []Plugin { return append([]Plugin(nil), r.order...) }

// Names returns the registered names, in registration order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	for i, p := range r.order {
		out[i] = p.PluginName()
	}
	return out
}

// Diagnostics returns what registration reported.
func (r *Registry) Diagnostics() []Diagnostic { return append([]Diagnostic(nil), r.diags...) }

// Backends, ToolProviders, Storages and EventHooks project the registry by
// category, preserving registration order.
func (r *Registry) Backends() []BackendPlugin {
	var out []BackendPlugin
	for _, p := range r.order {
		if b, ok := p.(BackendPlugin); ok {
			out = append(out, b)
		}
	}
	return out
}

func (r *Registry) ToolProviders() []ToolProviderPlugin {
	var out []ToolProviderPlugin
	for _, p := range r.order {
		if t, ok := p.(ToolProviderPlugin); ok {
			out = append(out, t)
		}
	}
	return out
}

func (r *Registry) Storages() []StoragePlugin {
	var out []StoragePlugin
	for _, p := range r.order {
		if s, ok := p.(StoragePlugin); ok {
			out = append(out, s)
		}
	}
	return out
}

func (r *Registry) EventHooks() []EventHookPlugin {
	var out []EventHookPlugin
	for _, p := range r.order {
		if h, ok := p.(EventHookPlugin); ok {
			out = append(out, h)
		}
	}
	return out
}

// KindsOf reports which categories a plugin actually implements.
func KindsOf(p Plugin) []Kind {
	var out []Kind
	if _, ok := p.(BackendPlugin); ok {
		out = append(out, KindBackend)
	}
	if _, ok := p.(ToolProviderPlugin); ok {
		out = append(out, KindToolProvider)
	}
	if _, ok := p.(StoragePlugin); ok {
		out = append(out, KindStorage)
	}
	if _, ok := p.(EventHookPlugin); ok {
		out = append(out, KindEventHook)
	}
	return out
}

// ---------------------------------------------------------------- composition

// ToolDecision runs the event hooks over one tool call (REQ-PLUGIN-04).
//
// Hooks run in REGISTRATION ORDER and the first "block" WINS — the scan stops
// there, so a later hook cannot un-block what an earlier one refused.
//
// "allow" is NOT a veto over the embedder's BeforeToolCall interceptor, and
// this is REQ-PLUGIN-04 as amended by REQ-SEC-03.5. The interceptor is the
// authorization boundary and may both widen and narrow; hooks compose with it
// and may only narrow further. The original ordering — a static allowlist
// running ahead of hooks — is gone, and with it the idea that anything in the
// SDK runs before the interceptor in a way it cannot override. So this is
// called AFTER the interceptor has allowed a call, and an "allow" here means
// only "this hook does not object".
func ToolDecision(ctx context.Context, hooks []EventHookPlugin, toolName string,
	toolInput json.RawMessage) (Decision, EventHookPlugin) {

	out := DecisionNone
	for _, h := range hooks {
		switch h.OnToolUse(ctx, toolName, toolInput) {
		case DecisionBlock:
			return DecisionBlock, h
		case DecisionAllow:
			if out == DecisionNone {
				out = DecisionAllow
			}
		}
	}
	return out, nil
}

// SortManifestsByName is REQ-PLUGIN-06's middle tier: manifest-declared
// plugins load alphabetically by name, so the order does not depend on
// directory iteration.
func SortManifestsByName(ms []Manifest) {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
}

func joinNames(ks []Kind) string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}
