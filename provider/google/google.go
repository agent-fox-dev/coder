// Package google implements the Gemini generateContent wire API.
//
// It is keyed by WIRE API, not by vendor (REQ-PROV-02): the same
// implementation serves Google AI Studio and Vertex AI, which differ in host,
// path prefix and credential, never in body shape.
//
// # Why this package is the third data point for REQ-LOOP-02
//
// Gemini agrees with NEITHER existing provider, so the canonical transcript
// cannot have been designed around either one:
//
//   - Messages are contents:[{role,parts}] and the assistant role is spelled
//     "model". Sending "assistant" is not a validation error the SDK can see:
//     the request is accepted and the turn is misattributed.
//   - A tool call is a functionCall PART and a tool result is a
//     functionResponse PART. Results are grouped into ONE user-role content —
//     like Anthropic in grouping, unlike Anthropic in shape, and unlike OpenAI
//     which mandates one message per result.
//   - The system prompt is systemInstruction, a top-level field, not a
//     message with a role.
//   - Tools are a single wrapper object holding every declaration, not a flat
//     array of tools.
//
// A canonical layer that had baked in either existing shape could not produce
// this body at all.
package google

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/schema"
)

// API is the wire API id.
const API core.API = "google-generative-ai"

// RoleUser and RoleModel are the only two content roles Gemini accepts.
//
// They are constants rather than string literals at the call sites because
// "assistant" — the canonical spelling, and the spelling both other first-party
// providers use — is silently wrong here: the request is accepted and the turn
// is attributed to the user, so the model is conditioned on its own output as
// if the human had written it. There is no 400 to notice.
const (
	RoleUser  = "user"
	RoleModel = "model"
)

// ---------------------------------------------------------------- wire types
//
// Every optional scalar below is a pointer with `omitzero`, never a bare value
// with `omitempty` (REQ-PROV-16). The difference is load-bearing twice in this
// file: `omitempty` on a bare float64 drops an explicit temperature of 0, and
// `omitempty` on a bare int drops thinkingBudget:0 — which is the ONLY way to
// turn Gemini's thinking off, so the request that meant "no thinking" would
// arrive as "default (dynamic) thinking" and be billed for it.

type request struct {
	// There is no Model field on purpose: Gemini takes the model as a URL
	// segment (see Path), not as a body field. A body field named "model" is
	// ignored, so an implementation copied from the OpenAI shape sends the
	// model nowhere and silently talks to whatever the URL named.
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitzero"`
	Tools             []toolSet         `json:"tools,omitzero"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitzero"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitzero"`
	// CachedContent references an explicit CachedContent resource (§6.2a
	// Gemini). Creating that resource is out of scope for request building.
	CachedContent string `json:"cachedContent,omitzero"`
}

type content struct {
	// Role is omitted on systemInstruction, which is a content with parts and
	// no role.
	Role  string `json:"role,omitzero"`
	Parts []part `json:"parts"`
}

// part is the wire content part. One struct with omitted fields rather than a
// union, because that is the shape Gemini accepts and the canonical union has
// already been resolved by the time we get here.
type part struct {
	Text             string            `json:"text,omitzero"`
	InlineData       *inlineData       `json:"inlineData,omitzero"`
	FunctionCall     *functionCall     `json:"functionCall,omitzero"`
	FunctionResponse *functionResponse `json:"functionResponse,omitzero"`
	// Thought marks a part as a thought summary rather than an answer.
	Thought bool `json:"thought,omitzero"`
	// ThoughtSignature is opaque and provider-issued. It rides on the part
	// that carries the thought AND on the functionCall part of a thinking
	// turn; REQ-PROV-11 rule 3 has already stripped it on a cross-model
	// replay, so anything still here is safe to send.
	ThoughtSignature string `json:"thoughtSignature,omitzero"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64, no data: URL prefix
}

type functionCall struct {
	Name string `json:"name"`
	// Args is the model's own argument bytes, verbatim (REQ-PROV-17). It is a
	// JSON OBJECT on this wire, not a string as on Chat Completions.
	Args json.RawMessage `json:"args"`
}

type functionResponse struct {
	// Name is how Gemini pairs a result with its call: there is no
	// tool_call_id in this wire shape. See encodeContents.
	Name string `json:"name"`
	// Response must be a JSON OBJECT. A bare string here is a 400.
	Response json.RawMessage `json:"response"`
}

// toolSet is the wrapper REQ-PROV-02's table implies but no other provider
// has: ONE object holding every declaration, rather than one object per tool.
// Emitting a flat array of {name,description,parameters} is a 400 on every
// request that carries a tool.
type toolSet struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

type functionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitzero"`
	Parameters  json.RawMessage `json:"parameters,omitzero"`
}

type toolConfig struct {
	FunctionCallingConfig functionCallingConfig `json:"functionCallingConfig"`
}

type functionCallingConfig struct {
	Mode string `json:"mode"` // AUTO | NONE | ANY
}

type generationConfig struct {
	Temperature     *float64        `json:"temperature,omitzero"`
	TopP            *float64        `json:"topP,omitzero"`
	MaxOutputTokens *int            `json:"maxOutputTokens,omitzero"`
	StopSequences   []string        `json:"stopSequences,omitzero"`
	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitzero"`
}

func (g *generationConfig) empty() bool {
	return g.Temperature == nil && g.TopP == nil && g.MaxOutputTokens == nil &&
		len(g.StopSequences) == 0 && g.ThinkingConfig == nil
}

type thinkingConfig struct {
	IncludeThoughts *bool `json:"includeThoughts,omitzero"`
	// ThinkingBudget is a POINTER so an explicit 0 — "do not think" — is
	// emitted rather than omitted (REQ-PROV-16.1).
	ThinkingBudget *int `json:"thinkingBudget,omitzero"`
}

// ------------------------------------------------------------------- compat

// Compat is the google-generative-ai compatibility profile (REQ-PROV-12).
// Profiles are per-API and share no keys with the OpenAI or Anthropic ones.
//
// Every field is a pointer so that "the catalog row said nothing" is
// distinguishable from "the catalog row said false" (REQ-PROV-16.3): the
// default for CanDisableThinking is TRUE, and Go's false zero value must never
// stand in for it.
type Compat struct {
	// CanDisableThinking is REQ-PROV-15's per-family disable capability.
	//
	// Some Gemini families (the 2.5 Pro line) CANNOT disable thinking: they
	// take a floor budget instead of a zero one, and thinkingBudget:0 is
	// rejected. A blanket zero budget is therefore wrong as a default — it is
	// the difference between "thinking off everywhere" and "a hard 400 on
	// exactly the models a user reaches for when they want thinking".
	CanDisableThinking *bool `json:"can_disable_thinking"`
	// MinThinkingBudget is the floor used for ThinkingOff on a family that
	// cannot disable thinking, and the lower clamp for every other level.
	MinThinkingBudget *int `json:"min_thinking_budget"`
	// MaxThinkingBudget is the family's ceiling; a budget above it is a 400.
	MaxThinkingBudget *int `json:"max_thinking_budget"`
	// IncludeThoughts requests thought summaries in the response. Default true
	// when thinking is on: without them a thinking turn streams nothing until
	// the answer begins.
	IncludeThoughts *bool `json:"include_thoughts"`
}

// DefaultCompat is the profile for a family that behaves like Gemini 2.5
// Flash: thinking can be turned off, and there is no floor.
func DefaultCompat() Compat {
	t := true
	return Compat{CanDisableThinking: &t, IncludeThoughts: &t}
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

func (c Compat) canDisableThinking() bool {
	return c.CanDisableThinking == nil || *c.CanDisableThinking
}

func (c Compat) includeThoughts() bool {
	return c.IncludeThoughts == nil || *c.IncludeThoughts
}

// DefaultThinkingBudgets is the fallback token-budget table, used only for a
// level the model's own ThinkingLevelMap does not price. The catalog row is
// authoritative (REQ-PROV-15, REQ-CAT-06); this table exists so a hand-built
// Model with no map still produces a sendable request instead of silently
// dropping the thinking level the caller asked for.
func DefaultThinkingBudgets() map[core.ThinkingLevel]int {
	return map[core.ThinkingLevel]int{
		core.ThinkingMinimal: 512,
		core.ThinkingLow:     2048,
		core.ThinkingMedium:  8192,
		core.ThinkingHigh:    16384,
		core.ThinkingXHigh:   24576,
		core.ThinkingMax:     32768,
	}
}

// resolveThinking is REQ-PROV-15's Google arm.
//
// Three outcomes, and the difference between them is the whole requirement:
//
//	unset  -> no thinkingConfig at all (absent is not the same as zero)
//	off    -> budget 0, or the family's FLOOR when it cannot disable thinking
//	level  -> the model's own budget for that level
//
// The level is assumed already clamped by the caller (REQ-PROV-15's
// ClampThinkingLevel); this function only prices it.
func resolveThinking(m *core.Model, level core.ThinkingLevel, c Compat) *thinkingConfig {
	if level == core.ThinkingUnset {
		return nil
	}
	floor := 0
	if c.MinThinkingBudget != nil {
		floor = *c.MinThinkingBudget
	}

	if level == core.ThinkingOff {
		budget := 0
		if !c.canDisableThinking() {
			// The floor, not zero. Sending 0 to a family that cannot disable
			// thinking is rejected outright, so "off" degrades to "as little
			// as this model will do" rather than to a failed request.
			budget = floor
		}
		return &thinkingConfig{ThinkingBudget: &budget}
	}

	budget := budgetFor(m, level)
	if budget >= 0 && budget < floor {
		budget = floor
	}
	if c.MaxThinkingBudget != nil && budget > *c.MaxThinkingBudget {
		budget = *c.MaxThinkingBudget
	}
	tc := &thinkingConfig{ThinkingBudget: &budget}
	if c.includeThoughts() {
		t := true
		tc.IncludeThoughts = &t
	}
	return tc
}

// budgetFor reads the model's own wire value for a level, falling back to the
// default table. A wire value of "-1" is Gemini's "dynamic" budget and is
// passed through as such.
func budgetFor(m *core.Model, level core.ThinkingLevel) int {
	if m != nil && m.ThinkingLevelMap != nil {
		if w, ok := m.ThinkingLevelMap[level]; ok && w != nil {
			if n, err := strconv.Atoi(strings.TrimSpace(*w)); err == nil {
				return n
			}
		}
	}
	if n, ok := DefaultThinkingBudgets()[level]; ok {
		return n
	}
	return DefaultThinkingBudgets()[core.ThinkingMedium]
}

// ------------------------------------------------------------ id normalization

// NormalizeToolCallID rewrites an id into a shape Gemini tolerates:
// characters outside [A-Za-z0-9_.-] become '_', truncated to 64. It is the
// function REQ-PROV-11 rule 5 uses when replaying a transcript produced by
// another provider.
//
// Gemini's classic generateContent shape carries NO id on a functionCall or a
// functionResponse — pairing is by name and position — so the normalized id
// never reaches this wire. It still matters: rule 5 rewrites ids in the
// repaired VIEW, and rule 6 mints synthetic results keyed by them, so a
// normalizer that collided two distinct calls onto one id would drop a result
// before this package ever saw the transcript.
func NormalizeToolCallID(s string) string {
	if s == "" {
		return s
	}
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			return r
		}
		return '_'
	}, s)
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// Path returns the request path for a model. The model id is a URL SEGMENT on
// this API, which is why request carries no model field.
//
// The streaming form appends ?alt=sse: without it, streamGenerateContent
// answers with one enormous JSON ARRAY delivered incrementally, not SSE, and
// an SSE reader waits for a data: line that never arrives.
func Path(m *core.Model, stream bool) string {
	id := strings.TrimPrefix(m.ID, "models/")
	if stream {
		return "/v1beta/models/" + id + ":streamGenerateContent?alt=sse"
	}
	return "/v1beta/models/" + id + ":generateContent"
}

// ------------------------------------------------------------ request building

// BuildRequest converts a canonical request into the Gemini generateContent
// body. It is exported so the differential harness and the golden tests can
// capture the exact bytes with no network call and no API key (NFR-TEST-06.2).
//
// It runs the shared repair pass FIRST (REQ-PROV-11) — that is part of the
// provider contract, not the loop's, because the loop is not running when a
// transcript is loaded from disk.
func BuildRequest(m *core.Model, req core.Request) (*request, provider.RepairReport, error) {
	compat := CompatFor(m)
	repaired, rep := provider.RepairTranscript(req.Messages, provider.TargetFor(m, NormalizeToolCallID))

	out := &request{Contents: encodeContents(repaired)}

	// The system prompt is a top-level field, NOT a message. Prepending it as
	// a content with role "system" is a 400 (there is no such role), and
	// prepending it as a user content silently makes it look like the first
	// human turn.
	if sys := encodeSystem(req.System); sys != nil {
		out.SystemInstruction = sys
	}

	gc := &generationConfig{
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.StopSequences,
	}
	if req.MaxTokens != nil {
		v := *req.MaxTokens
		gc.MaxOutputTokens = &v
	}
	gc.ThinkingConfig = resolveThinking(m, req.ThinkingLevel, compat)
	if !gc.empty() {
		out.GenerationConfig = gc
	}

	// ONE wrapper object holding every declaration (see toolSet).
	if len(req.Tools) > 0 {
		decls := make([]functionDeclaration, 0, len(req.Tools))
		for _, tw := range req.Tools {
			params, err := ConvertSchema(tw.InputSchema)
			if err != nil {
				return nil, rep, err
			}
			decls = append(decls, functionDeclaration{
				Name: tw.Name, Description: tw.Description, Parameters: params,
			})
		}
		out.Tools = []toolSet{{FunctionDeclarations: decls}}
	}

	// An absent ToolChoice is NOT auto: a provider must not invent a selection
	// when the caller expressed none (REQ-TOOL-16).
	switch req.ToolChoice {
	case core.ToolChoiceAuto:
		out.ToolConfig = &toolConfig{FunctionCallingConfig: functionCallingConfig{Mode: "AUTO"}}
	case core.ToolChoiceNone:
		out.ToolConfig = &toolConfig{FunctionCallingConfig: functionCallingConfig{Mode: "NONE"}}
	}

	return out, rep, nil
}

func encodeSystem(blocks []core.ContentBlock) *content {
	var sb strings.Builder
	for _, b := range blocks {
		if tb, ok := b.(core.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	if sb.Len() == 0 {
		return nil
	}
	// systemInstruction is a content with parts and NO role.
	return &content{Parts: []part{{Text: sb.String()}}}
}

// encodeContents is REQ-LOOP-02's Gemini half.
//
// Each ToolResultMessage becomes ONE functionResponse PART, and consecutive
// results land in a single {"role":"user"} content. That is a third distinct
// shape: Anthropic groups them as tool_result BLOCKS, OpenAI cannot group them
// at all, and Gemini groups them as PARTS of a user turn whose role name is
// not even "tool".
//
// Two consequences worth naming:
//
//   - The result is paired to its call BY NAME AND POSITION, because this wire
//     carries no tool_call_id. The canonical ToolUseID is therefore invisible
//     here, and the ORDER of the functionResponse parts must match the order
//     of the functionCall parts. Emitting them in map order, or sorting them,
//     silently swaps the answers of two parallel calls to the same tool.
//   - A ToolResultMessage whose ToolName is empty cannot be encoded from
//     itself. The name is recovered from the tool_use it answers, which is why
//     this function indexes the transcript first.
func encodeContents(ms core.Messages) []content {
	// id -> tool name, from the calls, for results that carry no name.
	nameByID := make(map[string]string)
	for _, m := range ms {
		if am, ok := m.(core.AssistantMessage); ok {
			for _, b := range am.Content {
				if tu, ok := b.(core.ToolUseBlock); ok {
					nameByID[tu.ID] = tu.Name
				}
			}
		}
	}

	var out []content
	appendTo := func(role string, parts ...part) {
		if len(parts) == 0 {
			return
		}
		// Coalesce adjacent same-role contents. Gemini expects alternating
		// turns; two adjacent user contents are what compaction produces on
		// its first request (a summary UserMessage prepended to a suffix that
		// begins with one) and what a tool-result run followed by a user
		// message produces every time (ruling P-5).
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Parts = append(out[n-1].Parts, parts...)
			return
		}
		out = append(out, content{Role: role, Parts: parts})
	}

	for _, m := range ms {
		switch v := m.(type) {
		case core.UserMessage:
			appendTo(RoleUser, encodeParts(v.Content, nameByID)...)

		case core.AssistantMessage:
			p := encodeParts(v.Content, nameByID)
			if len(p) == 0 {
				// Gemini rejects a content with an empty parts array.
				continue
			}
			appendTo(RoleModel, p...)

		case core.ToolResultMessage:
			name := v.ToolName
			if name == "" {
				name = nameByID[v.ToolUseID]
			}
			appendTo(RoleUser, part{FunctionResponse: &functionResponse{
				Name: name, Response: responseObject(v),
			}})
		}
	}
	return out
}

// encodeParts translates one canonical content list. nameByID resolves the
// tool name of a nested ToolResultBlock, which carries an id and no name while
// this wire needs the name and has no id field at all.
func encodeParts(c core.Content, nameByID map[string]string) []part {
	out := make([]part, 0, len(c))
	for _, b := range c {
		switch v := b.(type) {
		case core.TextBlock:
			if v.Text == "" {
				continue
			}
			out = append(out, part{Text: v.Text})
		case core.ThinkingBlock:
			if v.Redacted {
				// A redacted block is an opaque Anthropic construct with no
				// Gemini representation. Dropping it is the only option that
				// is not a 400; REQ-PROV-11 rule 3 has already dropped it on
				// every cross-model path, so this arm only fires for a
				// same-model transcript carrying one.
				continue
			}
			if v.Thinking == "" && v.Signature == "" {
				continue
			}
			out = append(out, part{
				Text: v.Thinking, Thought: true, ThoughtSignature: v.Signature,
			})
		case core.ToolUseBlock:
			args := v.Input
			if len(args) == 0 {
				// args must be an object; an absent one is a 400.
				args = json.RawMessage("{}")
			}
			out = append(out, part{
				// The model's own bytes, written into the args position
				// UNCHANGED — no decode-and-re-encode round trip, which would
				// sort the keys and change the text the model is conditioned
				// on next turn (REQ-PROV-17, REQ-TOOL-12).
				FunctionCall:     &functionCall{Name: v.Name, Args: args},
				ThoughtSignature: v.ThoughtSignature,
			})
		case core.ImageBlock:
			out = append(out, part{InlineData: &inlineData{MimeType: v.MimeType, Data: v.Data}})
		case core.ToolResultBlock:
			out = append(out, part{FunctionResponse: &functionResponse{
				Name:     nameByID[v.ToolUseID],
				Response: responseBytes(v.Content, v.IsError),
			}})
		}
	}
	return out
}

// responseObject renders a tool result into the OBJECT that
// functionResponse.response must be.
//
// Gemini rejects a bare string here, so the naive `response: result.Text()`
// 400s on every tool call — the single easiest way to get this provider wrong.
// A result whose text is already a JSON object is passed through VERBATIM,
// which keeps the tool's own key order intact rather than re-wrapping and
// re-sorting it.
func responseObject(v core.ToolResultMessage) json.RawMessage {
	return responseBytes(v.Content, v.IsError)
}

func responseBytes(c core.Content, isErr bool) json.RawMessage {
	text := c.Text()
	if isErr {
		b, _ := json.Marshal(map[string]string{"error": text})
		return b
	}
	if obj := objectBytes(text); obj != nil {
		return obj
	}
	b, _ := json.Marshal(map[string]string{"output": text})
	return b
}

func objectBytes(s string) json.RawMessage {
	t := strings.TrimSpace(s)
	if len(t) < 2 || t[0] != '{' || t[len(t)-1] != '}' || !json.Valid([]byte(t)) {
		return nil
	}
	return json.RawMessage(t)
}

// ------------------------------------------------------------- schema dialect

// geminiStripped are the JSON Schema keywords Gemini's dialect does not accept.
// Sending any of them is a 400 on every request that carries the tool, so they
// are removed from Extra as well as from the modelled fields — Extra is exactly
// how "$schema" arrives from a hand-authored schema.
var geminiStripped = map[string]bool{
	"additionalProperties":  true,
	"$schema":               true,
	"const":                 true,
	"$ref":                  true,
	"$defs":                 true,
	"definitions":           true,
	"not":                   true,
	"patternProperties":     true,
	"unevaluatedProperties": true,
}

// ConvertSchema translates a canonical schema into Gemini's dialect.
//
// Three things this must do that no other provider needs, each of which is a
// 400 on every tool-carrying request when skipped:
//
//  1. Type names are UPPERCASE: STRING, INTEGER, NUMBER, BOOLEAN, OBJECT,
//     ARRAY. Gemini's enum has no lowercase members.
//  2. additionalProperties, $schema and const are rejected outright and are
//     stripped. Stripping const LOSES a constraint; it is not translated into
//     a one-value enum because that is only expressible for strings and would
//     turn `const: null` — the case schema.Schema exists to represent — into
//     something else entirely.
//  3. Nullability is a `nullable: true` FLAG, not a type array. The canonical
//     encoder emits "type":["string","null"], which this dialect rejects.
//
// It is exported because the translation is the interesting part of this
// provider and deserves a direct test, not one mediated by a whole request.
func ConvertSchema(s *schema.Schema) (json.RawMessage, error) {
	if s == nil {
		return nil, nil
	}
	var b strings.Builder
	if err := writeSchema(&b, s); err != nil {
		return nil, err
	}
	return json.RawMessage(b.String()), nil
}

func writeSchema(b *strings.Builder, s *schema.Schema) error {
	if s == nil {
		b.WriteString("null")
		return nil
	}
	b.WriteByte('{')
	first := true
	kv := func(k string, raw string) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		km, _ := json.Marshal(k)
		b.Write(km)
		b.WriteByte(':')
		b.WriteString(raw)
	}
	str := func(k, v string) {
		if v == "" {
			return
		}
		vm, _ := json.Marshal(v)
		kv(k, string(vm))
	}

	if t := upperType(s.Type); t != "" {
		tm, _ := json.Marshal(t)
		kv("type", string(tm))
	}
	str("description", s.Description)
	str("title", s.Title)
	if s.Nullable {
		kv("nullable", "true")
	}
	if len(s.Enum) > 0 {
		var eb strings.Builder
		eb.WriteByte('[')
		for i, e := range s.Enum {
			if i > 0 {
				eb.WriteByte(',')
			}
			eb.Write(e)
		}
		eb.WriteByte(']')
		kv("enum", eb.String())
	}
	if s.Items != nil {
		var ib strings.Builder
		if err := writeSchema(&ib, s.Items); err != nil {
			return err
		}
		kv("items", ib.String())
	}
	order := s.PropertyList()
	if len(order) > 0 {
		var pb strings.Builder
		pb.WriteByte('{')
		for i, name := range order {
			if i > 0 {
				pb.WriteByte(',')
			}
			nm, _ := json.Marshal(name)
			pb.Write(nm)
			pb.WriteByte(':')
			if err := writeSchema(&pb, s.Properties[name]); err != nil {
				return err
			}
		}
		pb.WriteByte('}')
		kv("properties", pb.String())
		// propertyOrdering is a Gemini extension and the only way to keep
		// authored property order model-visible: the object above preserves it
		// on the wire, but the service does not promise to.
		om, _ := json.Marshal(order)
		kv("propertyOrdering", string(om))
	}
	if len(s.Required) > 0 {
		rm, _ := json.Marshal(s.Required)
		kv("required", string(rm))
	}
	// Gemini accepts anyOf and neither oneOf nor allOf. oneOf is rewritten to
	// anyOf rather than dropped: dropping the alternatives leaves a schema
	// with no type at all, which constrains nothing.
	alts := append(append([]*schema.Schema(nil), s.AnyOf...), s.OneOf...)
	if len(alts) > 0 {
		var ab strings.Builder
		ab.WriteByte('[')
		for i, a := range alts {
			if i > 0 {
				ab.WriteByte(',')
			}
			if err := writeSchema(&ab, a); err != nil {
				return err
			}
		}
		ab.WriteByte(']')
		kv("anyOf", ab.String())
	}
	for _, n := range []struct {
		k string
		v *int
	}{{"minItems", s.MinItems}, {"maxItems", s.MaxItems},
		{"minLength", s.MinLength}, {"maxLength", s.MaxLength}} {
		if n.v != nil {
			kv(n.k, strconv.Itoa(*n.v))
		}
	}
	for _, n := range []struct {
		k string
		v *float64
	}{{"minimum", s.Minimum}, {"maximum", s.Maximum}} {
		if n.v != nil {
			kv(n.k, strconv.FormatFloat(*n.v, 'g', -1, 64))
		}
	}
	str("pattern", s.Pattern)
	str("format", s.Format)
	// Extra last, in authored order, minus the keywords this dialect rejects.
	for _, m := range s.Extra {
		if geminiStripped[m.Key] {
			continue
		}
		raw, err := m.Value.MarshalJSON()
		if err != nil {
			return err
		}
		kv(m.Key, string(raw))
	}
	b.WriteByte('}')
	return nil
}

func upperType(t schema.Type) string {
	switch t {
	case schema.TypeString:
		return "STRING"
	case schema.TypeInteger:
		return "INTEGER"
	case schema.TypeNumber:
		return "NUMBER"
	case schema.TypeBoolean:
		return "BOOLEAN"
	case schema.TypeObject:
		return "OBJECT"
	case schema.TypeArray:
		return "ARRAY"
	}
	// TypeNull has no Gemini spelling; nullability is the `nullable` flag.
	return ""
}

// ------------------------------------------------------------- finish reasons

// MapFinishReason normalizes a Gemini finishReason. The provider's own string
// is preserved verbatim by the caller in RawStopReason; this mapping is lossy
// on purpose and never drives control flow (REQ-LOOP-01).
//
// The STOP-with-functionCall case is the reason REQ-LOOP-01 exists at all:
// Gemini routinely returns finishReason "STOP" on a turn whose parts contain
// functionCall entries. A loop gated on the finish reason executes none of
// them and returns an empty answer, and that bug passes every Anthropic-only
// test. Here it is reported honestly as tool_use.
//
// No input maps to StopReasonError, including an unrecognized one (ruling
// P-41) and including MALFORMED_FUNCTION_CALL: StopReasonError short-circuits
// the loop before tool extraction AND causes REQ-PROV-11 rule 2 to drop the
// whole turn from the next request, so mis-reporting an unknown reason as an
// error deletes a turn the model actually produced.
func MapFinishReason(reason string, hasFunctionCalls bool) core.StopReason {
	switch reason {
	case "STOP":
		if hasFunctionCalls {
			return core.StopReasonToolUse
		}
		return core.StopReasonStop
	case "MAX_TOKENS":
		// Reported even alongside function calls: a truncated turn's tool
		// calls must not be executed (§ "max_tokens with tool calls"), and
		// this is the only signal that says so.
		return core.StopReasonLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "LANGUAGE":
		return core.StopReasonRefusal
	}
	// Unknown, absent, OTHER, MALFORMED_FUNCTION_CALL: infer from content.
	if hasFunctionCalls {
		return core.StopReasonToolUse
	}
	return core.StopReasonStop
}

// MapStopReason is MapFinishReason for a caller that has not yet inspected the
// parts. It exists so the name is the same across providers; prefer
// MapFinishReason, which can see the tool calls.
func MapStopReason(reason string) core.StopReason { return MapFinishReason(reason, false) }
