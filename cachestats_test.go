package agentkit

import (
	"context"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/schema"
)

func pricedModel() *core.Model {
	return &core.Model{ID: "m", API: testAPI, Provider: "test",
		ContextWindow: 200000, MaxTokens: 4096,
		Cost: core.Cost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}}
}

// TestSavingsAreComputedFromReportedTokensNotAnEstimate is REQ-CACHE-08's
// sharpest clause: "savings are computed against the anchored token accounting
// of REQ-GO-15, not against a re-estimate".
//
// A savings figure derived from estimated tokens is a guess wearing a dollar
// sign — and it is the number an operator uses to decide whether caching is
// worth the complexity.
func TestSavingsAreComputedFromReportedTokensNotAnEstimate(t *testing.T) {
	m := NewCacheMeter()
	var u core.Usage
	u.SetField(core.UsageInputTokens, 100)
	u.SetField(core.UsageOutputTokens, 50)
	u.SetField(core.UsageCacheReadTokens, 900_000)

	m.ObserveTurn(pricedModel(), u)
	got := m.Stats()

	if got.ProviderCacheReadTokens != 900_000 {
		t.Fatalf("cache reads = %d, want the reported 900000", got.ProviderCacheReadTokens)
	}
	// 900k tokens billed at 0.3 instead of 3.0, per million.
	want := 0.9 * (3.0 - 0.3)
	if d := got.EstimatedSavingsUSD - want; d > 1e-9 || d < -1e-9 {
		t.Fatalf("savings = %.10f, want %.10f (the difference between the input rate "+
			"and the cache-read rate on the tokens the provider said were cached)",
			got.EstimatedSavingsUSD, want)
	}
}

func TestAnUnreportedUsageContributesNothing(t *testing.T) {
	m := NewCacheMeter()
	m.ObserveTurn(pricedModel(), core.Usage{}) // nothing set
	if s := m.Stats(); s.ProviderCacheReadTokens != 0 || s.EstimatedSavingsUSD != 0 {
		t.Fatalf("stats = %+v; a provider that reported nothing must contribute nothing, "+
			"or an all-zero response reads as a perfectly uncached turn", s)
	}
}

// TestALevel2HitCreditsWhatThatResponseActuallyCost is why the meter prices a
// response when it is STORED rather than averaging over the session.
func TestALevel2HitCreditsWhatThatResponseActuallyCost(t *testing.T) {
	meter := NewCacheMeter()
	var u core.Usage
	u.SetField(core.UsageInputTokens, 1000)
	u.SetCost(0.25)

	msg := core.AssistantMessage{
		Content:    core.Content{core.TextBlock{Text: "answer"}},
		StopReason: core.StopReasonStop, Usage: u,
	}
	h := func(context.Context, core.Request) *core.EventStream {
		s := core.NewEventStream(core.StreamOptions{})
		cp := msg
		s.Push(core.MessageEndEvent{Message: cp})
		s.End(core.StreamResult{Message: &cp})
		return s
	}
	mw := CachingMiddleware(CacheOptions{Meter: meter})
	req := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "q"}}}}}

	mw(h)(context.Background(), req).Result() // miss, stores and prices
	mw(h)(context.Background(), req).Result() // hit, credits 0.25

	s := meter.Stats()
	if s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("hits=%d misses=%d, want 1 and 1", s.Hits, s.Misses)
	}
	if d := s.EstimatedSavingsUSD - 0.25; d > 1e-12 || d < -1e-12 {
		t.Fatalf("savings = %v, want the exact cost of the response the hit avoided "+
			"re-requesting (0.25), not a session average", s.EstimatedSavingsUSD)
	}
}

// TestCacheAttributesReachTheModelCallSpan is REQ-CACHE-09.
func TestCacheAttributesReachTheModelCallSpan(t *testing.T) {
	rec := &spanRecorder{}
	h := func(context.Context, core.Request) *core.EventStream {
		s := core.NewEventStream(core.StreamOptions{})
		m := core.AssistantMessage{Content: core.Content{core.TextBlock{Text: "a"}},
			StopReason: core.StopReasonStop}
		s.Push(core.MessageEndEvent{Message: m})
		s.End(core.StreamResult{Message: &m})
		return s
	}
	// Tracing must WRAP caching for the note to be established before the
	// cache decision is made. core.Chain applies middleware so the LAST
	// registered is outermost.
	chain := core.Chain(h, CachingMiddleware(CacheOptions{}), TracingMiddleware(rec))
	req := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "q"}}}}}
	chain(context.Background(), req).Result()
	chain(context.Background(), req).Result()

	if len(rec.spans) != 2 {
		t.Fatalf("%d spans, want 2", len(rec.spans))
	}
	first, second := rec.spans[0], rec.spans[1]
	if first["cache.hit"] != false || first["cache.tier"] != "dedup" {
		t.Fatalf("first span = %v, want a recorded MISS", first)
	}
	if second["cache.hit"] != true {
		t.Fatalf("second span = %v, want a recorded hit", second)
	}
	if second["cache.fingerprint"] == nil || second["cache.fingerprint"] == "" {
		t.Fatal("cache.fingerprint must be present so a hit can be traced to its key")
	}
	if first["cache.fingerprint"] != second["cache.fingerprint"] {
		t.Fatal("the same request must fingerprint identically across calls")
	}
}

// spanRecorder keeps EVERY span's attributes, where the tracer in
// middleware_test keeps only the last. Comparing the first call's span with the
// second's is the whole test: one is a miss and the other a hit.
type spanRecorder struct{ spans []map[string]any }

func (r *spanRecorder) StartSpan(_ string, fn func(Span) error) error {
	sp := &collectingSpan{attrs: map[string]any{}}
	err := fn(sp)
	r.spans = append(r.spans, sp.attrs)
	return err
}

type collectingSpan struct{ attrs map[string]any }

func (s *collectingSpan) SetAttributes(a map[string]any) {
	for k, v := range a {
		s.attrs[k] = v
	}
}
func (s *collectingSpan) SetStatus(error)                 {}
func (s *collectingSpan) AddEvent(string, map[string]any) {}
func (s *collectingSpan) End()                            {}

// ------------------------------------------------------------ REQ-CACHE-10

func wire(name string) core.ToolWire {
	return core.ToolWire{Name: name, Description: name,
		InputSchema: schema.Object(schema.Opt("x", schema.String("x")))}
}

func addedResult(names ...string) core.ToolResultMessage {
	return core.ToolResultMessage{ToolUseID: "t", ToolName: "prev",
		Content: core.Content{core.TextBlock{Text: "ok"}}, AddedToolNames: names}
}

// TestAToolAddedMidSessionIsDeferred is REQ-CACHE-10's core case.
//
// Prepending a newly discovered tool to the prompt prefix invalidates the
// provider-side cache over the ENTIRE transcript — which, on the turn an MCP
// server connects, is exactly when the transcript is longest.
func TestAToolAddedMidSessionIsDeferred(t *testing.T) {
	history := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{toolUse(t, "c1", "read_file", `{}`)}},
		addedResult("mcp__db__query"),
	}
	s := provider.SplitDeferredTools([]core.ToolWire{wire("read_file"), wire("mcp__db__query")}, history)

	if len(s.Immediate) != 1 || s.Immediate[0].Name != "read_file" {
		t.Fatalf("immediate = %+v, want just read_file", s.Immediate)
	}
	if len(s.Deferred) != 1 || s.Deferred[0].Name != "mcp__db__query" {
		t.Fatalf("deferred = %+v, want the newly added tool", s.Deferred)
	}
}

// TestLaterUsageCannotUnDeferATool pins the forward-pass rule, which is the
// part of REQ-CACHE-10 that reads like a detail and is not.
//
// A tool used on the turn AFTER it was added is the normal case — a skill
// activates and the model calls its tool immediately. Un-deferring on later
// use would promote it exactly when promoting is most expensive.
func TestLaterUsageCannotUnDeferATool(t *testing.T) {
	history := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{toolUse(t, "c1", "read_file", `{}`)}},
		addedResult("mcp__db__query"),
		// The model uses it right away, which is the point.
		core.AssistantMessage{Content: core.Content{toolUse(t, "c2", "mcp__db__query", `{}`)}},
		core.ToolResultMessage{ToolUseID: "c2", Content: core.Content{core.TextBlock{Text: "rows"}}},
	}
	s := provider.SplitDeferredTools([]core.ToolWire{wire("read_file"), wire("mcp__db__query")}, history)
	if !s.IsDeferred("mcp__db__query") {
		t.Fatal("usage AFTER the marker must not un-defer: the pass is forward and the " +
			"decision is made at the marker")
	}
}

// TestUsageBeforeTheMarkerPreventsDeferral is the other side of the rule.
func TestUsageBeforeTheMarkerPreventsDeferral(t *testing.T) {
	history := core.Messages{
		core.AssistantMessage{Content: core.Content{toolUse(t, "c1", "shared", `{}`)}},
		addedResult("shared"),
	}
	s := provider.SplitDeferredTools([]core.ToolWire{wire("shared")}, history)
	if s.IsDeferred("shared") {
		t.Fatal("a tool already used before the add marker is part of the prefix the " +
			"model has been conditioned on; deferring it would move a declaration the " +
			"transcript already references")
	}
}

// TestTheSafetyValveFiresWhenEveryToolWouldDefer is REQ-CACHE-10's last
// bullet: with nothing immediate there is no prefix to anchor against.
func TestTheSafetyValveFiresWhenEveryToolWouldDefer(t *testing.T) {
	history := core.Messages{addedResult("a", "b")}
	s := provider.SplitDeferredTools([]core.ToolWire{wire("a"), wire("b")}, history)

	if len(s.Deferred) != 0 {
		t.Fatalf("deferred = %+v, want none: all promoted back", s.Deferred)
	}
	if len(s.Immediate) != 2 {
		t.Fatalf("immediate = %+v, want both", s.Immediate)
	}
	if !s.Promoted {
		t.Fatal("the promotion must be REPORTED, or REQ-CACHE-11 cannot tell an " +
			"operator why the session lost its cache")
	}
}

// ------------------------------------------------------------ REQ-CACHE-06

// TestAddingAToolDoesNotInvalidateThePrefix is REQ-CACHE-06's asymmetry, and
// the whole reason it is worth writing down: an ADDITION is free, a REMOVAL or
// a SCHEMA CHANGE is not.
func TestAddingAToolDoesNotInvalidateThePrefix(t *testing.T) {
	var p provider.ToolPrefix
	a, b := wire("a"), wire("b")

	if _, rep, err := p.Sync([]core.ToolWire{a}); err != nil || rep.Invalidated {
		t.Fatalf("first sync: err=%v report=%+v", err, rep)
	}
	_, rep, err := p.Sync([]core.ToolWire{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Invalidated {
		t.Fatalf("adding a tool invalidated the prefix: %+v. REQ-CACHE-06 is explicit "+
			"that it must not, because the cheap answer to mid-session discovery is "+
			"REQ-CACHE-10's deferral, not a cache wipe.", rep)
	}
	if len(rep.Added) != 1 || rep.Added[0] != "b" {
		t.Fatalf("added = %v, want [b]", rep.Added)
	}
	if rep.Marshalled != 1 {
		t.Fatalf("marshalled %d schemas, want 1: the unchanged tool must come from the "+
			"cache (NFR-PERF-03)", rep.Marshalled)
	}
}

func TestRemovingAToolInvalidatesThePrefix(t *testing.T) {
	var p provider.ToolPrefix
	a, b := wire("a"), wire("b")
	_, _, _ = p.Sync([]core.ToolWire{a, b})

	_, rep, err := p.Sync([]core.ToolWire{a})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Invalidated {
		t.Fatal("removing a tool rewrites the serialized prefix and must invalidate")
	}
	if rep.Reason == "" {
		t.Fatal("REQ-CACHE-11 wants a reason an operator can act on, not a count")
	}
}

func TestChangingAToolsSchemaInvalidatesThePrefix(t *testing.T) {
	var p provider.ToolPrefix
	a := wire("a")
	_, _, _ = p.Sync([]core.ToolWire{a})

	changed := core.ToolWire{Name: "a", Description: "a",
		InputSchema: schema.Object(schema.Opt("y", schema.Int("y")))}
	_, rep, err := p.Sync([]core.ToolWire{changed})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Invalidated {
		t.Fatal("a changed schema changes the bytes the model was shown and must invalidate")
	}
}

// TestRebuildingAnIdenticalToolDoesNotInvalidate is the case the pointer fast
// path alone gets wrong.
//
// Re-registering the same tools — which a resumed session does — produces new
// schema values. If a differing pointer were treated as a change, every resume
// would report a prefix invalidation that never happened.
func TestRebuildingAnIdenticalToolDoesNotInvalidate(t *testing.T) {
	var p provider.ToolPrefix
	_, _, _ = p.Sync([]core.ToolWire{wire("a")})

	_, rep, err := p.Sync([]core.ToolWire{wire("a")}) // a fresh, equal schema value
	if err != nil {
		t.Fatal(err)
	}
	if rep.Invalidated {
		t.Fatalf("rebuilding an identical tool invalidated the prefix: %+v. Only the "+
			"CONTENT may decide; a pointer comparison alone reports a cache wipe on "+
			"every resume.", rep)
	}
}

func TestMeterRecordsInvalidationsAndPromotions(t *testing.T) {
	m := NewCacheMeter()
	m.ObserveSync(provider.SyncReport{Invalidated: true, Reason: "tool removed: x"})
	m.ObserveSync(provider.SyncReport{}) // clean sync contributes nothing
	m.ObserveDeferredSplit(provider.DeferredSplit{Promoted: true})

	s := m.Stats()
	if s.PrefixInvalidations != 1 {
		t.Fatalf("invalidations = %d, want 1", s.PrefixInvalidations)
	}
	if s.DeferredToolPromotions != 1 {
		t.Fatalf("promotions = %d, want 1", s.DeferredToolPromotions)
	}
	if s.LastInvalidation != "tool removed: x" {
		t.Fatalf("last invalidation = %q, want the reason", s.LastInvalidation)
	}
}
