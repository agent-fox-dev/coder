package skills

import (
	"fmt"

	"github.com/agentfox/agentkit-go/core"
)

// SkillConflictError is REQ-SKILL-07's collision. It is a struct, not a
// sentinel, because the useful information is WHICH tool and WHOSE — an
// embedder cannot fix "skill conflict" but can fix "both `lint` skills
// register a tool named run_lint".
type SkillConflictError struct {
	// Tool is the colliding tool name.
	Tool string
	// Holder is the current owner: a skill name, or SessionOwner for a tool
	// that came from the session's own base list.
	Holder string
	// Incoming is the skill that tried to register the same name.
	Incoming string
}

// SessionOwner names the embedder's own base tool list in a conflict.
const SessionOwner = "<session>"

func (e *SkillConflictError) Error() string {
	return fmt.Sprintf("skills: tool %q registered by skill %q collides with %s; "+
		"declare overrides = [%q] in the skill manifest to replace it",
		e.Tool, e.Incoming, e.Holder, e.Tool)
}

// Contribution is one skill's tool registrations.
//
// The tools are supplied by the EMBEDDER, not loaded from the manifest.
// [skill.tools] names a Go module and factory, and Go plugins link at build
// time (REQ-PLUGIN-08), so there is nothing this package could load at
// runtime; pretending otherwise would produce a load path that never works.
type Contribution struct {
	// Skill is the contributing skill's name, used in conflict reports.
	Skill string
	Tools []core.Tool
	// Overrides is the manifest's `overrides` list: the tool names this skill
	// is permitted to replace (REQ-SKILL-07).
	Overrides []string
}

// Contribution builds a Contribution for this skill from tools the embedder
// resolved for it.
func (s Skill) Contribution(tools []core.Tool) Contribution {
	return Contribution{Skill: s.Name, Tools: tools, Overrides: s.Overrides}
}

// MergeTools merges skill-registered tools into the session's base tool list
// (REQ-SKILL-07).
//
// Rules, and why:
//
//   - A name collision is a SkillConflictError unless the INCOMING skill names
//     that tool in its `overrides`. Silently letting the last registration win
//     is how a skill quietly replaces `execute` or `write_file` with its own
//     implementation and nobody notices until it has run.
//
//   - An override REPLACES IN PLACE. Appending the replacement and deleting the
//     original would reorder the tool array, which changes the serialized
//     request and costs a provider cache-prefix miss for no behavioural gain.
//
//   - Two skills both claiming an override of the same name is STILL a
//     conflict. "Unless one skill declares overrides" resolves a skill against
//     the host's list; with two claimants there is no authority to prefer, and
//     the winner would be decided by discovery order — a silent,
//     filesystem-dependent outcome, which is the failure this error exists to
//     prevent.
//
// The base list is not mutated.
func MergeTools(base []core.Tool, contribs ...Contribution) ([]core.Tool, error) {
	out := append([]core.Tool(nil), base...)
	index := make(map[string]int, len(out))
	owner := make(map[string]string, len(out))
	claimed := map[string]bool{} // names already taken by a declared override
	for i, t := range out {
		index[t.Name] = i
		owner[t.Name] = SessionOwner
	}

	for _, c := range contribs {
		allowed := map[string]bool{}
		for _, n := range c.Overrides {
			allowed[n] = true
		}
		for _, t := range c.Tools {
			i, exists := index[t.Name]
			if !exists {
				index[t.Name] = len(out)
				owner[t.Name] = c.Skill
				out = append(out, t)
				continue
			}
			if !allowed[t.Name] || claimed[t.Name] {
				return nil, &SkillConflictError{
					Tool: t.Name, Holder: owner[t.Name], Incoming: c.Skill,
				}
			}
			out[i] = t
			owner[t.Name] = c.Skill
			claimed[t.Name] = true
		}
	}
	return out, nil
}
