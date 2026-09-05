package catalog

import (
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

func thinkingModel(entries map[core.ThinkingLevel]*string) *core.Model {
	return &core.Model{ID: "t", Reasoning: true, ThinkingLevelMap: entries}
}

func TestClampMaxTokensAgainstTheContextWindow(t *testing.T) {
	// window 200000, model output cap 64000, default margin 4096.
	m := &core.Model{ID: "m", ContextWindow: 200000, MaxTokens: 64000}

	tests := []struct {
		name      string
		model     *core.Model
		requested int
		est       int
		want      int
	}{
		{"empty context, request under the cap", m, 8000, 0, 8000},
		{"empty context, request over the cap is capped", m, 999999, 0, 64000},
		{"no request falls back to the model cap", m, 0, 0, 64000},
		{"window bites: 200000-190000-4096", m, 64000, 190000, 5904},
		{"window bites below the request", m, 4000, 190000, 4000},
		{"exhausted window yields the floor of 1, never 0 or negative", m, 64000, 199000, 1},
		{"over-full window still yields 1", m, 64000, 500000, 1},
		{"negative estimate is treated as zero", m, 64000, -5, 64000},
		{"nil model passes the request through", nil, 8000, 0, 8000},
		{"no cap and no window: nothing to clamp against",
			&core.Model{ID: "bare"}, 8000, 0, 8000},
		{"no cap, no window, no request: nothing to send",
			&core.Model{ID: "bare"}, 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampMaxTokens(tc.model, tc.requested, tc.est); got != tc.want {
				t.Errorf("ClampMaxTokens(requested=%d, est=%d) = %d, want %d",
					tc.requested, tc.est, got, tc.want)
			}
		})
	}
}

// TestUnknownContextWindowYieldsTheModelCapNotAFloorOfOne pins ruling P-29.
// REQ-CAT-04's literal words — "models with an unknown context window receive
// the floor only" — resolve to max(1, available) with an unknown window, i.e.
// exactly 1 token of output for every model the catalog has no window for,
// including every sibling clone whose template lacked one. This test fails
// loudly (want 8000, got 1) against that reading.
func TestUnknownContextWindowYieldsTheModelCapNotAFloorOfOne(t *testing.T) {
	m := &core.Model{ID: "unknown-window", ContextWindow: 0, MaxTokens: 64000}

	if got := ClampMaxTokens(m, 8000, 900000); got != 8000 {
		t.Errorf("ClampMaxTokens = %d, want 8000: with no window there is no window\n"+
			"arithmetic to do, however large the context estimate (P-29)", got)
	}
	if got := ClampMaxTokens(m, 999999, 0); got != 64000 {
		t.Errorf("ClampMaxTokens = %d, want the model's own cap 64000", got)
	}
}

func TestClampMaxTokensWithMarginHonoursTheMargin(t *testing.T) {
	m := &core.Model{ID: "m", ContextWindow: 100000, MaxTokens: 64000}
	if got := ClampMaxTokensWithMargin(m, 64000, 50000, 0); got != 50000 {
		t.Errorf("margin 0: got %d, want 50000", got)
	}
	if got := ClampMaxTokensWithMargin(m, 64000, 50000, 10000); got != 40000 {
		t.Errorf("margin 10000: got %d, want 40000", got)
	}
	if got := ClampMaxTokensWithMargin(m, 64000, 50000, -1); got != 50000 {
		t.Errorf("negative margin: got %d, want it treated as 0", got)
	}
	if DefaultSafetyMargin != 4096 {
		t.Errorf("DefaultSafetyMargin = %d, want REQ-CAT-04's 4096", DefaultSafetyMargin)
	}
}

// TestClampRequestMaxTokensPreservesAbsence: REQ-PROV-16 presence. An absent
// max_tokens must stay absent — inventing the model's cap silently caps output
// the caller never asked to cap, on every API where the field is optional.
// The estimate comes from Request.EstContextTokens, supplied by the loop
// (ruling P-30); the clamp never re-walks the transcript (REQ-GO-15).
func TestClampRequestMaxTokensPreservesAbsence(t *testing.T) {
	m := &core.Model{ID: "m", ContextWindow: 200000, MaxTokens: 64000}

	if got := ClampRequestMaxTokens(m, core.Request{EstContextTokens: 190000}); got != nil {
		t.Errorf("absent max_tokens became %d", *got)
	}

	req := 64000
	got := ClampRequestMaxTokens(m, core.Request{MaxTokens: &req, EstContextTokens: 190000})
	if got == nil {
		t.Fatal("present max_tokens became absent")
	}
	if *got != 5904 {
		t.Errorf("clamped to %d, want 5904 (200000-190000-4096)", *got)
	}
	if req != 64000 {
		t.Errorf("the caller's own int was mutated to %d", req)
	}
}

// TestClampThinkingLevelSearchesUpwardBeforeDownward is REQ-PROV-15's central
// rule: "a user asking for maximum thinking gets the most the model offers,
// never silently less."
func TestClampThinkingLevelSearchesUpwardBeforeDownward(t *testing.T) {
	max := "max"
	low := "low"
	high := "high"

	t.Run("the requirement's own fixture", func(t *testing.T) {
		// {xhigh: null, max: "max"}, requesting xhigh, clamps UP to max.
		m := thinkingModel(map[core.ThinkingLevel]*string{core.ThinkingXHigh: nil, core.ThinkingMax: &max})
		lvl, wire, ok := ClampThinkingLevel(m, core.ThinkingXHigh)
		if !ok || lvl != core.ThinkingMax || wire != "max" {
			t.Fatalf("got (%q, %q, %v), want (max, max, true)", lvl, wire, ok)
		}
	})

	t.Run("with a lower level also available", func(t *testing.T) {
		// The fixture above cannot tell upward-first from downward-first: with
		// nothing below xhigh, both orders end at max. Adding `low` makes the
		// orders disagree — downward-first answers "low" here, which is less
		// thinking than was asked for and looks, from outside, like success.
		m := thinkingModel(map[core.ThinkingLevel]*string{
			core.ThinkingLow: &low, core.ThinkingXHigh: nil, core.ThinkingMax: &max,
		})
		lvl, wire, ok := ClampThinkingLevel(m, core.ThinkingXHigh)
		if !ok || lvl != core.ThinkingMax || wire != "max" {
			t.Fatalf("got (%q, %q, %v), want (max, max, true): the search must go UP first", lvl, wire, ok)
		}
	})

	t.Run("downward only when nothing above exists", func(t *testing.T) {
		// REQ-PROV-15's other worked example: "requesting max on a plain
		// reasoning model clamps down to high".
		m := thinkingModel(map[core.ThinkingLevel]*string{
			core.ThinkingLow: &low, core.ThinkingHigh: &high,
		})
		lvl, wire, ok := ClampThinkingLevel(m, core.ThinkingMax)
		if !ok || lvl != core.ThinkingHigh || wire != "high" {
			t.Fatalf("got (%q, %q, %v), want (high, high, true)", lvl, wire, ok)
		}
	})

	t.Run("an exactly supported level is returned unchanged", func(t *testing.T) {
		m := thinkingModel(map[core.ThinkingLevel]*string{
			core.ThinkingLow: &low, core.ThinkingHigh: &high, core.ThinkingMax: &max,
		})
		lvl, wire, ok := ClampThinkingLevel(m, core.ThinkingHigh)
		if !ok || lvl != core.ThinkingHigh || wire != "high" {
			t.Fatalf("got (%q, %q, %v), want (high, high, true)", lvl, wire, ok)
		}
	})
}

// TestClampThinkingLevelNeverClampsDownToOff pins ruling P-27. Turning a
// request for SOME thinking into NO thinking changes what the model does, not
// how hard it tries; it is a behaviour change wearing a clamp's clothes.
func TestClampThinkingLevelNeverClampsDownToOff(t *testing.T) {
	disabled := "disabled"
	low := "low"

	t.Run("off is skipped when a real level is below the request", func(t *testing.T) {
		m := thinkingModel(map[core.ThinkingLevel]*string{
			core.ThinkingOff: &disabled, core.ThinkingLow: &low,
		})
		lvl, _, ok := ClampThinkingLevel(m, core.ThinkingMedium)
		if !ok || lvl != core.ThinkingLow {
			t.Fatalf("got (%q, %v), want low", lvl, ok)
		}
	})

	t.Run("off is not the answer when it is the only thing below", func(t *testing.T) {
		// Only `off` is supported. The downward search must find nothing and
		// report unsupported, so the caller omits the parameter — it must NOT
		// answer "off". An implementation that walks core.ThinkingLevelOrder
		// down to index 0 fails here.
		m := thinkingModel(map[core.ThinkingLevel]*string{core.ThinkingOff: &disabled})
		lvl, wire, ok := ClampThinkingLevel(m, core.ThinkingMedium)
		if ok {
			t.Fatalf("clamped a request for medium down to (%q, %q); off is excluded (P-27)", lvl, wire)
		}
		if lvl != core.ThinkingUnset {
			t.Errorf("level = %q, want unset", lvl)
		}
	})

	t.Run("off is honoured when it is what was asked for", func(t *testing.T) {
		m := thinkingModel(map[core.ThinkingLevel]*string{
			core.ThinkingOff: &disabled, core.ThinkingLow: &low,
		})
		lvl, wire, ok := ClampThinkingLevel(m, core.ThinkingOff)
		if !ok || lvl != core.ThinkingOff || wire != "disabled" {
			t.Fatalf("got (%q, %q, %v), want (off, disabled, true)", lvl, wire, ok)
		}
	})

	t.Run("a model that cannot be silenced gets the least it offers", func(t *testing.T) {
		// off is present-null: explicitly unsupported. Reporting "omit the
		// key" would send the model's own default, which is MORE thinking than
		// the caller asked for; the smallest supported level is the honest
		// answer to "off, please".
		m := thinkingModel(map[core.ThinkingLevel]*string{core.ThinkingOff: nil, core.ThinkingLow: &low})
		lvl, _, ok := ClampThinkingLevel(m, core.ThinkingOff)
		if !ok || lvl != core.ThinkingLow {
			t.Fatalf("got (%q, %v), want low", lvl, ok)
		}
	})
}

// TestPresentNullAndAbsentAreRuntimeIdentical pins ruling P-28. REQ-PROV-15
// calls the two "distinct", and they are — to the catalog author and to the
// REQ-CAT-06 regeneration diff. At runtime they are the same, and this test
// exists so nobody goes hunting for a difference that is not there.
func TestPresentNullAndAbsentAreRuntimeIdentical(t *testing.T) {
	high := "high"

	presentNull := thinkingModel(map[core.ThinkingLevel]*string{
		core.ThinkingHigh: &high, core.ThinkingMax: nil,
	})
	absent := thinkingModel(map[core.ThinkingLevel]*string{core.ThinkingHigh: &high})

	for _, req := range core.ThinkingLevelOrder {
		l1, w1, ok1 := ClampThinkingLevel(presentNull, req)
		l2, w2, ok2 := ClampThinkingLevel(absent, req)
		if l1 != l2 || w1 != w2 || ok1 != ok2 {
			t.Errorf("request %q: present-null gave (%q,%q,%v), absent gave (%q,%q,%v)",
				req, l1, w1, ok1, l2, w2, ok2)
		}
	}
	if SupportsThinkingLevel(presentNull, core.ThinkingMax) {
		t.Error("present-null must report unsupported")
	}
	// ...and the authoring distinction is still readable on the descriptor.
	if _, present := presentNull.ThinkingLevelMap[core.ThinkingMax]; !present {
		t.Error("the present-null entry was lost; the REQ-CAT-06 diff needs it")
	}
}

func TestClampThinkingLevelReportsUnsupported(t *testing.T) {
	high := "high"
	m := thinkingModel(map[core.ThinkingLevel]*string{core.ThinkingHigh: &high})

	tests := []struct {
		name  string
		model *core.Model
		req   core.ThinkingLevel
	}{
		{"unset stays unset: no opinion is not a request for max", m, core.ThinkingUnset},
		{"a level string this build does not know", m, core.ThinkingLevel("ludicrous")},
		{"a model with no thinking map at all", &core.Model{ID: "plain"}, core.ThinkingHigh},
		{"a nil model", nil, core.ThinkingHigh},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lvl, wire, ok := ClampThinkingLevel(tc.model, tc.req)
			if ok {
				t.Fatalf("got (%q, %q, true), want unsupported so the caller omits the parameter", lvl, wire)
			}
			if lvl != core.ThinkingUnset || wire != "" {
				t.Errorf("got (%q, %q), want (unset, \"\")", lvl, wire)
			}
		})
	}
}

func TestThinkingWireReturnsTheRowsOwnValue(t *testing.T) {
	empty := ""
	budget := "16384"
	m := thinkingModel(map[core.ThinkingLevel]*string{
		core.ThinkingMedium: &budget, core.ThinkingHigh: &empty,
	})
	if w, ok := ThinkingWire(m, core.ThinkingMedium); !ok || w != "16384" {
		t.Errorf("got (%q, %v), want (16384, true): the wire value is opaque to this package", w, ok)
	}
	// Presence, never emptiness, is the support signal (REQ-PROV-16.2).
	if w, ok := ThinkingWire(m, core.ThinkingHigh); !ok || w != "" {
		t.Errorf("got (%q, %v), want (\"\", true)", w, ok)
	}
	if _, ok := ThinkingWire(m, core.ThinkingLow); ok {
		t.Error("an absent level reported supported")
	}
	if _, ok := ThinkingWire(nil, core.ThinkingLow); ok {
		t.Error("a nil model reported supported")
	}
}

// The shipped catalog, exercised through the real clamps.
func TestShippedCatalogClamps(t *testing.T) {
	o3, err := ResolveModel("o3")
	if err != nil {
		t.Fatal(err)
	}
	// REQ-PROV-15's worked example, against a real row: o3 has low/medium/high
	// and neither xhigh nor max, so max clamps down to high — and never to off,
	// which o3 records as present-null because it cannot stop reasoning.
	lvl, wire, ok := ClampThinkingLevel(o3, core.ThinkingMax)
	if !ok || lvl != core.ThinkingHigh || wire != "high" {
		t.Errorf("o3 max -> (%q, %q, %v), want (high, high, true)", lvl, wire, ok)
	}
	if lvl, _, _ := ClampThinkingLevel(o3, core.ThinkingMinimal); lvl != core.ThinkingLow {
		t.Errorf("o3 minimal -> %q, want low (upward, and never off)", lvl)
	}

	gpt4o, err := ResolveModel("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ClampThinkingLevel(gpt4o, core.ThinkingHigh); ok {
		t.Error("gpt-4o has no thinking map; the parameter must be omitted, not clamped")
	}
	if got := ClampMaxTokens(gpt4o, 100000, 0); got != 16384 {
		t.Errorf("gpt-4o max_tokens = %d, want its 16384 output cap", got)
	}

	opus, err := ResolveModel("anthropic/claude-opus-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if got := ClampMaxTokens(opus, 64000, 190000); got != 5904 {
		t.Errorf("opus deep in a session: %d, want 5904", got)
	}
	// A sibling clone inherits the window, so the clamp keeps working on a
	// model that did not exist when this catalog was written (NFR-COMPAT-03).
	clone, err := ResolveModel("anthropic/claude-opus-5-1-20991231")
	if err != nil {
		t.Fatal(err)
	}
	if !clone.Cloned {
		t.Fatal("expected a clone")
	}
	if got := ClampMaxTokens(clone, 64000, 190000); got != 5904 {
		t.Errorf("clone clamp = %d, want 5904 from the inherited window", got)
	}
}
