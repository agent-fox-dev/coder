package skills

import (
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// REQ-SKILL-07's last sentence with REQ-CACHE-10: a skill activating
// mid-session MARKS its tools, and the marker is what the deferred split
// consumes. This test runs the two halves against each other, because the
// requirement is only met if the names skills hands back are the names
// provider.SplitDeferredTools acts on.
func TestAMidSessionActivationDefersTheSkillsToolsInsteadOfInvalidatingThePrefix(t *testing.T) {
	base := []core.Tool{tool("read_file"), tool("execute")}
	act, err := Activate(base, Contribution{Skill: "review", Tools: []core.Tool{tool("run_lint")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(act.AddedToolNames, ","); got != "run_lint" {
		t.Fatalf("AddedToolNames = %q, want only the new tool", got)
	}

	// The runner's one line: mark the tool result at whose position the skill
	// activated, then append it to the history.
	result := core.ToolResultMessage{ToolUseID: "t1", ToolName: "activate_skill"}
	act.Mark(&result)
	history := core.Messages{core.UserMessage{}, result}

	split := provider.SplitDeferredTools(core.ToolWires(act.Tools), history)
	if !split.IsDeferred("run_lint") {
		t.Fatalf("run_lint was not deferred: immediate=%v deferred=%v", split.Immediate, split.Deferred)
	}
	if len(split.Immediate) != 2 {
		t.Fatalf("the cached prefix changed: immediate = %v", split.Immediate)
	}
	if split.Promoted {
		t.Fatal("the safety valve fired with a non-empty prefix")
	}
}

// An override REPLACES a definition the prefix already carries, which is a
// schema change (REQ-CACHE-06) and not an addition. Marking it as added would
// claim a saving that was not made.
func TestAnOverriddenToolIsNotMarkedAsNewlyAdded(t *testing.T) {
	base := []core.Tool{tool("read_file")}
	act, err := Activate(base, Contribution{
		Skill: "sneaky", Tools: []core.Tool{tool("read_file"), tool("run_lint")},
		Overrides: []string{"read_file"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(act.AddedToolNames, ","); got != "run_lint" {
		t.Fatalf("AddedToolNames = %q, want the override excluded", got)
	}
}

// REQ-SKILL-07's conflict rule survives activation: activating late must not
// be a way around the check that activating early would have failed.
func TestActivationStillRaisesSkillConflictError(t *testing.T) {
	_, err := Activate([]core.Tool{tool("execute")},
		Contribution{Skill: "sneaky", Tools: []core.Tool{tool("execute")}})
	var conflict *SkillConflictError
	if !asConflict(err, &conflict) {
		t.Fatalf("err = %v, want a SkillConflictError", err)
	}
	if conflict.Tool != "execute" || conflict.Holder != SessionOwner {
		t.Fatalf("conflict = %+v", conflict)
	}
}

// Mark is additive: one turn can activate a skill and connect an MCP server,
// and both sets of names belong at the same transcript position.
func TestMarkAppendsToAnyExistingMarker(t *testing.T) {
	act := Activation{AddedToolNames: []string{"run_lint", "mcp__db__query"}}
	m := core.ToolResultMessage{AddedToolNames: []string{"mcp__db__query"}}
	act.Mark(&m)
	if got := strings.Join(m.AddedToolNames, ","); got != "mcp__db__query,run_lint" {
		t.Fatalf("AddedToolNames = %q", got)
	}
	act.Mark(nil) // must not panic
}
