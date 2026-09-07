package skills

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const toolsManifest = `description = "ships a tool"

[skill.tools]
module = "example.com/skill/tools"
factory = "New"
`

// REQ-SKILL-09: a skill whose plugin source imports an LLM client is REJECTED
// at load, not warned about. This is the one place REQ-SKILL-10's leniency
// does not apply — prohibited code is not a typo.
func TestASkillPluginImportingAModelClientIsRejectedAtLoad(t *testing.T) {
	home := t.TempDir()
	dir := writeSkill(t, userSkills(home), "greedy", toolsManifest)
	writeFile(t, filepath.Join(dir, "tools.go"), "package tools\n\nimport \"github.com/openai/openai-go\"\n")

	reg := Discover(Config{HomeDir: home})
	if got := names(reg.Skills()); got != "" {
		t.Fatalf("skills = %q, want the skill rejected", got)
	}
	if !hasDiag(reg.Diagnostics(), "REQ-SKILL-09") {
		t.Fatalf("diagnostics = %v, want the prohibited import named", reg.Diagnostics())
	}
	if !hasDiag(reg.Diagnostics(), "openai-go") {
		t.Fatalf("diagnostics = %v, want the offending import path", reg.Diagnostics())
	}
}

// The same rule for agentkit internals, which REQ-PLUGIN-09 already forbade
// plugins: a skill is not a way around it.
func TestASkillPluginImportingAgentkitInternalsIsRejectedAtLoad(t *testing.T) {
	home := t.TempDir()
	dir := writeSkill(t, userSkills(home), "nosy", toolsManifest)
	writeFile(t, filepath.Join(dir, "tools.go"),
		"package tools\n\nimport \"github.com/agentfox/agentkit-go/internal/toml\"\n")

	reg := Discover(Config{HomeDir: home})
	if len(reg.Skills()) != 0 {
		t.Fatalf("skills = %q, want the skill rejected", names(reg.Skills()))
	}
	if !errors.Is(lintErr(t, dir), ErrProhibitedImport) {
		t.Fatal("the rejection must be ErrProhibitedImport")
	}
}

// Clean plugin source loads, which is what makes the rejections above about
// the IMPORT and not about shipping Go code at all.
func TestASkillPluginWithCleanImportsLoads(t *testing.T) {
	home := t.TempDir()
	dir := writeSkill(t, userSkills(home), "tidy", toolsManifest)
	writeFile(t, filepath.Join(dir, "tools.go"),
		"package tools\n\nimport \"github.com/agentfox/agentkit-go/core\"\n\nvar _ = core.Tool{}\n")

	reg := Discover(Config{HomeDir: home})
	if got := names(reg.Skills()); got != "tidy@user" {
		t.Fatalf("skills = %q, want the skill loaded", got)
	}
	if hasDiag(reg.Diagnostics(), "REQ-SKILL-09") {
		t.Fatalf("diagnostics = %v, want none about imports", reg.Diagnostics())
	}
}

// A skill that declares no [skill.tools] ships no plugin code by declaration,
// so a stray .go file is inert and rejecting the skill over it would be
// REQ-SKILL-10's failure mode in a new place.
func TestASkillThatDeclaresNoToolsIsNotLinted(t *testing.T) {
	home := t.TempDir()
	dir := writeSkill(t, userSkills(home), "prose", `description = "no tools"`)
	writeFile(t, filepath.Join(dir, "example.go"), "package x\n\nimport \"github.com/openai/openai-go\"\n")

	reg := Discover(Config{HomeDir: home})
	if got := names(reg.Skills()); got != "prose@user" {
		t.Fatalf("skills = %q", got)
	}
}

// REQ-SEC-07's honesty: a declared module whose source is elsewhere cannot be
// linted here, and a check that silently did not run is worse than one that
// fails. It must say so.
func TestADeclaredModuleWithNoLocalSourceReportsThatTheLintDidNotRun(t *testing.T) {
	home := t.TempDir()
	writeSkill(t, userSkills(home), "linked", toolsManifest)

	reg := Discover(Config{HomeDir: home})
	if got := names(reg.Skills()); got != "linked@user" {
		t.Fatalf("skills = %q, want the skill loaded", got)
	}
	if !hasDiag(reg.Diagnostics(), "could not run") {
		t.Fatalf("diagnostics = %v, want the un-run lint reported", reg.Diagnostics())
	}
	for _, d := range reg.Diagnostics() {
		if d.Severity == SeverityError {
			t.Fatalf("an unlintable module must not reject the skill: %v", d)
		}
	}
}

// lintErr re-runs the lint directly, so the test can assert the sentinel the
// discovery path wraps.
func lintErr(t *testing.T, dir string) error {
	t.Helper()
	m := Manifest{Tools: ToolsSection{Module: "example.com/x", Factory: "New"}}
	_, err := lintPluginSource(dir, m)
	if err == nil {
		t.Fatal("want a rejection")
	}
	if !strings.Contains(err.Error(), "internal/toml") {
		t.Fatalf("err = %v, want the import named", err)
	}
	return err
}
