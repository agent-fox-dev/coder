// Package openai implements the OpenAI Chat Completions wire API.
//
// It is keyed by WIRE API, not by vendor (REQ-PROV-02). The same
// implementation serves OpenAI, OpenRouter's non-Anthropic models, Groq,
// DeepSeek, Together, vLLM and llama.cpp — differing only by a per-model
// compatibility profile, because "OpenAI-compatible" is not a base-URL swap
// (REQ-PROV-12).
//
// OpenAI Responses is a SEPARATE wire API and therefore a separate package: it
// differs in the message model, the tool-call identity model, the reasoning
// replay model and the billing model. It is not this implementation with a
// flag; see provider/openairesponses.
package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/agentfox/agentkit-go/catalog"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/schema"
)

// API is the wire API id.
const API core.API = "openai-completions"

// ---------------------------------------------------------------- wire types

type request struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Tools       []tool    `json:"tools,omitzero"`
	ToolChoice  string    `json:"tool_choice,omitzero"`
	Temperature *float64  `json:"temperature,omitzero"`
	TopP        *float64  `json:"top_p,omitzero"`
	// Stop is core.Request.StopSequences. See the note on the Anthropic
	// request struct: this was dropped silently until a request golden made
	// the omission visible.
	Stop   []string `json:"stop,omitzero"`
	Stream bool     `json:"stream"`
	// StreamOptions.include_usage is REQUIRED for a streamed request to report
	// usage at all. Without it OpenAI returns `usage: null` on every chunk,
	// which REQ-GO-15 forbids as a context anchor and REQ-PROV-05 forbids as a
	// cost basis — so the field is not optional in practice, and is emitted
	// unconditionally rather than behind a compat flag nobody has a
	// reproducing case for.
	StreamOptions *streamOptions `json:"stream_options,omitzero"`

	// Exactly one of these is emitted, chosen by the compat profile's
	// UseMaxTokens flag. Sending the wrong one is a 400 on the vendors that
	// have not tracked the rename (REQ-PROV-12).
	MaxTokens           *int `json:"max_tokens,omitzero"`
	MaxCompletionTokens *int `json:"max_completion_tokens,omitzero"`

	ReasoningEffort string `json:"reasoning_effort,omitzero"`
	// PromptCacheKey is §6.2a Level 0: without it, cache hits are best-effort
	// prefix matching rather than addressed. Clamped to 64 runes because the
	// API rejects longer.
	PromptCacheKey string `json:"prompt_cache_key,omitzero"`
	Store          *bool  `json:"store,omitzero"`
}

// message is one Chat Completions message. Note the shape: a tool result is
// its OWN message with role "tool", not a block inside a user message. This is
// the asymmetry with Anthropic that makes REQ-LOOP-02's "one user message
// holding every tool_result" unimplementable as a canonical invariant.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type message struct {
	Role string `json:"role"` // system | developer | user | assistant | tool

	// Content is a string for simple messages and a part array when it must
	// carry images. A present-null content is distinct from an absent one on
	// some gateways, which is why it is a pointer (REQ-PROV-16).
	Content any `json:"content"`

	// assistant
	ToolCalls []toolCall `json:"tool_calls,omitzero"`
	// deepseek and friends echo reasoning back on the assistant message
	ReasoningContent string `json:"reasoning_content,omitzero"`

	// tool
	ToolCallID string `json:"tool_call_id,omitzero"`
	Name       string `json:"name,omitzero"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitzero"`
	ImageURL *imageURL `json:"image_url,omitzero"`
	// CacheControl is Anthropic's breakpoint carried over the Chat Completions
	// wire, which OpenRouter forwards for its anthropic/* routes (§6.2a,
	// REQ-PROV-12 CacheControlFormat). Absent everywhere else.
	CacheControl *cacheControl `json:"cache_control,omitzero"`
}

type cacheControl struct {
	Type string `json:"type"`         // "ephemeral"
	TTL  string `json:"ttl,omitzero"` // "1h" when long retention is supported
}

type imageURL struct {
	URL string `json:"url"`
}

type toolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"` // "function"
	Function function `json:"function"`
}

type function struct {
	Name string `json:"name"`
	// Arguments is a JSON STRING, not an object. That is why key order is
	// model-visible on this wire and why replaying a re-serialized map shifts
	// the prompt-cache prefix for the rest of the session (REQ-TOOL-12.2).
	Arguments string `json:"arguments"`
}

type tool struct {
	Type     string       `json:"type"` // "function"
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitzero"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitzero"`
}

// Compat is the per-model compatibility profile of REQ-PROV-12. Each flag
// corresponds to a request that 400s, hangs, or silently produces no answer
// when the default is used. A flag is added only with a named vendor and a
// reproducing case.
//
// The JSON keys ARE the catalog vocabulary: a row's `compat` object is decoded
// straight into this struct with unknown fields rejected (see CompatFor), so
// a key this struct does not declare cannot be written into the catalog and
// silently ignored — which is exactly how every override was being lost.
type Compat struct {
	UseMaxTokens            bool `json:"use_max_tokens"`
	SupportsStore           bool `json:"supports_store"`
	SupportsDeveloperRole   bool `json:"supports_developer_role"`
	SupportsReasoningEffort bool `json:"supports_reasoning_effort"`
	SupportsStrictTools     bool `json:"supports_strict_tools"`
	// SupportsTemperature: the o-series rejects temperature and top_p
	// outright. When false both are omitted, whatever the caller set.
	SupportsTemperature bool `json:"supports_temperature"`
	// SupportsLongCacheRetention gates the 1h cache TTL where cache_control
	// is emitted at all (CacheControlFormat); false on Together, Cloudflare
	// and Nvidia, which reject the ttl field.
	SupportsLongCacheRetention bool `json:"supports_long_cache_retention"`
	// SupportsFinishReason: whether finish_reason may be trusted. False on
	// servers that never emit it, or emit one that contradicts the content;
	// the stop reason is then inferred from the content instead.
	SupportsFinishReason bool `json:"supports_finish_reason"`
	// AllowsNullAssistantContent: some gateways reject content:null and want "".
	AllowsNullAssistantContent bool `json:"allows_null_assistant_content"`
	// AllowsUserAfterToolResult: where false, a user message directly after a
	// tool result needs a synthetic assistant turn between them. Declared so
	// the catalog can record it; not yet wired.
	AllowsUserAfterToolResult bool `json:"allows_user_after_tool_result"`
	// RequiresToolResultName: some gateways require name on a tool message.
	RequiresToolResultName bool `json:"requires_tool_result_name"`
	// ThinkingFormat is the wire shape for reasoning replay: openai,
	// deepseek, together, openrouter or chat-template. Declared; not yet wired.
	ThinkingFormat string `json:"thinking_format"`
	// ThinkingTokenBudgetField names the reasoning-budget field on servers
	// that share max_tokens between reasoning and the answer. Declared; not
	// yet wired.
	ThinkingTokenBudgetField string `json:"thinking_token_budget_field"`
	// CacheControlFormat: "anthropic" emits cache_control on content parts
	// (OpenRouter anthropic/* routes). Empty emits nothing.
	CacheControlFormat string `json:"cache_control_format"`
}

// DefaultCompat is the api.openai.com profile. Every other vendor turns
// something off.
func DefaultCompat() Compat {
	return Compat{
		SupportsStore:              true,
		SupportsDeveloperRole:      true,
		SupportsReasoningEffort:    true,
		SupportsStrictTools:        true,
		SupportsTemperature:        true,
		SupportsLongCacheRetention: true,
		SupportsFinishReason:       true,
		AllowsNullAssistantContent: true,
		AllowsUserAfterToolResult:  true,
		ThinkingFormat:             "openai",
	}
}

// ErrUnknownCompatKey is returned by CompatFor for a catalog row whose compat
// object carries a key this profile does not declare.
var ErrUnknownCompatKey = errors.New("agentkit: unknown compat key for openai-completions")

// CompatFor resolves a model's profile (REQ-PROV-12): the default, then the
// profile INFERRED from model.Provider and model.BaseURL, then the catalog
// row's key-by-key overrides on top.
//
// An unknown key in the row is an ERROR, never ignored. The catalog rows
// once used a vocabulary this struct did not read, and every override — the
// o-series' max_completion_tokens rename, its temperature rejection — was
// dropped without a sound. Failing here turns that into a request that ends
// with a pre-closed error stream naming the key (REQ-PROV-04).
func CompatFor(m *core.Model) (Compat, error) {
	c := InferCompat(m)
	if len(m.Compat) == 0 {
		return c, nil
	}
	dec := json.NewDecoder(bytes.NewReader(m.Compat))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Compat{}, fmt.Errorf("%w: model %q: %v", ErrUnknownCompatKey, m.ID, err)
	}
	return c, nil
}

// InferCompat is the Provider+BaseURL half of REQ-PROV-12: the profile a
// model gets before its catalog row says anything.
//
// Every departure from DefaultCompat below is a row of the REQ-PROV-12 table.
// "OpenAI-compatible" is not a base-URL swap: the first request to any of
// these hosts with the api.openai.com profile 400s on `store`, or on the
// developer role, or on max_completion_tokens.
func InferCompat(m *core.Model) Compat {
	c := DefaultCompat()
	if m == nil {
		return c
	}
	host := strings.ToLower(hostOf(m.BaseURL))
	vendor := strings.ToLower(m.Provider)
	if host == "" || host == "api.openai.com" {
		return c
	}
	// Every non-api.openai.com host.
	c.SupportsStore = false
	c.SupportsDeveloperRole = false

	switch {
	case vendor == "openrouter" || host == "openrouter.ai":
		id := strings.ToLower(m.ID)
		if strings.HasPrefix(id, "anthropic/") || strings.HasPrefix(id, "openai/") {
			c.SupportsDeveloperRole = true
		}
		if strings.HasPrefix(id, "anthropic/") {
			c.CacheControlFormat = "anthropic"
			c.ThinkingFormat = "openrouter"
		}
	case vendor == "deepseek" || strings.HasSuffix(host, "deepseek.com"):
		c.UseMaxTokens = true
		c.ThinkingFormat = "deepseek"
	case vendor == "moonshot" || strings.Contains(host, "moonshot"):
		c.UseMaxTokens = true
		c.SupportsReasoningEffort = false
		c.SupportsStrictTools = false
	case vendor == "together" || strings.HasSuffix(host, "together.xyz") || strings.HasSuffix(host, "together.ai"):
		c.UseMaxTokens = true
		c.SupportsReasoningEffort = false
		c.SupportsStrictTools = false
		c.SupportsLongCacheRetention = false
		c.ThinkingFormat = "together"
	case vendor == "nvidia" || strings.HasSuffix(host, "api.nvidia.com"):
		c.UseMaxTokens = true
		c.SupportsReasoningEffort = false
		c.SupportsStrictTools = false
		c.SupportsLongCacheRetention = false
	case vendor == "cloudflare" || strings.HasSuffix(host, "gateway.ai.cloudflare.com"):
		c.UseMaxTokens = true
		c.SupportsReasoningEffort = false
		c.SupportsStrictTools = false
		c.SupportsLongCacheRetention = false
	case vendor == "xai" || strings.HasSuffix(host, "api.x.ai"):
		c.SupportsReasoningEffort = false
	}
	return c
}

func hostOf(base string) string {
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base
	}
	return u.Hostname()
}

// NormalizeToolCallID: OpenAI accepts its own ids and is tolerant, but a
// cross-provider replay still needs a deterministic mapping.
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
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// BuildRequest converts a canonical request into the Chat Completions body.
// Exported for the golden and differential harnesses (NFR-TEST-06.2).
//
// Cache retention is read from req.Options.CacheRetention, defaulting to
// short; Stream applies the ProviderStreamOptions default first.
func BuildRequest(m *core.Model, req core.Request) (*request, provider.RepairReport, error) {
	retention := core.CacheRetentionShort
	if r := req.Options.CacheRetention; r != nil {
		retention = *r
	}
	out, rep, _, err := buildRequest(m, req, retention)
	return out, rep, err
}

func buildRequest(m *core.Model, req core.Request, retention core.CacheRetention) (*request, provider.RepairReport, Compat, error) {
	compat, err := CompatFor(m)
	if err != nil {
		return nil, provider.RepairReport{}, compat, err
	}
	repaired, rep := provider.RepairTranscript(req.Messages, provider.TargetFor(m, NormalizeToolCallID))

	out := &request{
		Model:    m.ID,
		Messages: encodeMessages(repaired, req.System, compat),
		Stop:     req.StopSequences,
		Stream:   true,

		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if compat.SupportsTemperature {
		// The o-series rejects both fields outright; omitting them is the
		// only request that is not a 400, and the 400 names sampling rather
		// than the model, sending the reader to the wrong knob.
		out.Temperature = req.Temperature
		out.TopP = req.TopP
	}

	// REQ-CAT-04: the caller's max_tokens is an UPPER BOUND, clamped against
	// what remains of the window after the loop's anchored context estimate.
	// Absent stays absent — this wire does not require the field, and
	// inventing one caps output the caller never asked to cap.
	// The rename is a real vendor split, not a preference.
	if v := catalog.ClampRequestMaxTokens(m, req); v != nil {
		if compat.UseMaxTokens {
			out.MaxTokens = v
		} else {
			out.MaxCompletionTokens = v
		}
	}

	if compat.SupportsStore {
		f := false
		out.Store = &f
	}
	if req.Options.SessionID != "" {
		out.PromptCacheKey = clampCacheKey(req.Options.SessionID)
	}
	// REQ-PROV-15: clamp upward, then downward, and send the RETURNED wire
	// value. `reasoning_effort: "xhigh"` to a model that does not know it is
	// a 400, and a model with no map at all gets no key.
	if compat.SupportsReasoningEffort && req.ThinkingLevel != core.ThinkingUnset &&
		req.ThinkingLevel != core.ThinkingOff {
		if _, wire, ok := catalog.ClampThinkingLevel(m, req.ThinkingLevel); ok {
			out.ReasoningEffort = wire
		}
	}

	for _, tw := range req.Tools {
		params, strict, err := encodeSchema(tw, compat.SupportsStrictTools)
		if err != nil {
			return nil, rep, compat, err
		}
		f := toolFunction{Name: tw.Name, Description: tw.Description, Parameters: params}
		if strict {
			t := true
			f.Strict = &t
		}
		out.Tools = append(out.Tools, tool{Type: "function", Function: f})
	}
	stampCacheControl(out.Messages, retention, compat)

	switch req.ToolChoice {
	case core.ToolChoiceAuto:
		out.ToolChoice = "auto"
	case core.ToolChoiceNone:
		out.ToolChoice = "none"
	}
	return out, rep, compat, nil
}

// encodeSchema is REQ-TOOL-03 for this wire: the schema is PROBED through the
// strict-subset rewrite before anything is sent, and strict:true is emitted
// only on the rewritten schema.
//
// A bare strict:true on the raw schema is rejected the moment a tool has an
// optional property or a $ref, and the 400 kills the whole request — every
// turn carrying that tool, not just that tool. On a failed rewrite `prefer`
// (the default) falls back to the unconstrained schema; `require` fails the
// request with the rejection reason, which Stream turns into a pre-closed
// error stream (REQ-PROV-04). Emission is additionally gated on the compat
// profile's SupportsStrictTools (REQ-PROV-12): a profile that cannot emit
// strict cannot satisfy `require` either.
func encodeSchema(tw core.ToolWire, supportsStrict bool) (json.RawMessage, bool, error) {
	cs := tw.ConstrainedSampling
	wantStrict := cs != nil && cs.Type == core.ConstrainJSONSchema
	if wantStrict {
		var rewritten *schema.Schema
		var err error
		if !supportsStrict {
			err = fmt.Errorf("agentkit: this endpoint's compat profile does not support strict tool schemas")
		} else {
			rewritten, err = schema.StrictSubset(tw.InputSchema)
		}
		switch {
		case err == nil:
			raw, merr := json.Marshal(rewritten)
			if merr != nil {
				return nil, false, merr
			}
			return raw, true, nil
		case cs.Strict == core.StrictRequire:
			return nil, false, fmt.Errorf("tool %q requires constrained sampling: %w", tw.Name, err)
		}
		// prefer: fall through to the unconstrained schema.
	}
	raw, err := json.Marshal(tw.InputSchema)
	if err != nil {
		return nil, false, err
	}
	return raw, false, nil
}

// stampCacheControl places §6.2a Level 1 breakpoints on the Chat Completions
// wire where the compat profile says the gateway forwards them (OpenRouter's
// anthropic/* routes): on the system text and on the last user message's
// last text part. Everywhere else the wire has no such field and nothing is
// emitted. The 1h TTL is emitted only where SupportsLongCacheRetention says
// the gateway accepts it; Together, Cloudflare and Nvidia reject the field.
func stampCacheControl(ms []message, retention core.CacheRetention, compat Compat) {
	if compat.CacheControlFormat != "anthropic" || retention == core.CacheRetentionNone {
		return
	}
	cc := &cacheControl{Type: "ephemeral"}
	if retention == core.CacheRetentionLong && compat.SupportsLongCacheRetention {
		cc.TTL = "1h"
	}
	stamp := func(msg *message) {
		var parts []contentPart
		switch c := msg.Content.(type) {
		case string:
			parts = []contentPart{{Type: "text", Text: c}}
		case []contentPart:
			parts = c
		default:
			return
		}
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i].Type == "text" {
				parts[i].CacheControl = cc
				msg.Content = parts
				return
			}
		}
	}
	lastUser := -1
	for i := range ms {
		switch ms[i].Role {
		case "system", "developer":
			stamp(&ms[i])
		case "user":
			lastUser = i
		}
	}
	if lastUser >= 0 {
		stamp(&ms[lastUser])
	}
}

func clampCacheKey(s string) string {
	r := []rune(s)
	if len(r) <= 64 {
		return s
	}
	return string(r[:64])
}

// encodeMessages is REQ-LOOP-02's OpenAI half, and the direct counter-example
// to the PRD's original claim.
//
// Each ToolResultMessage becomes its OWN {"role":"tool","tool_call_id":...}
// message. Grouping N results into one message is NOT REPRESENTABLE here.
// A canonical layer that stored them as blocks of a shared user message could
// not produce this wire form at all — which is exactly why the canonical
// transcript keeps one ToolResultMessage per call and lets each provider pack
// them its own way.
func encodeMessages(ms core.Messages, system []core.ContentBlock, compat Compat) []message {
	var out []message

	if len(system) > 0 {
		role := "system"
		if compat.SupportsDeveloperRole {
			role = "developer"
		}
		var sb strings.Builder
		for _, b := range system {
			if tb, ok := b.(core.TextBlock); ok {
				sb.WriteString(tb.Text)
			}
		}
		out = append(out, message{Role: role, Content: sb.String()})
	}

	for _, m := range ms {
		switch v := m.(type) {
		case core.UserMessage:
			out = append(out, message{Role: "user", Content: encodeContent(v.Content)})

		case core.AssistantMessage:
			msg := message{Role: "assistant"}
			var text strings.Builder
			for _, b := range v.Content {
				switch bv := b.(type) {
				case core.TextBlock:
					text.WriteString(bv.Text)
				case core.ThinkingBlock:
					// Reasoning replay is profile-dependent; the neutral
					// behaviour is to drop it rather than invent a field the
					// vendor does not read.
					_ = bv
				case core.ToolUseBlock:
					msg.ToolCalls = append(msg.ToolCalls, toolCall{
						ID: bv.ID, Type: "function",
						Function: function{
							Name: bv.Name,
							// The model's own bytes, verbatim, as the JSON
							// string this wire expects (REQ-PROV-17).
							Arguments: string(bv.Input),
						},
					})
				}
			}
			if text.Len() > 0 {
				msg.Content = text.String()
			} else if compat.AllowsNullAssistantContent {
				msg.Content = nil
			} else {
				msg.Content = ""
			}
			out = append(out, msg)

		case core.ToolResultMessage:
			// ONE MESSAGE PER RESULT. This is the asymmetry.
			tm := message{Role: "tool", ToolCallID: v.ToolUseID, Content: v.Content.Text()}
			if compat.RequiresToolResultName {
				tm.Name = v.ToolName
			}
			out = append(out, tm)
		}
	}
	return out
}

// encodeContent returns a plain string when the content is text-only, and a
// part array only when it has to. Sending a one-element part array where a
// string would do changes the bytes for no reason, and the bytes are the
// prompt-cache key.
func encodeContent(c core.Content) any {
	hasNonText := false
	for _, b := range c {
		if _, ok := b.(core.TextBlock); !ok {
			hasNonText = true
			break
		}
	}
	if !hasNonText {
		return c.Text()
	}
	parts := make([]contentPart, 0, len(c))
	for _, b := range c {
		switch v := b.(type) {
		case core.TextBlock:
			parts = append(parts, contentPart{Type: "text", Text: v.Text})
		case core.ImageBlock:
			parts = append(parts, contentPart{Type: "image_url",
				ImageURL: &imageURL{URL: "data:" + v.MimeType + ";base64," + v.Data}})
		}
	}
	return parts
}

// MapFinishReason normalizes a Chat Completions finish_reason.
//
// The tool-call case is why REQ-LOOP-01 exists. Several OpenAI-compatible
// gateways emit finish_reason "stop" with a populated tool_calls array, and
// some emit no finish_reason at all. Mapping is therefore advisory: the loop
// decides whether to iterate from the CONTENT, and this function's output is
// used for reporting only.
func MapFinishReason(s string, hasToolCalls bool) core.StopReason {
	switch s {
	case "tool_calls", "function_call":
		return core.StopReasonToolUse
	case "length":
		return core.StopReasonLength
	case "content_filter":
		return core.StopReasonRefusal
	case "stop":
		if hasToolCalls {
			// Report it honestly. The loop does not read this to decide
			// iteration, but a consumer reading RunResult should not be told
			// "stop" when tool calls were emitted.
			return core.StopReasonToolUse
		}
		return core.StopReasonStop
	case "":
		// A server that never emits finish_reason: infer from content, and
		// never report an error (ruling P-41).
		if hasToolCalls {
			return core.StopReasonToolUse
		}
		return core.StopReasonStop
	}
	if hasToolCalls {
		return core.StopReasonToolUse
	}
	return core.StopReasonStop
}

// mapFinishReason is MapFinishReason gated on the compat profile
// (REQ-PROV-12 SupportsFinishReason): a server whose finish_reason cannot be
// trusted has its stop reason inferred from the content alone, as if the
// field had never arrived. The raw string is still recorded by the caller.
func mapFinishReason(s string, hasToolCalls bool, compat Compat) core.StopReason {
	if !compat.SupportsFinishReason {
		s = ""
	}
	return MapFinishReason(s, hasToolCalls)
}
