// Package provider holds the machinery every wire API implementation shares:
// send-time transcript repair, SSE reading, retry classification and cost.
//
// The concrete wire APIs live in subpackages (provider/anthropic,
// provider/openai, provider/faux), one per API, because REQ-PROV-02 keys
// providers by wire protocol rather than by vendor.
package provider

import (
	"fmt"
	"strings"

	"github.com/agentfox/agentkit-go/core"
)

// SyntheticResultText is the content of a tool result invented by rule 6. It
// is model-visible, so it is pinned here rather than formatted at the call
// site.
const SyntheticResultText = "No result provided"

// RepairReport records what a repair pass changed. It is returned rather than
// logged: a caller diagnosing a 400 needs to know whether the transcript it
// sent was the transcript it holds, and a log line is not available to a test.
type RepairReport struct {
	DroppedFailedTurns   int
	DroppedOrphanResults int
	SyntheticResults     int
	DowngradedThinking   int
	DroppedRedacted      int
	StrippedSignatures   int
	RewrittenIDs         int
	ImagesReplaced       int
}

func (r RepairReport) Changed() bool {
	return r.DroppedFailedTurns+r.DroppedOrphanResults+r.SyntheticResults+
		r.DowngradedThinking+r.DroppedRedacted+r.StrippedSignatures+
		r.RewrittenIDs+r.ImagesReplaced > 0
}

func (r RepairReport) String() string {
	var b strings.Builder
	add := func(n int, what string) {
		if n > 0 {
			if b.Len() > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%d %s", n, what)
		}
	}
	add(r.DroppedFailedTurns, "failed turns dropped")
	add(r.DroppedOrphanResults, "orphaned results dropped")
	add(r.SyntheticResults, "synthetic results inserted")
	add(r.DowngradedThinking, "thinking blocks downgraded")
	add(r.DroppedRedacted, "redacted thinking blocks dropped")
	add(r.StrippedSignatures, "signatures stripped")
	add(r.RewrittenIDs, "tool-call ids rewritten")
	add(r.ImagesReplaced, "images replaced")
	if b.Len() == 0 {
		return "no repairs"
	}
	return b.String()
}

// Target describes the model a transcript is being repaired FOR. same_model is
// the (provider, api, model) TRIPLE: two of three is not enough, and getting
// it wrong silently downgrades every signed thinking block to plain text on
// the first post-resume request (ruling P-4).
type Target struct {
	Provider string
	API      core.API
	Model    string
	// SupportsImages gates rule 7 (REQ-CAT-05).
	SupportsImages bool
	// NormalizeToolCallID rewrites an id into this API's accepted shape. Nil
	// means ids pass through unchanged.
	NormalizeToolCallID func(string) string
}

// TargetFor builds a Target from a resolved model.
func TargetFor(m *core.Model, normalize func(string) string) Target {
	return Target{
		Provider:            m.Provider,
		API:                 m.API,
		Model:               m.ID,
		SupportsImages:      m.SupportsImages(),
		NormalizeToolCallID: normalize,
	}
}

// RepairTranscript is REQ-PROV-11: the shared, unconditional pass every
// provider runs on every request, before wire-format conversion.
//
// It is part of the provider contract and not the loop's, because the loop is
// not running when a transcript is loaded from disk and no caller may be able
// to skip it. It produces a VIEW: the input is never mutated, and the result
// is never persisted, so the durable log stays complete while the request
// stays valid.
//
// The rules, in order. Order matters twice: rule 2b depends on rule 2 having
// run, and rule 6's synthetic results must carry the ids rule 5 rewrote.
//
//	1  compute same_model as the (provider, api, model) triple
//	2  drop assistant messages whose stop reason is Error or Aborted
//	2b drop tool results orphaned BY rule 2                       <-- see below
//	3  cross-model: downgrade signed thinking, drop redacted, strip signatures
//	4  demote thinking blocks with no signature to plain text
//	5  cross-model: rewrite tool-call ids, remembering the mapping
//	6  insert a synthetic result for any tool_use still unanswered
//	7  replace images when the target has no image modality
//
// # Rule 2b, which the PRD does not have
//
// Rule 2 drops an aborted assistant message — including its tool_use blocks.
// But the ToolResultMessages that answered those blocks are SEPARATE canonical
// messages and survive rule 2 untouched. Every provider rejects a tool result
// whose tool_use is absent, so the seven rules as written produce an INVALID
// request on the single commonest damaged transcript: Ctrl-C during a tool
// batch, then resume.
//
// Rule 2b closes that hole (ruling P-1). Without it the repair pass creates
// the exact failure it exists to prevent, and only on the resume path — which
// no single-process test exercises.
func RepairTranscript(in core.Messages, t Target) (core.Messages, RepairReport) {
	var rep RepairReport

	// ---- Rules 1-4: rebuild the assistant/user stream, dropping failed turns
	// and fixing up thinking blocks. Tool results are held back for 2b.
	kept := make(core.Messages, 0, len(in))
	liveToolUse := make(map[string]bool) // tool_use ids that survived
	for _, m := range in {
		switch v := m.(type) {
		case core.AssistantMessage:
			// Rule 2. Lossy by design: the partial text of an aborted turn
			// stays visible in the UI transcript and the session log, but is
			// invisible to the model on the next turn. A partial turn is not
			// one the model should be conditioned on.
			if v.StopReason.ShortCircuits() {
				rep.DroppedFailedTurns++
				continue
			}
			// Rule 1.
			same := v.Provider == t.Provider && v.API == t.API && v.Model == t.Model
			c := repairAssistantContent(v.Content, same, t, &rep)
			v.Content = c
			for _, b := range c {
				if tu, ok := b.(core.ToolUseBlock); ok {
					liveToolUse[tu.ID] = true
				}
			}
			kept = append(kept, v)
		case core.ToolResultMessage:
			kept = append(kept, v)
		default:
			kept = append(kept, m)
		}
	}

	// ---- Rule 2b: drop results orphaned by rule 2.
	out := make(core.Messages, 0, len(kept))
	for _, m := range kept {
		if tr, ok := m.(core.ToolResultMessage); ok {
			if !liveToolUse[tr.ToolUseID] {
				rep.DroppedOrphanResults++
				continue
			}
		}
		out = append(out, m)
	}

	// ---- Rule 5: rewrite tool-call ids for a cross-model replay, recording
	// the mapping so every matching result is rewritten identically.
	if t.NormalizeToolCallID != nil {
		out = rewriteToolCallIDs(out, t, &rep)
	}

	// ---- Rule 6: a synthetic result for every tool_use still unanswered.
	// This runs AFTER rule 5 so the synthetic carries the rewritten id; the
	// stated rule order gives that, but only by accident, so it is stated
	// explicitly here (ruling P-26).
	out = insertSyntheticResults(out, &rep)

	// ---- Rule 7.
	if !t.SupportsImages {
		out = replaceImages(out, &rep)
	}

	return out, rep
}

// repairAssistantContent applies rules 3 and 4 to one message's content.
func repairAssistantContent(c core.Content, sameModel bool, t Target, rep *RepairReport) core.Content {
	out := make(core.Content, 0, len(c))
	for _, b := range c {
		switch v := b.(type) {
		case core.ThinkingBlock:
			// Rule 3: a redacted thinking block is meaningless to another
			// vendor and is dropped outright.
			if !sameModel && v.Redacted {
				rep.DroppedRedacted++
				continue
			}
			// Rule 3 and rule 4 collapse to the same action for different
			// reasons: cross-model replay of an opaque signature is a hard
			// 400, and an unsigned block (the residue of an aborted stream)
			// is rejected too. Signatures are opaque — never parsed,
			// rewritten or synthesized.
			if !sameModel || v.Signature == "" {
				if v.Thinking == "" {
					// Nothing to preserve; drop rather than emit an empty
					// text block, which some providers also reject.
					rep.DowngradedThinking++
					continue
				}
				rep.DowngradedThinking++
				out = append(out, core.TextBlock{Text: v.Thinking})
				continue
			}
			out = append(out, v)
		case core.ToolUseBlock:
			if !sameModel && v.ThoughtSignature != "" {
				v.ThoughtSignature = ""
				rep.StrippedSignatures++
			}
			out = append(out, v)
		default:
			out = append(out, b)
		}
	}
	return out
}

// rewriteToolCallIDs applies rule 5 and carries the mapping to every matching
// result. Collisions get a deterministic suffix inside the target's budget:
// two distinct calls must not collapse onto one id, which would make the
// results ambiguous (ruling P-26).
func rewriteToolCallIDs(in core.Messages, t Target, rep *RepairReport) core.Messages {
	mapping := make(map[string]string)
	used := make(map[string]bool)

	assign := func(old string) string {
		if n, ok := mapping[old]; ok {
			return n
		}
		n := t.NormalizeToolCallID(old)
		if n != old {
			rep.RewrittenIDs++
		}
		base := n
		for i := 2; used[n]; i++ {
			n = withSuffix(base, i)
		}
		used[n] = true
		mapping[old] = n
		return n
	}

	out := make(core.Messages, 0, len(in))
	for _, m := range in {
		switch v := m.(type) {
		case core.AssistantMessage:
			c := make(core.Content, 0, len(v.Content))
			for _, b := range v.Content {
				if tu, ok := b.(core.ToolUseBlock); ok {
					tu.ID = assign(tu.ID)
					c = append(c, tu)
					continue
				}
				c = append(c, b)
			}
			v.Content = c
			out = append(out, v)
		case core.ToolResultMessage:
			if n, ok := mapping[v.ToolUseID]; ok {
				v.ToolUseID = n
			}
			out = append(out, v)
		default:
			out = append(out, m)
		}
	}
	return out
}

// withSuffix appends _<n> while keeping the result within 64 characters, the
// tightest budget any first-party API imposes.
func withSuffix(base string, n int) string {
	suf := fmt.Sprintf("_%d", n)
	if len(base)+len(suf) <= 64 {
		return base + suf
	}
	return base[:64-len(suf)] + suf
}

// insertSyntheticResults applies rule 6: at every assistant->non-result
// boundary and at the end of the list, any tool_use with no matching result
// gets one.
//
// An orphaned tool_use is what every cancellation, crashed tool and killed
// process leaves behind, and every provider rejects it with a 400.
func insertSyntheticResults(in core.Messages, rep *RepairReport) core.Messages {
	answered := make(map[string]bool)
	for _, m := range in {
		if tr, ok := m.(core.ToolResultMessage); ok {
			answered[tr.ToolUseID] = true
		}
	}

	out := make(core.Messages, 0, len(in))
	for i, m := range in {
		out = append(out, m)
		am, ok := m.(core.AssistantMessage)
		if !ok {
			continue
		}
		// Only synthesize at a boundary: the results for this turn, if any,
		// immediately follow it.
		j := i + 1
		for j < len(in) {
			if _, isResult := in[j].(core.ToolResultMessage); !isResult {
				break
			}
			j++
		}
		for _, b := range am.Content {
			tu, isToolUse := b.(core.ToolUseBlock)
			if !isToolUse || answered[tu.ID] {
				continue
			}
			answered[tu.ID] = true
			rep.SyntheticResults++
			out = append(out, core.ToolResultMessage{
				ToolUseID: tu.ID,
				ToolName:  tu.Name,
				Content:   core.Content{core.TextBlock{Text: SyntheticResultText}},
				IsError:   true,
			})
		}
	}
	return out
}

// ImagePlaceholder is REQ-CAT-05's replacement text.
const ImagePlaceholder = "(image omitted: model does not support images)"

func replaceImages(in core.Messages, rep *RepairReport) core.Messages {
	swap := func(c core.Content) core.Content {
		var changed bool
		out := make(core.Content, 0, len(c))
		for _, b := range c {
			switch v := b.(type) {
			case core.ImageBlock:
				changed = true
				rep.ImagesReplaced++
				out = append(out, core.TextBlock{Text: ImagePlaceholder})
			case core.ToolResultBlock:
				v.Content = swapContent(v.Content, rep, &changed)
				out = append(out, v)
			default:
				out = append(out, b)
			}
		}
		if !changed {
			return c
		}
		return out
	}

	out := make(core.Messages, 0, len(in))
	for _, m := range in {
		switch v := m.(type) {
		case core.UserMessage:
			v.Content = swap(v.Content)
			out = append(out, v)
		case core.AssistantMessage:
			v.Content = swap(v.Content)
			out = append(out, v)
		case core.ToolResultMessage:
			v.Content = swap(v.Content)
			out = append(out, v)
		default:
			out = append(out, m)
		}
	}
	return out
}

func swapContent(c core.Content, rep *RepairReport, changed *bool) core.Content {
	out := make(core.Content, 0, len(c))
	for _, b := range c {
		if _, ok := b.(core.ImageBlock); ok {
			*changed = true
			rep.ImagesReplaced++
			out = append(out, core.TextBlock{Text: ImagePlaceholder})
			continue
		}
		out = append(out, b)
	}
	return out
}
