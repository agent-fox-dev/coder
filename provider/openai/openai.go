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
// flag. (Not implemented in v1.)
package openai

import (
	"encoding/json"
	"strings"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
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
	Stream      bool      `json:"stream"`

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
type Compat struct {
	UseMaxTokens            bool `json:"use_max_tokens"`
	SupportsStore           bool `json:"supports_store"`
	SupportsDeveloperRole   bool `json:"supports_developer_role"`
	SupportsReasoningEffort bool `json:"supports_reasoning_effort"`
	SupportsStrictTools     bool `json:"supports_strict_tools"`
	// AllowsNullAssistantContent: some gateways reject content:null and want "".
	AllowsNullAssistantContent bool `json:"allows_null_assistant_content"`
	// RequiresToolResultName: some gateways require name on a tool message.
	RequiresToolResultName bool `json:"requires_tool_result_name"`
}

// DefaultCompat is the api.openai.com profile. Every other vendor turns
// something off.
func DefaultCompat() Compat {
	return Compat{
		SupportsStore:              true,
		SupportsDeveloperRole:      true,
		SupportsReasoningEffort:    true,
		SupportsStrictTools:        true,
		AllowsNullAssistantContent: true,
	}
}

// CompatFor resolves a model's profile, starting from the default and applying
// whatever the catalog row overrides key by key.
func CompatFor(m *core.Model) Compat {
	c := DefaultCompat()
	if len(m.Compat) > 0 {
		_ = json.Unmarshal(m.Compat, &c)
	}
	return c
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
func BuildRequest(m *core.Model, req core.Request) (*request, provider.RepairReport, error) {
	compat := CompatFor(m)
	repaired, rep := provider.RepairTranscript(req.Messages, provider.TargetFor(m, NormalizeToolCallID))

	out := &request{
		Model:       m.ID,
		Messages:    encodeMessages(repaired, req.System, compat),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      true,
	}

	// The rename is a real vendor split, not a preference.
	if req.MaxTokens != nil {
		if compat.UseMaxTokens {
			out.MaxTokens = req.MaxTokens
		} else {
			out.MaxCompletionTokens = req.MaxTokens
		}
	}

	if compat.SupportsStore {
		f := false
		out.Store = &f
	}
	if req.Options.SessionID != "" {
		out.PromptCacheKey = clampCacheKey(req.Options.SessionID)
	}
	if compat.SupportsReasoningEffort && req.ThinkingLevel != "" && req.ThinkingLevel != core.ThinkingOff {
		out.ReasoningEffort = string(req.ThinkingLevel)
	}

	for _, tw := range req.Tools {
		raw, err := json.Marshal(tw.InputSchema)
		if err != nil {
			return nil, rep, err
		}
		f := toolFunction{Name: tw.Name, Description: tw.Description, Parameters: raw}
		if compat.SupportsStrictTools && tw.ConstrainedSampling != nil &&
			tw.ConstrainedSampling.Type == core.ConstrainJSONSchema {
			t := true
			f.Strict = &t
		}
		out.Tools = append(out.Tools, tool{Type: "function", Function: f})
	}

	switch req.ToolChoice {
	case core.ToolChoiceAuto:
		out.ToolChoice = "auto"
	case core.ToolChoiceNone:
		out.ToolChoice = "none"
	}
	return out, rep, nil
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
