package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// This file is the DECODE half of the Anthropic wire API.
//
// Streaming and whole-response decoding share one block assembler, and that
// sharing is REQ-PROV-17: "a streaming provider must produce byte-identical
// Input to what its non-streaming path produces for the same tool call". Two
// assemblers is two chances to disagree, and the disagreement is invisible
// until a replayed transcript quietly misses the prompt cache.

// ---------------------------------------------------------------- wire types

type wireUsage struct {
	// Every field is a pointer: an explicit 0 from the provider must beat any
	// SDK fallback (REQ-PROV-16.4), and a wholly unreported usage must stay
	// distinguishable from a reported all-zero one (REQ-GO-15).
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m *int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h *int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// Into folds an Anthropic usage onto the canonical one.
//
// REQ-PROV-05.1's subtraction is NOT applied here, and that is the correct
// reading rather than an omission: the trap it describes is an OpenAI-family
// convention, where prompt_tokens INCLUDES cached tokens. Anthropic reports
// input_tokens EXCLUSIVE of both cache_read_input_tokens and
// cache_creation_input_tokens. Subtracting here would understate input by the
// full cached amount — the same magnitude of error as the trap, in the other
// direction, and just as silent.
func (w wireUsage) Into(u *core.Usage) {
	cw := core.UsageWire{
		InputTokens:      w.InputTokens,
		OutputTokens:     w.OutputTokens,
		CacheReadTokens:  w.CacheReadInputTokens,
		CacheWriteTokens: w.CacheCreationInputTokens,
	}
	if w.CacheCreation != nil {
		cw.CacheWrite1hTokens = w.CacheCreation.Ephemeral1h
		if cw.CacheWriteTokens == nil && w.CacheCreation.Ephemeral5m != nil {
			// Some responses carry only the breakdown. Total writes is the sum
			// of the two buckets; 1h remains a SUBSET of it (REQ-PROV-05.3).
			total := *w.CacheCreation.Ephemeral5m
			if w.CacheCreation.Ephemeral1h != nil {
				total += *w.CacheCreation.Ephemeral1h
			}
			cw.CacheWriteTokens = &total
		}
	}
	cw.Into(u)
}

// wireBlock is one response content block. Unknown types are retained by
// rawBlockJSON rather than decoded through this struct.
type wireBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Data      string `json:"data"` // redacted_thinking

	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type wireResponse struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Role         string            `json:"role"`
	Model        string            `json:"model"`
	Content      []json.RawMessage `json:"content"`
	StopReason   string            `json:"stop_reason"`
	StopSequence *string           `json:"stop_sequence"`
	Usage        wireUsage         `json:"usage"`
}

type wireError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e wireError) String() string {
	t, m := e.Error.Type, e.Error.Message
	switch {
	case t != "" && m != "":
		return t + ": " + m
	case m != "":
		return m
	case t != "":
		return t
	}
	return ""
}

// ---------------------------------------------------------------- assembler

// blockAcc accumulates one content block across however many events carry it.
type blockAcc struct {
	typ string

	text      strings.Builder
	thinking  strings.Builder
	signature string
	data      string

	id, name string
	// input is the concatenation of the streamed partial_json fragments,
	// VERBATIM. It is never decoded and re-encoded on the way to
	// core.NewToolUse (REQ-PROV-17).
	input []byte
	// raw retains a block type this build does not model, so it can be
	// replayed unchanged. This is what makes REQ-PROV-07's server-side
	// compaction blocks work without modelling them.
	raw json.RawMessage

	// salvaged records that the argument bytes had to be repaired, so the
	// caller can distinguish a truncated call from a complete one.
	salvaged bool
}

// startFrom seeds an accumulator from a content_block_start payload, or from a
// whole-response block.
//
// seedInput is the ONE place the two paths legitimately differ, and getting it
// wrong is silent. On the streaming wire `content_block_start` always carries
// `"input":{}` as a placeholder and the real bytes arrive as input_json_delta,
// so seeding from it yields `{}{"real":"args"}` — invalid JSON that salvage
// then reduces to `{}`, a tool call with no arguments that still validates
// against a schema with no required properties. On the whole-response path the
// same field IS the arguments.
func startFrom(raw json.RawMessage, seedInput bool) *blockAcc {
	var wb wireBlock
	_ = json.Unmarshal(raw, &wb)
	acc := &blockAcc{typ: wb.Type, id: wb.ID, name: wb.Name,
		signature: wb.Signature, data: wb.Data}
	switch wb.Type {
	case "text":
		acc.text.WriteString(wb.Text)
	case "thinking":
		acc.thinking.WriteString(wb.Thinking)
	case "tool_use":
		if seedInput {
			acc.input = append(acc.input, wb.Input...)
		}
	case "text_delta", "": // defensive; not a real start type
	default:
		if !knownBlockType(wb.Type) {
			acc.raw = append(json.RawMessage(nil), raw...)
		}
	}
	return acc
}

func knownBlockType(t string) bool {
	switch t {
	case "text", "thinking", "redacted_thinking", "tool_use":
		return true
	}
	return false
}

// block finalizes the accumulator into a canonical content block.
func (a *blockAcc) block() core.ContentBlock {
	switch a.typ {
	case "text":
		return core.TextBlock{Text: a.text.String()}
	case "thinking":
		return core.ThinkingBlock{Thinking: a.thinking.String(), Signature: a.signature}
	case "redacted_thinking":
		// The opaque payload rides in Signature, which is where encodeBlock
		// reads it from on the way back out. Round-tripping matters: a
		// redacted block that loses its data is a 400 on the next turn.
		return core.ThinkingBlock{Redacted: true, Signature: a.data}
	case "tool_use":
		raw := json.RawMessage(a.input)
		if repaired, changed := provider.SalvageJSON(raw); changed {
			raw = repaired
			a.salvaged = true
		}
		b, err := core.NewToolUse(a.id, a.name, raw)
		if err != nil {
			// SalvageJSON guarantees valid JSON, so this is reachable only for
			// a valid non-object (`[1,2]` as arguments). An empty object is
			// the representable form; the schema validator rejects it next.
			b, _ = core.NewToolUse(a.id, a.name, nil)
			a.salvaged = true
		}
		return b
	}
	if len(a.raw) > 0 {
		return core.RawBlock{Type: a.typ, Raw: a.raw}
	}
	return nil
}

// endEvent produces the block's AUTHORITATIVE event.
//
// It is a method rather than inline emission because REQ-OBS-08.3 requires
// every block-end to be emitted after the stream has fully ended, in block
// order, once each — so these are buffered and replayed, never pushed from the
// per-chunk handler.
func (a *blockAcc) endEvent(index int, b core.ContentBlock) core.Event {
	switch v := b.(type) {
	case core.TextBlock:
		// The end event carries the WHOLE text, not a final delta: a consumer
		// obeying REQ-OBS-06a discards its accumulator on the authoritative
		// event, which is impossible if that event carries only a fragment.
		return core.TextEndEvent{BlockIndex: index, Text: v.Text}
	case core.ThinkingBlock:
		return core.ThinkingEndEvent{BlockIndex: index, Thinking: v.Thinking,
			Signature: v.Signature, Redacted: v.Redacted}
	case core.ToolUseBlock:
		return core.ToolCallEndEvent{BlockIndex: index, Block: v}
	}
	return nil
}

// startEvent produces the block's opening event, or nil for a block type that
// has none.
func (a *blockAcc) startEvent(index int) core.Event {
	switch a.typ {
	case "text":
		return core.TextStartEvent{BlockIndex: index}
	case "thinking", "redacted_thinking":
		return core.ThinkingStartEvent{BlockIndex: index}
	case "tool_use":
		return core.ToolCallStartEvent{BlockIndex: index, ToolUseID: a.id, Name: a.name}
	}
	return nil
}
