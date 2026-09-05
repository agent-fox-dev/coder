package core

// StopReason is the canonical, lossy normalization of a provider finish
// reason (§5). RawStopReason on AssistantMessage keeps the provider's own
// string.
//
// NAMING RULING: §5 names this enum member "Length"; REQ-LOOP-10 and REQ-GO-16
// use the wire string "max_tokens". The Go identifier follows §5 and the wire
// value follows REQ-LOOP-10. They are the same thing.
type StopReason string

const (
	StopReasonStop         StopReason = "stop"
	StopReasonLength       StopReason = "max_tokens"
	StopReasonToolUse      StopReason = "tool_use"
	StopReasonStopSequence StopReason = "stop_sequence"
	StopReasonRefusal      StopReason = "refusal"
	StopReasonError        StopReason = "error"
	StopReasonAborted      StopReason = "aborted"
	// StopReasonDeferred is reserved by REQ-PROV-19/OQ-11. No v1 provider
	// emits it. The loop carries an explicit branch that returns
	// ErrDeferredUnsupported rather than treating it as an empty completion,
	// because reserving the constant alone reproduces the exact failure OQ-11
	// says reserving it prevents.
	StopReasonDeferred StopReason = "deferred"
)

// ShortCircuits reports the two reasons that terminate a turn BEFORE tool
// extraction (REQ-LOOP-01). Every other reason, recognized or not, is
// control-flow-identical.
func (r StopReason) ShortCircuits() bool {
	return r == StopReasonError || r == StopReasonAborted
}

// RunStopReason is why a RUN ended. It is a distinct type from StopReason
// because REQ-LOOP-07's "max_turns" is not something any model can say, and
// because RunResult.StopReason == StopReasonLength would otherwise be
// ambiguous between "the model hit its output cap" and "the run hit its turn
// cap".
type RunStopReason string

const (
	RunStopEndTurn        RunStopReason = "end_turn"
	RunStopMaxTurns       RunStopReason = "max_turns"
	RunStopBudgetExceeded RunStopReason = "budget_exceeded"
	RunStopPolicy         RunStopReason = "stop_policy"
	RunStopToolTerminate  RunStopReason = "tool_terminate"
	RunStopError          RunStopReason = "error"
	RunStopAborted        RunStopReason = "aborted"
)

// ExtractToolUse is the continuation predicate of REQ-LOOP-01, by name. It
// scans Content for ToolUseBlock and reads no stop reason. This is the whole
// of the fix for Appendix A correction #1.
func ExtractToolUse(m *AssistantMessage) []ToolUseBlock {
	if m == nil {
		return nil
	}
	var out []ToolUseBlock
	for _, b := range m.Content {
		if tu, ok := b.(ToolUseBlock); ok {
			out = append(out, tu)
		}
	}
	return out
}

// ShouldIterate is ExtractToolUse's boolean form. The loop calls this; nothing
// in the loop compares a StopReason to decide whether to continue.
func ShouldIterate(m *AssistantMessage) bool { return len(ExtractToolUse(m)) > 0 }

// API identifies a wire protocol (REQ-PROV-02). Providers are keyed by this,
// never by vendor: Model.Provider is a vendor id used only for credential
// resolution and catalog lookup.
type API string

const (
	APIAnthropicMessages API = "anthropic-messages"
	APIOpenAICompletions API = "openai-completions"
	APIOpenAIResponses   API = "openai-responses"     // reserved; not implemented in v1
	APIGoogleGenerative  API = "google-generative-ai" // reserved; not implemented in v1
	APIOllamaChat        API = "ollama-chat"          // reserved; not implemented in v1
	APIFaux              API = "faux"                 // NFR-TEST-05, shipped and supported
)

// ThinkingLevel is REQ-PROV-15's request parameter and assistant provenance.
type ThinkingLevel string

const (
	ThinkingUnset   ThinkingLevel = ""
	ThinkingOff     ThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel = "minimal"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
	ThinkingXHigh   ThinkingLevel = "xhigh"
	ThinkingMax     ThinkingLevel = "max"
)

// ThinkingLevelOrder is the total order ClampThinkingLevel searches (upward
// first, then downward). Exported so the catalog does not re-derive it.
// `off` is excluded from a DOWNWARD clamp target unless it was explicitly
// requested: clamping a request for some thinking down to no thinking is a
// behaviour change, not a clamp.
var ThinkingLevelOrder = []ThinkingLevel{
	ThinkingOff, ThinkingMinimal, ThinkingLow, ThinkingMedium,
	ThinkingHigh, ThinkingXHigh, ThinkingMax,
}

// ToolChoice is REQ-TOOL-16's provider-neutral tri-state. The zero value is
// Unset, so absent cannot be confused with auto and needs no pointer.
type ToolChoice string

const (
	ToolChoiceUnset ToolChoice = ""
	ToolChoiceAuto  ToolChoice = "auto"
	ToolChoiceNone  ToolChoice = "none"
)

func (c ToolChoice) IsSet() bool { return c != ToolChoiceUnset }

// CacheRetention is §6.2a Level 1's tri-state.
type CacheRetention string

const (
	CacheRetentionNone  CacheRetention = "none"
	CacheRetentionShort CacheRetention = "short"
	CacheRetentionLong  CacheRetention = "long"
)
