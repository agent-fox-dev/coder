package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// DefaultBaseURL is used when neither the catalog row nor ANTHROPIC_BASE_URL
// names one.
const DefaultBaseURL = "https://api.anthropic.com"

// APIVersion is the required anthropic-version header.
const APIVersion = "2023-06-01"

// BetaCompaction opts into REQ-PROV-07's server-side compaction. Compaction
// blocks in the response are retained as core.RawBlock and replayed verbatim
// on later turns; nothing else in the SDK needs to model them.
const BetaCompaction = "compact-2026-01-12"

// VendorAuth is REQ-AUTH-03's ORDERED table for the Anthropic vendor.
//
// The order is load-bearing and so is the per-row scheme. ANTHROPIC_AUTH_TOKEN
// is sent as `Authorization: Bearer` and ANTHROPIC_API_KEY as `x-api-key`;
// sending either under the other's header is a 401 whose body says nothing
// about which variable was picked. This is precisely why REQ-AUTH-03 rejects a
// single `<VENDOR>_API_KEY` convention.
var VendorAuth = provider.VendorAuth{
	Vars: []provider.EnvVar{
		{Name: "ANTHROPIC_AUTH_TOKEN", Scheme: provider.SchemeBearer},
		{Name: "ANTHROPIC_OAUTH_TOKEN", Scheme: provider.SchemeBearer},
		{Name: "ANTHROPIC_API_KEY", Scheme: provider.SchemeAPIKey},
	},
	BaseURLVar: "ANTHROPIC_BASE_URL",
}

// Options configures the provider. The zero value is usable.
type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	// Getenv is injectable so a test never mutates process environment
	// (NFR-TEST-04). Nil means os.Getenv.
	Getenv func(string) string
	Retry  provider.RetryPolicy
	// Betas are sent as anthropic-beta. BetaCompaction is REQ-PROV-07.
	Betas []string
	// Attribution overrides AgentConfig.Attribution for this provider
	// (REQ-SEC-13.2). Nil means on unless AGENTKIT_TELEMETRY=0.
	Attribution *bool
	// BillingLookup resolves a SERVED model id to its catalog row
	// (REQ-PROV-05.5). Nil bills a fallback-served response at the requested
	// model's rates and still records the served name.
	BillingLookup func(string) *core.Model
	// Now is injectable for deterministic timestamps in tests.
	Now func() time.Time
	// MaxSSEEventBytes bounds one accumulated SSE event; zero is the default.
	MaxSSEEventBytes int
}

// Provider returns the registry entry (REQ-PROV-09).
func Provider(opts Options) core.APIProvider {
	c := &client{opts: opts}
	return core.APIProvider{API: API, Stream: c.Stream}
}

type client struct{ opts Options }

func (c *client) now() time.Time {
	if c.opts.Now != nil {
		return c.opts.Now()
	}
	return time.Now()
}

// Stream implements core.StreamFunc.
//
// Every failure below is encoded in the returned stream, never returned as a
// Go error (REQ-PROV-04) — the signature has no error to return, which is the
// enforcement rather than a convention.
func (c *client) Stream(ctx context.Context, m *core.Model, req core.Request, o core.ProviderStreamOptions) *core.EventStream {
	retention := core.CacheRetentionShort
	if o.CacheRetention != "" {
		retention = o.CacheRetention
	}
	if r := req.Options.CacheRetention; r != nil {
		retention = *r
	}

	body, rep, err := BuildRequest(m, req, retention)
	if err != nil {
		return core.ErrorStream(nil, fmt.Errorf("anthropic: building request: %w", err))
	}
	if rep.Changed() && o.Warnf != nil {
		o.Warnf("anthropic: %s", rep.String())
	}
	applyThinking(body, m, req.ThinkingLevel)

	// REQ-PROV-18: OnPayload runs after canonical->wire translation and before
	// the first byte. Its error propagates to the caller UNMODIFIED, which is
	// why it is wrapped by ErrorStream rather than by fmt.Errorf.
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
		return core.ErrorStream(nil, fmt.Errorf("anthropic: encoding request: %w", err))
	}

	s := core.NewEventStream(core.StreamOptions{})
	go c.run(ctx, s, m, req, raw)
	return s
}

func (c *client) run(ctx context.Context, s *core.EventStream, m *core.Model, req core.Request, raw []byte) {
	d := &decodeState{
		s: s,
		partial: core.AssistantMessage{
			Provider: m.Provider, API: m.API, Model: m.ID,
			ThinkingLevel: req.ThinkingLevel,
			Timestamp:     c.now(),
		},
		accs: map[int]*blockAcc{},
	}

	if to := req.Options.TimeoutMs; to != nil && *to > 0 {
		// A per-request timeout INDEPENDENT of the caller's context deadline
		// (REQ-PROV-18). It must not outlive this function, hence the defer.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*to)*time.Millisecond)
		defer cancel()
	}

	env := provider.Env{Override: req.Options.Env, Getenv: c.opts.Getenv}
	auth := provider.ResolveAuth(VendorAuth, env)

	url := provider.ResolveBaseURL(m, auth, defaultBase(c.opts.BaseURL)) + "/v1/messages"

	headers := map[string]string{
		"content-type":      "application/json",
		"accept":            "text/event-stream",
		"anthropic-version": APIVersion,
	}
	if len(c.opts.Betas) > 0 {
		headers["anthropic-beta"] = strings.Join(c.opts.Betas, ",")
	}

	call := provider.Call{
		Method: http.MethodPost, URL: url, Body: raw, Headers: headers,
		Auth: auth, Model: m, Options: req.Options,
		Attribution: c.opts.Attribution, Env: env,
		Client: c.opts.HTTPClient, Retry: c.opts.Retry,
	}

	resp, err := call.Do(ctx)
	if err != nil {
		d.fail(transportError(ctx, err), err)
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
		msg := statusError(resp)
		d.fail(msg, errors.New(msg))
		return
	}

	d.emitStart()
	if err := d.consume(provider.NewSSEReader(resp.Body, c.opts.MaxSSEEventBytes)); err != nil {
		d.fail(err.Error(), err)
		return
	}
	d.finish(m, c.opts.BillingLookup)
}

// defaultBase is the compiled-in fallback, kept as a function so the zero
// Options value works without a constructor.
func defaultBase(configured string) string {
	if configured != "" {
		return configured
	}
	return DefaultBaseURL
}

// transportError renders a transport failure as text the SEMANTIC retry layer
// can classify (REQ-PROV-14). A cancellation is normalized separately by the
// caller; everything else keeps the underlying text, because that text is
// where "getaddrinfo", "connection reset" and "EOF" live.
func transportError(ctx context.Context, err error) string {
	if errors.Is(err, provider.ErrRetryDelayTooLong) {
		return err.Error()
	}
	return provider.TransportErrorText("anthropic", ctx, err)
}

// statusError builds the error text for a non-2xx response.
//
// The status CODE is included in the text on purpose: REQ-PROV-14's allowlist
// matches bare "429"/"500"/"503" strings, so a message that renders only the
// provider's prose loses the retry for a gateway that returns a 503 with an
// empty body.
func statusError(resp *http.Response) string {
	return provider.StatusError("anthropic", resp, func(body []byte) string {
		var we wireError
		if json.Unmarshal(body, &we) == nil {
			return we.String()
		}
		return ""
	})
}

// ---------------------------------------------------------------- decode state

type decodeState struct {
	s       *core.EventStream
	partial core.AssistantMessage

	accs    map[int]*blockAcc
	order   []int
	final   map[int]core.ContentBlock
	stopRaw string
	stopSeq string
	usage   core.Usage

	sawStop  bool
	salvaged int
}

func (d *decodeState) emitStart() { d.s.Push(core.MessageStartEvent{Message: d.partial}) }

// consume drives the SSE reader to completion.
func (d *decodeState) consume(r *provider.SSEReader) error {
	for {
		ev, err := r.Next()
		if err == io.EOF {
			if !d.sawStop {
				// A 200 whose body simply stops is the single commonest
				// streaming failure and it is invisible to the transport
				// layer. Only this check turns it into something
				// RetryMiddleware can classify.
				return provider.ErrSSETruncated
			}
			return nil
		}
		if err != nil {
			return err
		}
		if err := d.event(ev); err != nil {
			return err
		}
		if d.sawStop {
			return nil
		}
	}
}

func (d *decodeState) event(ev provider.SSEEvent) error {
	switch ev.Type {
	case "ping", "":
		return nil

	case "error":
		var we wireError
		_ = json.Unmarshal(ev.Data, &we)
		msg := we.String()
		if msg == "" {
			msg = "anthropic: stream error"
		}
		return errors.New(msg)

	case "message_start":
		var p struct {
			Message wireResponse `json:"message"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return fmt.Errorf("anthropic: message_start: %w", err)
		}
		d.partial.ResponseID = p.Message.ID
		d.partial.ResponseModel = p.Message.Model
		p.Message.Usage.Into(&d.usage)

	case "content_block_start":
		var p struct {
			Index int             `json:"index"`
			Block json.RawMessage `json:"content_block"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return fmt.Errorf("anthropic: content_block_start: %w", err)
		}
		acc := startFrom(p.Block, false)
		d.accs[p.Index] = acc
		d.order = append(d.order, p.Index)
		if e := acc.startEvent(p.Index); e != nil {
			d.s.Push(e)
		}

	case "content_block_delta":
		var p struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return fmt.Errorf("anthropic: content_block_delta: %w", err)
		}
		acc := d.accs[p.Index]
		if acc == nil {
			// A delta for a block we never saw start. Dropping it is the only
			// option that does not invent a block index.
			return nil
		}
		switch p.Delta.Type {
		case "text_delta":
			acc.text.WriteString(p.Delta.Text)
			d.s.Push(core.TextDeltaEvent{BlockIndex: p.Index, Delta: p.Delta.Text})
		case "thinking_delta":
			acc.thinking.WriteString(p.Delta.Thinking)
			d.s.Push(core.ThinkingDeltaEvent{BlockIndex: p.Index, Delta: p.Delta.Thinking})
		case "signature_delta":
			// A signature carries no incremental event: it is not content a UI
			// renders, and it arrives whole.
			acc.signature += p.Delta.Signature
		case "input_json_delta":
			acc.input = append(acc.input, p.Delta.PartialJSON...)
			d.s.Push(core.ToolInputDeltaEvent{BlockIndex: p.Index,
				ToolUseID: acc.id, Delta: p.Delta.PartialJSON})
		}

	case "content_block_stop":
		var p struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return fmt.Errorf("anthropic: content_block_stop: %w", err)
		}
		acc := d.accs[p.Index]
		if acc == nil {
			return nil
		}
		b := acc.block()
		if b == nil {
			return nil
		}
		if d.final == nil {
			d.final = map[int]core.ContentBlock{}
		}
		d.final[p.Index] = b
		if acc.salvaged {
			d.salvaged++
		}
		d.partial.Content = append(d.partial.Content, b)
		// A snapshot per completed block, so a diff-based renderer never needs
		// its own delta accumulator (REQ-OBS-06b).
		d.s.Push(core.MessageUpdateEvent{Message: d.partial})

	case "message_delta":
		var p struct {
			Delta struct {
				StopReason   string  `json:"stop_reason"`
				StopSequence *string `json:"stop_sequence"`
			} `json:"delta"`
			Usage wireUsage `json:"usage"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return fmt.Errorf("anthropic: message_delta: %w", err)
		}
		if p.Delta.StopReason != "" {
			d.stopRaw = p.Delta.StopReason
		}
		if p.Delta.StopSequence != nil {
			d.stopSeq = *p.Delta.StopSequence
		}
		p.Usage.Into(&d.usage)

	case "message_stop":
		d.sawStop = true
	}
	return nil
}

// finish emits the authoritative phase and ends the stream.
func (d *decodeState) finish(m *core.Model, lookup func(string) *core.Model) {
	// Block-end events, in BLOCK ORDER, after the stream has fully ended
	// (REQ-OBS-08.3). Emitting them from the per-chunk handler produces
	// duplicate ends and a message end with no usage, because usage arrives
	// with the terminal chunk — which is exactly the event we have only now.
	for _, i := range d.order {
		acc, b := d.accs[i], d.final[i]
		if acc == nil || b == nil {
			continue
		}
		if e := acc.endEvent(i, b); e != nil {
			d.s.Push(e)
		}
	}

	final := d.partial
	final.StopReason = MapStopReason(d.stopRaw)
	final.RawStopReason = d.stopRaw
	final.Usage = d.usage
	final.Usage.BilledModel = ""

	// REQ-PROV-05.5: bill the model that SERVED the request. Cost is computed
	// ONCE, here, from the final served name — never accumulated per event.
	// That is what makes "repriced back" fall out for free when a later event
	// names the requested model again.
	billModel, billed := provider.BillingModel(m, final.ResponseModel, lookup)
	final.Usage.BilledModel = billed
	if final.Usage.Reported() {
		final.Usage.SetCost(provider.ComputeCost(billModel, final.Usage))
	}

	d.s.Push(core.MessageEndEvent{Message: final})
	d.s.End(core.StreamResult{Message: &final})
}

// fail is REQ-PROV-04: the partial content and the failure are ONE value.
//
// Half an assistant message followed by a truncated stream is not an error
// with a message thrown away — the retry classifier reads the text and session
// persistence keeps the content, and neither works if the two are separated.
func (d *decodeState) fail(text string, err error) {
	final := d.partial
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

// ---------------------------------------------------------------- whole-response

// DecodeResponse decodes a NON-streaming Messages response.
//
// It exists for REQ-PROV-17's conformance test: the streaming and whole
// response paths must produce byte-identical ToolUseBlock.Input for the same
// call. Sharing the assembler is what makes that true by construction rather
// than by coincidence, and DecodeResponse is how the test can say so.
func DecodeResponse(m *core.Model, data []byte, lookup func(string) *core.Model) (*core.AssistantMessage, error) {
	var wr wireResponse
	if err := json.Unmarshal(data, &wr); err != nil {
		return nil, fmt.Errorf("anthropic: decoding response: %w", err)
	}
	msg := &core.AssistantMessage{
		Provider: m.Provider, API: m.API, Model: m.ID,
		ResponseID: wr.ID, ResponseModel: wr.Model,
		StopReason: MapStopReason(wr.StopReason), RawStopReason: wr.StopReason,
	}
	for _, raw := range wr.Content {
		acc := startFrom(raw, true)
		if b := acc.block(); b != nil {
			msg.Content = append(msg.Content, b)
		}
	}
	wr.Usage.Into(&msg.Usage)
	billModel, billed := provider.BillingModel(m, wr.Model, lookup)
	msg.Usage.BilledModel = billed
	if msg.Usage.Reported() {
		msg.Usage.SetCost(provider.ComputeCost(billModel, msg.Usage))
	}
	return msg, nil
}

// applyThinking is REQ-PROV-15's Anthropic arm: a TRI-state where undefined
// omits the key entirely and an explicit "off" sends {"type":"disabled"}.
//
// The level reaching here is expected to be CLAMPED already (REQ-PROV-15:
// "passing an unclamped level through is prohibited"). An unmapped level is
// therefore omitted rather than guessed: sending reasoning_effort a model does
// not know is a 400, and inventing a budget for it is worse than not thinking.
func applyThinking(r *request, m *core.Model, lvl core.ThinkingLevel) {
	switch lvl {
	case core.ThinkingUnset:
		return
	case core.ThinkingOff:
		r.Thinking = &thinking{Type: "disabled"}
		return
	}
	wire, ok := m.ThinkingLevelMap[lvl]
	if !ok || wire == nil {
		return // present-null and absent are runtime-identical
	}

	t := &thinking{Type: "enabled"}
	if n, err := strconv.Atoi(*wire); err == nil && n > 0 {
		// Anthropic rejects a thinking budget that is not strictly below
		// max_tokens. The budget is the value we may lower; max_tokens has
		// already been clamped against the context window (REQ-CAT-04) and
		// lowering it again would silently truncate the answer instead.
		if n >= r.MaxTokens {
			n = r.MaxTokens - 1
		}
		if n <= 0 {
			return
		}
		t.BudgetTokens = &n
	}
	r.Thinking = t

	// Extended thinking rejects any explicit temperature or top_p. Dropping
	// them is the only option that keeps the request valid; the alternative is
	// a 400 that names sampling and not thinking, sending the reader to the
	// wrong knob.
	r.Temperature, r.TopP = nil, nil
}
