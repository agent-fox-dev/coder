package agentkit

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

func user(s string) core.Message {
	return core.UserMessage{Content: core.Content{core.TextBlock{Text: s}}}
}

func assistantSaying(s string, tokens int64) core.Message {
	m := core.AssistantMessage{
		Content:    core.Content{core.TextBlock{Text: s}},
		StopReason: core.StopReasonStop,
	}
	if tokens > 0 {
		m.Usage.SetField(core.UsageInputTokens, tokens)
	}
	return m
}

func longConversation(turns int) core.Messages {
	var out core.Messages
	body := strings.Repeat("x", 4000) // ~1000 tokens each
	for i := 0; i < turns; i++ {
		out = append(out, user("question "+body))
		out = append(out, assistantSaying("answer "+body, 0))
	}
	return out
}

// TestCompactionDoesNotOscillate is the test the plan flagged as the one that
// matters most, and it is written so the WRONG implementation fails.
//
// The naive reading of "compact when over threshold" re-evaluates from scratch
// every turn: the compacted request reports small usage → the next check
// passes → full history returns → the check fails again. Each swing
// invalidates the provider's cache prefix and re-sends content already paid to
// summarize.
//
// Five successive transforms over a history that only grows. The summarizer
// must run ONCE.
func TestCompactionDoesNotOscillate(t *testing.T) {
	var summarizerCalls atomic.Int32
	h := core.NewConversationHistory()
	msgs := longConversation(12)

	tf := NewContextTransform(CompactionDeps{
		Strategy: SummarizationCompaction{ThresholdFraction: 0.5, KeepTokens: 2000},
		Summarizer: func(ctx context.Context, prefix core.Messages, prev string) (string, error) {
			summarizerCalls.Add(1)
			return "SUMMARY", nil
		},
		History: h,
		Model:   &core.Model{ContextWindow: 20000},
	})

	ctx := context.Background()
	var last core.Messages
	for i := 0; i < 5; i++ {
		last = tf(ctx, msgs)
	}

	if got := summarizerCalls.Load(); got != 1 {
		t.Fatalf("summarizer ran %d times over 5 transforms of the SAME history, want 1.\n"+
			"The checkpoint must be applied FIRST and the estimate taken on the "+
			"COMPACTED view, so the threshold decides only whether to EXTEND. "+
			"Re-evaluating from scratch oscillates (REQ-GO-12.2).", got)
	}
	if len(last) >= len(msgs) {
		t.Fatalf("compacted view has %d messages, original %d: nothing was compacted",
			len(last), len(msgs))
	}
	if !strings.Contains(last[0].(core.UserMessage).Content.Text(), "SUMMARY") {
		t.Fatal("the compacted view does not lead with the summary")
	}
}

// TestCheckpointIsAppliedBeforeTheEstimate is the mechanism behind the test
// above, isolated: once a checkpoint exists it is applied on EVERY call, even
// when the threshold would no longer fire.
func TestCheckpointIsAppliedBeforeTheEstimate(t *testing.T) {
	h := core.NewConversationHistory()
	msgs := longConversation(8)
	h.SetCheckpoint(core.CompactionCheckpoint{
		PrefixLen: 10, Summary: "EARLIER", CreatedAtLen: len(msgs),
	})

	tf := NewContextTransform(CompactionDeps{
		// A strategy that never wants to compact. The checkpoint must still
		// be applied.
		Strategy: SummarizationCompaction{ThresholdFraction: 0.99, KeepTokens: 2000},
		Summarizer: func(context.Context, core.Messages, string) (string, error) {
			t.Fatal("summarizer must not run when the threshold does not fire")
			return "", nil
		},
		History: h,
		Model:   &core.Model{ContextWindow: 1000000},
	})

	out := tf(context.Background(), msgs)
	if len(out) != len(msgs)-10+1 {
		t.Fatalf("view has %d messages, want %d: an existing checkpoint must be "+
			"re-applied unconditionally (REQ-GO-12.2)", len(out), len(msgs)-10+1)
	}
	if !strings.Contains(out[0].(core.UserMessage).Content.Text(), "EARLIER") {
		t.Fatal("the existing summary was dropped")
	}
}

// TestCompactionNeverMutatesHistory: the view is derived, the log stays whole.
func TestCompactionNeverMutatesHistory(t *testing.T) {
	h := core.NewConversationHistory()
	msgs := longConversation(10)
	before := len(msgs)

	tf := NewContextTransform(CompactionDeps{
		Strategy:   SummarizationCompaction{ThresholdFraction: 0.1, KeepTokens: 1000},
		Summarizer: func(context.Context, core.Messages, string) (string, error) { return "S", nil },
		History:    h,
		Model:      &core.Model{ContextWindow: 10000},
	})
	_ = tf(context.Background(), msgs)

	if len(msgs) != before {
		t.Fatalf("history length changed from %d to %d: compaction must produce a VIEW",
			before, len(msgs))
	}
}

// TestCutSnapsForwardNeverBackward pins REQ-GO-14's direction.
//
// Snapping BACKWARD leaves a tool result in the kept tail whose originating
// tool_use has been summarized away — an orphan the provider rejects. The
// fixture puts a run of tool results exactly where a naive boundary lands.
func TestCutSnapsForwardNeverBackward(t *testing.T) {
	tuse, err := core.NewToolUse("c1", "t", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	msgs := core.Messages{
		user("first"),
		core.AssistantMessage{Content: core.Content{tuse}, StopReason: core.StopReasonToolUse},
		core.ToolResultMessage{ToolUseID: "c1", ToolName: "t"},
		core.ToolResultMessage{ToolUseID: "c1b", ToolName: "t"},
		user("second"),
		assistantSaying("done", 0),
	}
	// A boundary aimed at index 2 or 3 (both tool results) must move FORWARD.
	for _, from := range []int{2, 3} {
		got := snapForward(msgs, from, CutNotToolResult)
		if got < from {
			t.Fatalf("snapForward(%d) = %d: it moved BACKWARD, leaving a tool result "+
				"whose call was summarized away (REQ-GO-14)", from, got)
		}
		if _, isResult := msgs[got].(core.ToolResultMessage); isResult {
			t.Fatalf("snapForward(%d) = %d, which is still a tool result", from, got)
		}
	}
}

// TestWindowStrategiesCutOnUserMessagesOnly pins ruling P-8.
//
// REQ-GO-14's "not a tool_result" is necessary but not sufficient: it permits
// a cut landing on an ASSISTANT message. Summarization is fine there because
// it prepends a summary user message; the window strategies have nothing to
// prepend and would produce a view starting on an assistant turn, which
// Anthropic rejects and which REQ-PROV-11 does not repair.
func TestWindowStrategiesCutOnUserMessagesOnly(t *testing.T) {
	msgs := core.Messages{
		user("u1"), assistantSaying("a1", 0),
		user("u2"), assistantSaying("a2", 0),
		user("u3"), assistantSaying("a3", 0),
	}
	for name, s := range map[string]CompactionStrategy{
		"turn window":  TurnWindowCompaction{MaxTurns: 1},
		"token window": TokenWindowCompaction{KeepTokens: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if s.CutPolicy() != CutUserOnly {
				t.Fatalf("CutPolicy = %v, want CutUserOnly (ruling P-8)", s.CutPolicy())
			}
			cut := s.CutIndex(msgs, 100000)
			if cut > 0 {
				if _, ok := msgs[cut].(core.UserMessage); !ok {
					t.Fatalf("cut index %d is a %T; a window strategy has no summary to "+
						"prepend, so the view would start on a non-user turn", cut, msgs[cut])
				}
			}
		})
	}
}

// TestSummaryValidationTaxonomy pins REQ-GO-16, including the asymmetry that
// aborted is NOT a failure.
func TestSummaryValidationTaxonomy(t *testing.T) {
	tuse, _ := core.NewToolUse("c1", "t", json.RawMessage(`{}`))
	text := core.Content{core.TextBlock{Text: "a summary"}}

	cases := []struct {
		name    string
		msg     *core.AssistantMessage
		wantErr bool
	}{
		{"good", &core.AssistantMessage{Content: text, StopReason: core.StopReasonStop}, false},
		{"error is a failure", &core.AssistantMessage{Content: text, StopReason: core.StopReasonError}, true},
		{"max_tokens is a failure", &core.AssistantMessage{Content: text, StopReason: core.StopReasonLength}, true},
		{"a tool call is a failure even with text alongside",
			&core.AssistantMessage{Content: append(text.Clone(), tuse), StopReason: core.StopReasonStop}, true},
		{"aborted is NOT a failure; keep the partial text",
			&core.AssistantMessage{Content: text, StopReason: core.StopReasonAborted}, false},
		{"empty is a failure", &core.AssistantMessage{StopReason: core.StopReasonStop}, true},
		{"nil is a failure", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ValidateSummary(c.msg)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
		})
	}
}

// TestFailedSummarizationNeverCheckpoints pins ruling P-39: REQ-GO-16 governs
// the CHECKPOINT (never persist a bad summary), NFR-REL-05 governs the VIEW
// (never abort the session). Both must hold at once.
func TestFailedSummarizationNeverCheckpoints(t *testing.T) {
	h := core.NewConversationHistory()
	msgs := longConversation(10)
	var reported error

	tf := NewContextTransform(CompactionDeps{
		Strategy: SummarizationCompaction{ThresholdFraction: 0.1, KeepTokens: 1000},
		Summarizer: func(context.Context, core.Messages, string) (string, error) {
			return "", &ErrBadSummary{Reason: "truncated"}
		},
		History: h,
		Model:   &core.Model{ContextWindow: 10000},
		OnError: func(err error) { reported = err },
	})

	out := tf(context.Background(), msgs)
	if _, ok := h.Checkpoint(); ok {
		t.Fatal("a failed summarization must never checkpoint: compaction is permanent, " +
			"so a bad summary would poison every later turn (REQ-GO-16)")
	}
	if len(out) != len(msgs) {
		t.Fatal("a failed summarization must return the current view unchanged, " +
			"not abort the session (NFR-REL-05)")
	}
	if reported == nil {
		t.Fatal("the failure must be surfaced; compaction never aborts, so without " +
			"OnError it would be invisible")
	}
}

// TestExtendingReusesThePreviousSummary: when the checkpoint is extended, the
// previous summary is handed to the summarizer rather than discarded.
func TestExtendingReusesThePreviousSummary(t *testing.T) {
	h := core.NewConversationHistory()
	h.SetCheckpoint(core.CompactionCheckpoint{PrefixLen: 4, Summary: "FIRST", CreatedAtLen: 6})

	var sawPrevious string
	msgs := longConversation(20) // far past the threshold, so it extends
	tf := NewContextTransform(CompactionDeps{
		Strategy: SummarizationCompaction{ThresholdFraction: 0.1, KeepTokens: 1000},
		Summarizer: func(_ context.Context, _ core.Messages, prev string) (string, error) {
			sawPrevious = prev
			return "SECOND", nil
		},
		History: h,
		Model:   &core.Model{ContextWindow: 10000},
	})
	_ = tf(context.Background(), msgs)

	if sawPrevious != "FIRST" {
		t.Fatalf("the summarizer saw previous = %q, want %q: extending must build on "+
			"the existing summary rather than re-summarizing from nothing", sawPrevious, "FIRST")
	}
}

// TestApplyCheckpointWrapperIsPinned: the wrapper is model-visible format
// contract (REQ-SESS-07's sibling rule), so it gets a golden.
func TestApplyCheckpointWrapperIsPinned(t *testing.T) {
	out := ApplyCheckpoint(core.Messages{user("a"), user("b"), user("c")},
		core.CompactionCheckpoint{PrefixLen: 2, Summary: "the summary"})
	got := out[0].(core.UserMessage).Content.Text()
	want := "[Earlier conversation, summarized]\n\nthe summary"
	if got != want {
		t.Fatalf("wrapper text is model-visible contract.\n got: %q\nwant: %q", got, want)
	}
}
