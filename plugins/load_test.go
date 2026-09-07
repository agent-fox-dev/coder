package plugins_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/plugins"
)

// ---- REQ-PLUGIN-05 / -07: [plugins] in config.toml

func TestParseConfigReadsPathsAndDisabled(t *testing.T) {
	cfg, diags, err := plugins.ParseConfig("config.toml", []byte(`
[plugins]
paths = ["/opt/plugins", " ./more "]
disabled = ["audit"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Paths, ","); got != "/opt/plugins,./more" {
		t.Fatalf("paths = %v (REQ-PLUGIN-05)", cfg.Paths)
	}
	if got := strings.Join(cfg.Disabled, ","); got != "audit" {
		t.Fatalf("disabled = %v (REQ-PLUGIN-07)", cfg.Disabled)
	}
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", diags)
	}
}

func TestParseConfigIsLenientOnAuthoredContent(t *testing.T) {
	cfg, diags, err := plugins.ParseConfig("config.toml", []byte(`
[plugins]
paths = "/not/an/array"
disabled = ["x"]
colour = "blue"
`))
	if err != nil {
		t.Fatalf("an unknown key or a wrong type must not reject the file (REQ-SEC-12.5): %v", err)
	}
	if cfg.Paths != nil {
		t.Fatalf("a wrong-typed paths must be ignored, got %v", cfg.Paths)
	}
	if len(cfg.Disabled) != 1 {
		t.Fatalf("the well-formed key next to a bad one must still load: %v", cfg.Disabled)
	}
	var typed, unknown bool
	for _, d := range diags {
		typed = typed || strings.Contains(d.Message, "array of strings")
		unknown = unknown || strings.Contains(d.Message, `unknown key "colour"`)
	}
	if !typed || !unknown {
		t.Fatalf("diagnostics = %v; both the type mismatch and the unknown key must be reported", diags)
	}
}

func TestParseConfigWithoutASectionIsEmptyAndQuiet(t *testing.T) {
	cfg, diags, err := plugins.ParseConfig("config.toml", []byte("[mcp]\n"))
	if err != nil || len(cfg.Paths) != 0 || len(cfg.Disabled) != 0 || len(diags) != 0 {
		t.Fatalf("cfg=%+v diags=%v err=%v; no [plugins] section is the ordinary case", cfg, diags, err)
	}
}

// ---- REQ-PLUGIN-06: load order

func manifestFor(t *testing.T, dir, name string, extra string) {
	t.Helper()
	writeManifest(t, dir, name+"/plugin.toml", `
[plugin]
name = "`+name+`"
module = "example.com/`+name+`"
kinds = ["event_hook"]
`+extra)
}

func TestLoadOrdersBuiltinsThenManifestsAlphabeticallyThenLocal(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "mike", "")
	manifestFor(t, dir, "bravo", "")

	reg := plugins.NewRegistry()
	// Built-ins registered in a deliberately non-alphabetical order, and the
	// manifest-declared values registered BEFORE the built-ins, so that any
	// ordering the test observes is Load's and not registration's.
	reg.Register(&namedHook{name: "mike"})
	reg.Register(&namedHook{name: "zulu-builtin"})
	reg.Register(&namedHook{name: "bravo"})
	reg.Register(&namedHook{name: "alpha-builtin"})

	res := plugins.Load(plugins.Config{Paths: []string{dir}}, reg, &namedHook{name: "local"})

	want := "zulu-builtin,alpha-builtin,bravo,mike,local"
	if got := strings.Join(res.Names, ","); got != want {
		t.Fatalf("order = %q, want %q: built-ins in registration order, then manifest "+
			"plugins alphabetically, then local last (REQ-PLUGIN-06)", got, want)
	}
	if got := strings.Join(reg.Names(), ","); got != want {
		t.Fatalf("registry order = %q, want %q; the result must describe the registry", got, want)
	}
	if len(res.Refused) != 0 || len(res.Disabled) != 0 {
		t.Fatalf("refused=%v disabled=%v, want none", res.Refused, res.Disabled)
	}
}

func TestLoadLetsALocalPluginOverrideWithAWarning(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "audit", "")

	reg := plugins.NewRegistry()
	fromManifest := &namedHook{name: "audit"}
	reg.Register(fromManifest)
	override := &namedHook{name: "audit", verdict: plugins.DecisionBlock}

	res := plugins.Load(plugins.Config{Paths: []string{dir}}, reg, override)

	if got := strings.Join(res.Names, ","); got != "audit" {
		t.Fatalf("names = %q, want the one collided name", got)
	}
	if hooks := reg.EventHooks(); len(hooks) != 1 || hooks[0] != plugins.EventHookPlugin(override) {
		t.Fatal("the LOCAL registration must win; it is registered last (REQ-PLUGIN-06)")
	}
	var warned bool
	for _, d := range reg.Diagnostics() {
		warned = warned || strings.Contains(d.Message, "re-registered")
	}
	if !warned {
		t.Fatal("a silent override is the collision REQ-PLUGIN-06 says to warn about")
	}
}

func TestLoadSkipsAManifestWithNothingRegisteredBehindIt(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "ghost", "")
	reg := plugins.NewRegistry()
	reg.Register(&namedHook{name: "present"})

	res := plugins.Load(plugins.Config{Paths: []string{dir}}, reg)
	if got := strings.Join(res.Names, ","); got != "present" {
		t.Fatalf("names = %q; a manifest with no registration is a skip, not a load (REQ-PLUGIN-08)", got)
	}
	var explained bool
	for _, d := range res.Diagnostics {
		explained = explained || strings.Contains(d.Message, `"ghost"`) && strings.Contains(d.Message, "skipped")
	}
	if !explained {
		t.Fatalf("diagnostics = %v; the skip must be reported", res.Diagnostics)
	}
}

// ---- REQ-PLUGIN-07: disabled reaches already-registered plugins

func TestLoadAppliesDisabledToAlreadyRegisteredPlugins(t *testing.T) {
	// The embedder registered "audit" in code, before any config was read.
	// Discover cannot see it (there is no manifest), so Registry.Remove is the
	// only path that can honour the disabled list — and until Load it had no
	// caller.
	reg := plugins.NewRegistry()
	reg.Register(&namedHook{name: "audit"})
	reg.Register(&namedHook{name: "keep"})

	res := plugins.Load(plugins.Config{Disabled: []string{"audit", "never-registered"}}, reg)

	if got := strings.Join(res.Names, ","); got != "keep" {
		t.Fatalf("names = %q, want audit removed (REQ-PLUGIN-07)", got)
	}
	if got := strings.Join(res.Disabled, ","); got != "audit" {
		t.Fatalf("Disabled = %v; a name that was never registered is not a removal", res.Disabled)
	}
	var reported bool
	for _, d := range res.Diagnostics {
		reported = reported || strings.Contains(d.Message, `"audit"`) && strings.Contains(d.Message, "disabled list")
	}
	if !reported {
		t.Fatalf("diagnostics = %v; a plugin absent because of a config line must be reported", res.Diagnostics)
	}
}

// ---- REQ-PLUGIN-09: reject at load time

func TestLoadRefusesAManifestPluginThatImportsAgentkitInternals(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "sneaky", `source = "src"`)
	writeManifest(t, dir, "sneaky/src/p.go", `package p

import "github.com/agentfox/agentkit-go/internal/toml"
`)
	manifestFor(t, dir, "honest", `source = "src"`)
	writeManifest(t, dir, "honest/src/p.go", `package p

import "github.com/agentfox/agentkit-go/core"
`)

	reg := plugins.NewRegistry()
	reg.Register(&namedHook{name: "sneaky"})
	reg.Register(&namedHook{name: "honest"})

	res := plugins.Load(plugins.Config{Paths: []string{dir}}, reg)

	if got := strings.Join(res.Names, ","); got != "honest" {
		t.Fatalf("names = %q; REQ-PLUGIN-09 says a violation REJECTS the plugin at load "+
			"time, not that it is reported and loaded anyway", got)
	}
	if got := strings.Join(res.Refused, ","); got != "sneaky" {
		t.Fatalf("Refused = %v, want sneaky", res.Refused)
	}
	if reg.Remove("sneaky") {
		t.Fatal("the refused plugin was still in the registry")
	}
	var found bool
	for _, d := range res.Diagnostics {
		if d.Severity == plugins.SeverityError && strings.Contains(d.Message, "refused at load") &&
			strings.Contains(d.Message, "internal/toml") {
			found = true
			if !strings.Contains(d.Message, "not a sandbox") {
				t.Fatalf("the refusal must carry REQ-SEC-07's honesty note: %s", d.Message)
			}
			if !strings.HasSuffix(d.Path, filepath.Join("sneaky", "src", "p.go")) {
				t.Fatalf("the refusal must name the offending file, got %q", d.Path)
			}
		}
	}
	if !found {
		t.Fatalf("diagnostics = %v; the refusal must be an error naming the import", res.Diagnostics)
	}
}

func TestLoadDoesNotRefuseAnUnlintableManifest(t *testing.T) {
	// No source path means the lint could not run. That is a warning in
	// Validate; refusing here would make `source` mandatory by the back door.
	dir := t.TempDir()
	manifestFor(t, dir, "plain", "")
	reg := plugins.NewRegistry()
	reg.Register(&namedHook{name: "plain"})

	res := plugins.Load(plugins.Config{Paths: []string{dir}}, reg)
	if got := strings.Join(res.Names, ","); got != "plain" {
		t.Fatalf("names = %q; an unlintable manifest loads, it is not refused", got)
	}
}
