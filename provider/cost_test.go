package provider_test

import (
	"math"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
)

func near(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %.10f, want %.10f", what, got, want)
	}
}

// priced is a model whose rates are round numbers, so every expectation below
// is arithmetic a reader can check in their head rather than a golden value.
func priced() *core.Model {
	return &core.Model{ID: "m", Cost: core.Cost{
		Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75,
	}}
}

func TestCostIsRatePerMillionTokens(t *testing.T) {
	var u core.Usage
	u.SetField(core.UsageInputTokens, 1_000_000)
	u.SetField(core.UsageOutputTokens, 1_000_000)
	near(t, provider.ComputeCost(priced(), u), 18, "cost")
}

// TestOneHourCacheWritesBillAtTwiceBaseInput is REQ-PROV-05.3, and the trap is
// the word SUBSET.
//
// cache_write_1h is inside cache_write, not beside it. Treating them as two
// addends bills the 1h portion twice — once at the cache-write rate and again
// at 2x input — which is a 150% overcharge on exactly the long-lived agent
// session that opted into 1h retention to save money.
func TestOneHourCacheWritesBillAtTwiceBaseInput(t *testing.T) {
	var u core.Usage
	u.SetField(core.UsageCacheWriteTokens, 1_000_000)
	u.SetField(core.UsageCacheWrite1hTokens, 400_000)

	// 600k at the 3.75 cache-write rate + 400k at 2 x 3 base input.
	want := 0.6*3.75 + 0.4*6
	near(t, provider.ComputeCost(priced(), u), want, "cost")

	var naive core.Usage
	naive.SetField(core.UsageCacheWriteTokens, 1_000_000)
	if provider.ComputeCost(priced(), u) <= provider.ComputeCost(priced(), naive) {
		t.Fatal("a 1h write must cost MORE than the same volume of 5m writes")
	}
}

// TestTiersAreRequestWideAndStrictlyExceeded is REQ-PROV-05.4.
//
// Two independent traps: the threshold is compared against
// input+cache_read+cache_write (not the total, and not output), and the
// selected tier's rates apply to the WHOLE request including output. Applying
// the tier only to the field that crossed it is the intuitive reading and it
// is wrong in both directions.
func TestTiersAreRequestWideAndStrictlyExceeded(t *testing.T) {
	m := priced()
	m.Cost.Tiers = []core.CostTier{{
		Threshold: 200_000, Input: 6, Output: 22.5, CacheRead: 0.6, CacheWrite: 7.5,
	}}

	t.Run("exactly at the threshold stays on base rates", func(t *testing.T) {
		var u core.Usage
		u.SetField(core.UsageInputTokens, 200_000)
		near(t, provider.ComputeCost(m, u), 0.2*3, "cost")
	})

	t.Run("strictly above switches the whole request", func(t *testing.T) {
		var u core.Usage
		u.SetField(core.UsageInputTokens, 200_001)
		u.SetField(core.UsageOutputTokens, 1_000_000)
		// OUTPUT is priced at the tier rate too, though output did not
		// participate in selecting the tier.
		want := 0.200001*6 + 1*22.5
		near(t, provider.ComputeCost(m, u), want, "cost")
	})

	t.Run("output alone never selects a tier", func(t *testing.T) {
		var u core.Usage
		u.SetField(core.UsageInputTokens, 10)
		u.SetField(core.UsageOutputTokens, 5_000_000)
		if r := provider.RatesFor(m, u); r.Output != 15 {
			t.Fatalf("output rate = %v, want the base 15: tier selection uses "+
				"input+cache_read+cache_write only", r.Output)
		}
	})

	t.Run("cache reads count toward selection", func(t *testing.T) {
		var u core.Usage
		u.SetField(core.UsageInputTokens, 1)
		u.SetField(core.UsageCacheReadTokens, 300_000)
		if r := provider.RatesFor(m, u); r.Input != 6 {
			t.Fatalf("input rate = %v, want the tier's 6: a well-cached request whose "+
				"cache reads exceed the threshold is still a large request", r.Input)
		}
	})
}

func TestHighestExceededTierWins(t *testing.T) {
	m := priced()
	m.Cost.Tiers = []core.CostTier{
		{Threshold: 1_000_000, Input: 12, Output: 45},
		{Threshold: 200_000, Input: 6, Output: 22.5},
	}
	var u core.Usage
	u.SetField(core.UsageInputTokens, 2_000_000)
	if r := provider.RatesFor(m, u); r.Input != 12 {
		t.Fatalf("input rate = %v, want 12: the HIGHEST exceeded threshold wins, "+
			"regardless of the order the rows appear in", r.Input)
	}
}

// TestServiceTierMultipliesPostHoc is REQ-PROV-05.6. Post-hoc matters: folding
// the multiplier into the rates would apply it before tier selection.
func TestServiceTierMultipliesPostHoc(t *testing.T) {
	var u core.Usage
	u.SetField(core.UsageInputTokens, 1_000_000)
	base := provider.ComputeCost(priced(), u)
	near(t, provider.ApplyServiceTier(base, provider.ServiceTier{Name: "flex", Multiplier: 0.5}), 1.5, "flex")
	near(t, provider.ApplyServiceTier(base, provider.ServiceTier{Name: "priority", Multiplier: 2}), 6, "priority")
	near(t, provider.ApplyServiceTier(base, provider.ServiceTier{}), 3, "unset tier is a no-op")
}

// TestAFallbackServedResponseIsBilledAtTheServedModelsRates is REQ-PROV-05.5.
//
// A server-side refusal fallback serves a cheaper or dearer model than the one
// requested. Billing the requested row is silently wrong, and the only symptom
// is a budget gate that fires at the wrong time.
func TestAFallbackServedResponseIsBilledAtTheServedModelsRates(t *testing.T) {
	requested := priced()
	served := &core.Model{ID: "cheap", Cost: core.Cost{Input: 0.25, Output: 1.25}}
	lookup := func(id string) *core.Model {
		if id == "cheap" {
			return served
		}
		return nil
	}

	m, billed := provider.BillingModel(requested, "cheap", lookup)
	if billed != "cheap" || m != served {
		t.Fatalf("BillingModel = (%v, %q), want the served row recorded as billed_model", m, billed)
	}

	// And when a later event names the requested model again, the SAME call
	// reprices BACK. This is why cost is computed once from the final name
	// rather than accumulated per event.
	m2, billed2 := provider.BillingModel(requested, requested.ID, lookup)
	if m2 != requested || billed2 != "" {
		t.Fatalf("repricing back gave (%v, %q), want the requested row and no billed_model",
			m2, billed2)
	}
}

func TestAnUnknownServedModelStillRecordsTheServedName(t *testing.T) {
	requested := priced()
	m, billed := provider.BillingModel(requested, "some-model-shipped-today", nil)
	if m != requested {
		t.Fatal("an unknown served model bills at the requested rates: the alternative " +
			"is billing a fallback-served response at zero")
	}
	if billed != "some-model-shipped-today" {
		t.Fatalf("billed_model = %q, want the served name recorded even when its row "+
			"is unknown", billed)
	}
}

func TestNegativeFiveMinuteWriteVolumeDoesNotCreditTheCaller(t *testing.T) {
	var u core.Usage
	u.SetField(core.UsageCacheWriteTokens, 100)
	u.SetField(core.UsageCacheWrite1hTokens, 500) // nonsense: 1h exceeds the total
	if got := provider.ComputeCost(priced(), u); got < 0 {
		t.Fatalf("cost = %v; a provider reporting something we do not model must never "+
			"produce a negative charge", got)
	}
}
