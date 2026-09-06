package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/schema"
)

// TestASteadyStateRequestSerializesNoSchemas is NFR-PERF-03, as a COUNT rather
// than a duration.
//
// "Tool schema serialization must be computed once per session and cached, not
// recomputed on every model call." The honest form of that is zero, and a zero
// does not flake on a busy CI runner the way a threshold does.
//
// This is the assertion that was missing: REQ-CACHE-06's cache was implemented,
// unit-tested, and attached to no provider, so every request re-serialized
// every schema. That is the same shape of defect as a session log that is
// built, tested, and never written to.
func TestASteadyStateRequestSerializesNoSchemas(t *testing.T) {
	m := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 4096}
	req := core.Request{Tools: prefixTools(16)}
	prefix := &provider.ToolPrefix{}

	_, _, first, err := anthropic.BuildRequestCached(m, req, core.CacheRetentionShort, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if first.Marshalled != 16 {
		t.Fatalf("first build serialized %d schemas, want all 16", first.Marshalled)
	}

	_, _, second, err := anthropic.BuildRequestCached(m, req, core.CacheRetentionShort, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if second.Marshalled != 0 {
		t.Fatalf("a steady-state build serialized %d schemas, want 0. The cache is "+
			"present but not on the request path — which is a cache that costs memory "+
			"and saves nothing.", second.Marshalled)
	}
	if second.Invalidated {
		t.Fatalf("an unchanged tool list invalidated the prefix: %s", second.Reason)
	}
}

// TestTheProviderOwnsAPrefixByDefault: the wiring must not require the caller
// to opt in, or the default configuration is the slow one.
func TestTheProviderOwnsAPrefixByDefault(t *testing.T) {
	var reports []provider.SyncReport
	p := anthropic.Provider(anthropic.Options{
		Getenv:           func(string) string { return "k" },
		OnToolPrefixSync: func(r provider.SyncReport) { reports = append(reports, r) },
	})

	m := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 4096}
	req := core.Request{Tools: prefixTools(8), Options: core.RequestOptions{
		Transport: refusingTransport{},
	}}
	for i := 0; i < 2; i++ {
		p.Stream(context.Background(), m, req, core.ProviderStreamOptions{}).Result()
	}

	if len(reports) != 2 {
		t.Fatalf("%d sync reports, want one per request", len(reports))
	}
	if reports[0].Marshalled != 8 {
		t.Fatalf("first request serialized %d schemas, want 8", reports[0].Marshalled)
	}
	if reports[1].Marshalled != 0 {
		t.Fatalf("second request serialized %d schemas, want 0: a provider constructed "+
			"with default Options must cache without being asked", reports[1].Marshalled)
	}
}

type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errRefused
}

// ---------------------------------------------------------------- NFR-PERF-04

// TestParallelToolsUseTrueConcurrency is NFR-PERF-04.
//
// "One goroutine per tool handler invocation." The failure it rules out is a
// batch executor that looks parallel and runs sequentially, which is invisible
// in every correctness test — the results are identical — and shows up only as
// a batch that takes N times as long as it should.
func TestParallelToolsUseTrueConcurrency(t *testing.T) {
	const n = 4
	const hold = 60 * time.Millisecond

	var started sync.WaitGroup
	started.Add(n)
	gate := make(chan struct{})

	blocking := func(name string) core.Tool {
		return core.Tool{
			Name: name, Description: name,
			InputSchema: emptySchema(),
			Handler: func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
				started.Done()
				// Every handler must be running at once for this to return.
				select {
				case <-gate:
				case <-time.After(2 * time.Second):
				}
				return json.RawMessage(`{"ok":true}`), nil
			},
		}
	}

	var blocks []core.ContentBlock
	for i := 0; i < n; i++ {
		blocks = append(blocks, mustUse(idOf(i), nameOf(i), `{}`))
	}
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, blocks...)}}

	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.ParallelTools = true })
	for i := 0; i < n; i++ {
		if err := a.RegisterTool(blocking(nameOf(i))); err != nil {
			t.Fatal(err)
		}
	}

	go func() {
		// Release only once ALL handlers have entered. If the executor were
		// sequential this never happens and the test fails on the handler
		// timeout rather than on a stopwatch — no timing threshold, no flake.
		started.Wait()
		close(gate)
	}()

	done := make(chan struct{})
	go func() { defer close(done); a.Run(context.Background(), "go") }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the batch did not complete: handlers are not running concurrently, so " +
			"they never all entered at once (NFR-PERF-04)")
	}
	_ = hold
}

func idOf(i int) string   { return "c" + string(rune('0'+i)) }
func nameOf(i int) string { return "slow_" + string(rune('0'+i)) }

var errRefused = errors.New("difftest: no network in a unit test")

func emptySchema() *schema.Schema { return schema.Object() }

// ---------------------------------------------------------------- NFR-PERF-05

// TestFirstTokenIsEmittedBeforeTheStreamEnds is NFR-PERF-05: the streaming
// path must not buffer a complete model response before yielding the first
// token.
//
// It is written as a DEADLOCK rather than a stopwatch. The response body is a
// pipe that stops after the first text delta and only continues once the
// consumer has actually seen a TextDeltaEvent. A provider that reads the body
// to EOF before emitting anything can never satisfy that, so it hangs and
// fails on the timeout — no threshold, no flake, and the failure names the
// cause.
func TestFirstTokenIsEmittedBeforeTheStreamEnds(t *testing.T) {
	pr, pw := io.Pipe()
	sawFirstDelta := make(chan struct{})

	go func() {
		fmt.Fprint(pw, "event: message_start\ndata: "+
			`{"message":{"id":"m","model":"claude-x","usage":{"input_tokens":1}}}`+"\n\n")
		fmt.Fprint(pw, "event: content_block_start\ndata: "+
			`{"index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
		fmt.Fprint(pw, "event: content_block_delta\ndata: "+
			`{"index":0,"delta":{"type":"text_delta","text":"first"}}`+"\n\n")

		// Nothing more until the consumer has seen that delta.
		select {
		case <-sawFirstDelta:
		case <-time.After(3 * time.Second):
			pw.CloseWithError(errors.New("the first delta was never observed"))
			return
		}

		fmt.Fprint(pw, "event: content_block_delta\ndata: "+
			`{"index":0,"delta":{"type":"text_delta","text":" and rest"}}`+"\n\n")
		fmt.Fprint(pw, "event: content_block_stop\ndata: "+`{"index":0}`+"\n\n")
		fmt.Fprint(pw, "event: message_delta\ndata: "+
			`{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`+"\n\n")
		fmt.Fprint(pw, "event: message_stop\ndata: {}\n\n")
		pw.Close()
	}()

	p := anthropic.Provider(anthropic.Options{Getenv: func(string) string { return "k" }})
	m := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 64}
	req := core.Request{Options: core.RequestOptions{
		Transport: pipeTransport{body: pr},
	}}

	s := p.Stream(context.Background(), m, req, core.ProviderStreamOptions{})

	deadline := time.After(5 * time.Second)
	var text string
	var signalled bool
	for {
		type ev struct {
			e  core.Event
			ok bool
		}
		next := make(chan ev, 1)
		go func() { e, ok := s.Next(); next <- ev{e, ok} }()

		select {
		case got := <-next:
			if !got.ok {
				if !signalled {
					t.Fatal("the stream ended without ever emitting a text delta")
				}
				if text != "first and rest" {
					t.Fatalf("accumulated %q, want the whole text", text)
				}
				return
			}
			if d, isDelta := got.e.(core.TextDeltaEvent); isDelta {
				text += d.Delta
				if !signalled {
					signalled = true
					close(sawFirstDelta)
				}
			}
		case <-deadline:
			t.Fatal("no TextDeltaEvent arrived while the response body was still open: " +
				"the provider is buffering the complete response before yielding the " +
				"first token (NFR-PERF-05)")
		}
	}
}

type pipeTransport struct{ body io.Reader }

func (p pipeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{},
		Body: io.NopCloser(p.body)}, nil
}
