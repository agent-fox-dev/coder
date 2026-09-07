package plugins

import (
	"fmt"
)

// LoadResult is what Load did to the registry, for the caller that has to
// explain a missing feature.
type LoadResult struct {
	// Names is the final registration order (REQ-PLUGIN-06), which is also
	// the order event hooks run in (REQ-PLUGIN-04).
	Names []string
	// Refused names the manifest plugins REQ-PLUGIN-09 rejected at load time.
	// They are NOT in the registry.
	Refused []string
	// Disabled names the plugins cfg.Disabled removed (REQ-PLUGIN-07).
	Disabled    []string
	Diagnostics []Diagnostic
}

// Load wires REQ-PLUGIN-05/06/07/09 into a registry. It is the missing caller
// of Discover, LintImports and Registry.Remove: each existed as a library
// piece and nothing composed them, so the manifest path was "supported" in the
// sense that every part had a unit test and no embedder could use it.
//
// Because plugins link at build time (REQ-PLUGIN-08), a manifest cannot cause
// code to load; it can only name a value the embedder already registered. So
// reg on entry holds the compiled-in pool, and Load REORDERS it into the tiers
// of REQ-PLUGIN-06:
//
//  1. built-in: what reg held on entry that no manifest declares, in its
//     original registration order;
//  2. manifest-declared: the discovered manifests, alphabetical by name, each
//     resolved to the registration of the same name. A manifest with nothing
//     behind it is a graceful skip with a warning (REQ-PLUGIN-08). A manifest
//     whose source fails the import lint is REFUSED: its registration is
//     dropped and an error diagnostic names the import (REQ-PLUGIN-09 says
//     "reject at load time", and a lint that only ever printed a report was
//     not that);
//  3. local: the plugins passed as local, registered LAST so that a local
//     override is possible at all. A name collision here is later-wins with
//     Register's warning.
//
// Then cfg.Disabled is applied with Remove, which is what reaches a plugin the
// embedder registered BEFORE the config was read (REQ-PLUGIN-07). Discover
// already drops a disabled manifest; without this pass the built-in behind it
// would stay loaded and the disabled list would be decorative.
//
// REQ-SEC-07's limit holds here exactly as it does in LintImports: refusing a
// plugin over an import path is a lint, not a sandbox. A plugin that passes it
// still runs in this process with these privileges.
func Load(cfg Config, reg *Registry, local ...Plugin) LoadResult {
	var res LoadResult
	if reg == nil {
		res.Diagnostics = append(res.Diagnostics, Diagnostic{Severity: SeverityError,
			Message: "plugins: Load called with a nil registry; nothing to load into"})
		return res
	}

	manifests, diags := Discover(cfg)
	res.Diagnostics = append(res.Diagnostics, diags...)

	pool := map[string]Plugin{}
	for _, p := range reg.Plugins() {
		pool[p.PluginName()] = p
	}
	declared := map[string]bool{}
	for _, m := range manifests {
		declared[m.Name] = true
	}

	// Tier 1: built-ins, in their original order.
	var order []Plugin
	for _, p := range reg.Plugins() {
		if !declared[p.PluginName()] {
			order = append(order, p)
		}
	}

	// Tier 2: manifests, alphabetical (Discover already sorted them).
	for _, m := range manifests {
		p, ok := pool[m.Name]
		if !ok {
			res.Diagnostics = append(res.Diagnostics, Diagnostic{Path: m.Path, Severity: SeverityWarning,
				Message: fmt.Sprintf("plugin %q is declared in %s but nothing is registered "+
					"under that name; skipped (REQ-PLUGIN-08)", m.Name, m.Module)})
			continue
		}
		if refused := refusalFor(m); len(refused) > 0 {
			res.Refused = append(res.Refused, m.Name)
			res.Diagnostics = append(res.Diagnostics, refused...)
			continue
		}
		order = append(order, p)
	}

	// Rebuild the registry in tier order. Registration diagnostics accumulated
	// before Load are kept: they describe the pool, and the pool is still the
	// pool.
	reg.order = nil
	reg.index = map[string]int{}
	for _, p := range order {
		reg.Register(p)
	}
	// Tier 3: local, last. Register carries REQ-PLUGIN-06's later-wins
	// warning.
	for _, p := range local {
		reg.Register(p)
	}

	// REQ-PLUGIN-07, against the whole result. Reported, for the same reason
	// Discover reports it: a feature missing because of a config line three
	// files away is the hardest kind to diagnose.
	for _, name := range cfg.Disabled {
		if reg.Remove(name) {
			res.Disabled = append(res.Disabled, name)
			res.Diagnostics = append(res.Diagnostics, Diagnostic{Severity: SeverityWarning,
				Message: fmt.Sprintf("plugin %q is in the disabled list; removed from the "+
					"registry (REQ-PLUGIN-07)", name)})
		}
	}

	res.Names = reg.Names()
	return res
}

// refusalFor runs REQ-PLUGIN-09's lint over one manifest and returns the
// diagnostics that justify refusing it, or nothing when it may load.
//
// Only an ERROR refuses. A manifest with no source path is unlintable, and
// Validate reports that as a warning; refusing it here would reject every
// plugin that did not opt into the lint, which is stricter than REQ-PLUGIN-09
// asks and would make the source field mandatory by the back door.
func refusalFor(m Manifest) []Diagnostic {
	var out []Diagnostic
	for _, v := range lintManifest(m) {
		if v.Severity != SeverityError {
			continue
		}
		out = append(out, Diagnostic{Path: v.Path, Severity: SeverityError,
			Message: fmt.Sprintf("plugin %q refused at load: %s (REQ-PLUGIN-09; this is an "+
				"import-path lint, not a sandbox — REQ-SEC-07)", m.Name, v.Message)})
	}
	return out
}
