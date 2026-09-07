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
	// OQ-8: a shell tool with no interceptor fails here, before anything is
	// recorded, so the omission is a returned error on the first Run and not
	// an unrestricted shell discovered from a bill.
	if err := a.checkExecuteGuard(); err != nil {
		return nil, err
	}
	rctx, cancel, pending, err := a.claimSlot(ctx)
	if err != nil {
		return nil, err
	}

	if isContinue {
		// REQ-LOOP-16, assistant branch: "drain the steering AND follow-up
		// queues and run with those". The slot claim drained steering; a
		// follow-up is normally polled only once the inner loop is exhausted
		// (REQ-LOOP-14), but here there is no inner loop to exhaust — the
		// last turn already completed — so a follow-up alone would otherwise
		// send the assistant-terminated transcript to the model first and be
		// delivered a turn late.
		if len(pending) == 0 && a.effectiveLastRole() == core.RoleAssistant {
			a.mu.Lock()
			pending = a.drainFollowUpLocked()
			a.mu.Unlock()
		}
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
	if _, ok := a.history.LastRole(); !ok {
		return fmt.Errorf("%w: history is empty", core.ErrNotContinuable)
	}
	role := a.effectiveLastRole()
	if role == "" {
		return fmt.Errorf("%w: history holds only a terminal marker", core.ErrNotContinuable)
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

// effectiveLastRole is the role of the last message THE MODEL WILL SEE. A
// trailing aborted or errored assistant message is the REQ-LOOP-09 terminal
// marker, not a completed turn: REQ-PROV-11 rule 2 drops it from every
// outbound request, so the model still owes a reply to whatever preceded it.
// The REQ-LOOP-16 precondition is therefore evaluated past those markers
// (ruling: "a completed assistant turn" means one that completed). Empty
// history, or history holding only markers, yields "".
func (a *Agent) effectiveLastRole() core.Role {
	msgs := a.history.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if am, ok := msgs[i].(core.AssistantMessage); ok && am.StopReason.ShortCircuits() {
			continue
		}
		return msgs[i].Role()
	}
	return ""
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
func (a *Agent) runLoop(ctx context.Context, s *core.EventStream, initial *core.UserMessage, pending []core.Message) (res core.RunResult, runErr error) {
	startedAt := time.Now()
	var (
		newMessages core.Messages
		turnCount   int
		runReason   = core.RunStopEndTurn
	)

	// The config is read under the lock ONCE here for the run boundary
	// events. SetModel writes cfg.Model under a.mu from any goroutine
	// (REQ-SESS-03), so an unlocked read races it — the detector flags it,
	// and the value observed could be half of a model change.
	a.mu.Lock()
	startCfg := a.cfg
	a.mu.Unlock()

	// finish is the ONE exit path: it builds the RunResult and fires the
	// terminal events and the session-end audit. It exists as a closure so
	// the panic recovery below reaches the same tail as a normal exit — a
	// run that panicked still owes its AgentDoneEvent and its session-end
	// audit, or an auditor cannot tell it from one still running.
	finish := func() {
		a.setPhase(core.PhaseIdle)
		res = core.RunResult{
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

		// REQ-OBS-03's session end fires on EVERY exit — clean, errored or
		// aborted. A hook that fires only on the happy path is worse than
		// none: an auditor cannot then tell a session that ended badly from
		// one still running, which is the case they most need to see.
		end := core.AuditEvent{
			Kind: core.AuditSessionEnd, Usage: res.Usage, StopReason: res.StopReason,
		}
		if runErr != nil {
			end.Error = runErr.Error()
		}
		a.audit(end)

		if runErr != nil {
			a.fireError(runErr)
		}
	}

	// NFR-REL-02: a panic in code the loop calls — middleware, a stop
	// policy, a context transform, a per-tool argument shim, a tracer — must
	// never crash the agent process. Each of those is also wrapped at its own
	// call site; this is the backstop for whatever is not, and for the loop's
	// own bugs. It leaves the terminal marker REQ-LOOP-09 requires: a turn
	// that started always has a terminal message, and REQ-PROV-11 drops it
	// from the next request so the transcript stays sendable.
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		runErr = fmt.Errorf("agentkit: panic in run: %v", r)
		runReason = core.RunStopError
		needMarker := true
		if n := len(newMessages); n > 0 {
			if am, ok := newMessages[n-1].(core.AssistantMessage); ok && am.StopReason.ShortCircuits() {
				needMarker = false
			}
		}
		if needMarker {
			marker := core.AssistantMessage{
				StopReason:   core.StopReasonError,
				ErrorMessage: runErr.Error(),
				Timestamp:    time.Now(),
				Provider:     startCfg.Model.Provider,
				API:          startCfg.Model.API,
				Model:        startCfg.Model.ID,
			}
			if _, err := a.rec.RecordMessage(marker); err != nil {
				a.fireError(fmt.Errorf("agentkit: persisting message: %w", err))
			}
			newMessages = append(newMessages, marker)
		}
		finish()
	}()

	// record is the ONLY path a message enters the run by. It goes through the
	// recorder, so history and the durable log advance together and cannot
	// drift: a message in history that never reached the log is exactly the
	// state that makes a resumed session lose the last turn (REQ-SESS-01).
	record := func(msgs ...core.Message) {
		for _, m := range msgs {
			if _, err := a.rec.RecordMessage(m); err != nil {
				// Persistence failures are surfaced, never swallowed
				// (REQ-SESS-08), but they do not abort the run: losing the
				// log is bad, losing the turn in flight is worse.
				a.fireError(fmt.Errorf("agentkit: persisting message: %w", err))
			}
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

	s.Push(core.AgentStartEvent{SessionID: startCfg.SessionID, Provider: startCfg.Model.Provider, API: startCfg.Model.API, Model: startCfg.Model.ID})
	a.audit(core.AuditEvent{Kind: core.AuditSessionStart, Timestamp: startedAt})

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
					runReason, runErr = core.RunStopAborted, a.abortError(ctx)
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
				// A turn that started owes its TurnEndEvent (REQ-OBS-06):
				// a consumer that opened a turn on TurnStart must be able
				// to close it without inferring the boundary from
				// AgentDone.
				te := core.TurnEndEvent{TurnIndex: turnCount - 1, Message: assistant, ToolResults: []core.ToolResultMessage{}, Usage: assistant.Usage}
				s.Push(te)
				a.fireTurnEnd(te)
				runReason, runErr = core.RunStopError, core.ErrDeferredUnsupported
				break outer
			}

			// ---- Phase 6: the continuation predicate. PRESENCE of tool_use
			// blocks, never stop_reason (REQ-LOOP-01). Gemini and several
			// OpenAI-compatible gateways return a STOP-family finish reason
			// alongside tool calls; a stop-reason gate drops them silently and
			// passes every Anthropic-only test.
			toolCalls := core.ExtractToolUse(&assistant)

			// Non-nil even for a no-tool turn: REQ-OBS-06 promises "always
			// non-nil; [] for a no-tool turn", and the promise has to hold
			// for the OnTurnEnd hook and StopContext too, not only for the
			// stream copy that clone() happens to rebuild.
			var (
				results   = []core.ToolResultMessage{}
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

			// A cancellation that landed during the tool batch ends the run
			// HERE, at the turn boundary, with the results in history. Going
			// around again would issue a provider request on a dead context,
			// which resolves instantly to a content-free aborted assistant
			// message — a turn that never happened, recorded as though it
			// did. The transcript this leaves ends in a tool_result, which
			// REQ-LOOP-16 names as the normal outcome of REQ-LOOP-09
			// cancellation and the reason Continue exists.
			if ctx.Err() != nil {
				runReason, runErr = core.RunStopAborted, a.abortError(ctx)
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

	finish()
	return res, runErr
}

// abortError names WHO stopped the run (REQ-GO-09): ErrAborted for
// Agent.Abort(), and the caller's own ctx error — context.Canceled or
// context.DeadlineExceeded — when it was their context. The two are
// deliberately distinguishable: a UI that called Abort expects ErrAborted and
// a server whose request deadline expired expects DeadlineExceeded, and
// collapsing them makes a timeout read as a user action. When neither applies
// (a provider normalized a backoff-time cancellation into an aborted message
// with the run's context still live) the generic sentinel stands.
func (a *Agent) abortError(ctx context.Context) error {
	if a.wasAborted() {
		return core.ErrAborted
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return core.ErrAborted
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
	// The assembled prompt, not the raw field: per-tool guidelines
	// (NFR-TEST-08a) and REQ-TOOL-04e's conditional guideline are only visible
	// to the model if something assembles them, and the tool set they describe
	// is the RESOLVED one just computed above.
	if sys := BuildSystemPrompt(PromptInput{
		Custom: cfg.SystemPrompt, Tools: tools, ExtraBlocks: cfg.PromptBlocks,
	}); sys != "" {
		req.System = []core.ContentBlock{core.TextBlock{Text: sys}}
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

	// Middleware is third-party code executing inside the loop (NFR-REL-02).
	// A panic in it becomes the error message this function would return for
	// any other provider failure, so the transcript keeps the terminal marker
	// of REQ-LOOP-09 and the run keeps going through the same exit as an HTTP
	// 500 would.
	var ps *core.EventStream
	func() {
		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("agentkit: panic in middleware: %v", r)
				a.fireError(err)
				ps = core.ErrorStream(&core.AssistantMessage{
					StopReason:   core.StopReasonError,
					ErrorMessage: err.Error(),
					Timestamp:    time.Now(),
					Provider:     cfg.Model.Provider,
					API:          cfg.Model.API,
					Model:        cfg.Model.ID,
				}, err)
			}
		}()
		ps = h(ctx, req)
	}()
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
			Provider:     cfg.Model.Provider,
			API:          cfg.Model.API,
			Model:        cfg.Model.ID,
		}
	}
	// The aborted message is returned AS THE PROVIDER PRODUCED IT. REQ-LOOP-09
	// requires error_message set on an aborted turn and forbids rewriting the
	// message at abort time; REQ-LOOP-09a's "error message cleared" applies
	// only to a cancellation landing during a retry BACKOFF, which
	// RetryMiddleware normalizes itself. Clearing it here for every abort
	// erased the diagnostic the transcript is supposed to carry — and did it
	// by writing through the provider stream's own message.
	return *msg
}

// synthesizeTruncated implements REQ-LOOP-10: no handler runs, but every call
// still emits the normal event sequence and produces a result, so the
// transcript and the UI stay well-formed and the model can re-issue.
func (a *Agent) synthesizeTruncated(s *core.EventStream, calls []core.ToolUseBlock) []core.ToolResultMessage {
	out := make([]core.ToolResultMessage, 0, len(calls))
	for _, c := range calls {
		// The execution triple only; the provider already emitted the call
		// events as the model streamed them (ruling C19).
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

	// A panicking transform is contained (NFR-REL-02): the request goes out
	// over the untransformed view, which is always a valid one, and the
	// panic is surfaced through OnError. Compaction failing must never abort
	// the session (NFR-REL-05), and a panic is the loudest way to fail.
	view := msgs
	safely(a.hooks().OnError, "TransformContext", func() {
		if out := tf(ctx, msgs); out != nil {
			view = out
		}
	})
	return view
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
	// A panicking stop policy STOPS the run (ruling: a limit that fails open
	// is an unbounded bill). NFR-REL-02.2 says a hook or middleware panic
	// must not abort the run, and this is neither: the policy is the caller's
	// own limit, and ending at a turn boundary with every result in history
	// is a clean stop, not the dirty REQ-LOOP-09 marker. The panic is
	// surfaced through OnError so the caller can see their policy is broken.
	stop, panicked := false, false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				a.fireError(fmt.Errorf("agentkit: panic in StopPolicy: %v", r))
			}
		}()
		stop = p(sc)
	}()
	if panicked {
		return true, core.RunStopPolicy
	}
	if stop {
		return true, reason
	}
	return false, core.RunStopEndTurn
}

func (a *Agent) addUsage(u core.Usage) {
	a.mu.Lock()
	a.usage = a.usage.Add(u)
	model := a.cfg.Model
	a.mu.Unlock()
	// Level 1 accounting is folded from the SAME reported usage the turn was
	// billed against (REQ-CACHE-08). Recomputing it from a token estimate here
	// would make the savings figure disagree with the invoice.
	a.meter.ObserveTurn(model, u)
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
