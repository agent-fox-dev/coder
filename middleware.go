package agentkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// This file is Axis 1: middleware wrapping the whole model call, operating on
// CANONICAL types. It can change *what* is asked for, not *how* the provider
// encoded it — post-serialization interception is RequestOptions.OnPayload.
//
// Compaction is deliberately NOT here. It is a context transform inside the
// loop (REQ-GO-12), because a compaction middleware's own summarization call
// would re-enter the chain, be fingerprinted by the dedup cache, and be
// charged against the budget gate as though it were a conversational turn.

// ---------------------------------------------------------------- retry

// RetryOptions configures RetryMiddleware.
type RetryOptions struct {
	// MaxAttempts counts the FIRST attempt. 1 means no retries.
	//
	// The default is 1, not 4. A hidden retry multiplies cost and tail latency
	// invisibly, and in a loop that may run to max_turns it multiplies them
	// once per turn — against the same budget the SDK separately promises to
	// enforce. A caller who has not asked for retries is spending money they
	// did not authorize (OQ-9).
	MaxAttempts int
	BaseDelay   time.Duration // default 500ms
	MaxDelay    time.Duration // default 8s
	// Sleep is injectable so tests do not actually wait.
	Sleep func(ctx context.Context, d time.Duration) error
	// Rand returns a value in [0,1) for jitter. Injectable for determinism.
	Rand func() float64
}

func (o RetryOptions) withDefaults() RetryOptions {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 1
	}
	if o.BaseDelay == 0 {
		o.BaseDelay = 500 * time.Millisecond
	}
	if o.MaxDelay == 0 {
		o.MaxDelay = 8 * time.Second
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if o.Rand == nil {
		o.Rand = rand.Float64
	}
	return o
}

// RetryMiddleware is the SEMANTIC retry layer of REQ-PROV-14.
//
// It operates on a completed AssistantMessage with stop_reason "error",
// classifying its error text — because a large fraction of real provider
// failures are not HTTP statuses at all. Truncated SSE streams, DNS failures
// and gateway text bodies all arrive as a completed message carrying prose,
// and a policy that keys only on status sees none of them.
//
// The transport layer (status codes, x-should-retry, Retry-After, and the
// 60s server-delay ceiling above which a request is abandoned rather than
// slept) belongs inside the provider (REQ-PROV-13). It is NOT implemented in
// v1 and deliberately has no knobs here: exposing MaxServerDelay on this
// struct would advertise a control that nothing reads.
func RetryMiddleware(opts RetryOptions) core.Middleware {
	o := opts.withDefaults()
	return func(next core.Handler) core.Handler {
		return func(ctx context.Context, req core.Request) *core.EventStream {
			var last *core.EventStream
			for attempt := 0; attempt < o.MaxAttempts; attempt++ {
				if attempt > 0 {
					d := backoff(o, attempt)
					if err := o.Sleep(ctx, d); err != nil {
						// A cancellation landing during the backoff sleep
						// normalizes to an ABORTED message with the error
						// cleared, not an error (REQ-PROV-14).
						return core.ErrorStream(&core.AssistantMessage{
							StopReason: core.StopReasonAborted,
						}, core.ErrAborted)
					}
				}
				s := next(ctx, req)
				msg := s.Result()
				last = s
				if msg == nil || !Retryable(msg) {
					return s
				}
			}
			return last
		}
	}
}

func backoff(o RetryOptions, attempt int) time.Duration {
	d := o.BaseDelay << (attempt - 1)
	if d > o.MaxDelay {
		d = o.MaxDelay
	}
	// Jitter is applied DOWNWARD only (REQ-PROV-13): up to 25% off, never on.
	// Upward jitter would let a retry storm drift past the caller's deadline.
	f := 1 - 0.25*o.Rand()
	return time.Duration(float64(d) * f)
}

// nonRetryable is checked FIRST and wins. Retrying an out-of-credit 429 burns
// the whole backoff budget and returns the same error, slowly.
var nonRetryable = []string{
	"insufficient_quota", "quota exceeded", "billing", "out of budget",
	"available balance", "monthly limit", "usage limit", "credit balance",
	"payment required", "account is not active",
}

// retryable is checked second.
var retryable = []string{
	"overloaded", "rate limit", "too many requests", "429", "500", "502",
	"503", "504", "524", "socket hang up", "getaddrinfo", "eai_again",
	"econnreset", "connection reset", "stream ended before message_stop",
	"resourceexhausted", "you can retry your request", "timeout",
	"temporarily unavailable", "unexpected eof",
}

// Retryable classifies a completed assistant message (REQ-PROV-14).
//
// Denylist first, allowlist second, and an aborted turn never. The ordering
// matters: "rate limit" appears in the body of many quota-exhaustion errors,
// so an allowlist-first classifier retries the one failure that will never
// succeed.
func Retryable(msg *core.AssistantMessage) bool {
	if msg == nil {
		return false
	}
	if msg.StopReason == core.StopReasonAborted {
		return false // terminal, always
	}
	if msg.StopReason != core.StopReasonError {
		return false
	}
	text := strings.ToLower(msg.ErrorMessage)
	for _, s := range nonRetryable {
		if strings.Contains(text, s) {
			return false
		}
	}
	for _, s := range retryable {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- budget

// ErrBudgetGate is returned by BudgetMiddleware when a turn is refused.
var ErrBudgetGate = errors.New("agentkit: budget gate refused the turn")

// BudgetMiddleware is the PRE-TURN cost gate.
//
// It is a different mechanism from StopOverBudget, and both exist on purpose.
// The stop policy runs AFTER a turn, so a run may overshoot by one turn plus
// its tool batch — inherent to a post-turn predicate. This refuses the request
// before it is sent, which is the only way to not spend the money at all. A
// caller who cares about the ceiling wants both: this to stop the next turn,
// the policy to end the run cleanly.
func BudgetMiddleware(maxUSD float64, usage func() core.Usage) core.Middleware {
	return func(next core.Handler) core.Handler {
		return func(ctx context.Context, req core.Request) *core.EventStream {
			if usage != nil && usage().CostUSD >= maxUSD {
				return core.ErrorStream(&core.AssistantMessage{
					StopReason:   core.StopReasonError,
					ErrorMessage: fmt.Sprintf("budget of $%.4f is already spent", maxUSD),
				}, ErrBudgetGate)
			}
			return next(ctx, req)
		}
	}
}

// ---------------------------------------------------------------- caching

// CacheOptions configures CachingMiddleware.
type CacheOptions struct {
	MaxSize int // default 128 (REQ-CACHE-03)
	// IgnoreTemperature replays non-deterministic responses. Off by default:
	// REQ-CACHE-04 forbids returning a cached response when temperature > 0,
	// because replaying a sample the caller asked to be random is a
	// correctness bug dressed as an optimization.
	IgnoreTemperature bool
	// OnHit and OnMiss emit the REQ-CACHE-05 events.
	OnHit  func(fingerprint string)
	OnMiss func(fingerprint string)
	// Meter accumulates REQ-CACHE-08's session aggregate. Nil disables it;
	// the middleware works unmetered.
	Meter *CacheMeter
}

// CachingMiddleware is Level 2, in-process request deduplication.
//
// The fingerprint is a SHA-256 over the SERIALIZED REQUEST BYTES — the exact
// bytes a provider would put on the wire, including preserved tool-argument
// key order — never over a map. Go map iteration is randomized, so a
// map-derived fingerprint is nondeterministic across runs; canonicalizing the
// map to fix that produces a stable fingerprint that no longer identifies the
// bytes actually sent, which is worse than the instability it cured
// (REQ-CACHE-01).
func CachingMiddleware(opts CacheOptions) core.Middleware {
	if opts.MaxSize <= 0 {
		opts.MaxSize = 128
	}
	c := newLRU(opts.MaxSize)
	return func(next core.Handler) core.Handler {
		return func(ctx context.Context, req core.Request) *core.EventStream {
			if req.Temperature != nil && *req.Temperature > 0 && !opts.IgnoreTemperature {
				return next(ctx, req)
			}
			fp, err := Fingerprint(req)
			if err != nil {
				return next(ctx, req) // never fail a request over a cache
			}
			if msg, ok := c.get(fp); ok {
				if opts.OnHit != nil {
					opts.OnHit(fp)
				}
				opts.Meter.hit(fp)
				if n := noteFrom(ctx); n != nil {
					n.Hit, n.Tier, n.Fingerprint = true, "dedup", fp
				}
				s := core.NewEventStream(core.StreamOptions{})
				cp := *msg
				s.Push(core.MessageEndEvent{Message: cp})
				s.End(core.StreamResult{Message: &cp})
				return s
			}
			if opts.OnMiss != nil {
				opts.OnMiss(fp)
			}
			opts.Meter.miss(fp)
			if n := noteFrom(ctx); n != nil {
				n.Hit, n.Tier, n.Fingerprint = false, "dedup", fp
			}
			s := next(ctx, req)
			// Store only a successful, complete response. Caching an error
			// would replay a transient failure for the life of the session.
			if msg := s.Result(); msg != nil && !msg.StopReason.ShortCircuits() {
				c.put(fp, msg)
				// Price it NOW, so a later hit credits the exact amount this
				// response cost rather than an average over the session.
				opts.Meter.price(fp, msg.Usage.CostUSD)
			}
			return s
		}
	}
}

// Fingerprint hashes the canonical request in a form that identifies the bytes
// a provider would send.
//
// Tool-call inputs contribute their RAW Input bytes, so two requests differing
// only in the key order the model authored hash differently — which is correct,
// because they are different prompts to every provider that carries arguments
// as a JSON string.
func Fingerprint(req core.Request) (string, error) {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	if req.Temperature != nil {
		write("temp", fmt.Sprint(*req.Temperature))
	}
	if req.MaxTokens != nil {
		write("max", fmt.Sprint(*req.MaxTokens))
	}
	write("thinking", string(req.ThinkingLevel), "toolchoice", string(req.ToolChoice))

	for _, b := range req.System {
		if tb, ok := b.(core.TextBlock); ok {
			write("sys", tb.Text)
		}
	}
	for _, t := range req.Tools {
		sj, err := json.Marshal(t.InputSchema)
		if err != nil {
			return "", err
		}
		write("tool", t.Name, t.Description, string(sj))
	}
	for _, m := range req.Messages {
		write("role", string(m.Role()))
		switch v := m.(type) {
		case core.UserMessage:
			fingerprintContent(write, v.Content)
		case core.AssistantMessage:
			fingerprintContent(write, v.Content)
		case core.ToolResultMessage:
			write("tuid", v.ToolUseID, "tname", v.ToolName, "err", fmt.Sprint(v.IsError))
			fingerprintContent(write, v.Content)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fingerprintContent(write func(...string), c core.Content) {
	for _, b := range c {
		switch v := b.(type) {
		case core.TextBlock:
			write("text", v.Text)
		case core.ThinkingBlock:
			write("think", v.Thinking, v.Signature)
		case core.ToolUseBlock:
			// The model's own bytes, verbatim.
			write("use", v.ID, v.Name, string(v.Input))
		case core.ToolResultBlock:
			write("res", v.ToolUseID)
			fingerprintContent(write, v.Content)
		case core.ImageBlock:
			write("img", v.MimeType, v.Data)
		}
	}
}

// lru is a bounded LRU keyed by fingerprint.
type lru struct {
	mu    sync.Mutex
	max   int
	items map[string]*core.AssistantMessage
	order []string
}

func newLRU(max int) *lru {
	return &lru{max: max, items: map[string]*core.AssistantMessage{}}
}

func (l *lru) get(k string) (*core.AssistantMessage, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.items[k]
	if ok {
		l.touch(k)
	}
	return v, ok
}

func (l *lru) put(k string, v *core.AssistantMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.items[k]; !exists && len(l.order) >= l.max {
		oldest := l.order[0]
		l.order = l.order[1:]
		delete(l.items, oldest)
	}
	l.items[k] = v
	l.touch(k)
}

func (l *lru) touch(k string) {
	for i, x := range l.order {
		if x == k {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
	l.order = append(l.order, k)
}

func (l *lru) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.items)
}

// ---------------------------------------------------------------- tracing

// Span is the tracing contract, defined by AgentKit rather than imported.
//
// REQ-OBS-01 named OpenTelemetry and REQ-GO-11 forbids any third-party
// dependency in the root module; the two could not both hold as written. Two
// small interfaces plus a no-op default resolve it, and the OTel binding
// becomes the host's — which is where it belongs anyway, since the host
// already has a tracer configured.
type Span interface {
	SetAttributes(kv map[string]any)
	SetStatus(err error)
	AddEvent(name string, kv map[string]any)
	End()
}

// Tracer starts spans. StartSpan is callback-scoped and deliberately does NOT
// take a context.Context: cancellation belongs to the work the callback closes
// over, not to the tracing of it.
type Tracer interface {
	StartSpan(name string, fn func(Span) error) error
}

type noopSpan struct{}

func (noopSpan) SetAttributes(map[string]any)    {}
func (noopSpan) SetStatus(error)                 {}
func (noopSpan) AddEvent(string, map[string]any) {}
func (noopSpan) End()                            {}

type noopTracer struct{}

func (noopTracer) StartSpan(_ string, fn func(Span) error) error { return fn(noopSpan{}) }

// NoopTracer is the shared, fieldless default. An untraced run neither
// inspects nor retains what it is handed.
var NoopTracer Tracer = noopTracer{}

// TracingMiddleware wraps every model call in a span carrying REQ-OBS-01's
// attributes, plus REQ-CACHE-09's cache attributes when a cache decision was
// made inside the span.
//
// ORDERING: for the cache attributes to appear, tracing must WRAP caching.
// Since the last registered middleware is outermost (§5, Axis 1), that means
// registering TracingMiddleware AFTER CachingMiddleware. Registered the other
// way round the spans still carry every REQ-OBS-01 attribute and simply omit
// the cache ones — a missing attribute, never a wrong one.
func TracingMiddleware(t Tracer) core.Middleware {
	if t == nil {
		t = NoopTracer
	}
	return func(next core.Handler) core.Handler {
		return func(ctx context.Context, req core.Request) *core.EventStream {
			var out *core.EventStream
			ctx, note := withCacheNote(ctx)
			_ = t.StartSpan("agentkit.model_call", func(sp Span) error {
				defer sp.End()
				out = next(ctx, req)
				msg := out.Result()
				attrs := map[string]any{
					"tool_count":         len(req.Tools),
					"est_context_tokens": req.EstContextTokens,
				}
				if msg != nil {
					attrs["model"] = msg.Model
					attrs["provider"] = msg.Provider
					attrs["stop_reason"] = string(msg.StopReason)
					attrs["input_tokens"] = msg.Usage.InputTokens
					attrs["output_tokens"] = msg.Usage.OutputTokens
					attrs["cost_usd"] = msg.Usage.CostUSD
					if msg.StopReason == core.StopReasonError {
						sp.SetStatus(errors.New(msg.ErrorMessage))
					}
				}
				if note.Fingerprint != "" {
					attrs["cache.hit"] = note.Hit
					attrs["cache.tier"] = note.Tier
					attrs["cache.fingerprint"] = note.Fingerprint
				}
				sp.SetAttributes(attrs)
				return nil
			})
			return out
		}
	}
}

// ---------------------------------------------------------------- rate limit

// RateLimitMiddleware is a token bucket over model calls.
func RateLimitMiddleware(perSecond float64, burst int) core.Middleware {
	if burst < 1 {
		burst = 1
	}
	var (
		mu     sync.Mutex
		tokens = float64(burst)
		last   = time.Now()
	)
	return func(next core.Handler) core.Handler {
		return func(ctx context.Context, req core.Request) *core.EventStream {
			mu.Lock()
			now := time.Now()
			tokens += now.Sub(last).Seconds() * perSecond
			if tokens > float64(burst) {
				tokens = float64(burst)
			}
			last = now
			wait := time.Duration(0)
			if tokens < 1 {
				wait = time.Duration((1 - tokens) / perSecond * float64(time.Second))
				tokens = 0
			} else {
				tokens--
			}
			mu.Unlock()

			if wait > 0 {
				t := time.NewTimer(wait)
				defer t.Stop()
				select {
				case <-t.C:
				case <-ctx.Done():
					return core.ErrorStream(&core.AssistantMessage{
						StopReason: core.StopReasonAborted,
					}, core.ErrAborted)
				}
			}
			return next(ctx, req)
		}
	}
}
