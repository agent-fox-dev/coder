package compaction

import (
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// TestEstimateSkipRulesEachFireIndependently pins REQ-GO-15. Each case
// satisfies the OTHER two skip rules, so deleting any one `continue` from the
// implementation fails exactly one subtest.
func TestEstimateSkipRulesEachFireIndependently(t *testing.T) {
	good := core.AssistantMessage{StopReason: core.StopReasonStop}
	good.Usage.SetField(core.UsageInputTokens, 1000)

	t.Run("(a) aborted turn is not an anchor", func(t *testing.T) {
		bad := core.AssistantMessage{StopReason: core.StopReasonAborted}
		bad.Usage.SetField(core.UsageInputTokens, 999999)
		got := EstimateContextTokens(core.Messages{good, bad}, nil)
		if got > 2000 {
			t.Fatalf("estimate %d used an aborted turn as the anchor", got)
		}
	})

	t.Run("(b) zero-usage turn is not an anchor", func(t *testing.T) {
		var zero core.AssistantMessage
		zero.StopReason = core.StopReasonStop
		zero.Usage.SetField(core.UsageInputTokens, 0)
		got := EstimateContextTokens(core.Messages{good, zero}, nil)
		if got < 1000 {
			t.Fatalf("estimate %d fell back past the valid anchor: a zero-usage response "+
				"was treated as authoritative", got)
		}
	})

	t.Run("(c) anchor invalidated by a later-inserted prefix", func(t *testing.T) {
		// The checkpoint says a summary was inserted, so an assistant message
		// from before it was sent under a different prefix.
		cp := &core.CompactionCheckpoint{PrefixLen: 1, Summary: "s", CreatedAtLen: 4}
		msgs := core.Messages{good, good, good}
		got := EstimateContextTokens(msgs, cp)
		if got >= 1000 {
			t.Fatalf("estimate %d used a stale anchor: rule (c) never fired. Without "+
				"CompactionCheckpoint.CreatedAtLen it cannot fire at all (ruling P-2)", got)
		}
	})
}
