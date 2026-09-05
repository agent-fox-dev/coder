package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
)

// ---------------------------------------------------------------- scaffolding

const testAPI core.API = "test-api"

func testModel() *core.Model {
	return &core.Model{ID: "test-model", Name: "Test", API: testAPI, Provider: "test", ContextWindow: 100000, MaxTokens: 4096}
}

// scripted is a provider that replays a predetermined sequence of assistant
// messages, one per turn. It is the executable double the loop is tested
// against; provider/faux is the shipped, supported form of the same idea
// (NFR-TEST-05).
type scripted struct {
	mu    sync.Mutex
	turns []core.AssistantMessage
	calls int
	// seen records the message list each turn was asked to complete, so a test
	// can assert what the loop actually sent and when.
	seen []core.Messages
}

func (s *scripted) provider() core.APIProvider {
	return core.APIProvider{API: testAPI, Stream: s.stream}
}

func (s *scripted) stream(ctx context.Context, m *core.Model, req core.Request, _ core.ProviderStreamOptions) *core.EventStream {
	st := core.NewEventStream(core.StreamOptions{})
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.seen = append(s.seen, req.Messages)
	var msg core.AssistantMessage
	if i < len(s.turns) {
		msg = s.turns[i]
	} else {
		msg = core.AssistantMessage{
			Content:    core.Content{core.TextBlock{Text: "done"}},
			StopReason: core.StopReasonStop,
		}
	}
	s.mu.Unlock()

	msg.Provider, msg.API, msg.Model = m.Provider, m.API, m.ID
	go func() {
		st.Push(core.MessageStartEvent{Message: msg})
		st.Push(core.MessageEndEvent{Message: msg})
		st.End(core.StreamResult{Message: &msg})
	}()
	return st
}

func (s *scripted) sentAt(turn int) core.Messages {
	s.mu.Lock()
	defer s.mu.Unlock()
	if turn >= len(s.seen) {
		return nil
	}
	return s.seen[turn]
}

func (s *scripted) turnsRun() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

func toolUse(t *testing.T, id, name, args string) core.ToolUseBlock {
	t.Helper()
	b, err := core.NewToolUse(id, name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("NewToolUse: %v", err)
	}
	return b
}

func assistantWithTools(reason core.StopReason, blocks ...core.ContentBlock) core.AssistantMessage {
	return core.AssistantMessage{Content: core.Content(blocks), StopReason: reason}
}

func newTestAgent(t *testing.T, s *scripted, mutate func(*core.AgentConfig)) *Agent {
	t.Helper()
	cfg := core.AgentConfig{
		Model:      testModel(),
		StopPolicy: StopAfterTurns(10),
		Providers:  core.ProviderRegistry{testAPI: s.provider()},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return a
}

func echoTool(name string, calls *atomic.Int32) core.Tool {
	return core.Tool{
		Name:        name,
		Description: "echo",
		InputSchema: schema.Object(schema.Opt("v", schema.String())),
		Handler: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			if calls != nil {
				calls.Add(1)
			}
			return json.RawMessage(`{"echoed":true}`), nil
		},
	}
}

// ------------------------------------------------------------------- REQ-LOOP-01

// TestIterationOnToolUsePresenceNotStopReason is the single most important
// test in the suite. Gemini and several OpenAI-compatible gateways return a
// STOP-family finish reason ALONGSIDE tool calls. A loop that gates iteration
// on stop_reason drops those calls silently and returns an empty answer — and
// passes every Anthropic-only test, because Anthropic does set "tool_use".
//
// Every row here carries tool calls. Every row must execute them.
func TestIterationOnToolUsePresenceNotStopReason(t *testing.T) {
	for _, reason := range []core.StopReason{
		core.StopReasonStop,         // Gemini's STOP alongside functionCall
		core.StopReasonToolUse,      // Anthropic's own
		core.StopReasonStopSequence, //
		"",                          // a gateway that emits nothing
		"finish_reason_unset",       // an unrecognized reason
	} {
		t.Run(string("reason="+reason), func(t *testing.T) {
			var handlerCalls atomic.Int32
			s := &scripted{turns: []core.AssistantMessage{
				assistantWithTools(reason, toolUse(t, "c1", "echo", `{"v":"x"}`)),
				{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
			}}
			a := newTestAgent(t, s, nil)
			if err := a.RegisterTool(echoTool("echo", &handlerCalls)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Run(context.Background(), "go"); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := handlerCalls.Load(); got != 1 {
				t.Fatalf("stop_reason %q: handler ran %d times, want 1.\n"+
					"The loop gated iteration on stop_reason instead of on the presence "+
					"of tool_use blocks (REQ-LOOP-01).", reason, got)
			}
		})
	}
}

// TestErrorAndAbortedShortCircuitBeforeToolExtraction pins the other half of
// REQ-LOOP-01: Error and Aborted are the ONLY reasons that short-circuit, and
// they do so before tools are extracted.
func TestErrorAndAbortedShortCircuitBeforeToolExtraction(t *testing.T) {
	for _, reason := range []core.StopReason{core.StopReasonError, core.StopReasonAborted} {
		t.Run(string(reason), func(t *testing.T) {
			var handlerCalls atomic.Int32
			s := &scripted{turns: []core.AssistantMessage{
				assistantWithTools(reason, toolUse(t, "c1", "echo", `{}`)),
			}}
			a := newTestAgent(t, s, nil)
			_ = a.RegisterTool(echoTool("echo", &handlerCalls))
			_, err := a.Run(context.Background(), "go")
			if err == nil {
				t.Fatal("want an error for a short-circuiting stop reason")
			}
			if handlerCalls.Load() != 0 {
				t.Fatal("tools were extracted and run despite a short-circuiting stop reason")
			}
		})
	}
}

// ------------------------------------------------------------------- REQ-LOOP-02

// TestOneToolResultMessagePerCallInSlotOrder pins that canonical history
// carries one ToolResultMessage per call, in the order the calls appeared —
// not one user message holding all of them (which is an Anthropic wire rule),
// and not in completion order.
func TestOneToolResultMessagePerCallInSlotOrder(t *testing.T) {
	// Handlers finish in REVERSE call order, so a design that appends on
	// completion rather than by slot index produces the wrong order.
	slow := func(name string, d time.Duration) core.Tool {
		return core.Tool{
			Name: name, Description: name,
			InputSchema: schema.Object(),
			Handler: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
				time.Sleep(d)
				return json.RawMessage(`{}`), nil
			},
		}
	}
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "slow", `{}`),
			toolUse(t, "c2", "mid", `{}`),
			toolUse(t, "c3", "fast", `{}`),
		),
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.ParallelTools = true })
	_ = a.RegisterTool(slow("slow", 60*time.Millisecond))
	_ = a.RegisterTool(slow("mid", 30*time.Millisecond))
	_ = a.RegisterTool(slow("fast", time.Millisecond))

	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var ids []string
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok {
			ids = append(ids, tr.ToolUseID)
		}
	}
	want := []string{"c1", "c2", "c3"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("tool result order = %v, want %v (slot order, not completion order — REQ-LOOP-05)", ids, want)
	}
	if len(ids) != 3 {
		t.Fatalf("got %d ToolResultMessages, want 3 — one per call (REQ-LOOP-02)", len(ids))
	}
}

// ------------------------------------------------------------------- REQ-LOOP-10

// TestMaxTokensWithToolCallsExecutesZeroHandlers pins the failure that
// silently corrupts files: streamed arguments are salvage-repaired into valid
// JSON, so a truncated edit passes schema validation and applies cleanly. Only
// the stop reason can catch it, and the response is to execute NOTHING while
// still producing a well-formed result for every call.
func TestMaxTokensWithToolCallsExecutesZeroHandlers(t *testing.T) {
	var handlerCalls atomic.Int32
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonLength,
			toolUse(t, "c1", "echo", `{"v":"a"}`),
			toolUse(t, "c2", "echo", `{"v":"b"}`),
		),
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, nil)
	_ = a.RegisterTool(echoTool("echo", &handlerCalls))

	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := handlerCalls.Load(); got != 0 {
		t.Fatalf("handlers ran %d times on a max_tokens turn, want 0 (REQ-LOOP-10)", got)
	}

	var results []core.ToolResultMessage
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok {
			results = append(results, tr)
		}
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 — every call in the batch still needs one", len(results))
	}
	for _, r := range results {
		if !r.IsError {
			t.Error("a synthesized max_tokens result must be an error result")
		}
		if !strings.Contains(r.Content.Text(), "output token limit") {
			t.Errorf("result text does not carry the pinned REQ-LOOP-10 message: %q", r.Content.Text())
		}
	}
	// The loop must CONTINUE so the model can re-issue.
	if s.turnsRun() < 2 {
		t.Fatalf("loop ran %d turns; a max_tokens turn with tool calls must not terminate the run", s.turnsRun())
	}
}

// ------------------------------------------------------------------- REQ-LOOP-11

// TestBatchAbortIsAllOrNothing pins that a cancelled batch runs NO handler,
// rather than whichever ones the scheduler happened to start. Per-goroutine
// ctx.Err() checks — the obvious Go idiom — let the scheduler split the batch,
// which shows up in production as phantom side effects after Ctrl-C.
//
// Repeated, because a split batch is a race: a single run proves nothing.
func TestBatchAbortIsAllOrNothing(t *testing.T) {
	const runs = 200
	for i := 0; i < runs; i++ {
		var ran atomic.Int32
		s := &scripted{turns: []core.AssistantMessage{
			assistantWithTools(core.StopReasonToolUse,
				toolUse(t, "c1", "echo", `{}`),
				toolUse(t, "c2", "echo", `{}`),
				toolUse(t, "c3", "echo", `{}`),
				toolUse(t, "c4", "echo", `{}`),
			),
		}}
		ctx, cancel := context.WithCancel(context.Background())
		a := newTestAgent(t, s, func(c *core.AgentConfig) { c.ParallelTools = true })
		_ = a.RegisterTool(echoTool("echo", &ran))
		cancel() // already cancelled when the batch is reached
		_, _ = a.Run(ctx, "go")

		if got := ran.Load(); got != 0 {
			t.Fatalf("run %d: %d handlers ran under a cancelled context, want 0.\n"+
				"The abort decision must be made ONCE on the loop goroutine before any "+
				"handler starts, not re-checked per goroutine (REQ-LOOP-11).", i, got)
		}
	}
}

// TestAbortDuringBatchDoesNotSplitIt is the test that actually discriminates.
//
// Cancelling BEFORE the batch is the easy case: every implementation runs zero
// handlers. The bug REQ-LOOP-11 exists to prevent is a batch SPLIT — the abort
// landing while the batch is in progress, so whichever calls had already been
// scheduled run and the rest do not. That is nondeterministic in production
// and shows up as phantom side effects after the user pressed Ctrl-C.
//
// The first handler cancels the context itself, so the abort lands strictly
// after the batch has started. Because the decision was made ONCE, before any
// handler ran, all three must still run. An implementation that re-checks
// ctx.Err() per call — the obvious Go idiom, and what REQ-GO-05 alone implies
// — runs the first and aborts the rest.
func TestAbortDuringBatchDoesNotSplitIt(t *testing.T) {
	var ran atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())

	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "cancels", `{}`),
			toolUse(t, "c2", "echo", `{}`),
			toolUse(t, "c3", "echo", `{}`),
		),
	}}
	// Sequential execution makes the ordering deterministic: thunk 1 runs to
	// completion (cancelling as it goes) before thunk 2 is reached.
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.ParallelTools = false })
	_ = a.RegisterTool(core.Tool{
		Name: "cancels", Description: "cancels the run", InputSchema: schema.Object(),
		Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			ran.Add(1)
			cancel()
			return json.RawMessage(`{}`), nil
		},
	})
	_ = a.RegisterTool(echoTool("echo", &ran))

	_, _ = a.Run(ctx, "go")

	if got := ran.Load(); got != 3 {
		t.Fatalf("%d of 3 handlers ran after an abort landed mid-batch, want all 3.\n"+
			"The batch was SPLIT: the abort decision is being re-checked per call "+
			"instead of being made once, on the loop goroutine, before any handler "+
			"starts (REQ-LOOP-11).", got)
	}
}

// TestAbortedBatchStillProducesAResultPerCall: an aborted call still emits its
// events and a result, so the transcript stays well-formed and resumable.
func TestAbortedBatchStillProducesAResultPerCall(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "echo", `{}`), toolUse(t, "c2", "echo", `{}`)),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	a := newTestAgent(t, s, nil)
	_ = a.RegisterTool(echoTool("echo", nil))
	cancel()
	res, _ := a.Run(ctx, "go")

	n := 0
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok {
			n++
			if !tr.IsError {
				t.Error("an aborted call must yield an error result")
			}
		}
	}
	if n != 2 {
		t.Fatalf("got %d results for an aborted batch of 2, want 2 (REQ-LOOP-11.3)", n)
	}
}

// ------------------------------------------------------------------- REQ-LOOP-05

// TestOneSequentialToolDemotesTheWholeBatch pins REQ-LOOP-05a. It detects
// concurrency directly: a parallel batch would observe overlap.
func TestOneSequentialToolDemotesTheWholeBatch(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32
	mk := func(name string, mode core.ExecutionMode) core.Tool {
		return core.Tool{
			Name: name, Description: name, InputSchema: schema.Object(), ExecutionMode: mode,
			Handler: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
				cur := inFlight.Add(1)
				for {
					old := maxInFlight.Load()
					if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				inFlight.Add(-1)
				return json.RawMessage(`{}`), nil
			},
		}
	}
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "par1", `{}`),
			toolUse(t, "c2", "seq", `{}`),
			toolUse(t, "c3", "par2", `{}`),
		),
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.ParallelTools = true })
	_ = a.RegisterTool(mk("par1", core.Parallel))
	_ = a.RegisterTool(mk("seq", core.Sequential))
	_ = a.RegisterTool(mk("par2", core.Parallel))

	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("max concurrent handlers = %d, want 1: one Sequential tool must demote "+
			"the WHOLE batch (REQ-LOOP-05a)", got)
	}
}

// TestPanickingAfterToolCallDoesNotDeadlockPeers pins NFR-REL-02.1 with a hard
// deadline. A panicking interceptor inside a manual Lock/emit/Unlock leaks the
// mutex and hangs every peer at the join — a DEADLOCK, not a crash, so it
// produces no stack trace and no error. A test without a deadline would hang
// the suite instead of failing it.
func TestPanickingAfterToolCallDoesNotDeadlockPeers(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "echo", `{}`), toolUse(t, "c2", "echo", `{}`),
			toolUse(t, "c3", "echo", `{}`), toolUse(t, "c4", "echo", `{}`)),
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.ParallelTools = true
		c.AfterToolCall = func(ctx context.Context, in core.AfterToolCallContext) core.AfterToolCallDecision {
			panic("interceptor exploded")
		}
	})
	_ = a.RegisterTool(echoTool("echo", nil))

	done := make(chan struct{})
	go func() { defer close(done); _, _ = a.Run(context.Background(), "go") }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish within 5s: a panicking AfterToolCall leaked the " +
			"finalize mutex and deadlocked the peer tool goroutines (NFR-REL-02.1)")
	}
}

// TestPanickingHandlerBecomesErrorResult: NFR-REL-02.2.
func TestPanickingHandlerBecomesErrorResult(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", "boom", `{}`)),
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, nil)
	_ = a.RegisterTool(core.Tool{
		Name: "boom", Description: "panics", InputSchema: schema.Object(),
		Handler: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			panic("handler exploded")
		},
	})
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("a panicking handler must not fail the run: %v", err)
	}
	found := false
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok && tr.IsError {
			found = true
		}
	}
	if !found {
		t.Fatal("a panicking handler must become a tool result with is_error set")
	}
}

// ------------------------------------------------------------------- REQ-LOOP-04

// TestStopPolicyRunsAfterResultsAreInHistory pins REQ-LOOP-04a. A limit
// checked between tool extraction and execution ends the transcript with
// dangling tool_use blocks that no provider accepts on resume.
func TestStopPolicyRunsAfterResultsAreInHistory(t *testing.T) {
	var sawResults int
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", "echo", `{}`)),
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.StopPolicy = func(sc core.StopContext) bool {
			sawResults = len(sc.ToolResults)
			return true // stop after the first turn
		}
	})
	_ = a.RegisterTool(echoTool("echo", nil))

	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if sawResults != 1 {
		t.Fatalf("StopContext.ToolResults had %d entries, want 1: the stop check must run "+
			"AFTER the turn's tools executed and their results are in history (REQ-LOOP-04a)", sawResults)
	}
	// And the transcript must not end on an unanswered tool_use.
	last := res.Messages[len(res.Messages)-1]
	if _, isAssistant := last.(core.AssistantMessage); isAssistant {
		t.Fatal("run ended on an assistant message carrying tool_use with no results: " +
			"that transcript is unresumable")
	}
}

// TestStopPolicyReasonSurvivesStopAny: with a bare bool predicate the loop
// cannot tell ErrMaxTurns from ErrBudgetExceeded, and StopAny erases it.
func TestStopPolicyReasonSurvivesStopAny(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "a"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.ErrorOnLimit = true
		c.StopPolicy = StopAny(StopOverBudget(1e9), StopAfterTurns(1))
	})
	res, err := a.Run(context.Background(), "go")
	if !errors.Is(err, core.ErrMaxTurns) {
		t.Fatalf("err = %v, want ErrMaxTurns", err)
	}
	if res.StopReason != core.RunStopMaxTurns {
		t.Fatalf("StopReason = %q, want %q", res.StopReason, core.RunStopMaxTurns)
	}
}

// ------------------------------------------------------------------- REQ-LOOP-13/15

// TestSteeringIsDeliveredBeforeTheNextRequest pins REQ-LOOP-13's drain point:
// a steered message must be visible to the NEXT provider request, and must
// never land between an assistant response and its tool results.
func TestSteeringIsDeliveredBeforeTheNextRequest(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", "echo", `{}`)),
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	var a *Agent
	a = newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Hooks.OnTurnEnd = func(e core.TurnEndEvent) {
			if e.TurnIndex == 0 {
				_ = a.SteerText("steered")
			}
		}
	})
	_ = a.RegisterTool(echoTool("echo", nil))
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	sent := s.sentAt(1)
	if len(sent) == 0 {
		t.Fatal("no second request was made")
	}
	// The steered message must be present, and must come after the tool result.
	var sawToolResult bool
	var steerIdx = -1
	for i, m := range sent {
		switch v := m.(type) {
		case core.ToolResultMessage:
			sawToolResult = true
		case core.UserMessage:
			if v.Content.Text() == "steered" {
				steerIdx = i
			}
		}
	}
	if steerIdx < 0 {
		t.Fatal("the steered message was not delivered into the next request (REQ-LOOP-13)")
	}
	if !sawToolResult {
		t.Fatal("the tool result vanished from the request")
	}
	if _, isTR := sent[steerIdx-1].(core.AssistantMessage); isTR {
		t.Fatal("the steered message landed between an assistant response and its tool " +
			"results, violating REQ-LOOP-02")
	}
}

// TestSteeringKeepsInnerLoopAlive: pending steering keeps the loop alive even
// when the assistant produced NO tool calls (REQ-LOOP-13).
func TestSteeringKeepsInnerLoopAlive(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "one"}}, StopReason: core.StopReasonStop},
		{Content: core.Content{core.TextBlock{Text: "two"}}, StopReason: core.StopReasonStop},
	}}
	var a *Agent
	a = newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Hooks.OnTurnEnd = func(e core.TurnEndEvent) {
			if e.TurnIndex == 0 {
				_ = a.SteerText("keep going")
			}
		}
	})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if s.turnsRun() != 2 {
		t.Fatalf("ran %d turns, want 2: a pending steering message must keep the inner "+
			"loop alive even with no tool calls (REQ-LOOP-13)", s.turnsRun())
	}
}

// TestFollowUpRestartsWithinTheSameRun: one RunResult, no second AgentStart.
func TestFollowUpRestartsWithinTheSameRun(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "one"}}, StopReason: core.StopReasonStop},
		{Content: core.Content{core.TextBlock{Text: "two"}}, StopReason: core.StopReasonStop},
	}}
	var a *Agent
	var starts int
	a = newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Hooks.OnTurnEnd = func(e core.TurnEndEvent) {
			if e.TurnIndex == 0 {
				_ = a.FollowUpText("and now this")
			}
		}
	})
	st, err := a.Stream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	for e := range st.Events() {
		if _, ok := e.(core.AgentStartEvent); ok {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("saw %d AgentStartEvents, want exactly 1: a follow-up restarts the outer "+
			"loop WITHIN the same run (REQ-LOOP-14)", starts)
	}
	if s.turnsRun() != 2 {
		t.Fatalf("ran %d turns, want 2", s.turnsRun())
	}
}

// TestConcurrentRunReturnsErrBusy pins REQ-LOOP-15: conflicting operations
// fail rather than queue, and never block.
func TestConcurrentRunReturnsErrBusy(t *testing.T) {
	release := make(chan struct{})
	s := &scripted{}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Hooks.OnTurnStart = func(core.TurnStartEvent) { <-release }
	})
	go func() { _, _ = a.Run(context.Background(), "first") }()

	// Wait until the first run has the slot.
	deadline := time.Now().Add(2 * time.Second)
	for a.Phase() == core.PhaseIdle && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, err := a.Run(context.Background(), "second")
	close(release)
	if !errors.Is(err, core.ErrBusy) {
		t.Fatalf("second Run returned %v, want ErrBusy", err)
	}
}

// TestSteerBeforeRunIsDeliveredIntoTheRun pins the claim-before-drain ordering
// of REQ-LOOP-15: the slot is claimed and the queue drained under ONE lock, so
// a message queued before the run starts cannot be lost.
func TestSteerBeforeRunIsDeliveredIntoTheRun(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.SteeringQueueMode = core.QueueDrainAll })
	if err := a.SteerText("queued first"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "prompt"); err != nil {
		t.Fatal(err)
	}
	sent := s.sentAt(0)
	found := false
	for _, m := range sent {
		if u, ok := m.(core.UserMessage); ok && u.Content.Text() == "queued first" {
			found = true
		}
	}
	if !found {
		t.Fatal("a message steered before Run was silently dropped: the run slot must be " +
			"claimed and the queue drained under one lock (REQ-LOOP-15)")
	}
}

// ------------------------------------------------------------------- REQ-LOOP-16

func TestContinuePreconditions(t *testing.T) {
	t.Run("empty history is not continuable", func(t *testing.T) {
		a := newTestAgent(t, &scripted{}, nil)
		if _, err := a.Continue(context.Background()); !errors.Is(err, core.ErrNotContinuable) {
			t.Fatalf("err = %v, want ErrNotContinuable", err)
		}
	})

	t.Run("completed assistant turn is not continuable", func(t *testing.T) {
		s := &scripted{turns: []core.AssistantMessage{
			{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
		}}
		a := newTestAgent(t, s, nil)
		if _, err := a.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Continue(context.Background()); !errors.Is(err, core.ErrNotContinuable) {
			t.Fatalf("err = %v, want ErrNotContinuable", err)
		}
	})

	t.Run("assistant turn with a queued message is continuable", func(t *testing.T) {
		s := &scripted{turns: []core.AssistantMessage{
			{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
			{Content: core.Content{core.TextBlock{Text: "more"}}, StopReason: core.StopReasonStop},
		}}
		a := newTestAgent(t, s, nil)
		if _, err := a.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		if err := a.SteerText("carry on"); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Continue(context.Background()); err != nil {
			t.Fatalf("Continue: %v", err)
		}
	})

	t.Run("history ending in a tool result is continuable without a new message", func(t *testing.T) {
		// This is the normal outcome of REQ-LOOP-09 cancellation, so Continue
		// is not an optional convenience.
		h := core.NewConversationHistory()
		h.Record(core.NullLeaf, core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}})
		h.Record(core.NullLeaf, core.AssistantMessage{
			Content:    core.Content{toolUse(t, "c1", "echo", `{}`)},
			StopReason: core.StopReasonToolUse,
		})
		h.Record(core.NullLeaf, core.ToolResultMessage{ToolUseID: "c1", ToolName: "echo"})

		s := &scripted{turns: []core.AssistantMessage{
			{Content: core.Content{core.TextBlock{Text: "resumed"}}, StopReason: core.StopReasonStop},
		}}
		cfg := core.AgentConfig{Model: testModel(), StopPolicy: StopAfterTurns(5),
			Providers: core.ProviderRegistry{testAPI: s.provider()}}
		a, err := NewAgentWithHistory(cfg, h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Continue(context.Background()); err != nil {
			t.Fatalf("Continue on a transcript ending in a tool result: %v", err)
		}
	})
}

// ------------------------------------------------------------------- REQ-TOOL-13

func TestBatchTerminationIsAnAndNotAnOr(t *testing.T) {
	finish := func(name string, terminate bool) core.Tool {
		return core.Tool{
			Name: name, Description: name, InputSchema: schema.Object(),
			Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
				r := core.OKResult(map[string]any{"n": name})
				r.Terminate = terminate
				return r
			},
		}
	}
	t.Run("one terminating tool does not end a mixed batch", func(t *testing.T) {
		s := &scripted{turns: []core.AssistantMessage{
			assistantWithTools(core.StopReasonToolUse,
				toolUse(t, "c1", "finish", `{}`), toolUse(t, "c2", "keep", `{}`)),
			{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
		}}
		a := newTestAgent(t, s, nil)
		_ = a.RegisterTool(finish("finish", true))
		_ = a.RegisterTool(finish("keep", false))
		res, err := a.Run(context.Background(), "go")
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason == core.RunStopToolTerminate {
			t.Fatal("a single terminating tool ended a mixed batch: the vote is an AND, " +
				"not an OR (REQ-TOOL-13.1). Under OR the other results are computed, " +
				"written to history, and never shown to the model.")
		}
	})

	t.Run("a unanimous batch terminates", func(t *testing.T) {
		s := &scripted{turns: []core.AssistantMessage{
			assistantWithTools(core.StopReasonToolUse,
				toolUse(t, "c1", "finish", `{}`), toolUse(t, "c2", "finish", `{}`)),
		}}
		a := newTestAgent(t, s, nil)
		_ = a.RegisterTool(finish("finish", true))
		res, err := a.Run(context.Background(), "go")
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != core.RunStopToolTerminate {
			t.Fatalf("StopReason = %q, want %q", res.StopReason, core.RunStopToolTerminate)
		}
	})

	t.Run("an empty batch never terminates", func(t *testing.T) {
		if core.BatchTerminates(nil) {
			t.Fatal("an empty batch must not terminate")
		}
	})
}

// ------------------------------------------------------------------- REQ-SEC-03

func TestBlockedCallProducesAnErrorResultAndTheLoopContinues(t *testing.T) {
	var ran atomic.Int32
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", "echo", `{}`)),
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.BeforeToolCall = func(ctx context.Context, in core.BeforeToolCallContext) core.BeforeToolCallDecision {
			return core.BeforeToolCallDecision{Block: true, Reason: "not allowed here"}
		}
	})
	_ = a.RegisterTool(echoTool("echo", &ran))
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 {
		t.Fatal("a blocked call must not reach the handler")
	}
	found := false
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok && tr.IsError &&
			strings.Contains(tr.Content.Text(), "not allowed here") {
			found = true
		}
	}
	if !found {
		t.Fatal("a blocked call must produce an error result carrying the policy's reason")
	}
}

// TestPanickingInterceptorFailsClosed: a security boundary that opens on panic
// is not a boundary.
func TestPanickingInterceptorFailsClosed(t *testing.T) {
	var ran atomic.Int32
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", "echo", `{}`)),
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.BeforeToolCall = func(ctx context.Context, in core.BeforeToolCallContext) core.BeforeToolCallDecision {
			panic("policy exploded")
		}
	})
	_ = a.RegisterTool(echoTool("echo", &ran))
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 {
		t.Fatal("a panicking BeforeToolCall must fail CLOSED: the handler ran anyway")
	}
}

// ------------------------------------------------------------------- lifecycle

func TestAbortFromAnotherGoroutine(t *testing.T) {
	started := make(chan struct{})
	s := &scripted{}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Hooks.OnTurnStart = func(core.TurnStartEvent) {
			select {
			case <-started:
			default:
				close(started)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	go func() { <-started; a.Abort() }()
	_, err := a.Run(context.Background(), "go")
	if err == nil {
		t.Skip("provider completed before the abort landed; timing-dependent")
	}
	if !a.Idle() {
		t.Fatal("agent must be Idle after an aborted run")
	}
}

func TestSnapshotCarriesProducerIDAndRevision(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, nil)
	before, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	after, _ := a.Snapshot(context.Background())

	if before.ProducerID != after.ProducerID {
		t.Fatal("ProducerID must be stable for the lifetime of one Agent value")
	}
	if after.Revision <= before.Revision {
		t.Fatalf("Revision did not advance: %d -> %d", before.Revision, after.Revision)
	}
	if !after.Idle {
		t.Fatal("snapshot taken after the run should report Idle")
	}
}

func TestHoldKeepsAgentNonIdle(t *testing.T) {
	a := newTestAgent(t, &scripted{}, nil)
	if !a.Idle() {
		t.Fatal("a fresh agent should be idle")
	}
	release := a.Hold()
	if a.Idle() {
		t.Fatal("Idle must be false while a hold is outstanding (REQ-LIFE-06)")
	}
	release()
	release() // idempotent
	if !a.Idle() {
		t.Fatal("Idle must return true once the hold is released")
	}
}

// ------------------------------------------------------------------- estimate

// TestEstimateSkipRulesEachFireIndependently pins REQ-GO-15. Each case
// satisfies the OTHER two skip rules, so deleting any one `continue` from the
// implementation fails exactly one subtest.
func TestEstimateSkipRulesEachFireIndependently(t *testing.T) {
	good := core.AssistantMessage{StopReason: core.StopReasonStop}
	good.Usage.SetField(core.UsageInputTokens, 1000)

	t.Run("(a) aborted turn is not an anchor", func(t *testing.T) {
		bad := core.AssistantMessage{StopReason: core.StopReasonAborted}
		bad.Usage.SetField(core.UsageInputTokens, 999999)
		got := EstimateContextTokens(core.Messages{good, bad}, nil)
		if got > 2000 {
			t.Fatalf("estimate %d used an aborted turn as the anchor", got)
		}
	})

	t.Run("(b) zero-usage turn is not an anchor", func(t *testing.T) {
		var zero core.AssistantMessage
		zero.StopReason = core.StopReasonStop
		zero.Usage.SetField(core.UsageInputTokens, 0)
		got := EstimateContextTokens(core.Messages{good, zero}, nil)
		if got < 1000 {
			t.Fatalf("estimate %d fell back past the valid anchor: a zero-usage response "+
				"was treated as authoritative", got)
		}
	})

	t.Run("(c) anchor invalidated by a later-inserted prefix", func(t *testing.T) {
		// The checkpoint says a summary was inserted, so an assistant message
		// from before it was sent under a different prefix.
		cp := &core.CompactionCheckpoint{PrefixLen: 1, Summary: "s", CreatedAtLen: 4}
		msgs := core.Messages{good, good, good}
		got := EstimateContextTokens(msgs, cp)
		if got >= 1000 {
			t.Fatalf("estimate %d used a stale anchor: rule (c) never fired. Without "+
				"CompactionCheckpoint.CreatedAtLen it cannot fire at all (ruling P-2)", got)
		}
	})
}

// ------------------------------------------------------------------- toolpolicy

func TestToolPolicyResolution(t *testing.T) {
	builtin := func(n string) core.Tool {
		return core.Tool{Name: n, Description: n, Builtin: true, InputSchema: schema.Object(),
			Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }}
	}
	custom := func(n string) core.Tool {
		return core.Tool{Name: n, Description: n, InputSchema: schema.Object(),
			Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }}
	}
	reg := []core.Tool{builtin("read"), builtin("write"), builtin("exec")}

	names := func(ts []core.Tool) string {
		var out []string
		for _, t := range ts {
			out = append(out, t.Name)
		}
		return strings.Join(out, ",")
	}

	// The four non-obvious consequences of REQ-TOOL-10, each its own row.
	cases := []struct {
		name   string
		policy core.ToolPolicy
		want   string
	}{
		{`NoTools "all" disables CUSTOM tools too`,
			core.ToolPolicy{NoTools: core.NoToolsAll, CustomTools: []core.Tool{custom("mine")}}, ""},
		{`NoTools "builtin" leaves custom tools alive`,
			core.ToolPolicy{NoTools: core.NoToolsBuiltin, CustomTools: []core.Tool{custom("mine")}}, "mine"},
		{`a ToolNames allowlist constrains custom tools`,
			core.ToolPolicy{ToolNames: []string{"read"}, CustomTools: []core.Tool{custom("mine")}}, "read"},
		{`ExcludeTools applies to custom tools`,
			core.ToolPolicy{ExcludeTools: []string{"mine"}, CustomTools: []core.Tool{custom("mine")}}, "read,write,exec"},
		{`Tools non-nil bypasses everything`,
			core.ToolPolicy{Tools: []core.Tool{custom("only")}, ToolNames: []string{"read"}, NoTools: core.NoToolsAll}, "only"},
		{`Tools non-nil but EMPTY means no tools, deliberately`,
			core.ToolPolicy{Tools: []core.Tool{}}, ""},
		{`nil ToolNames means the default set`,
			core.ToolPolicy{}, "read,write,exec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := names(ResolveToolPolicy(reg, tc.policy)); got != tc.want {
				t.Fatalf("resolved %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCustomToolOverridesBuiltinInPlace: overriding must not reorder the tool
// list, because the tool list is part of the cached prompt prefix.
func TestCustomToolOverridesBuiltinInPlace(t *testing.T) {
	reg := []core.Tool{
		{Name: "a", Builtin: true}, {Name: "read", Builtin: true, Description: "builtin"}, {Name: "z", Builtin: true},
	}
	got := ResolveToolPolicy(reg, core.ToolPolicy{
		CustomTools: []core.Tool{{Name: "read", Description: "custom"}},
	})
	if len(got) != 3 || got[1].Name != "read" || got[1].Description != "custom" {
		t.Fatalf("override did not happen in place: %+v", got)
	}
}

func TestPreparedArgumentsPreserveKeyOrder(t *testing.T) {
	tool := core.Tool{
		Name: "t", InputSchema: schema.Object(
			schema.Prop("zeta", schema.String()), schema.Opt("alpha", schema.String())),
	}
	c := toolUse(t, "c1", "t", `{"zeta":"1","alpha":"2"}`)
	p, err := PrepareArguments(tool, c)
	if err != nil {
		t.Fatal(err)
	}
	if string(p.Raw) != `{"zeta":"1","alpha":"2"}` {
		t.Fatalf("Raw = %s; the model's own bytes must pass through untouched when "+
			"nothing changed (REQ-PROV-17)", p.Raw)
	}
}

func TestOptionalNullsAreDeletedNotRejected(t *testing.T) {
	// Constrained sampling forces the model to emit every declared property,
	// so optional fields arrive as explicit nulls.
	tool := core.Tool{
		Name: "t", InputSchema: schema.Object(
			schema.Prop("path", schema.String()), schema.Opt("limit", schema.Int())),
	}
	c := toolUse(t, "c1", "t", `{"path":"/x","limit":null}`)
	p, err := PrepareArguments(tool, c)
	if err != nil {
		t.Fatalf("an explicit null for an OPTIONAL property must be deleted, not rejected "+
			"(REQ-TOOL-11.2): %v", err)
	}
	if _, present := p.Args["limit"]; present {
		t.Fatal("the optional null was not deleted")
	}
}

func TestValidationErrorEchoesTheModelsOwnKeyOrder(t *testing.T) {
	tool := core.Tool{
		Name: "t", InputSchema: schema.Object(schema.Prop("path", schema.String())),
	}
	c := toolUse(t, "c1", "t", `{"zeta":1,"alpha":2}`)
	_, err := PrepareArguments(tool, c)
	if err == nil {
		t.Fatal("want a validation error for a missing required property")
	}
	msg := err.Error()
	zi, ai := strings.Index(msg, "zeta"), strings.Index(msg, "alpha")
	if zi < 0 || ai < 0 || zi > ai {
		t.Fatalf("error text did not echo the model's own key order (REQ-TOOL-12.3):\n%s", msg)
	}
}

func TestStreamResultAvailableWithoutReadingAnyEvent(t *testing.T) {
	// REQ-GO-08: the result is fed by the terminal event, not by consumption,
	// and that is what makes abandoning a stream safe.
	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "hello"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, nil)
	st, err := a.Stream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.RunResult() // no event ever read
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText() != "hello" {
		t.Fatalf("FinalText = %q", res.FinalText())
	}
}

func TestUnknownToolYieldsAnErrorResultNotACrash(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", "nope", `{}`)),
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, nil)
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok && tr.IsError &&
			strings.Contains(tr.Content.Text(), "nope") {
			found = true
		}
	}
	if !found {
		t.Fatal("an unknown tool must produce an error result the model can see")
	}
}

var _ = fmt.Sprintf
