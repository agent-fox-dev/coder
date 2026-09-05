package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// DefaultBaseURL is the fallback when neither the catalog row nor a vendor
// environment variable names one.
const DefaultBaseURL = "https://api.openai.com/v1"

// Path is the Chat Completions endpoint, appended to the base URL.
const Path = "/chat/completions"

// vendorAuth is REQ-AUTH-03's per-vendor table for the vendors that speak this
// wire API. It is keyed by VENDOR while the provider is keyed by wire API —
// that split is the whole point of REQ-PROV-09, and it is why one
// implementation can serve OpenAI, OpenRouter, DeepSeek and a self-hosted
// gateway without a vendor-keyed registry.
var vendorAuth = map[string]provider.VendorAuth{
	"openai": {
		Vars:       []provider.EnvVar{{Name: "OPENAI_API_KEY", Scheme: provider.SchemeBearer}},
		BaseURLVar: "OPENAI_BASE_URL",
	},
	"openrouter": {
		Vars:       []provider.EnvVar{{Name: "OPENROUTER_API_KEY", Scheme: provider.SchemeBearer}},
		BaseURLVar: "OPENROUTER_BASE_URL",
	},
	"deepseek": {
		Vars:       []provider.EnvVar{{Name: "DEEPSEEK_API_KEY", Scheme: provider.SchemeBearer}},
		BaseURLVar: "DEEPSEEK_BASE_URL",
	},
	"xai": {
		Vars:       []provider.EnvVar{{Name: "XAI_API_KEY", Scheme: provider.SchemeBearer}},
		BaseURLVar: "XAI_BASE_URL",
	},
	"groq": {
		Vars:       []provider.EnvVar{{Name: "GROQ_API_KEY", Scheme: provider.SchemeBearer}},
		BaseURLVar: "GROQ_BASE_URL",
	},
	"together": {
		Vars:       []provider.EnvVar{{Name: "TOGETHER_API_KEY", Scheme: provider.SchemeBearer}},
		BaseURLVar: "TOGETHER_BASE_URL",
	},
	"moonshot": {
		Vars:       []provider.EnvVar{{Name: "MOONSHOT_API_KEY", Scheme: provider.SchemeBearer}},
		BaseURLVar: "MOONSHOT_BASE_URL",
	},
}

// AuthFor returns a vendor's table, falling back to <VENDOR>_API_KEY.
//
// The fallback is a LAST RESORT for a vendor this build has never heard of,
// not the convention REQ-AUTH-03 rejects. The requirement's objection is to
// that convention being the whole model — which loses Anthropic's bearer/key
// split and every gateway's base-URL credential. As a default for an unknown
// vendor it beats refusing to authenticate at all.
func AuthFor(vendor string) provider.VendorAuth {
	if v, ok := vendorAuth[vendor]; ok {
		return v
	}
	up := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_", "/", "_").Replace(vendor))
	if up == "" {
		up = "OPENAI"
	}
	return provider.VendorAuth{
		Vars:       []provider.EnvVar{{Name: up + "_API_KEY", Scheme: provider.SchemeBearer}},
		BaseURLVar: up + "_BASE_URL",
	}
}

// Options configures the provider. The zero value is usable.
type Options struct {
	BaseURL       string
	HTTPClient    *http.Client
	Getenv        func(string) string
	Retry         provider.RetryPolicy
	Attribution   *bool
	BillingLookup func(string) *core.Model
	Now           func() time.Time
	// Credentials is REQ-AUTH-05's application-owned store. When set it is
	// consulted BEFORE the environment table, because it is the layer that can
	// hold a refreshed OAuth token and the environment is static. An empty
	// store falls through, so adding one never breaks a working env setup.
	Credentials *provider.Credentials
	// Auth overrides the per-vendor table for a vendor this build does not
	// know. Nil uses AuthFor(model.Provider).
	Auth             *provider.VendorAuth
	MaxSSEEventBytes int
}

// Provider returns the registry entry (REQ-PROV-09).
func Provider(opts Options) core.APIProvider {
	c := &client{opts: opts}
	return core.APIProvider{API: API, Stream: c.Stream}
}

type client struct{ opts Options }

func (c *client) Stream(ctx context.Context, m *core.Model, req core.Request, o core.ProviderStreamOptions) *core.EventStream {
	body, rep, err := BuildRequest(m, req)
	if err != nil {
		return core.ErrorStream(nil, fmt.Errorf("openai: building request: %w", err))
	}
	if rep.Changed() && o.Warnf != nil {
		o.Warnf("openai: %s", rep.String())
	}

	var payload any = body
	if fn := req.Options.OnPayload; fn != nil {
		out, perr := fn(body, m)
		if perr != nil {
			return core.ErrorStream(nil, perr)
		}
		if out != nil {
			payload = out
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return core.ErrorStream(nil, fmt.Errorf("openai: encoding request: %w", err))
	}

	s := core.NewEventStream(core.StreamOptions{})
	go c.run(ctx, s, m, req, raw)
	return s
}

func (c *client) run(ctx context.Context, s *core.EventStream, m *core.Model, req core.Request, raw []byte) {
	now := time.Now
	if c.opts.Now != nil {
		now = c.opts.Now
	}
	d := &decoder{s: s, partial: core.AssistantMessage{
		Provider: m.Provider, API: m.API, Model: m.ID,
		ThinkingLevel: req.ThinkingLevel, Timestamp: now(),
	}}

	if to := req.Options.TimeoutMs; to != nil && *to > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*to)*time.Millisecond)
		defer cancel()
	}

	env := provider.Env{Override: req.Options.Env, Getenv: c.opts.Getenv}
	table := AuthFor(m.Provider)
	if c.opts.Auth != nil {
		table = *c.opts.Auth
	}
	auth, err := provider.ResolveAuthWith(ctx, m.Provider, c.opts.Credentials, table, env)
	if err != nil {
		d.fail(provider.TransportErrorText("openai", ctx, err), err)
		return
	}

	base := c.opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	call := provider.Call{
		Method: http.MethodPost,
		URL:    provider.ResolveBaseURL(m, auth, base) + Path,
		Body:   raw,
		Headers: map[string]string{
			"content-type": "application/json",
			"accept":       "text/event-stream",
		},
		Auth: auth, Model: m, Options: req.Options,
		Attribution: c.opts.Attribution, Env: env,
		Client: c.opts.HTTPClient, Retry: c.opts.Retry,
	}

	resp, err := call.Do(ctx)
	if err != nil {
		if errors.Is(err, provider.ErrRetryDelayTooLong) {
			d.fail(err.Error(), err)
			return
		}
		d.fail(provider.TransportErrorText("openai", ctx, err), err)
		return
	}
	defer resp.Body.Close()

	if fn := req.Options.OnResponse; fn != nil {
		if err := fn(resp, m); err != nil {
			d.fail(err.Error(), err)
			return
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := provider.StatusError("openai", resp, provider.JSONErrorDetail)
		d.fail(msg, errors.New(msg))
		return
	}

	d.s.Push(core.MessageStartEvent{Message: d.partial})
	if err := d.consume(provider.NewSSEReader(resp.Body, c.opts.MaxSSEEventBytes)); err != nil {
		d.fail(err.Error(), err)
		return
	}
	d.finish(m, c.opts.BillingLookup)
}

// ---------------------------------------------------------------- wire decode

type wireUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	TotalTokens         *int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`

	// REQ-PROV-05.2: the cached count lives in THREE places and all three arms
	// are required. DeepSeek reports prompt_cache_hit_tokens; Moonshot reports
	// a top-level cached_tokens; everyone else nests it in
	// prompt_tokens_details. A decoder that reads only the nested arm silently
	// treats every cached token as a full-price input token on two vendors.
	PromptCacheHitTokens *int64 `json:"prompt_cache_hit_tokens"`
	CachedTokens         *int64 `json:"cached_tokens"`

	// Anthropic-style cache writes arriving over this wire (OpenRouter's
	// anthropic/* routes with CacheControlFormat).
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

// Into folds an OpenAI-family usage onto the canonical one, applying
// REQ-PROV-05.1's subtraction.
//
// prompt_tokens INCLUDES cached tokens on this wire. Setting
// input_tokens = prompt_tokens double-counts the cached portion and overstates
// cost by up to ~90% on a well-cached agent loop — which then trips the
// REQ-LOOP-08 budget gate early, with no error and nothing to look at.
func (w wireUsage) Into(u *core.Usage) {
	var cacheRead *int64
	for _, cand := range []*int64{
		firstNonNil(w.PromptTokensDetails), w.PromptCacheHitTokens, w.CachedTokens,
	} {
		if cand != nil {
			cacheRead = cand
			break
		}
	}

	cw := core.UsageWire{
		OutputTokens:     w.CompletionTokens,
		TotalTokens:      w.TotalTokens,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: w.CacheCreationInputTokens,
	}
	if w.CompletionTokensDetails != nil {
		cw.ReasoningTokens = w.CompletionTokensDetails.ReasoningTokens
	}
	if w.PromptTokens != nil {
		net := *w.PromptTokens
		if cacheRead != nil {
			net -= *cacheRead
		}
		if w.CacheCreationInputTokens != nil {
			net -= *w.CacheCreationInputTokens
		}
		if net < 0 {
			net = 0
		}
		cw.InputTokens = &net
	}
	cw.Into(u)
}

func firstNonNil(d *struct {
	CachedTokens *int64 `json:"cached_tokens"`
}) *int64 {
	if d == nil {
		return nil
	}
	return d.CachedTokens
}

type wireToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments is a JSON STRING containing JSON. Its fragments are
		// concatenated verbatim; the concatenation is the argument bytes.
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireDelta struct {
	Role             string         `json:"role"`
	Content          *string        `json:"content"`
	ReasoningContent *string        `json:"reasoning_content"`
	Reasoning        *string        `json:"reasoning"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

type wireChoice struct {
	Index        int        `json:"index"`
	Delta        *wireDelta `json:"delta"`
	Message      *wireDelta `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type wireChunk struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------------------------------------------------------- assembler

type slot struct {
	kind  string // "text" | "reasoning" | "tool"
	index int    // canonical block index
	text  strings.Builder
	id    string
	name  string
	args  strings.Builder
}

type decoder struct {
	s       *core.EventStream
	partial core.AssistantMessage

	slots    map[string]*slot
	order    []string
	usage    core.Usage
	respID   string
	respMod  string
	finishRs string
	sawDone  bool
}

// slotFor assigns canonical block indices in FIRST-APPEARANCE order.
//
// Chat Completions has no block index of its own: text arrives in `content`,
// reasoning in `reasoning_content`, and tool calls under their own separate
// index space. Deriving one order here is what lets the event taxonomy's
// BlockIndex mean the same thing on this wire as on Anthropic's.
func (d *decoder) slotFor(key, kind string) *slot {
	if d.slots == nil {
		d.slots = map[string]*slot{}
	}
	if s, ok := d.slots[key]; ok {
		return s
	}
	s := &slot{kind: kind, index: len(d.order)}
	d.slots[key] = s
	d.order = append(d.order, key)
	return s
}

func (d *decoder) consume(r *provider.SSEReader) error {
	for {
		ev, err := r.Next()
		if err == io.EOF {
			if !d.sawDone && len(d.order) == 0 {
				return provider.ErrSSETruncated
			}
			// A server that closes without [DONE] but did deliver content is
			// common enough among OpenAI-compatible gateways that treating it
			// as truncation would fail working deployments. Content is the
			// evidence; the sentinel is a convenience.
			return nil
		}
		if err != nil {
			return err
		}
		data := strings.TrimSpace(string(ev.Data))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			d.sawDone = true
			return nil
		}
		if err := d.chunk([]byte(data)); err != nil {
			return err
		}
	}
}

func (d *decoder) chunk(data []byte) error {
	var ch wireChunk
	if err := json.Unmarshal(data, &ch); err != nil {
		return fmt.Errorf("openai: decoding chunk: %w", err)
	}
	if ch.Error != nil {
		msg := ch.Error.Type
		if ch.Error.Message != "" {
			if msg != "" {
				msg += ": "
			}
			msg += ch.Error.Message
		}
		return errors.New("openai: " + msg)
	}
	if ch.ID != "" {
		d.respID = ch.ID
	}
	if ch.Model != "" {
		d.respMod = ch.Model
	}
	if ch.Usage != nil {
		ch.Usage.Into(&d.usage)
	}

	changed := false
	for _, c := range ch.Choices {
		if c.FinishReason != "" {
			d.finishRs = c.FinishReason
		}
		delta := c.Delta
		if delta == nil {
			delta = c.Message
		}
		if delta == nil {
			continue
		}
		if reason := firstString(delta.ReasoningContent, delta.Reasoning); reason != "" {
			s := d.slotFor("reasoning", "reasoning")
			if s.text.Len() == 0 {
				d.s.Push(core.ThinkingStartEvent{BlockIndex: s.index})
			}
			s.text.WriteString(reason)
			d.s.Push(core.ThinkingDeltaEvent{BlockIndex: s.index, Delta: reason})
			changed = true
		}
		if delta.Content != nil && *delta.Content != "" {
			s := d.slotFor("text", "text")
			if s.text.Len() == 0 {
				d.s.Push(core.TextStartEvent{BlockIndex: s.index})
			}
			s.text.WriteString(*delta.Content)
			d.s.Push(core.TextDeltaEvent{BlockIndex: s.index, Delta: *delta.Content})
			changed = true
		}
		for i, tc := range delta.ToolCalls {
			key := fmt.Sprintf("tool:%d", i)
			if tc.Index != nil {
				key = fmt.Sprintf("tool:%d", *tc.Index)
			}
			s := d.slotFor(key, "tool")
			fresh := s.id == "" && s.name == ""
			if tc.ID != "" {
				s.id = tc.ID
			}
			if tc.Function.Name != "" {
				s.name = tc.Function.Name
			}
			if fresh && (s.id != "" || s.name != "") {
				d.s.Push(core.ToolCallStartEvent{BlockIndex: s.index, ToolUseID: s.id, Name: s.name})
			}
			if a := tc.Function.Arguments; a != "" {
				s.args.WriteString(a)
				d.s.Push(core.ToolInputDeltaEvent{BlockIndex: s.index, ToolUseID: s.id, Delta: a})
				changed = true
			}
		}
	}

	if changed {
		// A snapshot per CHUNK, not per completed block.
		//
		// Anthropic announces when a block ends mid-stream; Chat Completions
		// does not — every block finalizes at end of stream. Emitting
		// snapshots only on finalization would bunch them all at the end,
		// which defeats the purpose of a snapshot event. ClassSnapshot is
		// explicitly "complete as of now, repeatable, never final", so a
		// per-chunk cadence is within contract.
		d.partial.Content = d.snapshot()
		d.s.Push(core.MessageUpdateEvent{Message: d.partial})
	}
	return nil
}

func firstString(ps ...*string) string {
	for _, p := range ps {
		if p != nil && *p != "" {
			return *p
		}
	}
	return ""
}

// snapshot materializes the accumulators into canonical content, in block
// order.
func (d *decoder) snapshot() core.Content {
	keys := append([]string(nil), d.order...)
	sort.SliceStable(keys, func(i, j int) bool {
		return d.slots[keys[i]].index < d.slots[keys[j]].index
	})
	out := make(core.Content, 0, len(keys))
	for _, k := range keys {
		s := d.slots[k]
		switch s.kind {
		case "text":
			out = append(out, core.TextBlock{Text: s.text.String()})
		case "reasoning":
			// No signature: this wire carries none, and REQ-PROV-11 rule 4
			// will demote it to plain text on replay. That is the correct
			// outcome — replaying unsigned reasoning as reasoning is what the
			// rule exists to prevent — and the compat profile's ThinkingFormat
			// is where a vendor that DOES round-trip it belongs.
			out = append(out, core.ThinkingBlock{Thinking: s.text.String()})
		case "tool":
			raw := json.RawMessage(s.args.String())
			if repaired, changed := provider.SalvageJSON(raw); changed {
				raw = repaired
			}
			b, err := core.NewToolUse(s.id, s.name, raw)
			if err != nil {
				b, _ = core.NewToolUse(s.id, s.name, nil)
			}
			out = append(out, b)
		}
	}
	return out
}

func (d *decoder) finish(m *core.Model, lookup func(string) *core.Model) {
	content := d.snapshot()

	// Block ends, in block order, after the stream has fully ended
	// (REQ-OBS-08.3).
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
	final.StopReason = MapFinishReason(d.finishRs, hasTools)
	final.RawStopReason = d.finishRs
	final.Usage = d.usage

	billModel, billed := provider.BillingModel(m, d.respMod, lookup)
	final.Usage.BilledModel = billed
	if final.Usage.Reported() {
		final.Usage.SetCost(provider.ComputeCost(billModel, final.Usage))
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
	d.s.Push(core.ErrorEvent{Message: text, Err: err, Terminal: true})
	d.s.Push(core.MessageEndEvent{Message: final})
	d.s.End(core.StreamResult{Message: &final, Err: err})
}

// DecodeResponse decodes a NON-streaming Chat Completions response. It exists
// for REQ-PROV-17's byte-identity conformance test, and shares the assembler
// with the streaming path so that identity holds by construction.
func DecodeResponse(m *core.Model, data []byte, lookup func(string) *core.Model) (*core.AssistantMessage, error) {
	var ch wireChunk
	if err := json.Unmarshal(data, &ch); err != nil {
		return nil, fmt.Errorf("openai: decoding response: %w", err)
	}
	d := &decoder{s: core.NewEventStream(core.StreamOptions{}), partial: core.AssistantMessage{
		Provider: m.Provider, API: m.API, Model: m.ID,
	}}
	if err := d.chunk(data); err != nil {
		return nil, err
	}
	content := d.snapshot()
	msg := d.partial
	msg.Content = content
	msg.ResponseID = d.respID
	msg.ResponseModel = d.respMod
	hasTools := len(core.ExtractToolUse(&msg)) > 0
	msg.StopReason = MapFinishReason(d.finishRs, hasTools)
	msg.RawStopReason = d.finishRs
	msg.Usage = d.usage
	billModel, billed := provider.BillingModel(m, d.respMod, lookup)
	msg.Usage.BilledModel = billed
	if msg.Usage.Reported() {
		msg.Usage.SetCost(provider.ComputeCost(billModel, msg.Usage))
	}
	return &msg, nil
}
