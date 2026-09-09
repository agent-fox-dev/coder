// Package anthropic implements the Anthropic Messages wire API.
//
// It is keyed by WIRE API, not by vendor (REQ-PROV-02): the same
// implementation serves Anthropic direct and any gateway that speaks Messages.
package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/agentfox/agentkit-go/catalog"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// API is the wire API id.
const API core.API = "anthropic-messages"

// DefaultMaxTokens is sent when neither the request nor the model bounds the
// output. max_tokens is REQUIRED on this wire and 0 is rejected, so a
// hand-built Model with no cap and no window still needs a positive number;
// this is the floor REQ-CAT-04 leaves for an unknown window, sized so a
// normal answer fits rather than the literal 1 the formula's max(1, ·) yields.
const DefaultMaxTokens = 4096

// ---------------------------------------------------------------- wire types
//
// Every struct below uses `omitzero` with pointers for optional scalars, never
// `omitempty` on a bare value (REQ-PROV-16). The difference is not cosmetic:
// `omitempty` on a bare float64 DROPS an explicit temperature of 0, turning
// "be deterministic" into "use the provider default".

type request struct {
	// Model is omitzero because the Vertex deployment names the model in the
	// URL and REJECTS the body field. Every direct request sets it, so the
	// key never disappears from a request that needs it.
	Model string `json:"model,omitzero"`
	// AnthropicVersion is the Vertex deployment's version, which lives in the
	// body where the direct API's lives in the anthropic-version header. It is
	// empty — and therefore absent — on every direct request.
	AnthropicVersion string      `json:"anthropic_version,omitzero"`
	MaxTokens        int         `json:"max_tokens"`
	System           []sysBlock  `json:"system,omitzero"`
	Messages         []message   `json:"messages"`
	Tools            []tool      `json:"tools,omitzero"`
	ToolChoice       *toolChoice `json:"tool_choice,omitzero"`
	Temperature      *float64    `json:"temperature,omitzero"`
	TopP             *float64    `json:"top_p,omitzero"`
	// StopSequences is core.Request.StopSequences. Dropping it silently, as
	// this did until the NFR-TEST-08 request golden showed the field missing
	// from the body, means a caller's stop condition never takes effect and
	// nothing says so.
	StopSequences []string  `json:"stop_sequences,omitzero"`
	Stream        bool      `json:"stream"`
	Thinking      *thinking `json:"thinking,omitzero"`
	// OutputConfig carries the effort level for a row whose thinking ladder
	// speaks effort tokens. It is emitted alongside {"type":"adaptive"} and
	// never alongside a budget: the two are different generations of the
	// same control, and a model accepts one or the other.
	OutputConfig *outputConfig `json:"output_config,omitzero"`
	// ContextManagement is REQ-PROV-07's body half. The compact-2026-01-12
	// beta header alone changes nothing; the server compacts only when the
	// body also names the edit.
	ContextManagement *contextManagement `json:"context_management,omitzero"`

	// immediateTools is the count of leading non-deferred tools. It is
	// unexported and therefore never marshalled; StampCacheControl reads it to
	// find the last tool eligible for a breakpoint.
	immediateTools int
}

type thinking struct {
	// Type is "enabled" (with a budget), "adaptive" (with an effort in
	// output_config, or none) or "disabled".
	Type         string `json:"type"`
	BudgetTokens *int   `json:"budget_tokens,omitzero"`
}

type outputConfig struct {
	Effort string `json:"effort,omitzero"` // low | medium | high | xhigh | max
}

type contextManagement struct {
	Edits []contextEdit `json:"edits"`
}

type contextEdit struct {
	Type string `json:"type"` // "compact_20260112"
}

type sysBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitzero"`
}

type cacheControl struct {
	Type string `json:"type"`         // "ephemeral"
	TTL  string `json:"ttl,omitzero"` // "1h" for long retention
}

type message struct {
	Role    string  `json:"role"` // "user" | "assistant"
	Content []block `json:"content"`
}

// block is the wire content block. It is a single struct with omitted fields
// rather than a union, because that is the shape Anthropic accepts and the
// canonical union has already been resolved by the time we get here.
type block struct {
	Type string `json:"type"`

	Text string `json:"text,omitzero"`

	// tool_use
	ID    string          `json:"id,omitzero"`
	Name  string          `json:"name,omitzero"`
	Input json.RawMessage `json:"input,omitzero"`

	// tool_result
	ToolUseID string  `json:"tool_use_id,omitzero"`
	Content   []block `json:"content,omitzero"`
	IsError   bool    `json:"is_error,omitzero"`

	// thinking. Thinking and Data are pointers because they are REQUIRED on
	// the block types that carry them, and an empty one is a value the model
	// really sends: a thinking block whose summary came out empty still
	// arrives signed and still has to be replayed. A plain string would be
	// dropped by omitzero and produce `{"type":"thinking","signature":"…"}`,
	// which the Messages API rejects with
	// `content.0.thinking.thinking: Field required` — the same shape of 400
	// an empty tool_result placeholder used to cause. A nil pointer omits the
	// key; a pointer to "" writes it.
	Thinking  *string `json:"thinking,omitzero"`
	Signature string  `json:"signature,omitzero"`
	Data      *string `json:"data,omitzero"` // redacted_thinking

	// image
	Source *imageSource `json:"source,omitzero"`

	CacheControl *cacheControl `json:"cache_control,omitzero"`

	// Raw carries a block this build does not model, verbatim. It is how
	// REQ-PROV-07's server-side compaction blocks are "passed back unchanged
	// in subsequent turns" without the SDK having to model a beta wire shape
	// that is expected to change: a compaction block decodes to core.RawBlock
	// and re-encodes to exactly the bytes that arrived.
	Raw json.RawMessage `json:"-"`
}

// MarshalJSON emits Raw verbatim when present, and the modelled fields
// otherwise. The alias type is required: a defined struct type does not
// inherit its source type's methods, so json.Marshal(alias(b)) recurses no
// further.
func (b block) MarshalJSON() ([]byte, error) {
	if len(b.Raw) > 0 {
		return b.Raw, nil
	}
	type alias block
	return json.Marshal(alias(b))
}

type imageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitzero"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitzero"`
	// There is deliberately no defer_loading here. REQ-CACHE-10's Anthropic
	// arm is ORDER alone: a tool that appeared mid-session is appended after
	// the established ones so the cached prefix stays byte-identical, and it
	// carries no cache_control because a breakpoint there would sit past the
	// prefix it exists to preserve. `defer_loading: true` is a different
	// feature — it HIDES the tool from the model until a tool-search server
	// tool finds it — and sending it without declaring that server tool made
	// every late-added tool invisible for the rest of the session.
}

type toolChoice struct {
	Type string `json:"type"` // "auto" | "none"
}

// ------------------------------------------------------------ id normalization

// NormalizeToolCallID rewrites an id into Anthropic's accepted shape:
// characters outside [A-Za-z0-9_-] become '_', truncated to 64. It is the
// function REQ-PROV-11 rule 5 uses when replaying a transcript produced by
// another provider.
func NormalizeToolCallID(s string) string {
	if s == "" {
		return s
	}
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// ------------------------------------------------------------ request building

// BuildRequest converts a canonical request into the Anthropic wire body. It
// is exported so the differential harness and the golden tests can capture the
// exact bytes without a network call or an API key (NFR-TEST-06.2).
//
// It runs the shared repair pass first (REQ-PROV-11) — that is part of the
// provider contract, not the loop's, because the loop is not running when a
// transcript is loaded from disk.
func BuildRequest(m *core.Model, req core.Request, retention core.CacheRetention) (*request, provider.RepairReport, error) {
	out, rep, _, err := BuildRequestCached(m, req, retention, nil)
	return out, rep, err
}

// BuildRequestCached is BuildRequest with REQ-CACHE-06's per-session schema
// cache attached. A nil prefix marshals every schema, which is what
// BuildRequest does and what a one-shot caller wants.
//
// NFR-PERF-03 is why this exists rather than being an internal detail: tool
// schema serialization "must be computed once per session and cached, not
// recomputed on every model call", and at 128 tools that recomputation is
// ~0.9 ms of the ~1.5 ms it takes to build a request — the dominant term, paid
// on every turn, for bytes that did not change.
func BuildRequestCached(m *core.Model, req core.Request, retention core.CacheRetention,
	prefix *provider.ToolPrefix) (*request, provider.RepairReport, provider.SyncReport, error) {
	var sync provider.SyncReport
	repaired, rep := provider.RepairTranscript(req.Messages, provider.TargetFor(m, NormalizeToolCallID))

	out := &request{
		Model:         m.ID,
		Messages:      encodeMessages(repaired),
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.StopSequences,
		Stream:        true,
	}
	// The current generation rejects temperature and top_p outright (a 400
	// naming the field), and the catalog row records that as
	// compat.supports_sampling: false. Dropping them is the only request that
	// is not an error; the caller's knob simply has no counterpart on this
	// model.
	if !parseCompat(m).supportsSampling() {
		out.Temperature, out.TopP = nil, nil
	}

	// A nil messages array is not an empty one on the wire: `"messages":null`
	// is a 400 where `[]` is merely an empty (also rejected, but by a message
	// that says so) transcript.
	if out.Messages == nil {
		out.Messages = []message{}
	}

	// REQ-CAT-04: the caller's max_tokens is an UPPER BOUND, not the value
	// sent. Input and output share one window here, so a request whose
	// max_tokens no longer fits is rejected — first seen deep into a long
	// session. The context estimate is the loop's anchored one (P-30). This
	// wire requires the field, so an absent request value falls back to the
	// model's own cap (clamped the same way) and never to 0.
	requested := 0
	if req.MaxTokens != nil {
		requested = *req.MaxTokens
	}
	out.MaxTokens = catalog.ClampMaxTokens(m, requested, req.EstContextTokens)
	if out.MaxTokens <= 0 {
		out.MaxTokens = DefaultMaxTokens
	}

	for _, b := range req.System {
		if tb, ok := b.(core.TextBlock); ok {
			out.System = append(out.System, sysBlock{Type: "text", Text: tb.Text})
		}
	}

	// REQ-CACHE-10: immediate tools first, deferred tools after. The ORDER is
	// the mechanism — appending late arrivals keeps the cached prefix byte-
	// identical, where inserting them anywhere else rewrites it and costs the
	// whole provider-side cache over one added tool.
	split := provider.SplitDeferredTools(req.Tools, req.Messages)
	out.immediateTools = len(split.Immediate)
	// ONE Sync over the whole tool list, before the split. Syncing the two
	// halves separately would make each call see the other half as removed —
	// reporting a prefix invalidation on every turn and evicting the very
	// entries the cache exists to keep.
	schemas, srep, err := prefix.Sync(req.Tools)
	if err != nil {
		return nil, rep, sync, err
	}
	sync = srep
	byName := make(map[string]json.RawMessage, len(req.Tools))
	for i, tw := range req.Tools {
		byName[tw.Name] = schemas[i]
	}
	// Both halves are declared as ordinary, model-visible tools. The deferred
	// half differs only in POSITION (after the prefix) and in carrying no
	// breakpoint; see the tool struct for why it is not marked defer_loading.
	appendTools := func(ts []core.ToolWire) {
		for _, tw := range ts {
			out.Tools = append(out.Tools, tool{Name: tw.Name, Description: tw.Description,
				InputSchema: byName[tw.Name]})
		}
	}
	appendTools(split.Immediate)
	appendTools(split.Deferred)

	// ToolChoice absent is NOT auto: a provider must not invent a selection
	// when the field is empty (REQ-TOOL-16). An explicit choice is forwarded
	// even with no tools, which is what makes a tool-free summarization turn
	// reliably forceable.
	switch req.ToolChoice {
	case core.ToolChoiceAuto:
		out.ToolChoice = &toolChoice{Type: "auto"}
	case core.ToolChoiceNone:
		out.ToolChoice = &toolChoice{Type: "none"}
	}

	StampCacheControl(out, retention, m)
	return out, rep, sync, nil
}

// StampCacheControl places the §6.2a Level 1 breakpoints.
//
// Exactly three placements, recomputed on EVERY request:
//
//  1. the LAST system block only — a marker caches everything rendered
//     before it, and the wire renders tools, then system, then messages, so
//     one breakpoint here covers the tool list and the whole system prompt
//     together. Stamping every system block spent one of the four
//     breakpoints per block, and a prompt with three or more blocks pushed
//     the request over the limit;
//  2. the last IMMEDIATE tool — the marker covers all preceding tools, so
//     one breakpoint suffices and the budget is not spent on the tool list;
//  3. the last content block of the last message, when that message is a user
//     message and the block is text, image or tool_result.
//
// Placement 3 is a ROLLING breakpoint: it moves forward every turn, extending
// the cached prefix to include the previous turn's tool results. That is the
// whole point. The "optimization" of recomputing only when the system prompt
// or tool list changes produces a static prefix-only breakpoint, which re-pays
// full input price on the entire growing transcript — the dominant cost in
// exactly the multi-turn agent workload this SDK exists for.
//
// Under CacheRetentionLong the 1h TTL goes on placements 1 and 2 only. The
// rolling breakpoint stays at the default 5-minute TTL: it is rewritten every
// turn anyway, a 1h write bills at twice the 5m rate, and the wire requires
// every 1h entry to precede every 5m one — which tools -> system -> messages
// order satisfies exactly when the message breakpoint is the short one.
func StampCacheControl(r *request, retention core.CacheRetention, m *core.Model) {
	if retention == core.CacheRetentionNone {
		return
	}
	short := &cacheControl{Type: "ephemeral"}
	prefix := short
	if retention == core.CacheRetentionLong && supportsLongRetention(m) {
		prefix = &cacheControl{Type: "ephemeral", TTL: "1h"}
	}

	if n := len(r.System); n > 0 {
		r.System[n-1].CacheControl = prefix
	}
	// The breakpoint goes on the last IMMEDIATE tool, not the last tool.
	// A deferred tool sits after the prefix by construction (REQ-CACHE-10);
	// stamping it would place the breakpoint past the very content the
	// deferral exists to keep cached.
	if n := r.immediateTools; n > 0 && n <= len(r.Tools) {
		r.Tools[n-1].CacheControl = prefix
	}
	if n := len(r.Messages); n > 0 {
		last := &r.Messages[n-1]
		if last.Role == "user" && len(last.Content) > 0 {
			lb := &last.Content[len(last.Content)-1]
			switch lb.Type {
			case "text", "image", "tool_result":
				lb.CacheControl = short
			}
		}
	}
}

// compat is the subset of a catalog row's compat object this adapter reads.
// Absent keys take the conservative default; an unparseable object reads as
// wholly absent rather than failing the request.
type compat struct {
	SupportsLongCacheRetention *bool `json:"supports_long_cache_retention"`
	// SupportsSampling is false on rows whose model rejects temperature and
	// top_p (the Claude 4.7+ generation). Absent means true.
	SupportsSampling *bool `json:"supports_sampling"`
}

func parseCompat(m *core.Model) compat {
	var c compat
	if m == nil || len(m.Compat) == 0 {
		return c
	}
	if err := json.Unmarshal(m.Compat, &c); err != nil {
		return compat{}
	}
	return c
}

func (c compat) supportsSampling() bool {
	return c.SupportsSampling == nil || *c.SupportsSampling
}

func supportsLongRetention(m *core.Model) bool {
	c := parseCompat(m)
	return c.SupportsLongCacheRetention == nil || *c.SupportsLongCacheRetention
}

// encodeMessages is where REQ-LOOP-02's Anthropic half lives.
//
// Consecutive ToolResultMessages are COALESCED into ONE {"role":"user"}
// message holding all their tool_result blocks. Splitting them across messages
// silently degrades parallel tool call quality — that part of the PRD is
// right. What the PRD got wrong is calling it a LOOP invariant: it is a wire
// rule, true here and false for OpenAI, which mandates one message per result.
//
// Consecutive USER messages are also coalesced. Nothing in the PRD asks for
// that, and nothing in it forbids the state that needs it: compaction prepends
// a summary UserMessage to a suffix that, when the cut lands on a user message
// (which REQ-GO-14's turn-splitting actively tries to produce), begins with
// another UserMessage. Anthropic rejects two adjacent user turns, so without
// this the first compacted request 400s (ruling P-5).
func encodeMessages(ms core.Messages) []message {
	var out []message

	appendTo := func(role string, blocks ...block) {
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, message{Role: role, Content: blocks})
	}

	for _, m := range ms {
		switch v := m.(type) {
		case core.UserMessage:
			b := encodeBlocks(v.Content)
			if len(b) == 0 {
				// Every block was dropped (empty text, unmodelled raw), and
				// `"content":[]` is a 400. A skipped user turn keeps role
				// alternation intact whenever the neighbours already
				// alternated, which is the only way the transcript could
				// have been valid before the message was emptied.
				continue
			}
			appendTo("user", b...)

		case core.AssistantMessage:
			b := encodeBlocks(v.Content)
			if len(b) == 0 {
				// Anthropic rejects an assistant turn with empty content.
				continue
			}
			appendTo("assistant", b...)

		case core.ToolResultMessage:
			// A tool result is a USER-role block on this wire.
			//
			// Ordinary content on the result nests inside the tool_result
			// block when it is text or image, and is displaced into siblings
			// positioned AFTER every tool_result block otherwise — Anthropic
			// rejects the interleaved mix (ruling P-42).
			inner, displaced := splitResultContent(v.Content)
			tr := block{
				Type:      "tool_result",
				ToolUseID: v.ToolUseID,
				Content:   inner,
				IsError:   v.IsError,
			}
			appendTo("user", tr)
			if len(displaced) > 0 {
				// Displaced blocks must come after ALL tool_result blocks in
				// the message, so they are appended once the run of results
				// ends. Appending here is correct because a later result in
				// the same run appends its tool_result after them only if we
				// keep tool_result blocks first — so hold them to the end.
				n := len(out) - 1
				out[n].Content = reorderToolResultsFirst(append(out[n].Content, displaced...))
			}
		}
	}
	return out
}

// splitResultContent separates content that may nest inside a tool_result from
// content that must be displaced into siblings.
func splitResultContent(c core.Content) (inner []block, displaced []block) {
	for _, b := range c {
		switch v := b.(type) {
		case core.TextBlock:
			if v.Text == "" {
				// Text is omitzero, so this would serialize as {"type":"text"}
				// — a text block with no text key, which is a 400 — and the
				// block carries nothing the model could read anyway.
				continue
			}
			inner = append(inner, block{Type: "text", Text: v.Text})
		case core.ImageBlock:
			inner = append(inner, block{Type: "image", Source: &imageSource{
				Type: "base64", MediaType: v.MimeType, Data: v.Data}})
		default:
			displaced = append(displaced, encodeBlock(b))
		}
	}
	// An empty result stays EMPTY: content is optional on a tool_result, and
	// the block's own omitzero drops the key. The previous placeholder,
	// {Type:"text", Text:""}, serialized as {"type":"text"} — Text is omitzero
	// too — and a text block with no text key is a 400 on every empty tool
	// output, which is what a successful `rm` produces (REQ-LOOP-02).
	return inner, displaced
}

// reorderToolResultsFirst is a stable partition: every tool_result block
// first, in order, then everything else, in order.
func reorderToolResultsFirst(bs []block) []block {
	out := make([]block, 0, len(bs))
	for _, b := range bs {
		if b.Type == "tool_result" {
			out = append(out, b)
		}
	}
	for _, b := range bs {
		if b.Type != "tool_result" {
			out = append(out, b)
		}
	}
	return out
}

func encodeBlocks(c core.Content) []block {
	out := make([]block, 0, len(c))
	for _, b := range c {
		if eb := encodeBlock(b); eb.Type != "" {
			out = append(out, eb)
		}
	}
	return out
}

func encodeBlock(b core.ContentBlock) block {
	switch v := b.(type) {
	case core.TextBlock:
		if v.Text == "" {
			return block{}
		}
		return block{Type: "text", Text: v.Text}
	case core.ThinkingBlock:
		if v.Redacted {
			return block{Type: "redacted_thinking", Data: &v.Signature}
		}
		return block{Type: "thinking", Thinking: &v.Thinking, Signature: v.Signature}
	case core.ToolUseBlock:
		// The model's own argument bytes are written into the input position
		// UNCHANGED — no decode-and-re-encode round trip, which would sort the
		// keys and shift the prompt-cache prefix (REQ-PROV-17, REQ-TOOL-12).
		return block{Type: "tool_use", ID: v.ID, Name: v.Name, Input: v.Input}
	case core.ImageBlock:
		return block{Type: "image", Source: &imageSource{
			Type: "base64", MediaType: v.MimeType, Data: v.Data}}
	case core.ToolResultBlock:
		inner, _ := splitResultContent(v.Content)
		return block{Type: "tool_result", ToolUseID: v.ToolUseID, Content: inner, IsError: v.IsError}
	case core.RawBlock:
		// Replayed verbatim (REQ-PROV-07). Dropping it would be the safe-
		// looking choice and it is wrong here: a server-side compaction block
		// the model expects to see again is load-bearing state, and a
		// transcript that silently loses it re-sends the history the
		// compaction was paid to remove.
		if len(v.Raw) == 0 || v.Type == "" {
			return block{}
		}
		return block{Type: v.Type, Raw: v.Raw}
	}
	return block{}
}

// MapStopReason normalizes an Anthropic stop_reason. The provider's own string
// is preserved verbatim by the caller in RawStopReason; this mapping is lossy
// on purpose and never drives control flow (REQ-LOOP-01).
func MapStopReason(s string) core.StopReason {
	switch s {
	case "end_turn":
		return core.StopReasonStop
	case "max_tokens":
		return core.StopReasonLength
	case "tool_use":
		return core.StopReasonToolUse
	case "stop_sequence":
		return core.StopReasonStopSequence
	case "refusal":
		return core.StopReasonRefusal
	}
	// "pause_turn" lands here on purpose. It is the server pausing a long
	// server-tool turn, not the model finishing, and the canonical vocabulary
	// has no stop reason for it; mapping it to Stop ends the turn cleanly
	// while RawStopReason keeps the vendor's own word for a caller that wants
	// to resume by re-sending the transcript. Inventing a Continue reason
	// would be a loop-level contract change, not a wire mapping.
	return core.StopReasonStop
}
