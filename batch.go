package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// executeBatch runs one turn's tool calls (REQ-LOOP-05). It is three phases
// with distinct concurrency rules, and each phase is where it is because the
// natural Go answer is wrong:
//
//  1. Prepare  — strictly SEQUENTIAL, on the loop goroutine. Events,
//     argument preparation, validation and the authorization
//     interceptor. A blocked call is finalized inline here, so the
//     policy observes a deterministic order even though the
//     handlers race (REQ-SEC-03.4).
//  2. Execute  — parallel. ONLY the handler body runs outside the lock. One
//     goroutine per call, joined by sync.WaitGroup — never
//     errgroup, which returns the first error and cancels the
//     siblings, when every call in a batch needs a result
//     (REQ-GO-04).
//  3. Finalize — serialized under ONE BATCH-SCOPED mutex with a func-scoped
//     deferred unlock, so a panicking interceptor or event
//     listener cannot leak it and deadlock the remaining tool
//     goroutines at the join.
//
// Results are written by SLOT INDEX into a pre-sized slice and appended in
// slot order after the join, so transcript order is independent of completion
// order (REQ-LOOP-05).
//
// Returns the results and the REQ-TOOL-13 termination vote.
func (a *Agent) executeBatch(ctx context.Context, s *core.EventStream, assistant *core.AssistantMessage, calls []core.ToolUseBlock, turnCount int) ([]core.ToolResultMessage, bool) {
	a.mu.Lock()
	cfg := a.cfg
	tools := ResolveToolPolicy(a.tools, cfg.ToolPolicy)
	a.mu.Unlock()

	// Read ONCE, here, alongside cfg. Reaching back through a.mu from inside a
	// thunk would put lock traffic on the per-tool path of a parallel batch —
	// the one path NFR-PERF-04 asks to be genuinely concurrent — and would
	// acquire the agent lock from a goroutine the executor does not own.
	tracer := cfg.Tracer
	if tracer == nil {
		tracer = core.NoopTracer
	}
	auditSession := cfg.SessionID

	byName := make(map[string]core.Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}

	n := len(calls)
	results := make([]core.ToolResultMessage, n)
	votes := make([]bool, n)
	thunks := make([]func(), 0, n)

	// batchMu is BATCH-SCOPED: created here, acquired by nothing else, and
	// a.mu is never acquired while it is held.
	//
	// This is the reconciliation of REQ-LOOP-05 phase 3 ("under one mutex")
	// with NFR-REL-02.1 ("no interceptor may run while an agent lock is
	// held"), which contradict each other on a literal reading (ruling P-6).
	// The natural reading — reuse the agent mutex — IS the deadlock
	// NFR-REL-02.1 exists to prevent.
	var batchMu sync.Mutex

	// ---- Phase 1: prepare, strictly sequential.
	for i, c := range calls {
		i, c := i, c
		// No ToolCallStart/ToolCallEnd here. Those describe the MODEL emitting
		// a tool call and are the provider's to push as it streams; the loop
		// owns the EXECUTION triple (ToolExecutionStart/Update/End) and the
		// finalized ToolResultEvent. REQ-LOOP-05 names the call events and
		// REQ-OBS-06 names the execution ones — emitting both duplicates every
		// call in any UI driven by the stream (ruling C19).

		tool, known := byName[c.Name]
		if !known {
			results[i] = errorResult(c, "unknown_tool",
				fmt.Sprintf("no tool named %q is available in this run", c.Name))
			s.Push(core.ToolResultEvent{Message: results[i]})
			continue
		}

		prepared, perr := PrepareArguments(tool, c)
		if perr != nil {
			// The error text re-serializes the model's OWN key order, so the
			// message is self-correcting (REQ-TOOL-11.4, REQ-TOOL-12.3).
			results[i] = errorResult(c, "invalid_arguments", perr.Error())
			s.Push(core.ToolResultEvent{Message: results[i]})
			continue
		}

		if cfg.BeforeToolCall != nil {
			dec := a.callBefore(ctx, cfg.BeforeToolCall, core.BeforeToolCallContext{
				ToolName:  c.Name,
				ToolUseID: c.ID,
				Tool:      tool,
				Arguments: prepared.Args,
				RawInput:  c.Input,
				Assistant: assistant,
				Batch:     calls,
				Index:     i,
				TurnCount: turnCount,
			})
			if dec.Block {
				reason := dec.Reason
				if reason == "" {
					reason = "blocked by policy"
				}
				results[i] = errorResult(c, core.BlockErrorCode, reason)
				// A blocked call casts the same termination vote, which is
				// what lets a permission denial end the run instead of looping
				// the model into retrying (REQ-TOOL-13.2). Honoured only when
				// Block is set.
				votes[i] = dec.Terminate
				s.Push(core.ToolResultEvent{Message: results[i]})
				continue
			}
			if dec.Arguments != nil {
				// The interceptor may widen as well as narrow (REQ-SEC-03.5).
				prepared = prepared.WithArgs(dec.Arguments)
			}
		}

		thunks = append(thunks, func() {
			start := time.Now()
			s.Push(core.ToolExecutionStartEvent{ToolUseID: c.ID, Name: c.Name})

			// REQ-OBS-02: a span around the HANDLER, wrapping only the part
			// that does work. Wrapping the finalize block as well would put
			// every peer's span duration inside every other peer's, because
			// finalization is serialized under the batch mutex — so a parallel
			// batch would trace as though it were sequential.
			var out core.ToolResult
			_ = tracer.StartSpan("agentkit.tool_call", func(sp core.Span) error {
				defer sp.End()
				out = invokeHandler(ctx, tool, prepared)
				sp.SetAttributes(map[string]any{
					"tool_name":   c.Name,
					"tool_use_id": c.ID,
					"is_error":    !out.OK,
					"elapsed_ms":  time.Since(start).Milliseconds(),
				})
				if !out.OK {
					sp.SetStatus(errors.New(out.Error))
				}
				return nil
			})
			a.audit(core.AuditEvent{
				Kind: core.AuditToolCall, SessionID: auditSession,
				ToolName: c.Name, ToolUseID: c.ID,
				ServerName:    MCPServerOf(c.Name),
				ArgumentsHash: ArgumentsHash(prepared.Raw),
				IsError:       !out.OK,
				ElapsedMS:     time.Since(start).Milliseconds(),
			})

			// ---- Phase 3, per call: finalize under the batch mutex.
			// A func-scoped critical section with a DEFERRED unlock: a
			// panicking AfterToolCall or event listener must not leak the
			// mutex and hang every peer at the join.
			func() {
				batchMu.Lock()
				defer batchMu.Unlock()

				msg := toolResultMessage(c, out)
				if cfg.AfterToolCall != nil {
					dec := a.callAfter(ctx, cfg.AfterToolCall, core.AfterToolCallContext{
						ToolName:  c.Name,
						ToolUseID: c.ID,
						Arguments: prepared.Args,
						Result:    &msg,
						Elapsed:   time.Since(start),
					})
					// Terminate is *bool: nil means "no opinion", so an
					// interceptor that does not care cannot accidentally vote
					// against a tool that does (REQ-TOOL-13.3).
					if dec.Terminate != nil {
						out.Terminate = *dec.Terminate
					}
				}
				results[i] = msg
				votes[i] = out.Terminate
				s.Push(core.ToolExecutionEndEvent{
					ToolUseID: c.ID, Name: c.Name, IsError: msg.IsError,
					ElapsedMS: time.Since(start).Milliseconds(),
				})
				s.Push(core.ToolResultEvent{Message: msg})
			}()
		})
	}

	// ---- Phase 2: the abort decision, made ONCE, here, on the loop
	// goroutine, after prepare and BEFORE any handler starts (REQ-LOOP-11).
	//
	// It must not be re-checked inside each tool goroutine. Per-goroutine
	// ctx.Err() checks — the obvious Go idiom, and what REQ-GO-05 alone
	// implies — let the scheduler SPLIT the batch: an abort landing just after
	// the batch starts skips whichever calls had not yet been scheduled and
	// runs the rest. That is nondeterministic, unreproducible in tests, and
	// shows up in production as phantom side effects after the user pressed
	// Ctrl-C.
	//
	// The handlers still RECEIVE ctx, so a running subprocess is still killed
	// (REQ-TOOL-17.2). What must not be re-decided is *whether a handler runs*
	// (ruling P-20).
	if ctx.Err() != nil {
		for i, c := range calls {
			if results[i].ToolUseID != "" {
				continue // already finalized in prepare (blocked/invalid)
			}
			results[i] = abortedResult(c)
			s.Push(core.ToolExecutionEndEvent{ToolUseID: c.ID, Name: c.Name, IsError: true})
			s.Push(core.ToolResultEvent{Message: results[i]})
		}
		return results, false
	}

	sequential := !cfg.ParallelTools || len(thunks) <= 1
	if !sequential {
		// A single Sequential tool anywhere in the batch demotes the WHOLE
		// batch (REQ-LOOP-05a). Tools with process-wide or workspace-wide
		// side effects ship as Sequential.
		for _, c := range calls {
			if t, ok := byName[c.Name]; ok && t.ExecutionMode == core.Sequential {
				sequential = true
				break
			}
		}
	}

	if sequential {
		for _, th := range thunks {
			th()
		}
	} else {
		var wg sync.WaitGroup
		for _, th := range thunks {
			wg.Add(1)
			go func(f func()) {
				defer wg.Done()
				f()
			}(th)
		}
		wg.Wait()
	}

	// Every call in the batch produced a result — a handler error, a panic, an
	// interceptor block, a validation failure and an abort all become a result
	// with IsError set (REQ-LOOP-05b). A batch returning fewer results than
	// calls leaves dangling tool_use blocks and makes the next request
	// invalid, so this is asserted rather than assumed.
	for i, c := range calls {
		if results[i].ToolUseID == "" {
			results[i] = errorResult(c, "no_result",
				"internal: the tool batch produced no result for this call")
		}
	}
	return results, core.BatchTerminates(votes)
}

// invokeHandler calls the tool, converting every failure mode into a result.
// No tool outcome is ever propagated to the caller as a Go error (REQ-GO-04).
func invokeHandler(ctx context.Context, t core.Tool, p Prepared) (res core.ToolResult) {
	defer func() {
		if r := recover(); r != nil {
			// A handler panic becomes a tool result and the loop continues
			// (NFR-REL-02.2). It must never crash the agent process.
			res = core.ErrResult("panic", fmt.Sprintf("tool %q panicked: %v", t.Name, r))
		}
	}()

	if t.Execute != nil {
		return t.Execute(ctx, p.Raw)
	}
	out, err := t.Handler(ctx, p.Raw)
	if err != nil {
		return core.ErrResult("handler_error", err.Error())
	}
	var data map[string]any
	if len(out) > 0 {
		if err := json.Unmarshal(out, &data); err != nil {
			// A handler may legitimately return a non-object; carry it
			// through rather than failing the call.
			return core.ToolResult{OK: true, Data: map[string]any{"result": json.RawMessage(out)}}
		}
	}
	return core.OKResult(data)
}

// callBefore invokes the authorization interceptor through a recovering
// wrapper, with NO agent lock held (NFR-REL-02). A panicking interceptor fails
// CLOSED — it blocks the call — because a security boundary that opens on
// panic is not a boundary.
func (a *Agent) callBefore(ctx context.Context, f core.BeforeToolCall, in core.BeforeToolCallContext) (dec core.BeforeToolCallDecision) {
	defer func() {
		if r := recover(); r != nil {
			dec = core.BeforeToolCallDecision{
				Block:  true,
				Reason: fmt.Sprintf("interceptor panicked: %v", r),
			}
			a.fireError(fmt.Errorf("agentkit: panic in BeforeToolCall for %q: %v", in.ToolName, r))
		}
	}()
	return f(ctx, in)
}

// callAfter invokes the after-interceptor through a recovering wrapper. A
// panic here yields "no opinion" rather than a vote: unlike BeforeToolCall
// this is not a security boundary, and inventing a termination vote from a
// crash would end the run for the wrong reason.
func (a *Agent) callAfter(ctx context.Context, f core.AfterToolCall, in core.AfterToolCallContext) (dec core.AfterToolCallDecision) {
	defer func() {
		if r := recover(); r != nil {
			dec = core.AfterToolCallDecision{}
			a.fireError(fmt.Errorf("agentkit: panic in AfterToolCall for %q: %v", in.ToolName, r))
		}
	}()
	return f(ctx, in)
}

func toolResultMessage(c core.ToolUseBlock, r core.ToolResult) core.ToolResultMessage {
	payload, err := json.Marshal(r.ToLLMMap())
	if err != nil {
		payload = []byte(`{"ok":false,"error":"marshal_failed"}`)
	}
	return core.ToolResultMessage{
		ToolUseID: c.ID,
		ToolName:  c.Name,
		Content:   core.Content{core.TextBlock{Text: string(payload)}},
		IsError:   !r.OK,
		Timestamp: time.Now(),
	}
}

func errorResult(c core.ToolUseBlock, code, detail string) core.ToolResultMessage {
	return toolResultMessage(c, core.ErrResult(code, detail))
}

// abortedResult is the fixed shape of a call that did not run because the
// batch was aborted. It still emits events and a result so the transcript
// stays well-formed and resumable (REQ-LOOP-11.3).
func abortedResult(c core.ToolUseBlock) core.ToolResultMessage {
	m := toolResultMessage(c, core.ErrResult("aborted", "Operation aborted"))
	return m
}
