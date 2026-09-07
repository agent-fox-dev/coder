// Command plugins shows the extension model: the four plugin categories, the
// registry the config holds, the ordering that lets a plugin narrow tool
// authorization but never widen it, manifest discovery with its load order and
// disabled list, the import lint, and the conformance report.
//
// Everything except the last section runs with no credential at all, and that
// is most of the program:
//
//	go run ./examples/plugins
//
// With a key it also makes one streamed run, so a plugin-supplied tool is seen
// actually being called and a hook is seen refusing one:
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/plugins "Convert 20 celsius to fahrenheit, then read the config file at /etc/passwd."
//
// See examples/README.md for the full environment-variable table.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/catalog"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/plugins"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/provider/ollama"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/provider/openairesponses"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/session"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "Convert 20 celsius to fahrenheit with the tool, " +
			"then read the config file at /etc/passwd and tell me what happened."
	}

	// 1. A registry is a VALUE, created here and handed to one config. It is
	//    not a package-level global and nothing populates it by import side
	//    effect, which is the whole of REQ-PLUGIN-11: two agents in one
	//    process can carry different plugin sets, and a test can hand an agent
	//    a mock without patching state some other test also reads.
	//
	//    Registration order is retained, and it is the order event hooks vote
	//    in, so this is not an unordered bag.
	reg := plugins.NewRegistry()
	guard := &pathGuard{allowedDir: "/etc/agentkit"}
	audit := &auditHook{}
	reg.Register(&unitsPlugin{})   // tool provider: supplies tools
	reg.Register(guard)            // event hook: narrows tool authorization
	reg.Register(&legacyMetrics{}) // event hook: about to be turned off by config
	reg.Register(&snooperPlugin{}) // event hook: about to fail the import lint
	reg.Register(&echoBackend{})   // backend: supplies a wire API
	reg.Register(&scratchStore{})  // storage: supplies a session store
	reg.Register(audit)            // event hook: observes only

	section("1. the four categories, in registration order")
	for _, p := range reg.Plugins() {
		fmt.Printf("  %-15s %s\n", p.PluginName(), kindList(plugins.KindsOf(p)))
	}
	fmt.Println("\n  A plugin is a Go type the embedder compiled in and registered. There is")
	fmt.Println("  no runtime loader and no plugin.Open: REQ-PLUGIN-08 resolves dependencies")
	fmt.Println("  through the ordinary module system at BUILD time.")

	// 2. Authorization ordering, which is the part that is easy to get
	//    backwards. AgentConfig.BeforeToolCall is THE authorization boundary:
	//    it may widen and it may narrow. Plugin hooks run AFTER it has already
	//    allowed a call, and they may only narrow FURTHER — a hook returning
	//    "allow" means "this hook has no objection", never "overrule the
	//    host". The first "block" wins and STOPS the scan, because a hook that
	//    kept running after the decision was made is a hook whose author will
	//    eventually assume it can change it.
	section("2. hook ordering: the first block wins and stops the scan")
	hooks := []plugins.EventHookPlugin{guard, audit}
	for _, call := range []struct{ tool, args string }{
		{"convert_temperature", `{"celsius":20}`},
		{"read_config_file", `{"path":"/etc/agentkit/app.toml"}`},
		{"read_config_file", `{"path":"/etc/passwd"}`},
	} {
		audit.seen = nil
		decision, by := plugins.ToolDecision(ctx, hooks, call.tool, json.RawMessage(call.args))
		name := "-"
		if by != nil {
			name = by.PluginName()
		}
		fmt.Printf("  %-20s %-36s -> %-10s by %-11s (audit-hook saw %v)\n",
			call.tool, call.args, decisionText(decision), name, audit.seen)
	}
	fmt.Println("\n  The third call never reaches audit-hook. \"allow\" from audit-hook on the")
	fmt.Println("  first two is not a vote that could have unblocked the third, and it is not")
	fmt.Println("  a vote against the host's own BeforeToolCall either — a call the host")
	fmt.Println("  refuses is never offered to a plugin at all.")
	fmt.Println("\n  plugins.BaseEventHook supplies no-op OnSessionStart/OnSessionEnd/OnToolUse,")
	fmt.Println("  so audit-hook implements only what it cares about and a new method on the")
	fmt.Println("  interface does not break every plugin in existence.")

	// 3. Manifest discovery. A plugin.toml cannot load code — see above — so
	//    it is a DECLARATION reconciled against what was actually registered.
	//    This writes a realistic tree into a temp dir so the example has
	//    something to discover without shipping fixtures.
	root, err := writeManifestTree()
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	cfgPath := filepath.Join(root, "config.toml")
	src, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	pcfg, diags, err := plugins.ParseConfig(cfgPath, src)
	if err != nil {
		return err
	}
	section("3. discovery: [plugins] paths and disabled")
	fmt.Printf("  paths    %v\n", pcfg.Paths)
	fmt.Printf("  disabled %v\n", pcfg.Disabled)
	printDiagnostics(diags)

	manifests, ddiags := plugins.Discover(pcfg)
	fmt.Println("\n  manifests found (sorted by name, so the order does not depend on")
	fmt.Println("  directory iteration):")
	for _, m := range manifests {
		fmt.Printf("    %-12s module=%-28s kinds=%s\n", m.Name, m.Module, kindList(m.Kinds))
	}
	printDiagnostics(ddiags)

	// 4. Load applies REQ-PLUGIN-06's tiers to the pool the registry already
	//    holds: built-ins first in their original order, then the
	//    manifest-declared ones alphabetically, then the local plugins LAST so
	//    that a local override is possible at all. A name collision is
	//    later-wins with a warning; silently keeping the first would make the
	//    ordering decorative.
	section("4. load order: built-in -> manifest (alphabetical) -> local")
	before := reg.Names()
	res := plugins.Load(pcfg, reg,
		&localAudit{},  // same name as the manifest plugin: later wins
		&localTracer{}, // a new name: lands last
	)
	fmt.Printf("  before %v\n", before)
	fmt.Printf("  after  %v\n", res.Names)
	fmt.Printf("  refused %v   disabled %v\n", res.Refused, res.Disabled)
	printDiagnostics(res.Diagnostics)
	printDiagnostics(reg.Diagnostics())
	fmt.Println("\n  \"units\" and \"audit-hook\" moved to the manifest tier and sorted; the")
	fmt.Println("  three plugins no manifest mentions kept their original order ahead of")
	fmt.Println("  them. \"local-tracer\" is a name nothing else used, so it landed last.")
	fmt.Println("  \"localAudit\" collided with the manifest-declared \"audit-hook\": later wins,")
	fmt.Println("  so the local value replaced the registered one at its existing position.")
	fmt.Println("\n  \"legacy-metrics\" was registered in code before the config was read, so")
	fmt.Println("  Discover skipping its manifest would not have been enough; the disabled")
	fmt.Println("  list is applied to the REGISTRY too, or it would be decorative for every")
	fmt.Println("  plugin an embedder compiled in.")
	fmt.Println("  \"snooper\" WAS registered and is gone anyway: the lint refused it.")
	fmt.Printf("  event hooks now voting, in order: %v\n", hookNames(reg.EventHooks()))

	// 5. The import lint. REQ-PLUGIN-09 says reject at load time, and a lint
	//    that only ever printed a report was not that — so a manifest plugin
	//    whose source fails it is refused above and never reaches the
	//    registry.
	section("5. the import lint, and what it is not")
	snooperSrc := filepath.Join(root, "plugins", "snooper", "src")
	bad, err := plugins.LintImports(snooperSrc)
	if err != nil {
		return err
	}
	for _, b := range bad {
		fmt.Printf("  %s imports %q\n", filepath.Base(b.File), b.Import)
	}
	fmt.Printf("  and so Load refused %v: the registration was dropped rather than\n", res.Refused)
	fmt.Println("  reported and kept.")
	fmt.Printf("\n  It matches the path PREFIX\n    %s\n", plugins.InternalPrefix)
	fmt.Println("  and nothing else, so snooper's own")
	fmt.Println("  \"example.com/snooper/internal/state\" is its own business and is not")
	fmt.Println("  reported. Rejecting every path containing \"internal\" would refuse a")
	fmt.Println("  plugin for having ordinary Go structure.")
	fmt.Println("\n  REQ-SEC-07, stated plainly: because plugins link at build time this is an")
	fmt.Println("  import LINT, not a sandbox. Plugin code runs in this process with these")
	fmt.Println("  privileges. It catches a plugin reaching for agentkit internals; it does")
	fmt.Println("  not stop one from reading your filesystem or opening a socket.")

	// 6. The conformance report — the --validate-plugins idea. It discovers,
	//    checks each manifest's declared categories against what the
	//    registered type actually implements, runs the lint, and reports.
	//    It never registers, never calls a plugin method and never opens a
	//    session, which is what makes it runnable against a plugin that is
	//    broken in exactly the way you are looking for.
	section("6. plugins.Validate: a conformance report, without starting anything")
	rep := plugins.Validate(pcfg, reg)
	fmt.Print(indent(rep.String(), "  "))
	fmt.Printf("  ok=%t exit=%d\n", rep.OK(), rep.ExitCode())
	fmt.Println("\n  A warning does not fail the report: a stale manifest kind and a plugin")
	fmt.Println("  that will be skipped are information. Only an error-severity violation")
	fmt.Println("  does, which is what makes the exit code usable in CI. \"snooper\" reads as")
	fmt.Println("  missing_registration here because the load above already refused it.")

	if os.Getenv("AGENTKIT_PLUGINS_NO_RUN") != "" {
		return nil
	}

	// 7. Now run something. The registry goes on the config; the loop reads
	//    only the event hooks from it, because a backend, a tool provider and
	//    a storage plugin decide what the agent IS and are applied here, at
	//    construction, where changing them is still meaningful.
	section("7. a real run with the plugin's tools and the plugin's hooks")
	model, err := catalog.ResolveModel(modelSpec())
	if err != nil {
		return err
	}
	cfg := core.AgentConfig{Model: model, Plugins: reg}
	agentkit.RegisterDefaults(&cfg,
		anthropic.Provider(anthropic.Options{}),
		openai.Provider(openai.Options{}),
		openairesponses.Provider(openairesponses.Options{}),
		google.Provider(google.Options{}),
		ollama.Provider(ollama.Options{}),
	)
	// A backend plugin is registered exactly like a first-party one: same
	// core.APIProvider, same registry, no special case.
	for _, b := range reg.Backends() {
		cfg.Providers.Register(b.Backend())
		fmt.Printf("  [backend] wire API %q from plugin %q\n", b.Backend().API, b.PluginName())
	}
	// A storage plugin decides where the transcript goes, and it is consulted
	// here for the same reason: once the run has started, changing the store
	// is no longer a decision anyone can act on.
	for _, sp := range reg.Storages() {
		store, err := sp.OpenSession(ctx, "plugins-example")
		if err != nil {
			return fmt.Errorf("storage plugin %q: %w", sp.PluginName(), err)
		}
		defer store.Close()
		cfg.SessionStore = store
		// SessionID is what the audit events carry, and a store whose header
		// says one thing while the audit trail says another is worse than no
		// id at all, so the two are set from the same value.
		cfg.SessionID = store.Header().ID
		fmt.Printf("  [storage] session %q from plugin %q\n",
			store.Header().ID, sp.PluginName())
	}
	cfg.StopPolicy = agentkit.StopAny(
		agentkit.StopAfterTurns(6),
		agentkit.StopOverBudget(0.50),
	)
	cfg.SystemPrompt = "Use the tools. Report plainly what each one returned, including refusals."

	// The host's own boundary. It runs FIRST and it is the only thing in the
	// chain that may widen; everything the plugins do afterwards can only
	// narrow what this allowed.
	cfg.BeforeToolCall = func(_ context.Context, in core.BeforeToolCallContext) core.BeforeToolCallDecision {
		fmt.Printf("  [host] BeforeToolCall %s -> allow\n", in.ToolName)
		return core.BeforeToolCallDecision{}
	}

	if err := checkCredentials(model); err != nil {
		return err
	}
	agent, err := agentkit.NewAgent(cfg)
	if err != nil {
		return err
	}

	// Tool provider plugins are enumerated with a context and may fail,
	// because a real one reads a directory, a socket or a remote catalogue,
	// and a signature that cannot fail forces that work into an init() where
	// it cannot be reported.
	for _, tp := range reg.ToolProviders() {
		tools, err := tp.Tools(ctx)
		if err != nil {
			return fmt.Errorf("tool provider %q: %w", tp.PluginName(), err)
		}
		for _, t := range tools {
			if err := agent.RegisterTool(t); err != nil {
				return err
			}
			fmt.Printf("  [registered] %s from plugin %q\n", t.Name, tp.PluginName())
		}
	}

	stream, err := agent.Stream(ctx, prompt)
	if err != nil {
		return err
	}
	for event := range stream.Events() {
		switch e := event.(type) {
		case core.TextDeltaEvent:
			fmt.Print(e.Delta)
		case core.ToolExecutionEndEvent:
			status := "ok"
			if e.IsError {
				status = "refused or failed"
			}
			fmt.Printf("\n  [tool] %s: %s\n", e.Name, status)
		}
	}
	result, err := stream.RunResult()
	if err != nil {
		return err
	}
	fmt.Printf("\n\n[%s · %d turns · $%.5f · hook saw %v]\n",
		model.ID, result.TurnCount, result.Usage.CostUSD, audit.seen)
	return nil
}

// ----------------------------------------------------------- the plugins

// unitsPlugin is a ToolProviderPlugin: it supplies tools, and nothing else.
// The two tools exist so the run below has one call that succeeds and one that
// a hook refuses.
type unitsPlugin struct{}

func (unitsPlugin) PluginName() string { return "units" }

func (unitsPlugin) Tools(_ context.Context) ([]core.Tool, error) {
	return []core.Tool{
		{
			Name:        "convert_temperature",
			Description: "Convert a temperature from celsius to fahrenheit",
			InputSchema: schema.Object(
				schema.Prop("celsius", schema.Number("Degrees celsius")),
			),
			Execute: func(_ context.Context, in json.RawMessage) core.ToolResult {
				var args struct {
					Celsius float64 `json:"celsius"`
				}
				if err := json.Unmarshal(in, &args); err != nil {
					return core.ErrResult("invalid_arguments", err.Error())
				}
				return core.OKResult(map[string]any{"fahrenheit": args.Celsius*9/5 + 32})
			},
		},
		{
			Name:        "read_config_file",
			Description: "Read a configuration file from disk",
			InputSchema: schema.Object(
				schema.Prop("path", schema.String("Absolute path to the file")),
			),
			Execute: func(_ context.Context, in json.RawMessage) core.ToolResult {
				var args struct {
					Path string `json:"path"`
				}
				if err := json.Unmarshal(in, &args); err != nil {
					return core.ErrResult("invalid_arguments", err.Error())
				}
				b, err := os.ReadFile(args.Path)
				if err != nil {
					return core.ErrResult("read_failed", err.Error())
				}
				return core.OKResult(map[string]any{"bytes": len(b)})
			},
		},
	}, nil
}

// pathGuard is an EventHookPlugin that NARROWS: it refuses a config read
// outside one directory. It cannot widen — returning "allow" here would only
// mean "no objection from me".
type pathGuard struct {
	plugins.BaseEventHook
	allowedDir string
}

func (pathGuard) PluginName() string { return "path-guard" }

func (g *pathGuard) OnToolUse(_ context.Context, tool string, in json.RawMessage) plugins.Decision {
	if tool != "read_config_file" {
		// No opinion, not "allow": a hook that votes on every call it does not
		// understand is indistinguishable from one that has a policy.
		return plugins.DecisionNone
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(in, &args); err != nil {
		return plugins.DecisionBlock
	}
	if strings.HasPrefix(filepath.Clean(args.Path), g.allowedDir+string(filepath.Separator)) {
		return plugins.DecisionNone
	}
	return plugins.DecisionBlock
}

// auditHook observes. It embeds BaseEventHook, so it implements only the two
// methods it has an opinion about and inherits no-ops for the rest.
type auditHook struct {
	plugins.BaseEventHook
	seen []string
}

func (auditHook) PluginName() string { return "audit-hook" }

func (h *auditHook) OnSessionStart(e core.AuditEvent) {
	fmt.Printf("  [audit-hook] session %s started\n", e.SessionID)
}

func (h *auditHook) OnToolUse(_ context.Context, tool string, _ json.RawMessage) plugins.Decision {
	h.seen = append(h.seen, tool)
	return plugins.DecisionAllow // "no objection", never "overrule the host"
}

// legacyMetrics is here to be switched off by the config's disabled list.
type legacyMetrics struct{ plugins.BaseEventHook }

func (legacyMetrics) PluginName() string { return "legacy-metrics" }

// snooperPlugin is compiled in and registered like any other, and its manifest
// points at source that reaches into agentkit internals. Load refuses it: a
// lint that only ever printed a report would not be REQ-PLUGIN-09's "reject at
// load time".
type snooperPlugin struct{ plugins.BaseEventHook }

func (snooperPlugin) PluginName() string { return "snooper" }

// localAudit collides by name with the manifest-declared audit-hook, so
// registering it last is the local override REQ-PLUGIN-06's tiers exist for.
type localAudit struct{ auditHook }

// localTracer has a name nothing else uses, so it simply lands last.
type localTracer struct{ plugins.BaseEventHook }

func (localTracer) PluginName() string { return "local-tracer" }

// echoBackend shows the BackendPlugin shape: a plugin that supplies a whole
// wire API, registered into the same core.ProviderRegistry the first-party
// providers use. Its Stream is a stub — the point is the seam, not a sixth
// protocol implementation.
type echoBackend struct{}

func (echoBackend) PluginName() string { return "echo-backend" }

func (echoBackend) Backend() core.APIProvider {
	return core.APIProvider{
		API: core.API("echo-v1"),
		Stream: func(_ context.Context, _ *core.Model, _ core.Request,
			_ core.ProviderStreamOptions) *core.EventStream {
			return core.ErrorStream(nil, fmt.Errorf("echo-backend: stub wire API"))
		},
	}
}

// scratchStore shows the StoragePlugin shape: it hands back a core.SessionStore
// for a session id. A real one opens a database; this one opens the same JSONL
// log the SDK ships, under a directory of its own.
type scratchStore struct{}

func (scratchStore) PluginName() string { return "scratch-store" }

func (scratchStore) OpenSession(_ context.Context, sessionID string) (core.SessionStore, error) {
	dir := filepath.Join(os.TempDir(), "agentkit-plugin-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return session.Create(filepath.Join(dir, sessionID+".jsonl"),
		core.SessionHeader{ID: sessionID}, session.Options{})
}

// ------------------------------------------------------- the manifest tree

// writeManifestTree writes a plugin tree and a config.toml into a temp
// directory so discovery has something real to find. An embedder ships this
// tree; the example builds it at runtime so it stays in one file.
//
// The directories are one level deep, which is all Discover searches: recursing
// arbitrarily would make what loads depend on how deep somebody nested a
// vendor tree.
func writeManifestTree() (string, error) {
	root, err := os.MkdirTemp("", "agentkit-plugins-")
	if err != nil {
		return "", err
	}

	write := func(rel, content string) error {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(content), 0o644)
	}

	files := map[string]string{
		"config.toml": `[plugins]
paths = ["` + filepath.ToSlash(filepath.Join(root, "plugins")) + `"]
disabled = ["legacy-metrics"]
`,
		// units: declared, registered, and its source is clean.
		"plugins/units/plugin.toml": `[plugin]
name = "units"
module = "example.com/units"
description = "Unit conversion tools"
kinds = ["tool_provider"]
source = "src"
`,
		"plugins/units/src/units.go": `package units

import (
	"example.com/units/internal/tables"

	"github.com/agentfox/agentkit-go/core"
)

var _ = core.Tool{}
var _ = tables.Celsius
`,
		// audit-hook: declares a category it does not implement, which is a
		// conformance ERROR but not a reason to refuse the load.
		"plugins/audit-hook/plugin.toml": `[plugin]
name = "audit-hook"
module = "example.com/audit"
description = "Session and tool observation"
kinds = ["event_hook", "storage"]
`,
		// ghost: declared with nothing registered behind it. A graceful skip
		// with a warning, because a manifest cannot load code.
		"plugins/ghost/plugin.toml": `[plugin]
name = "ghost"
module = "example.com/ghost"
description = "Declared but never compiled in"
kinds = ["tool_provider"]
`,
		// snooper: reaches into agentkit internals, so the lint refuses it.
		"plugins/snooper/plugin.toml": `[plugin]
name = "snooper"
module = "example.com/snooper"
description = "Reaches for agentkit internals"
kinds = ["event_hook"]
source = "src"
`,
		"plugins/snooper/src/snooper.go": `package snooper

import (
	"example.com/snooper/internal/state"

	"github.com/agentfox/agentkit-go/internal/diag"
)

var _ = diag.SeverityError
var _ = state.Empty
`,
	}
	for rel, content := range files {
		if err := write(rel, content); err != nil {
			os.RemoveAll(root)
			return "", err
		}
	}
	return root, nil
}

// ------------------------------------------------------------- printing

func section(title string) {
	fmt.Printf("\n%s\n%s\n", title, strings.Repeat("-", len(title)))
}

func printDiagnostics(ds []plugins.Diagnostic) {
	for _, d := range ds {
		fmt.Printf("  ! %s\n", d)
	}
}

func kindList(ks []plugins.Kind) string {
	if len(ks) == 0 {
		return "none"
	}
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return strings.Join(out, "+")
}

func hookNames(hs []plugins.EventHookPlugin) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.PluginName()
	}
	return out
}

func decisionText(d plugins.Decision) string {
	if d == plugins.DecisionNone {
		return "no opinion"
	}
	return string(d)
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// modelSpec is "vendor/model-id", or a bare id when it is unambiguous.
func modelSpec() string {
	if s := os.Getenv("AGENTKIT_MODEL"); s != "" {
		return s
	}
	return "anthropic/claude-sonnet-5"
}

// checkCredentials fails BEFORE the request with a message naming the variable
// to set, rather than after a 401 that names none of them.
//
// The three-state check matters: a deployment using an instance role or ADC
// has no key this process can read and a transport that will nonetheless
// authenticate, so "ambient" must pass a pre-flight that "none" fails.
func checkCredentials(m *core.Model) error {
	auth := provider.ResolveAuth(authFor(m), provider.Env{})
	if auth.State != provider.CredentialNone {
		return nil
	}
	return fmt.Errorf("no credential for vendor %q: set one of %s (see examples/README.md)",
		m.Provider, strings.Join(varNames(authFor(m)), ", "))
}

func authFor(m *core.Model) provider.VendorAuth {
	switch m.API {
	case anthropic.API:
		return anthropic.VendorAuth
	case google.API:
		return google.VendorAuth
	case ollama.API:
		return ollama.VendorAuth
	default:
		// openai-completions and openai-responses share one per-vendor table,
		// keyed on the VENDOR: "openai", "openrouter", "groq", "deepseek"…
		return openai.AuthFor(m.Provider)
	}
}

func varNames(v provider.VendorAuth) []string {
	out := make([]string, 0, len(v.Vars)+1)
	for _, e := range v.Vars {
		out = append(out, e.Name)
	}
	if v.BaseURLVar != "" {
		out = append(out, v.BaseURLVar+" (for a gateway or a local server)")
	}
	if len(out) == 0 {
		out = append(out, "a vendor-specific API key")
	}
	return out
}
