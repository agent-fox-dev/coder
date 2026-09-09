package agentkit

import (
	"context"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// gatedHandler returns a handler whose stream pushes one text delta
// immediately and ends only when release is closed. It is how a test proves
// an event reached the consumer BEFORE the response completed.
func gatedHandler(release <-chan struct{}) core.Handler {
	return func(ctx context.Context, _ core.Request) *core.EventStream {
		s := core.NewEventStream(core.StreamOptions{})
		s.Push(core.TextDeltaEvent{Delta: "first"})
		go func() {
			<-release
			s.End(core.StreamResult{Message: &core.AssistantMessage{
				Content: core.Content{core.TextBlock{Text: "first"}}, StopReason: core.StopReasonStop,
				Usage: func() core.Usage { var u core.Usage; u.SetField(core.UsageInputTokens, 1); return u }(),
			}})
		}()
		return s
	}
}

// firstEventBeforeEnd fails the test unless an event can be read from the
// stream while the producer is still open.
func firstEventBeforeEnd(t *testing.T, name string, s *core.EventStream) {
	t.Helper()
	got := make(chan core.Event, 1)
	go func() {
		e, _ := s.Next()
		got <- e
	}()
	select {
	case e := <-got:
		if e == nil {
			t.Fatalf("%s: stream ended before any event", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s held every event back until the response completed; incremental streaming is off", name)
	}
}

// TestMiddlewareDoesNotHoldEventsUntilTheResponseCompletes: RetryMiddleware
// (on its final attempt), CachingMiddleware and TracingMiddleware each used
// to call Result() on the provider stream before returning it, which held
// every event until the response ended. The loop forwards the provider
// stream onto the agent stream, so installing any of them switched off
// incremental streaming for the session.
func TestMiddlewareDoesNotHoldEventsUntilTheResponseCompletes(t *testing.T) {
	cases := map[string]core.Middleware{
		"RetryMiddleware":   RetryMiddleware(noSleep()),
		"CachingMiddleware": CachingMiddleware(CacheOptions{}),
		"TracingMiddleware": TracingMiddleware(nil),
	}
	for name, mw := range cases {
		release := make(chan struct{})
		s := mw(gatedHandler(release))(context.Background(), core.Request{})
		firstEventBeforeEnd(t, name, s)
		close(release)
		if s.Result() == nil {
			t.Fatalf("%s: result lost", name)
		}
	}
}

// TestACacheEntryExistsOnceResultReturns: the store happens on the tee before
// the returned stream ends, so a caller that has seen Result() gets a hit on
// the next identical request rather than racing the store.
func TestACacheEntryExistsOnceResultReturns(t *testing.T) {
	calls := 0
	h := func(ctx context.Context, _ core.Request) *core.EventStream {
		calls++
		s := core.NewEventStream(core.StreamOptions{})
		s.End(core.StreamResult{Message: &core.AssistantMessage{
			Content: core.Content{core.TextBlock{Text: "x"}}, StopReason: core.StopReasonStop}})
		return s
	}
	mw := CachingMiddleware(CacheOptions{})(h)
	for i := 0; i < 3; i++ {
		if mw(context.Background(), core.Request{}).Result() == nil {
			t.Fatal("no result")
		}
	}
	if calls != 1 {
		t.Fatalf("handler called %d times; the second and third must be cache hits", calls)
	}
}

// TestRateLimiterDoesNotDoubleCreditTheWait: a caller that waited for a token
// spent the token that accrued during the wait. Crediting that interval again
// on the next call let two callers through at roughly twice the rate.
func TestRateLimiterDoesNotDoubleCreditTheWait(t *testing.T) {
	h := func(ctx context.Context, _ core.Request) *core.EventStream {
		s := core.NewEventStream(core.StreamOptions{})
		s.End(core.StreamResult{Message: &core.AssistantMessage{StopReason: core.StopReasonStop}})
		return s
	}
	mw := RateLimitMiddleware(10, 1)(h) // one token, then one per 100ms
	start := time.Now()
	for i := 0; i < 4; i++ {
		_ = mw(context.Background(), core.Request{}).Result()
	}
	// Burst 1 is free; the next three each wait ~100ms => >= 300ms. The
	// double-credit bug let alternate calls through immediately (~150ms).
	if el := time.Since(start); el < 280*time.Millisecond {
		t.Fatalf("4 calls at 10/s with burst 1 took %v; want >= ~300ms", el)
	}
}

// TestAnAbandonedOverlongDelayIsNotRetriedSemantically: the transport
// abandons a request whose Retry-After exceeds the ceiling (REQ-PROV-13);
// the semantic layer must not retry that on its own short backoff.
func TestAnAbandonedOverlongDelayIsNotRetriedSemantically(t *testing.T) {
	msg := &core.AssistantMessage{StopReason: core.StopReasonError,
		ErrorMessage: "provider: retry delay exceeds the ceiling: server asked for 1h0m0s, ceiling is 1m0s (status 429)"}
	if Retryable(msg) {
		t.Fatal("an abandoned over-ceiling delay must not be re-retried by the semantic layer")
	}
}
