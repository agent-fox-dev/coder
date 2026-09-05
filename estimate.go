package agentkit

import (
	"github.com/agentfox/agentkit-go/core"
)

// Token estimation constants. These are a heuristic, deliberately: there is no
// tokenizer in AgentKit and adding one would be a dependency (REQ-GO-11) whose
// output would still be wrong for every provider that does not use it.
const (
	// charsPerToken is the classic 4:1 approximation, used ONLY for the
	// messages after the anchor.
	charsPerToken = 4
	// charsPerImage is a flat per-image cost. Estimating an image by its
	// base64 length is wildly wrong — providers tile and resize server-side —
	// so a flat figure is both simpler and more accurate (REQ-GO-15).
	charsPerImage = 4800
)

// EstimateContextTokens is REQ-GO-15's anchored accounting.
//
// It does NOT walk the transcript summing characters. It scans backwards for
// the newest assistant message that is a valid anchor, takes that message's
// PROVIDER-REPORTED usage as the base, and estimates only the messages after
// it. That is both cheaper and far more accurate than re-estimating, and it is
// the number the compaction trigger and the REQ-CAT-04 clamp both consume.
//
// Three skip rules are mandatory, and each exists because the thing it skips
// LOOKS like a valid anchor and silently resets the estimate to near zero:
//
//	(a) stop_reason aborted or error — the turn did not complete, so its usage
//	    describes a request that was never fully answered.
//	(b) zero reported usage — REQ-PROV-05 explicitly permits a provider to
//	    report zeros, and a zero anchor reads as "the context is empty".
//	(c) invalidated by a later-inserted prefix message — after a compaction,
//	    an assistant message from before the summary was sent under a
//	    different prefix, so its usage cannot describe the reshaped one.
//
// Rule (c) is not evaluable from []Message alone: nothing on AssistantMessage
// records which prefix it was sent under. It needs the checkpoint's
// CreatedAtLen, which is why that field exists (ruling P-2). An implementation
// without it passes a naive test suite while rule (c) never fires at all.
//
// When no valid anchor exists, it falls back to the pure heuristic over the
// whole view.
func EstimateContextTokens(msgs core.Messages, cp *core.CompactionCheckpoint) int {
	minAnchor := 0
	if cp != nil {
		minAnchor = cp.MinAnchorIndex()
	}

	for i := len(msgs) - 1; i >= 0; i-- {
		am, ok := msgs[i].(core.AssistantMessage)
		if !ok {
			continue
		}
		// (a)
		if am.StopReason.ShortCircuits() {
			continue
		}
		// (b)
		if !am.Usage.Reported() || am.Usage.IsZero() {
			continue
		}
		// (c)
		if i < minAnchor {
			continue
		}
		return int(am.Usage.ContextTokens()) + estimateRange(msgs, i+1)
	}
	return estimateRange(msgs, 0)
}

// estimateRange is the pure heuristic over msgs[from:].
func estimateRange(msgs core.Messages, from int) int {
	chars := 0
	for i := from; i < len(msgs); i++ {
		chars += estimateMessage(msgs[i])
	}
	return chars / charsPerToken
}

func estimateMessage(m core.Message) int {
	switch v := m.(type) {
	case core.UserMessage:
		return estimateContent(v.Content)
	case core.AssistantMessage:
		return estimateContent(v.Content)
	case core.ToolResultMessage:
		return estimateContent(v.Content) + len(v.ToolName)
	}
	return 0
}

func estimateContent(c core.Content) int {
	n := 0
	for _, b := range c {
		switch v := b.(type) {
		case core.TextBlock:
			n += len(v.Text)
		case core.ThinkingBlock:
			n += len(v.Thinking)
		case core.ToolUseBlock:
			n += len(v.Name) + len(v.Input)
		case core.ToolResultBlock:
			n += estimateContent(v.Content)
		case core.ImageBlock:
			n += charsPerImage
		case core.RawBlock:
			n += len(v.Raw)
		}
	}
	return n
}
