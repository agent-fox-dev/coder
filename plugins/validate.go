package plugins

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// InternalPrefix is the import path REQ-PLUGIN-09 forbids plugin code from
// reaching into.
const InternalPrefix = "github.com/agentfox/agentkit-go/internal/"

// Rule names what a violation broke.
type Rule string

const (
	// RuleForbiddenImport: plugin code imports agentkit/internal/...
	RuleForbiddenImport Rule = "forbidden_import"
	// RuleMissingRegistration: a manifest declares a plugin that nothing
	// registered. REQ-PLUGIN-08 makes this a graceful SKIP during loading; it
	// is reported here because --validate-plugins exists to find it before the
	// feature turns out to be missing at runtime.
	RuleMissingRegistration Rule = "missing_registration"
	// RuleKindNotImplemented: the manifest declares a category the registered
	// type does not satisfy.
	RuleKindNotImplemented Rule = "kind_not_implemented"
	// RuleUndeclaredKind: the type satisfies a category the manifest does not
	// declare. A warning: the code is fine and the manifest is stale.
	RuleUndeclaredKind Rule = "undeclared_kind"
	// RuleUnlintable: a manifest with no source path, so the import lint could
	// not run. Reported rather than passed, because a check that silently does
	// not run is worse than one that fails.
	RuleUnlintable Rule = "unlintable"
)

// Violation is one finding.
type Violation struct {
	Plugin   string
	Rule     Rule
	Severity Severity
	Path     string
	Message  string
}

func (v Violation) String() string {
	at := v.Path
	if at == "" {
		at = v.Plugin
	}
	return fmt.Sprintf("%s: %s: %s: %s", at, v.Severity, v.Rule, v.Message)
}

// Report is what --validate-plugins prints (REQ-PLUGIN-10).
type Report struct {
	Manifests   []Manifest
	Registered  []string
	Violations  []Violation
	Diagnostics []Diagnostic
}

// OK reports whether the report is clean. A WARNING does not fail it: a stale
// manifest kind or a disabled plugin is information, not a defect.
func (r Report) OK() bool {
	for _, v := range r.Violations {
		if v.Severity == SeverityError {
			return false
		}
	}
	return true
}

// ExitCode is 0 for clean, 1 for any error-severity violation.
func (r Report) ExitCode() int {
	if r.OK() {
		return 0
	}
	return 1
}

func (r Report) String() string {
	var b strings.Builder
	for _, d := range r.Diagnostics {
		fmt.Fprintf(&b, "%s\n", d)
	}
	for _, v := range r.Violations {
		fmt.Fprintf(&b, "%s\n", v)
	}
	errs, warns := 0, 0
	for _, v := range r.Violations {
		if v.Severity == SeverityError {
			errs++
		} else {
			warns++
		}
	}
	fmt.Fprintf(&b, "%d manifest(s), %d registered plugin(s), %d error(s), %d warning(s)\n",
		len(r.Manifests), len(r.Registered), errs, warns)
	return b.String()
}

// Validate is REQ-PLUGIN-10: load every configured manifest, run interface
// conformance checks and the import lint, and report violations WITHOUT
// starting anything.
//
// It never registers, never calls a plugin method and never opens a session.
// A validation pass that runs the thing it is validating is a smoke test with
// extra steps, and it cannot be run against a plugin that is broken in the way
// you are looking for.
func Validate(cfg Config, reg *Registry) Report {
	manifests, diags := Discover(cfg)
	rep := Report{Manifests: manifests, Diagnostics: diags}
	if reg != nil {
		rep.Registered = reg.Names()
		rep.Diagnostics = append(rep.Diagnostics, reg.Diagnostics()...)
	}

	for _, m := range manifests {
		var p Plugin
		if reg != nil {
			for _, cand := range reg.Plugins() {
				if cand.PluginName() == m.Name {
					p = cand
					break
				}
			}
		}

		if p == nil {
			rep.Violations = append(rep.Violations, Violation{
				Plugin: m.Name, Rule: RuleMissingRegistration, Severity: SeverityWarning,
				Path: m.Path,
				Message: fmt.Sprintf("declared in %s but nothing is registered under that "+
					"name; it will be skipped at load (REQ-PLUGIN-08)", m.Module),
			})
		} else {
			have := KindsOf(p)
			for _, want := range m.Kinds {
				if !hasKind(have, want) {
					rep.Violations = append(rep.Violations, Violation{
						Plugin: m.Name, Rule: RuleKindNotImplemented, Severity: SeverityError,
						Path: m.Path,
						Message: fmt.Sprintf("manifest declares kind %q but the registered "+
							"type implements %s", want, joinNames(have)),
					})
				}
			}
			for _, k := range have {
				if !hasKind(m.Kinds, k) {
					rep.Violations = append(rep.Violations, Violation{
						Plugin: m.Name, Rule: RuleUndeclaredKind, Severity: SeverityWarning,
						Path: m.Path,
						Message: fmt.Sprintf("the registered type implements kind %q, which "+
							"the manifest does not declare", k),
					})
				}
			}
		}

		rep.Violations = append(rep.Violations, lintManifest(m)...)
	}

	sort.SliceStable(rep.Violations, func(i, j int) bool {
		if rep.Violations[i].Plugin != rep.Violations[j].Plugin {
			return rep.Violations[i].Plugin < rep.Violations[j].Plugin
		}
		return rep.Violations[i].Rule < rep.Violations[j].Rule
	})
	return rep
}

func hasKind(ks []Kind, k Kind) bool {
	for _, v := range ks {
		if v == k {
			return true
		}
	}
	return false
}

func lintManifest(m Manifest) []Violation {
	if m.Source == "" {
		return []Violation{{
			Plugin: m.Name, Rule: RuleUnlintable, Severity: SeverityWarning, Path: m.Path,
			Message: "no [plugin] source path, so REQ-PLUGIN-09's import lint could not " +
				"run; a check that silently does not run is worse than one that fails",
		}}
	}
	dir := m.Source
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(m.Path), m.Source)
	}
	bad, err := LintImports(dir)
	if err != nil {
		return []Violation{{
			Plugin: m.Name, Rule: RuleUnlintable, Severity: SeverityWarning, Path: dir,
			Message: "import lint could not read the source: " + err.Error(),
		}}
	}
	out := make([]Violation, 0, len(bad))
	for _, b := range bad {
		out = append(out, Violation{
			Plugin: m.Name, Rule: RuleForbiddenImport, Severity: SeverityError,
			Path:    b.File,
			Message: fmt.Sprintf("imports %q; plugin code may not reach into agentkit internals", b.Import),
		})
	}
	return out
}

// BadImport is one forbidden import.
type BadImport struct {
	File   string
	Import string
}

// LintImports is REQ-PLUGIN-09, and REQ-SEC-07's honest limit in one function.
//
// It is an IMPORT-PATH LINT, not a sandbox. With build-time Go module linkage
// (REQ-PLUGIN-08) plugin code runs in this process with these privileges, and
// nothing here changes that: it catches a plugin reaching for
// agentkit/internal, and it does not stop one from reading your filesystem.
// Calling it a sandbox would be the more comfortable description and the false
// one.
//
// Only imports are parsed — parser.ImportsOnly — so a plugin whose body does
// not compile against this SDK version is still linted rather than skipped,
// which is the case where the lint matters most.
func LintImports(dir string) ([]BadImport, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("plugins: %s is not a directory", dir)
	}

	var out []BadImport
	fset := token.NewFileSet()
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is skipped, not fatal
		}
		if d.IsDir() {
			// Vendored and test-data trees are somebody else's code and are
			// not what the plugin ships.
			switch d.Name() {
			case "vendor", "testdata", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil // unparseable Go is the compiler's problem, not the lint's
		}
		for _, imp := range f.Imports {
			p, uerr := strconvUnquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if forbiddenImport(p) {
				out = append(out, BadImport{File: path, Import: p})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Import < out[j].Import
	})
	return out, nil
}

// forbiddenImport matches agentkit's internal tree.
//
// It matches the PREFIX and the bare package, and nothing else: a plugin's own
// `example.com/plugin/internal/x` is its business, and rejecting every path
// containing "internal" would refuse a plugin for having ordinary Go structure.
func forbiddenImport(path string) bool {
	return strings.HasPrefix(path, InternalPrefix) ||
		path == strings.TrimSuffix(InternalPrefix, "/")
}

func strconvUnquote(s string) (string, error) {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1], nil
	}
	return "", fmt.Errorf("plugins: unquoted import path %s", s)
}
