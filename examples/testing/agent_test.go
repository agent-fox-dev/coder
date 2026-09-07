package testingexample_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/faux"
	"github.com/agentfox/agentkit-go/schema"
)

// ---------------------------------------------------------------- fixtures
//
// Everything below this line is what YOU would write in your own repository:
// a tool, and a constructor that wires an agent to a scripted provider. There
// is no key, no network and no environment variable anywhere in this file.

const systemPrompt = "You are a weather bot. Answer in one sentence."

// weatherTool is the code under test. calls, when non-nil, counts executions
// so a test can assert the handler ran — or, more often, that it did not.
func weatherTool(calls *atomic.Int32) core.Tool {
	return core.Tool{
		Name:        "get_weather",
		Description: "Look up the current temperature in a city.",
		InputSchema: schema.Object(schema.Prop("city", schema.String("City name"))),
		Handler: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			if calls != nil {
				calls.Add(1)
			}
			var args struct {
				City string `json:"city"`
			}
			if err := json.Unmarshal(in, &args); err != nil {
				return nil, err
			}
			if args.City == "" {
				return nil, errors.New("city is required")
			}
			if args.City != "Oslo" {
				return nil, errors.New("no station for " + args.City)
			}
			return json.RawMessage(`{"city":"Oslo","temp_c":7}`), nil
		},
	}
}

// newAgent wires an Agent to a scripted provider. The three lines that matter
// are Model, Providers and StopPolicy: faux.Model() names the faux wire, the
// registry maps that wire to this scripted provider, and the stop policy
// bounds a test that under-scripts so it fails on an assertion instead of
// spinning. Nothing else about the agent changes for a test.
func newAgent(t *testing.T, p *faux.Provider, mutate func(*core.AgentConfig)) *agentkit.Agent {
	t.Helper()
	cfg := core.AgentConfig{
		Model:        faux.Model(),
		Providers:    core.ProviderRegistry{faux.API: p.APIProvider()},
		SystemPrompt: systemPrompt,
		StopPolicy:   agentkit.StopAfterTurns(5),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := agentkit.NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return a
}

// weatherScript is the two-turn conversation most of these tests replay: the
// model calls the tool, sees the result, and answers.
func weatherScript() *faux.Provider {
	return faux.New(
		faux.Turn{
			Blocks: []core.ContentBlock{
				faux.FauxText("Let me look that up."),
				faux.FauxToolCall("call_1", "get_weather", `{"city":"Oslo"}`),
			},
			StopReason: core.StopReasonToolUse,
		},
		faux.Turn{
			Blocks:     []core.ContentBlock{faux.FauxText("It is 7C in Oslo.")},
			StopReason: core.StopReasonStop,
		},
	)
}

// toolResults collects the tool results out of a run, which is where an
// assertion about what a tool actually returned to the model belongs.
func toolResults(msgs core.Messages) []core.ToolResultMessage {
	var out []core.ToolResultMessage
	for _, m := range msgs {
		if tr, ok := m.(core.ToolResultMessage); ok {
			out = append(out, tr)
		}
	}
	return out
}

// --------------------------------------------------------------------- 1 --
//
// Scripting a run.

// TestAScriptedProviderDrivesAToolCallAndAFinalAnswer is the shape of almost
// every test you will write: script the turns, run, assert on the outcome.
//
// The script is the model. faux.Turn is one assistant response; the loop is
// asked for a second turn only because the first one carried a tool call, so
// the fact that turn 2 was reached at all is itself the assertion that the
// call was executed and fed back.
func TestAScriptedProviderDrivesAToolCallAndAFinalAnswer(t *testing.T) {
	var calls atomic.Int32
	p := weatherScript()
	a := newAgent(t, p, nil)
	if err := a.RegisterTool(weatherTool(&calls)); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	res, err := a.Run(context.Background(), "What is the weather in Oslo?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("the handler ran %d times, want 1.\n"+
			"The scripted turn carried one tool call, so the loop owed exactly one "+
			"execution: zero means the call never reached the registry (usually a name "+
			"mismatch between the script and RegisterTool), more than one means it was "+
			"replayed.", got)
	}
	if got := p.Calls(); got != 2 {
		t.Fatalf("the provider was called %d times, want 2.\n"+
			"A turn with tool calls must be followed by another model call carrying the "+
			"results; a run that stops at 1 never showed the tool output to the model.", got)
	}
	if got := res.FinalText(); got != "It is 7C in Oslo." {
		t.Fatalf("final text = %q, want the text scripted for the second turn.\n"+
			"RunResult.FinalText is the last assistant message; getting the first turn's "+
			"text back means the run ended a turn early.", got)
	}

	results := toolResults(res.Messages)
	if len(results) != 1 {
		t.Fatalf("got %d tool results, want 1 — one per call, always", len(results))
	}
	if results[0].IsError {
		t.Fatalf("the tool result is an error result: %s.\n"+
			"The handler returned successfully, so nothing should have marked this an "+
			"error; if it did, the failure is between the handler and the transcript.",
			results[0].Content.Text())
	}
	if !strings.Contains(results[0].Content.Text(), `"temp_c":7`) {
		t.Fatalf("tool result content = %q, want the handler's payload.\n"+
			"What the handler returns is wrapped in an envelope but never rewritten; if "+
			"the payload is missing, the model never saw the answer.", results[0].Content.Text())
	}
}

// --------------------------------------------------------------------- 2 --
//
// Asserting what was actually SENT.

// TestTheRequestCarriesTheSystemPromptTheToolAndTheToolResult asserts on the
// provider's inbox rather than on the run's outbox.
//
// This is the thing you cannot check any other way. A system prompt that never
// reaches the model, a tool that is registered but not declared, or a tool
// result that is computed and then dropped from the transcript all produce a
// run that looks fine from the outside and a model that behaves as though your
// code did not run. faux.Provider.Requests is the only place the difference is
// visible.
func TestTheRequestCarriesTheSystemPromptTheToolAndTheToolResult(t *testing.T) {
	p := weatherScript()
	a := newAgent(t, p, nil)
	if err := a.RegisterTool(weatherTool(nil)); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	if _, err := a.Run(context.Background(), "What is the weather in Oslo?"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := p.Requests()
	if len(reqs) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(reqs))
	}
	first, second := reqs[0], reqs[1]

	// The system prompt is ASSEMBLED — your text plus whatever the SDK adds —
	// so assert that yours is in there, not that it is the whole thing.
	var system string
	for _, b := range first.System {
		if tb, ok := b.(core.TextBlock); ok {
			system += tb.Text
		}
	}
	if !strings.Contains(system, systemPrompt) {
		t.Fatalf("the system prompt sent to the provider does not contain the configured "+
			"text.\nsent: %q\nwant it to contain: %q\n"+
			"AgentConfig.SystemPrompt is assembled with the SDK's own sections before it "+
			"is sent; if your text is absent the model is running with instructions you "+
			"never wrote.", system, systemPrompt)
	}

	// Tools are declared as ToolWire — name, description, schema. A registered
	// tool that is not declared is invisible to the model, which then answers
	// from memory instead of calling it.
	var declared []string
	for _, tw := range first.Tools {
		declared = append(declared, tw.Name)
		if tw.Name == "get_weather" {
			if tw.Description == "" {
				t.Error("get_weather was declared with an empty description; the description " +
					"is the only thing telling the model when to call it")
			}
			if tw.InputSchema == nil || !tw.InputSchema.IsRequired("city") {
				t.Error("get_weather's declared schema does not require \"city\"; the model " +
					"is being invited to omit it")
			}
		}
	}
	if len(declared) != 1 || declared[0] != "get_weather" {
		t.Fatalf("declared tools = %v, want exactly [get_weather].\n"+
			"A tool registered but not declared is a tool the model cannot call.", declared)
	}

	// The second request is the transcript the model sees AFTER the tool ran.
	// The tool result must be in it, addressed to the call it answers.
	var found *core.ToolResultMessage
	for _, m := range second.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok && tr.ToolUseID == "call_1" {
			found = &tr
			break
		}
	}
	if found == nil {
		t.Fatalf("the second request carries no tool result for call_1 (%d messages).\n"+
			"An unanswered tool call is a broken transcript: most providers reject it, and "+
			"the ones that do not will re-issue the same call forever.", len(second.Messages))
	}
	if !strings.Contains(found.Content.Text(), `"temp_c":7`) {
		t.Fatalf("the tool result sent back to the model is %q; the handler's payload did "+
			"not survive the round trip", found.Content.Text())
	}
}

// --------------------------------------------------------------------- 3 --
//
// Testing a tool handler in isolation.

// TestAToolHandlerIsTestedByCallingIt needs no agent, no provider and no
// script — a handler is an ordinary function from json.RawMessage to
// json.RawMessage.
//
// Write these first and write most of them. Nearly every tool bug is a bug in
// the handler — a missing field, an unchecked error, a path that escapes its
// root — and none of them need a loop to find. Save the scripted runs for what
// only the loop can show you.
func TestAToolHandlerIsTestedByCallingIt(t *testing.T) {
	handler := weatherTool(nil).Handler

	for _, tc := range []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "known city", input: `{"city":"Oslo"}`, want: `{"city":"Oslo","temp_c":7}`},
		{name: "unknown city", input: `{"city":"Atlantis"}`, wantErr: "no station"},
		{name: "missing city", input: `{}`, wantErr: "city is required"},
		{name: "not an object", input: `[]`, wantErr: "cannot unmarshal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := handler(context.Background(), json.RawMessage(tc.input))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("input %s returned no error and output %s.\n"+
						"A handler that swallows bad input hands the model a plausible answer "+
						"it cannot tell from a real one.", tc.input, out)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to mention %q — the message is what the "+
						"model reads and retries against", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("input %s: unexpected error %v", tc.input, err)
			}
			if string(out) != tc.want {
				t.Fatalf("output = %s, want %s", out, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------- 4 --
//
// Testing a BeforeToolCall interceptor.

// TestABlockedToolCallBecomesAnErrorResultAndTheRunContinues pins both halves
// of what an authorization boundary owes.
//
// Blocking is easy; the half that gets missed is that the run must SURVIVE it.
// A denial is not a crash — the model is supposed to see the refusal, in the
// slot of the call it made, and choose what to do next. An interceptor that
// aborts the run instead turns "you may not delete that" into a dead session,
// and the transcript is left with an unanswered tool call in it.
func TestABlockedToolCallBecomesAnErrorResultAndTheRunContinues(t *testing.T) {
	var calls atomic.Int32
	var saw []string // the calls the interceptor was consulted about

	blockOslo := func(_ context.Context, in core.BeforeToolCallContext) core.BeforeToolCallDecision {
		saw = append(saw, in.ToolName)
		if in.ToolName == "get_weather" && in.Arguments["city"] == "Oslo" {
			return core.BeforeToolCallDecision{Block: true, Reason: "Oslo is out of policy"}
		}
		return core.BeforeToolCallDecision{}
	}

	p := weatherScript()
	a := newAgent(t, p, func(c *core.AgentConfig) { c.BeforeToolCall = blockOslo })
	if err := a.RegisterTool(weatherTool(&calls)); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	res, err := a.Run(context.Background(), "What is the weather in Oslo?")
	if err != nil {
		t.Fatalf("Run: %v.\nA blocked call is a normal outcome, not a run failure.", err)
	}

	if len(saw) != 1 || saw[0] != "get_weather" {
		t.Fatalf("the interceptor was consulted about %v, want exactly [get_weather].\n"+
			"Every call must pass the boundary; one that does not is an unauthorized "+
			"execution.", saw)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("the handler ran %d times despite being blocked.\n"+
			"Blocking after the side effect has happened is not blocking.", got)
	}

	results := toolResults(res.Messages)
	if len(results) != 1 {
		t.Fatalf("got %d tool results, want 1.\n"+
			"A blocked call still owes a result: the model made a call and the transcript "+
			"must answer it.", len(results))
	}
	if !results[0].IsError {
		t.Fatalf("the result for a blocked call is not marked as an error: %s.\n"+
			"Without is_error the model reads the refusal as a successful tool return.",
			results[0].Content.Text())
	}
	if body := results[0].Content.Text(); !strings.Contains(body, core.BlockErrorCode) ||
		!strings.Contains(body, "Oslo is out of policy") {
		t.Fatalf("the blocked result is %q; it must carry the %q code and the reason.\n"+
			"The reason is the only thing that lets the model do something else instead of "+
			"re-issuing the same call.", body, core.BlockErrorCode)
	}

	// The half that is easy to forget: the loop kept going.
	if got := p.Calls(); got != 2 {
		t.Fatalf("the provider was called %d times, want 2.\n"+
			"A blocked call must not end the run — the model has to see the refusal and "+
			"get a chance to respond to it. (Set BeforeToolCallDecision.Terminate when you "+
			"do want a denial to stop the run.)", got)
	}
	if res.FinalText() == "" {
		t.Fatal("the run produced no final assistant text after the block; the model never " +
			"got its turn to respond to the refusal")
	}
}

// --------------------------------------------------------------------- 5 --
//
// Testing middleware.

// countingMiddleware records the order calls enter the chain and counts them.
// The mutex is not ceremony: middleware is user code running on the loop's
// goroutine, and tests run under -race.
type countingMiddleware struct {
	mu    sync.Mutex
	label string
	order *[]string
	calls int
}

func (m *countingMiddleware) wrap(next core.Handler) core.Handler {
	return func(ctx context.Context, req core.Request) *core.EventStream {
		m.mu.Lock()
		m.calls++
		*m.order = append(*m.order, m.label)
		m.mu.Unlock()
		return next(ctx, req)
	}
}

// headerMiddleware is the other thing middleware is for: changing WHAT is
// asked. It copies the map rather than writing through it, because the Request
// it is handed is shared with the rest of the chain.
func headerMiddleware(name, value string) core.Middleware {
	return func(next core.Handler) core.Handler {
		return func(ctx context.Context, req core.Request) *core.EventStream {
			headers := make(map[string]*string, len(req.Options.Headers)+1)
			for k, v := range req.Options.Headers {
				headers[k] = v
			}
			v := value
			headers[name] = &v
			req.Options.Headers = headers
			return next(ctx, req)
		}
	}
}

// TestMiddlewareWrapsEveryModelCallAndTheLastRegisteredIsOutermost pins the
// ordering rule, which is the part people get backwards.
//
// The LAST middleware in the slice is the OUTERMOST: it sees the request first
// and the stream last. Get it wrong and a retry wrapper ends up inside the
// logging wrapper, so retries never appear in the log — everything still
// works, and the observability is quietly a lie.
func TestMiddlewareWrapsEveryModelCallAndTheLastRegisteredIsOutermost(t *testing.T) {
	var order []string
	inner := &countingMiddleware{label: "registered-first", order: &order}
	outer := &countingMiddleware{label: "registered-last", order: &order}

	p := weatherScript()
	a := newAgent(t, p, func(c *core.AgentConfig) {
		c.Middleware = []core.Middleware{
			inner.wrap,
			outer.wrap,
			headerMiddleware("X-Trace-Id", "trace-123"),
		}
	})
	if err := a.RegisterTool(weatherTool(nil)); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	if _, err := a.Run(context.Background(), "What is the weather in Oslo?"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if inner.calls != 2 || outer.calls != 2 {
		t.Fatalf("middleware ran %d and %d times for a 2-turn run, want 2 each.\n"+
			"Middleware wraps EVERY model call, not just the first; a count of 1 means "+
			"the chain was rebuilt or bypassed after the first turn.", inner.calls, outer.calls)
	}
	if len(order) < 2 || order[0] != "registered-last" || order[1] != "registered-first" {
		t.Fatalf("entry order = %v, want the last-registered middleware to run first.\n"+
			"Chain composes so the LAST registered is OUTERMOST. A test that passes with "+
			"the order reversed means your logging, retry and auth layers are nested in "+
			"the opposite order to the one you designed.", order)
	}

	// The request the provider received is the one the chain rewrote.
	reqs := p.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	got, ok := reqs[0].Options.Headers["X-Trace-Id"]
	if !ok || got == nil || *got != "trace-123" {
		t.Fatalf("the provider received headers %v, want X-Trace-Id=trace-123.\n"+
			"Middleware that mutates a copy of the Request and drops it on the floor is "+
			"the silent failure here: the chain runs, nothing changes.", reqs[0].Options.Headers)
	}
}

// --------------------------------------------------------------------- 6 --
//
// Asserting the event sequence.

// TestTheEventSequenceIsTheOneAStreamingUIExpects collects the whole stream
// and asserts on its shape.
//
// This is the test to write if you render tokens as they arrive. Two rules
// carry it. Events come in pairs of kinds: incremental deltas, which may be
// coalesced or missing entirely, and one authoritative end event per item,
// which is complete and final. Every delta for an item precedes that item's
// end event, so a renderer that discards its accumulator on the end event
// never double-applies. If your UI shows doubled text, this test is where you
// will see why.
func TestTheEventSequenceIsTheOneAStreamingUIExpects(t *testing.T) {
	p := weatherScript()
	p.ChunkSize = 4 // force several deltas per block, as a real provider would
	a := newAgent(t, p, nil)
	if err := a.RegisterTool(weatherTool(nil)); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	stream, err := a.Stream(context.Background(), "What is the weather in Oslo?")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var (
		events []core.Event
		text   strings.Builder
	)
	for e := range stream.Events() {
		events = append(events, e)
		if d, ok := e.(core.TextDeltaEvent); ok {
			text.WriteString(d.Delta)
		}
	}
	if _, err := stream.RunResult(); err != nil {
		t.Fatalf("RunResult: %v", err)
	}

	// faux.EventNames renders the sequence as one comma-separated line, which
	// is what makes a failure here readable instead of a wall of structs.
	names := faux.EventNames(events)

	// Assert an ORDERED SUBSEQUENCE, not an exact match: a real provider may
	// coalesce deltas or add events, and a test pinned to the exact list
	// breaks on a change that harmed nobody.
	want := []core.EventType{
		core.EvAgentStart,
		core.EvTurnStart,
		core.EvMessageStart,
		core.EvTextStart,
		core.EvTextDelta,
		core.EvToolCallStart,  // the second block starts while the first is open
		core.EvToolInputDelta, // arguments arrive as text, and may be invalid JSON until the end
		core.EvTextEnd,        // authoritative: carries the WHOLE text, not the last delta
		core.EvToolCallEnd,    // authoritative: carries the parsed block
		core.EvMessageEnd,
		core.EvToolExecutionStart,
		core.EvToolResult,
		core.EvTurnEnd,
		core.EvAgentDone,
	}
	at := 0
	for _, w := range want {
		found := -1
		for i := at; i < len(events); i++ {
			if events[i].EventType() == w {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("event %q did not appear after position %d.\nfull sequence: %s\n"+
				"The order here is the contract a streaming consumer is written against; a "+
				"missing or out-of-order event means a UI built on it renders the turn wrong.",
				w, at, names)
		}
		at = found + 1
	}

	// The authoritative event replaces the accumulator; it does not extend it.
	var ends []core.TextEndEvent
	for _, e := range events {
		if te, ok := e.(core.TextEndEvent); ok {
			ends = append(ends, te)
		}
	}
	if len(ends) != 2 {
		t.Fatalf("got %d text_end events for a 2-turn run, want 2 — exactly one per text "+
			"block.\nfull sequence: %s", len(ends), names)
	}
	if ends[0].Text != "Let me look that up." {
		t.Fatalf("text_end carried %q, want the whole block.\n"+
			"The end event is authoritative and complete: a consumer that appends it to "+
			"its accumulated deltas instead of replacing them shows the text twice.",
			ends[0].Text)
	}
	if got := text.String(); got != "Let me look that up."+"It is 7C in Oslo." {
		t.Fatalf("accumulated deltas = %q, want the concatenation of both turns' text.\n"+
			"Deltas must reassemble exactly; a mismatch means chunks were dropped or "+
			"reordered.", got)
	}
}

// --------------------------------------------------------------------- 7 --
//
// Failure paths.

// TestAFailedOrAbortedTurnStillLeavesATerminalMessage pins the invariant every
// resume, replay and audit path depends on: a turn that STARTED always ends
// with a message in the transcript.
//
// Both subtests are deterministic — no sleeps, no timeouts. That matters more
// here than anywhere else, because a flaky failure-path test gets deleted and
// then the failure path is the one nobody tests.
func TestAFailedOrAbortedTurnStillLeavesATerminalMessage(t *testing.T) {
	t.Run("provider error mid-stream", func(t *testing.T) {
		// Turn.Err ends the stream with an error AFTER the scripted blocks
		// were produced: half a message, then a failure — which is what a
		// truncated SSE body actually looks like.
		p := faux.New(faux.Turn{
			Blocks: []core.ContentBlock{faux.FauxText("The temperature in Os")},
			Err:    errors.New("upstream 503: service unavailable"),
		})
		a := newAgent(t, p, nil)
		if err := a.RegisterTool(weatherTool(nil)); err != nil {
			t.Fatalf("RegisterTool: %v", err)
		}

		res, err := a.Run(context.Background(), "What is the weather in Oslo?")
		if err == nil {
			t.Fatal("Run returned no error for a failed turn; the caller has no way to " +
				"tell a broken run from a finished one")
		}
		if !strings.Contains(err.Error(), "503") {
			t.Fatalf("error = %v, want it to carry the provider's own message — a "+
				"rewritten error is one you cannot debug from a log", err)
		}

		last := terminalAssistant(t, res.Messages)
		if last.StopReason != core.StopReasonError {
			t.Fatalf("terminal stop reason = %q, want %q", last.StopReason, core.StopReasonError)
		}
		if !strings.Contains(last.ErrorMessage, "upstream 503") {
			t.Fatalf("terminal message carries error_message %q, want the provider's "+
				"failure.\nA terminal marker with no error text is indistinguishable from "+
				"a turn that simply stopped.", last.ErrorMessage)
		}
		if got := last.Content.Text(); got != "The temperature in Os" {
			t.Fatalf("partial content = %q, want the text produced before the failure.\n"+
				"History after a failure is recoverable, not clean: throwing the partial "+
				"away loses the only record of what was paid for.", got)
		}
	})

	t.Run("cancelled from inside a tool handler", func(t *testing.T) {
		// Cancelling from the handler is the deterministic way to test an
		// abort: the handler is guaranteed to have run before the next model
		// call, so there is no sleep and no race. It also models what actually
		// happens — the user hits Ctrl-C while a tool is working.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		p := weatherScript()
		a := newAgent(t, p, nil)
		cancelling := weatherTool(nil)
		inner := cancelling.Handler
		cancelling.Handler = func(c context.Context, in json.RawMessage) (json.RawMessage, error) {
			out, err := inner(c, in)
			cancel()
			return out, err
		}
		if err := a.RegisterTool(cancelling); err != nil {
			t.Fatalf("RegisterTool: %v", err)
		}

		res, err := a.Run(ctx, "What is the weather in Oslo?")
		if err == nil {
			t.Fatal("Run returned no error after the context was cancelled; a caller that " +
				"cannot see the abort will treat a half-finished run as an answer")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want it to wrap context.Canceled so errors.Is works", err)
		}
		if res.StopReason != core.RunStopAborted {
			t.Fatalf("run stop reason = %q, want %q.\n"+
				"An abort is not an error: the difference is what a caller uses to decide "+
				"between retrying and giving up.", res.StopReason, core.RunStopAborted)
		}

		// The abort landed BETWEEN turns, so the turn that started finished
		// normally and the transcript stops at a clean boundary. That is the
		// property that makes it resumable: every tool call has its answer.
		if got := p.Calls(); got != 1 {
			t.Fatalf("the provider was called %d times, want 1 — the cancelled run must not "+
				"start another model call", got)
		}
		answered := map[string]bool{}
		for _, m := range res.Messages {
			if tr, ok := m.(core.ToolResultMessage); ok {
				answered[tr.ToolUseID] = true
			}
		}
		for _, m := range res.Messages {
			am, ok := m.(core.AssistantMessage)
			if !ok {
				continue
			}
			for _, b := range am.Content {
				call, ok := b.(core.ToolUseBlock)
				if !ok {
					continue
				}
				if !answered[call.ID] {
					t.Fatalf("tool call %q (%s) has no result in the aborted transcript.\n"+
						"An unanswered tool call cannot be sent back to a provider, so the "+
						"session is not resumable — every call needs a result even when the "+
						"run is cut short.", call.ID, call.Name)
				}
			}
		}
	})
}

// terminalAssistant returns the last assistant message, failing the test if
// the transcript does not end with one. Every failure-path test wants this.
func terminalAssistant(t *testing.T, msgs core.Messages) core.AssistantMessage {
	t.Helper()
	for i := len(msgs) - 1; i >= 0; i-- {
		if am, ok := msgs[i].(core.AssistantMessage); ok {
			return am
		}
	}
	t.Fatalf("the transcript holds no assistant message at all (%d messages).\n"+
		"A turn that started always owes a terminal message; without one the transcript "+
		"cannot be resumed or replayed.", len(msgs))
	return core.AssistantMessage{}
}
