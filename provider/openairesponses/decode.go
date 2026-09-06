package openairesponses

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/wire"
)

// Streaming event names. The Responses wire is EVENT-TYPED — the `event:`
// field selects the shape — where Chat Completions sends one chunk shape and
// leaves the consumer to work out which field moved.
const (
	evOutputItemAdded   = "response.output_item.added"
	evOutputItemDone    = "response.output_item.done"
	evOutputTextDelta   = "response.output_text.delta"
	evFunctionArgsDelta = "response.function_call_arguments.delta"
	evReasoningDelta    = "response.reasoning_summary_text.delta"
	evCompleted         = "response.completed"
	evIncomplete        = "response.incomplete"
	evFailed            = "response.failed"
	evError             = "error"
)

// ErrTruncated is the Responses analogue of a stream that stopped before its
// terminal event.
var ErrTruncated = errors.New("openai-responses: stream ended before response.completed")

type streamEvent struct {
	Type        string          `json:"type"`
	OutputIndex int             `json:"output_index"`
	ItemID      string          `json:"item_id"`
	Delta       string          `json:"delta"`
	Item        *wireItem       `json:"item"`
	Response    *wireResponse   `json:"response"`
	Error       *wireError      `json:"error"`
	Raw         json.RawMessage `json:"-"`
}

type wireError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type"`
}

type wireItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	CallID           string        `json:"call_id"`
	Name             string        `json:"name"`
	Arguments        string        `json:"arguments"`
	EncryptedContent string        `json:"encrypted_content"`
	Summary          []summaryPart `json:"summary"`
	Content          []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type wireResponse struct {
	ID          string     `json:"id"`
	Model       string     `json:"model"`
	Status      string     `json:"status"`
	ServiceTier string     `json:"service_tier"`
	Usage       *wireUsage `json:"usage"`
	Incomplete  *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

// wireUsage is this wire's usage shape. The field NAMES differ from Chat
// Completions — input_tokens rather than prompt_tokens — and reusing that
// decoder yields a silent zero, which REQ-GO-15 then treats as an unknown
// context size.
type wireUsage struct {
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	TotalTokens        *int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// Into folds onto the canonical usage, applying REQ-PROV-05.1's subtraction.
//
// input_tokens INCLUDES the cached portion on this wire, exactly as
// prompt_tokens does on Chat Completions. Assigning it straight across
// double-counts every cached token and overstates cost on precisely the
// well-cached agent loop the SDK exists for.
func (w wireUsage) Into(u *core.Usage) {
	var cacheRead *int64
	if w.InputTokensDetails != nil {
		cacheRead = w.InputTokensDetails.CachedTokens
	}
	var reasoning *int64
	if w.OutputTokensDetails != nil {
		reasoning = w.OutputTokensDetails.ReasoningTokens
	}
	cw := core.UsageWire{
		OutputTokens:    w.OutputTokens,
		TotalTokens:     w.TotalTokens,
		CacheReadTokens: cacheRead,
		ReasoningTokens: reasoning,
	}
	if w.InputTokens != nil {
		net := *w.InputTokens
		if cacheRead != nil {
			net -= *cacheRead
		}
		if net < 0 {
			net = 0
		}
		cw.InputTokens = &net
	}
	cw.Into(u)
}

// ---------------------------------------------------------------- decoder

type decoder struct {
	s       *core.EventStream
	partial core.AssistantMessage
	usage   core.Usage

	respID  string
	respMod string
	status  string
	// tier is the service tier the RESPONSE reports, which may differ from the
	// one requested — a flex request can be served at standard.
	tier string

	// blocks are accumulated per output_index. The Responses wire addresses
	// items by index and delivers them interleaved, so a slice keyed on
	// arrival order would put a late text delta on the wrong block.
	blocks map[int]*blockState
	order  []int
	closed bool
}

type blockState struct {
	kind      string // "text" | "tool" | "reasoning"
	index     int    // canonical block index, assigned in arrival order
	text      []byte
	args      []byte
	callID    string
	itemID    string
	name      string
	encrypted string
	started   bool
}

func (d *decoder) block(outIdx int, kind string) *blockState {
	if d.blocks == nil {
		d.blocks = map[int]*blockState{}
	}
	b, ok := d.blocks[outIdx]
	if !ok {
		b = &blockState{kind: kind, index: len(d.order)}
		d.blocks[outIdx] = b
		d.order = append(d.order, outIdx)
	}
	return b
}

func (d *decoder) consume(r *provider.SSEReader) error {
	for {
		ev, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !d.closed {
					// A stream that stopped before response.completed. Saying
					// so lets the retry layer treat it as retryable, where a
					// silent success would hand the loop a truncated turn.
					return ErrTruncated
				}
				return nil
			}
			return err
		}
		if err := d.event(ev); err != nil {
			return err
		}
	}
}

func (d *decoder) event(ev provider.SSEEvent) error {
	if len(ev.Data) == 0 {
		return nil
	}
	// REQ-SEC-11 bounds and duplicate-key rejection on an untrusted response
	// (PRD 0.3.4 keeps provider responses on the bounds, not the
	// unknown-property rejection).
	if err := wire.Guard(ev.Data, wire.Limits{}); err != nil {
		return fmt.Errorf("openai-responses: %w", err)
	}
	var e streamEvent
	if err := json.Unmarshal(ev.Data, &e); err != nil {
		return fmt.Errorf("openai-responses: decoding %s: %w", ev.Type, err)
	}
	if e.Type == "" {
		e.Type = ev.Type
	}

	switch e.Type {
	case evOutputItemAdded:
		d.itemAdded(e)

	case evOutputTextDelta:
		b := d.block(e.OutputIndex, "text")
		d.startOnce(b)
		b.text = append(b.text, e.Delta...)
		d.s.Push(core.TextDeltaEvent{BlockIndex: b.index, Delta: e.Delta})

	case evReasoningDelta:
		b := d.block(e.OutputIndex, "reasoning")
		d.startOnce(b)
		b.text = append(b.text, e.Delta...)
		d.s.Push(core.ThinkingDeltaEvent{BlockIndex: b.index, Delta: e.Delta})

	case evFunctionArgsDelta:
		b := d.block(e.OutputIndex, "tool")
		d.startOnce(b)
		b.args = append(b.args, e.Delta...)
		d.s.Push(core.ToolInputDeltaEvent{BlockIndex: b.index,
			ToolUseID: JoinID(b.callID, b.itemID), Delta: e.Delta})

	case evOutputItemDone:
		d.itemDone(e)

	case evCompleted, evIncomplete:
		d.closed = true
		d.response(e)

	case evFailed:
		d.closed = true
		d.response(e)
		if e.Response != nil && e.Response.Status == "failed" {
			return errors.New("openai-responses: response failed")
		}

	case evError:
		if e.Error != nil {
			return fmt.Errorf("openai-responses: %s: %s", e.Error.Code, e.Error.Message)
		}
		return errors.New("openai-responses: stream reported an error")
	}
	return nil
}

func (d *decoder) startOnce(b *blockState) {
	if b.started {
		return
	}
	b.started = true
	switch b.kind {
	case "text":
		d.s.Push(core.TextStartEvent{BlockIndex: b.index})
	case "reasoning":
		d.s.Push(core.ThinkingStartEvent{BlockIndex: b.index})
	case "tool":
		d.s.Push(core.ToolCallStartEvent{BlockIndex: b.index,
			ToolUseID: JoinID(b.callID, b.itemID), Name: b.name})
	}
}

func (d *decoder) itemAdded(e streamEvent) {
	if e.Item == nil {
		return
	}
	switch e.Item.Type {
	case "message":
		b := d.block(e.OutputIndex, "text")
		// Text that arrived complete rather than as deltas.
		for _, c := range e.Item.Content {
			if c.Text != "" {
				b.text = append(b.text, c.Text...)
			}
		}
	case "function_call":
		b := d.block(e.OutputIndex, "tool")
		b.callID, b.itemID, b.name = e.Item.CallID, e.Item.ID, e.Item.Name
		// Deliberately NOT seeding b.args from Item.Arguments. On this wire
		// output_item.added carries an empty or placeholder arguments string
		// and the real value arrives as deltas; seeding it produces
		// `""{"real":…}` — invalid JSON that then has to be salvaged, and the
		// salvage keeps the placeholder. The Anthropic decoder had exactly
		// this bug against `"input":{}`.
		d.startOnce(b)
	case "reasoning":
		b := d.block(e.OutputIndex, "reasoning")
		b.itemID, b.encrypted = e.Item.ID, e.Item.EncryptedContent
	}
}

func (d *decoder) itemDone(e streamEvent) {
	if e.Item == nil {
		return
	}
	b, ok := d.blocks[e.OutputIndex]
	if !ok {
		// An item delivered whole, with no deltas at all.
		switch e.Item.Type {
		case "message":
			b = d.block(e.OutputIndex, "text")
		case "function_call":
			b = d.block(e.OutputIndex, "tool")
		case "reasoning":
			b = d.block(e.OutputIndex, "reasoning")
		default:
			return
		}
	}
	switch e.Item.Type {
	case "message":
		if len(b.text) == 0 {
			for _, c := range e.Item.Content {
				b.text = append(b.text, c.Text...)
			}
		}
	case "function_call":
		b.callID, b.itemID, b.name = e.Item.CallID, e.Item.ID, e.Item.Name
		if len(b.args) == 0 && e.Item.Arguments != "" {
			b.args = []byte(e.Item.Arguments)
		}
	case "reasoning":
		b.itemID = e.Item.ID
		if e.Item.EncryptedContent != "" {
			b.encrypted = e.Item.EncryptedContent
		}
		if len(b.text) == 0 {
			for _, sp := range e.Item.Summary {
				b.text = append(b.text, sp.Text...)
			}
		}
	}
	d.startOnce(b)
}

func (d *decoder) response(e streamEvent) {
	if e.Response == nil {
		return
	}
	d.respID, d.respMod = e.Response.ID, e.Response.Model
	d.status = e.Response.Status
	if e.Response.ServiceTier != "" {
		d.tier = e.Response.ServiceTier
	}
	if e.Response.Incomplete != nil && e.Response.Incomplete.Reason != "" {
		d.status = e.Response.Incomplete.Reason
	}
	if e.Response.Usage != nil {
		e.Response.Usage.Into(&d.usage)
	}
}

// snapshot builds the content blocks in canonical order.
func (d *decoder) snapshot() core.Content {
	idx := append([]int(nil), d.order...)
	sort.SliceStable(idx, func(i, j int) bool {
		return d.blocks[idx[i]].index < d.blocks[idx[j]].index
	})

	out := make(core.Content, 0, len(idx))
	for _, i := range idx {
		b := d.blocks[i]
		switch b.kind {
		case "text":
			if len(b.text) > 0 {
				out = append(out, core.TextBlock{Text: string(b.text)})
			}
		case "reasoning":
			sig := EncodeThinkingSignature(b.itemID, b.encrypted)
			if len(b.text) == 0 && sig == "" {
				continue
			}
			out = append(out, core.ThinkingBlock{Thinking: string(b.text), Signature: sig})
		case "tool":
			args, _ := provider.SalvageJSON(b.args)
			blk, err := core.NewToolUse(JoinID(b.callID, b.itemID), b.name, args)
			if err != nil {
				continue
			}
			out = append(out, blk)
		}
	}
	return out
}

func (d *decoder) finish(m *core.Model, lookup func(string) *core.Model, tier provider.ServiceTier) {
	content := d.snapshot()

	for i, b := range content {
		switch v := b.(type) {
		case core.TextBlock:
			d.s.Push(core.TextEndEvent{BlockIndex: i, Text: v.Text})
		case core.ThinkingBlock:
			d.s.Push(core.ThinkingEndEvent{BlockIndex: i, Thinking: v.Thinking,
				Signature: v.Signature, Redacted: v.Redacted})
		case core.ToolUseBlock:
			d.s.Push(core.ToolCallEndEvent{BlockIndex: i, Block: v})
		}
	}

	final := d.partial
	final.Content = content
	final.ResponseID = d.respID
	final.ResponseModel = d.respMod
	hasTools := len(core.ExtractToolUse(&final)) > 0
	final.StopReason = MapStatus(d.status, hasTools)
	final.RawStopReason = d.status
	final.Usage = d.usage

	billModel, billed := provider.BillingModel(m, d.respMod, lookup)
	final.Usage.BilledModel = billed
	if final.Usage.Reported() {
		// §4: the Responses billing model applies a service-tier multiplier
		// POST HOC — the tier scales the computed cost rather than selecting a
		// different rate table, so it composes with REQ-PROV-05.4's tiering
		// instead of replacing it.
		final.Usage.SetCost(provider.ApplyServiceTier(
			provider.ComputeCost(billModel, final.Usage), tier))
	}

	d.s.Push(core.MessageEndEvent{Message: final})
	d.s.End(core.StreamResult{Message: &final})
}

func (d *decoder) fail(text string, err error) {
	final := d.partial
	final.Content = d.snapshot()
	final.Usage = d.usage
	if text == provider.AbortText {
		final.StopReason = core.StopReasonAborted
		final.ErrorMessage = text
		d.s.Push(core.MessageEndEvent{Message: final})
		d.s.End(core.StreamResult{Message: &final, Err: core.ErrAborted})
		return
	}
	final.StopReason = core.StopReasonError
	final.ErrorMessage = text
	d.s.Push(core.MessageEndEvent{Message: final})
	d.s.End(core.StreamResult{Message: &final, Err: err})
}

// MapStatus normalizes this wire's terminal status onto the canonical set.
//
// hasTools decides the tool-use case, exactly as on the other wires: a
// completed response carrying function_call items is a tool turn, and reading
// the status alone reports "stop" for a turn the loop must continue.
func MapStatus(status string, hasTools bool) core.StopReason {
	switch status {
	case "max_output_tokens":
		return core.StopReasonLength
	case "content_filter":
		return core.StopReasonRefusal
	case "failed":
		return core.StopReasonError
	}
	if hasTools {
		return core.StopReasonToolUse
	}
	return core.StopReasonStop
}
