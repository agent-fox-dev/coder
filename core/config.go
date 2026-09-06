package core

import (
	"context"
	"time"
)

// StopContext is handed to the single post-turn stop predicate (REQ-LOOP-04).
type StopContext struct {
	Message     *AssistantMessage
	ToolResults []ToolResultMessage
	History     *ConversationHistory
	NewMessages Messages
	TurnCount   int
	Usage       Usage

	// Reason is an ADDITION. REQ-LOOP-04's predicate returns one bit, but
	// REQ-LOOP-07 requires RunResult.StopReason = "max_turns" and, under
	// ErrorOnLimit, ErrMaxTurns specifically — versus ErrBudgetExceeded for
	// REQ-LOOP-08. With a bare bool the loop cannot tell which policy fired,
	// and StopAny erases it entirely. A policy sets *Reason before returning
	// true; the loop defaults it to RunStopPolicy.
	Reason *RunStopReason

	// StartedAt is an ADDITION. §5 names wall-clock deadlines as a default
	// StopPolicy, but StopContext carries no clock, and a closure capturing
	// time.Now() at construction is wrong for a policy value reused across
	// runs. A ctx deadline is not a substitute: it aborts mid-turn and
	// produces the REQ-LOOP-09 dirty marker, where a StopPolicy ends the run
	// cleanly at a turn boundary.
	StartedAt time.Time
}

func (sc StopContext) SetReason(r RunStopReason) {
	if sc.Reason != nil {
		*sc.Reason = r
	}
}

// StopPolicy is REQ-LOOP-04's pinned signature. Policies compose via StopAny.
type StopPolicy func(StopContext) bool

// ContextTransform is AgentConfig.TransformContext (REQ-GO-12): invoked
// immediately before canonical->wire conversion on every model call, at the
// head of each loop iteration (REQ-LOOP-04b, NFR-REL-05).
//
// The field holds a BOUND CLOSURE, not a free function: the pinned signature
// cannot return an error, cannot see the current model (which changes
// mid-session under REQ-SESS-03) and cannot reach the SessionStore to write
// the REQ-SESS-04 entry. Those inputs are supplied by binding.
type ContextTransform func(ctx context.Context, msgs Messages) Messages

// Hooks are Axis 2 (REQ-OBS-07): observation, never interception.
type Hooks struct {
	OnTurnStart func(TurnStartEvent)
	OnTurnEnd   func(TurnEndEvent)
	OnAgentDone func(AgentDoneEvent)
	OnError     func(error)

	// OnSessionStart and OnSessionEnd are REQ-OBS-03.
	//
	// The requirement names EventHookPlugin, and plugins are not built. The
	// hook POINTS are not a plugin feature though — a plugin would be one more
	// registrant — so they ship here, and a plugin host later becomes a
	// registrant rather than a redesign.
	//
	// OnSessionEnd fires exactly once per run, including on an error or an
	// abort. A hook that fires only on the happy path is worse than none: an
	// auditor cannot tell a session that ended badly from one still running.
	OnSessionStart func(AuditEvent)
	OnSessionEnd   func(AuditEvent)
	// OnAudit receives every AuditEvent, session start and end included, so a
	// single sink needs one registration rather than four.
	OnAudit func(AuditEvent)
}

// QueueMode is REQ-LOOP-15's per-queue delivery mode.
type QueueMode int

const (
	QueueOneAtATime QueueMode = iota // default
	QueueDrainAll
)

// AgentConfig collects the fields §5's table and the §6 requirements imply.
// Five of them (ErrorOnLimit, Hooks, AfterToolCall, the queue modes and
// TransformContext) are required by requirement text but absent from §5's
// table.
type AgentConfig struct {
	Model        *Model
	Provider     string // vendor id; credential resolution + catalog only
	MaxTokens    *int   // REQ-PROV-16 presence; an upper bound (REQ-CAT-04)
	Temperature  *float64
	TopP         *float64
	SystemPrompt string
	// PromptBlocks are extra system-prompt sections appended after the
	// built-in ones, in order — the skills and project-context block, or
	// anything an embedder assembles itself.
	//
	// Untyped text because core cannot import skills without inverting the
	// package graph, and because discovery is the embedder's affirmative act
	// (REQ-SKILL-04, REQ-SEC-10) rather than something the loop performs.
	PromptBlocks []string

	StopPolicy StopPolicy
	// ErrorOnLimit gates whether a limit stop also returns ErrMaxTurns /
	// ErrBudgetExceeded (REQ-LOOP-07).
	ErrorOnLimit  bool
	ParallelTools bool
	ToolChoice    ToolChoice
	ThinkingLevel ThinkingLevel
	ToolPolicy    ToolPolicy

	BeforeToolCall BeforeToolCall
	AfterToolCall  AfterToolCall
	Hooks          Hooks
	Middleware     []Middleware // last registered is outermost

	TransformContext ContextTransform

	SteeringQueueMode QueueMode
	FollowUpQueueMode QueueMode

	SessionID    string
	TrustProject bool
	// Plugins is REQ-PLUGIN-11's registry, held HERE and not in a
	// package-level global — so two agents in one process can carry different
	// plugin sets, and a test can inject a mock without patching global state
	// or freezing a registry against late registration.
	Plugins PluginRegistry
	// Tracer receives the REQ-OBS-02 tool spans. Nil means NoopTracer.
	//
	// It is separate from TracingMiddleware's tracer, which wraps the MODEL
	// call: middleware cannot see a tool execution at all, so a tracer that
	// reached the SDK only through Axis 1 would leave REQ-OBS-02
	// unimplementable. Pass the same value to both to get one trace.
	Tracer Tracer
	// Attribution defaults on and is disclosed (REQ-SEC-13). A single kill
	// switch disables every attribution header.
	Attribution    *bool
	CacheRetention CacheRetention
	RequestOptions RequestOptions
	StreamOptions  StreamOptions

	// Providers is the registry (REQ-PROV-09). Nil means the first-party
	// defaults, supplied by a pure function at construction — there is no
	// package-level registry and no init() to populate one (NFR-SEC-05).
	Providers ProviderRegistry
	// SessionStore is optional. A non-empty store passed to NewAgent is
	// ErrSessionNotEmpty: a non-empty log must be folded first (REQ-SESS-02).
	SessionStore SessionStore
	// OnPersistError is REQ-SESS-08's mandatory seam for an internally
	// subscribed store. Silent failure is prohibited.
	OnPersistError func(error)
}

// Middleware is Axis 1: it wraps the entire model call and operates on
// canonical types. It can change WHAT is asked for, not HOW the provider
// encoded it — post-serialization interception is RequestOptions.OnPayload.
type Middleware func(next Handler) Handler

// Handler is one link of the Axis 1 chain.
type Handler func(ctx context.Context, req Request) *EventStream

// Chain composes middleware so that the LAST registered is outermost and first
// to execute (§5, Axis 1).
func Chain(base Handler, mw ...Middleware) Handler {
	h := base
	for i := 0; i < len(mw); i++ {
		h = mw[i](h)
	}
	return h
}
