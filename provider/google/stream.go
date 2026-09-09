package google

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// DefaultBaseURL is the Generative Language endpoint. The Vertex AI path uses
// a different host and resolves to the AMBIENT credential state of
// REQ-AUTH-04 (NFR-COMPAT-05); set it through the catalog row or
// GOOGLE_GEMINI_BASE_URL.
const DefaultBaseURL = "https://generativelanguage.googleapis.com"

const (
	// vertexHostSuffix identifies a Vertex AI endpoint. Every Vertex host is
	// either this or "<location>-" + this.
	vertexHostSuffix = "aiplatform.googleapis.com"
	// VertexGlobalLocation is the location used when none is configured. The
	// global endpoint is the one that needs no regional host prefix, so it is
	// the only safe default for a deployment that named a project and nothing
	// else.
	VertexGlobalLocation = "global"
)

// ResolveVertex decides NFR-COMPAT-05's deployment from configuration alone.
//
// The switch is a CONFIG change, and it can be spelled two ways, because the
// two halves of a Vertex deployment arrive from different places in practice:
//
//   - project (Options.VertexProject, else GOOGLE_CLOUD_PROJECT /
//     CLOUDSDK_CORE_PROJECT) selects the Vertex path shape and, with no base
//     URL configured, the Vertex host;
//   - a Vertex base URL (catalog row or GOOGLE_GEMINI_BASE_URL) selects it
//     too, with the project supplied by the environment.
//
// The environment variables alone do NOT flip a deployment to Vertex: they are
// set on every GCE/Cloud Run instance and are also what VendorAuth.Ambient
// reads, so treating them as the switch would break every AI Studio API-key
// deployment that happens to run on Google Cloud. Something has to name Vertex
// explicitly — a project on the provider Options, or a Vertex host.
//
// A Vertex host with no resolvable project is an error rather than a silent
// fallback: the AI Studio path shape on a Vertex host is a 404 several layers
// down, and the message that produces says nothing about the missing project.
func ResolveVertex(base, project, location string, env provider.Env) (Vertex, error) {
	onHost := isVertexHost(base)
	if project == "" && !onHost {
		return Vertex{}, nil
	}
	if project == "" {
		project = firstEnv(env, "GOOGLE_CLOUD_PROJECT", "CLOUDSDK_CORE_PROJECT")
	}
	if project == "" {
		return Vertex{}, errors.New("google: the Vertex AI endpoint needs a project: " +
			"set Options.VertexProject or GOOGLE_CLOUD_PROJECT")
	}
	if location == "" {
		location = firstEnv(env, "GOOGLE_CLOUD_LOCATION", "CLOUDSDK_COMPUTE_REGION")
	}
	if location == "" {
		location = locationFromHost(base)
	}
	if location == "" {
		location = VertexGlobalLocation
	}
	return Vertex{Project: project, Location: location}, nil
}

func firstEnv(env provider.Env, names ...string) string {
	for _, n := range names {
		if v := env.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// hostOf is deliberately string surgery rather than net/url parsing: the base
// URL may be a bare host, and a parse failure must not decide a deployment.
func hostOf(base string) string {
	h := base
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/:"); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

func isVertexHost(base string) bool {
	h := hostOf(base)
	return h == vertexHostSuffix || strings.HasSuffix(h, "-"+vertexHostSuffix)
}

// locationFromHost reads the region out of a regional Vertex host, so
// "https://us-central1-aiplatform.googleapis.com" needs no second config
// field to say what it already says.
func locationFromHost(base string) string {
	h := hostOf(base)
	if s, ok := strings.CutSuffix(h, "-"+vertexHostSuffix); ok {
		return s
	}
	return ""
}

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
	// VertexProject and VertexLocation select the Vertex AI deployment
	// (NFR-COMPAT-05). Setting the project is the whole switch: it selects the
	// Vertex path shape and, with no base URL configured, the regional Vertex
	// host. Both fall back to the environment — see ResolveVertex — so a
	// deployment can also flip with no code change at all.
	VertexProject  string
	VertexLocation string
	// Credentials is REQ-AUTH-05's application-owned store. When set it is
	// consulted BEFORE the environment table, because it is the layer that can
	// hold a refreshed OAuth token and the environment is static. An empty
	// store falls through, so adding one never breaks a working env setup.
	Credentials      *provider.Credentials
	MaxSSEEventBytes int
	// ToolPrefix is REQ-CACHE-06's per-session schema cache. Nil means this
	// provider value owns one, which is the right scope in practice: a
	// registry is built per agent config. Pass one explicitly to share it, or
	// to read its reconciliation reports.
	ToolPrefix *provider.ToolPrefix
	// OnToolPrefixSync reports each reconciliation, so an embedder can feed
	// REQ-CACHE-11's prefix-invalidation counter.
	OnToolPrefixSync func(provider.SyncReport)
	// ContextCache opts this provider into §6.2a Level 1's explicit
	// CachedContent resource (NFR-PERF-08). Nil — the default — leaves Gemini's
	// implicit caching to do its work and makes no creation call.
	ContextCache *ContextCacheOptions
}

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
	// caches is the §6.2a Level 1 CachedContent resource per cached prefix.
	// It is a value on the provider, like ToolPrefix, and never a package
	// global: two agents in one process hold different system prompts.
	caches sync.Map // prefix key -> *cacheEntry
}

func (c *client) Stream(ctx context.Context, m *core.Model, req core.Request, o core.ProviderStreamOptions) *core.EventStream {
	body, rep, sync, err := BuildRequestCached(m, req, c.prefix)
	if err != nil {
		return core.ErrorStream(nil, fmt.Errorf("google: building request: %w", err))
	}
	if fn := c.opts.OnToolPrefixSync; fn != nil {
		fn(sync)
	}
	if rep.Changed() && o.Warnf != nil {
		o.Warnf("google: %s", rep.String())
	}

	// §6.2a Level 1 / NFR-PERF-08. The lookup is a map read and a mutex: if
	// the resource for this prefix exists and has not expired it is
	// referenced and the prefix is withheld from the body; if creation is
	// still in flight — or has never started — this request goes out
	// uncached. Nothing here waits.
	job := c.contextCacheJob(m, body)
	var attached *cacheRef
	if job != nil {
		if attached = c.attachCachedContent(body, job); attached != nil {
			job = nil
		}
	}

	encode := func(body *request) ([]byte, error) {
		var payload any = body
		if fn := req.Options.OnPayload; fn != nil {
			out, perr := fn(body, m)
			if perr != nil {
				return nil, perr
			}
			if out != nil {
				payload = out
			}
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("google: encoding request: %w", err)
		}
		return raw, nil
	}
	raw, err := encode(body)
	if err != nil {
		return core.ErrorStream(nil, err)
	}
	// The uncached form of the same request, encoded only if the cached one
	// is refused (run): a resource the service no longer holds is a 4xx on a
	// request that was otherwise fine.
	var fallback func() ([]byte, error)
	if attached != nil {
		uncached := *body
		attached.restore(&uncached)
		fallback = func() ([]byte, error) { return encode(&uncached) }
	}

	s := core.NewEventStream(core.StreamOptions{})
	go c.run(ctx, s, m, req, raw, job, attached, fallback)
	return s
}

func (c *client) run(ctx context.Context, s *core.EventStream, m *core.Model, req core.Request,
	raw []byte, job *cacheJob, attached *cacheRef, fallback func() ([]byte, error)) {
	now := time.Now
	if c.opts.Now != nil {
		now = c.opts.Now
	}
	d := &decoder{s: s, partial: core.AssistantMessage{
		Provider: m.Provider, API: m.API, Model: m.ID,
		ThinkingLevel: req.ThinkingLevel, Timestamp: now(),
	}}

	// caller is the ctx handed to Stream; ctx may become its TimeoutMs child.
	// The caller's expiry is an abort (REQ-LOOP-09), the child's a retryable
	// timeout (REQ-PROV-18) — see provider.TransportErrorText.
	caller := ctx
	if to := req.Options.TimeoutMs; to != nil && *to > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*to)*time.Millisecond)
		defer cancel()
	}

	env := provider.Env{Override: req.Options.Env, Getenv: c.opts.Getenv}
	auth, err := provider.ResolveAuthWith(ctx, m.Provider, c.opts.Credentials, VendorAuth, env)
	if err != nil {
		d.fail(provider.TransportErrorText("google", caller, ctx, err), err)
		return
	}

	base := c.opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	// NFR-COMPAT-05: the deployment is resolved from config, never from a
	// different provider implementation. The base URL is resolved FIRST
	// because an operator may have named the Vertex host there (catalog row or
	// GOOGLE_GEMINI_BASE_URL) and said nothing else.
	url := provider.ResolveBaseURL(m, auth, base)
	vx, err := ResolveVertex(url, c.opts.VertexProject, c.opts.VertexLocation, env)
	if err != nil {
		d.fail(err.Error(), err)
		return
	}
	// A project configured with no base URL anywhere: follow it to the Vertex
	// host rather than sending a Vertex path to AI Studio. An explicitly
	// configured base URL (a proxy, a gateway) is left exactly as it is.
	if vx.On() && url == strings.TrimRight(DefaultBaseURL, "/") {
		url = vx.BaseURL()
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
		URL:     url + vx.Path(m, true),
		Body:    raw,
		Headers: headers,
		Auth:    auth, Model: m, Options: req.Options,
		Attribution: c.opts.Attribution, Env: env,
		Client: c.opts.HTTPClient, Retry: c.opts.Retry,
	}

	// NFR-PERF-08: creation starts BEFORE the first model call and the loop
	// does not wait for it — startContextCache launches a goroutine and
	// returns. The model request below goes out uncached this turn.
	if job != nil {
		c.startContextCache(ctx, job, m, vx, url, auth, env, req.Options)
	}

	resp, err := call.Do(ctx)
	if err != nil {
		if errors.Is(err, provider.ErrRetryDelayTooLong) {
			d.fail(err.Error(), err)
			return
		}
		d.fail(provider.TransportErrorText("google", caller, ctx, err), err)
		return
	}
	if attached != nil && fallback != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
		// The request referenced a CachedContent resource and the service
		// refused it: the resource expired or was deleted under us, or the
		// service rejected the pairing. The entry is cleared — the next turn
		// starts a fresh creation — and THIS turn is retried once without
		// the reference rather than failed for a cache it never needed.
		_ = resp.Body.Close()
		c.entry(attached.key).clear(attached.name)
		if call.Body, err = fallback(); err != nil {
			d.fail(err.Error(), err)
			return
		}
		attached = nil
		if resp, err = call.Do(ctx); err != nil {
			if errors.Is(err, provider.ErrRetryDelayTooLong) {
				d.fail(err.Error(), err)
				return
			}
			d.fail(provider.TransportErrorText("google", caller, ctx, err), err)
			return
		}
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
		d.fail(provider.StreamErrorText("google", caller, ctx, err), err)
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
	// textBuf and thinkBuf accumulate the open text and thought blocks.
	// Builder.String() is a view, not a copy, so refreshing the block after
	// each delta is O(delta); appending to the block's own string was O(block)
	// per delta and quadratic over a long answer.
	textBuf  strings.Builder
	thinkBuf strings.Builder
	usage    core.Usage
	finishRs string
	respID   string
	respMod  string
	calls    int
	sawAny   bool
	// nonce scopes synthesized tool-call ids when the response carries no id
	// of its own; see callScope.
	nonce string
}

// ThoughtMarkerSignature marks a Gemini thought part that arrived WITHOUT a
// thoughtSignature.
//
// It is not a credential and replays nothing: the encoder drops a block
// carrying it (encodeParts), because on this wire only the signature on a
// functionCall part matters to the model. It exists so the block is not
// demoted to text by REQ-PROV-11 rule 4 and re-sent as the model's own
// visible prose on every later turn — re-billed each time, and conditioning
// the model on its reasoning as if it had said it. Rule 3 still downgrades
// it for another model, where the text is at least real context.
const ThoughtMarkerSignature = "google-generative-ai/thought"

func (d *decoder) consume(r *provider.SSEReader) error {
	d.textIdx, d.thinkIdx = -1, -1
	for {
		ev, err := r.Next()
		if err == io.EOF {
			if !d.sawAny || d.finishRs == "" {
				// A finishReason on the final candidate is this wire's
				// terminal signal. Without it the stream stopped before the
				// model did, and what arrived is a partial turn — a partial
				// functionCall included, which reported as tool_use would be
				// executed.
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
					d.thinkBuf.Reset()
					// The marker until a real signature arrives; a later
					// part's signature replaces it.
					d.blocks = append(d.blocks, core.ThinkingBlock{Signature: ThoughtMarkerSignature})
					d.s.Push(core.ThinkingStartEvent{BlockIndex: d.thinkIdx})
				}
				tb := d.blocks[d.thinkIdx].(core.ThinkingBlock)
				d.thinkBuf.WriteString(p.Text)
				tb.Thinking = d.thinkBuf.String()
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
					d.textBuf.Reset()
					d.blocks = append(d.blocks, core.TextBlock{})
					d.s.Push(core.TextStartEvent{BlockIndex: d.textIdx})
				}
				tb := d.blocks[d.textIdx].(core.TextBlock)
				d.textBuf.WriteString(p.Text)
				tb.Text = d.textBuf.String()
				d.blocks[d.textIdx] = tb
				d.s.Push(core.TextDeltaEvent{BlockIndex: d.textIdx, Delta: p.Text})
				changed = true
				if p.ThoughtSignature != "" {
					d.keepSignature(p.ThoughtSignature)
				}

			case p.ThoughtSignature != "":
				// A part carrying nothing but a signature.
				d.keepSignature(p.ThoughtSignature)
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

// keepSignature records a thoughtSignature that arrived on a part the
// canonical layer has no signature field for — a text part, or a part that
// is nothing but the signature.
//
// Gemini 3 attaches the turn's signature to the LAST text part when the turn
// makes no function call, and a replay without it loses the chain (and on
// some models is rejected). A ThinkingBlock carrying only the signature is
// emitted at that position; the encoder sends it back as a signature-only
// part. It is a REAL signature, never the marker, so rule 4 keeps it.
func (d *decoder) keepSignature(sig string) {
	idx := len(d.blocks)
	d.blocks = append(d.blocks, core.ThinkingBlock{Signature: sig})
	d.s.Push(core.ThinkingStartEvent{BlockIndex: idx})
	// The signature closes the text run it rode on: later text is a new
	// block, so the signature stays at the position it arrived in.
	d.textIdx, d.thinkIdx = -1, -1
}

// callScope namespaces a synthesized tool-call id so two turns cannot mint
// the same one — the repair pass keys results by id, and a repeat lets a
// result answer the wrong call. The response id is the scope wherever the
// API supplies one; without it a per-response nonce is minted rather than
// falling back to the model name, which repeated "<model>-0" on every turn.
func (d *decoder) callScope() string {
	if d.respID != "" {
		return d.respID
	}
	if d.nonce == "" {
		d.nonce = newNonce()
	}
	if d.respMod != "" {
		return d.respMod + "-" + d.nonce
	}
	return "call-" + d.nonce
}

func newNonce() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
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
