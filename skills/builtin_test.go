package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// TestTheShippedSkillLoadsThroughTheRealLoader is REQ-SKILL-04's built-in
// tier and REQ-SKILL-13's worked example, end to end: BuiltinDir finds the
// tree, Discover accepts the skill with no diagnostics, and the prompt file
// carries every part of the authoring contract under its own heading.
func TestTheShippedSkillLoadsThroughTheRealLoader(t *testing.T) {
	dir := BuiltinDir()
	if dir == "" {
		t.Skip("BuiltinDir is unresolvable here (trimpath or a stripped tree); the tier is skipped, not faked")
	}
	if !filepath.IsAbs(dir) || filepath.Base(dir) != BuiltinDirName {
		t.Fatalf("BuiltinDir = %q, want an absolute path ending in %s", dir, BuiltinDirName)
	}

	// HomeDir and WorkDir are left empty on purpose: this must be the SDK's
	// tier alone, not whatever the host has under ~/.nightshift.
	reg := Discover(Config{BuiltinDir: dir})
	if diags := reg.Diagnostics(); len(diags) != 0 {
		t.Fatalf("the shipped tier must load clean, got: %v", diags)
	}
	var review *Skill
	for _, s := range reg.Skills() {
		if s.Name == "code-review" {
			s := s
			review = &s
		}
	}
	if review == nil {
		t.Fatalf("code-review is not among the built-in skills: %s", names(reg.Skills()))
	}
	if review.Tier != TierBuiltin || !review.Tier.Trusted() {
		t.Fatalf("tier = %s; the SDK's own skills are the trusted built-in tier (REQ-SKILL-04)", review.Tier)
	}
	if review.Description == "" || len(review.Archetypes) != 0 {
		t.Fatalf("manifest = %+v; the example is universal and must describe itself", review.Manifest)
	}

	body, err := os.ReadFile(review.PromptPath)
	if err != nil {
		t.Fatal(err)
	}
	// REQ-SKILL-13's six parts, in order. The headings are the contract's
	// names, so a prompt that renames one has to say why in review.
	parts := []string{
		"## Charter", "## Hard prohibitions", "## Mechanical gates",
		"## Output contract", "## Anti-false-positive carve-outs", "## Pointers, not copies",
	}
	last := -1
	for _, h := range parts {
		i := strings.Index(string(body), h)
		if i < 0 {
			t.Errorf("prompt.md lacks the %q section (REQ-SKILL-13)", h)
			continue
		}
		if i < last {
			t.Errorf("%q appears out of contract order", h)
		}
		last = i
	}
	// Part 2's distinguishing rule: every prohibition names its incident.
	if strings.Count(string(body), "Incident:") < 3 {
		t.Error("hard prohibitions must each record the incident that produced them (REQ-SKILL-13.2)")
	}
	if !strings.Contains(string(body), "agent-kit-prd.md") {
		t.Error("the prompt must point at the PRD rather than restate it (REQ-SKILL-13.6)")
	}
}

// TestTheShippedSkillRendersThroughAssemble: the skill contributes exactly
// its name, description and absolute prompt path to the system prompt
// (REQ-SKILL-06), and nothing from its body.
func TestTheShippedSkillRendersThroughAssemble(t *testing.T) {
	dir := BuiltinDir()
	if dir == "" {
		t.Skip("BuiltinDir is unresolvable here")
	}
	reg := Discover(Config{BuiltinDir: dir})
	out := Assemble(Input{
		Skills: reg.LoadForSession("", ""),
		Tools:  []core.Tool{{Name: "read_file"}},
	})
	want := `<skill name="code-review" path="` + filepath.Join(dir, "code-review", PromptName) + `">`
	if !strings.Contains(out, want) {
		t.Fatalf("assembled block lacks %s:\n%s", want, out)
	}
	if strings.Contains(out, "## Charter") {
		t.Fatal("the prompt BODY leaked into the system prompt; REQ-SKILL-06 offers metadata only")
	}
}

// TestBuiltinDirIsEmptyRatherThanRelativeWhenAbsent: "" is the contract for
// an unresolvable tier, the same as Config.HomeDir. A relative fallback would
// resolve against the untrusted working directory (REQ-SKILL-12.3).
func TestBuiltinDirIsEmptyRatherThanRelativeWhenAbsent(t *testing.T) {
	if got := BuiltinDir(); got != "" && !filepath.IsAbs(got) {
		t.Fatalf("BuiltinDir = %q; it must be absolute or empty, never relative", got)
	}
	// And an empty BuiltinDir skips the tier without a diagnostic.
	reg := Discover(Config{BuiltinDir: ""})
	if len(reg.Skills()) != 0 || len(reg.Diagnostics()) != 0 {
		t.Fatalf("an empty BuiltinDir must skip the tier silently: %v %v", reg.Skills(), reg.Diagnostics())
	}
}
