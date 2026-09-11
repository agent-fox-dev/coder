// Package stop holds the built-in StopPolicy implementations (REQ-LOOP-04).
//
// A stop policy is the single post-turn predicate the loop consults after a
// completed turn's tools have executed and their results are in history. It
// ends a run cleanly at a turn boundary, which is what distinguishes it from a
// context deadline (an abort mid-turn, leaving the REQ-LOOP-09 marker) and
// from BudgetMiddleware (a pre-turn gate that refuses to send at all).
//
// The types are core.StopPolicy and core.StopContext; this package only
// supplies implementations, so an embedder writing its own policy needs
// nothing from here.
package stop

import (
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// AfterTurns is REQ-LOOP-07 as what it actually is: a built-in StopPolicy
// implementation, not a loop primitive. The run ends with StopReason
// "max_turns"; ErrMaxTurns is returned only when AgentConfig.ErrorOnLimit is
// set.
func AfterTurns(n int) core.StopPolicy {
	return func(sc core.StopContext) bool {
		if sc.TurnCount >= n {
			sc.SetReason(core.RunStopMaxTurns)
			return true
		}
		return false
	}
}

// OverBudget is REQ-LOOP-08.
//
// Because the check runs post-turn, a run may overshoot the budget by at most
// one turn plus its tool batch. That is inherent to a post-turn predicate and
// is the documented behaviour: the pre-turn gate that prevents overshoot is
// BudgetMiddleware (Axis 1), a separate mechanism.
func OverBudget(usd float64) core.StopPolicy {
	return func(sc core.StopContext) bool {
		if sc.Usage.CostUSD > usd {
			sc.SetReason(core.RunStopBudgetExceeded)
			return true
		}
		return false
	}
}

// AfterDuration ends the run at the first turn boundary past d.
//
// It uses StopContext.StartedAt rather than a captured time.Now(): a policy
// value is reused across runs, so a closure capturing construction time would
// fire immediately on the second run. It is also not the same as a ctx
// deadline — a deadline aborts mid-turn and leaves the REQ-LOOP-09 dirty
// marker, where a StopPolicy ends the run cleanly at a turn boundary.
func AfterDuration(d time.Duration) core.StopPolicy {
	return func(sc core.StopContext) bool {
		if !sc.StartedAt.IsZero() && time.Since(sc.StartedAt) >= d {
			sc.SetReason(core.RunStopPolicy)
			return true
		}
		return false
	}
}

// WhenToolCalled ends the run once a named tool has produced a result. It
// is the sentinel-tool detection §5 names as a default policy implementation,
// and is distinct from REQ-TOOL-13 termination: this is the caller's policy
// observing the transcript, that is the tool voting on its own batch.
func WhenToolCalled(name string) core.StopPolicy {
	return func(sc core.StopContext) bool {
		for _, r := range sc.ToolResults {
			if r.ToolName == name {
				sc.SetReason(core.RunStopPolicy)
				return true
			}
		}
		return false
	}
}

// Any composes policies. The first to return true wins and its reason is
// preserved — which is why StopContext carries a Reason pointer at all: with a
// bare bool, Any erases which policy fired and the loop cannot tell
// ErrMaxTurns from ErrBudgetExceeded.
func Any(policies ...core.StopPolicy) core.StopPolicy {
	return func(sc core.StopContext) bool {
		for _, p := range policies {
			if p == nil {
				continue
			}
			if p(sc) {
				return true
			}
		}
		return false
	}
}

// Never is the explicit "run until the model stops" policy. It exists so
// that "no policy" and "deliberately unbounded" are distinguishable in a
// config a reviewer is reading.
func Never() core.StopPolicy { return func(core.StopContext) bool { return false } }
