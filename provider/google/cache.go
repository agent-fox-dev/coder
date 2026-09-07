package google

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

// This file is §6.2a Level 1's Gemini arm and NFR-PERF-08.
//
// "For sessions with a large, stable system prompt (>32k tokens), AgentKit
// optionally creates an explicit CachedContent resource and references it in
// subsequent calls. For smaller prompts, implicit caching applies
// automatically. context_cache_ttl defaults to 60 minutes."
//
// The structural half of NFR-PERF-08 is the part that is testable offline and
// the part that is easy to get wrong: creation is a NETWORK CALL, it runs on a
// background goroutine started before the first model call, and THE AGENT LOOP
// MUST NOT BLOCK WAITING FOR IT. A turn whose cache has not been created yet
// goes out uncached — a slightly more expensive request, not a stalled one.
// The obvious implementation (create, then send) turns every session's first
// turn into two serial round trips and makes a hung creation a hung agent.

const (
	// DefaultContextCacheTTL is §6.2a's context_cache_ttl default.
	DefaultContextCacheTTL = 60 * time.Minute
	// DefaultContextCacheMinTokens is §6.2a's ">32k tokens" threshold. Below
	// it the explicit resource is not worth a creation call: Gemini's implicit
	// caching already applies, and an explicit CachedContent has a minimum
	// size the service itself enforces.
	DefaultContextCacheMinTokens = 32768
	// defaultContextCacheTimeout bounds one creation attempt. It is generous
	// because nothing waits on it, and bounded because a goroutine holding a
	// socket for a dead endpoint is a leak.
	defaultContextCacheTimeout = 60 * time.Second
	// charsPerToken is REQ-GO-15's 4:1 heuristic, repeated rather than
	// imported because the estimator lives in the root package, which this one
	// cannot import. The threshold is an order-of-magnitude gate, not an
	// accounting figure.
	charsPerToken = 4
)

// ContextCacheOptions turns the explicit CachedContent path ON. It is opt-in
// (a nil Options.ContextCache) because creation spends a request and a
// provider-side resource that the caller pays for, and because the implicit
// cache already covers the small-prompt case at no cost.
type ContextCacheOptions struct {
	// TTL is context_cache_ttl. Zero means DefaultContextCacheTTL.
	TTL time.Duration
	// MinTokens overrides §6.2a's 32k threshold. Zero means the default.
	MinTokens int
	// Timeout bounds one creation attempt. Zero means the default.
	Timeout time.Duration
	// OnCreate reports the outcome of a creation attempt: the resource name on
	// success, the error otherwise. It is the only way an embedder — or a test
	// — can observe a background operation that by construction nothing waits
	// for.
	OnCreate func(name string, err error)
}

func (o *ContextCacheOptions) ttl() time.Duration {
	if o != nil && o.TTL > 0 {
		return o.TTL
	}
	return DefaultContextCacheTTL
}

func (o *ContextCacheOptions) minTokens() int {
	if o != nil && o.MinTokens > 0 {
		return o.MinTokens
	}
	return DefaultContextCacheMinTokens
}

func (o *ContextCacheOptions) timeout() time.Duration {
	if o != nil && o.Timeout > 0 {
		return o.Timeout
	}
	return defaultContextCacheTimeout
}

// cachedContentRequest is the CachedContent creation body.
type cachedContentRequest struct {
	Model             string      `json:"model"`
	SystemInstruction *content    `json:"systemInstruction,omitzero"`
	Tools             []toolSet   `json:"tools,omitzero"`
	ToolConfig        *toolConfig `json:"toolConfig,omitzero"`
	// TTL is a protobuf Duration: seconds with an "s" suffix.
	TTL string `json:"ttl,omitzero"`
}

// cacheJob is one pending creation: the prefix to cache and the key it is
// filed under.
type cacheJob struct {
	key  string
	body cachedContentRequest
	opts *ContextCacheOptions
}

type cacheEntry struct {
	once sync.Once
	mu   sync.RWMutex
	name string
}

func (e *cacheEntry) get() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.name
}

func (e *cacheEntry) set(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.name = name
}

func (c *client) entry(key string) *cacheEntry {
	v, _ := c.caches.LoadOrStore(key, &cacheEntry{})
	return v.(*cacheEntry)
}

// contextCacheJob decides whether this request has a prefix worth caching and
// what that prefix is.
//
// The prefix is the whole stable head of the request — systemInstruction,
// tools and toolConfig — not just the system prompt: a CachedContent resource
// and a request may not both carry those fields, so whatever is cached must be
// EVERYTHING that is withheld. Keying on their exact bytes is what makes the
// entry self-invalidating: a changed system prompt or an added tool produces a
// different key, a new background creation, and uncached operation in the
// meantime, rather than a stale resource silently conditioning the model.
func (c *client) contextCacheJob(m *core.Model, body *request) *cacheJob {
	o := c.opts.ContextCache
	if o == nil || body.SystemInstruction == nil {
		return nil
	}
	chars := 0
	for _, p := range body.SystemInstruction.Parts {
		chars += len(p.Text)
	}
	if chars/charsPerToken < o.minTokens() {
		// Below the threshold the implicit cache applies automatically and an
		// explicit resource buys a round trip for nothing (§6.2a).
		return nil
	}

	j := &cacheJob{opts: o, body: cachedContentRequest{
		SystemInstruction: body.SystemInstruction,
		Tools:             body.Tools,
		ToolConfig:        body.ToolConfig,
		TTL:               strconv.Itoa(int(o.ttl().Seconds())) + "s",
	}}
	h := sha256.New()
	fmt.Fprint(h, m.ID, "\x00")
	enc := json.NewEncoder(h)
	_ = enc.Encode(body.SystemInstruction)
	_ = enc.Encode(body.Tools)
	_ = enc.Encode(body.ToolConfig)
	j.key = hex.EncodeToString(h.Sum(nil))
	return j
}

// attachCachedContent references an ALREADY-CREATED resource, and reports
// whether it did.
//
// This is the non-blocking read NFR-PERF-08 turns on: it takes a mutex around
// a string and never waits on the creation goroutine, so a session whose cache
// is still being created sends the ordinary uncached request instead of
// stalling behind a network call it did not need.
func (c *client) attachCachedContent(body *request, j *cacheJob) bool {
	name := c.entry(j.key).get()
	if name == "" {
		return false
	}
	body.CachedContent = name
	// The cached resource carries these; sending them again alongside
	// cachedContent is rejected outright.
	body.SystemInstruction = nil
	body.Tools = nil
	body.ToolConfig = nil
	return true
}

// startContextCache launches creation once per prefix, on a background
// goroutine, and returns immediately (NFR-PERF-08).
//
// The context is DETACHED from the request's. The whole point of creating in
// the background is that the resource is there for the NEXT turn, and the next
// turn happens after this one's context is cancelled — a creation inheriting
// that context is cancelled at exactly the moment its result becomes useful,
// so the session falls back to uncached forever and the cost looks like the
// feature simply does not work.
func (c *client) startContextCache(ctx context.Context, j *cacheJob, m *core.Model, vx Vertex,
	base string, auth provider.ModelAuth, env provider.Env, opts core.RequestOptions) {
	e := c.entry(j.key)
	e.once.Do(func() {
		body := j.body
		body.Model = vx.modelResource(m)
		url := base + vx.cachePath()
		go func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), j.opts.timeout())
			defer cancel()
			name, err := createCachedContent(cctx, c, url, body, m, auth, env, opts)
			if err == nil && name != "" {
				e.set(name)
			}
			if fn := j.opts.OnCreate; fn != nil {
				fn(name, err)
			}
		}()
	})
}

// cachePath and modelResource are the two spellings NFR-COMPAT-05 already
// established for generateContent, applied to the cache resource.
func (v Vertex) cachePath() string {
	if !v.On() {
		return "/v1beta/cachedContents"
	}
	return "/v1/projects/" + v.Project + "/locations/" + v.Location + "/cachedContents"
}

func (v Vertex) modelResource(m *core.Model) string {
	id := m.ID
	if !v.On() {
		if !strings.HasPrefix(id, "models/") {
			id = "models/" + id
		}
		return id
	}
	if strings.HasPrefix(id, "projects/") {
		return id
	}
	return "projects/" + v.Project + "/locations/" + v.Location +
		"/publishers/google/models/" + strings.TrimPrefix(id, "models/")
}

func createCachedContent(ctx context.Context, c *client, url string, body cachedContentRequest,
	m *core.Model, auth provider.ModelAuth, env provider.Env, opts core.RequestOptions) (string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	// The same Call pipeline the model request uses: identical credential
	// resolution, header precedence and injected transport (REQ-PROV-18), so
	// the cache is created against exactly the endpoint the request goes to.
	// OnPayload/OnResponse are deliberately NOT run — they are hooks on the
	// model call, and a caller rewriting a generateContent body must not have
	// it applied to a resource creation.
	call := provider.Call{
		Method: http.MethodPost,
		URL:    url,
		Body:   raw,
		Headers: map[string]string{
			"content-type": "application/json",
			"accept":       "application/json",
		},
		Auth: auth, Model: m,
		Options:     core.RequestOptions{Headers: opts.Headers, Env: opts.Env, Transport: opts.Transport},
		Attribution: c.opts.Attribution, Env: env,
		Client: c.opts.HTTPClient, Retry: c.opts.Retry,
	}
	resp, err := call.Do(ctx)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New(provider.StatusError("google: creating cachedContent", resp, provider.JSONErrorDetail))
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Name, nil
}
