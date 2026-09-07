package skills

import (
	"strings"
	"testing"
)

type fakeAuditor struct{ got [][]string }

func (a *fakeAuditor) AuditSkills(names []string) { a.got = append(a.got, names) }

// REQ-SKILL-11 / REQ-OBS-04: every loaded skill name reaches the session's
// audit event. Record wraps the selection so the two cannot come apart.
func TestRecordHandsEverySelectedSkillNameToTheAuditSink(t *testing.T) {
	home := t.TempDir()
	writeSkill(t, userSkills(home), "b", `description = "b"`)
	writeSkill(t, userSkills(home), "a", `description = "a"`)
	writeSkill(t, userSkills(home), "scoped", "description = \"s\"\narchetypes = [\"planner\"]\n")

	reg := Discover(Config{HomeDir: home})
	sink := &fakeAuditor{}
	sel := Record(sink, reg.LoadForSession("coder", "", reg.Config()))

	if got := names(sel); got != "a@user,b@user" {
		t.Fatalf("selection = %q", got)
	}
	if len(sink.got) != 1 {
		t.Fatalf("audit calls = %d, want exactly one", len(sink.got))
	}
	// The recorded set is the INJECTED set, not everything discovered: a skill
	// filtered out by archetype was never offered to the model.
	if got := strings.Join(sink.got[0], ","); got != "a,b" {
		t.Fatalf("audited %q, want the injected skills", got)
	}
}

// A host with no audit sink uses the same line.
func TestRecordWithNoAuditorIsAWorkingNoOp(t *testing.T) {
	sel := []Skill{{Manifest: Manifest{Name: "a"}}}
	if got := Record(nil, sel); len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("Record dropped the selection: %+v", got)
	}
}
