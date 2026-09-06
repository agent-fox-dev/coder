package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// DefaultBaseURL is the Generative Language endpoint. The Vertex AI path uses
// a different host and resolves to the AMBIENT credential state of
// REQ-AUTH-04 (NFR-COMPAT-05); set it through the catalog row or
// GOOGLE_GEMINI_BASE_URL.
const DefaultBaseURL = "https://generativelanguage.googleapis.com"

// VendorAuth is REQ-AUTH-03's ordered table for Google.
//
// The Ambient detector is NFR-COMPAT-05's requirement in one line: an ADC or
// Vertex deployment has no readable key and must not resolve to "no
// credential", or every service-account deployment fails a pre-flight check
// that a plain API key would have passed.
var VendorAuth = provider.VendorAuth{
	Vars: []provider.EnvVar{
		{Name: "GOOGLE_GENERATIVE_AI_API_KEY", Scheme: provider.SchemeAPIKey},
		{Name: "GEMINI_API_KEY", Scheme: provider.SchemeAPIKey},
		{Name: "GOOGLE_API_KEY", Scheme: provider.SchemeAPIKey},
	},
	BaseURLVar: "GOOGLE_GEMINI_BASE_URL",
	Ambient: func(e provider.Env) bool {
		return e.Has("GOOGLE_APPLICATION_CREDENTIALS") ||
			e.Has("GOOGLE_CLOUD_PROJECT") ||
			e.Has("CLOUDSDK_CORE_PROJECT")
	},
}

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
	Credentials      *provider.Credentials
	MaxSSEEventBytes int
}

func Provider(opts Options) core.APIProvider {
	c := &client{opts: opts}
	return core.APIProvider{API: API, Stream: c.Stream}
}

type client struct{ opts Options }

func (c *client) Stream(ctx context.Context, m *core.Model, req core.Request, o core.ProviderStreamOptions) *core.EventStream {
	body, rep, err := BuildRequest(m, req)
	if err != nil {
		return core.ErrorStream(nil, fmt.Errorf("google: building request: %w", err))
	}
	if rep.Changed() && o.Warnf != nil {
		o.Warnf("google: %s", rep.String())
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
		return core.ErrorStream(nil, fmt.Errorf("google: encoding request: %w", err))
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
	auth, err := provider.ResolveAuthWith(ctx, m.Provider, c.opts.Credentials, VendorAuth, env)
	if err != nil {
		d.fail(provider.TransportErrorText("google", ctx, err), err)
		return
	}

	base := c.opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	headers := map[string]string{
		"content-type": "application/json",
		"accept":       "text/event-stream",
	}
	// Google carries an API key in its own header, not in Authorization, and
	// NOT in the query string: a key in a URL lands in every access log and
	// proxy trace between here and the endpoint.
	//
	// A credential store that already supplied an Authorization header is left
	// alone — that is the Vertex/ADC path (NFR-COMPAT-05), and rewriting it
	// into x-goog-api-key would send an OAuth token under a field that expects
	// an API key.
	if auth.Headers == nil {
		auth.Headers = map[string]*string{}
	}
	if auth.APIKey != "" && auth.Headers["Authorization"] == nil {
		k := auth.APIKey
		delete(auth.Headers, "x-api-key")
		auth.Headers["x-goog-api-key"] = &k
	}

	call := provider.Call{
		Method:  http.MethodPost,
		URL:     provider.ResolveBaseURL(m, auth, base) + Path(m, true),
		Body:    raw,
		Headers: headers,
		Auth:    auth, Model: m, Options: req.Options,
		Attribution: c.opts.Attribution, Env: env,
		Client: c.opts.HTTPClient, Retry: c.opts.Retry,
	}

	resp, err := call.Do(ctx)
	if err != nil {
		if errors.Is(err, provider.ErrRetryDelayTooLong) {
			d.fail(err.Error(), err)
			return
		}
		d.fail(provider.TransportErrorText("google", ctx, err), err)
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
		msg := provider.StatusError("google", resp, provider.JSONErrorDetail)
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

type wirePart struct {
	Text             string `json:"text"`
	Thought          bool   `json:"thought"`
	ThoughtSignature string `json:"thoughtSignature"`
	InlineData       *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData"`
	FunctionCall *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
}

type wireCandidate struct {
	Content *struct {
		Role  string     `json:"role"`
		Parts []wirePart `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
	Index        int    `json:"index"`
}

type wireUsage struct {
	PromptTokenCount        *int64 `json:"promptTokenCount"`
	CandidatesTokenCount    *int64 `json:"candidatesTokenCount"`
	TotalTokenCount         *int64 `json:"totalTokenCount"`
	CachedContentTokenCount *int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      *int64 `json:"thoughtsTokenCount"`
}

// Into folds a Gemini usage onto the canonical one.
//
// Two normalizations the wire does not do for us:
//
//  1. promptTokenCount INCLUDES cachedContentTokenCount, so the REQ-PROV-05.1
//     subtraction applies here just as it does on the OpenAI family.
//  2. thoughtsTokenCount is reported BESIDE candidatesTokenCount, but the
//     canonical contract says ReasoningTokens is a SUBSET of OutputTokens.
//     Output is therefore the sum, and reasoning the subset — otherwise every
//     thinking turn under-reports output by exactly its reasoning volume and
//     is under-billed by the same.
func (w wireUsage) Into(u *core.Usage) {
	cw := core.UsageWire{
		TotalTokens:     w.TotalTokenCount,
		CacheReadTokens: w.CachedContentTokenCount,
		ReasoningTokens: w.ThoughtsTokenCount,
	}
	if w.PromptTokenCount != nil {
		net := *w.PromptTokenCount
		if w.CachedContentTokenCount != nil {
			net -= *w.CachedContentTokenCount
		}
		if net < 0 {
			net = 0
		}
		cw.InputTokens = &net
	}
	if w.CandidatesTokenCount != nil {
		out := *w.CandidatesTokenCount
		if w.ThoughtsTokenCount != nil {
			out += *w.ThoughtsTokenCount
		}
		cw.OutputTokens = &out
	}
	cw.Into(u)
}

type wireResponse struct {
	Candidates     []wireCandidate `json:"candidates"`
	UsageMetadata  *wireUsage      `json:"usageMetadata"`
	ModelVersion   string          `json:"modelVersion"`
	ResponseID     string          `json:"responseId"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error json.RawMessage `json:"error"`
}

// ---------------------------------------------------------------- assembler

type decoder struct {
	s       *core.EventStream
	partial core.AssistantMessage

	blocks   []core.ContentBlock
	textIdx  int // index into blocks of the open text block, -1 when none
	thinkIdx int
	usage    core.Usage
	finishRs string
	respID   string
	respMod  string
	calls    int
	sawAny   bool
}

func (d *decoder) consume(r *provider.SSEReader) error {
	d.textIdx, d.thinkIdx = -1, -1
	for {
		ev, err := r.Next()
		if err == io.EOF {
			if !d.sawAny {
				return provider.ErrSSETruncated
			}
			return nil
		}
		if err != nil {
			return err
		}
		data := strings.TrimSpace(string(ev.Data))
		if data == "" {
			continue
		}
		if err := d.chunk([]byte(data)); err != nil {
			return err
		}
	}
}

func (d *decoder) chunk(data []byte) error {
	if err := provider.GuardUntrusted(data); err != nil {
		return fmt.Errorf("google: %w", err)
	}
	var wr wireResponse
	if err := json.Unmarshal(data, &wr); err != nil {
		return fmt.Errorf("google: decoding chunk: %w", err)
	}
	if len(wr.Error) > 0 {
		if s := provider.JSONErrorDetail(data); s != "" {
			return errors.New("google: " + s)
		}
		return errors.New("google: " + string(wr.Error))
	}
	d.sawAny = true
	if wr.ResponseID != "" {
		d.respID = wr.ResponseID
	}
	if wr.ModelVersion != "" {
		d.respMod = wr.ModelVersion
	}
	if wr.UsageMetadata != nil {
		wr.UsageMetadata.Into(&d.usage)
	}
	if wr.PromptFeedback != nil && wr.PromptFeedback.BlockReason != "" {
		d.finishRs = "SAFETY"
	}

	changed := false
	for _, cand := range wr.Candidates {
		if cand.FinishReason != "" {
			d.finishRs = cand.FinishReason
		}
		if cand.Content == nil {
			continue
		}
		for _, p := range cand.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				// Gemini carries NO tool-call id on the wire; results pair
				// positionally. The canonical layer keys on an id, so one is
				// synthesized here — scoped to the response id so two turns
				// cannot collide, which would let the repair pass match a
				// result against the wrong call.
				id := fmt.Sprintf("%s-%d", d.callScope(), d.calls)
				d.calls++
				raw := json.RawMessage(p.FunctionCall.Args)
				if len(raw) == 0 {
					raw = json.RawMessage("{}")
				}
				b, err := core.NewToolUse(id, p.FunctionCall.Name, raw)
				if err != nil {
					b, _ = core.NewToolUse(id, p.FunctionCall.Name, nil)
				}
				b.ThoughtSignature = p.ThoughtSignature
				idx := len(d.blocks)
				d.blocks = append(d.blocks, b)
				d.s.Push(core.ToolCallStartEvent{BlockIndex: idx, ToolUseID: id, Name: b.Name})
				d.s.Push(core.ToolInputDeltaEvent{BlockIndex: idx, ToolUseID: id, Delta: string(b.Input)})
				// A function call closes any open text run: a later text part
				// is a NEW block, not a continuation of one the call
				// interrupted.
				d.textIdx, d.thinkIdx = -1, -1
				changed = true

			case p.Thought:
				if d.thinkIdx < 0 {
					d.thinkIdx = len(d.blocks)
					d.blocks = append(d.blocks, core.ThinkingBlock{Signature: p.ThoughtSignature})
					d.s.Push(core.ThinkingStartEvent{BlockIndex: d.thinkIdx})
				}
				tb := d.blocks[d.thinkIdx].(core.ThinkingBlock)
				tb.Thinking += p.Text
				if p.ThoughtSignature != "" {
					tb.Signature = p.ThoughtSignature
				}
				d.blocks[d.thinkIdx] = tb
				if p.Text != "" {
					d.s.Push(core.ThinkingDeltaEvent{BlockIndex: d.thinkIdx, Delta: p.Text})
					changed = true
				}

			case p.InlineData != nil:
				// An image block has no start/delta/end triple in the
				// taxonomy: it arrives whole and is carried on the message
				// snapshot rather than announced.
				d.blocks = append(d.blocks, core.ImageBlock{
					Data: p.InlineData.Data, MimeType: p.InlineData.MimeType})
				d.textIdx, d.thinkIdx = -1, -1
				changed = true

			case p.Text != "":
				if d.textIdx < 0 {
					d.textIdx = len(d.blocks)
					d.blocks = append(d.blocks, core.TextBlock{})
					d.s.Push(core.TextStartEvent{BlockIndex: d.textIdx})
				}
				tb := d.blocks[d.textIdx].(core.TextBlock)
				tb.Text += p.Text
				d.blocks[d.textIdx] = tb
				d.s.Push(core.TextDeltaEvent{BlockIndex: d.textIdx, Delta: p.Text})
				changed = true
			}
		}
	}

	if changed {
		d.partial.Content = append(core.Content(nil), d.blocks...)
		d.s.Push(core.MessageUpdateEvent{Message: d.partial})
	}
	return nil
}

// callScope namespaces a synthesized tool-call id. Falling back to the model
// version keeps ids stable for a golden test while still varying per response
// wherever the API supplies a responseId.
func (d *decoder) callScope() string {
	if d.respID != "" {
		return d.respID
	}
	if d.respMod != "" {
		return d.respMod
	}
	return "call"
}

func (d *decoder) finish(m *core.Model, lookup func(string) *core.Model) {
	content := core.Content(append([]core.ContentBlock(nil), d.blocks...))
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
	final.StopReason = MapFinishReason(d.finishRs, len(core.ExtractToolUse(&final)) > 0)
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
	final.Content = core.Content(append([]core.ContentBlock(nil), d.blocks...))
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

// DecodeResponse decodes a NON-streaming generateContent response, sharing the
// assembler with the streaming path (REQ-PROV-17).
func DecodeResponse(m *core.Model, data []byte, lookup func(string) *core.Model) (*core.AssistantMessage, error) {
	d := &decoder{s: core.NewEventStream(core.StreamOptions{}), partial: core.AssistantMessage{
		Provider: m.Provider, API: m.API, Model: m.ID,
	}}
	d.textIdx, d.thinkIdx = -1, -1
	if err := d.chunk(data); err != nil {
		return nil, err
	}
	content := core.Content(append([]core.ContentBlock(nil), d.blocks...))
	msg := d.partial
	msg.Content = content
	msg.ResponseID = d.respID
	msg.ResponseModel = d.respMod
	msg.StopReason = MapFinishReason(d.finishRs, len(core.ExtractToolUse(&msg)) > 0)
	msg.RawStopReason = d.finishRs
	msg.Usage = d.usage
	billModel, billed := provider.BillingModel(m, d.respMod, lookup)
	msg.Usage.BilledModel = billed
	if msg.Usage.Reported() {
		msg.Usage.SetCost(provider.ComputeCost(billModel, msg.Usage))
	}
	return &msg, nil
}
