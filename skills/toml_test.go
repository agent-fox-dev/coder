package skills

import (
	"errors"
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) (*Table, []Diagnostic) {
	t.Helper()
	tbl, diags, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatalf("ParseTOML: %v", err)
	}
	return tbl, diags
}

func str(t *testing.T, tbl *Table, key string) string {
	t.Helper()
	v, ok := tbl.Get(key)
	if !ok {
		t.Fatalf("key %q missing", key)
	}
	if v.Kind != KindString {
		t.Fatalf("key %q is kind %v, want string", key, v.Kind)
	}
	return v.Str
}

func TestTOMLParsesStringsBoolsIntegersAndStringArrays(t *testing.T) {
	tbl, diags := mustParse(t, `
name = "reviewer"
disable_model_invocation = true
retries = -12
grouped = 1_000
archetypes = ["coder", "reviewer"]
`)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if got := str(t, tbl, "name"); got != "reviewer" {
		t.Errorf("name = %q", got)
	}
	if v, _ := tbl.Get("disable_model_invocation"); v.Kind != KindBool || !v.Bool {
		t.Errorf("disable_model_invocation = %+v", v)
	}
	if v, _ := tbl.Get("retries"); v.Kind != KindInt || v.Int != -12 {
		t.Errorf("retries = %+v", v)
	}
	if v, _ := tbl.Get("grouped"); v.Int != 1000 {
		t.Errorf("grouped = %+v", v)
	}
	v, _ := tbl.Get("archetypes")
	if v.Kind != KindStringArray || strings.Join(v.Array, ",") != "coder,reviewer" {
		t.Errorf("archetypes = %+v", v)
	}
}

// A '#' inside a string is data, not a comment. The obvious implementation —
// strip everything after the first '#' on the line, then split on '=' — reads
// this description as `counts ` and truncates the only field REQ-SKILL-10
// treats as mandatory.
func TestTOMLHashInsideAStringIsNotAComment(t *testing.T) {
	tbl, _ := mustParse(t, `description = "counts # of open PRs"  # trailing note`)
	if got := str(t, tbl, "description"); got != "counts # of open PRs" {
		t.Fatalf("description = %q", got)
	}
}

func TestTOMLEqualsInsideAStringIsNotASeparator(t *testing.T) {
	tbl, _ := mustParse(t, `description = "set x = 1"`)
	if got := str(t, tbl, "description"); got != "set x = 1" {
		t.Fatalf("description = %q", got)
	}
}

func TestTOMLArrayMaySpanLinesAndCarryATrailingComma(t *testing.T) {
	tbl, diags := mustParse(t, `
archetypes = [
  "coder",   # the default
  "planner",
]
`)
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	v, _ := tbl.Get("archetypes")
	if strings.Join(v.Array, "|") != "coder|planner" {
		t.Fatalf("archetypes = %+v", v)
	}
}

func TestTOMLSectionsAndSubSectionsNest(t *testing.T) {
	tbl, _ := mustParse(t, `
name = "x"

[skill.tools]
module = "example.com/m"
factory = "New"

[skill.session]
max_turns_add = 5
`)
	skill, ok := tbl.Sub("skill")
	if !ok {
		t.Fatal("[skill] missing")
	}
	tools, ok := skill.Sub("tools")
	if !ok {
		t.Fatal("[skill.tools] missing")
	}
	if got := str(t, tools, "module"); got != "example.com/m" {
		t.Errorf("module = %q", got)
	}
	sess, _ := skill.Sub("session")
	if v, _ := sess.Get("max_turns_add"); v.Int != 5 {
		t.Errorf("max_turns_add = %+v", v)
	}
	if tools.Name() != "skill.tools" {
		t.Errorf("qualified name = %q", tools.Name())
	}
}

func TestTOMLDottedKeyWritesIntoTheNestedTable(t *testing.T) {
	tbl, _ := mustParse(t, `skill.tools.module = "example.com/m"`)
	skill, _ := tbl.Sub("skill")
	tools, ok := skill.Sub("tools")
	if !ok {
		t.Fatal("skill.tools missing")
	}
	if got := str(t, tools, "module"); got != "example.com/m" {
		t.Fatalf("module = %q", got)
	}
}

// An unsupported value form loses THAT KEY and nothing else. A parser that
// returned an error here would delete a whole skill over a field it does not
// even read, which is the failure REQ-SKILL-10 exists to prevent.
func TestTOMLUnsupportedValueSkipsOnlyItsOwnKey(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
	}{
		{"float", `weight = 0.5`, "floats"},
		{"datetime", `built = 1979-05-27T07:32:00Z`, "dates"},
		{"inline table", `t = { a = "b" }`, "inline tables"},
		{"multi-line string", "s = \"\"\"a\nb\"\"\"", "multi-line"},
		{"hex integer", `mask = 0xdeadbeef`, "decimal"},
		{"mixed array", `a = ["x", 3]`, "arrays of strings"},
		{"nested array", `a = [["x"]]`, "arrays of strings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl, diags := mustParse(t, tc.src+"\ndescription = \"survives\"\n")
			if got := str(t, tbl, "description"); got != "survives" {
				t.Fatalf("later key lost: %q", got)
			}
			if len(diags) != 1 {
				t.Fatalf("diagnostics = %v, want exactly one", diags)
			}
			if !strings.Contains(diags[0].Message, tc.want) {
				t.Fatalf("diagnostic %q does not mention %q", diags[0].Message, tc.want)
			}
			if diags[0].Severity != SeverityWarning {
				t.Fatalf("severity = %q, want warning", diags[0].Severity)
			}
		})
	}
}

func TestTOMLStructuralFailuresAreErrorsWithALineNumber(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		line      int
	}{
		{"array of tables", "name = \"a\"\n[[servers]]\n", 2},
		{"unterminated string", "name = \"a\ndescription = \"b\"\n", 1},
		{"unterminated array", "a = [\"x\",\n", 1},
		{"missing equals", "name \"a\"\n", 1},
		{"garbage after value", "name = \"a\" oops\n", 1},
		{"unclosed header", "[skill\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseTOML([]byte(tc.src))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("error %v is not a *SyntaxError", err)
			}
			if se.Line != tc.line {
				t.Fatalf("line = %d, want %d (%v)", se.Line, tc.line, err)
			}
		})
	}
}

func TestTOMLBasicStringProcessesEscapesAndLiteralStringDoesNot(t *testing.T) {
	tbl, _ := mustParse(t, "basic = \"a\\tb\\n\\u00e9\"\nliteral = 'a\\tb'\n")
	if got := str(t, tbl, "basic"); got != "a\tb\né" {
		t.Errorf("basic = %q", got)
	}
	if got := str(t, tbl, "literal"); got != `a\tb` {
		t.Errorf("literal = %q", got)
	}
}

func TestTOMLDuplicateKeyWarnsAndTheLastValueWins(t *testing.T) {
	tbl, diags := mustParse(t, "description = \"first\"\ndescription = \"second\"\n")
	if got := str(t, tbl, "description"); got != "second" {
		t.Fatalf("description = %q", got)
	}
	if len(diags) != 1 || !strings.Contains(diags[0].Message, "duplicate") {
		t.Fatalf("diagnostics = %v", diags)
	}
	if n := len(tbl.Keys()); n != 1 {
		t.Fatalf("Keys() = %v, want the key recorded once", tbl.Keys())
	}
}

func TestTOMLLeadingBOMIsNotPartOfTheFirstKey(t *testing.T) {
	tbl, _ := mustParse(t, "\ufeffdescription = \"x\"")
	if _, ok := tbl.Get("description"); !ok {
		t.Fatalf("keys = %v; the BOM was read as part of the key", tbl.Keys())
	}
}

func TestTOMLKeyOrderFollowsTheFileNotTheMap(t *testing.T) {
	tbl, _ := mustParse(t, "zulu = \"1\"\nalpha = \"2\"\nmike = \"3\"\n")
	if got := strings.Join(tbl.Keys(), ","); got != "zulu,alpha,mike" {
		t.Fatalf("Keys() = %q, want written order", got)
	}
}
