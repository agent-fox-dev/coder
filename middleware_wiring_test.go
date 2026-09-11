package agentkit

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/middleware"
)

// recordingTracer keeps the last span's attributes and status.
type recordingTracer struct {
	spans  atomic.Int32
	attrs  map[string]any
	status error
}

func (r *recordingTracer) StartSpan(_ string, fn func(core.Span) error) error {
	r.spans.Add(1)
	return fn(&recordingSpan{t: r})
}

type recordingSpan struct{ t *recordingTracer }

func (s *recordingSpan) SetAttributes(kv map[string]any) { s.t.attrs = kv }
func (s *recordingSpan) SetStatus(err error)             { s.t.status = err }
func (s *recordingSpan) AddEvent(string, map[string]any) {}
func (s *recordingSpan) End()                            {}

func noSleep() middleware.RetryOptions {
	return middleware.RetryOptions{
		Sleep: func(context.Context, time.Duration) error { return nil },
		Rand:  func() float64 { return 0 },
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
			middleware.Tracing(tr),
			middleware.Caching(middleware.CacheOptions{
				OnHit:  func(string) { hits.Add(1) },
				OnMiss: func(string) { misses.Add(1) },
			}),
			middleware.Retry(noSleep()),
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
