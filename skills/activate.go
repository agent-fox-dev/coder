package skills

import "github.com/agentfox/agentkit-go/core"

// Activation is the result of merging a skill's tools into a session that is
// ALREADY RUNNING (REQ-SKILL-07's last sentence).
//
// A mid-session activation is not a smaller version of a start-of-session
// merge, it is a different problem: the tool list is already serialized into
// the provider's cached prompt prefix (REQ-CACHE-06), and prepending a new
// tool to that prefix invalidates the whole cached history over one skill.
// REQ-CACHE-10's answer is to declare the new tools at the TRANSCRIPT POSITION
// where they appeared, and the marker that says where that is, is
// core.ToolResultMessage.AddedToolNames.
//
// This type is the seam: the skills package cannot see the session's history
// or its provider, so it produces the merged list and the exact names to mark,
// and the runner attaches them.
type Activation struct {
	// Tools is the merged list, identical to what MergeTools returns.
	Tools []core.Tool
	// AddedToolNames is what provider.SplitDeferredTools consumes. It holds
	// only the NEW names — see Activate for why an override is not one.
	AddedToolNames []string
}

// Activate merges skill tool contributions into a running session's tool list
// and reports the names to mark per REQ-CACHE-10.
//
// Conflict handling is MergeTools' verbatim, including REQ-SKILL-07's rule
// that a collision is a SkillConflictError unless the incoming skill declares
// `overrides`: activation is not a back door around the conflict check, and a
// skill must not be able to replace `execute` by activating late instead of
// early.
//
// An OVERRIDDEN tool is deliberately NOT in AddedToolNames. Deferral exists
// for a definition the cached prefix has never seen; an override replaces a
// definition the prefix already carries, which is a schema CHANGE and
// invalidates the prefix by REQ-CACHE-06 no matter where it is declared.
// Marking it as newly added would claim a saving that was not made and would
// leave the model with two conflicting definitions of the same name.
//
// Wiring, which is one line at the point the runner appends the tool result
// that activated the skill:
//
//	act, err := skills.Activate(session.Tools, sk.Contribution(tools))
//	if err != nil { return err }
//	act.Mark(&result)              // result is the core.ToolResultMessage
//	history = append(history, result)
//	session.Tools = act.Tools
//
// From there provider.SplitDeferredTools(core.ToolWires(session.Tools),
// history) puts the skill's tools in Deferred and leaves the prefix alone.
func Activate(base []core.Tool, contribs ...Contribution) (Activation, error) {
	known := make(map[string]bool, len(base))
	for _, t := range base {
		known[t.Name] = true
	}

	merged, err := MergeTools(base, contribs...)
	if err != nil {
		return Activation{}, err
	}

	var added []string
	for _, t := range merged {
		if !known[t.Name] {
			added = append(added, t.Name)
		}
	}
	return Activation{Tools: merged, AddedToolNames: added}, nil
}

// Mark stamps the added names onto the tool result message at whose position
// the skill activated (REQ-CACHE-10).
//
// It APPENDS rather than assigns: one turn can activate a skill and connect an
// MCP server, and both sets of names belong at the same transcript position.
// A nil message or an empty activation is a no-op, so a caller can mark
// unconditionally.
func (a Activation) Mark(m *core.ToolResultMessage) {
	if m == nil || len(a.AddedToolNames) == 0 {
		return
	}
	seen := make(map[string]bool, len(m.AddedToolNames))
	for _, n := range m.AddedToolNames {
		seen[n] = true
	}
	for _, n := range a.AddedToolNames {
		if seen[n] {
			continue
		}
		seen[n] = true
		m.AddedToolNames = append(m.AddedToolNames, n)
	}
}
