package subagent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/internal/testkit"
	"github.com/agentfox/agentkit-go/stop"
)

func newTestAgent(t *testing.T, s *testkit.Scripted, mutate func(*core.AgentConfig)) *agentkit.Agent {
	t.Helper()
	cfg := core.AgentConfig{
		Model:      testkit.TestModel(),
		StopPolicy: stop.AfterTurns(10),
		Providers:  core.ProviderRegistry{testkit.TestAPI: s.Provider()},
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

func findToolResult(t *testing.T, msgs core.Messages, id string) core.ToolResultMessage {
	t.Helper()
	for _, m := range msgs {
		if r, ok := m.(core.ToolResultMessage); ok && r.ToolUseID == id {
			return r
		}
	}
	t.Fatalf("no tool result for %q in %d messages", id, len(msgs))
	return core.ToolResultMessage{}
}

// TestSubagentGetsFreshHistory pins REQ-MULTI-02. Sharing the parent's
// transcript is a prompt-injection surface and inflates the child's input by
// the whole parent conversation.
func TestSubagentGetsFreshHistory(t *testing.T) {
	childProv := &testkit.Scripted{Turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "child says hi"}}, StopReason: core.StopReasonStop},
	}}
	parentProv := &testkit.Scripted{Turns: []core.AssistantMessage{
		testkit.AssistantWithTools(core.StopReasonToolUse,
			testkit.ToolUse(t, "c1", "specialist", `{"prompt":"do the thing"}`)),
		{Content: core.Content{core.TextBlock{Text: "parent done"}}, StopReason: core.StopReasonStop},
	}}

	parent := newTestAgent(t, parentProv, nil)
	factory := func(ctx context.Context) (*agentkit.Agent, error) {
		return agentkit.NewAgent(core.AgentConfig{
			Model:      testkit.TestModel(),
			StopPolicy: stop.AfterTurns(3),
			Providers:  core.ProviderRegistry{testkit.TestAPI: childProv.Provider()},
		})
	}
	if err := parent.RegisterTool(Tool(parent, factory,
		Options{Name: "specialist"})); err != nil {
		t.Fatal(err)
	}

	res, err := parent.Run(context.Background(), "delegate please")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.FinalText(), "parent done") {
		t.Fatalf("parent did not finish: %q", res.FinalText())
	}

	// The child's request must contain ONLY its own prompt.
	childSent := childProv.SentAt(0)
	if len(childSent) != 1 {
		t.Fatalf("the child saw %d messages, want exactly 1 (its own prompt).\n"+
			"A child always starts with fresh, empty history (REQ-MULTI-02).", len(childSent))
	}
	if got := childSent[0].(core.UserMessage).Content.Text(); got != "do the thing" {
		t.Fatalf("child prompt = %q, want %q", got, "do the thing")
	}
}

// TestParallelDelegationDoesNotHitTheRunSlot is the reason Tool takes
// a FACTORY rather than an *agentkit.Agent.
//
// A single shared child value looks correct and fails under exactly the
// condition delegation exists for: two parallel calls to the same specialist,
// where the second finds the run slot taken and returns ErrBusy.
func TestParallelDelegationDoesNotHitTheRunSlot(t *testing.T) {
	var childRuns atomic.Int32
	parentProv := &testkit.Scripted{Turns: []core.AssistantMessage{
		testkit.AssistantWithTools(core.StopReasonToolUse,
			testkit.ToolUse(t, "c1", "specialist", `{"prompt":"task one"}`),
			testkit.ToolUse(t, "c2", "specialist", `{"prompt":"task two"}`),
			testkit.ToolUse(t, "c3", "specialist", `{"prompt":"task three"}`)),
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}

	parent := newTestAgent(t, parentProv, func(c *core.AgentConfig) { c.ParallelTools = true })
	factory := func(ctx context.Context) (*agentkit.Agent, error) {
		childRuns.Add(1)
		p := &testkit.Scripted{Turns: []core.AssistantMessage{
			{Content: core.Content{core.TextBlock{Text: "child result"}},
				StopReason: core.StopReasonStop},
		}}
		return agentkit.NewAgent(core.AgentConfig{
			Model:      testkit.TestModel(),
			StopPolicy: stop.AfterTurns(3),
			Providers:  core.ProviderRegistry{testkit.TestAPI: p.Provider()},
		})
	}
	if err := parent.RegisterTool(Tool(parent, factory,
		Options{Name: "specialist"})); err != nil {
		t.Fatal(err)
	}

	res, err := parent.Run(context.Background(), "delegate three things")
	if err != nil {
		t.Fatal(err)
	}
	if got := childRuns.Load(); got != 3 {
		t.Fatalf("the factory produced %d children for 3 parallel calls, want 3", got)
	}
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok && tr.IsError {
			t.Fatalf("a parallel delegation failed: %s\n"+
				"A shared child agent would return ErrBusy here; the factory exists so "+
				"each child is an independent value (REQ-MULTI-04).", tr.Content.Text())
		}
	}
}

// TestSubagentRejectsAPrePopulatedChild: a factory that defeats REQ-MULTI-02
// must fail loudly rather than leak the parent's transcript.
func TestSubagentRejectsAPrePopulatedChild(t *testing.T) {
	parentProv := &testkit.Scripted{Turns: []core.AssistantMessage{
		testkit.AssistantWithTools(core.StopReasonToolUse,
			testkit.ToolUse(t, "c1", "specialist", `{"prompt":"go"}`)),
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}
	parent := newTestAgent(t, parentProv, nil)

	factory := func(ctx context.Context) (*agentkit.Agent, error) {
		h := core.NewConversationHistory()
		h.Record(core.NullLeaf, core.UserMessage{
			Content: core.Content{core.TextBlock{Text: "leaked parent context"}}})
		return agentkit.NewAgentWithHistory(core.AgentConfig{
			Model: testkit.TestModel(), Providers: core.ProviderRegistry{testkit.TestAPI: parentProv.Provider()},
		}, h)
	}
	_ = parent.RegisterTool(Tool(parent, factory, Options{Name: "specialist"}))

	res, err := parent.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok && tr.IsError &&
			strings.Contains(tr.Content.Text(), "history") {
			found = true
		}
	}
	if !found {
		t.Fatal("a factory returning a pre-populated agent must be rejected, not silently " +
			"allowed to carry the parent's transcript into the child")
	}
}

// TestBudgetIsPropagatedAsConfigNotContext pins REQ-MULTI-03: the child's
// budget is an explicit config field, never a context.Context value. A budget
// smuggled through ctx is invisible to the type system and silently absent
// whenever a caller passes a bare context.Background().
func TestBudgetIsPropagatedAsConfigNotContext(t *testing.T) {
	parentProv := &testkit.Scripted{Turns: []core.AssistantMessage{
		testkit.AssistantWithTools(core.StopReasonToolUse,
			testkit.ToolUse(t, "c1", "specialist", `{"prompt":"go"}`)),
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}
	parent := newTestAgent(t, parentProv, nil)

	var child *agentkit.Agent
	factory := func(ctx context.Context) (*agentkit.Agent, error) {
		p := &testkit.Scripted{Turns: []core.AssistantMessage{
			{Content: core.Content{core.TextBlock{Text: "child"}}, StopReason: core.StopReasonStop},
		}}
		a, err := agentkit.NewAgent(core.AgentConfig{
			Model:     testkit.TestModel(),
			Providers: core.ProviderRegistry{testkit.TestAPI: p.Provider()},
			// Deliberately NO StopPolicy: if one appears, Tool put it
			// there.
		})
		child = a
		return a, err
	}
	_ = parent.RegisterTool(Tool(parent, factory, Options{
		Name: "specialist", BudgetFraction: 0.3, MaxBudgetUSD: 1.0,
	}))
	if _, err := parent.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if child == nil {
		t.Fatal("the factory never ran")
	}
	got := child.Config().StopPolicy
	if got == nil {
		t.Fatal("Tool did not compose a budget policy onto the child; the budget " +
			"must reach the child as an explicit config field (REQ-MULTI-03)")
	}
}

// TestDelegationRefusesWhenTheParentBudgetIsSpent: the fraction is of the
// parent's REMAINING budget, so an exhausted parent delegates nothing.
func TestDelegationRefusesWhenTheParentBudgetIsSpent(t *testing.T) {
	// The delegating turn itself reports a cost past the whole budget, so by
	// the time the tool batch runs the parent has nothing left to grant.
	spent := testkit.AssistantWithTools(core.StopReasonToolUse,
		testkit.ToolUse(t, "c1", "specialist", `{"prompt":"go"}`))
	spent.Usage.SetCost(5.0)
	parentProv := &testkit.Scripted{Turns: []core.AssistantMessage{
		spent,
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}
	parent := newTestAgent(t, parentProv, nil)

	called := false
	factory := func(ctx context.Context) (*agentkit.Agent, error) {
		called = true
		p := &testkit.Scripted{}
		return agentkit.NewAgent(core.AgentConfig{Model: testkit.TestModel(),
			Providers: core.ProviderRegistry{testkit.TestAPI: p.Provider()}})
	}
	_ = parent.RegisterTool(Tool(parent, factory, Options{
		Name: "specialist", BudgetFraction: 0.3, MaxBudgetUSD: 1.0,
	}))
	res, err := parent.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	_ = called
	found := false
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok && tr.IsError &&
			strings.Contains(tr.Content.Text(), "budget") {
			found = true
		}
	}
	if !found {
		t.Fatal("delegating with no remaining parent budget must be refused, not granted " +
			"a negative slice")
	}
}

func TestRunParallelPreservesInputOrder(t *testing.T) {
	items := []int{5, 1, 3}
	got, errs := RunParallel(context.Background(), items,
		func(ctx context.Context, n int) (int, error) {
			time.Sleep(time.Duration(n) * 10 * time.Millisecond)
			return n * 2, nil
		})
	for _, e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if got[0] != 10 || got[1] != 2 || got[2] != 6 {
		t.Fatalf("results = %v, want [10 2 6] in INPUT order, not completion order", got)
	}
}

func TestRunParallelSurvivesAPanickingTask(t *testing.T) {
	_, errs := RunParallel(context.Background(), []int{1, 2},
		func(ctx context.Context, n int) (int, error) {
			if n == 1 {
				panic("boom")
			}
			return n, nil
		})
	if errs[0] == nil || !strings.Contains(errs[0].Error(), "panic") {
		t.Fatalf("a panicking task must become an error, got %v", errs[0])
	}
}

// TestNamedSpecialistsAreInvokableByName: a registered definition becomes a
// tool the parent model can call, and every call gets a fresh child scoped
// by the definition's ToolPolicy.
func TestNamedSpecialistsAreInvokableByName(t *testing.T) {
	// One scripted double serves parent and children alike (the child
	// inherits the parent's providers): the first call is the parent's
	// delegating turn, and every call after it answers "done".
	prov := &testkit.Scripted{Turns: []core.AssistantMessage{
		testkit.AssistantWithTools(core.StopReasonToolUse,
			testkit.ToolUse(t, "d1", "reviewer", `{"prompt":"look at x"}`),
			testkit.ToolUse(t, "d2", "reviewer", `{"prompt":"look at y"}`)),
	}}
	reg := core.ProviderRegistry{testkit.TestAPI: prov.Provider()}
	parent, err := agentkit.NewAgent(core.AgentConfig{Model: testkit.TestModel(), Providers: reg, StopPolicy: stop.AfterTurns(5), ParallelTools: true})
	if err != nil {
		t.Fatal(err)
	}

	specialists := NewRegistry()
	if err := specialists.Register(Definition{
		Name: "reviewer", Description: "reviews code", SystemPrompt: "You review.",
		ToolPolicy: core.ToolPolicy{ToolNames: []string{"read_file"}},
		StopPolicy: stop.AfterTurns(2),
	}); err != nil {
		t.Fatal(err)
	}
	if err := specialists.Register(Definition{Name: "reviewer"}); err == nil {
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
	child, err := FromDefinition(parent, mustLookup(t, specialists, "reviewer"))
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

func mustLookup(t *testing.T, r *Registry, name string) Definition {
	t.Helper()
	d, ok := r.Lookup(name)
	if !ok {
		t.Fatalf("no specialist %q", name)
	}
	return d
}
