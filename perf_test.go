package agentkit

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/schema"
)

// This file is NFR-PERF-09's acceptance mechanism.
//
// The requirement is unusually direct about why it exists: "a number with no
// benchmark behind it cannot fail, and therefore does not constrain anything",
// and "if the benchmarks are not going to be written, the honest move is to
// delete the numbers rather than ship unenforceable ones."
//
// So each benchmark here is paired with a THRESHOLD assertion in
// perf_budget_test.go. A Benchmark function alone does not fail CI — it prints
// a number nobody reads — and a budget that cannot fail is decoration.
//
// The budget assertions live in a separate file behind `//go:build !race`,
// because the race detector inflates every measurement by roughly an order of
// magnitude and a threshold that survives it is too loose to catch anything.

// benchAgent builds an agent whose provider returns instantly, so what is
// measured is the LOOP and nothing else (NFR-PERF-01 excludes model latency
// and tool execution time by definition).
func benchAgent(b *testing.B, tools int) *Agent {
	b.Helper()
	s := &scripted{}
	cfg := core.AgentConfig{
		Model:      testModel(),
		StopPolicy: StopAfterTurns(1),
		Providers:  core.ProviderRegistry{testAPI: s.provider()},
	}
	a, err := NewAgent(cfg)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < tools; i++ {
		if err := a.RegisterTool(echoTool(fmt.Sprintf("tool_%d", i), nil)); err != nil {
			b.Fatal(err)
		}
	}
	return a
}

// BenchmarkLoopTurnOverhead is NFR-PERF-01: loop overhead per turn, excluding
// model API latency and tool execution.
//
// A FRESH agent per iteration, with the clock stopped around construction.
// Reusing one agent across b.N measures something else entirely — the
// transcript grows by a turn each iteration, so the reported ns/op is the
// average over a history that ends up thousands of turns deep. That is a real
// property, and it is BenchmarkLoopTurnAtDepth's job, not this one's. Averaging
// the two together produces a number that answers neither question and fails
// the budget for the wrong reason.
func BenchmarkLoopTurnOverhead(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		a := benchAgent(b, 8)
		b.StartTimer()
		if _, err := a.Run(ctx, "go"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLoopTurnAtDepth measures one turn against an already-long
// transcript, which is where the loop's per-turn cost actually lives.
//
// NFR-PERF-01 budgets "per turn" without saying at what depth, and the
// difference is large: the REQ-GO-15 estimate and the send-time repair pass are
// both O(history), so a turn 500 messages deep costs meaningfully more than the
// first one. This benchmark is REPORTED rather than budgeted, because inventing
// a threshold the requirement does not state would be exactly the unenforceable
// rigour NFR-PERF-09 objects to.
func BenchmarkLoopTurnAtDepth(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		a := benchAgentAtDepth(b, 500)
		b.StartTimer()
		if _, err := a.Run(ctx, "go"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkContextEstimate isolates the O(history) term of REQ-GO-15, so a
// regression in the walk is attributable without re-reading a loop profile.
func BenchmarkContextEstimate(b *testing.B) {
	msgs := deepHistory(1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		EstimateContextTokens(msgs, nil)
	}
}

// BenchmarkLoopTurnWithToolBatch measures a turn that also dispatches a
// parallel tool batch, so the batch executor's own overhead is visible
// separately from the bare turn. A fresh agent per iteration, for the same
// reason as above.
func BenchmarkLoopTurnWithToolBatch(b *testing.B) {
	build := func() *Agent {
		s := &scripted{turns: []core.AssistantMessage{
			assistantWithTools(core.StopReasonToolUse,
				mustUse("c1", "tool_0", `{"v":"x"}`),
				mustUse("c2", "tool_1", `{"v":"y"}`),
				mustUse("c3", "tool_2", `{"v":"z"}`)),
		}}
		a, err := NewAgent(core.AgentConfig{
			Model: testModel(), StopPolicy: StopAfterTurns(2), ParallelTools: true,
			Providers: core.ProviderRegistry{testAPI: s.provider()},
		})
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if err := a.RegisterTool(echoTool(fmt.Sprintf("tool_%d", i), nil)); err != nil {
				b.Fatal(err)
			}
		}
		return a
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		a := build()
		b.StartTimer()
		if _, err := a.Run(ctx, "go"); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------- NFR-PERF-06

func cachedHandler() (core.Handler, core.Handler, core.Request) {
	msg := core.AssistantMessage{
		Content:    core.Content{core.TextBlock{Text: "a cached answer of some length"}},
		StopReason: core.StopReasonStop,
	}
	direct := func(context.Context, core.Request) *core.EventStream {
		s := core.NewEventStream(core.StreamOptions{})
		cp := msg
		s.Push(core.MessageEndEvent{Message: cp})
		s.End(core.StreamResult{Message: &cp})
		return s
	}
	req := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "a question"}}}}}
	cached := CachingMiddleware(CacheOptions{})(direct)
	// Warm it, so the benchmark measures HITS.
	cached(context.Background(), req).Result()
	return direct, cached, req
}

// BenchmarkDirectResponse is NFR-PERF-06's baseline.
func BenchmarkDirectResponse(b *testing.B) {
	direct, _, req := cachedHandler()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		direct(ctx, req).Result()
	}
}

// BenchmarkCacheHit is NFR-PERF-06: a Level 2 hit must add less than 0.5 ms
// over a direct response return.
//
// The fingerprint is a SHA-256 over the whole serialized request, so the
// overhead scales with transcript size rather than being constant — which is
// exactly why the budget needs measuring rather than asserting.
func BenchmarkCacheHit(b *testing.B) {
	_, cached, req := cachedHandler()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cached(ctx, req).Result()
	}
}

// ---------------------------------------------------------------- NFR-PERF-07

// stampFixture is NFR-PERF-07's stated worst case: 128 tools and 1000
// messages.
func stampFixture(b testingTB) (*core.Model, core.Request) {
	m := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 4096}
	req := core.Request{
		System: []core.ContentBlock{core.TextBlock{Text: "a system prompt"}},
	}
	for i := 0; i < 128; i++ {
		req.Tools = append(req.Tools, core.ToolWire{
			Name: fmt.Sprintf("tool_%d", i), Description: "a tool",
			InputSchema: schema.Object(schema.Opt("v", schema.String("v")))})
	}
	for i := 0; i < 500; i++ {
		req.Messages = append(req.Messages,
			core.UserMessage{Content: core.Content{core.TextBlock{Text: fmt.Sprintf("question %d", i)}}},
			core.AssistantMessage{
				Content:  core.Content{core.TextBlock{Text: fmt.Sprintf("answer %d", i)}},
				Provider: "anthropic", API: anthropic.API, Model: "claude-x",
				StopReason: core.StopReasonStop})
	}
	_ = b
	return m, req
}

type testingTB interface{ Helper() }

// BenchmarkCacheControlStamping is NFR-PERF-07's actual budget: "stamping the
// three markers must add less than 1 ms for tool sets up to 128 tools and
// transcripts up to 1000 messages".
//
// It measures the STAMP, on an already-built body, because that is what the
// number is about. The whole-request benchmarks below give the context the
// number needs — see the note there.
func BenchmarkCacheControlStamping(b *testing.B) {
	m, req := stampFixture(b)
	body, _, _, err := anthropic.BuildRequestCached(m, req, core.CacheRetentionNone, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		anthropic.StampCacheControl(body, core.CacheRetentionShort, m)
	}
}

// BenchmarkAnthropicBuildRequestUncached and its cached twin are what found
// the NFR-PERF-03 gap.
//
// The uncached build of a 128-tool, 1000-message request costs ~1.5 ms, and
// ~0.9 ms of that is re-serializing tool schemas that did not change — paid on
// every turn, for identical bytes. NFR-PERF-03 says that serialization "must
// be computed once per session and cached", and REQ-CACHE-06 was implemented
// and attached to nothing until this benchmark measured it.
func BenchmarkAnthropicBuildRequestUncached(b *testing.B) {
	m, req := stampFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := anthropic.BuildRequestCached(m, req, core.CacheRetentionShort, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAnthropicBuildRequestCached(b *testing.B) {
	m, req := stampFixture(b)
	prefix := &provider.ToolPrefix{}
	if _, _, _, err := anthropic.BuildRequestCached(m, req, core.CacheRetentionShort, prefix); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := anthropic.BuildRequestCached(m, req, core.CacheRetentionShort, prefix); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------- NFR-PERF-03

func prefixTools(n int) []core.ToolWire {
	out := make([]core.ToolWire, n)
	for i := range out {
		out[i] = core.ToolWire{
			Name: fmt.Sprintf("tool_%d", i), Description: "a tool",
			InputSchema: schema.Object(
				schema.Opt("path", schema.String("path")),
				schema.Opt("limit", schema.Int("limit")),
				schema.Opt("mode", schema.Enum("mode", "a", "b", "c")))}
	}
	return out
}

// BenchmarkToolSchemaSerializationUncached is NFR-PERF-03's baseline: what a
// model call costs when the schemas are re-serialized every time.
func BenchmarkToolSchemaSerializationUncached(b *testing.B) {
	tools := prefixTools(128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, t := range tools {
			if _, err := json.Marshal(t.InputSchema); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkToolSchemaSerializationCached is REQ-CACHE-06's steady state: the
// same reconciliation with nothing changed. NFR-PERF-03 says serialization
// must be computed ONCE PER SESSION, so the steady-state cost is the number
// that matters.
func BenchmarkToolSchemaSerializationCached(b *testing.B) {
	tools := prefixTools(128)
	var p provider.ToolPrefix
	if _, _, err := p.Sync(tools); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := p.Sync(tools); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFingerprint measures the REQ-CACHE-01 hash over a large transcript,
// since it is the dominant term in a Level 2 hit.
func BenchmarkFingerprint(b *testing.B) {
	_, req := stampFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Fingerprint(req); err != nil {
			b.Fatal(err)
		}
	}
}

func mustUse(id, name, args string) core.ToolUseBlock {
	b, err := core.NewToolUse(id, name, json.RawMessage(args))
	if err != nil {
		panic(err)
	}
	return b
}

// deepHistory builds a transcript of n user/assistant pairs.
func deepHistory(n int) core.Messages {
	var msgs core.Messages
	for i := 0; i < n; i++ {
		msgs = append(msgs,
			core.UserMessage{Content: core.Content{core.TextBlock{
				Text: fmt.Sprintf("question number %d, with enough text to be realistic", i)}}},
			core.AssistantMessage{
				Content: core.Content{core.TextBlock{
					Text: fmt.Sprintf("answer number %d, likewise of a realistic length", i)}},
				Provider: "test", API: testAPI, Model: "test-model",
				StopReason: core.StopReasonStop})
	}
	return msgs
}

func benchAgentAtDepth(b *testing.B, turns int) *Agent {
	b.Helper()
	s := &scripted{}
	h := core.NewConversationHistory()
	for _, m := range deepHistory(turns) {
		h.Record(core.NullLeaf, m)
	}
	a, err := NewAgentWithHistory(core.AgentConfig{
		Model: testModel(), StopPolicy: StopAfterTurns(1),
		Providers: core.ProviderRegistry{testAPI: s.provider()},
	}, h)
	if err != nil {
		b.Fatal(err)
	}
	return a
}
