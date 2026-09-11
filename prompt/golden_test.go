package prompt

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/tools"
)

// NFR-TEST-08(a): byte-for-byte goldens of the assembled system prompt.
//
// -update rewrites them. That flag is the danger the requirement names: "a
// golden regenerated from the output it exists to check is circular". The
// discipline is that a diff is REVIEWED, not blessed — the point of the
// assembled-prompt golden is precisely that a change to any tool's description
// shows up in a code review as a prompt diff.
var update = flag.Bool("update", false, "rewrite golden files")

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v\n\nRun `go test -run %s -update` and REVIEW the diff before "+
			"committing it.", err, t.Name())
	}
	if string(want) != got {
		t.Fatalf("%s drifted.\n\n--- want ---\n%s\n--- got ---\n%s\n\n"+
			"If the change is intended, run `go test -run %s -update` and review "+
			"the diff as part of the change.", name, want, got, t.Name())
	}
}

// defaultToolSet builds the REAL default tools, through the real resolver
// (NFR-TEST-08a). A fixture list here would defeat the whole test: the golden
// exists so that editing any tool's description or guidelines shows up as a
// prompt diff in review.
func defaultToolSet(t *testing.T) []core.Tool {
	t.Helper()
	ws, err := tools.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	all, err := tools.All(tools.Options{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	return core.ToolPolicy{}.Resolve(all)
}

// TestGoldenDefaultSystemPrompt pins the whole assembled default prompt.
//
// PROVENANCE (NFR-TEST-08.1)
//
//	golden:    testdata/golden/system_prompt_default.txt
//	reference: AgentKit itself — BuildSystemPrompt over the real default tool
//	           set, resolved through tools.All and ResolveToolPolicy. There is
//	           no external reference for this artifact and there cannot be:
//	           the prompt IS ours, and NFR-TEST-08.2's "regenerate from the
//	           reference" reduces here to "regenerate from the resolver, never
//	           from a hand-edited expectation".
//	version:   the working tree
//	command:   go test -run TestGoldenDefaultSystemPrompt -update .
func TestGoldenDefaultSystemPrompt(t *testing.T) {
	got := Build(Input{Tools: defaultToolSet(t)})
	checkGolden(t, "system_prompt_default.txt", got)
}

// TestGoldenCustomSystemPrompt is the second golden NFR-TEST-08(a) asks for:
// assembly order, and the assertion that built-in blocks are ABSENT.
//
// PROVENANCE (NFR-TEST-08.1)
//
//	golden:    testdata/golden/system_prompt_custom.txt
//	reference: AgentKit itself, as above — the custom-prompt branch of
//	           BuildSystemPrompt over the real default tool set.
//	version:   the working tree
//	command:   go test -run TestGoldenCustomSystemPrompt -update .
func TestGoldenCustomSystemPrompt(t *testing.T) {
	got := Build(Input{
		Custom: "You are a release engineer. Answer only about this repository.",
		Tools:  defaultToolSet(t),
		ExtraBlocks: []string{
			"<project_context>\n  <file path=\"/repo/AGENTS.md\">house style</file>\n</project_context>",
		},
	})
	checkGolden(t, "system_prompt_custom.txt", got)

	// Stated as assertions too, not left to a reader of the golden: a diff
	// that reintroduced a built-in block would still be a passing golden the
	// day someone regenerated it.
	if strings.Contains(got, BaseInstructions) {
		t.Fatal("a custom system prompt must replace the built-in base instructions")
	}
	if strings.Contains(got, "Guidelines:") {
		t.Fatal("a custom system prompt must replace the built-in guidelines block")
	}
	if !strings.Contains(got, "project_context") {
		t.Fatal("discovered content must survive a custom prompt: an embedder enabled " +
			"it by a separate affirmative act (REQ-SEC-10), and a custom prompt is " +
			"not a decision to revoke that")
	}
}

// TestGoldenPromptWithoutFileNavigationTools pins REQ-TOOL-04e: the guideline
// appears when list_files, find_files and search_files are ABSENT and execute
// is present. Its condition is an absence, which no per-tool field can express.
//
// PROVENANCE (NFR-TEST-08.1)
//
//	golden:    testdata/golden/system_prompt_no_navigation.txt
//	reference: AgentKit itself — the real default set with the navigation
//	           trio removed, through the real resolver.
//	version:   the working tree
//	command:   go test -run TestGoldenPromptWithoutFileNavigationTools -update .
func TestGoldenPromptWithoutFileNavigationTools(t *testing.T) {
	all := defaultToolSet(t)
	var kept []core.Tool
	for _, tl := range all {
		switch tl.Name {
		case "list_files", "find_files", "search_files":
		default:
			kept = append(kept, tl)
		}
	}
	got := Build(Input{Tools: kept})
	checkGolden(t, "system_prompt_no_navigation.txt", got)

	if !strings.Contains(got, tools.ExecuteFallbackGuideline) {
		t.Fatalf("REQ-TOOL-04e's guideline is missing:\n%s", got)
	}
}

// TestGuidelinesAreDeduplicatedPreservingFirstSeenOrder is NFR-TEST-08a's
// other half.
//
// It uses synthetic tools rather than the default set, because the default set
// happens to have no duplicate guidelines — so the assembled-prompt golden
// cannot tell a deduplicating builder from one that just never had to. Sorting
// is the tempting alternative and is wrong: the resolution order is the order
// the model reads the tools in, and alphabetizing separates a guideline from
// the tool it is about.
func TestGuidelinesAreDeduplicatedPreservingFirstSeenOrder(t *testing.T) {
	set := []core.Tool{
		{Name: "zeta", PromptGuidelines: []string{"Zeta first.", "Shared advice."}},
		{Name: "alpha", PromptGuidelines: []string{"Shared advice.", "Alpha second."}},
	}
	got := Build(Input{Tools: set})

	want := "Guidelines:\n" +
		"- Zeta first.\n" +
		"- Shared advice.\n" +
		"- Alpha second.\n" +
		"- Do not guess at file contents or APIs; read them.\n" +
		"- Report what you actually did, including what failed."
	if !strings.Contains(got, want) {
		t.Fatalf("guidelines block wrong.\nwant:\n%s\n\ngot:\n%s", want, got)
	}
	if strings.Count(got, "Shared advice.") != 1 {
		t.Fatalf("a guideline declared by two tools must appear once:\n%s", got)
	}
}
