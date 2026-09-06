package agentkit

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/session"
	"github.com/agentfox/agentkit-go/tools"
)

// NFR-TEST-08: byte-for-byte goldens for the artifacts assembled from many
// parts, where no single unit is wrong but the composed whole drifts.
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

// ---- (a) the assembled system prompt

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
	return ResolveToolPolicy(all, core.ToolPolicy{})
}

// TestGoldenDefaultSystemPrompt pins the whole assembled default prompt.
func TestGoldenDefaultSystemPrompt(t *testing.T) {
	got := BuildSystemPrompt(PromptInput{Tools: defaultToolSet(t)})
	checkGolden(t, "system_prompt_default.txt", got)
}

// TestGoldenCustomSystemPrompt is the second golden NFR-TEST-08(a) asks for:
// assembly order, and the assertion that built-in blocks are ABSENT.
func TestGoldenCustomSystemPrompt(t *testing.T) {
	got := BuildSystemPrompt(PromptInput{
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
	got := BuildSystemPrompt(PromptInput{Tools: kept})
	checkGolden(t, "system_prompt_no_navigation.txt", got)

	if !strings.Contains(got, tools.ExecuteFallbackGuideline) {
		t.Fatalf("REQ-TOOL-04e's guideline is missing:\n%s", got)
	}
}

// ---- (b) the per-provider request body

// TestGoldenProviderRequestBodies pins the wire body each provider builds from
// one canonical request (NFR-TEST-06/08b).
//
// The same input for all of them, so a diff shows what a provider does
// DIFFERENTLY rather than what its fixture happened to contain.
func TestGoldenProviderRequestBodies(t *testing.T) {
	for _, tc := range goldenRequestCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			checkGolden(t, "request_"+tc.name+".json", tc.body)
		})
	}
}

// ---- (c) the serialized session log

// TestGoldenSessionLog pins the on-disk format (NFR-REL-04).
//
// Ids and timestamps are injected, which is what makes a whole-file golden
// possible at all — session.Options documents both hooks as existing for this.
func TestGoldenSessionLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	n := 0
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	opts := session.Options{
		Durability: session.DurabilityPerEntry,
		NewID: func() core.EntryID {
			n++
			return core.EntryID(strings.Repeat("0", 28) + padID(n))
		},
		Now: func() time.Time {
			at = at.Add(time.Second)
			return at
		},
	}
	store, err := session.Create(path, core.SessionHeader{
		Version: 1, ID: "golden-session", Timestamp: at, CWD: "/repo",
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	rec := session.NewRecorder(store, core.NewConversationHistory(), func(err error) { t.Fatal(err) })

	if _, err := rec.RecordMessage(core.UserMessage{
		Content: core.Content{core.TextBlock{Text: "list the go files"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.RecordModelChange("anthropic", core.API("anthropic-messages"), "claude-x"); err != nil {
		t.Fatal(err)
	}
	call, err := core.NewToolUse("call_1", "find_files", json.RawMessage(`{"pattern":"**/*.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.RecordMessage(core.AssistantMessage{
		Content:    core.Content{core.TextBlock{Text: "Looking."}, call},
		StopReason: core.StopReasonToolUse,
		Provider:   "anthropic", API: core.API("anthropic-messages"), Model: "claude-x",
		Usage: core.Usage{InputTokens: 12, OutputTokens: 7},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.RecordMessage(core.ToolResultMessage{
		ToolUseID: "call_1", ToolName: "find_files",
		Content: core.Content{core.TextBlock{Text: `{"ok":true,"data":{"entries":["main.go"]}}`}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.RecordBranchSummary("The earlier attempt used the wrong glob.",
		core.EntryID("leaf"), core.EntryID("fork")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "session_log.jsonl", string(raw))
}

func padID(n int) string {
	s := "0000"
	d := []byte(s)
	for i := len(d) - 1; i >= 0 && n > 0; i-- {
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d)
}

// ---- (d) the model-visible wrapper strings of REQ-SESS-07

// TestGoldenModelVisibleWrappers pins the wrapper strings as RENDERED.
//
// The constants are already asserted in their own packages; what this pins is
// the composed result — prefix, body and suffix as the model reads them. A
// change to either constant, or to how they are joined, shows up here.
func TestGoldenModelVisibleWrappers(t *testing.T) {
	var b strings.Builder
	b.WriteString("### branch_summary\n")
	b.WriteString(session.RenderBranchSummary("Tried the wrong glob; switched to **/*.go."))
	b.WriteString("\n### compaction\n")
	b.WriteString(CompactionSummaryPrefix + "The user asked for the Go files and got them.")
	b.WriteString("\n")
	checkGolden(t, "model_visible_wrappers.txt", b.String())
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
	got := BuildSystemPrompt(PromptInput{Tools: set})

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

// TestTheAssembledPromptReachesTheProvider. Everything above tests the
// assembler; this tests that the loop uses it. Until it did, PromptGuidelines
// was a field nothing read — a tool could declare guidance the model never saw.
func TestTheAssembledPromptReachesTheProvider(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, nil)
	if err := a.RegisterTool(core.Tool{
		Name: "widget", Description: "does a thing", InputSchema: schema.Object(),
		PromptGuidelines: []string{"Use widget for widget-shaped problems."},
		Execute: func(context.Context, json.RawMessage) core.ToolResult {
			return core.OKResult(nil)
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	if len(s.seen) == 0 {
		t.Fatal("the provider was never called")
	}
	sys := systemTextOf(t, s)
	if !strings.Contains(sys, "Use widget for widget-shaped problems.") {
		t.Fatalf("a registered tool's guideline never reached the provider.\nsystem = %q", sys)
	}
	if !strings.Contains(sys, BaseInstructions) {
		t.Fatalf("the built-in base instructions never reached the provider.\nsystem = %q", sys)
	}
}

// TestACustomPromptReachesTheProviderWithoutBuiltins is the same wiring for
// the other branch.
func TestACustomPromptReachesTheProviderWithoutBuiltins(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.SystemPrompt = "Only answer in haiku."
	})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	sys := systemTextOf(t, s)
	if sys != "Only answer in haiku." {
		t.Fatalf("a custom prompt must reach the provider alone; got %q", sys)
	}
}

func systemTextOf(t *testing.T, s *scripted) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.systems) == 0 {
		t.Fatal("the provider recorded no system prompt")
	}
	var b strings.Builder
	for _, blk := range s.systems[0] {
		if tb, ok := blk.(core.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}
