package agentkit

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// TestExtendingSummarizesOnlyTheDelta: extending a checkpoint hands the
// summarizer the messages the previous checkpoint did NOT cover, plus the
// previous summary — never the whole original prefix again.
//
// The mutation this catches is summarizing msgs[:cut] on every extension.
// That is O(total history) tokens per extension, and once the history has
// outgrown the context window the summarization request itself fails on
// every turn while the view stays over the threshold — compaction stops
// working exactly when it is needed.
func TestExtendingSummarizesOnlyTheDelta(t *testing.T) {
	h := core.NewConversationHistory()
	h.SetCheckpoint(core.CompactionCheckpoint{PrefixLen: 6, Summary: "FIRST", CreatedAtLen: 8})

	msgs := numberedConversation(20) // 40 messages, far past the threshold
	var (
		sawFirst int
		sawLen   int
		sawPrev  string
	)
	tf := NewContextTransform(CompactionDeps{
		Strategy: SummarizationCompaction{ThresholdFraction: 0.1, KeepTokens: 1000},
		Summarizer: func(_ context.Context, prefix core.Messages, prev string) (string, error) {
			sawPrev, sawLen = prev, len(prefix)
			sawFirst = indexOfText(msgs, prefix)
			return "SECOND", nil
		},
		History: h,
		Model:   &core.Model{ContextWindow: 10000},
	})
	_ = tf(context.Background(), msgs)

	if sawPrev != "FIRST" {
		t.Fatalf("previous summary = %q, want FIRST", sawPrev)
	}
	if sawFirst != 6 {
		t.Fatalf("the summarizer's prefix started at original index %d, want 6 (the first message "+
			"the previous checkpoint did not cover); re-summarizing from 0 re-sends paid-for history", sawFirst)
	}
	cp, _ := h.Checkpoint()
	if cp.PrefixLen <= 6 || sawLen != cp.PrefixLen-6 {
		t.Fatalf("new PrefixLen %d, summarized %d messages; want PrefixLen > 6 and exactly PrefixLen-6 summarized",
			cp.PrefixLen, sawLen)
	}
}

// numberedConversation is longConversation with a distinct text per message,
// so a message can be located by content.
func numberedConversation(turns int) core.Messages {
	var out core.Messages
	body := strings.Repeat("x", 4000)
	for i := 0; i < turns; i++ {
		out = append(out, user(fmt.Sprintf("q%d %s", i, body)))
		out = append(out, assistantSaying(fmt.Sprintf("a%d %s", i, body), 0))
	}
	return out
}

func textOf(m core.Message) string {
	switch v := m.(type) {
	case core.UserMessage:
		return v.Content.Text()
	case core.AssistantMessage:
		return v.Content.Text()
	}
	return ""
}

// indexOfText locates prefix[0] in msgs by text; -1 when absent or empty.
func indexOfText(msgs, prefix core.Messages) int {
	if len(prefix) == 0 {
		return -1
	}
	for i := range msgs {
		if textOf(msgs[i]) == textOf(prefix[0]) {
			return i
		}
	}
	return -1
}

// TestACutBelowTheCheckpointDoesNotShrinkIt: the cut is chosen by a character
// heuristic and the threshold by a provider-reported anchor, so a cut can land
// at or before the existing checkpoint. Compaction is permanent
// (REQ-GO-12.2): such a cut extends nothing and must not move the checkpoint
// backwards or call the summarizer.
func TestACutBelowTheCheckpointDoesNotShrinkIt(t *testing.T) {
	h := core.NewConversationHistory()
	msgs := longConversation(20)
	h.SetCheckpoint(core.CompactionCheckpoint{PrefixLen: 38, Summary: "DEEP", CreatedAtLen: 40})

	calls := 0
	tf := NewContextTransform(CompactionDeps{
		// KeepTokens large enough that the cut lands well before index 38.
		Strategy: SummarizationCompaction{ThresholdFraction: 0.01, KeepTokens: 10000},
		Summarizer: func(context.Context, core.Messages, string) (string, error) {
			calls++
			return "WRONG", nil
		},
		History: h,
		Model:   &core.Model{ContextWindow: 10000},
	})
	view := tf(context.Background(), msgs)

	cp, _ := h.Checkpoint()
	if cp.PrefixLen != 38 || cp.Summary != "DEEP" {
		t.Fatalf("checkpoint moved to %+v; a cut below the checkpoint must leave it alone", cp)
	}
	if calls != 0 {
		t.Fatalf("summarizer called %d times for a cut that extends nothing", calls)
	}
	if len(view) != 1+len(msgs)-38 {
		t.Fatalf("view has %d messages, want the compacted view (%d)", len(view), 1+len(msgs)-38)
	}
}

// TestADeltaStartingOnAnAssistantGetsAUserTurnFirst: a CutNotToolResult
// checkpoint can leave the next delta beginning on an assistant message, and
// every wire rejects a request whose first message is not the user's. The
// summarizer prepends a fixed note so the request stays valid.
func TestADeltaStartingOnAnAssistantGetsAUserTurnFirst(t *testing.T) {
	var sent core.Messages
	p := core.ClientFunc(func(_ context.Context, _ *core.Model, r core.Request, _ core.ProviderStreamOptions) *core.EventStream {
		sent = r.Messages
		s := core.NewEventStream(core.StreamOptions{})
		s.End(core.StreamResult{Message: &core.AssistantMessage{
			Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop}})
		return s
	})
	sum := ModelSummarizer(p, &core.Model{MaxTokens: 1000}, 0)
	_, err := sum(context.Background(), core.Messages{assistantSaying("reply", 0), user("next")}, "PREV")
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 3 || sent[0].Role() != core.RoleUser {
		t.Fatalf("request must open with a user message, got %d messages starting with %q", len(sent), sent[0].Role())
	}
	if got := sent[0].(core.UserMessage).Content.Text(); got != summaryContinuationNote {
		t.Fatalf("continuation note is model-visible contract, got %q", got)
	}
}

// TestSummaryMaxTokensIsBoundedWithoutAReserve: with no reserve stated the
// summary's max_tokens is the model cap bounded by DefaultMaxTokens, not the
// raw 128K cap.
func TestSummaryMaxTokensIsBoundedWithoutAReserve(t *testing.T) {
	if got := summaryMaxTokens(&core.Model{MaxTokens: 128000}, 0); got != DefaultMaxTokens {
		t.Fatalf("got %d, want %d", got, DefaultMaxTokens)
	}
	if got := summaryMaxTokens(&core.Model{MaxTokens: 128000}, 100000); got != 80000 {
		t.Fatalf("an explicit reserve is honoured: got %d, want 80000", got)
	}
}
