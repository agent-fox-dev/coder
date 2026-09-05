package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// handlerReturning builds a Handler that replays a fixed sequence of assistant
// messages, one per call.
func handlerReturning(msgs ...core.AssistantMessage) (core.Handler, *atomic.Int32) {
	var calls atomic.Int32
	h := func(ctx context.Context, req core.Request) *core.EventStream {
		i := int(calls.Add(1)) - 1
		m := core.AssistantMessage{Content: core.Content{core.TextBlock{Text: "ok"}},
			StopReason: core.StopReasonStop}
		if i < len(msgs) {
			m = msgs[i]
		}
		s := core.NewEventStream(core.StreamOptions{})
		s.Push(core.MessageEndEvent{Message: m})
		s.End(core.StreamResult{Message: &m})
		return s
	}
	return h, &calls
}

func errMsg(text string) core.AssistantMessage {
	return core.AssistantMessage{StopReason: core.StopReasonError, ErrorMessage: text}
}

func noSleep() RetryOptions {
	return RetryOptions{
		Sleep: func(context.Context, time.Duration) error { return nil },
		Rand:  func() float64 { return 0 },
	}
}

// ------------------------------------------------------------------ retry

// TestRetryDenylistIsCheckedFirst is the ordering that matters.
//
// "rate limit" appears in the body of many quota-exhaustion errors, so an
// allowlist-first classifier retries the one failure that will never succeed —
// burning the whole backoff budget to return the same error, slowly.
func TestRetryDenylistIsCheckedFirst(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"plain overload", "Overloaded, please try again", true},
		{"bare 529 text", "upstream returned 503", true},
		{"truncated stream", "stream ended before message_stop", true},
		{"dns failure", "getaddrinfo ENOTFOUND api.example.com", true},
		{"quota, worded as a rate limit", "You have hit your rate limit: insufficient_quota", false},
		{"billing", "Your credit balance is too low", false},
		{"monthly limit mentioning 429", "429: monthly limit reached", false},
		{"unrecognized", "something entirely new", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Retryable(&core.AssistantMessage{
				StopReason: core.StopReasonError, ErrorMessage: c.text}); got != c.want {
				t.Fatalf("Retryable(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

func TestAbortedIsNeverRetried(t *testing.T) {
	if Retryable(&core.AssistantMessage{StopReason: core.StopReasonAborted,
		ErrorMessage: "overloaded"}) {
		t.Fatal("an aborted turn is terminal and must never be retried, even when its " +
			"text looks retryable (REQ-PROV-14)")
	}
}

func TestRetryDefaultsToNoRetries(t *testing.T) {
	// A hidden retry multiplies cost and tail latency invisibly, once per turn,
	// against the same budget the SDK promises to enforce (OQ-9).
	h, calls := handlerReturning(errMsg("overloaded"), errMsg("overloaded"))
	mw := RetryMiddleware(noSleep())
	_ = mw(h)(context.Background(), core.Request{}).Result()
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times with default options, want 1: retries are "+
			"opt-in, not a hidden default", got)
	}
}

func TestRetryStopsOnFirstSuccess(t *testing.T) {
	ok := core.AssistantMessage{Content: core.Content{core.TextBlock{Text: "fine"}},
		StopReason: core.StopReasonStop}
	h, calls := handlerReturning(errMsg("overloaded"), errMsg("rate limit"), ok)
	o := noSleep()
	o.MaxAttempts = 5
	msg := RetryMiddleware(o)(h)(context.Background(), core.Request{}).Result()

	if calls.Load() != 3 {
		t.Fatalf("handler called %d times, want 3 (two failures then success)", calls.Load())
	}
	if msg.StopReason != core.StopReasonStop {
		t.Fatalf("stop reason = %q, want the successful response", msg.StopReason)
	}
}

func TestRetryGivesUpOnANonRetryableError(t *testing.T) {
	h, calls := handlerReturning(errMsg("insufficient_quota"), errMsg("insufficient_quota"))
	o := noSleep()
	o.MaxAttempts = 5
	_ = RetryMiddleware(o)(h)(context.Background(), core.Request{}).Result()
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times on a quota error, want 1", got)
	}
}

func TestBackoffJitterIsDownwardOnly(t *testing.T) {
	o := RetryOptions{BaseDelay: time.Second, MaxDelay: time.Minute}.withDefaults()
	for _, r := range []float64{0, 0.5, 0.999} {
		o.Rand = func() float64 { return r }
		d := backoff(o, 1)
		if d > time.Second {
			t.Fatalf("backoff with rand=%v gave %v, which is LONGER than the base delay: "+
				"jitter is applied downward only (REQ-PROV-13)", r, d)
		}
		if d < 750*time.Millisecond {
			t.Fatalf("backoff with rand=%v gave %v, below the 25%% floor", r, d)
		}
	}
}

func TestCancellationDuringBackoffNormalizesToAborted(t *testing.T) {
	h, _ := handlerReturning(errMsg("overloaded"), errMsg("overloaded"))
	o := RetryOptions{MaxAttempts: 3, Rand: func() float64 { return 0 }}
	o.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	msg := RetryMiddleware(o)(h)(context.Background(), core.Request{}).Result()
	if msg.StopReason != core.StopReasonAborted {
		t.Fatalf("stop reason = %q, want aborted: a cancellation landing during the "+
			"backoff sleep normalizes to an aborted message (REQ-PROV-14)", msg.StopReason)
	}
	if msg.ErrorMessage != "" {
		t.Fatalf("error message = %q, want it cleared on an abort", msg.ErrorMessage)
	}
}

// ------------------------------------------------------------------ budget

func TestBudgetGateRefusesBeforeSending(t *testing.T) {
	h, calls := handlerReturning()
	var u core.Usage
	u.SetCost(2.0)
	mw := BudgetMiddleware(1.0, func() core.Usage { return u })

	s := mw(h)(context.Background(), core.Request{})
	if calls.Load() != 0 {
		t.Fatal("the gate must refuse BEFORE the request is sent; that is the whole " +
			"difference from StopOverBudget, which can only stop the NEXT turn")
	}
	if !errors.Is(s.Err(), ErrBudgetGate) {
		t.Fatalf("err = %v, want ErrBudgetGate", s.Err())
	}
}

func TestBudgetGatePassesWhenUnderBudget(t *testing.T) {
	h, calls := handlerReturning()
	var u core.Usage
	u.SetCost(0.1)
	_ = BudgetMiddleware(1.0, func() core.Usage { return u })(h)(context.Background(), core.Request{}).Result()
	if calls.Load() != 1 {
		t.Fatal("an under-budget request must pass through")
	}
}

// ------------------------------------------------------------------ caching

func TestCacheHitSkipsTheProvider(t *testing.T) {
	h, calls := handlerReturning()
	var hits, misses atomic.Int32
	mw := CachingMiddleware(CacheOptions{
		OnHit:  func(string) { hits.Add(1) },
		OnMiss: func(string) { misses.Add(1) },
	})
	req := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "same question"}}}}}

	first := mw(h)(context.Background(), req).Result()
	second := mw(h)(context.Background(), req).Result()

	if calls.Load() != 1 {
		t.Fatalf("provider called %d times for two identical requests, want 1", calls.Load())
	}
	if hits.Load() != 1 || misses.Load() != 1 {
		t.Fatalf("hits=%d misses=%d, want 1 and 1", hits.Load(), misses.Load())
	}
	if first.Content.Text() != second.Content.Text() {
		t.Fatal("the cached response differs from the original")
	}
}

// TestFingerprintIsStableAcrossRuns is the reason the fingerprint is taken
// over bytes rather than a map.
//
// Go map iteration is randomized, so a map-derived fingerprint differs between
// runs of the same binary. Canonicalizing the map to fix that produces a
// stable fingerprint that no longer identifies the bytes actually sent — worse
// than the instability it cured.
func TestFingerprintIsStableAcrossRuns(t *testing.T) {
	b, err := core.NewToolUse("c1", "t",
		json.RawMessage(`{"zeta":1,"alpha":{"yankee":true,"bravo":[{"zulu":"z","charlie":0}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	req := core.Request{Messages: core.Messages{
		core.AssistantMessage{Content: core.Content{b}, StopReason: core.StopReasonToolUse}}}

	first, err := Fingerprint(req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		got, err := Fingerprint(req)
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("fingerprint changed between calls: %s vs %s", first, got)
		}
	}
}

// TestFingerprintDistinguishesToolArgumentKeyOrder: two requests differing
// only in the key order the model authored ARE different prompts to every
// provider that carries arguments as a JSON string.
func TestFingerprintDistinguishesToolArgumentKeyOrder(t *testing.T) {
	mk := func(args string) core.Request {
		b, err := core.NewToolUse("c1", "t", json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return core.Request{Messages: core.Messages{
			core.AssistantMessage{Content: core.Content{b}, StopReason: core.StopReasonToolUse}}}
	}
	a, _ := Fingerprint(mk(`{"alpha":1,"zeta":2}`))
	b, _ := Fingerprint(mk(`{"zeta":2,"alpha":1}`))
	if a == b {
		t.Fatal("two tool calls differing only in authored key order hashed the same; " +
			"they are different bytes on the wire and different prompt-cache prefixes")
	}
}

func TestCacheDoesNotReplayNonDeterministicResponses(t *testing.T) {
	h, calls := handlerReturning()
	temp := 0.7
	req := core.Request{
		Temperature: &temp,
		Messages:    core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "x"}}}},
	}
	mw := CachingMiddleware(CacheOptions{})
	_ = mw(h)(context.Background(), req).Result()
	_ = mw(h)(context.Background(), req).Result()
	if calls.Load() != 2 {
		t.Fatal("a response sampled at temperature > 0 must not be replayed: the caller " +
			"asked for randomness (REQ-CACHE-04)")
	}
}

func TestCacheDoesNotStoreErrors(t *testing.T) {
	h, calls := handlerReturning(errMsg("overloaded"))
	mw := CachingMiddleware(CacheOptions{})
	req := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "x"}}}}}
	_ = mw(h)(context.Background(), req).Result()
	_ = mw(h)(context.Background(), req).Result()
	if calls.Load() != 2 {
		t.Fatal("caching an error would replay a transient failure for the life of the session")
	}
}

func TestCacheEvictsAtMaxSize(t *testing.T) {
	c := newLRU(2)
	m := &core.AssistantMessage{}
	c.put("a", m)
	c.put("b", m)
	c.put("c", m)
	if c.len() != 2 {
		t.Fatalf("cache holds %d entries, want 2", c.len())
	}
	if _, ok := c.get("a"); ok {
		t.Fatal("the least recently used entry should have been evicted")
	}
}

// ------------------------------------------------------------------ tracing

type recordingTracer struct {
	spans  atomic.Int32
	attrs  map[string]any
	status error
}

func (r *recordingTracer) StartSpan(_ string, fn func(Span) error) error {
	r.spans.Add(1)
	return fn(&recordingSpan{t: r})
}

type recordingSpan struct{ t *recordingTracer }

func (s *recordingSpan) SetAttributes(kv map[string]any) { s.t.attrs = kv }
func (s *recordingSpan) SetStatus(err error)             { s.t.status = err }
func (s *recordingSpan) AddEvent(string, map[string]any) {}
func (s *recordingSpan) End()                            {}

func TestTracingCarriesTheRequiredAttributes(t *testing.T) {
	msg := core.AssistantMessage{
		Content: core.Content{core.TextBlock{Text: "hi"}}, StopReason: core.StopReasonStop,
		Provider: "acme", Model: "m1",
	}
	msg.Usage.SetField(core.UsageInputTokens, 100)
	msg.Usage.SetField(core.UsageOutputTokens, 20)
	msg.Usage.SetCost(0.005)

	h, _ := handlerReturning(msg)
	tr := &recordingTracer{}
	_ = TracingMiddleware(tr)(h)(context.Background(), core.Request{}).Result()

	if tr.spans.Load() != 1 {
		t.Fatalf("spans = %d, want 1", tr.spans.Load())
	}
	for _, k := range []string{"model", "provider", "stop_reason", "input_tokens",
		"output_tokens", "cost_usd"} {
		if _, ok := tr.attrs[k]; !ok {
			t.Errorf("span is missing required attribute %q (REQ-OBS-01)", k)
		}
	}
}

func TestNoopTracerRetainsNothing(t *testing.T) {
	h, _ := handlerReturning()
	// The point is that this neither panics nor requires a consumer: an
	// untraced run must not inspect or retain what it is handed.
	msg := TracingMiddleware(nil)(h)(context.Background(), core.Request{}).Result()
	if msg == nil {
		t.Fatal("the no-op tracer must not swallow the response")
	}
}

func TestTracingReportsAnErrorStatus(t *testing.T) {
	h, _ := handlerReturning(errMsg("boom"))
	tr := &recordingTracer{}
	_ = TracingMiddleware(tr)(h)(context.Background(), core.Request{}).Result()
	if tr.status == nil || !strings.Contains(tr.status.Error(), "boom") {
		t.Fatalf("span status = %v, want the provider error", tr.status)
	}
}

// ------------------------------------------------------------------ ordering

// TestLastRegisteredIsOutermost pins §5's Axis 1 ordering. Getting it backwards
// puts the retry inside the budget gate, so a retried turn is charged once.
func TestLastRegisteredIsOutermost(t *testing.T) {
	var order []string
	mk := func(name string) core.Middleware {
		return func(next core.Handler) core.Handler {
			return func(ctx context.Context, req core.Request) *core.EventStream {
				order = append(order, name)
				return next(ctx, req)
			}
		}
	}
	h, _ := handlerReturning()
	chained := core.Chain(h, mk("first"), mk("second"), mk("third"))
	_ = chained(context.Background(), core.Request{}).Result()

	got := strings.Join(order, ",")
	if got != "third,second,first" {
		t.Fatalf("execution order = %q, want %q: the LAST registered middleware is "+
			"outermost and executes first (§5, Axis 1)", got, "third,second,first")
	}
}

// TestMiddlewareComposesEndToEndThroughTheAgent runs the real loop with a
// chain installed, so the wiring is exercised rather than only the pieces.
func TestMiddlewareComposesEndToEndThroughTheAgent(t *testing.T) {
	var hits, misses atomic.Int32
	tr := &recordingTracer{}

	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "answer"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Middleware = []core.Middleware{
			TracingMiddleware(tr),
			CachingMiddleware(CacheOptions{
				OnHit:  func(string) { hits.Add(1) },
				OnMiss: func(string) { misses.Add(1) },
			}),
			RetryMiddleware(noSleep()),
		}
	})
	if _, err := a.Run(context.Background(), "question"); err != nil {
		t.Fatal(err)
	}
	if tr.spans.Load() != 1 {
		t.Fatalf("spans = %d, want 1 span for the one model call", tr.spans.Load())
	}
	if misses.Load() != 1 {
		t.Fatalf("cache misses = %d, want 1", misses.Load())
	}
}

// TestRateLimitDelaysTheSecondCall.
func TestRateLimitDelaysTheSecondCall(t *testing.T) {
	h, _ := handlerReturning()
	mw := RateLimitMiddleware(100, 1) // 100/s, burst 1
	chained := mw(h)

	start := time.Now()
	_ = chained(context.Background(), core.Request{}).Result()
	_ = chained(context.Background(), core.Request{}).Result()
	elapsed := time.Since(start)

	if elapsed < 5*time.Millisecond {
		t.Fatalf("two calls at 100/s with burst 1 took %v; the second should have "+
			"waited for a token", elapsed)
	}
}
