package agentkit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// maxTokensToolText is REQ-LOOP-10's fixed result text, pinned byte-for-byte
// because it is model-visible. The line break inside it is a literal newline
// (ruling P-43): the PRD renders it across two lines inside a fenced block,
// and "inherit whatever the implementer typed" is not a specification for text
// the model reads.
const maxTokensToolText = "Tool call %q was not executed: the response hit the output token limit,\n" +
	"so its arguments may be truncated. Re-issue the tool call with complete arguments."

// Run appends a user message and runs the loop to a stop condition.
func (a *Agent) Run(ctx context.Context, prompt string) (core.RunResult, error) {
	return a.RunMessage(ctx, core.UserMessage{
		Content:   core.Content{core.TextBlock{Text: prompt}},
		Timestamp: time.Now(),
	})
}

// RunMessage is Run with a caller-built message.
func (a *Agent) RunMessage(ctx context.Context, m core.UserMessage) (core.RunResult, error) {
	s, err := a.stream(ctx, &m, false)
	if err != nil {
		return core.RunResult{}, err
	}
	return s.RunResult()
}

// Continue resumes a transcript without supplying a new user message
// (REQ-LOOP-16). It is a distinct operation from Run and is required for
// resuming a session loaded from disk: Run cannot express it, because Run
// would append a spurious user turn.
//
// Precondition by the role of the last message:
//
//	user, tool_result — resume with no new message; the model owes a reply.
//	assistant         — drain the queues and run with those; if both are
//	                    empty, ErrNotContinuable. A completed assistant turn
//	                    is not continuable.
//
// Resuming a transcript that ends in a tool_result is the normal outcome of
// REQ-LOOP-09 cancellation, so this is not an optional convenience.
func (a *Agent) Continue(ctx context.Context) (core.RunResult, error) {
	s, err := a.stream(ctx, nil, true)
	if err != nil {
		return core.RunResult{}, err
	}
	return s.RunResult()
}

// Stream runs the loop and returns the event stream. The producer never blocks
// on the consumer (REQ-GO-08), so abandoning the stream is safe and the result
// stays available via RunResult.
//
// It returns an error rather than only a pre-closed stream for the one case a
// caller most often gets wrong — calling Run twice concurrently — because a
// silent ErrBusy inside a stream nobody reads is indistinguishable from a run
// that produced nothing (ruling C5).
func (a *Agent) Stream(ctx context.Context, prompt string) (*core.EventStream, error) {
	m := core.UserMessage{Content: core.Content{core.TextBlock{Text: prompt}}, Timestamp: time.Now()}
	return a.stream(ctx, &m, false)
}

func (a *Agent) stream(ctx context.Context, initial *core.UserMessage, isContinue bool) (*core.EventStream, error) {
	rctx, cancel, pending, err := a.claimSlot(ctx)
	if err != nil {
		return nil, err
	}

	if isContinue {
		if err := a.checkContinuable(pending); err != nil {
			// Put the drained messages back: we claimed the slot and drained
			// under one lock, so bailing out here must not eat them.
			a.mu.Lock()
			a.steering = append(pending, a.steering...)
			a.mu.Unlock()
			cancel()
			a.releaseSlot()
			return nil, err
		}
	}

	a.mu.Lock()
	opts := a.cfg.StreamOptions
	a.mu.Unlock()

	s := core.NewEventStream(opts)
	go func() {
		defer cancel()
		defer a.releaseSlot()
		res, err := a.runLoop(rctx, s, initial, pending)
		s.End(core.StreamResult{Message: lastAssistant(res.Messages), Result: &res, Err: err})
	}()
	return s, nil
}

// checkContinuable enforces REQ-LOOP-16's precondition table.
func (a *Agent) checkContinuable(pending []core.Message) error {
	role, ok := a.history.LastRole()
	if !ok {
		return fmt.Errorf("%w: history is empty", core.ErrNotContinuable)
	}
	switch role {
	case core.RoleUser, core.RoleToolResult:
		return nil
	case core.RoleAssistant:
		a.mu.Lock()
		queued := len(pending) > 0 || len(a.steering) > 0 || len(a.followUp) > 0
		a.mu.Unlock()
		if queued {
			return nil
		}
		return fmt.Errorf("%w: last message is a completed assistant turn and no message is queued; "+
			"use Run to add one, or Steer/FollowUp first", core.ErrNotContinuable)
	}
	return fmt.Errorf("%w: unexpected last role %q", core.ErrNotContinuable, role)
}

func lastAssistant(ms core.Messages) *core.AssistantMessage {
	for i := len(ms) - 1; i >= 0; i-- {
		if am, ok := ms[i].(core.AssistantMessage); ok {
			return &am
		}
	}
	return nil
}

// runLoop is the loop of §5. Read it top to bottom; the order of the phases is
// the specification, and several of them are placed where they are because the
// obvious placement is a bug.
func (a *Agent) runLoop(ctx context.Context, s *core.EventStream, initial *core.UserMessage, pending []core.Message) (core.RunResult, error) {
	startedAt := time.Now()
	var (
		newMessages core.Messages
		turnCount   int
		runReason   = core.RunStopEndTurn
		runErr      error
	)

	record := func(msgs ...core.Message) {
		for _, m := range msgs {
			a.history.Record(a.entryFor(m), m)
			newMessages = append(newMessages, m)
		}
	}

	if initial != nil {
		record(*initial)
	}
	// Steering drained at slot-claim time is delivered into this run's first
	// turn rather than dropped.
	if len(pending) > 0 {
		record(pending...)
	}

	s.Push(core.AgentStartEvent{SessionID: a.cfg.SessionID, Provider: a.cfg.Model.Provider, API: a.cfg.Model.API, Model: a.cfg.Model.ID})

	// Outer loop: a pending follow-up restarts it within the SAME run — no
	// second AgentStartEvent, one RunResult (REQ-LOOP-14).
outer:
	for {
	inner:
		for {
			// ---- Phase 1: drain steering, BEFORE the context transform.
			//
			// §5's pseudocode drains after PrepareNextTurn; REQ-LOOP-13 says
			// before, and REQ-LOOP-13 wins (ruling P-18). Under §5's order a
			// steering message is invisible to the compaction that just built
			// the view it will be appended to.
			a.mu.Lock()
			drained := a.drainSteeringLocked()
			a.mu.Unlock()
			delivered := len(drained)
			record(drained...)

			// ---- Phase 2: PrepareNextTurn at the head of THIS iteration,
			// immediately before the request it prepares (REQ-LOOP-04b,
			// NFR-REL-05). Not after TurnEnd: there it would fire for a turn
			// that will not happen, and would miss the request issued after a
			// tool batch within the same user turn.
			view := a.prepareNextTurn(ctx)

			// ---- Phase 3: second poll. PrepareNextTurn may be long-running
			// (a summarization round trip), so a message that arrived during
			// it would otherwise wait a whole turn.
			//
			// The guard is "nothing already delivered into THIS turn", not
			// REQ-LOOP-13's literal `len(pending)==0`, which read literally
			// can never yield anything — pending is empty exactly when the
			// first drain took everything (ruling P-17).
			if delivered == 0 {
				a.mu.Lock()
				more := a.drainSteeringLocked()
				a.mu.Unlock()
				if len(more) > 0 {
					record(more...)
					view = append(view.Clone(), more...)
				}
			}

			// ---- Phase 4: call the provider.
			a.setPhase(core.PhaseCallingModel)
			s.Push(core.TurnStartEvent{TurnIndex: turnCount})
			a.fireTurnStart(core.TurnStartEvent{TurnIndex: turnCount})

			assistant := a.callModel(ctx, s, view)
			record(assistant)
			a.addUsage(assistant.Usage)

			// ---- Phase 5: only Error and Aborted short-circuit, and they do
			// so BEFORE tool extraction (REQ-LOOP-01). Every other reason —
			// Stop, StopSequence, Length, anything unrecognized — is treated
			// identically for control flow.
			if assistant.StopReason.ShortCircuits() {
				a.setPhase(core.PhaseBetweenTurns)
				turnCount++
				s.Push(core.TurnEndEvent{TurnIndex: turnCount - 1, Message: assistant, ToolResults: []core.ToolResultMessage{}, Usage: assistant.Usage})
				a.fireTurnEnd(core.TurnEndEvent{TurnIndex: turnCount - 1, Message: assistant, ToolResults: []core.ToolResultMessage{}, Usage: assistant.Usage})
				if assistant.StopReason == core.StopReasonAborted {
					runReason, runErr = core.RunStopAborted, core.ErrAborted
				} else {
					runReason = core.RunStopError
					runErr = errors.New(assistant.ErrorMessage)
				}
				break outer
			}

			// A deferred response has no content and no tool calls, so under
			// REQ-LOOP-01 it would exit the inner loop as "an empty
			// completion" — the exact failure OQ-11 warns about. v1 has no
			// poller, so say so instead of returning nothing (ruling P-50).
			if assistant.StopReason == core.StopReasonDeferred {
				a.setPhase(core.PhaseBetweenTurns)
				turnCount++
				runReason, runErr = core.RunStopError, core.ErrDeferredUnsupported
				break outer
			}

			// ---- Phase 6: the continuation predicate. PRESENCE of tool_use
			// blocks, never stop_reason (REQ-LOOP-01). Gemini and several
			// OpenAI-compatible gateways return a STOP-family finish reason
			// alongside tool calls; a stop-reason gate drops them silently and
			// passes every Anthropic-only test.
			toolCalls := core.ExtractToolUse(&assistant)

			var (
				results   []core.ToolResultMessage
				terminate bool
			)
			if len(toolCalls) > 0 {
				if assistant.StopReason == core.StopReasonLength {
					// REQ-LOOP-10. Reading stop_reason HERE is not a violation
					// of REQ-LOOP-01: the prohibition is scoped to the
					// continuation predicate, which already ran above (ruling
					// P-19). This is a different decision.
					//
					// Execute NONE of them. Streamed arguments are finalized
					// by a best-effort salvage parser, so a truncated
					// {"path":"/et becomes a syntactically valid object that
					// passes schema validation — and a truncated edit whose
					// new_string was cut off applies cleanly and silently
					// corrupts the file. Only the stop reason can catch this.
					results = a.synthesizeTruncated(s, toolCalls)
				} else {
					a.setPhase(core.PhaseExecutingTools)
					results, terminate = a.executeBatch(ctx, s, &assistant, toolCalls, turnCount)
				}
				for _, r := range results {
					record(r)
					if r.Usage != nil {
						a.addUsage(*r.Usage)
					}
				}
			}

			a.setPhase(core.PhaseBetweenTurns)
			turnCount++
			te := core.TurnEndEvent{TurnIndex: turnCount - 1, Message: assistant, ToolResults: results, Usage: assistant.Usage}
			s.Push(te)
			a.fireTurnEnd(te)

			// ---- Phase 7: termination votes, then the stop predicate.
			//
			// Terminate is checked first and wins, so a `finish` tool reports
			// "tool_terminate" rather than being masked by a turn limit that
			// happens to fire on the same turn (ruling P-35).
			if terminate {
				runReason = core.RunStopToolTerminate
				break outer
			}

			// The stop check runs AFTER the completed turn's tools have
			// executed and their results are in history (REQ-LOOP-04a). A
			// limit checked between extraction and execution ends the
			// transcript with dangling tool_use blocks that no provider
			// accepts on resume.
			if stop, reason := a.consultStopPolicy(assistant, results, newMessages, turnCount, startedAt); stop {
				runReason = reason
				if a.cfg.ErrorOnLimit {
					switch reason {
					case core.RunStopMaxTurns:
						runErr = core.ErrMaxTurns
					case core.RunStopBudgetExceeded:
						runErr = core.ErrBudgetExceeded
					}
				}
				break outer
			}

			// ---- Phase 8: inner-loop condition. Pending steering keeps the
			// inner loop alive even when the assistant produced no tool calls
			// (REQ-LOOP-13): the condition is hasMoreToolCalls || pending > 0.
			a.mu.Lock()
			morePending := len(a.steering) > 0
			a.mu.Unlock()
			if len(toolCalls) == 0 && !morePending {
				break inner
			}
		}

		// ---- Outer: the follow-up queue is polled only once the inner loop
		// is exhausted (REQ-LOOP-14).
		a.mu.Lock()
		fu := a.drainFollowUpLocked()
		a.mu.Unlock()
		if len(fu) == 0 {
			break outer
		}
		record(fu...)
	}

	a.setPhase(core.PhaseIdle)
	res := core.RunResult{
		Messages:   newMessages,
		StopReason: runReason,
		Usage:      a.Usage(),
		TurnCount:  turnCount,
		Error:      runErr,
	}
	if am := lastAssistant(newMessages); am != nil {
		res.LastReason = am.StopReason
	}
	done := core.AgentDoneEvent{Result: res, Usage: res.Usage}
	s.Push(done)
	a.fireAgentDone(done)
	if runErr != nil {
		a.fireError(runErr)
	}
	return res, runErr
}

// callModel issues one provider request through the middleware chain and
// returns the assistant message. It never returns an error: failures are
// encoded in the message (REQ-PROV-04), which is what lets a provider emit
// half a message and then fail without the partial content being lost.
func (a *Agent) callModel(ctx context.Context, out *core.EventStream, view core.Messages) core.AssistantMessage {
	a.mu.Lock()
	cfg := a.cfg
	tools := ResolveToolPolicy(a.tools, cfg.ToolPolicy)
	a.mu.Unlock()

	req := core.Request{
		Messages:         view,
		Tools:            core.ToolWires(tools),
		ToolChoice:       cfg.ToolChoice,
		MaxTokens:        cfg.MaxTokens,
		Temperature:      cfg.Temperature,
		TopP:             cfg.TopP,
		ThinkingLevel:    cfg.ThinkingLevel,
		EstContextTokens: EstimateContextTokens(view, checkpointOf(a.history)),
		Options:          cfg.RequestOptions,
	}
	if cfg.SystemPrompt != "" {
		req.System = []core.ContentBlock{core.TextBlock{Text: cfg.SystemPrompt}}
	}

	registry := cfg.Providers
	if registry == nil {
		registry = DefaultProviders()
	}
	base := core.Handler(func(ctx context.Context, r core.Request) *core.EventStream {
		return registry.Dispatch(ctx, cfg.Model, r, core.ProviderStreamOptions{
			CacheRetention: cfg.CacheRetention,
		})
	})
	h := core.Chain(base, cfg.Middleware...)

	ps := h(ctx, req)
	// Forward provider events onto the agent stream. The provider stream is
	// unbounded and non-blocking, so this cannot stall the model call.
	for e := range ps.Events() {
		out.Push(e)
	}
	msg := ps.Result()
	if msg == nil {
		// A provider that ended without a message is a provider bug; produce
		// the terminal marker rather than a nil deref, so the transcript still
		// has the "a turn that started always has a terminal message"
		// property REQ-LOOP-09 depends on.
		err := ps.Err()
		if err == nil {
			err = errors.New("provider ended the stream with no assistant message")
		}
		return core.AssistantMessage{
			StopReason:   core.StopReasonError,
			ErrorMessage: err.Error(),
			Timestamp:    time.Now(),
			Provider:     a.cfg.Model.Provider,
			API:          a.cfg.Model.API,
			Model:        a.cfg.Model.ID,
		}
	}
	// An abort that lands during a retry backoff normalizes to an aborted
	// message with the error message cleared (REQ-LOOP-09a).
	if a.wasAborted() && msg.StopReason == core.StopReasonAborted {
		msg.ErrorMessage = ""
	}
	return *msg
}

// synthesizeTruncated implements REQ-LOOP-10: no handler runs, but every call
// still emits the normal event sequence and produces a result, so the
// transcript and the UI stay well-formed and the model can re-issue.
func (a *Agent) synthesizeTruncated(s *core.EventStream, calls []core.ToolUseBlock) []core.ToolResultMessage {
	out := make([]core.ToolResultMessage, 0, len(calls))
	for i, c := range calls {
		s.Push(core.ToolCallStartEvent{BlockIndex: i, ToolUseID: c.ID, Name: c.Name})
		s.Push(core.ToolCallEndEvent{BlockIndex: i, Block: c})
		s.Push(core.ToolExecutionStartEvent{ToolUseID: c.ID, Name: c.Name})

		text := fmt.Sprintf(maxTokensToolText, c.Name)
		m := core.ToolResultMessage{
			ToolUseID: c.ID,
			ToolName:  c.Name,
			Content:   core.Content{core.TextBlock{Text: text}},
			IsError:   true,
			Timestamp: time.Now(),
		}
		s.Push(core.ToolExecutionEndEvent{ToolUseID: c.ID, Name: c.Name, IsError: true})
		s.Push(core.ToolResultEvent{Message: m})
		out = append(out, m)
	}
	return out
}

// prepareNextTurn applies the context transform (REQ-GO-12). It produces the
// message list sent on THIS request and never rewrites stored history, so the
// append-only transcript stays complete and a later run against a larger
// context window can be given the full history.
func (a *Agent) prepareNextTurn(ctx context.Context) core.Messages {
	msgs := a.history.Messages()
	a.mu.Lock()
	tf := a.cfg.TransformContext
	a.mu.Unlock()
	if tf == nil {
		return msgs
	}
	prev := a.Phase()
	a.setPhase(core.PhaseCompacting)
	defer a.setPhase(prev)
	return tf(ctx, msgs)
}

func checkpointOf(h *core.ConversationHistory) *core.CompactionCheckpoint {
	if cp, ok := h.Checkpoint(); ok {
		return &cp
	}
	return nil
}

// consultStopPolicy runs the single post-turn predicate (REQ-LOOP-04).
func (a *Agent) consultStopPolicy(m core.AssistantMessage, results []core.ToolResultMessage, newMessages core.Messages, turnCount int, startedAt time.Time) (bool, core.RunStopReason) {
	a.mu.Lock()
	p := a.cfg.StopPolicy
	a.mu.Unlock()
	if p == nil {
		return false, core.RunStopEndTurn
	}
	reason := core.RunStopPolicy
	sc := core.StopContext{
		Message:     &m,
		ToolResults: results,
		History:     a.history,
		NewMessages: newMessages,
		TurnCount:   turnCount,
		Usage:       a.Usage(),
		Reason:      &reason,
		StartedAt:   startedAt,
	}
	if p(sc) {
		return true, reason
	}
	return false, core.RunStopEndTurn
}

func (a *Agent) addUsage(u core.Usage) {
	a.mu.Lock()
	a.usage = a.usage.Add(u)
	a.mu.Unlock()
}

// entryFor is the session-log id a message is recorded under. v1 records
// messages into history without a store-assigned id when no store is attached.
func (a *Agent) entryFor(core.Message) core.EntryID { return core.NullLeaf }

// ------------------------------------------------------------------- hooks
//
// Hooks are Axis 2: observation, never interception (REQ-OBS-07). Each is
// invoked through a recovering wrapper and with NO agent lock held
// (NFR-REL-02): a panicking listener inside a held lock is a deadlock, not a
// crash, so it produces no stack trace and no error.

func (a *Agent) hooks() core.Hooks {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Hooks
}

func safely(onErr func(error), what string, f func()) {
	if f == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("agentkit: panic in %s: %v", what, r)
			if onErr != nil {
				// The error reporter is user code too; a panic in it must not
				// unwind the loop.
				func() {
					defer func() { _ = recover() }()
					onErr(err)
				}()
			}
		}
	}()
	f()
}

func (a *Agent) fireTurnStart(e core.TurnStartEvent) {
	h := a.hooks()
	safely(h.OnError, "OnTurnStart", func() {
		if h.OnTurnStart != nil {
			h.OnTurnStart(e)
		}
	})
}

func (a *Agent) fireTurnEnd(e core.TurnEndEvent) {
	h := a.hooks()
	safely(h.OnError, "OnTurnEnd", func() {
		if h.OnTurnEnd != nil {
			h.OnTurnEnd(e)
		}
	})
}

func (a *Agent) fireAgentDone(e core.AgentDoneEvent) {
	h := a.hooks()
	safely(h.OnError, "OnAgentDone", func() {
		if h.OnAgentDone != nil {
			h.OnAgentDone(e)
		}
	})
}

func (a *Agent) fireError(err error) {
	h := a.hooks()
	safely(nil, "OnError", func() {
		if h.OnError != nil {
			h.OnError(err)
		}
	})
}
