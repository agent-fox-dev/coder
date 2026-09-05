package core

import "errors"

// Sentinel errors of REQ-GO-09, all comparable with errors.Is.
var (
	ErrMaxTurns       = errors.New("agentkit: maximum turns reached")
	ErrBudgetExceeded = errors.New("agentkit: budget exceeded")
	ErrToolRejected   = errors.New("agentkit: tool call rejected")
	ErrRefusal        = errors.New("agentkit: model refused the request")
	// ErrBusy: a conflicting operation was attempted while a turn was in
	// flight (REQ-LOOP-15). The caller may retry; it is never queued.
	ErrBusy = errors.New("agentkit: agent is busy")
	// ErrAborted: stopped by Agent.Abort(), deliberately distinguishable from
	// context.Canceled, which signals the caller's own ctx (REQ-GO-09).
	ErrAborted       = errors.New("agentkit: run aborted")
	ErrStreamOverrun = errors.New("agentkit: stream consumer overrun")

	// Raised by the loop / session layer.
	ErrNotContinuable = errors.New("agentkit: transcript is not continuable")
	ErrSessionExists  = errors.New("agentkit: session log already exists at path")
	// ErrSessionNotEmpty makes the forbidden "construct, then patch the model
	// onto it" resume a runtime failure on the first line rather than a style
	// violation (REQ-SESS-02).
	ErrSessionNotEmpty = errors.New(
		"agentkit: session store is not empty; resume with LoadSession + NewAgentFromSession")
	// ErrDeferredUnsupported closes OQ-11's gap: under REQ-LOOP-01 an
	// unrecognized reason with no tool calls exits the inner loop normally, so
	// a deferred response would become "an empty completion" — the exact
	// failure reserving the constant is supposed to prevent.
	ErrDeferredUnsupported = errors.New("agentkit: provider returned a deferred response; v1 has no poller")
)
