package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/openai"
)

// Options configures the client.
type Options struct {
	BaseURL       string
	HTTPClient    *http.Client
	Getenv        func(string) string
	Retry         provider.RetryPolicy
	Attribution   *bool
	BillingLookup func(string) *core.Model
	Now           func() time.Time
	Credentials   *provider.Credentials
	Auth          *provider.VendorAuth
	// ServiceTier is REQ-PROV-05's post-hoc multiplier for this wire.
	//
	// It is a client option rather than a per-request field because the tier
	// is an account-level commercial arrangement, not something a turn
	// chooses. A zero Multiplier leaves cost untouched.
	ServiceTier      provider.ServiceTier
	MaxSSEEventBytes int
}

// AuthFor DELEGATES to the Chat Completions table.
//
// This is the same vendor and the same key over a different wire, so a second
// table would be one more place for OPENAI_API_KEY to be spelled — and the
// failure of the copy that drifted is a 401 with no explanation.
func AuthFor(vendor string) provider.VendorAuth { return openai.AuthFor(vendor) }

// Provider returns the registry entry (REQ-PROV-09).
func Provider(opts Options) core.APIProvider {
	c := &client{opts: opts}
	return core.APIProvider{API: API, Stream: c.Stream}
}

type client struct{ opts Options }

func (c *client) Stream(ctx context.Context, m *core.Model, req core.Request, o core.ProviderStreamOptions) *core.EventStream {
	body, rep, err := BuildRequest(m, req)
	if err != nil {
		return core.ErrorStream(nil, fmt.Errorf("openai-responses: building request: %w", err))
	}
	if rep.Changed() && o.Warnf != nil {
		o.Warnf("openai-responses: %s", rep.String())
	}
	if len(req.StopSequences) > 0 && o.Warnf != nil {
		// The Responses API has no stop-sequence parameter. Silence here is
		// exactly the failure the NFR-TEST-08 request golden caught on the
		// Chat Completions wire: a caller's stop condition that never takes
		// effect and nothing saying so.
		o.Warnf("openai-responses: the Responses API has no stop-sequence " +
			"parameter; the request's stop sequences were not sent")
	}
	if c.opts.ServiceTier.Name != "" {
		body.ServiceTier = c.opts.ServiceTier.Name
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
		return core.ErrorStream(nil, fmt.Errorf("openai-responses: encoding request: %w", err))
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
		d.fail(provider.TransportErrorText("openai-responses", ctx, err), err)
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
		d.fail(provider.TransportErrorText("openai-responses", ctx, err), err)
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
		msg := provider.StatusError("openai-responses", resp, provider.JSONErrorDetail)
		d.fail(msg, errors.New(msg))
		return
	}

	d.s.Push(core.MessageStartEvent{Message: d.partial})
	if err := d.consume(provider.NewSSEReader(resp.Body, c.opts.MaxSSEEventBytes)); err != nil {
		d.fail(err.Error(), err)
		return
	}
	d.finish(m, c.opts.BillingLookup, c.opts.ServiceTier)
}
