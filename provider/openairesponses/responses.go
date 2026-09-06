// Package openairesponses implements the OpenAI Responses wire API
// (REQ-PROV-02's `openai-responses`).
//
// It is a SEPARATE implementation from openai-completions, not that one with a
// flag, and §4's model table says why: the two differ in the message model
// (messages vs items), the tool-call identity model (`call_id` vs a composite
// of call id and item id), the reasoning-replay model, the caching parameters
// and the billing model. A shared implementation with a boolean would have to
// branch on that boolean in every one of those places, which is two
// implementations wearing one name.
package openairesponses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// API is the registry key and DefaultBaseURL/Path the endpoint.
const (
	API            core.API = "openai-responses"
	DefaultBaseURL          = "https://api.openai.com/v1"
	Path                    = "/responses"
)

// ---------------------------------------------------------------- request

type request struct {
	Model string `json:"model"`
	// Instructions is this wire's system prompt. It is a top-level STRING, not
	// a message with a role, and it is not part of `input` — so a system
	// prompt does not occupy an item and cannot be confused for one.
	Instructions string `json:"instructions,omitzero"`
	Input        []item `json:"input"`
	Tools        []tool `json:"tools,omitzero"`
	ToolChoice   string `json:"tool_choice,omitzero"`

	MaxOutputTokens *int     `json:"max_output_tokens,omitzero"`
	Temperature     *float64 `json:"temperature,omitzero"`
	TopP            *float64 `json:"top_p,omitzero"`

	Reasoning *reasoningConfig `json:"reasoning,omitzero"`
	// Include asks for fields that are otherwise omitted. Encrypted reasoning
	// is the one that matters: without it a stateless caller cannot replay the
	// model's chain across turns at all.
	Include []string `json:"include,omitzero"`

	// Store=false is the stateless mode. It is a POINTER because false is a
	// meaningful value the server does not default to, and a plain bool would
	// make "the caller asked not to store" indistinguishable from "the caller
	// said nothing".
	Store          *bool  `json:"store,omitzero"`
	PromptCacheKey string `json:"prompt_cache_key,omitzero"`
	ServiceTier    string `json:"service_tier,omitzero"`

	Stream bool `json:"stream"`
}

type reasoningConfig struct {
	Effort string `json:"effort,omitzero"`
	// Summary asks for a human-readable trace of the reasoning. Without it the
	// only thing that comes back is the encrypted blob, which replays
	// correctly and shows the caller nothing.
	Summary string `json:"summary,omitzero"`
}

// item is one element of `input`. The Responses wire is a list of ITEMS of
// different kinds, not a list of messages with content — a function call and
// its output are siblings of a message, not blocks inside one.
type item struct {
	Type string `json:"type"`

	// message
	Role    string `json:"role,omitzero"`
	Content []part `json:"content,omitzero"`

	// function_call / function_call_output
	CallID    string `json:"call_id,omitzero"`
	Name      string `json:"name,omitzero"`
	Arguments string `json:"arguments,omitzero"`
	Output    string `json:"output,omitzero"`

	// reasoning, and the item id every kind may carry
	ID               string          `json:"id,omitzero"`
	Summary          []summaryPart   `json:"summary,omitzero"`
	EncryptedContent string          `json:"encrypted_content,omitzero"`
	Status           string          `json:"status,omitzero"`
	Unknown          json.RawMessage `json:"-"`
}

type summaryPart struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

type part struct {
	Type string `json:"type"` // input_text | output_text | input_image
	Text string `json:"text,omitzero"`
	// ImageURL carries a data: URL on this wire. There is no base64+media_type
	// pair as on the Anthropic wire.
	ImageURL string `json:"image_url,omitzero"`
}

// tool is FLAT here: name, description and parameters sit on the tool itself
// rather than under a nested `function` object as on the Chat Completions
// wire. Reusing that shape produces a 400 with a message that does not say so.
type tool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitzero"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitzero"`
}

// ---------------------------------------------------------------- identity

// IDSeparator joins the call id and the item id into one canonical
// ToolUseBlock.ID (§4: "composite callId|itemId").
//
// Both are needed and they are not interchangeable. `call_id` is what a
// function_call_output must reference. The item `id` is what a reasoning
// replay must line up against — send the wrong one and the server rejects the
// turn or silently drops the chain. A canonical ToolUseBlock has ONE id field,
// so the two travel joined and are split at the wire boundary.
const IDSeparator = "|"

// JoinID builds the canonical id. An empty item id yields the bare call id, so
// a gateway that does not issue item ids round-trips unchanged.
func JoinID(callID, itemID string) string {
	if itemID == "" {
		return callID
	}
	return callID + IDSeparator + itemID
}

// SplitID recovers the pair. An id with no separator is a bare call id — which
// is what a tool call replayed from another provider looks like.
func SplitID(id string) (callID, itemID string) {
	if i := strings.Index(id, IDSeparator); i >= 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

// ---------------------------------------------------------------- compat

// Compat is this API's own profile (REQ-PROV-12).
//
// Every JSON key here is DISJOINT from the openai-completions profile's, as
// §4.2 requires. That is not tidiness: a catalog row's `compat` blob is a bag
// of keys, and a shared name would let a flag written for one wire be silently
// read by the other — where it means something else or nothing at all.
type Compat struct {
	// UseInstructionsField sends the system prompt as `instructions`. Off, it
	// becomes a leading message item instead, which is what gateways that
	// proxy Responses onto another backend accept.
	UseInstructionsField bool `json:"use_instructions_field"`
	// SupportsReasoningItems allows reasoning items in `input`.
	SupportsReasoningItems bool `json:"supports_reasoning_items"`
	// SupportsEncryptedReasoning requests and replays encrypted_content.
	SupportsEncryptedReasoning bool `json:"supports_encrypted_reasoning"`
	SupportsServiceTier        bool `json:"supports_service_tier"`
	SupportsPromptCacheKey     bool `json:"supports_prompt_cache_key"`
	SupportsFunctionStrict     bool `json:"supports_function_strict"`
	SupportsStoreFlag          bool `json:"supports_store_flag"`
	// SupportsReasoningSummary asks for a readable summary alongside the
	// encrypted blob.
	SupportsReasoningSummary bool `json:"supports_reasoning_summary"`
}

// DefaultCompat is the api.openai.com profile. Every other vendor turns
// something off.
func DefaultCompat() Compat {
	return Compat{
		UseInstructionsField:       true,
		SupportsReasoningItems:     true,
		SupportsEncryptedReasoning: true,
		SupportsServiceTier:        true,
		SupportsPromptCacheKey:     true,
		SupportsFunctionStrict:     true,
		SupportsStoreFlag:          true,
		SupportsReasoningSummary:   true,
	}
}

// CompatFor resolves a model's profile, overriding key by key from the catalog
// row.
func CompatFor(m *core.Model) Compat {
	c := DefaultCompat()
	if len(m.Compat) > 0 {
		_ = json.Unmarshal(m.Compat, &c)
	}
	return c
}

// ---------------------------------------------------------------- build

// BuildRequest translates a canonical request onto the Responses wire.
func BuildRequest(m *core.Model, req core.Request) (*request, provider.RepairReport, error) {
	compat := CompatFor(m)
	repaired, rep := provider.RepairTranscript(req.Messages, provider.TargetFor(m, NormalizeCallID))

	out := &request{
		Model:       m.ID,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      true,
	}

	sys := systemText(req.System)
	if sys != "" {
		if compat.UseInstructionsField {
			out.Instructions = sys
		} else {
			out.Input = append(out.Input, item{
				Type: "message", Role: "system",
				Content: []part{{Type: "input_text", Text: sys}},
			})
		}
	}
	out.Input = append(out.Input, encodeItems(repaired, compat)...)

	out.MaxOutputTokens = clampTokens(m, req)
	for _, t := range req.Tools {
		enc, err := encodeTool(t, compat)
		if err != nil {
			return nil, rep, err
		}
		out.Tools = append(out.Tools, enc)
	}
	if req.ToolChoice.IsSet() {
		// The same two words as the Chat Completions wire, and unlike it a
		// bare string rather than an object. Absent stays absent: REQ-TOOL-16
		// forbids inventing a selection.
		out.ToolChoice = string(req.ToolChoice)
	}
	applyReasoning(out, m, req, compat)
	applyCaching(out, req, compat)
	return out, rep, nil
}

func clampTokens(m *core.Model, req core.Request) *int {
	limit := m.MaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 && (*req.MaxTokens < limit || limit == 0) {
		limit = *req.MaxTokens
	}
	if limit <= 0 {
		return nil
	}
	return &limit
}

func encodeTool(t core.ToolWire, compat Compat) (tool, error) {
	raw, err := json.Marshal(t.InputSchema)
	if err != nil {
		return tool{}, err
	}
	// FLAT, unlike the Chat Completions wire's nested `function` object.
	out := tool{Type: "function", Name: t.Name, Description: t.Description, Parameters: raw}
	if compat.SupportsFunctionStrict && t.ConstrainedSampling != nil &&
		t.ConstrainedSampling.Type == core.ConstrainJSONSchema {
		yes := true
		out.Strict = &yes
	}
	return out, nil
}

// systemText flattens the canonical system blocks.
func systemText(blocks []core.ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if tb, ok := blk.(core.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// NormalizeCallID keeps a replayed id acceptable to this wire while preserving
// the composite form: only the CALL half is normalized, because the item half
// is either one this server issued or absent.
func NormalizeCallID(s string) string {
	callID, itemID := SplitID(s)
	callID = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, callID)
	if len(callID) > 64 {
		callID = callID[:64]
	}
	return JoinID(callID, itemID)
}

// clampCacheKey matches the Chat Completions clamp: the API rejects longer.
func clampCacheKey(s string) string {
	r := []rune(s)
	if len(r) <= 64 {
		return s
	}
	return string(r[:64])
}

// applyReasoning maps the canonical thinking level onto `reasoning.effort`.
func applyReasoning(out *request, m *core.Model, req core.Request, compat Compat) {
	if req.ThinkingLevel == core.ThinkingUnset || req.ThinkingLevel == core.ThinkingOff {
		return
	}
	out.Reasoning = &reasoningConfig{Effort: string(req.ThinkingLevel)}
	if compat.SupportsReasoningSummary {
		out.Reasoning.Summary = "auto"
	}
	if compat.SupportsEncryptedReasoning {
		// Without this the response carries no encrypted_content, and a
		// stateless caller — which is what AgentKit is, since it does not rely
		// on server-side conversation state — has nothing to replay. The chain
		// is silently lost between turns rather than reported.
		out.Include = append(out.Include, "reasoning.encrypted_content")
	}
}

// applyCaching is this wire's caching parameter set (§4: "the caching
// parameters" differ).
func applyCaching(out *request, req core.Request, compat Compat) {
	if compat.SupportsStoreFlag {
		// Stateless by default. Storing a response server-side keeps the full
		// prompt and completion on the vendor's side, which is a decision an
		// SDK must not make on an embedder's behalf.
		no := false
		out.Store = &no
	}
	if compat.SupportsPromptCacheKey && req.Options.SessionID != "" {
		out.PromptCacheKey = clampCacheKey(req.Options.SessionID)
	}
}

// encodeItems flattens canonical messages into wire items.
func encodeItems(msgs core.Messages, compat Compat) []item {
	var out []item
	for _, m := range msgs {
		switch v := m.(type) {
		case core.UserMessage:
			out = append(out, item{Type: "message", Role: "user",
				Content: encodeParts(v.Content, "input")})

		case core.AssistantMessage:
			out = append(out, encodeAssistant(v, compat)...)

		case core.ToolResultMessage:
			callID, _ := SplitID(v.ToolUseID)
			out = append(out, item{
				Type: "function_call_output", CallID: callID,
				Output: v.Content.Text(),
			})
		}
	}
	return out
}

// encodeAssistant is where the item model bites: one assistant message becomes
// SEVERAL items — reasoning, then text, then one per tool call — and their
// order is the order the model produced them.
func encodeAssistant(m core.AssistantMessage, compat Compat) []item {
	var (
		out  []item
		text []part
	)
	flushText := func() {
		if len(text) > 0 {
			out = append(out, item{Type: "message", Role: "assistant", Content: text})
			text = nil
		}
	}

	for _, b := range m.Content {
		switch v := b.(type) {
		case core.TextBlock:
			text = append(text, part{Type: "output_text", Text: v.Text})

		case core.ThinkingBlock:
			if !compat.SupportsReasoningItems || v.Signature == "" {
				// No signature means nothing to replay: the item id and the
				// encrypted blob travel in Signature, and an item without them
				// is rejected. Dropping it loses the chain, which is the same
				// outcome as sending it and being refused, minus the error.
				continue
			}
			flushText()
			out = append(out, decodeThinkingSignature(v, compat))

		case core.ToolUseBlock:
			flushText()
			callID, itemID := SplitID(v.ID)
			out = append(out, item{
				Type: "function_call", CallID: callID, ID: itemID,
				// The model's own bytes, verbatim, as the JSON string this
				// wire expects (REQ-PROV-17).
				Name: v.Name, Arguments: string(v.Input),
			})
		}
	}
	flushText()
	return out
}

// thinkingSignature is how a reasoning item survives a round trip through the
// canonical ThinkingBlock, which has one opaque Signature string and no place
// for an item id.
type thinkingSignature struct {
	ItemID    string `json:"item_id,omitzero"`
	Encrypted string `json:"encrypted,omitzero"`
}

// EncodeThinkingSignature packs the fields a reasoning replay needs.
//
// Signature is documented as provider-issued and opaque, never inspected by
// anything outside its own provider — so packing structured data into it is
// legitimate here and nowhere else.
func EncodeThinkingSignature(itemID, encrypted string) string {
	if itemID == "" && encrypted == "" {
		return ""
	}
	b, err := json.Marshal(thinkingSignature{ItemID: itemID, Encrypted: encrypted})
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeThinkingSignature(v core.ThinkingBlock, compat Compat) item {
	var sig thinkingSignature
	if err := json.Unmarshal([]byte(v.Signature), &sig); err != nil {
		// A signature from another provider. It cannot be replayed here, and
		// REQ-PROV-11 rule 3 strips it on cross-model replay anyway; emitting
		// an item with a foreign id would be rejected.
		return item{Type: "reasoning"}
	}
	it := item{Type: "reasoning", ID: sig.ItemID}
	if compat.SupportsEncryptedReasoning {
		it.EncryptedContent = sig.Encrypted
	}
	if v.Thinking != "" {
		it.Summary = []summaryPart{{Type: "summary_text", Text: v.Thinking}}
	}
	return it
}

func encodeParts(c core.Content, kind string) []part {
	var out []part
	for _, b := range c {
		switch v := b.(type) {
		case core.TextBlock:
			out = append(out, part{Type: kind + "_text", Text: v.Text})
		case core.ImageBlock:
			out = append(out, part{Type: "input_image",
				ImageURL: fmt.Sprintf("data:%s;base64,%s", v.MimeType, v.Data)})
		}
	}
	return out
}
