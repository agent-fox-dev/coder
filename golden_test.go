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

	"github.com/agentfox/agentkit-go/compaction"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/prompt"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/session"
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

// ---- (b) the per-provider request body

// TestGoldenProviderRequestBodies pins the wire body each provider builds from
// one canonical request (NFR-TEST-06/08b).
//
// The same input for all of them, so a diff shows what a provider does
// DIFFERENTLY rather than what its fixture happened to contain.
//
// PROVENANCE (NFR-TEST-08.1) — and the honest limit of these five files
//
//	goldens:   testdata/golden/request_{anthropic,openai,openai_responses,google,ollama}.json
//	reference: AgentKit itself, captured through RequestOptions.OnPayload with
//	           no network and no API key. THIS IS NOT A VENDOR CAPTURE. These
//	           pin the request body against REGRESSION — they catch AgentKit
//	           changing what it sends — and say nothing about whether what it
//	           sends is what the vendor currently accepts.
//	version:   the working tree; the pinned API versions are in docs/PROVIDERS.md
//	command:   go test -run TestGoldenProviderRequestBodies -update .
//
// Pinning these against TRUTH is NFR-TEST-06's differential harness
// (difftest/), which reports DARK until a vendor SDK or a live capture
// supplies an independent reference. NFR-TEST-08.2 forbids treating this
// file's output as that reference, which is why the ledger's capture-date
// column is empty rather than filled in with today.
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
//
// PROVENANCE (NFR-TEST-08.1)
//
//	golden:    testdata/golden/session_log.jsonl
//	reference: AgentKit itself — the real store writing through the real
//	           codec. The format is ours to define, so there is no external
//	           reference; what the golden buys is that a codec change shows
//	           up as a diff in the durable format rather than as a resume
//	           failure in someone's session six months from now.
//	version:   the working tree
//	command:   go test -run TestGoldenSessionLog -update .
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
//
// PROVENANCE (NFR-TEST-08.1)
//
//	golden:    testdata/golden/model_visible_wrappers.txt
//	reference: AgentKit itself — REQ-SESS-07 makes these strings OUR format
//	           contract with the model, so we are the reference by definition.
//	version:   the working tree
//	command:   go test -run TestGoldenModelVisibleWrappers -update .
func TestGoldenModelVisibleWrappers(t *testing.T) {
	var b strings.Builder
	b.WriteString("### branch_summary\n")
	b.WriteString(session.RenderBranchSummary("Tried the wrong glob; switched to **/*.go."))
	b.WriteString("\n### compaction\n")
	b.WriteString(compaction.SummaryPrefix + "The user asked for the Go files and got them.")
	b.WriteString("\n### compaction, split turn (REQ-GO-14)\n")
	b.WriteString(compaction.SummaryPrefix + "The user asked for the Go files and got them." +
		compaction.SplitSeparator + "The user then asked for the tests; the assistant had listed the directory.")
	b.WriteString("\n")
	checkGolden(t, "model_visible_wrappers.txt", b.String())
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
	if !strings.Contains(sys, prompt.BaseInstructions) {
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
