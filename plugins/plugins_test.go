package plugins_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/plugins"
)

// ---- doubles

type namedHook struct {
	plugins.BaseEventHook
	name    string
	verdict plugins.Decision
	calls   *[]string
}

func (h *namedHook) PluginName() string { return h.name }
func (h *namedHook) OnToolUse(_ context.Context, tool string, _ json.RawMessage) plugins.Decision {
	if h.calls != nil {
		*h.calls = append(*h.calls, h.name)
	}
	return h.verdict
}

type backendOnly struct{ name string }

func (b backendOnly) PluginName() string        { return b.name }
func (b backendOnly) Backend() core.APIProvider { return core.APIProvider{API: "x"} }

type toolsOnly struct{ name string }

func (t toolsOnly) PluginName() string { return t.name }
func (t toolsOnly) Tools(context.Context) ([]core.Tool, error) {
	return []core.Tool{{Name: "from_plugin"}}, nil
}

// ---- registry

// TestABaseEventHookAbstains is REQ-PLUGIN-03.
//
// The embedded base exists so that adding a method to EventHookPlugin does not
// break every plugin in existence, and its default must be NO OPINION rather
// than allow: a plugin that has not thought about tool authorization must not
// be casting votes about it.
func TestABaseEventHookAbstains(t *testing.T) {
	var base plugins.BaseEventHook
	if got := base.OnToolUse(context.Background(), "anything", nil); got != plugins.DecisionNone {
		t.Fatalf("default OnToolUse = %q, want no opinion", got)
	}
	// And the zero value satisfies the interface, which is the point of it.
	var _ interface {
		OnSessionStart(core.AuditEvent)
		OnSessionEnd(core.AuditEvent)
	} = base
}

// TestALaterRegistrationWins is REQ-PLUGIN-06.
//
// Later-wins is what makes the load ORDER meaningful: built-ins, then
// manifest plugins, then local ones, so a local override is possible at all.
// Silently keeping the first would make that ordering decorative.
func TestALaterRegistrationWins(t *testing.T) {
	r := plugins.NewRegistry()
	first := &namedHook{name: "audit"}
	second := &namedHook{name: "audit", verdict: plugins.DecisionBlock}
	r.Register(first)
	r.Register(second)

	if names := r.Names(); len(names) != 1 || names[0] != "audit" {
		t.Fatalf("names = %v, want one entry", names)
	}
	if got := r.EventHooks()[0]; got != core.EventHookPlugin(second) {
		t.Fatal("the LATER registration must win")
	}
	var warned bool
	for _, d := range r.Diagnostics() {
		if strings.Contains(d.Message, "re-registered") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("a collision must be reported; a silent override is a feature that " +
			"disappears without explanation")
	}
}

func TestAnEmptyPluginNameIsRejected(t *testing.T) {
	r := plugins.NewRegistry()
	r.Register(&namedHook{name: ""})
	if len(r.Names()) != 0 {
		t.Fatal("a nameless plugin can never be disabled or overridden and must not register")
	}
	if len(r.Diagnostics()) == 0 {
		t.Fatal("the rejection must be reported")
	}
}

func TestRegistrationOrderIsPreserved(t *testing.T) {
	r := plugins.NewRegistry()
	for _, n := range []string{"c", "a", "b"} {
		r.Register(&namedHook{name: n})
	}
	if got := strings.Join(r.Names(), ","); got != "c,a,b" {
		t.Fatalf("names = %s, want registration order; REQ-PLUGIN-04 makes hook order "+
			"observable, so it must not be sorted behind the caller's back", got)
	}
}

func TestRemoveKeepsTheIndexConsistent(t *testing.T) {
	r := plugins.NewRegistry()
	for _, n := range []string{"a", "b", "c"} {
		r.Register(&namedHook{name: n})
	}
	if !r.Remove("a") {
		t.Fatal("Remove reported nothing removed")
	}
	if got := strings.Join(r.Names(), ","); got != "b,c" {
		t.Fatalf("names = %s, want b,c", got)
	}
	// The index must still point at the right entries, or a later Remove
	// deletes the wrong plugin.
	if !r.Remove("c") {
		t.Fatal("Remove(c) failed after an earlier removal shifted the slice")
	}
	if got := strings.Join(r.Names(), ","); got != "b" {
		t.Fatalf("names = %s, want b", got)
	}
}

func TestCategoryProjectionsUseInterfaceSatisfaction(t *testing.T) {
	r := plugins.NewRegistry()
	r.Register(backendOnly{name: "b"})
	r.Register(toolsOnly{name: "t"})
	r.Register(&namedHook{name: "h"})

	if len(r.Backends()) != 1 || len(r.ToolProviders()) != 1 || len(r.EventHooks()) != 1 {
		t.Fatalf("projections: %d backends, %d tool providers, %d hooks",
			len(r.Backends()), len(r.ToolProviders()), len(r.EventHooks()))
	}
	if len(r.Storages()) != 0 {
		t.Fatal("nothing implements StoragePlugin")
	}
	if got := plugins.KindsOf(backendOnly{}); len(got) != 1 || got[0] != plugins.KindBackend {
		t.Fatalf("KindsOf = %v, want [backend]", got)
	}
}

// ---- REQ-PLUGIN-04

// TestTheFirstBlockWinsAndStopsTheScan is REQ-PLUGIN-04.
//
// Stopping matters as much as blocking: if the scan continued, a later hook
// returning "allow" would have to be ignored anyway — and a hook that runs
// after the decision is made is a hook whose author will eventually assume it
// can change it.
func TestTheFirstBlockWinsAndStopsTheScan(t *testing.T) {
	var calls []string
	hooks := []plugins.EventHookPlugin{
		&namedHook{name: "first", verdict: plugins.DecisionNone, calls: &calls},
		&namedHook{name: "second", verdict: plugins.DecisionBlock, calls: &calls},
		&namedHook{name: "third", verdict: plugins.DecisionAllow, calls: &calls},
	}
	d, by := plugins.ToolDecision(context.Background(), hooks, "execute", nil)
	if d != plugins.DecisionBlock {
		t.Fatalf("decision = %q, want block", d)
	}
	if by == nil || by.PluginName() != "second" {
		t.Fatal("the blocking hook must be identified, so the refusal can name it")
	}
	if strings.Join(calls, ",") != "first,second" {
		t.Fatalf("hooks called = %v; the scan must STOP at the first block, not run a "+
			"later hook whose author may come to believe it can overturn one", calls)
	}
}

func TestNoOpinionIsTheDefaultOutcome(t *testing.T) {
	hooks := []plugins.EventHookPlugin{&namedHook{name: "a"}, &namedHook{name: "b"}}
	if d, _ := plugins.ToolDecision(context.Background(), hooks, "x", nil); d != plugins.DecisionNone {
		t.Fatalf("decision = %q, want no opinion when nothing votes", d)
	}
	if d, _ := plugins.ToolDecision(context.Background(), nil, "x", nil); d != plugins.DecisionNone {
		t.Fatalf("decision = %q with no hooks, want no opinion", d)
	}
}

// ---- manifests

func writeManifest(t *testing.T, dir, rel, body string) string {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestDiscoverySortsByNameAndHonoursTheDisabledList(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"zulu", "alpha", "mike"} {
		writeManifest(t, dir, n+"/plugin.toml", `
[plugin]
name = "`+n+`"
module = "example.com/`+n+`"
kinds = ["event_hook"]
`)
	}

	ms, _ := plugins.Discover(plugins.Config{Paths: []string{dir}})
	var names []string
	for _, m := range ms {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "alpha,mike,zulu" {
		t.Fatalf("names = %v; REQ-PLUGIN-06 sorts manifest plugins alphabetically so the "+
			"load order does not depend on directory iteration", names)
	}

	ms, diags := plugins.Discover(plugins.Config{Paths: []string{dir}, Disabled: []string{"mike"}})
	names = nil
	for _, m := range ms {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "alpha,zulu" {
		t.Fatalf("names = %v, want mike excluded (REQ-PLUGIN-07)", names)
	}
	var explained bool
	for _, d := range diags {
		if strings.Contains(d.Message, "disabled list") {
			explained = true
		}
	}
	if !explained {
		t.Fatal("a plugin absent because of a config line three files away is the hardest " +
			"kind of missing feature to diagnose; the skip must be reported")
	}
}

func TestAManifestWithoutANameIsRejected(t *testing.T) {
	_, _, err := plugins.ParseManifest("p.toml", []byte("[plugin]\nmodule = \"m\"\n"))
	if err == nil {
		t.Fatal("the name is the key the registry, the disabled list and every collision " +
			"warning use; a manifest without one cannot be referred to at all")
	}
}

// TestAnUnknownManifestKeyIsADiagnosticNotARejection is REQ-SEC-12.5's other
// side, and the contrast is the point: the wire package rejects an unknown
// property because a PEER wrote it. A manifest is locally authored, so an
// unknown key is a note to its author.
func TestAnUnknownManifestKeyIsADiagnosticNotARejection(t *testing.T) {
	m, _, err := plugins.ParseManifest("p.toml", []byte(`
[plugin]
name = "x"
module = "example.com/x"
kinds = ["event_hook", "nonsense"]
future_field = "from a newer SDK"
`))
	if err != nil {
		t.Fatalf("a manifest with an unknown key must still load: %v", err)
	}
	if len(m.Kinds) != 1 || m.Kinds[0] != plugins.KindEventHook {
		t.Fatalf("kinds = %v, want the unknown one dropped and the known one kept", m.Kinds)
	}
}

// ---- REQ-PLUGIN-09 / -10

func TestTheImportLintRejectsAgentkitInternals(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "src/good.go", `package p

import (
	"fmt"
	"github.com/agentfox/agentkit-go/core"
	"example.com/other/internal/fine"
)

var _ = fmt.Sprint
`)
	writeManifest(t, dir, "src/bad.go", `package p

import "github.com/agentfox/agentkit-go/internal/toml"
`)
	writeManifest(t, dir, "src/vendor/skipped.go", `package v

import "github.com/agentfox/agentkit-go/internal/diag"
`)

	bad, err := plugins.LintImports(filepath.Join(dir, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 {
		t.Fatalf("violations = %+v, want exactly one", bad)
	}
	if !strings.HasSuffix(bad[0].File, "bad.go") {
		t.Fatalf("flagged %s, want bad.go", bad[0].File)
	}
	if !strings.Contains(bad[0].Import, "internal/toml") {
		t.Fatalf("import = %q", bad[0].Import)
	}
}

// TestAPluginsOwnInternalPackageIsAllowed: rejecting every path containing
// "internal" would refuse a plugin for having ordinary Go structure.
func TestAPluginsOwnInternalPackageIsAllowed(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "src/x.go", `package p

import "example.com/myplugin/internal/state"
`)
	bad, err := plugins.LintImports(filepath.Join(dir, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Fatalf("flagged %+v; REQ-PLUGIN-09 forbids AGENTKIT internals, not the word", bad)
	}
}

// TestTheLintReadsFilesThatDoNotCompile is why it parses imports only.
//
// A plugin whose body does not build against this SDK version is exactly the
// case where the lint matters most, and a lint that needs a working build
// cannot run there.
func TestTheLintReadsFilesThatDoNotCompile(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "src/broken.go", `package p

import "github.com/agentfox/agentkit-go/internal/toml"

func Broken() { this is not go }
`)
	bad, err := plugins.LintImports(filepath.Join(dir, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 {
		t.Fatalf("violations = %+v; imports must be linted even when the body does not "+
			"parse as a whole file", bad)
	}
}

func TestValidateReportsConformance(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "hooky/plugin.toml", `
[plugin]
name = "hooky"
module = "example.com/hooky"
kinds = ["event_hook", "storage"]
source = "src"
`)
	writeManifest(t, dir, "hooky/src/x.go", "package p\n")
	writeManifest(t, dir, "ghost/plugin.toml", `
[plugin]
name = "ghost"
module = "example.com/ghost"
kinds = ["backend"]
`)

	reg := plugins.NewRegistry()
	reg.Register(&namedHook{name: "hooky"})

	rep := plugins.Validate(plugins.Config{Paths: []string{dir}}, reg)

	byRule := map[plugins.Rule][]plugins.Violation{}
	for _, v := range rep.Violations {
		byRule[v.Rule] = append(byRule[v.Rule], v)
	}
	if len(byRule[plugins.RuleKindNotImplemented]) != 1 {
		t.Fatalf("violations = %v; hooky declares storage and does not implement it",
			rep.Violations)
	}
	if byRule[plugins.RuleKindNotImplemented][0].Severity != plugins.SeverityError {
		t.Fatal("a declared category the type does not satisfy is an error: the manifest " +
			"promises a capability that is not there")
	}
	if len(byRule[plugins.RuleMissingRegistration]) != 1 {
		t.Fatalf("ghost is declared and unregistered; want one missing_registration: %v",
			rep.Violations)
	}
	if byRule[plugins.RuleMissingRegistration][0].Severity != plugins.SeverityWarning {
		t.Fatal("REQ-PLUGIN-08 makes a missing plugin a graceful SKIP, so it is a warning " +
			"during validation, not a failure")
	}
	if rep.OK() {
		t.Fatal("a kind_not_implemented error must fail the report")
	}
	if rep.ExitCode() != 1 {
		t.Fatalf("exit = %d, want 1", rep.ExitCode())
	}
}

// TestAManifestWithNoSourceIsReportedRatherThanPassed: a check that silently
// does not run is worse than one that fails, because the report reads clean.
func TestAManifestWithNoSourceIsReportedRatherThanPassed(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "p/plugin.toml", `
[plugin]
name = "p"
module = "example.com/p"
kinds = ["event_hook"]
`)
	reg := plugins.NewRegistry()
	reg.Register(&namedHook{name: "p"})
	rep := plugins.Validate(plugins.Config{Paths: []string{dir}}, reg)

	var unlintable bool
	for _, v := range rep.Violations {
		if v.Rule == plugins.RuleUnlintable {
			unlintable = true
		}
	}
	if !unlintable {
		t.Fatalf("violations = %v; a manifest with no source path means the import lint "+
			"could not run, and that must be visible", rep.Violations)
	}
}

func TestValidateStartsNothing(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "p/plugin.toml", `
[plugin]
name = "p"
module = "example.com/p"
kinds = ["tool_provider"]
`)
	var called bool
	reg := plugins.NewRegistry()
	reg.Register(spyProvider{name: "p", called: &called})

	plugins.Validate(plugins.Config{Paths: []string{dir}}, reg)
	if called {
		t.Fatal("Validate must not CALL a plugin: a validation pass that runs the thing " +
			"it validates is a smoke test with extra steps, and cannot be run against a " +
			"plugin broken in the way you are looking for (REQ-PLUGIN-10)")
	}
}

type spyProvider struct {
	name   string
	called *bool
}

func (s spyProvider) PluginName() string { return s.name }
func (s spyProvider) Tools(context.Context) ([]core.Tool, error) {
	*s.called = true
	return nil, nil
}
