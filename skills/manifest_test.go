package skills

import (
	"errors"
	"strings"
	"testing"
)

func parseManifest(t *testing.T, src string) (Manifest, []Diagnostic) {
	t.Helper()
	m, diags, err := ParseManifest("skill.toml", []byte(src))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	return m, diags
}

func hasDiag(diags []Diagnostic, substr string) bool {
	for _, d := range diags {
		if strings.Contains(d.Message, substr) {
			return true
		}
	}
	return false
}

// REQ-SKILL-10: description is the ONLY field whose absence rejects a skill.
func TestOnlyAMissingDescriptionRejectsAManifest(t *testing.T) {
	m, diags, err := ParseManifest("skill.toml", []byte(`description = "the whole manifest"`))
	if err != nil {
		t.Fatalf("a description-only manifest must load: %v", err)
	}
	if m.Description != "the whole manifest" {
		t.Fatalf("description = %q", m.Description)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	_, _, err = ParseManifest("skill.toml", []byte(`
name = "everything-but"
version = "1.2.3"
author = "someone"
sdk_min_version = "0.3.0"
archetypes = ["coder"]
`))
	if !errors.Is(err, ErrNoDescription) {
		t.Fatalf("err = %v, want ErrNoDescription", err)
	}
}

func TestAWhitespaceOnlyDescriptionIsNoDescription(t *testing.T) {
	_, _, err := ParseManifest("skill.toml", []byte("description = \"   \"\n"))
	if !errors.Is(err, ErrNoDescription) {
		t.Fatalf("err = %v, want ErrNoDescription", err)
	}
}

// REQ-SKILL-10's core invariant. Strict decoding here — the correct posture at
// a wire boundary (REQ-SEC-12) — would delete a working skill over a typo.
func TestAnUnknownKeyIsAWarningAndTheSkillStillLoads(t *testing.T) {
	m, diags := parseManifest(t, `
description = "still here"
descriptoin = "typo"
[skill.nonsense]
whatever = true
`)
	if m.Description != "still here" {
		t.Fatalf("description = %q", m.Description)
	}
	if !hasDiag(diags, `unknown key "descriptoin"`) {
		t.Fatalf("diagnostics = %v, want the typo named", diags)
	}
	if !hasDiag(diags, "unknown table [skill.nonsense]") {
		t.Fatalf("diagnostics = %v, want the unknown table named", diags)
	}
	for _, d := range diags {
		if d.Severity != SeverityWarning {
			t.Fatalf("severity = %q, want every unknown-key diagnostic to be a warning", d.Severity)
		}
	}
}

// The four fields REQ-SKILL-06 removed get their own diagnostic. A bare
// "unknown key" would read as a typo and send the author looking for a
// misspelling that is not there.
func TestRemovedFieldsAreDiagnosedByNameWithTheReason(t *testing.T) {
	_, diags := parseManifest(t, `
description = "d"
injection = "pre"
keywords = ["a"]
prompt_file = "other.md"
prompt_position = "replace_section"
`)
	for _, want := range []string{"injection", "keywords", "prompt_file", "prompt_position"} {
		if !hasDiag(diags, `"`+want+`" is removed by REQ-SKILL-06`) {
			t.Errorf("diagnostics = %v, want %q reported as removed", diags, want)
		}
	}
	if hasDiag(diags, "unknown key") {
		t.Errorf("a removed field must not be reported as a plain unknown key: %v", diags)
	}
}

func TestAllManifestSectionsAreParsed(t *testing.T) {
	m, diags := parseManifest(t, `
name = "reviewer"
version = "1.2.3"
description = "Reviews diffs."
author = "team"
sdk_min_version = "0.3.0"
archetypes = ["coder", "reviewer"]
disable_model_invocation = true
overrides = ["run_lint"]

[skill.tools]
module = "example.com/reviewer"
factory = "NewTools"

[skill.security]
allowlist_extend = ["gh", "golangci-lint"]

[skill.session]
max_turns_add = 7

[skill.subagent]
archetype = "analyst"
mode = "pre"
prompt_template = "Summarize {{diff}}"
result_key = "analysis"
on_failure = "skip"
`)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if m.Name != "reviewer" || m.Version != "1.2.3" || m.Author != "team" || m.SDKMinVersion != "0.3.0" {
		t.Errorf("metadata = %+v", m)
	}
	if strings.Join(m.Archetypes, ",") != "coder,reviewer" {
		t.Errorf("archetypes = %v", m.Archetypes)
	}
	if !m.DisableModelInvocation {
		t.Error("disable_model_invocation not read")
	}
	if strings.Join(m.Overrides, ",") != "run_lint" {
		t.Errorf("overrides = %v", m.Overrides)
	}
	if m.Tools.Module != "example.com/reviewer" || m.Tools.Factory != "NewTools" {
		t.Errorf("tools = %+v", m.Tools)
	}
	if strings.Join(m.Security.AllowlistExtend, ",") != "gh,golangci-lint" {
		t.Errorf("security = %+v", m.Security)
	}
	if m.Session.MaxTurnsAdd != 7 {
		t.Errorf("session = %+v", m.Session)
	}
	if m.Subagent.Archetype != "analyst" || m.Subagent.Mode != "pre" ||
		m.Subagent.PromptTemplate != "Summarize {{diff}}" || m.Subagent.ResultKey != "analysis" ||
		m.Subagent.OnFailure != "skip" {
		t.Errorf("subagent = %+v", m.Subagent)
	}
}

func TestOnFailureDefaultsToWarnAndAnUnknownValueDegradesToIt(t *testing.T) {
	m, diags := parseManifest(t, "description = \"d\"\n[skill.subagent]\narchetype = \"a\"\n")
	if m.Subagent.OnFailure != "warn" {
		t.Errorf("default on_failure = %q, want warn", m.Subagent.OnFailure)
	}
	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics: %v", diags)
	}

	m, diags = parseManifest(t, "description = \"d\"\n[skill.subagent]\non_failure = \"explode\"\n")
	if m.Subagent.OnFailure != "warn" {
		t.Errorf("on_failure = %q, want it normalized to warn", m.Subagent.OnFailure)
	}
	if !hasDiag(diags, "abort|warn|skip") {
		t.Errorf("diagnostics = %v", diags)
	}
}

// A field of the wrong type is authored content being wrong, not a wire peer
// smuggling a field: warn, keep the zero value, keep the skill.
func TestAWrongTypedFieldWarnsAndKeepsTheZeroValue(t *testing.T) {
	m, diags := parseManifest(t, `
description = "d"
disable_model_invocation = "yes"
archetypes = "coder"
`)
	if m.DisableModelInvocation {
		t.Error("a string must not be read as true")
	}
	if m.Archetypes != nil {
		t.Errorf("archetypes = %v, want nil", m.Archetypes)
	}
	if !hasDiag(diags, "is not a boolean") || !hasDiag(diags, "is not an array of strings") {
		t.Fatalf("diagnostics = %v", diags)
	}
}

// [skill.tools] invites an author to write [skill] for the metadata too.
// Accepting it costs nothing and converts an honest mistake into a loaded
// skill instead of an ErrNoDescription rejection (REQ-SKILL-10).
func TestMetadataMayBeWrittenInTheSkillTableAndTheRootWins(t *testing.T) {
	m, diags := parseManifest(t, `
[skill]
name = "from-table"
description = "from the table"
archetypes = ["planner"]
`)
	if m.Name != "from-table" || m.Description != "from the table" {
		t.Fatalf("manifest = %+v", m)
	}
	if strings.Join(m.Archetypes, ",") != "planner" {
		t.Fatalf("archetypes = %v", m.Archetypes)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}

	m, _ = parseManifest(t, "description = \"root\"\n[skill]\ndescription = \"table\"\n")
	if m.Description != "root" {
		t.Fatalf("description = %q, want the root to win", m.Description)
	}
}

func TestAManifestSyntaxErrorReturnsTheErrorAndTheDiagnosticsSoFar(t *testing.T) {
	_, diags, err := ParseManifest("/x/skill.toml", []byte("weight = 0.5\n[[oops]]\n"))
	if err == nil {
		t.Fatal("want a syntax error")
	}
	if len(diags) != 1 || diags[0].Path != "/x/skill.toml" {
		t.Fatalf("diagnostics = %v, want the float warning carrying the file path", diags)
	}
}
