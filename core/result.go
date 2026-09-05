package core

import "time"

// RunResult is the flattened ACTIVE BRANCH — a view, not the durable
// representation, and not what a caller should persist (§5, NFR-REL-04).
type RunResult struct {
	Messages   Messages
	StopReason RunStopReason
	// LastReason is the final assistant message's canonical reason, kept
	// because RunStopReason and StopReason are different vocabularies.
	LastReason StopReason
	Usage      Usage
	TurnCount  int
	Error      error
}

func (r RunResult) FinalText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if a, ok := r.Messages[i].(AssistantMessage); ok {
			return a.Content.Text()
		}
	}
	return ""
}

// Phase reports what the agent is doing right now (REQ-LIFE-01). It is backed
// by an atomic, never by a lock held across a model call.
type Phase int32

const (
	PhaseIdle Phase = iota
	PhaseCallingModel
	PhaseExecutingTools
	PhaseCompacting
	// PhaseBetweenTurns covers the window after TurnEndEvent while the stop
	// policy and hooks run. The PRD's four-value enum has no state for it, and
	// reporting PhaseIdle there would be actively dangerous: REQ-LIFE-06 makes
	// Idle() the only safe point to dispose of the agent.
	PhaseBetweenTurns
)

func (p Phase) String() string {
	switch p {
	case PhaseIdle:
		return "idle"
	case PhaseCallingModel:
		return "calling_model"
	case PhaseExecutingTools:
		return "executing_tools"
	case PhaseCompacting:
		return "compacting"
	case PhaseBetweenTurns:
		return "between_turns"
	}
	return "unknown"
}

type ConfigView struct {
	ModelID       string        `json:"model_id"`
	Provider      string        `json:"provider"`
	API           API           `json:"api"`
	MaxTokens     *int          `json:"max_tokens"`
	ThinkingLevel ThinkingLevel `json:"thinking_level"`
	ToolNames     []string      `json:"tool_names"`
}

// SessionSnapshot is REQ-LIFE-02's resync target.
type SessionSnapshot struct {
	// ProducerID must be compared BEFORE Revision: revisions from two
	// producers are unordered, and a snapshot restored into a new Agent
	// restarts the counter.
	ProducerID string     `json:"producer_id"`
	Revision   uint64     `json:"revision"`
	Phase      Phase      `json:"phase"`
	// Idle false means the snapshot is consistent, not complete (REQ-LIFE-07).
	Idle      bool       `json:"idle"`
	SessionID string     `json:"session_id"`
	Config    ConfigView `json:"config"`
	Messages  Messages   `json:"messages"`
	Usage     Usage      `json:"usage"`
	TakenAt   time.Time  `json:"taken_at"`
}
