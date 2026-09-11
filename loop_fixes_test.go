package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agentfox/agentkit-go/compaction"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/stop"
)

// blocking is a provider that honours ctx: it holds the stream open until the
// context is cancelled and then resolves it to an ABORTED message, the way a
// real adapter does when the caller pulls the plug mid-body. The scripted
// double ignores ctx, which is why the original abort test could never
// observe an abort and always skipped.
type blocking struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func (b *blocking) provider() core.APIProvider {
	return core.APIProvider{API: testAPI, Stream: b.stream}
}

func (b *blocking) stream(ctx context.Context, m *core.Model, _ core.Request, _ core.ProviderStreamOptions) *core.EventStream {
	st := core.NewEventStream(core.StreamOptions{})
	b.mu.Lock()
	b.calls++
	first := b.calls == 1
	b.mu.Unlock()
	go func() {
		if first && b.started != nil {
			close(b.started)
		}
		<-ctx.Done()
		msg := core.AssistantMessage{
			Content:      core.Content{core.TextBlock{Text: "partial"}},
			StopReason:   core.StopReasonAborted,
			ErrorMessage: ctx.Err().Error(),
			Provider:     m.Provider, API: m.API, Model: m.ID,
		}
		st.Push(core.MessageEndEvent{Message: msg})
		st.End(core.StreamResult{Message: &msg})
	}()
	return st
}

// ------------------------------------------------------------------ REQ-GO-09

// TestAbortAndCallerCancellationAreDistinguishable: ErrAborted means
// Agent.Abort(); a cancelled caller ctx reports context.Canceled. Collapsing
// them makes a server timeout read as a user action.
func TestAbortAndCallerCancellationAreDistinguishable(t *testing.T) {
	t.Run("Agent.Abort", func(t *testing.T) {
		b := &blocking{started: make(chan struct{})}
		a := newTestAgent(t, nil, nil)
		a.cfg.Providers = core.ProviderRegistry{testAPI: b.provider()}
		go func() { <-b.started; a.Abort() }()
		res, err := a.Run(context.Background(), "go")
		if !errors.Is(err, core.ErrAborted) {
			t.Fatalf("err = %v, want ErrAborted", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Fatal("an Agent.Abort must not read as the caller's own cancellation")
		}
		if res.StopReason != core.RunStopAborted {
			t.Fatalf("StopReason = %q, want aborted", res.StopReason)
		}
		if !a.Idle() {
			t.Fatal("agent must be Idle after an aborted run")
		}
	})
	t.Run("caller ctx", func(t *testing.T) {
		b := &blocking{started: make(chan struct{})}
		a := newTestAgent(t, nil, nil)
		a.cfg.Providers = core.ProviderRegistry{testAPI: b.provider()}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-b.started; cancel() }()
		_, err := a.Run(ctx, "go")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if errors.Is(err, core.ErrAborted) {
			t.Fatal("a caller cancellation must not read as Agent.Abort (REQ-GO-09)")
		}
	})
}

// ------------------------------------------------------------------ REQ-LOOP-09

// TestAnAbortedMessageKeepsItsErrorMessage: REQ-LOOP-09 requires
// error_message SET on the aborted turn and forbids rewriting it at abort
// time. The loop used to clear it for every Agent.Abort.
func TestAnAbortedMessageKeepsItsErrorMessage(t *testing.T) {
	b := &blocking{started: make(chan struct{})}
	a := newTestAgent(t, nil, nil)
	a.cfg.Providers = core.ProviderRegistry{testAPI: b.provider()}
	go func() { <-b.started; a.Abort() }()
	res, _ := a.Run(context.Background(), "go")
	am := lastAssistant(res.Messages)
	if am == nil || am.StopReason != core.StopReasonAborted {
		t.Fatalf("no aborted assistant message in %v", res.Messages)
	}
	if am.ErrorMessage == "" {
		t.Fatal("the aborted message's error_message was cleared; REQ-LOOP-09 says it is set " +
			"and the message is appended verbatim")
	}
	if am.Content.Text() != "partial" {
		t.Fatalf("partial content %q was not kept", am.Content.Text())
	}
}

// ------------------------------------------------------------------ REQ-LOOP-16

// TestACancelledBatchEndsTheRunAtTheTurnBoundary: a cancellation that lands
// during a tool batch must not be followed by a provider call on a dead
// context. The transcript then ends in a tool_result — REQ-LOOP-16's
// "normal outcome of REQ-LOOP-09 cancellation" — and Continue accepts it.
func TestACancelledBatchEndsTheRunAtTheTurnBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", "cancels", `{}`)),
	}}
	a := newTestAgent(t, s, nil)
	_ = a.RegisterTool(core.Tool{
		Name: "cancels", Description: "cancels the run", InputSchema: schema.Object(),
		Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			cancel()
			return json.RawMessage(`{}`), nil
		},
	})
	res, err := a.Run(ctx, "go")
	if s.turnsRun() != 1 {
		t.Fatalf("provider was called %d times; the loop issued a request on a cancelled context", s.turnsRun())
	}
	if !errors.Is(err, context.Canceled) || res.StopReason != core.RunStopAborted {
		t.Fatalf("err=%v stop=%q; want context.Canceled / aborted", err, res.StopReason)
	}
	if role, _ := a.History().LastRole(); role != core.RoleToolResult {
		t.Fatalf("transcript ends in %q, want tool_result", role)
	}
	// And the resume path the requirement names actually works.
	if _, err := a.Continue(context.Background()); err != nil {
		t.Fatalf("Continue after a cancelled batch: %v", err)
	}
	if s.turnsRun() != 2 {
		t.Fatalf("Continue did not resume the loop: %d provider calls", s.turnsRun())
	}
}

// TestContinueAfterAnAbortedTurnIsAllowed: a trailing aborted assistant
// message is the REQ-LOOP-09 terminal marker, not a completed turn. Rule 2 of
// REQ-PROV-11 drops it from the outbound request, so the model still owes a
// reply and Continue must accept the transcript.
func TestContinueAfterAnAbortedTurnIsAllowed(t *testing.T) {
	b := &blocking{started: make(chan struct{})}
	a := newTestAgent(t, nil, nil)
	a.cfg.Providers = core.ProviderRegistry{testAPI: b.provider()}
	go func() { <-b.started; a.Abort() }()
	_, _ = a.Run(context.Background(), "go")
	if role, _ := a.History().LastRole(); role != core.RoleAssistant {
		t.Fatalf("setup: last role %q", role)
	}

	// Swap in a provider that completes, then Continue.
	s := &scripted{}
	a.cfg.Providers = core.ProviderRegistry{testAPI: s.provider()}
	if _, err := a.Continue(context.Background()); err != nil {
		t.Fatalf("Continue after an aborted turn: %v (REQ-LOOP-16)", err)
	}
	if s.turnsRun() != 1 {
		t.Fatal("Continue did not issue a request")
	}
}

// TestContinueWithOnlyAFollowUpQueuedDeliversItFirst: REQ-LOOP-16's
// assistant branch drains BOTH queues. A follow-up alone used to be delivered
// a turn late, after a request carrying the assistant-terminated transcript.
func TestContinueWithOnlyAFollowUpQueuedDeliversItFirst(t *testing.T) {
	s := &scripted{}
	a := newTestAgent(t, s, nil)
	if _, err := a.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := a.FollowUpText("second"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Continue(context.Background()); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	sent := s.sentAt(1)
	if sent == nil {
		t.Fatal("no second request")
	}
	last := sent[len(sent)-1]
	um, ok := last.(core.UserMessage)
	if !ok || um.Content.Text() != "second" {
		t.Fatalf("the first request after Continue ended in %v; want the follow-up %q", last, "second")
	}
	if s.turnsRun() != 2 {
		t.Fatalf("%d provider calls; the follow-up must be delivered in ONE request, not after a spurious one", s.turnsRun())
	}
}

// ------------------------------------------------------------------ NFR-REL-02

// TestPanicsInThirdPartyCodeDoNotCrashTheProcess covers the call sites that
// were bare: middleware, the context transform, the stop policy, a tool's
// argument shim, and the tracer. Each was confirmed to crash the test binary
// before the wrappers landed.
func TestPanicsInThirdPartyCodeDoNotCrashTheProcess(t *testing.T) {
	explode := func(what string) func(*core.AgentConfig) {
		return func(c *core.AgentConfig) {
			switch what {
			case "middleware":
				c.Middleware = []core.Middleware{func(core.Handler) core.Handler {
					return func(context.Context, core.Request) *core.EventStream { panic("mw exploded") }
				}}
			case "transform":
				c.TransformContext = func(context.Context, core.Messages) core.Messages { panic("tf exploded") }
			case "stoppolicy":
				c.StopPolicy = func(core.StopContext) bool { panic("policy exploded") }
			case "tracer":
				c.Tracer = panickingTracer{}
			}
		}
	}

	t.Run("middleware", func(t *testing.T) {
		var errs []string
		s := &scripted{}
		a := newTestAgent(t, s, func(c *core.AgentConfig) {
			explode("middleware")(c)
			c.Hooks.OnError = func(err error) { errs = append(errs, err.Error()) }
		})
		res, err := a.Run(context.Background(), "go")
		if err == nil || res.StopReason != core.RunStopError {
			t.Fatalf("err=%v stop=%q; a middleware panic must end the run as an error", err, res.StopReason)
		}
		am := lastAssistant(res.Messages)
		if am == nil || am.StopReason != core.StopReasonError {
			t.Fatal("the terminal marker of REQ-LOOP-09 is missing after a middleware panic")
		}
		if len(errs) == 0 || !strings.Contains(errs[0], "mw exploded") {
			t.Fatalf("OnError did not see the panic: %v", errs)
		}
	})
	t.Run("transform", func(t *testing.T) {
		s := &scripted{}
		a := newTestAgent(t, s, explode("transform"))
		if _, err := a.Run(context.Background(), "go"); err != nil {
			t.Fatalf("a panicking transform must be contained and the run continue on the raw view: %v", err)
		}
		if s.turnsRun() != 1 {
			t.Fatal("the request was not issued")
		}
	})
	t.Run("stoppolicy", func(t *testing.T) {
		s := &scripted{}
		a := newTestAgent(t, s, explode("stoppolicy"))
		res, err := a.Run(context.Background(), "go")
		if err != nil {
			t.Fatalf("a panicking stop policy stops the run cleanly, it does not error it: %v", err)
		}
		if res.StopReason != core.RunStopPolicy {
			t.Fatalf("StopReason = %q; a broken limit must fail CLOSED (ruling in consultStopPolicy)", res.StopReason)
		}
	})
	t.Run("prepare_arguments", func(t *testing.T) {
		s := oneToolTurn(t)
		a := newTestAgent(t, s, nil)
		_ = a.RegisterTool(core.Tool{
			Name: "echo", Description: "echo", InputSchema: schema.Object(schema.Opt("v", schema.String())),
			PrepareArguments: func(map[string]any) map[string]any { panic("shim exploded") },
			Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{}`), nil
			},
		})
		res, err := a.Run(context.Background(), "go")
		if err != nil {
			t.Fatal(err)
		}
		tr := findToolResult(t, res.Messages, "c1")
		if !tr.IsError || !strings.Contains(tr.Content.Text(), "panicked") {
			t.Fatalf("a panicking PrepareArguments must become an error tool result (REQ-TOOL-11): %v", tr.Content.Text())
		}
	})
	t.Run("tracer", func(t *testing.T) {
		var ran atomic.Int32
		s := oneToolTurn(t)
		a := newTestAgent(t, s, explode("tracer"))
		_ = a.RegisterTool(echoTool("echo", &ran))
		res, err := a.Run(context.Background(), "go")
		if err != nil {
			t.Fatal(err)
		}
		if ran.Load() != 1 {
			t.Fatalf("handler ran %d times; a panicking tracer must not eat the tool call", ran.Load())
		}
		if tr := findToolResult(t, res.Messages, "c1"); tr.IsError {
			t.Fatalf("tool result is an error under a broken tracer: %s", tr.Content.Text())
		}
	})
}

type panickingTracer struct{}

func (panickingTracer) StartSpan(string, func(core.Span) error) error { panic("tracer exploded") }

// ------------------------------------------------------------------ REQ-LOOP-11.3

// TestEveryCallOpensAndClosesExactlyOnce pins the execution event pairing
// for calls that never reach a handler: blocked, invalid, unknown, and
// aborted. The original test asserted results only, which is why an End
// without a Start went unnoticed.
func TestEveryCallOpensAndClosesExactlyOnce(t *testing.T) {
	count := func(evs []core.Event) (starts, ends map[string]int) {
		starts, ends = map[string]int{}, map[string]int{}
		for _, e := range evs {
			switch v := e.(type) {
			case core.ToolExecutionStartEvent:
				starts[v.ToolUseID]++
			case core.ToolExecutionEndEvent:
				ends[v.ToolUseID]++
			}
		}
		return
	}
	check := func(t *testing.T, evs []core.Event, ids ...string) {
		t.Helper()
		starts, ends := count(evs)
		for _, id := range ids {
			if starts[id] != 1 || ends[id] != 1 {
				t.Fatalf("call %s: %d start / %d end events, want 1/1 (REQ-LOOP-11.3)", id, starts[id], ends[id])
			}
		}
	}
	drain := func(st *core.EventStream) []core.Event {
		var evs []core.Event
		for e := range st.Events() {
			evs = append(evs, e)
		}
		return evs
	}

	t.Run("aborted", func(t *testing.T) {
		s := &scripted{turns: []core.AssistantMessage{
			assistantWithTools(core.StopReasonToolUse,
				toolUse(t, "c1", "echo", `{}`), toolUse(t, "c2", "echo", `{}`)),
		}}
		ctx, cancel := context.WithCancel(context.Background())
		a := newTestAgent(t, s, nil)
		_ = a.RegisterTool(echoTool("echo", nil))
		cancel()
		st, err := a.Stream(ctx, "go")
		if err != nil {
			t.Fatal(err)
		}
		check(t, drain(st), "c1", "c2")
	})
	t.Run("blocked, invalid, unknown", func(t *testing.T) {
		s := &scripted{turns: []core.AssistantMessage{
			assistantWithTools(core.StopReasonToolUse,
				toolUse(t, "b1", "echo", `{}`),
				toolUse(t, "b2", "echo", `{"v":{"not":"a string"}}`),
				toolUse(t, "b3", "nope", `{}`)),
		}}
		a := newTestAgent(t, s, func(c *core.AgentConfig) {
			c.BeforeToolCall = func(_ context.Context, in core.BeforeToolCallContext) core.BeforeToolCallDecision {
				return core.BeforeToolCallDecision{Block: in.ToolUseID == "b1", Reason: "no"}
			}
		})
		_ = a.RegisterTool(echoTool("echo", nil))
		st, err := a.Stream(context.Background(), "go")
		if err != nil {
			t.Fatal(err)
		}
		check(t, drain(st), "b1", "b2", "b3")
	})
}

// ------------------------------------------------------------------ REQ-OBS-06

// TestTurnEndToolResultsAreNonNilForHooksToo: "always non-nil; [] for a
// no-tool turn" must hold for the OnTurnEnd hook and StopContext, not only
// for the stream copy that clone() rebuilds.
func TestTurnEndToolResultsAreNonNilForHooksToo(t *testing.T) {
	var hookNil, policyNil bool
	s := &scripted{}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Hooks.OnTurnEnd = func(e core.TurnEndEvent) { hookNil = e.ToolResults == nil }
		c.StopPolicy = func(sc core.StopContext) bool { policyNil = sc.ToolResults == nil; return false }
	})
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if hookNil || policyNil {
		t.Fatalf("ToolResults nil: hook=%v policy=%v (REQ-OBS-06)", hookNil, policyNil)
	}
}

// TestDeferredResponseStillEmitsTurnEnd: a turn that started owes its
// TurnEndEvent whichever way it ended.
func TestDeferredResponseStillEmitsTurnEnd(t *testing.T) {
	s := &scripted{turns: []core.AssistantMessage{{StopReason: core.StopReasonDeferred}}}
	a := newTestAgent(t, s, nil)
	st, err := a.Stream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	starts, ends := 0, 0
	for e := range st.Events() {
		switch e.(type) {
		case core.TurnStartEvent:
			starts++
		case core.TurnEndEvent:
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("turn events: %d start / %d end; a deferred turn must close", starts, ends)
	}
	if _, err := st.RunResult(); !errors.Is(err, core.ErrDeferredUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

// ------------------------------------------------------------------ REQ-LIFE-02

// TestSnapshotRevisionMatchesItsMessages: Messages and Revision are read
// under ONE lock, so a concurrent append cannot land between them.
func TestSnapshotRevisionMatchesItsMessages(t *testing.T) {
	h := core.NewConversationHistory()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.Record(core.NullLeaf, user("m"))
		}
	}()
	for i := 0; i < 2000; i++ {
		msgs, rev := h.SnapshotBranch()
		if uint64(len(msgs)) != rev {
			// Every Record appends exactly one message and bumps the revision
			// by one, so a consistent read has them equal.
			close(stop)
			wg.Wait()
			t.Fatalf("snapshot has %d messages at revision %d", len(msgs), rev)
		}
	}
	close(stop)
	wg.Wait()
}

// ------------------------------------------------------------------ REQ-LIFE-03

// TestSetModelDuringARunDoesNotRace is a race-detector test: SetModel writes
// cfg.Model under the lock while the run reads it for its start event.
func TestSetModelDuringARunDoesNotRace(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := &scripted{}
		a := newTestAgent(t, s, nil)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = a.SetModel(&core.Model{ID: "other", API: testAPI, Provider: "test", ContextWindow: 1000, MaxTokens: 10})
		}()
		_, _ = a.Run(context.Background(), "go")
		<-done
	}
}

// ------------------------------------------------------------------ REQ-OBS-03

type sessionHook struct {
	votingHook
	starts, ends *atomic.Int32
}

func (h *sessionHook) OnSessionStart(core.AuditEvent) { h.starts.Add(1) }
func (h *sessionHook) OnSessionEnd(core.AuditEvent)   { h.ends.Add(1) }

// TestPluginSessionHooksFire: REQ-OBS-03 names EventHookPlugin explicitly,
// and the registry's hooks never received session boundaries at all.
func TestPluginSessionHooksFire(t *testing.T) {
	var starts, ends atomic.Int32
	h := &sessionHook{votingHook: votingHook{name: "obs"}, starts: &starts, ends: &ends}
	s := &scripted{}
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.Plugins = registryWith(h) })
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || ends.Load() != 1 {
		t.Fatalf("plugin saw %d session starts and %d ends, want 1/1 (REQ-OBS-03)", starts.Load(), ends.Load())
	}
}

// ------------------------------------------------------------------ OQ-8

// TestAShellToolWithNoInterceptorFailsTheRun: construction of a run fails
// loudly when execute is registered and nothing guards it.
func TestAShellToolWithNoInterceptorFailsTheRun(t *testing.T) {
	shell := func(name string) core.Tool {
		return core.Tool{Name: name, Description: "shell", InputSchema: schema.Object(schema.Prop("command", schema.String())),
			Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }}
	}
	for _, name := range ShellToolNames {
		s := &scripted{}
		a := newTestAgent(t, s, nil)
		_ = a.RegisterTool(shell(name))
		if _, err := a.Run(context.Background(), "go"); !errors.Is(err, core.ErrUnguardedExecute) {
			t.Fatalf("%s: err = %v, want ErrUnguardedExecute", name, err)
		}
		if s.turnsRun() != 0 {
			t.Fatal("a request was issued before the guard fired")
		}
	}
	// The explicit opt-out is an interceptor, so passing it is an act.
	s := &scripted{}
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.BeforeToolCall = AllowAllToolCalls })
	_ = a.RegisterTool(shell("execute"))
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatalf("AllowAllToolCalls: %v", err)
	}
	// And a policy that excludes the shell tool from the run needs no guard.
	s = &scripted{}
	a = newTestAgent(t, s, func(c *core.AgentConfig) { c.ToolPolicy.ExcludeTools = []string{"execute"} })
	_ = a.RegisterTool(shell("execute"))
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatalf("excluded shell tool: %v", err)
	}
}

// TestRestrictedPolicy pins the reference interceptor's decisions.
func TestRestrictedPolicy(t *testing.T) {
	p := RestrictedPolicy(RestrictedOptions{AllowedPrograms: []string{"go", "/usr/bin/git"}})
	call := func(tool string, args map[string]any) core.BeforeToolCallDecision {
		return p(context.Background(), core.BeforeToolCallContext{ToolName: tool, Arguments: args})
	}
	cases := []struct {
		tool  string
		args  map[string]any
		block bool
		why   string
	}{
		{"execute", map[string]any{"command": "go test ./..."}, false, "allowed program"},
		{"execute", map[string]any{"command": "GOFLAGS=-mod=mod go build"}, false, "env assignment prefix"},
		{"execute", map[string]any{"command": "git log 'a;b'"}, false, "operator inside single quotes"},
		{"execute", map[string]any{"command": "go test | tee out"}, true, "pipe"},
		{"execute", map[string]any{"command": "go test; rm -rf /"}, true, "list operator"},
		{"execute", map[string]any{"command": "echo $(whoami)"}, true, "command substitution"},
		{"execute", map[string]any{"command": "git log \"$HOME\""}, true, "expansion inside double quotes"},
		{"execute", map[string]any{"command": "rm -rf /"}, true, "program not allowed"},
		{"execute", map[string]any{"command": ""}, true, "empty"},
		{"run_command", map[string]any{"argv": []any{"go", "vet", "a;b"}}, false, "argv is not re-parsed"},
		{"run_command", map[string]any{"argv": []any{"curl", "x"}}, true, "argv program not allowed"},
		{"powershell", map[string]any{"command": "Get-ChildItem"}, true, "no PowerShell grammar: refused outright"},
		{"read_file", map[string]any{"path": "x"}, false, "non-shell tools pass"},
	}
	for _, c := range cases {
		if got := call(c.tool, c.args).Block; got != c.block {
			t.Errorf("%s %v: block=%v, want %v (%s)", c.tool, c.args, got, c.block, c.why)
		}
	}
	if !call("execute", map[string]any{"command": "go test | tee"}).Block {
		t.Fatal("pipe")
	}
	loose := RestrictedPolicy(RestrictedOptions{AllowedPrograms: []string{"go"}, AllowShellOperators: true})
	if loose(context.Background(), core.BeforeToolCallContext{ToolName: "execute",
		Arguments: map[string]any{"command": "go test | tee"}}).Block {
		t.Fatal("AllowShellOperators must permit the pipe")
	}
	term := RestrictedPolicy(RestrictedOptions{TerminateOnBlock: true})
	if d := term(context.Background(), core.BeforeToolCallContext{ToolName: "execute",
		Arguments: map[string]any{"command": "ls"}}); !d.Block || !d.Terminate {
		t.Fatal("TerminateOnBlock must cast the REQ-TOOL-13.2 vote")
	}
}

// ------------------------------------------------------------------ REQ-MULTI-05

// TestNamedSpecialistsAreInvokableByName: a registered definition becomes a
// tool the parent model can call, and every call gets a fresh child scoped
// by the definition's ToolPolicy.
func TestNamedSpecialistsAreInvokableByName(t *testing.T) {
	// One scripted double serves parent and children alike (the child
	// inherits the parent's providers): the first call is the parent's
	// delegating turn, and every call after it answers "done".
	prov := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "d1", "reviewer", `{"prompt":"look at x"}`),
			toolUse(t, "d2", "reviewer", `{"prompt":"look at y"}`)),
	}}
	reg := core.ProviderRegistry{testAPI: prov.provider()}
	parent, err := NewAgent(core.AgentConfig{Model: testModel(), Providers: reg, StopPolicy: stop.AfterTurns(5), ParallelTools: true})
	if err != nil {
		t.Fatal(err)
	}

	specialists := NewAgentRegistry()
	if err := specialists.Register(AgentDefinition{
		Name: "reviewer", Description: "reviews code", SystemPrompt: "You review.",
		ToolPolicy: core.ToolPolicy{ToolNames: []string{"read_file"}},
		StopPolicy: stop.AfterTurns(2),
	}); err != nil {
		t.Fatal(err)
	}
	if err := specialists.Register(AgentDefinition{Name: "reviewer"}); err == nil {
		t.Fatal("a duplicate name must be refused")
	}
	for _, tool := range specialists.Tools(parent, 0) {
		if err := parent.RegisterTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	res, err := parent.Run(context.Background(), "review both")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"d1", "d2"} {
		tr := findToolResult(t, res.Messages, id)
		if tr.IsError {
			t.Fatalf("%s: %s", id, tr.Content.Text())
		}
	}
	child, err := NewAgentFromDefinition(parent, mustLookup(t, specialists, "reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	if child.History().Len() != 0 {
		t.Fatal("a child must start with empty history (REQ-MULTI-02)")
	}
	if names := child.Tools(); len(names) != 0 {
		t.Fatalf("the child's tool policy allowlists read_file only; got %d tools", len(names))
	}
	if child.ResolvedModel() != parent.ResolvedModel() {
		t.Fatal("a definition with no model inherits the parent's")
	}
}

func mustLookup(t *testing.T, r *AgentRegistry, name string) AgentDefinition {
	t.Helper()
	d, ok := r.Lookup(name)
	if !ok {
		t.Fatalf("no specialist %q", name)
	}
	return d
}

// ------------------------------------------------------------------ REQ-GO-14

// TestACutInsideATurnSummarizesTheTurnSeparately pins the split: when the
// boundary lands on an assistant message, the completed turns and the
// interrupted turn are summarized separately and joined with the fixed
// separator.
func TestACutInsideATurnSummarizesTheTurnSeparately(t *testing.T) {
	h := core.NewConversationHistory()
	msgs := core.Messages{
		user("q1"), assistantSaying("a1", 0),
		user("q2"), assistantSaying("a2 "+strings.Repeat("x", 4000), 0),
		user("q3"), assistantSaying("a3", 0),
	}
	var mainSeen, turnSeen core.Messages
	tf := compaction.NewContextTransform(compaction.Deps{
		Strategy: cutAt{3}, // lands on assistant a2: inside turn q2
		Summarizer: func(_ context.Context, prefix core.Messages, _ string) (string, error) {
			mainSeen = prefix
			return "HEAD", nil
		},
		TurnSummarizer: func(_ context.Context, prefix core.Messages, _ string) (string, error) {
			turnSeen = prefix
			return "TURN", nil
		},
		History: h, Model: testModel(),
	})
	view := tf(context.Background(), msgs)
	if len(mainSeen) != 2 || len(turnSeen) != 1 {
		t.Fatalf("main summarizer saw %d messages, turn summarizer %d; want 2 (q1,a1) and 1 (q2)", len(mainSeen), len(turnSeen))
	}
	want := compaction.SummaryPrefix + "HEAD" + compaction.SplitSeparator + "TURN"
	if got := view[0].(core.UserMessage).Content.Text(); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if len(view) != 1+3 {
		t.Fatalf("view has %d messages, want the summary plus the kept tail of 3", len(view))
	}
}

type cutAt struct{ at int }

func (cutAt) ShouldCompact(int, int) bool       { return true }
func (c cutAt) CutIndex(core.Messages, int) int { return c.at }
func (cutAt) CutPolicy() compaction.CutPolicy   { return compaction.CutNotToolResult }
