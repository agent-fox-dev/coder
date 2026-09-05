package provider

import "github.com/agentfox/agentkit-go/core"

// This file is REQ-PROV-05's arithmetic, shared by every wire API.
//
// It is shared because every trap in that requirement produces a WRONG NUMBER
// rather than an error. A wrong number is invisible: it does not fail a test
// that asserts the request succeeded, it does not appear in a log, and its
// only symptom is that the REQ-LOOP-08 budget gate fires early or late. Four
// independent implementations of this would be four chances to be silently
// wrong about money, and no way to notice.

// ServiceTier is REQ-PROV-05.6's post-hoc multiplier.
type ServiceTier struct {
	Name       string
	Multiplier float64 // 1 for standard, 0.5 flex, 2 priority
}

// RatesFor implements REQ-PROV-05.4: tier selection is REQUEST-WIDE.
//
// Selection uses input + cache_read + cache_write — NOT output, and NOT the
// total including output — and picks the highest tier whose threshold is
// STRICTLY exceeded. The selected tier's rates then apply to the WHOLE
// request, output included. Applying the tier only to the field that crossed
// the threshold is the intuitive reading and it is wrong in both directions.
func RatesFor(m *core.Model, u core.Usage) core.Cost {
	rates := m.Cost
	sizing := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	best := -1
	for _, t := range m.Cost.Tiers {
		if sizing > int64(t.Threshold) && t.Threshold > best {
			best = t.Threshold
			rates = core.Cost{
				Input: t.Input, Output: t.Output,
				CacheRead: t.CacheRead, CacheWrite: t.CacheWrite,
				Tiers: m.Cost.Tiers,
			}
		}
	}
	return rates
}

// ComputeCost prices a usage against a model's catalog row. Rates are USD per
// 1M tokens.
//
// Two subtleties, both from REQ-PROV-05:
//
//  1. InputTokens must already be NET of cache reads and writes (05.1). This
//     function does not subtract, because it cannot tell a net value from a
//     gross one, and subtracting an already-net value understates cost as
//     silently as not subtracting a gross one overstates it. The netting is
//     the DECODER's job, at the one place that knows the wire convention: the
//     OpenAI family reports prompt_tokens inclusive of cached tokens and must
//     subtract; Anthropic reports input_tokens exclusive of both and must not.
//
//  2. 1h cache writes bill at 2x BASE INPUT, not at the cache-write rate
//     (05.3). CacheWrite1hTokens is a SUBSET of CacheWriteTokens, so the
//     5-minute portion is the difference. Treating them as separate addends
//     double-counts every 1h write.
func ComputeCost(m *core.Model, u core.Usage) float64 {
	if m == nil {
		return 0
	}
	r := RatesFor(m, u)
	const perM = 1_000_000.0

	cw5m := u.CacheWriteTokens - u.CacheWrite1hTokens
	if cw5m < 0 {
		// A provider reporting more 1h writes than total writes is reporting
		// something we do not model. Bill the whole amount at the 1h rate
		// rather than crediting the caller a negative charge.
		cw5m = 0
	}

	cost := r.Input*float64(u.InputTokens) +
		r.Output*float64(u.OutputTokens) +
		r.CacheRead*float64(u.CacheReadTokens) +
		r.CacheWrite*float64(cw5m) +
		r.Input*2*float64(u.CacheWrite1hTokens)

	return cost / perM
}

// ApplyServiceTier is REQ-PROV-05.6. It is a separate call rather than a
// parameter of ComputeCost because the multiplier applies POST-HOC to the
// whole computed cost — including the tiered portion — and folding it into the
// rates would apply it before tier selection.
func ApplyServiceTier(cost float64, t ServiceTier) float64 {
	if t.Multiplier == 0 {
		return cost
	}
	return cost * t.Multiplier
}

// BillingModel implements REQ-PROV-05.5: bill the model that actually SERVED
// the request.
//
// served is the model id the response named. When it differs from the
// requested id, lookup resolves the served model's catalog row and that row's
// prices are used; billed reports what to record in Usage.BilledModel. When a
// later event names the requested model again, calling this again with that id
// reprices BACK — which is why cost is computed once at the end of a stream
// from the final name, never incrementally per event.
//
// lookup may return nil for an unknown served model. That case bills at the
// REQUESTED model's rates and still records the served name, which is the
// least-wrong option: the alternative is billing a fallback-served response at
// zero.
func BillingModel(requested *core.Model, served string, lookup func(string) *core.Model) (m *core.Model, billed string) {
	if served == "" || requested == nil || served == requested.ID {
		return requested, ""
	}
	if lookup != nil {
		if row := lookup(served); row != nil {
			return row, served
		}
	}
	return requested, served
}
