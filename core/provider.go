package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Request is the canonical, provider-independent model call. Messages are the
// RAW canonical history: REQ-PROV-11 repair happens inside the provider, never
// here, because the loop is not running when a transcript is loaded from disk
// and no caller may be able to skip it.
type Request struct {
	System   []ContentBlock
	Messages Messages
	// Tools is []ToolWire, not []Tool. That is the enforcement of REQ-TOOL-01.
	Tools         []ToolWire
	ToolChoice    ToolChoice
	MaxTokens     *int     // upper bound; clamped by REQ-CAT-04
	Temperature   *float64 // REQ-PROV-16 presence
	TopP          *float64
	StopSequences []string
	ThinkingLevel ThinkingLevel
	// EstContextTokens is the REQ-GO-15 anchored estimate, supplied by the
	// loop. Providers never re-walk the transcript to estimate; without this
	// field the REQ-CAT-04 clamp is either wrong or duplicated per provider.
	EstContextTokens int
	Options          RequestOptions
}

type ProviderStreamOptions struct {
	// Warnf is nil for a no-op. Never a global logger.
	Warnf          func(format string, args ...any)
	CacheRetention CacheRetention
}

// StreamFunc is the registry-facing form.
type StreamFunc func(ctx context.Context, model *Model, req Request, opts ProviderStreamOptions) *EventStream

// ProviderClient has exactly one required method (REQ-PROV-01). Streaming is
// the primitive; Complete is derived once in the SDK so a provider has exactly
// one place that parses its wire format.
type ProviderClient interface {
	Stream(ctx context.Context, model *Model, req Request, opts ProviderStreamOptions) *EventStream
}

type ClientFunc StreamFunc

func (f ClientFunc) Stream(ctx context.Context, m *Model, r Request, o ProviderStreamOptions) *EventStream {
	return f(ctx, m, r, o)
}

// Complete is the ONE derivation of the non-streaming path (REQ-PROV-01). It
// returns no error: failures live in the message (REQ-PROV-04).
func Complete(ctx context.Context, p ProviderClient, m *Model, r Request, o ProviderStreamOptions) *AssistantMessage {
	return p.Stream(ctx, m, r, o).Result()
}

type APIProvider struct {
	API    API
	Stream StreamFunc
}

// ProviderRegistry is held on AgentConfig, never in a package-level global,
// and is never populated by import side effect (REQ-PROV-09, NFR-SEC-05).
type ProviderRegistry map[API]APIProvider

func (r ProviderRegistry) Register(p APIProvider) { r[p.API] = p }

func (r ProviderRegistry) Get(a API) (APIProvider, bool) {
	p, ok := r[a]
	return p, ok
}

// Dispatch is the single call site every agent turn goes through.
// RequestOptions.StreamFn (REQ-PROV-18) shadows the registry entirely.
func (r ProviderRegistry) Dispatch(ctx context.Context, m *Model, req Request, o ProviderStreamOptions) *EventStream {
	if req.Options.StreamFn != nil {
		return req.Options.StreamFn(ctx, m, req, o)
	}
	p, ok := r.Get(m.API)
	if !ok {
		return ErrorStream(nil, fmt.Errorf(
			"agentkit: no provider registered for api %q (model %q, vendor %q); "+
				"set AgentConfig.Providers or call agentkit.RegisterDefaults(&cfg)",
			m.API, m.ID, m.Provider))
	}
	return p.Stream(ctx, m, req, o)
}

// RequestOptions is REQ-PROV-18's escape hatch, applied to every provider call.
type RequestOptions struct {
	// Headers merges into every request. A present-nil value is a DELETION
	// MARKER suppressing a provider default of that name (REQ-AUTH-02); no
	// string value can express that.
	Headers         map[string]*string
	TimeoutMs       *int
	MaxRetries      *int // nil => 0 (OQ-9)
	MaxRetryDelayMs *int // nil => 60000
	SessionID       string
	CacheRetention  *CacheRetention
	// Env is consulted before os.Getenv (REQ-AUTH-03). An empty override falls
	// through rather than masking.
	Env       map[string]string
	Transport http.RoundTripper
	StreamFn  StreamFunc
	// OnPayload runs after canonical->wire translation and before the first
	// byte. Returning (nil, nil) leaves the payload unchanged.
	OnPayload  func(payload any, model *Model) (any, error)
	OnResponse func(resp *http.Response, model *Model) error
}

// Model is REQ-PROV-10's descriptor. Provider is a VENDOR id used only for
// credential resolution and catalog lookup; API selects the implementation.
type Model struct {
	ID            string                    `json:"id"`
	Name          string                    `json:"name"`
	API           API                       `json:"api"`
	Provider      string                    `json:"provider"`
	BaseURL       string                    `json:"base_url"`
	Headers       map[string]*string        `json:"headers,omitzero"`
	Compat        json.RawMessage           `json:"compat,omitzero"`
	ContextWindow int                       `json:"context_window"`
	MaxTokens     int                       `json:"max_tokens"`
	Cost          Cost                      `json:"cost"`
	Input         []string                  `json:"input"` // modalities: "text","image"
	Reasoning     bool                      `json:"reasoning"`
	// ThinkingLevelMap distinguishes present-null ("explicitly unsupported")
	// from absent. Both mean unsupported for clamping; the distinction is
	// catalog-authoring metadata for the REQ-CAT-06 diff, not a runtime
	// semantic, and REQ-PROV-15 should say so.
	ThinkingLevelMap map[ThinkingLevel]*string `json:"thinking_level_map,omitzero"`
	Cloned           bool                      `json:"-"` // REQ-CAT-03
	ClonedFrom       string                    `json:"-"`
}

func (m *Model) SupportsImages() bool {
	for _, in := range m.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

type Cost struct {
	Input      float64    `json:"input"` // USD per 1M tokens
	Output     float64    `json:"output"`
	CacheRead  float64    `json:"cache_read"`
	CacheWrite float64    `json:"cache_write"`
	Tiers      []CostTier `json:"tiers,omitzero"` // REQ-PROV-05.4
}

type CostTier struct {
	Threshold  int     `json:"threshold"` // strictly exceeded
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}
