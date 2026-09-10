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

	"github.com/agentfox/agentkit-go/catalog"
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
//
// The names are constants because the deployment switch reads the same three
// (directCredential): a credential only the direct deployment can use is what
// outranks a leftover ANTHROPIC_VERTEX_PROJECT_ID, and a second copy of the
// list is a second place to forget a row.
const (
	AuthTokenVar  = "ANTHROPIC_AUTH_TOKEN"
	OAuthTokenVar = "ANTHROPIC_OAUTH_TOKEN"
	APIKeyVar     = "ANTHROPIC_API_KEY"
)

var VendorAuth = provider.VendorAuth{
	Vars: []provider.EnvVar{
		{Name: AuthTokenVar, Scheme: provider.SchemeBearer},
		{Name: OAuthTokenVar, Scheme: provider.SchemeBearer},
		{Name: APIKeyVar, Scheme: provider.SchemeAPIKey},
		// A base URL is configuration, not a credential (REQ-AUTH-03's
		// "discovery and retrieval are distinct operations"). Sending a proxy
		// URL as a bearer token is nonsense; its presence still means the
		// vendor is set up.
		{Name: VertexBaseURLVar, DiscoveryOnly: true},
	},
	BaseURLVar: BaseURLVar,
	// Ambient is REQ-AUTH-04 for the Vertex deployment, and it is the fix for
	// the whole reported symptom: such a deployment authenticates with a
	// Google OAuth token this process cannot read, so without this it resolves
	// to CredentialNone and every pre-flight check refuses the run with a
	// message saying the vendor is unconfigured. It is configured — for a
	// deployment the table did not know existed.
	Ambient: VertexSelected,
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
	// VertexProject and VertexLocation select the Vertex AI deployment
	// (NFR-COMPAT-05). Setting the project is the whole switch: it selects the
	// Vertex path shape and, with no base URL configured, the regional Vertex
	// host. Both fall back to the environment — see ResolveVertex — so a
	// deployment can also flip with no code change at all.
	VertexProject  string
	VertexLocation string
	// Attribution overrides AgentConfig.Attribution for this provider
	// (REQ-SEC-13.2). Nil means on unless AGENTKIT_TELEMETRY=0.
	Attribution *bool
	// BillingLookup resolves a SERVED model id to its catalog row
	// (REQ-PROV-05.5). Nil bills a fallback-served response at the requested
	// model's rates and still records the served name.
	BillingLookup func(string) *core.Model
	// Credentials is REQ-AUTH-05's application-owned store. When set it is
	// consulted BEFORE the environment table, because it is the layer that can
	// hold a refreshed OAuth token and the environment is static. An empty
	// store falls through, so adding one never breaks a working env setup.
	Credentials *provider.Credentials
	// ToolPrefix is REQ-CACHE-06's per-session schema cache. Nil means this
	// provider value owns one, which is the right scope in practice: a
	// registry is built per agent config. Pass one explicitly to share it, or
	// to read its reconciliation reports.
	ToolPrefix *provider.ToolPrefix
	// OnToolPrefixSync reports each reconciliation, so an embedder can feed
	// REQ-CACHE-11's prefix-invalidation counter.
	OnToolPrefixSync func(provider.SyncReport)
	// Now is injectable for deterministic timestamps in tests.
	Now func() time.Time
	// MaxSSEEventBytes bounds one accumulated SSE event; zero is the default.
	MaxSSEEventBytes int
}

// Provider returns the registry entry (REQ-PROV-09).
func Provider(opts Options) core.APIProvider {
	c := &client{opts: opts, prefix: opts.ToolPrefix}
	if c.prefix == nil {
		c.prefix = &provider.ToolPrefix{}
	}
	return core.APIProvider{API: API, Stream: c.Stream}
}

type client struct {
	opts   Options
	prefix *provider.ToolPrefix
}

// wantsCompaction reports whether Options.Betas opted into REQ-PROV-07.
func (c *client) wantsCompaction() bool {
	for _, b := range c.opts.Betas {
		if strings.TrimSpace(b) == BetaCompaction {
			return true
		}
	}
	return false
}

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
	// NFR-COMPAT-05: the deployment is resolved from config, never from a
	// second provider implementation. It is decided HERE rather than in run
	// because it changes the request BODY as well as the URL, and the body is
	// serialized below.
	env := provider.Env{Override: req.Options.Env, Getenv: c.opts.Getenv}
	vx, err := ResolveVertex(deploymentBase(m, defaultBase(c.opts.BaseURL), env),
		c.opts.VertexProject, c.opts.VertexLocation, env)
	if err != nil {
		return core.ErrorStream(nil, err)
	}

	retention := core.CacheRetentionShort
	if o.CacheRetention != "" {
		retention = o.CacheRetention
	}
	if r := req.Options.CacheRetention; r != nil {
		retention = *r
	}

	body, rep, sync, err := BuildRequestCached(m, req, retention, c.prefix)
	if err != nil {
		return core.ErrorStream(nil, fmt.Errorf("anthropic: building request: %w", err))
	}
	if fn := c.opts.OnToolPrefixSync; fn != nil {
		fn(sync)
	}
	if rep.Changed() && o.Warnf != nil {
		o.Warnf("anthropic: %s", rep.String())
	}
	applyThinking(body, m, req.ThinkingLevel)
	if vx.On() {
		// The body half of NFR-COMPAT-05's second deployment. The model id is
		// a URL segment here and the body field is rejected; the version moves
		// out of the header and into the body.
		body.Model = ""
		body.AnthropicVersion = VertexAPIVersion
	}
	if c.wantsCompaction() {
		// REQ-PROV-07: the beta header opts the REQUEST into the feature and
		// the body names the edit; the server compacts only when both are
		// present. A header alone was silently a no-op.
		body.ContextManagement = &contextManagement{Edits: []contextEdit{{Type: "compact_20260112"}}}
	}

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
	go c.run(ctx, s, m, req, raw, vx)
	return s
}

func (c *client) run(ctx context.Context, s *core.EventStream, m *core.Model, req core.Request,
	raw []byte, vx Vertex) {
	d := &decodeState{
		s: s, model: m, lookup: c.opts.BillingLookup,
		partial: core.AssistantMessage{
			Provider: m.Provider, API: m.API, Model: m.ID,
			ThinkingLevel: req.ThinkingLevel,
			Timestamp:     c.now(),
		},
		accs: map[int]*blockAcc{},
	}

	// caller is the ctx the caller handed to Stream; ctx below may be a
	// TimeoutMs-derived child of it. The two are kept apart because their
	// expiries mean different things: the caller's is an abort (REQ-LOOP-09),
	// the derived one a retryable timeout (REQ-PROV-18).
	caller := ctx
	if to := req.Options.TimeoutMs; to != nil && *to > 0 {
		// A per-request timeout INDEPENDENT of the caller's context deadline
		// (REQ-PROV-18). It must not outlive this function, hence the defer.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*to)*time.Millisecond)
		defer cancel()
	}

	env := provider.Env{Override: req.Options.Env, Getenv: c.opts.Getenv}
	auth, err := provider.ResolveAuthWith(ctx, m.Provider, c.opts.Credentials, VendorAuth, env)
	if err != nil {
		d.fail(provider.TransportErrorText("anthropic", caller, ctx, err), err)
		return
	}

	base := provider.ResolveBaseURL(m, auth, defaultBase(c.opts.BaseURL))
	if vx.On() {
		// A Vertex proxy beats a general one: the two name different
		// upstreams and a machine can carry both.
		if u := env.Get(VertexBaseURLVar); u != "" {
			base = strings.TrimRight(u, "/")
		} else if base == strings.TrimRight(DefaultBaseURL, "/") {
			// A project configured with no base URL anywhere: follow it to the
			// Vertex host rather than sending a Vertex path to api.anthropic.com.
			// An explicitly configured base URL is left exactly as it is.
			base = vx.BaseURL()
		}
		auth = vertexAuth(auth)
	}
	url := base + vx.Path(m, true)

	headers := map[string]string{
		"content-type": "application/json",
		"accept":       "text/event-stream",
	}
	if !vx.On() {
		// Vertex carries the version in the body instead, and rejects a
		// request that names it in both places.
		headers["anthropic-version"] = APIVersion
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
		d.fail(transportError(caller, ctx, err), err)
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
		msg := statusError(resp) + vertexAuthNote(resp.StatusCode, vx, env)
		d.fail(msg, errors.New(msg))
		return
	}

	d.emitStart()
	if err := d.consume(provider.NewSSEReader(resp.Body, c.opts.MaxSSEEventBytes)); err != nil {
		// A cancellation lands here as whatever the body reader reported —
		// an EOF, a "context canceled" — and only the contexts say which of
		// REQ-LOOP-09's abort or REQ-PROV-18's timeout it was.
		d.fail(provider.StreamErrorText("anthropic", caller, ctx, err), err)
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
func transportError(caller, req context.Context, err error) string {
	if errors.Is(err, provider.ErrRetryDelayTooLong) {
		return err.Error()
	}
	return provider.TransportErrorText("anthropic", caller, req, err)
}

// statusError builds the error text for a non-2xx response.
//
// The status CODE is included in the text on purpose: REQ-PROV-14's allowlist
// matches bare "429"/"500"/"503" strings, so a message that renders only the
// provider's prose loses the retry for a gateway that returns a 503 with an
// empty body.
// vertexAuthNote explains an authentication failure from the Vertex
// deployment, and it is the second half of the same defect the ranked
// selection in VertexSelectedBy fixes.
//
// Vertex answers a missing credential with a Google JSON blob — "Request is
// missing required authentication credential", CREDENTIALS_MISSING, a link to
// the Google sign-in console — that names neither Claude, nor the deployment,
// nor the setting that routed the request to Google. Rendered under this
// package's "anthropic:" prefix it reads as an Anthropic outage on a machine
// whose ANTHROPIC_API_KEY is perfectly good, and nothing in it suggests
// looking at ANTHROPIC_VERTEX_PROJECT_ID.
func vertexAuthNote(status int, vx Vertex, env provider.Env) string {
	if !vx.On() || (status != http.StatusUnauthorized && status != http.StatusForbidden) {
		return ""
	}
	note := " [Claude on Vertex AI: project " + vx.Project + ", location " + vx.Location +
		", selected by " + vx.SelectedBy + ". This deployment authenticates with " +
		"Google Application Default Credentials, not " + APIKeyVar
	if directCredential(env) {
		// Saying so is the whole point: the key IS set, it was deliberately
		// withheld from a Google endpoint, and without this line the operator
		// reads the 401 as the key being rejected.
		note += " — which is set, and is never sent to a Google endpoint"
	}
	return note + ". To use the Anthropic API directly instead, unset " +
		VertexProjectVar + " or set " + VertexEnableVar + "=0.]"
}

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
	// model and lookup are what finish and fail price usage against
	// (REQ-PROV-05.5); fail needs them too, because a truncated stream that
	// reported usage in message_start still cost money.
	model  *core.Model
	lookup func(string) *core.Model

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
	if ev.Type == "ping" || (ev.Type == "" && len(ev.Data) == 0) {
		return nil
	}
	// REQ-SEC-11: bytes a provider sent are bytes we did not produce. One
	// linear scan enforces the size, depth and container bounds and rejects
	// duplicate keys, which is what stops a gateway sending two stop_reasons
	// and letting last-wins choose which one we act on.
	if err := provider.GuardUntrusted(ev.Data); err != nil {
		return fmt.Errorf("anthropic: %s event: %w", ev.Type, err)
	}
	if ev.Type == "" {
		// No `event:` line. Every Messages payload also names its type in
		// the JSON, and a gateway that relays data lines without the event
		// line is a real thing; treating such a stream as a run of pings
		// ended every turn with ErrSSETruncated and no content.
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(ev.Data, &probe) != nil || probe.Type == "" || probe.Type == "ping" {
			return nil
		}
		ev.Type = probe.Type
	}
	switch ev.Type {

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
		acc, err := startFrom(p.Block, false)
		if err != nil {
			return fmt.Errorf("anthropic: content_block_start: %w", err)
		}
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
	d.price(&final, m, lookup)

	d.s.Push(core.MessageEndEvent{Message: final})
	d.s.End(core.StreamResult{Message: &final})
}

// price bills a message against the model that served it. It is shared by
// the success and failure paths: a failed turn whose message_start already
// reported input tokens was billed for them, and a session aggregate that
// omits it under-reports by exactly the turns that went wrong.
func (d *decodeState) price(msg *core.AssistantMessage, m *core.Model, lookup func(string) *core.Model) {
	billModel, billed := provider.BillingModel(m, msg.ResponseModel, lookup)
	msg.Usage.BilledModel = billed
	if msg.Usage.Reported() {
		msg.Usage.SetCost(provider.ComputeCost(billModel, msg.Usage))
	}
}

// fail is REQ-PROV-04: the partial content and the failure are ONE value.
//
// Half an assistant message followed by a truncated stream is not an error
// with a message thrown away — the retry classifier reads the text and session
// persistence keeps the content, and neither works if the two are separated.
func (d *decodeState) fail(text string, err error) {
	final := d.partial
	final.Content = d.partialContent()
	final.Usage = d.usage
	final.Usage.BilledModel = ""
	d.price(&final, d.model, d.lookup)
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

// partialContent is what a failed or aborted stream keeps: every completed
// block, then every block still open when the stream died, salvaged the same
// way the other wires' per-chunk snapshots keep theirs. REQ-LOOP-09 appends
// this message to history verbatim; the half-streamed text is what the UI and
// the session log display, so dropping an open block loses it twice.
func (d *decodeState) partialContent() core.Content {
	out := append(core.Content(nil), d.partial.Content...)
	for _, i := range d.order {
		if _, done := d.final[i]; done {
			continue
		}
		if acc := d.accs[i]; acc != nil {
			if b := acc.block(); b != nil {
				out = append(out, b)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- whole-response

// DecodeResponse decodes a NON-streaming Messages response.
//
// It exists for REQ-PROV-17's conformance test: the streaming and whole
// response paths must produce byte-identical ToolUseBlock.Input for the same
// call. Sharing the assembler is what makes that true by construction rather
// than by coincidence, and DecodeResponse is how the test can say so.
func DecodeResponse(m *core.Model, data []byte, lookup func(string) *core.Model) (*core.AssistantMessage, error) {
	if err := provider.GuardUntrusted(data); err != nil {
		return nil, fmt.Errorf("anthropic: decoding response: %w", err)
	}
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
		acc, err := startFrom(raw, true)
		if err != nil {
			return nil, fmt.Errorf("anthropic: decoding response: %w", err)
		}
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

// effortTokens is the output_config.effort vocabulary this adapter will send.
// "minimal" is not in it: no Anthropic model has that level, so a row that
// wants minimal to mean something maps it to "low" itself, and a request that
// reaches here as minimal is priced as low rather than rejected.
var effortTokens = map[string]string{
	"minimal": "low",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"xhigh":   "xhigh",
	"max":     "max",
}

// minThinkingBudget is the vendor minimum for budget_tokens; a smaller value
// is a 400.
const minThinkingBudget = 1024

// applyThinking is REQ-PROV-15's Anthropic arm: a TRI-state where undefined
// omits the key entirely and an explicit "off" sends {"type":"disabled"} —
// when the row says the model accepts it.
//
// The catalog row's wire value for a level decides which generation of the
// control is sent:
//
//   - an INTEGER is a thinking budget: {"type":"enabled","budget_tokens":N},
//     held below max_tokens and never below the vendor minimum;
//   - an EFFORT token (low … max) is the current control:
//     {"type":"adaptive"} plus output_config.effort, which is what every
//     model since Claude 4.6 takes and what budget_tokens is a 400 on.
//
// The requested level is CLAMPED here — upward first, then downward — and the
// RETURNED wire value is what is priced; an unclamped level never reaches the
// wire (REQ-PROV-15: "passing an unclamped level through is prohibited").
//
// `off` skips the clamp (ruling P-27: a request for some thinking is never
// clamped down to none, and a request for none is never clamped up to some)
// but NOT the catalog. A row that maps off to a wire value sends
// {"type":"disabled"}; a row that records off as present-and-null, or whose
// ladder has no off entry, omits the key — on the models that cannot stop
// thinking, disabled is a 400, and omission is the least thinking they offer.
// A descriptor with no ladder at all also omits: the adapter sends disabled
// only where something says the model accepts it.
func applyThinking(r *request, m *core.Model, requested core.ThinkingLevel) {
	switch requested {
	case core.ThinkingUnset:
		return
	case core.ThinkingOff:
		if _, ok := catalog.ThinkingWire(m, core.ThinkingOff); ok {
			r.Thinking = &thinking{Type: "disabled"}
		}
		return
	}
	_, wire, ok := catalog.ClampThinkingLevel(m, requested)
	if !ok {
		return // no reachable level: omit rather than guess
	}
	wire = strings.ToLower(strings.TrimSpace(wire))

	if n, err := strconv.Atoi(wire); err == nil {
		applyBudget(r, n)
		return
	}
	effort, known := effortTokens[wire]
	if !known {
		// Neither a budget nor an effort token. Omitting is the only request
		// that is not a 400: `enabled` without budget_tokens is, and so is an
		// effort the model has never heard of.
		return
	}
	r.Thinking = &thinking{Type: "adaptive"}
	r.OutputConfig = &outputConfig{Effort: effort}
	// Thinking rejects any explicit temperature or top_p. Dropping them is the
	// only option that keeps the request valid; the alternative is a 400 that
	// names sampling and not thinking, sending the reader to the wrong knob.
	r.Temperature, r.TopP = nil, nil
}

// applyBudget is the budget_tokens arm of applyThinking.
func applyBudget(r *request, n int) {
	// Anthropic rejects a thinking budget that is not strictly below
	// max_tokens. The budget is the value we may lower; max_tokens has
	// already been clamped against the context window (REQ-CAT-04) and
	// lowering it again would silently truncate the answer instead.
	if n >= r.MaxTokens {
		n = r.MaxTokens - 1
	}
	if n < minThinkingBudget {
		// Below the vendor minimum — after the clamp, which is where this
		// happens in practice: a small max_tokens deep into a long session
		// leaves no room for the smallest legal budget. A sub-minimum budget
		// is a 400; no thinking is the answer that still returns.
		return
	}
	r.Thinking = &thinking{Type: "enabled", BudgetTokens: &n}
	r.Temperature, r.TopP = nil, nil
}
