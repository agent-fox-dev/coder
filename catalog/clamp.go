package catalog

import "github.com/agentfox/agentkit-go/core"

// DefaultSafetyMargin is REQ-CAT-04's safety margin, in tokens. It absorbs the
// difference between the loop's context estimate and the provider's own
// tokenizer; being a few hundred tokens optimistic here is a 400 deep into a
// long session, which is exactly when losing the conversation costs most.
const DefaultSafetyMargin = 4096

// ClampMaxTokens implements REQ-CAT-04 with the default safety margin.
//
// estContextTokens is SUPPLIED BY THE CALLER, not recomputed here — ruling
// P-30. REQ-GO-15 anchors the context estimate in the loop and forbids
// re-walking the transcript to derive it, so the estimate arrives on
// core.Request.EstContextTokens and this function does arithmetic only. A
// clamp that re-estimated would disagree with the compaction gate that used
// the anchored number, and the two would fight.
func ClampMaxTokens(m *core.Model, requested, estContextTokens int) int {
	return ClampMaxTokensWithMargin(m, requested, estContextTokens, DefaultSafetyMargin)
}

// ClampMaxTokensWithMargin is ClampMaxTokens with an explicit safety margin.
//
// The result is the value to send:
//
//	available = ContextWindow - estContextTokens - safetyMargin
//	sent      = min(requested, model.MaxTokens, max(1, available))
//
// with two departures from REQ-CAT-04's literal formula, both deliberate:
//
//   - model.MaxTokens participates. The requirement's formula omits the
//     model's own output cap, but exceeding it is a 400 on every provider, and
//     a request whose context is nearly empty would otherwise be handed the
//     entire window as an output budget.
//
//   - An UNKNOWN context window (ContextWindow <= 0) yields the model's own
//     MaxTokens cap with NO window arithmetic — ruling P-29. Read literally,
//     "models with an unknown context window receive the floor only" means the
//     floor of max(1, available), which is 1: a one-token completion for every
//     model the catalog has no window for, including every sibling clone whose
//     template lacked one. That is not a clamp, it is an outage.
//
// A requested value of 0 or less means "the caller set no bound" (REQ-PROV-16
// presence lives in the *int on Request; this function takes the dereferenced
// number), and the model's cap is used instead. The result is 0 only when
// nothing was requested and the model declares no cap and no window, which
// means "there is nothing to send".
func ClampMaxTokensWithMargin(m *core.Model, requested, estContextTokens, safetyMargin int) int {
	if m == nil {
		return requested
	}
	limit := requested
	if limit <= 0 {
		limit = m.MaxTokens
	}
	if m.MaxTokens > 0 && (limit <= 0 || limit > m.MaxTokens) {
		limit = m.MaxTokens
	}
	if m.ContextWindow <= 0 {
		return limit // P-29: no window arithmetic, and no floor of 1.
	}
	if estContextTokens < 0 {
		estContextTokens = 0
	}
	if safetyMargin < 0 {
		safetyMargin = 0
	}
	available := m.ContextWindow - estContextTokens - safetyMargin
	if available < 1 {
		available = 1 // REQ-CAT-04's max(1, available)
	}
	if limit <= 0 || limit > available {
		limit = available
	}
	return limit
}

// ClampRequestMaxTokens is the provider-facing form: it reads the request's
// own max_tokens and the loop-supplied context estimate (P-30) and returns the
// value to send, preserving REQ-PROV-16 presence.
//
// A nil result means the caller set no max_tokens and none should be emitted.
// Absence is NOT converted into the model's cap here: on an API where
// max_tokens is optional, inventing one silently caps output that the caller
// never asked to cap. A provider whose API requires the field passes
// model.MaxTokens to ClampMaxTokens explicitly instead — the clamp is the same
// arithmetic either way, and the decision to supply a default stays with the
// adapter that knows its API requires one.
func ClampRequestMaxTokens(m *core.Model, req core.Request) *int {
	if req.MaxTokens == nil {
		return nil
	}
	v := ClampMaxTokens(m, *req.MaxTokens, req.EstContextTokens)
	return &v
}

// ThinkingWire returns a model's wire value for one thinking level, and
// whether the level is supported at all.
//
// Present-and-null and absent are BOTH unsupported here. REQ-PROV-15 says the
// two are "distinct", and they are — but only to the catalog author: a
// present-null entry records "we checked this model and it cannot do this
// level", an absent one records nothing, and the REQ-CAT-06 regeneration diff
// needs to tell those apart. At runtime there is no difference and there is no
// hidden behaviour to find (ruling P-28). Anyone hunting for one is hunting
// for something that does not exist.
func ThinkingWire(m *core.Model, level core.ThinkingLevel) (string, bool) {
	if m == nil || m.ThinkingLevelMap == nil {
		return "", false
	}
	w, present := m.ThinkingLevelMap[level]
	if !present || w == nil {
		return "", false
	}
	// A supported level may map to the empty string: on some APIs the level is
	// carried by a separate field and the token itself is empty. Presence of a
	// non-nil value is the support signal, never emptiness (REQ-PROV-16.2).
	return *w, true
}

// SupportsThinkingLevel reports whether a model supports exactly this level,
// with no clamping.
func SupportsThinkingLevel(m *core.Model, level core.ThinkingLevel) bool {
	_, ok := ThinkingWire(m, level)
	return ok
}

// ClampThinkingLevel implements REQ-PROV-15's clamp: it returns the level to
// use, that model's wire value for it, and whether the model supports any
// usable level at all.
//
// The search runs UPWARD from the requested level first, and only then
// downward:
//
//   - Upward first, because "a user asking for maximum thinking gets the most
//     the model offers, never silently less". Requesting xhigh on a model
//     whose map is {xhigh: null, max: "max"} clamps UP to max. A
//     nearest-neighbour search would answer high there, which is less thinking
//     than was asked for and is indistinguishable, from the outside, from the
//     request having worked.
//
//   - Downward second, and never to off — ruling P-27. `off` is index 0 of
//     core.ThinkingLevelOrder and the downward pass stops above it. Turning a
//     request for *some* thinking into *no* thinking is a behaviour change
//     wearing a clamp's clothes: it changes what the model does, not merely
//     how hard it tries. "Unless explicitly requested" needs no exception in
//     the downward pass, because a request for off is served by the upward
//     pass, which starts at off itself.
//
// ok == false means the model supports no level reachable from the request;
// the caller omits the parameter entirely rather than passing an unclamped
// level through, which REQ-PROV-15 prohibits ("reasoning_effort: \"xhigh\" to
// a model that does not know it is a 400").
//
// A request of ThinkingUnset returns ok == false: the zero value means the
// caller expressed no opinion (core.ThinkingUnset), and clamping "no opinion"
// upward to max would be the SDK spending the caller's money on its own.
func ClampThinkingLevel(m *core.Model, requested core.ThinkingLevel) (core.ThinkingLevel, string, bool) {
	if m == nil || requested == core.ThinkingUnset {
		return core.ThinkingUnset, "", false
	}
	order := core.ThinkingLevelOrder
	i := -1
	for idx, l := range order {
		if l == requested {
			i = idx
			break
		}
	}
	if i < 0 {
		// A level string this build does not know. Omitting is the only safe
		// answer: passing it through is the 400 REQ-PROV-15 prohibits.
		return core.ThinkingUnset, "", false
	}

	// Upward, starting at the requested level itself.
	for j := i; j < len(order); j++ {
		if w, ok := ThinkingWire(m, order[j]); ok {
			return order[j], w, true
		}
	}
	// Downward, stopping above index 0 (ThinkingOff) — P-27.
	for j := i - 1; j >= 1; j-- {
		if w, ok := ThinkingWire(m, order[j]); ok {
			return order[j], w, true
		}
	}
	return core.ThinkingUnset, "", false
}
