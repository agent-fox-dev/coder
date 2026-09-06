package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentfox/agentkit-go/internal/diag"
	"github.com/agentfox/agentkit-go/internal/toml"
)

// ManifestName is REQ-PLUGIN-05's file.
const ManifestName = "plugin.toml"

// Manifest is a parsed plugin.toml.
//
// It is a DECLARATION, not a loader input (REQ-PLUGIN-08). Nothing here causes
// code to be loaded; it states what the embedder is expected to have linked
// in, so that a mismatch can be reported instead of discovered as a missing
// feature at runtime.
type Manifest struct {
	Name string
	// Module is the Go module path the plugin's code lives in. It is recorded
	// so a validation failure can name the module a maintainer has to fix,
	// rather than the directory a manifest happened to sit in.
	Module      string
	Description string
	Kinds       []Kind
	// Source is an optional path, relative to the manifest, holding the
	// plugin's Go source. It is what REQ-PLUGIN-09's import lint reads. Absent
	// means the lint cannot run and says so rather than passing.
	Source string
	// Path is the manifest file itself.
	Path string
}

// Config is REQ-PLUGIN-05 and -07's `[plugins]` section.
type Config struct {
	// Paths are the EXPLICITLY configured directories. There is no implicit
	// search path and no discovery from the working directory: REQ-SEC-10's
	// argument about project-local prompt material applies with more force
	// here, because a plugin is code rather than text.
	Paths []string
	// Disabled opts out by name (REQ-PLUGIN-07).
	Disabled []string
}

// ParseManifest parses one plugin.toml.
//
// Manifests are LOCALLY AUTHORED content, so they are decoded leniently
// (REQ-SEC-12.5, REQ-SKILL-10): an unknown key is a diagnostic, not a
// rejection. That is the opposite of the wire package's rule and the
// difference is who wrote the bytes.
func ParseManifest(path string, src []byte) (Manifest, []Diagnostic, error) {
	root, diags, err := toml.ParseTOML(src)
	if err != nil {
		return Manifest{}, diags, fmt.Errorf("plugins: %s: %w", path, err)
	}

	tbl, ok := root.Sub("plugin")
	if !ok {
		return Manifest{}, diags, fmt.Errorf("plugins: %s: no [plugin] table", path)
	}

	m := Manifest{Path: path}
	if v, ok := tbl.Get("name"); ok && v.Kind == toml.KindString {
		m.Name = strings.TrimSpace(v.Str)
	}
	if v, ok := tbl.Get("module"); ok && v.Kind == toml.KindString {
		m.Module = strings.TrimSpace(v.Str)
	}
	if v, ok := tbl.Get("description"); ok && v.Kind == toml.KindString {
		m.Description = v.Str
	}
	if v, ok := tbl.Get("source"); ok && v.Kind == toml.KindString {
		m.Source = v.Str
	}
	if v, ok := tbl.Get("kinds"); ok && v.Kind == toml.KindStringArray {
		for _, s := range v.Array {
			k := Kind(strings.TrimSpace(s))
			if !validKind(k) {
				diags = append(diags, Diagnostic{Path: path, Severity: SeverityWarning,
					Message: fmt.Sprintf("unknown plugin kind %q; known kinds are %s",
						s, joinNames(AllKinds))})
				continue
			}
			m.Kinds = append(m.Kinds, k)
		}
	}

	if m.Name == "" {
		return Manifest{}, diags, fmt.Errorf(
			"plugins: %s: [plugin] name is required; it is the key the registry, the "+
				"disabled list and every collision warning use", path)
	}
	if m.Module == "" {
		// A warning, not an error: a manifest is still usable without it, and
		// refusing to load over a missing diagnostic field would be the
		// strictness REQ-SEC-12.5 explicitly does not want on authored content.
		diags = append(diags, Diagnostic{Path: path, Severity: SeverityWarning,
			Message: "[plugin] module is unset; a validation failure cannot name the " +
				"module a maintainer has to fix (REQ-PLUGIN-08)"})
	}
	if len(m.Kinds) == 0 {
		diags = append(diags, Diagnostic{Path: path, Severity: SeverityWarning,
			Message: "[plugin] declares no kinds; nothing can be conformance-checked " +
				"against it (REQ-PLUGIN-10)"})
	}
	return m, diags, nil
}

// Discover reads every plugin.toml under the configured directories
// (REQ-PLUGIN-05), returning manifests sorted by name (REQ-PLUGIN-06).
//
// A directory is searched ONE level deep: <dir>/<plugin>/plugin.toml, plus a
// plugin.toml directly in <dir>. Recursing arbitrarily would make what gets
// loaded depend on how deep somebody nested a vendor tree.
func Discover(cfg Config) ([]Manifest, []Diagnostic) {
	var (
		out   []Manifest
		diags []Diagnostic
		seen  = map[string]string{}
	)
	disabled := map[string]bool{}
	for _, n := range cfg.Disabled {
		disabled[n] = true
	}

	for _, dir := range cfg.Paths {
		for _, path := range manifestPaths(dir, &diags) {
			src, err := os.ReadFile(path)
			if err != nil {
				diags = append(diags, Diagnostic{Path: path, Severity: SeverityWarning,
					Message: "unreadable: " + err.Error()})
				continue
			}
			m, mdiags, err := ParseManifest(path, src)
			diags = append(diags, mdiags...)
			if err != nil {
				diags = append(diags, Diagnostic{Path: path, Severity: SeverityError,
					Message: err.Error()})
				continue
			}
			if disabled[m.Name] {
				// REQ-PLUGIN-07. Reported, because a plugin silently absent
				// because of a config line three files away is the hardest
				// kind of missing feature to diagnose.
				diags = append(diags, Diagnostic{Path: path, Severity: SeverityWarning,
					Message: fmt.Sprintf("plugin %q is in the disabled list; skipped", m.Name)})
				continue
			}
			if prev, dup := seen[m.Name]; dup {
				diags = append(diags, Diagnostic{Path: path, Severity: SeverityWarning,
					Message: fmt.Sprintf("plugin %q also declared at %s; the later "+
						"declaration wins (REQ-PLUGIN-06)", m.Name, prev)})
				out = replaceByName(out, m)
				seen[m.Name] = path
				continue
			}
			seen[m.Name] = path
			out = append(out, m)
		}
	}

	SortManifestsByName(out)
	sortDiagnostics(diags)
	return out, diags
}

func replaceByName(ms []Manifest, m Manifest) []Manifest {
	for i := range ms {
		if ms[i].Name == m.Name {
			ms[i] = m
			return ms
		}
	}
	return append(ms, m)
}

func manifestPaths(dir string, diags *[]Diagnostic) []string {
	var out []string
	if _, err := os.Stat(filepath.Join(dir, ManifestName)); err == nil {
		out = append(out, filepath.Join(dir, ManifestName))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		*diags = append(*diags, Diagnostic{Path: dir, Severity: SeverityWarning,
			Message: "plugin path is unreadable: " + err.Error()})
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), ManifestName)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func sortDiagnostics(ds []Diagnostic) {
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].Path != ds[j].Path {
			return ds[i].Path < ds[j].Path
		}
		return ds[i].Line < ds[j].Line
	})
}

var _ = diag.SeverityError
