package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// DefaultBaseURL is the local Ollama server.
const DefaultBaseURL = "http://localhost:11434"

// VendorAuth is REQ-AUTH-03's table for Ollama.
//
// Ambient is unconditionally true, and that is the point of REQ-AUTH-04's
// third state rather than a shortcut: a local Ollama needs NO credential, so
// "no key" here is a correct configuration and not a misconfiguration. A
// two-valued credential state would make every local deployment fail a
// pre-flight check.
var VendorAuth = provider.VendorAuth{
	Vars: []provider.EnvVar{
		{Name: "OLLAMA_API_KEY", Scheme: provider.SchemeBearer},
	},
	BaseURLVar: "OLLAMA_HOST",
	Ambient:    func(provider.Env) bool { return true },
}

type Options struct {
	BaseURL       string
	HTTPClient    *http.Client
	Getenv        func(string) string
	Retry         provider.RetryPolicy
	Attribution   *bool
	BillingLookup func(string) *core.Model
	Now           func() time.Time
	MaxLineBytes  int
}

func Provider(opts Options) core.APIProvider {
	c := &client{opts: opts}
	return core.APIProvider{API: API, Stream: c.Stream}
}

type client struct{ opts Options }

func (c *client) Stream(ctx context.Context, m *core.Model, req core.Request, o core.ProviderStreamOptions) *core.EventStream {
	body, rep, err := BuildRequest(m, req)
	if err != nil {
		return core.ErrorStream(nil, fmt.Errorf("ollama: building request: %w", err))
	}
	if rep.Changed() && o.Warnf != nil {
		o.Warnf("ollama: %s", rep.String())
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
		return core.ErrorStream(nil, fmt.Errorf("ollama: encoding request: %w", err))
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
	}, textIdx: -1, thinkIdx: -1}

	if to := req.Options.TimeoutMs; to != nil && *to > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*to)*time.Millisecond)
		defer cancel()
	}

	env := provider.Env{Override: req.Options.Env, Getenv: c.opts.Getenv}
	auth := provider.ResolveAuth(VendorAuth, env)

	base := c.opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	call := provider.Call{
		Method:  http.MethodPost,
		URL:     provider.ResolveBaseURL(m, auth, base) + Path,
		Body:    raw,
		Headers: map[string]string{"content-type": "application/json", "accept": "application/x-ndjson"},
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
		d.fail(provider.TransportErrorText("ollama", ctx, err), err)
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
		msg := provider.StatusError("ollama", resp, provider.JSONErrorDetail)
		d.fail(msg, errors.New(msg))
		return
	}

	d.s.Push(core.MessageStartEvent{Message: d.partial})
	if err := d.consume(provider.NewNDJSONReader(resp.Body, c.opts.MaxLineBytes)); err != nil {
		d.fail(err.Error(), err)
		return
	}
	d.finish(m, c.opts.BillingLookup)
}

// ---------------------------------------------------------------- wire decode

type wireToolCall struct {
	Function struct {
		Name string `json:"name"`
		// Arguments is a JSON OBJECT on this wire, not a string as on the
		// OpenAI family. Held as RawMessage so the bytes reach ToolUseBlock
		// unchanged (REQ-PROV-17).
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type wireMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Thinking  string         `json:"thinking"`
	ToolCalls []wireToolCall `json:"tool_calls"`
}

type wireChunk struct {
	Model      string       `json:"model"`
	Message    *wireMessage `json:"message"`
	Done       bool         `json:"done"`
	DoneReason string       `json:"done_reason"`
	Error      string       `json:"error"`

	PromptEvalCount *int64 `json:"prompt_eval_count"`
	EvalCount       *int64 `json:"eval_count"`
}

type decoder struct {
	s       *core.EventStream
	partial core.AssistantMessage

	blocks   []core.ContentBlock
	textIdx  int
	thinkIdx int
	usage    core.Usage
	doneRs   string
	respMod  string
	calls    int
	sawDone  bool
	sawAny   bool
}

func (d *decoder) consume(r *provider.NDJSONReader) error {
	for {
		line, err := r.Next()
		if err == io.EOF {
			if !d.sawDone && !d.sawAny {
				return provider.ErrSSETruncated
			}
			return nil
		}
		if err != nil {
			return err
		}
		if err := d.chunk(line); err != nil {
			return err
		}
		if d.sawDone {
			return nil
		}
	}
}

func (d *decoder) chunk(line []byte) error {
	var ch wireChunk
	if err := json.Unmarshal(line, &ch); err != nil {
		return fmt.Errorf("ollama: decoding chunk: %w", err)
	}
	// Ollama reports errors as a top-level string on an otherwise ordinary
	// chunk, with a 200 status. The transport layer sees nothing.
	if ch.Error != "" {
		return errors.New("ollama: " + ch.Error)
	}
	d.sawAny = true
	if ch.Model != "" {
		d.respMod = ch.Model
	}
	if ch.Done {
		d.sawDone = true
		d.doneRs = ch.DoneReason
	}
	cw := core.UsageWire{InputTokens: ch.PromptEvalCount, OutputTokens: ch.EvalCount}
	cw.Into(&d.usage)

	if ch.Message == nil {
		return nil
	}
	changed := false

	if t := ch.Message.Thinking; t != "" {
		if d.thinkIdx < 0 {
			d.thinkIdx = len(d.blocks)
			// No signature: this wire carries none, so REQ-PROV-11 rule 4
			// demotes it to plain text on replay. That is the intended
			// outcome, not a loss — replaying unsigned reasoning as reasoning
			// is what the rule exists to prevent.
			d.blocks = append(d.blocks, core.ThinkingBlock{})
			d.s.Push(core.ThinkingStartEvent{BlockIndex: d.thinkIdx})
		}
		tb := d.blocks[d.thinkIdx].(core.ThinkingBlock)
		tb.Thinking += t
		d.blocks[d.thinkIdx] = tb
		d.s.Push(core.ThinkingDeltaEvent{BlockIndex: d.thinkIdx, Delta: t})
		changed = true
	}

	if c := ch.Message.Content; c != "" {
		if d.textIdx < 0 {
			d.textIdx = len(d.blocks)
			d.blocks = append(d.blocks, core.TextBlock{})
			d.s.Push(core.TextStartEvent{BlockIndex: d.textIdx})
		}
		tb := d.blocks[d.textIdx].(core.TextBlock)
		tb.Text += c
		d.blocks[d.textIdx] = tb
		d.s.Push(core.TextDeltaEvent{BlockIndex: d.textIdx, Delta: c})
		changed = true
	}

	for _, tc := range ch.Message.ToolCalls {
		// Ollama's native tool message carries NO id — results pair
		// POSITIONALLY. One is synthesized so the canonical layer, which keys
		// on an id, has something to key on; the encoder drops it again on the
		// way back out.
		id := fmt.Sprintf("%s-%d", d.callScope(), d.calls)
		d.calls++
		raw := tc.Function.Arguments
		if len(raw) == 0 {
			raw = json.RawMessage("{}")
		}
		b, err := core.NewToolUse(id, tc.Function.Name, raw)
		if err != nil {
			b, _ = core.NewToolUse(id, tc.Function.Name, nil)
		}
		idx := len(d.blocks)
		d.blocks = append(d.blocks, b)
		d.s.Push(core.ToolCallStartEvent{BlockIndex: idx, ToolUseID: id, Name: b.Name})
		d.s.Push(core.ToolInputDeltaEvent{BlockIndex: idx, ToolUseID: id, Delta: string(b.Input)})
		d.textIdx, d.thinkIdx = -1, -1
		changed = true
	}

	if changed {
		d.partial.Content = append(core.Content(nil), d.blocks...)
		d.s.Push(core.MessageUpdateEvent{Message: d.partial})
	}
	return nil
}

func (d *decoder) callScope() string {
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
	final.ResponseModel = d.respMod
	final.StopReason = MapStopReason(d.doneRs, len(core.ExtractToolUse(&final)) > 0)
	final.RawStopReason = d.doneRs
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

// DecodeResponse decodes a NON-streaming /api/chat response, sharing the
// assembler with the streaming path (REQ-PROV-17).
func DecodeResponse(m *core.Model, data []byte, lookup func(string) *core.Model) (*core.AssistantMessage, error) {
	d := &decoder{s: core.NewEventStream(core.StreamOptions{}), partial: core.AssistantMessage{
		Provider: m.Provider, API: m.API, Model: m.ID,
	}, textIdx: -1, thinkIdx: -1}
	if err := d.chunk(data); err != nil {
		return nil, err
	}
	content := core.Content(append([]core.ContentBlock(nil), d.blocks...))
	msg := d.partial
	msg.Content = content
	msg.ResponseModel = d.respMod
	msg.StopReason = MapStopReason(d.doneRs, len(core.ExtractToolUse(&msg)) > 0)
	msg.RawStopReason = d.doneRs
	msg.Usage = d.usage
	billModel, billed := provider.BillingModel(m, d.respMod, lookup)
	msg.Usage.BilledModel = billed
	if msg.Usage.Reported() {
		msg.Usage.SetCost(provider.ComputeCost(billModel, msg.Usage))
	}
	return &msg, nil
}
