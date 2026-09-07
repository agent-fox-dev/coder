package agentkit

import (
	"context"
	"fmt"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// Deferred submission, REQ-PROV-19 as scoped by OQ-11 option (b): the type,
// the stop reason and the redemption call ship; the POLLER does not. Nothing
// here sleeps, retries, or watches a handle — when to come back is the
// embedder's decision, and a library that guessed would be spending someone
// else's latency budget on a schedule it invented.
//
// The shape of a deferred session:
//
//	if !reg.SupportsDeferred(model.API) { /* submit normally */ }
//	cfg.RequestOptions.Deferred = &core.DeferredRequest{Window: 24 * time.Hour}
//	res, _ := agent.Run(ctx, prompt)          // ends RunStopDeferred
//	h, _ := res.DeferredHandle()              // durable; persisted in the log
//	                                          // ... a day and a restart later:
//	msg, err := agent.RedeemDeferred(ctx, h)  // the answer, appended to history

// SupportsDeferred reports whether this agent's wire can accept a background
// submission (REQ-PROV-19's capability probe). Ask BEFORE submitting: a
// provider that ignores the field answers immediately, and a caller that
// assumed otherwise waits for a handle that is never coming.
func (a *Agent) SupportsDeferred() bool {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	reg := cfg.Providers
	if reg == nil {
		reg = DefaultProviders()
	}
	return reg.SupportsDeferred(cfg.Model.API)
}

// RedeemDeferred collects the answer for a handle and appends it to history
// and to the session log, exactly as a live turn would land.
//
// It is NOT a poller: one call, one attempt. A handle whose `PollAfterMS` has
// not elapsed is refused rather than sent, because a provider that told us
// when to come back has already answered the question of whether it is ready,
// and burning the request to hear "not yet" is a round trip for nothing.
//
// The run slot is claimed for the duration, so redemption cannot interleave
// with a live turn writing to the same history (REQ-LOOP-15).
func (a *Agent) RedeemDeferred(ctx context.Context, h core.DeferredHandle) (core.AssistantMessage, error) {
	if h.IsZero() {
		return core.AssistantMessage{}, fmt.Errorf("agentkit: RedeemDeferred: empty handle")
	}
	now := time.Now()
	if h.Expired(now) {
		return core.AssistantMessage{}, fmt.Errorf(
			"agentkit: deferred handle %q expired at %s; the answer is gone and the request must be re-issued",
			h.ID, h.ExpiresAt.Format(time.RFC3339))
	}

	rctx, cancel, pending, err := a.claimSlot(ctx)
	if err != nil {
		return core.AssistantMessage{}, err
	}
	defer cancel()
	defer a.releaseSlot()
	// Anything the slot claim drained goes back: redemption is not a turn and
	// must not swallow a steering message meant for the next one.
	if len(pending) > 0 {
		a.mu.Lock()
		a.steering = append(pending, a.steering...)
		a.mu.Unlock()
	}

	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	reg := cfg.Providers
	if reg == nil {
		reg = DefaultProviders()
	}

	// The handle's own model, not the agent's current one: a session whose
	// model changed after the submission (REQ-SESS-03) must still redeem
	// against the wire holding the answer.
	model := cfg.Model
	if model == nil || model.ID != h.ModelID || model.API != h.API {
		return core.AssistantMessage{}, fmt.Errorf(
			"agentkit: deferred handle %q was issued for model %q on api %q, and this agent is on %q; "+
				"redeem it from an agent configured for the issuing model",
			h.ID, h.ModelID, h.API, cfg.Model.ID)
	}

	a.setPhase(core.PhaseCallingModel)
	defer a.setPhase(core.PhaseIdle)

	stream := reg.FetchDeferred(rctx, model, h, core.ProviderStreamOptions{CacheRetention: cfg.CacheRetention})
	msg := stream.Result()
	if msg == nil {
		err := stream.Err()
		if err == nil {
			err = fmt.Errorf("agentkit: the provider ended the redemption stream with no message")
		}
		return core.AssistantMessage{}, err
	}
	if msg.StopReason == core.StopReasonError {
		return *msg, fmt.Errorf("agentkit: redeeming deferred handle %q: %s", h.ID, msg.ErrorMessage)
	}

	// The redeemed answer replaces nothing: it is appended, so the transcript
	// reads submission-then-answer and the append-only log stays true.
	if _, err := a.rec.RecordMessage(*msg); err != nil {
		a.fireError(fmt.Errorf("agentkit: persisting redeemed message: %w", err))
	}
	a.addUsage(msg.Usage)
	return *msg, nil
}
