package skills

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

func toolNames(ts []core.Tool) string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return strings.Join(out, ",")
}

func TestSkillToolsAppendToTheSessionsBaseList(t *testing.T) {
	base := []core.Tool{tool("read_file"), tool("execute")}
	got, err := MergeTools(base,
		Contribution{Skill: "review", Tools: []core.Tool{tool("run_lint")}},
		Contribution{Skill: "release", Tools: []core.Tool{tool("changelog")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if n := toolNames(got); n != "read_file,execute,run_lint,changelog" {
		t.Fatalf("tools = %q", n)
	}
	if toolNames(base) != "read_file,execute" {
		t.Fatalf("the base list was mutated: %q", toolNames(base))
	}
}

// REQ-SKILL-07. Last-registration-wins is the obvious implementation and is
// how a skill quietly replaces `execute` with its own version.
func TestACollidingToolNameRaisesSkillConflictError(t *testing.T) {
	base := []core.Tool{tool("execute")}
	_, err := MergeTools(base, Contribution{Skill: "sneaky", Tools: []core.Tool{tool("execute")}})

	var conflict *SkillConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *SkillConflictError", err)
	}
	if conflict.Tool != "execute" || conflict.Incoming != "sneaky" || conflict.Holder != SessionOwner {
		t.Fatalf("conflict = %+v", conflict)
	}
	if !strings.Contains(conflict.Error(), "overrides") {
		t.Fatalf("the error must say how to fix it: %v", conflict)
	}
}

func TestTwoSkillsRegisteringTheSameToolNameConflict(t *testing.T) {
	_, err := MergeTools(nil,
		Contribution{Skill: "a", Tools: []core.Tool{tool("fmt")}},
		Contribution{Skill: "b", Tools: []core.Tool{tool("fmt")}},
	)
	var conflict *SkillConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *SkillConflictError", err)
	}
	if conflict.Holder != "a" || conflict.Incoming != "b" {
		t.Fatalf("conflict = %+v", conflict)
	}
}

// A declared override replaces IN PLACE. Appending the replacement and
// deleting the original reorders the tool array, which changes the serialized
// request and costs a provider cache-prefix miss for no behavioural gain.
func TestADeclaredOverrideReplacesTheToolInPlace(t *testing.T) {
	base := []core.Tool{tool("read_file"), {Name: "execute", Description: "base"}, tool("write_file")}
	got, err := MergeTools(base, Contribution{
		Skill:     "sandboxed",
		Tools:     []core.Tool{{Name: "execute", Description: "sandboxed"}},
		Overrides: []string{"execute"},
	})
	if err != nil {
		t.Fatalf("a declared override must be permitted: %v", err)
	}
	if n := toolNames(got); n != "read_file,execute,write_file" {
		t.Fatalf("tools = %q, want the original position preserved", n)
	}
	if got[1].Description != "sandboxed" {
		t.Fatalf("the override did not take effect: %+v", got[1])
	}
}

func TestAnOverrideDeclarationOnlyCoversTheNamesItLists(t *testing.T) {
	base := []core.Tool{tool("execute"), tool("write_file")}
	_, err := MergeTools(base, Contribution{
		Skill:     "partial",
		Tools:     []core.Tool{tool("execute"), tool("write_file")},
		Overrides: []string{"execute"},
	})
	var conflict *SkillConflictError
	if !errors.As(err, &conflict) || conflict.Tool != "write_file" {
		t.Fatalf("err = %v, want a conflict on write_file", err)
	}
}

// With two claimants there is no authority to prefer and the winner would be
// decided by discovery order — a silent, filesystem-dependent outcome, which
// is the failure SkillConflictError exists to prevent.
func TestTwoSkillsOverridingTheSameToolIsStillAConflict(t *testing.T) {
	base := []core.Tool{tool("execute")}
	_, err := MergeTools(base,
		Contribution{Skill: "a", Tools: []core.Tool{tool("execute")}, Overrides: []string{"execute"}},
		Contribution{Skill: "b", Tools: []core.Tool{tool("execute")}, Overrides: []string{"execute"}},
	)
	var conflict *SkillConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *SkillConflictError", err)
	}
	if conflict.Holder != "a" || conflict.Incoming != "b" {
		t.Fatalf("conflict = %+v", conflict)
	}
}

func TestContributionCarriesTheManifestsOverridesList(t *testing.T) {
	s := Skill{Manifest: Manifest{Name: "review", Overrides: []string{"execute"}}}
	c := s.Contribution([]core.Tool{tool("execute")})
	if c.Skill != "review" || strings.Join(c.Overrides, ",") != "execute" {
		t.Fatalf("contribution = %+v", c)
	}
	if _, err := MergeTools([]core.Tool{tool("execute")}, c); err != nil {
		t.Fatalf("the manifest's own overrides did not reach the merge: %v", err)
	}
}
