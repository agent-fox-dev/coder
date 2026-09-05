// Package ollama implements Ollama's NATIVE /api/chat wire API.
//
// It is keyed by WIRE API, not by vendor (REQ-PROV-02).
//
// # Why this is not the OpenAI-compatible shim
//
// Ollama also exposes an /v1 endpoint that speaks Chat Completions, and
// pointing the openai-completions implementation at it is one line. NFR-COMPAT-04
// is explicit that this is the wrong integration path: the compatibility layer
// does not fully implement streaming tool calls or the cache-token usage
// fields, so an agent loop built on it drops tool calls mid-stream and reports
// usage that the REQ-LOOP-08 budget gate then reads as zero. The native API is
// a DIFFERENT WIRE PROTOCOL, not a compat variant of Chat Completions, and
// therefore a separate Api rather than a compat profile flag.
//
// Four differences from Chat Completions that a reader coming from
// provider/openai will trip over:
//
//   - tool_call arguments are a JSON OBJECT, not a stringified JSON object.
//   - a tool result is {"role":"tool","content":...} with NO tool_call_id:
//     results are matched to calls POSITIONALLY. See encodeMessages.
//   - images ride as bare base64 strings in a message-level "images" array,
//     not as data: URLs inside content parts.
//   - sampling parameters live under "options", and the output cap is
//     "num_predict" — neither max_tokens nor max_completion_tokens.
//
// Streaming is NDJSON — one JSON object per line, terminated by an object with
// "done": true — not SSE. There are no "data:" prefixes and no [DONE]
// sentinel, so an SSE reader pointed at this endpoint blocks forever.
//
// There is no provider-side prompt caching on Ollama (NFR-COMPAT-04, §6.2a):
// nothing in this file stamps a cache breakpoint, and there is deliberately no
// equivalent of the Anthropic StampCacheControl. Level 2 (in-process request
// deduplication) is the only caching that applies.
package ollama

import (
	"encoding/json"
	"strings"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// API is the wire API id.
const API core.API = "ollama-chat"

// ---------------------------------------------------------------- wire types
//
// Optional scalars are pointers with `omitzero`, never bare values with
// `omitempty` (REQ-PROV-16): `omitempty` on options.temperature would DROP an
// explicit 0, turning "be deterministic" into "use the model's default
// temperature of 0.8" — a silently nondeterministic agent.

type request struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []tool    `json:"tools,omitzero"`
	// Stream is always emitted. Ollama's own default is true, but a bare bool
	// with omitempty could not express "stream: false" at all (REQ-PROV-16.3).
	Stream  bool     `json:"stream"`
	Options *options `json:"options,omitzero"`
	// Think is a tri-state on the wire: absent, false, or true. It is a
	// pointer for exactly that reason — sending think:false to a model with no
	// thinking support is a 400, and so is sending think:true, so "absent" has
	// to be reachable.
	Think *bool `json:"think,omitzero"`
	// Format carries a whole-response JSON schema for constrained decoding.
	Format json.RawMessage `json:"format,omitzero"`
	// KeepAlive controls how long the model stays resident. Empty means the
	// server default (5m), which is what a caller who said nothing wants.
	KeepAlive string `json:"keep_alive,omitzero"`
}

// options is Ollama's sampling block. Nothing here is a top-level request
// field, which is the difference a reader coming from Chat Completions is most
// likely to get wrong: a top-level "temperature" is silently IGNORED.
type options struct {
	Temperature *float64 `json:"temperature,omitzero"`
	TopP        *float64 `json:"top_p,omitzero"`
	// NumPredict is the output cap. Neither max_tokens nor
	// max_completion_tokens exists on this wire; both are ignored silently.
	NumPredict *int     `json:"num_predict,omitzero"`
	Stop       []string `json:"stop,omitzero"`
}

func (o *options) empty() bool {
	return o.Temperature == nil && o.TopP == nil && o.NumPredict == nil && len(o.Stop) == 0
}

type message struct {
	Role string `json:"role"` // system | user | assistant | tool
	// Content is always emitted, including as "": the field is required, and
	// an assistant message that carries only tool calls still needs it.
	Content string `json:"content"`
	// Thinking is where a thinking model's reasoning is echoed back.
	Thinking string `json:"thinking,omitzero"`
	// Images are BARE base64 strings — no "data:image/png;base64," prefix.
	// The prefix is not rejected; it is decoded as image bytes and the request
	// fails deep inside the vision model.
	Images    []string   `json:"images,omitzero"`
	ToolCalls []toolCall `json:"tool_calls,omitzero"`
	// ToolName is a newer, optional field on a tool message. It is emitted
	// only when the compat profile says the server understands it — see
	// Compat.SupportsToolName.
	ToolName string `json:"tool_name,omitzero"`
}

type toolCall struct {
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON OBJECT on this wire, unlike Chat Completions where
	// it is a JSON string. The model's own bytes go here verbatim
	// (REQ-PROV-17); re-encoding them from a map would sort the keys and
	// change the text the model is conditioned on next turn.
	Arguments json.RawMessage `json:"arguments"`
}

type tool struct {
	Type     string       `json:"type"` // "function"
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitzero"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ------------------------------------------------------------------- compat

// Compat is the ollama-chat compatibility profile (REQ-PROV-12). Profiles are
// per-API and share no keys with the OpenAI, Anthropic or Google ones.
//
// Fields are pointers so "the catalog row said nothing" stays distinguishable
// from "the catalog row said false" (REQ-PROV-16.3).
type Compat struct {
	// SupportsThink gates the think field. Sending it to a model without
	// thinking support is a hard error from the server ("does not support
	// thinking"), so the level is dropped rather than guessed at.
	SupportsThink *bool `json:"supports_think"`
	// SupportsToolName gates tool_name on a tool result message. It defaults
	// to FALSE: the field is recent, and an older server rejects unknown
	// message fields rather than ignoring them. Turning it on is the only
	// partial fix for the positional-matching limitation described in
	// encodeMessages.
	SupportsToolName *bool `json:"supports_tool_name"`
	// KeepAlive overrides the server's model-residency default.
	KeepAlive string `json:"keep_alive"`
}

// DefaultCompat is the profile for a current Ollama server.
func DefaultCompat() Compat {
	t, f := true, false
	return Compat{SupportsThink: &t, SupportsToolName: &f}
}

// CompatFor resolves a model's profile from the catalog row's Compat, starting
// from the default and overriding key by key.
func CompatFor(m *core.Model) Compat {
	c := DefaultCompat()
	if m != nil && len(m.Compat) > 0 {
		_ = json.Unmarshal(m.Compat, &c)
	}
	return c
}

func (c Compat) supportsThink() bool    { return c.SupportsThink == nil || *c.SupportsThink }
func (c Compat) supportsToolName() bool { return c.SupportsToolName != nil && *c.SupportsToolName }

// ------------------------------------------------------------ id normalization

// NormalizeToolCallID rewrites an id into a shape this API tolerates:
// characters outside [A-Za-z0-9_-] become '_', truncated to 64.
//
// Ollama's native shape carries NO id on a tool call or a tool result, so no
// normalized id ever reaches this wire. The function still matters: REQ-PROV-11
// rule 5 rewrites ids in the repaired VIEW and rule 6 mints synthetic results
// keyed by them, so returning something non-injective here would drop a result
// before this package saw the transcript.
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

// Path is the native chat endpoint. It is NOT /v1/chat/completions — see the
// package doc for why that endpoint is not used.
const Path = "/api/chat"

// ------------------------------------------------------------ request building

// BuildRequest converts a canonical request into the /api/chat body. It is
// exported so the differential harness and the golden tests can capture the
// exact bytes with no network call and no server running (NFR-TEST-06.2).
//
// It runs the shared repair pass FIRST (REQ-PROV-11) — that is part of the
// provider contract, not the loop's, because the loop is not running when a
// transcript is loaded from disk.
func BuildRequest(m *core.Model, req core.Request) (*request, provider.RepairReport, error) {
	compat := CompatFor(m)
	repaired, rep := provider.RepairTranscript(req.Messages, provider.TargetFor(m, NormalizeToolCallID))

	out := &request{
		Model:     m.ID,
		Messages:  encodeMessages(repaired, req.System, compat),
		Stream:    true,
		KeepAlive: compat.KeepAlive,
	}

	opts := &options{Temperature: req.Temperature, TopP: req.TopP, Stop: req.StopSequences}
	if req.MaxTokens != nil {
		v := *req.MaxTokens
		opts.NumPredict = &v
	}
	if !opts.empty() {
		out.Options = opts
	}

	// Thinking is a boolean toggle here, not a budget and not an effort
	// string. An unset level emits nothing at all: absent, false and true are
	// three different requests (REQ-PROV-16.1).
	if compat.supportsThink() && req.ThinkingLevel != core.ThinkingUnset {
		v := req.ThinkingLevel != core.ThinkingOff
		out.Think = &v
	}

	for _, tw := range req.Tools {
		raw, err := json.Marshal(tw.InputSchema)
		if err != nil {
			return nil, rep, err
		}
		out.Tools = append(out.Tools, tool{Type: "function", Function: toolFunction{
			Name: tw.Name, Description: tw.Description, Parameters: raw,
		}})
	}

	// REQ-TOOL-16's tri-state has no wire form here: /api/chat has no
	// tool_choice field. ToolChoiceNone is honoured the only way this API
	// allows — by withholding the tools — because forwarding them and hoping
	// the model declines is exactly the "the provider invented a selection"
	// failure the requirement is about. ToolChoiceAuto is the server's
	// behaviour already, so it needs nothing.
	if req.ToolChoice == core.ToolChoiceNone {
		out.Tools = nil
	}

	return out, rep, nil
}

// encodeMessages is REQ-LOOP-02's Ollama half: it follows the OpenAI-compatible
// SHAPE — one {"role":"tool"} message per result — with one consequential
// difference.
//
// # There is no tool_call_id
//
// A tool result message carries content and nothing else. The server pairs
// results to the preceding assistant turn's tool_calls BY POSITION. Three
// consequences, all real:
//
//   - The ORDER of these messages is load-bearing. Emitting them in any order
//     but call order silently swaps the answers of two parallel calls.
//   - A partial batch cannot be expressed. REQ-PROV-11 rule 6's synthetic
//     results are what make this work at all: every call must be answered, in
//     order, or every later result is attributed to the wrong call.
//   - is_error has nowhere to go. It is rendered into the content text, which
//     is model-visible but not machine-readable.
//
// This is a fidelity limitation of the wire, not of the canonical transcript:
// ToolResultMessage keeps its ToolUseID, name, is_error and usage, and every
// other provider uses them. Setting Compat.SupportsToolName emits the newer
// tool_name field, which lets a modern server disambiguate by NAME — still not
// by identity, so two parallel calls to the SAME tool remain positional.
func encodeMessages(ms core.Messages, system []core.ContentBlock, compat Compat) []message {
	var out []message

	if len(system) > 0 {
		var sb strings.Builder
		for _, b := range system {
			if tb, ok := b.(core.TextBlock); ok {
				sb.WriteString(tb.Text)
			}
		}
		if sb.Len() > 0 {
			out = append(out, message{Role: "system", Content: sb.String()})
		}
	}

	for _, m := range ms {
		switch v := m.(type) {
		case core.UserMessage:
			text, images := splitContent(v.Content)
			out = append(out, message{Role: "user", Content: text, Images: images})

		case core.AssistantMessage:
			msg := message{Role: "assistant"}
			var text strings.Builder
			for _, b := range v.Content {
				switch bv := b.(type) {
				case core.TextBlock:
					text.WriteString(bv.Text)
				case core.ThinkingBlock:
					// Replayed under its own key, never merged into content:
					// concatenating reasoning into the answer teaches the
					// model to emit reasoning as answer text.
					msg.Thinking += bv.Thinking
				case core.ToolUseBlock:
					args := bv.Input
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					msg.ToolCalls = append(msg.ToolCalls, toolCall{
						Function: toolCallFunction{Name: bv.Name, Arguments: args},
					})
				}
			}
			msg.Content = text.String()
			out = append(out, msg)

		case core.ToolResultMessage:
			tm := message{Role: "tool", Content: resultText(v)}
			if compat.supportsToolName() {
				tm.ToolName = v.ToolName
			}
			out = append(out, tm)
		}
	}
	return out
}

// resultText renders a tool result into the single string this wire allows.
// An error result is marked in the text because the wire has no is_error flag
// and the model is the only consumer that can act on it.
func resultText(v core.ToolResultMessage) string {
	text, _ := splitContent(v.Content)
	if v.IsError && text != "" {
		return "Error: " + text
	}
	return text
}

// splitContent separates text from images: on this wire images are a
// message-level array of bare base64 strings, not content parts.
func splitContent(c core.Content) (string, []string) {
	var sb strings.Builder
	var images []string
	for _, b := range c {
		switch v := b.(type) {
		case core.TextBlock:
			sb.WriteString(v.Text)
		case core.ImageBlock:
			images = append(images, v.Data)
		}
	}
	return sb.String(), images
}

// ------------------------------------------------------------- finish reasons

// MapStopReason normalizes Ollama's done_reason. The provider's own string is
// preserved verbatim by the caller in RawStopReason; this mapping is lossy on
// purpose and never drives control flow (REQ-LOOP-01).
//
// Ollama emits done_reason "stop" on a turn whose message carries tool_calls —
// there is no "tool_calls" reason on this wire at all — so a loop gated on the
// reason would execute none of them and return an empty answer. hasToolCalls
// is therefore a parameter, and it wins.
//
// No input maps to StopReasonError, including an unrecognized one and
// including "load"/"unload" (model residency events, not failures): ruling
// P-41, and because StopReasonError short-circuits the loop AND makes
// REQ-PROV-11 rule 2 drop the whole turn from the next request.
func MapStopReason(doneReason string, hasToolCalls bool) core.StopReason {
	switch doneReason {
	case "length":
		return core.StopReasonLength
	case "stop", "", "load", "unload":
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

// MapFinishReason is MapStopReason under the name the other providers use, so
// a caller writing generic code does not have to remember which wire calls the
// field done_reason.
func MapFinishReason(doneReason string, hasToolCalls bool) core.StopReason {
	return MapStopReason(doneReason, hasToolCalls)
}
