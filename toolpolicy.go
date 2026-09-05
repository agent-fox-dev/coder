package agentkit

import "github.com/agentfox/agentkit-go/core"

// ResolveToolPolicy is REQ-TOOL-10's five-field resolution. It applies
// UNIFORMLY to built-in and caller-supplied tools — that uniformity is the
// requirement, and it is what makes four otherwise surprising consequences
// true. Each is normative and each has a test:
//
//	NoTools "all"     disables CUSTOM tools too, not just built-ins.
//	NoTools "builtin" leaves the allowlist unset, so custom tools SURVIVE.
//	ToolNames         constrains custom tools, not only built-ins.
//	ExcludeTools      applies to custom tools, not only built-ins.
//
// Resolution order is Tools → NoTools → ToolNames → ExcludeTools, with
// CustomTools merged in before name-based selection so the selectors see one
// undifferentiated set.
//
// This is what REQ-MULTI-05's per-agent "tool allowlist" resolves to, and it
// is what lets a subagent be scoped to read-and-search-only per delegation
// without rebuilding the tool set by hand.
func ResolveToolPolicy(registered []core.Tool, p core.ToolPolicy) []core.Tool {
	// Tools, when non-nil INCLUDING EMPTY, is used verbatim and bypasses
	// everything below. Non-nil-but-empty is deliberately distinct from nil:
	// "no tools, and I mean it" must be expressible.
	if p.Tools != nil {
		return append([]core.Tool(nil), p.Tools...)
	}

	set := make([]core.Tool, 0, len(registered)+len(p.CustomTools))
	seen := make(map[string]int, len(registered)+len(p.CustomTools))

	add := func(t core.Tool) {
		if i, ok := seen[t.Name]; ok {
			// A custom tool overrides a built-in of the same name, in place,
			// so overriding does not reorder the tool list — the tool list is
			// part of the cached prompt prefix.
			set[i] = t
			return
		}
		seen[t.Name] = len(set)
		set = append(set, t)
	}

	if p.NoTools != core.NoToolsAll {
		for _, t := range registered {
			if t.Builtin && p.NoTools == core.NoToolsBuiltin {
				continue
			}
			add(t)
		}
		for _, t := range p.CustomTools {
			add(t)
		}
	}

	// NoTools "all" sets an empty allowlist: nothing survives, custom
	// included. Returning here rather than falling through keeps that true
	// even if ToolNames names something.
	if p.NoTools == core.NoToolsAll {
		return nil
	}

	// ToolNames nil means "the default set"; non-nil is an allowlist applied
	// to everything, custom tools included.
	if p.ToolNames != nil {
		allow := make(map[string]bool, len(p.ToolNames))
		for _, n := range p.ToolNames {
			allow[n] = true
		}
		set = filter(set, func(t core.Tool) bool { return allow[t.Name] })
	}

	// ExcludeTools is a denylist applied AFTER the allowlist, and applies to
	// custom tools too.
	if len(p.ExcludeTools) > 0 {
		deny := make(map[string]bool, len(p.ExcludeTools))
		for _, n := range p.ExcludeTools {
			deny[n] = true
		}
		set = filter(set, func(t core.Tool) bool { return !deny[t.Name] })
	}

	return set
}

func filter(ts []core.Tool, keep func(core.Tool) bool) []core.Tool {
	out := ts[:0]
	for _, t := range ts {
		if keep(t) {
			out = append(out, t)
		}
	}
	return out
}
