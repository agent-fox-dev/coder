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
	"strconv"
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

	// thinkingBudget is REQ-PROV-15's last bullet: an endpoint that shares
	// max_tokens between the reasoning and the answer needs an explicit
	// budget, or a reasoning-heavy turn spends the whole response thinking and
	// emits no answer. The FIELD NAME is the compat profile's
	// ThinkingTokenBudgetField (REQ-PROV-12) — vLLM spells it
	// thinking_token_budget, Qwen thinking_budget, llama.cpp
	// thinking_budget_tokens — so it cannot be a struct tag, and a
	// map[string]any body would sort every key (REQ-PROV-16.5). It is spliced
	// in by MarshalJSON instead, which keeps the rest of the body's authored
	// order intact.
	thinkingBudgetField string
	thinkingBudget      *int
}

// MarshalJSON appends the profile-named reasoning budget to the encoded body.
// Unexported fields are invisible to the alias, so the alias marshal is the
// ordinary one and the only difference is the appended key.
func (r *request) MarshalJSON() ([]byte, error) {
	type alias request
	raw, err := json.Marshal((*alias)(r))
	if err != nil || r.thinkingBudgetField == "" || r.thinkingBudget == nil {
		return raw, err
	}
	key, err := json.Marshal(r.thinkingBudgetField)
	if err != nil {
		return nil, err
	}
	// raw always ends in '}' and is never "{}": model is required.
	out := append(raw[:len(raw)-1], ',')
	out = append(out, key...)
	out = append(out, ':')
	out = strconv.AppendInt(out, int64(*r.thinkingBudget), 10)
	return append(out, '}'), nil
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

	// deferred marks the REQ-CACHE-10 declaration message. It sits AFTER the
	// cached prefix, so it must never carry a §6.2a breakpoint — the same rule
	// that keeps cache_control off Anthropic's deferred tools, for the same
	// reason: a breakpoint there sits past the very content the deferral
	// exists to keep cached. Unexported, so it never reaches the wire.
	deferred bool
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
	// tool result gets a synthetic assistant turn between them
	// (encodeMessages). The gateways that need it are named in InferCompat.
	AllowsUserAfterToolResult bool `json:"allows_user_after_tool_result"`
	// RequiresToolResultName: some gateways require name on a tool message.
	RequiresToolResultName bool `json:"requires_tool_result_name"`
	// ThinkingFormat is the wire shape for REASONING REPLAY: openai,
	// deepseek, together, openrouter or chat-template. Only `deepseek`
	// documents a round trip — reasoning_content echoed back on the assistant
	// message — and only that arm emits one; see encodeMessages for what the
	// others do and why they can do nothing else.
	ThinkingFormat string `json:"thinking_format"`
	// ThinkingTokenBudgetField names the reasoning-budget field on servers
	// that share max_tokens between reasoning and the answer (REQ-PROV-15's
	// last bullet): thinking_token_budget on vLLM, thinking_budget on Qwen,
	// thinking_budget_tokens on llama.cpp. Empty emits no budget.
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
	base := ""
	if m != nil {
		base = m.BaseURL
	}
	return CompatForBase(m, base)
}

// CompatForBase is CompatFor against the base URL the request will actually
// go to — the environment override applied (OPENAI_BASE_URL and its vendor
// siblings, provider.ResolveBaseURL's precedence) — rather than the catalog
// row's. The provider uses this form: an operator who points an `openai` row
// at a DeepSeek or Together host through the environment is talking to that
// host, and inferring the profile from the row's URL sends it api.openai.com's
// `store` and developer role, which those hosts reject.
func CompatForBase(m *core.Model, base string) (Compat, error) {
	c := inferCompat(m, base)
	if m == nil || len(m.Compat) == 0 {
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
// model gets before its catalog row says anything, read from the row's own
// BaseURL. See CompatForBase for the resolved form.
//
// Every departure from DefaultCompat below is a row of the REQ-PROV-12 table.
// "OpenAI-compatible" is not a base-URL swap: the first request to any of
// these hosts with the api.openai.com profile 400s on `store`, or on the
// developer role, or on max_completion_tokens.
func InferCompat(m *core.Model) Compat {
	if m == nil {
		return DefaultCompat()
	}
	return inferCompat(m, m.BaseURL)
}

func inferCompat(m *core.Model, base string) Compat {
	c := DefaultCompat()
	if m == nil {
		return c
	}
	host := strings.ToLower(hostOf(base))
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
			// A role:"tool" message is translated into an Anthropic
			// tool_result, which rides on a USER turn — so a user message
			// straight after one is two consecutive user turns upstream, which
			// the Messages API rejects (REQ-PROV-12
			// AllowsUserAfterToolResult).
			c.AllowsUserAfterToolResult = false
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
	case vendor == "mistral" || strings.HasSuffix(host, "api.mistral.ai"):
		c.UseMaxTokens = true
		c.SupportsReasoningEffort = false
		c.SupportsStrictTools = false
		// Mistral rejects a user message that follows a tool result outright
		// ("Conversation roles must alternate"), so one is bridged with a
		// synthetic assistant turn (REQ-PROV-12 AllowsUserAfterToolResult).
		c.AllowsUserAfterToolResult = false
	case vendor == "vllm" || vendor == "sglang":
		// A self-hosted OpenAI-compatible server shares ONE max_tokens between
		// the reasoning and the answer, so a reasoning-heavy turn with no
		// budget returns thinking and no answer (REQ-PROV-15). vLLM reads the
		// budget as thinking_token_budget and has no reasoning_effort at all.
		c.SupportsReasoningEffort = false
		c.ThinkingTokenBudgetField = "thinking_token_budget"
		c.ThinkingFormat = "chat-template"
		c.SupportsStrictTools = false
	case vendor == "llamacpp" || vendor == "llama.cpp" || vendor == "llama-cpp":
		c.SupportsReasoningEffort = false
		c.ThinkingTokenBudgetField = "thinking_budget_tokens"
		c.ThinkingFormat = "chat-template"
		c.SupportsStrictTools = false
	case vendor == "qwen" || vendor == "dashscope" || strings.Contains(host, "dashscope"):
		c.UseMaxTokens = true
		c.SupportsReasoningEffort = false
		c.ThinkingTokenBudgetField = "thinking_budget"
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
	out, rep, _, _, err := buildRequest(m, req, retention, nil, m.BaseURL)
	return out, rep, err
}

// BuildRequestCached is BuildRequest with REQ-CACHE-06's per-session schema
// cache attached. A nil prefix marshals every schema, which is what
// BuildRequest does and what a one-shot caller wants.
//
// NFR-PERF-03 is why this is on the request path rather than an internal
// detail: "tool schema serialization must be computed once per session and
// cached, not recomputed on every model call", and this wire was
// re-serializing every schema on every turn for bytes that had not changed.
func BuildRequestCached(m *core.Model, req core.Request, retention core.CacheRetention,
	prefix *provider.ToolPrefix) (*request, provider.RepairReport, provider.SyncReport, error) {
	out, rep, _, sync, err := buildRequest(m, req, retention, prefix, m.BaseURL)
	return out, rep, sync, err
}

// buildRequest builds against base, the URL the request will go to: the
// compat profile is inferred from THAT host (CompatForBase), not the catalog
// row's.
func buildRequest(m *core.Model, req core.Request, retention core.CacheRetention,
	prefix *provider.ToolPrefix, base string) (*request, provider.RepairReport, Compat, provider.SyncReport, error) {
	var sync provider.SyncReport
	compat, err := CompatForBase(m, base)
	if err != nil {
		return nil, provider.RepairReport{}, compat, sync, err
	}
	repaired, rep := provider.RepairTranscript(req.Messages, provider.TargetFor(m, NormalizeToolCallID))

	// REQ-CACHE-06/NFR-PERF-03: ONE Sync over the WHOLE tool list, before the
	// REQ-CACHE-10 split. Syncing the two halves separately would make each
	// call see the other half as removed — a reported prefix invalidation on
	// every turn, evicting the entries the cache exists to keep.
	schemas, strict, srep, err := encodeSchemas(req.Tools, compat.SupportsStrictTools, prefix)
	if err != nil {
		return nil, rep, compat, sync, err
	}
	sync = srep
	split := provider.SplitDeferredTools(req.Tools, req.Messages)

	out := &request{
		Model:    m.ID,
		Messages: encodeMessages(repaired, req.System, compat, deferredDeclaration(split.Deferred, schemas, req.Tools, compat)),
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
	applyThinkingBudget(out, m, req.ThinkingLevel, compat)

	// REQ-CACHE-10, third arm: this wire has no defer_loading (nor does the
	// Responses API have the additional_tools the PRD named, ruling L-10), so
	// a tool that appeared mid-session is WITHHELD from the tools array here
	// and re-declared in a system message at its transcript position
	// (deferredDeclaration above).
	// Prepending it to the array instead would rewrite the cached prefix and
	// cost the whole provider-side cache over one added tool.
	byName := make(map[string]int, len(req.Tools))
	for i, tw := range req.Tools {
		byName[tw.Name] = i
	}
	for _, tw := range split.Immediate {
		i := byName[tw.Name]
		f := toolFunction{Name: tw.Name, Description: tw.Description, Parameters: schemas[i]}
		if strict[i] {
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
	return out, rep, compat, sync, nil
}

// applyThinkingBudget is REQ-PROV-15's last bullet for this wire: an
// `openai-completions` endpoint that shares max_tokens between the reasoning
// and the answer "requires an explicit budget under the field name their
// compat profile specifies — without one, a reasoning-heavy turn consumes the
// whole response and emits no answer".
//
// The budget comes from the model's own ThinkingLevelMap, clamped the same way
// every other level is (REQ-PROV-15), because a row that prices levels in
// TOKENS is how a vLLM or llama.cpp row expresses them at all. A row whose
// levels are effort STRINGS prices no budget and gets none: inventing one
// would cap thinking the row never asked to cap.
func applyThinkingBudget(out *request, m *core.Model, level core.ThinkingLevel, compat Compat) {
	if compat.ThinkingTokenBudgetField == "" ||
		level == core.ThinkingUnset || level == core.ThinkingOff {
		return
	}
	n, ok := thinkingBudgetFor(m, level)
	if !ok {
		return
	}
	out.thinkingBudgetField, out.thinkingBudget = compat.ThinkingTokenBudgetField, &n
}

// DefaultThinkingBudgets is the fallback token-budget table, used ONLY for a
// model with no ThinkingLevelMap at all — a hand-built descriptor with nothing
// to clamp against. The catalog row is authoritative (REQ-CAT-06); this table
// exists so a self-hosted endpoint configured without a row still gets a
// budget rather than the answerless turn REQ-PROV-15 describes. It mirrors the
// Google adapter's table, which faces the same problem.
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

func thinkingBudgetFor(m *core.Model, level core.ThinkingLevel) (int, bool) {
	if m != nil && len(m.ThinkingLevelMap) > 0 {
		_, wire, ok := catalog.ClampThinkingLevel(m, level)
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(wire))
		if err != nil {
			// An effort string, not a budget. A row that prices no level in
			// tokens gets no budget key rather than a 400 on "high".
			return 0, false
		}
		return n, true
	}
	if n, ok := DefaultThinkingBudgets()[level]; ok {
		return n, true
	}
	return 0, false
}

// encodeSchemas serializes every tool's parameters THROUGH the session's
// schema prefix (REQ-CACHE-06, NFR-PERF-03) and returns the per-tool strict
// flag alongside.
//
// The strict-subset rewrite is decided here rather than inside the marshaller
// because it is a property of the TOOL — its ConstrainedSampling — and the
// Marshaller signature is schema in, bytes out. The decision is carried into
// the marshaller by schema pointer, which is the identity the prefix itself
// uses. A schema VALUE shared by two tools that disagree about constrained
// sampling would need two encodings under one identity; that request falls
// back to the uncached path rather than serving one tool the other's bytes.
func encodeSchemas(tools []core.ToolWire, supportsStrict bool,
	prefix *provider.ToolPrefix) ([]json.RawMessage, []bool, provider.SyncReport, error) {
	var sync provider.SyncReport
	strict := make([]bool, len(tools))
	plan := make(map[*schema.Schema]*schema.Schema, len(tools))
	mode := make(map[*schema.Schema]bool, len(tools))
	conflict := false

	for i, tw := range tools {
		target, err := strictTarget(tw, supportsStrict)
		if err != nil {
			return nil, nil, sync, err
		}
		if target != nil {
			strict[i] = true
		} else {
			target = tw.InputSchema
		}
		if prev, ok := mode[tw.InputSchema]; ok && prev != strict[i] {
			conflict = true
		}
		mode[tw.InputSchema] = strict[i]
		plan[tw.InputSchema] = target
	}

	p := prefix
	if conflict {
		p = nil
	}
	raws, rep, err := p.SyncWith(tools, func(s *schema.Schema) (json.RawMessage, error) {
		if t, ok := plan[s]; ok {
			s = t
		}
		if s == nil {
			// A tool with no schema takes no arguments; `parameters: null`
			// is a 400, and an empty object is what the API documents.
			return json.RawMessage(EmptyParameters), nil
		}
		return json.Marshal(s)
	})
	return raws, strict, rep, err
}

// EmptyParameters is the schema sent for a tool with no InputSchema.
const EmptyParameters = `{"type":"object","properties":{}}`

// strictTarget is REQ-TOOL-03 for this wire: the schema is PROBED through the
// strict-subset rewrite before anything is sent, and strict:true is emitted
// only on the rewritten schema, which is what this returns (nil means no
// strict).
//
// A bare strict:true on the raw schema is rejected the moment a tool has an
// optional property or a $ref, and the 400 kills the whole request — every
// turn carrying that tool, not just that tool. On a failed rewrite `prefer`
// (the default) falls back to the unconstrained schema; `require` fails the
// request with the rejection reason, which Stream turns into a pre-closed
// error stream (REQ-PROV-04). Emission is additionally gated on the compat
// profile's SupportsStrictTools (REQ-PROV-12): a profile that cannot emit
// strict cannot satisfy `require` either.
func strictTarget(tw core.ToolWire, supportsStrict bool) (*schema.Schema, error) {
	cs := tw.ConstrainedSampling
	if cs == nil || cs.Type != core.ConstrainJSONSchema {
		return nil, nil
	}
	var rewritten *schema.Schema
	var err error
	if !supportsStrict {
		err = fmt.Errorf("agentkit: this endpoint's compat profile does not support strict tool schemas")
	} else {
		rewritten, err = schema.StrictSubset(tw.InputSchema)
	}
	switch {
	case err == nil:
		return rewritten, nil
	case cs.Strict == core.StrictRequire:
		return nil, fmt.Errorf("tool %q requires constrained sampling: %w", tw.Name, err)
	}
	return nil, nil
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
			if ms[i].deferred {
				continue
			}
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
func encodeMessages(ms core.Messages, system []core.ContentBlock, compat Compat, decl *deferredDecl) []message {
	var out []message
	// insertAt is where decl's system message goes: after the LAST run of tool
	// results that announced a deferred tool (REQ-CACHE-10). -1 means no
	// marker was seen in the repaired view, and the declaration is appended.
	insertAt, inRun := -1, false

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
			inRun = false
			out = append(out, message{Role: "user", Content: encodeContent(v.Content)})

		case core.AssistantMessage:
			inRun = false
			msg := message{Role: "assistant"}
			var text, reasoning strings.Builder
			for _, b := range v.Content {
				switch bv := b.(type) {
				case core.TextBlock:
					text.WriteString(bv.Text)
				case core.ThinkingBlock:
					// REQ-PROV-12 ThinkingFormat: the wire shape for reasoning
					// REPLAY. Only DeepSeek documents a round trip, so only
					// that arm emits one; every other profile DROPS the block.
					//
					//	deepseek       reasoning_content on the assistant message
					//	openai         no replay field on this wire at all
					//	openrouter     wants reasoning_details, which this
					//	               decoder does not receive, so there is
					//	               nothing faithful to send back
					//	together       no documented replay shape
					//	chat-template  the server re-renders the template from
					//	               content alone; a reasoning field is
					//	               dropped, not read
					//
					// Inventing a field for the others would either 400 or be
					// silently ignored, and being silently ignored is worse:
					// the chain looks replayed and is not. Folding the text
					// into `content` is worse still: the model is then
					// conditioned on its own reasoning as if it had SAID it,
					// re-sent and re-billed on every later turn, and learns to
					// emit reasoning as answer text. A block only reaches
					// this arm on a same-model replay (the decoder's marker
					// signature survives REQ-PROV-11 rule 4 for exactly that
					// case; rule 3 downgrades it for any other model), and a
					// same-model replay that cannot carry the chain carries
					// nothing.
					if compat.ThinkingFormat == "deepseek" {
						reasoning.WriteString(bv.Thinking)
					}
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
			msg.ReasoningContent = reasoning.String()
			out = append(out, msg)

		case core.ToolResultMessage:
			// ONE MESSAGE PER RESULT. This is the asymmetry.
			tm := message{Role: "tool", ToolCallID: v.ToolUseID, Content: v.Content.Text()}
			if compat.RequiresToolResultName {
				tm.Name = v.ToolName
			}
			out = append(out, tm)
			if decl != nil {
				if !inRun {
					for _, n := range v.AddedToolNames {
						if decl.names[n] {
							inRun = true
							break
						}
					}
				}
				if inRun {
					insertAt = len(out)
				}
			}
		}
	}
	if decl != nil {
		if insertAt < 0 {
			insertAt = len(out)
		}
		out = append(out[:insertAt], append([]message{decl.msg}, out[insertAt:]...)...)
	}
	return bridgeUserAfterToolResult(out, compat)
}

// bridgeUserAfterToolResult is REQ-PROV-12's AllowsUserAfterToolResult: where
// the profile says false, a user message directly after a tool result gets a
// synthetic assistant turn between them. See InferCompat for the gateways —
// OpenRouter's anthropic/* routes, where a tool message becomes a user turn
// upstream and two user turns in a row are a 400, and Mistral, which rejects
// the adjacency by name.
//
// The bridge runs over the ENCODED messages rather than the canonical ones so
// it also covers a message this encoder inserted itself, and so the canonical
// transcript keeps saying what actually happened.
func bridgeUserAfterToolResult(ms []message, compat Compat) []message {
	if compat.AllowsUserAfterToolResult {
		return ms
	}
	out := make([]message, 0, len(ms))
	for i, m := range ms {
		if i > 0 && m.Role == "user" && ms[i-1].Role == "tool" {
			bridge := message{Role: "assistant", Content: SyntheticAssistantText}
			out = append(out, bridge)
		}
		out = append(out, m)
	}
	return out
}

// SyntheticAssistantText is the content of the turn bridgeUserAfterToolResult
// invents. It is MODEL-VISIBLE, so it is pinned here rather than formatted at
// the call site (compare provider.SyntheticResultText), and it is non-empty
// because the gateways that need the bridge reject an empty assistant turn for
// the same reason they reject the adjacency.
const SyntheticAssistantText = "Understood."

// deferredDecl is the REQ-CACHE-10 third-arm declaration: the tools withheld
// from the tools array, and the system message that re-declares them.
type deferredDecl struct {
	names map[string]bool
	msg   message
}

// deferredDeclaration renders the withheld tools as prose. The schemas are the
// ones the REQ-CACHE-06 prefix already serialized, so declaring a tool this
// way costs no extra marshalling.
//
// Prose is the only declaration this wire has: `tools` is a prefix-position
// array, so a tool added mid-session cannot be declared at its transcript
// position any other way. It is weaker than Anthropic's defer_loading — the
// model may call a tool that is not in the array, which some servers reject —
// and it is still cheaper than rewriting the cached prefix. The Responses
// wire shares it (ruling L-10).
func deferredDeclaration(deferred []core.ToolWire, schemas []json.RawMessage, all []core.ToolWire,
	compat Compat) *deferredDecl {
	if len(deferred) == 0 {
		return nil
	}
	index := make(map[string]int, len(all))
	for i, tw := range all {
		index[tw.Name] = i
	}
	d := &deferredDecl{names: make(map[string]bool, len(deferred))}
	var b strings.Builder
	b.WriteString("Additional tools became available at this point in the conversation " +
		"and may be called from here on:\n")
	for _, tw := range deferred {
		d.names[tw.Name] = true
		b.WriteString("\n- " + tw.Name)
		if tw.Description != "" {
			b.WriteString(": " + tw.Description)
		}
		if i, ok := index[tw.Name]; ok && len(schemas[i]) > 0 {
			b.WriteString("\n  parameters: " + string(schemas[i]))
		}
	}
	role := "system"
	if compat.SupportsDeveloperRole {
		role = "developer"
	}
	d.msg = message{Role: role, Content: b.String(), deferred: true}
	return d
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
