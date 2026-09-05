package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

const targetAPI core.API = "target-api"

func target() Target {
	return Target{Provider: "acme", API: targetAPI, Model: "m1", SupportsImages: true}
}

func tu(t *testing.T, id, name string) core.ToolUseBlock {
	t.Helper()
	b, err := core.NewToolUse(id, name, json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sameModelAssistant(blocks ...core.ContentBlock) core.AssistantMessage {
	return core.AssistantMessage{
		Content: core.Content(blocks), StopReason: core.StopReasonToolUse,
		Provider: "acme", API: targetAPI, Model: "m1",
	}
}

func roles(ms core.Messages) string {
	var out []string
	for _, m := range ms {
		out = append(out, string(m.Role()))
	}
	return strings.Join(out, ",")
}

// TestRepairRule2bDropsResultOrphanedByRule2 is the test for ruling P-1, the
// hole in REQ-PROV-11 as written.
//
// Rule 2 drops the aborted assistant message — and with it the tool_use block.
// The ToolResultMessage that answered it is a SEPARATE canonical message and
// survives rule 2 untouched. Every provider rejects a tool result whose
// tool_use is absent, so without rule 2b the repair pass emits an invalid
// request on the commonest damaged transcript there is: Ctrl-C during a tool
// batch, then resume.
func TestRepairRule2bDropsResultOrphanedByRule2(t *testing.T) {
	in := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}},
		// An aborted turn that nonetheless got one tool result written before
		// the process died.
		core.AssistantMessage{
			Content:    core.Content{tu(t, "call_1", "read")},
			StopReason: core.StopReasonAborted,
			Provider:   "acme", API: targetAPI, Model: "m1",
		},
		core.ToolResultMessage{ToolUseID: "call_1", ToolName: "read",
			Content: core.Content{core.TextBlock{Text: "file contents"}}},
	}

	out, rep := RepairTranscript(in, target())

	if rep.DroppedFailedTurns != 1 {
		t.Fatalf("DroppedFailedTurns = %d, want 1", rep.DroppedFailedTurns)
	}
	if rep.DroppedOrphanResults != 1 {
		t.Fatalf("DroppedOrphanResults = %d, want 1.\n"+
			"Rule 2 dropped the assistant turn carrying tool_use call_1, but its result "+
			"survived. Rule 2b (ruling P-1) must drop it, or the request 400s.", rep.DroppedOrphanResults)
	}
	if got := roles(out); got != "user" {
		t.Fatalf("roles = %q, want %q — the orphaned result is still in the view", got, "user")
	}
}

// TestRepairInsertsSyntheticResultForUnansweredToolUse: rule 6. An orphaned
// tool_use is what every cancellation, crashed tool and killed process leaves
// behind.
func TestRepairInsertsSyntheticResultForUnansweredToolUse(t *testing.T) {
	in := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}},
		sameModelAssistant(tu(t, "call_1", "read"), tu(t, "call_2", "write")),
		core.ToolResultMessage{ToolUseID: "call_1", ToolName: "read"},
		// call_2 never got a result: the process died mid-batch.
	}
	out, rep := RepairTranscript(in, target())
	if rep.SyntheticResults != 1 {
		t.Fatalf("SyntheticResults = %d, want 1", rep.SyntheticResults)
	}
	var found bool
	for _, m := range out {
		if tr, ok := m.(core.ToolResultMessage); ok && tr.ToolUseID == "call_2" {
			found = true
			if !tr.IsError {
				t.Error("a synthetic result must be an error result")
			}
			if tr.Content.Text() != SyntheticResultText {
				t.Errorf("synthetic text = %q, want %q", tr.Content.Text(), SyntheticResultText)
			}
		}
	}
	if !found {
		t.Fatal("no synthetic result was inserted for the unanswered tool_use")
	}
}

// TestRepairNeverMutatesInput: the pass produces a VIEW. Mutating history
// would corrupt the durable record for the sake of one request.
func TestRepairNeverMutatesInput(t *testing.T) {
	orig := sameModelAssistant(tu(t, "call_1", "read"))
	orig.Content = append(orig.Content, core.ThinkingBlock{Thinking: "hmm"}) // unsigned
	in := core.Messages{orig}

	before := len(orig.Content)
	beforeKind := orig.Content[1].BlockType()

	_, rep := RepairTranscript(in, target())
	if rep.DowngradedThinking != 1 {
		t.Fatalf("expected the unsigned thinking block to be downgraded, got %+v", rep)
	}
	am := in[0].(core.AssistantMessage)
	if len(am.Content) != before || am.Content[1].BlockType() != beforeKind {
		t.Fatal("RepairTranscript mutated its input; it must produce a view")
	}
}

// TestUnsignedThinkingIsDemotedEvenForSameModel: rule 4 is not conditional on
// cross-model replay. An unsigned block is the residue of an aborted stream
// and is rejected regardless of who produced it.
func TestUnsignedThinkingIsDemotedEvenForSameModel(t *testing.T) {
	in := core.Messages{sameModelAssistant(core.ThinkingBlock{Thinking: "partial"})}
	out, rep := RepairTranscript(in, target())
	if rep.DowngradedThinking != 1 {
		t.Fatalf("DowngradedThinking = %d, want 1", rep.DowngradedThinking)
	}
	am := out[0].(core.AssistantMessage)
	if am.Content[0].BlockType() != core.BlockText {
		t.Fatalf("block type = %q, want text", am.Content[0].BlockType())
	}
}

// TestCrossModelReplayStripsOpaqueMaterial: rule 3. Replaying another vendor's
// opaque signature is a hard 400, and this is what makes mid-session model
// switching and per-agent model selection safe.
func TestCrossModelReplayStripsOpaqueMaterial(t *testing.T) {
	other := core.AssistantMessage{
		Content: core.Content{
			core.ThinkingBlock{Thinking: "reasoned", Signature: "sig-from-another-vendor"},
			core.ThinkingBlock{Thinking: "hidden", Redacted: true, Signature: "s"},
			func() core.ToolUseBlock { b := tu(t, "call_1", "read"); b.ThoughtSignature = "ts"; return b }(),
		},
		StopReason: core.StopReasonToolUse,
		Provider:   "other", API: "other-api", Model: "other-model",
	}
	in := core.Messages{other, core.ToolResultMessage{ToolUseID: "call_1", ToolName: "read"}}

	out, rep := RepairTranscript(in, target())
	if rep.DowngradedThinking != 1 {
		t.Errorf("DowngradedThinking = %d, want 1 (the signed block becomes text)", rep.DowngradedThinking)
	}
	if rep.DroppedRedacted != 1 {
		t.Errorf("DroppedRedacted = %d, want 1", rep.DroppedRedacted)
	}
	if rep.StrippedSignatures != 1 {
		t.Errorf("StrippedSignatures = %d, want 1", rep.StrippedSignatures)
	}

	am := out[0].(core.AssistantMessage)
	for _, b := range am.Content {
		if tb, ok := b.(core.ThinkingBlock); ok {
			t.Fatalf("a thinking block survived cross-model replay: %+v", tb)
		}
		if ub, ok := b.(core.ToolUseBlock); ok && ub.ThoughtSignature != "" {
			t.Fatal("thought_signature survived cross-model replay")
		}
	}
}

// TestSameModelReplayKeepsSignedThinking is the negative control: without it,
// an implementation that simply strips everything always would pass the test
// above and silently discard reasoning it was entitled to replay.
func TestSameModelReplayKeepsSignedThinking(t *testing.T) {
	in := core.Messages{sameModelAssistant(
		core.ThinkingBlock{Thinking: "reasoned", Signature: "sig"},
	)}
	out, rep := RepairTranscript(in, target())
	if rep.DowngradedThinking != 0 {
		t.Fatalf("a signed same-model thinking block must be replayed verbatim, got %+v", rep)
	}
	am := out[0].(core.AssistantMessage)
	tb, ok := am.Content[0].(core.ThinkingBlock)
	if !ok || tb.Signature != "sig" {
		t.Fatalf("signed thinking block was not preserved: %+v", am.Content[0])
	}
}

// TestToolCallIDRewriteAppliesToResultsToo: rule 5's mapping must reach the
// matching results, or the rewrite orphans every one of them.
func TestToolCallIDRewriteAppliesToResultsToo(t *testing.T) {
	tgt := target()
	tgt.Provider, tgt.Model = "other", "other" // force cross-model
	tgt.NormalizeToolCallID = func(s string) string {
		return strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
				return r
			}
			return '_'
		}, s)
	}
	in := core.Messages{
		sameModelAssistant(tu(t, "call:1", "read")),
		core.ToolResultMessage{ToolUseID: "call:1", ToolName: "read"},
	}
	out, rep := RepairTranscript(in, tgt)
	if rep.RewrittenIDs != 1 {
		t.Fatalf("RewrittenIDs = %d, want 1", rep.RewrittenIDs)
	}
	am := out[0].(core.AssistantMessage)
	newID := am.Content[0].(core.ToolUseBlock).ID
	if newID != "call_1" {
		t.Fatalf("rewritten id = %q, want %q", newID, "call_1")
	}
	tr := out[1].(core.ToolResultMessage)
	if tr.ToolUseID != newID {
		t.Fatalf("result id = %q, want %q: the mapping did not reach the result, "+
			"which orphans it", tr.ToolUseID, newID)
	}
	if rep.SyntheticResults != 0 {
		t.Fatal("rule 6 invented a result for a call that already had one: rule 5 must " +
			"run before rule 6 (ruling P-26)")
	}
}

// TestIDRewriteCollisionsGetDistinctIDs: two distinct calls must not collapse
// onto one id, which would make their results ambiguous.
func TestIDRewriteCollisionsGetDistinctIDs(t *testing.T) {
	tgt := target()
	tgt.Provider = "other"
	tgt.NormalizeToolCallID = func(string) string { return "same" } // pathological
	in := core.Messages{sameModelAssistant(tu(t, "a", "read"), tu(t, "b", "read"))}
	out, _ := RepairTranscript(in, tgt)
	am := out[0].(core.AssistantMessage)
	id0 := am.Content[0].(core.ToolUseBlock).ID
	id1 := am.Content[1].(core.ToolUseBlock).ID
	if id0 == id1 {
		t.Fatalf("two calls collapsed onto id %q; their results would be ambiguous", id0)
	}
}

// TestImagesReplacedWhenTargetLacksModality: rule 7 / REQ-CAT-05. Sending an
// image to a text-only model is a 400.
func TestImagesReplacedWhenTargetLacksModality(t *testing.T) {
	tgt := target()
	tgt.SupportsImages = false
	in := core.Messages{
		core.UserMessage{Content: core.Content{
			core.TextBlock{Text: "look"},
			core.ImageBlock{Data: "AAAA", MimeType: "image/png"},
		}},
	}
	out, rep := RepairTranscript(in, tgt)
	if rep.ImagesReplaced != 1 {
		t.Fatalf("ImagesReplaced = %d, want 1", rep.ImagesReplaced)
	}
	um := out[0].(core.UserMessage)
	if um.Content[1].BlockType() != core.BlockText {
		t.Fatal("the image block was not replaced")
	}
	if got := um.Content[1].(core.TextBlock).Text; got != ImagePlaceholder {
		t.Fatalf("placeholder = %q, want %q", got, ImagePlaceholder)
	}
}

// TestRepairIsIdempotent: running the pass on its own output must change
// nothing further. A pass that keeps finding work has a rule that fights
// another one.
func TestRepairIsIdempotent(t *testing.T) {
	in := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}},
		core.AssistantMessage{
			Content: core.Content{tu(t, "call_1", "read"), core.ThinkingBlock{Thinking: "x"}},
			StopReason: core.StopReasonToolUse, Provider: "acme", API: targetAPI, Model: "m1",
		},
	}
	once, rep1 := RepairTranscript(in, target())
	if !rep1.Changed() {
		t.Fatal("expected the first pass to change something")
	}
	twice, rep2 := RepairTranscript(once, target())
	if rep2.Changed() {
		t.Fatalf("second pass still made changes (%s): the rules are not idempotent", rep2)
	}
	if roles(once) != roles(twice) {
		t.Fatalf("roles differ between passes: %q vs %q", roles(once), roles(twice))
	}
}

// TestEveryToolUseIsAnsweredAfterRepair is the property that actually matters,
// asserted over a set of damaged transcripts. Whatever the damage, the output
// must be sendable: every tool_use answered, and no result left orphaned.
func TestEveryToolUseIsAnsweredAfterRepair(t *testing.T) {
	cases := map[string]core.Messages{
		"aborted mid-batch": {
			sameModelAssistant(tu(t, "a", "x"), tu(t, "b", "y")),
			core.ToolResultMessage{ToolUseID: "a", ToolName: "x"},
		},
		"aborted turn with a surviving result": {
			core.AssistantMessage{Content: core.Content{tu(t, "a", "x")},
				StopReason: core.StopReasonAborted, Provider: "acme", API: targetAPI, Model: "m1"},
			core.ToolResultMessage{ToolUseID: "a", ToolName: "x"},
		},
		"error turn followed by a fresh user message": {
			core.AssistantMessage{Content: core.Content{tu(t, "a", "x")},
				StopReason: core.StopReasonError, Provider: "acme", API: targetAPI, Model: "m1"},
			core.UserMessage{Content: core.Content{core.TextBlock{Text: "try again"}}},
		},
		"result with no call at all": {
			core.ToolResultMessage{ToolUseID: "ghost", ToolName: "x"},
		},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, _ := RepairTranscript(in, target())
			live := map[string]bool{}
			for _, m := range out {
				if am, ok := m.(core.AssistantMessage); ok {
					for _, b := range am.Content {
						if ub, ok := b.(core.ToolUseBlock); ok {
							live[ub.ID] = true
						}
					}
				}
			}
			answered := map[string]bool{}
			for _, m := range out {
				if tr, ok := m.(core.ToolResultMessage); ok {
					if !live[tr.ToolUseID] {
						t.Errorf("orphaned result %q survived repair", tr.ToolUseID)
					}
					answered[tr.ToolUseID] = true
				}
			}
			for id := range live {
				if !answered[id] {
					t.Errorf("tool_use %q left unanswered after repair", id)
				}
			}
		})
	}
}

// FuzzRepairAlwaysSendable asserts the property over arbitrary structures: the
// pass never panics, and its output always satisfies the sendability
// invariant.
func FuzzRepairAlwaysSendable(f *testing.F) {
	f.Add(uint16(0x0000))
	f.Add(uint16(0xFFFF))
	f.Add(uint16(0xA5A5))
	f.Fuzz(func(t *testing.T, shape uint16) {
		// Build a transcript from the bits, so the fuzzer explores orderings
		// rather than JSON syntax.
		var in core.Messages
		for i := 0; i < 8; i++ {
			switch (shape >> (i * 2)) & 0x3 {
			case 0:
				in = append(in, core.UserMessage{Content: core.Content{core.TextBlock{Text: "u"}}})
			case 1:
				b, _ := core.NewToolUse("id"+string(rune('a'+i)), "tool", json.RawMessage(`{}`))
				in = append(in, core.AssistantMessage{Content: core.Content{b},
					StopReason: core.StopReasonToolUse, Provider: "acme", API: targetAPI, Model: "m1"})
			case 2:
				in = append(in, core.ToolResultMessage{ToolUseID: "id" + string(rune('a'+i)), ToolName: "tool"})
			case 3:
				in = append(in, core.AssistantMessage{Content: core.Content{core.TextBlock{Text: "partial"}},
					StopReason: core.StopReasonAborted, Provider: "acme", API: targetAPI, Model: "m1"})
			}
		}
		out, _ := RepairTranscript(in, target())

		live := map[string]bool{}
		for _, m := range out {
			if am, ok := m.(core.AssistantMessage); ok {
				for _, b := range am.Content {
					if ub, ok := b.(core.ToolUseBlock); ok {
						live[ub.ID] = true
					}
				}
			}
		}
		answered := map[string]bool{}
		for _, m := range out {
			if tr, ok := m.(core.ToolResultMessage); ok {
				if !live[tr.ToolUseID] {
					t.Fatalf("orphaned result %q survived repair of %v", tr.ToolUseID, roles(in))
				}
				answered[tr.ToolUseID] = true
			}
		}
		for id := range live {
			if !answered[id] {
				t.Fatalf("tool_use %q unanswered after repair of %v", id, roles(in))
			}
		}
	})
}
