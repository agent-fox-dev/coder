package agentkit

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/session"
)

func openTestSession(t *testing.T, path string) (*session.Store, *session.Resume) {
	t.Helper()
	store, r, err := OpenSession(path, session.Options{Durability: session.DurabilityPerEntry})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	return store, r
}

// TestARunIsPersistedAsItHappens is the end-to-end gap this closes.
//
// Both halves existed and were tested independently: session had a complete
// JSONL store with its own suite, and the loop had its own. Nothing connected
// them, so every message lived in memory and the log stayed empty — durable
// sessions were built, tested, and did not work.
func TestARunIsPersistedAsItHappens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store, _ := openTestSession(t, path)
	defer store.Close()

	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", "echo", `{"v":"x"}`)),
		{Content: core.Content{core.TextBlock{Text: "all done"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.SessionStore = store })
	if err := a.RegisterTool(echoTool("echo", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	entries := store.Entries()
	var kinds []string
	for _, e := range entries {
		if e.Type == core.EntryMessage && e.Message != nil {
			kinds = append(kinds, string(e.Message.Message.Role()))
		}
	}
	want := "user,assistant,tool_result,assistant"
	if got := strings.Join(kinds, ","); got != want {
		t.Fatalf("persisted roles = %q, want %q.\nThe loop must write to the session log "+
			"as the run happens; a message in history that never reached the log is "+
			"exactly the state that loses the last turn on resume.", got, want)
	}
}

// TestKillAndResume is the whole point of the session layer: a process dies
// mid-run and the next one picks up the transcript.
func TestKillAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")

	// ---- Process 1: runs one turn, then "dies" (we just stop and close).
	store1, _ := openTestSession(t, path)
	s1 := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "first answer"}}, StopReason: core.StopReasonStop},
	}}
	a1 := newTestAgent(t, s1, func(c *core.AgentConfig) { c.SessionStore = store1 })
	if _, err := a1.Run(context.Background(), "first question"); err != nil {
		t.Fatal(err)
	}
	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}

	// ---- Process 2: reopens, folds, and continues.
	store2, resume := openTestSession(t, path)
	defer store2.Close()

	if len(resume.Messages) != 2 {
		t.Fatalf("folded %d messages, want 2 (the user turn and the assistant reply)",
			len(resume.Messages))
	}
	s2 := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "second answer"}}, StopReason: core.StopReasonStop},
	}}
	cfg := core.AgentConfig{
		Model:        testModel(),
		StopPolicy:   StopAfterTurns(5),
		Providers:    core.ProviderRegistry{testAPI: s2.provider()},
		SessionStore: store2,
	}
	a2, err := NewAgentFromSession(cfg, resume,
		func(provider string, api core.API, modelID string) (*core.Model, error) {
			return testModel(), nil
		})
	if err != nil {
		t.Fatalf("NewAgentFromSession: %v", err)
	}

	if _, err := a2.Run(context.Background(), "second question"); err != nil {
		t.Fatal(err)
	}

	// The second process's provider must have SEEN the first process's turns.
	sent := s2.sentAt(0)
	if len(sent) != 3 {
		t.Fatalf("the resumed request carried %d messages, want 3 "+
			"(two recovered plus the new question); the transcript did not survive", len(sent))
	}
	if !strings.Contains(sent[1].(core.AssistantMessage).Content.Text(), "first answer") {
		t.Fatal("the recovered transcript is missing the first process's answer")
	}

	// And the log now holds both processes' turns, in order.
	var texts []string
	for _, e := range store2.Entries() {
		if e.Type == core.EntryMessage && e.Message != nil {
			texts = append(texts, contentTextOf(e.Message.Message))
		}
	}
	joined := strings.Join(texts, "|")
	for _, want := range []string{"first question", "first answer", "second question", "second answer"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the resumed log is missing %q; got %q", want, joined)
		}
	}
}

func contentTextOf(m core.Message) string {
	switch v := m.(type) {
	case core.UserMessage:
		return v.Content.Text()
	case core.AssistantMessage:
		return v.Content.Text()
	case core.ToolResultMessage:
		return v.Content.Text()
	}
	return ""
}

// TestResumeRecoversTheModelTripleAndRejectsAMismatch pins P-4 at the
// construction boundary: replaying a transcript as though a different model
// produced it is what strips its reasoning.
func TestResumeRecoversTheModelTripleAndRejectsAMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store, _ := openTestSession(t, path)

	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) { c.SessionStore = store })
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	store2, resume := openTestSession(t, path)
	defer store2.Close()

	if resume.ModelID != "test-model" || resume.API != testAPI || resume.Provider != "test" {
		t.Fatalf("fold recovered (%q, %q, %q), want the full triple",
			resume.Provider, resume.API, resume.ModelID)
	}

	// No resolver and a mismatched cfg.Model must fail loudly rather than
	// silently replay under the wrong provenance.
	other := &core.Model{ID: "a-different-model", API: testAPI, Provider: "test"}
	_, err := NewAgentFromSession(core.AgentConfig{
		Model: other, Providers: core.ProviderRegistry{testAPI: s.provider()},
	}, resume, nil)
	if err == nil {
		t.Fatal("resuming under a different model with no resolver must be an error: " +
			"REQ-PROV-11 rule 1 computes same_model over the recovered triple, and a " +
			"mismatch silently downgrades every signed thinking block")
	}
}

// TestPersistErrorsAreSurfacedNotSwallowed pins REQ-SESS-08 through the loop.
func TestPersistErrorsAreSurfacedNotSwallowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store, _ := openTestSession(t, path)
	// Closing the store makes every later Append fail.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var reported atomic.Int32
	s := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "ok"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.SessionStore = store
		c.Hooks.OnError = func(error) { reported.Add(1) }
	})
	// The run must still complete: losing the log is bad, losing the turn in
	// flight is worse.
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatalf("a persistence failure must not abort the run: %v", err)
	}
	if reported.Load() == 0 {
		t.Fatal("a persistence failure must be surfaced; silent failure is prohibited " +
			"for an embeddable library (REQ-SESS-08)")
	}
}

// ------------------------------------------------------------------ subagent

// TestSubagentGetsFreshHistory pins REQ-MULTI-02. Sharing the parent's
// transcript is a prompt-injection surface and inflates the child's input by
// the whole parent conversation.
func TestSubagentGetsFreshHistory(t *testing.T) {
	childProv := &scripted{turns: []core.AssistantMessage{
		{Content: core.Content{core.TextBlock{Text: "child says hi"}}, StopReason: core.StopReasonStop},
	}}
	parentProv := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "specialist", `{"prompt":"do the thing"}`)),
		{Content: core.Content{core.TextBlock{Text: "parent done"}}, StopReason: core.StopReasonStop},
	}}

	parent := newTestAgent(t, parentProv, nil)
	factory := func(ctx context.Context) (*Agent, error) {
		return NewAgent(core.AgentConfig{
			Model:      testModel(),
			StopPolicy: StopAfterTurns(3),
			Providers:  core.ProviderRegistry{testAPI: childProv.provider()},
		})
	}
	if err := parent.RegisterTool(SubagentTool(parent, factory,
		SubagentOptions{Name: "specialist"})); err != nil {
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
	childSent := childProv.sentAt(0)
	if len(childSent) != 1 {
		t.Fatalf("the child saw %d messages, want exactly 1 (its own prompt).\n"+
			"A child always starts with fresh, empty history (REQ-MULTI-02).", len(childSent))
	}
	if got := childSent[0].(core.UserMessage).Content.Text(); got != "do the thing" {
		t.Fatalf("child prompt = %q, want %q", got, "do the thing")
	}
}

// TestParallelDelegationDoesNotHitTheRunSlot is the reason SubagentTool takes
// a FACTORY rather than an *Agent.
//
// A single shared child value looks correct and fails under exactly the
// condition delegation exists for: two parallel calls to the same specialist,
// where the second finds the run slot taken and returns ErrBusy.
func TestParallelDelegationDoesNotHitTheRunSlot(t *testing.T) {
	var childRuns atomic.Int32
	parentProv := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "specialist", `{"prompt":"task one"}`),
			toolUse(t, "c2", "specialist", `{"prompt":"task two"}`),
			toolUse(t, "c3", "specialist", `{"prompt":"task three"}`)),
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}

	parent := newTestAgent(t, parentProv, func(c *core.AgentConfig) { c.ParallelTools = true })
	factory := func(ctx context.Context) (*Agent, error) {
		childRuns.Add(1)
		p := &scripted{turns: []core.AssistantMessage{
			{Content: core.Content{core.TextBlock{Text: "child result"}},
				StopReason: core.StopReasonStop},
		}}
		return NewAgent(core.AgentConfig{
			Model:      testModel(),
			StopPolicy: StopAfterTurns(3),
			Providers:  core.ProviderRegistry{testAPI: p.provider()},
		})
	}
	if err := parent.RegisterTool(SubagentTool(parent, factory,
		SubagentOptions{Name: "specialist"})); err != nil {
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
	parentProv := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "specialist", `{"prompt":"go"}`)),
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}
	parent := newTestAgent(t, parentProv, nil)

	factory := func(ctx context.Context) (*Agent, error) {
		h := core.NewConversationHistory()
		h.Record(core.NullLeaf, core.UserMessage{
			Content: core.Content{core.TextBlock{Text: "leaked parent context"}}})
		return NewAgentWithHistory(core.AgentConfig{
			Model: testModel(), Providers: core.ProviderRegistry{testAPI: parentProv.provider()},
		}, h)
	}
	_ = parent.RegisterTool(SubagentTool(parent, factory, SubagentOptions{Name: "specialist"}))

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
	parentProv := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "specialist", `{"prompt":"go"}`)),
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}
	parent := newTestAgent(t, parentProv, nil)

	var child *Agent
	factory := func(ctx context.Context) (*Agent, error) {
		p := &scripted{turns: []core.AssistantMessage{
			{Content: core.Content{core.TextBlock{Text: "child"}}, StopReason: core.StopReasonStop},
		}}
		a, err := NewAgent(core.AgentConfig{
			Model:     testModel(),
			Providers: core.ProviderRegistry{testAPI: p.provider()},
			// Deliberately NO StopPolicy: if one appears, SubagentTool put it
			// there.
		})
		child = a
		return a, err
	}
	_ = parent.RegisterTool(SubagentTool(parent, factory, SubagentOptions{
		Name: "specialist", BudgetFraction: 0.3, MaxBudgetUSD: 1.0,
	}))
	if _, err := parent.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if child == nil {
		t.Fatal("the factory never ran")
	}
	child.mu.Lock()
	got := child.cfg.StopPolicy
	child.mu.Unlock()
	if got == nil {
		t.Fatal("SubagentTool did not compose a budget policy onto the child; the budget " +
			"must reach the child as an explicit config field (REQ-MULTI-03)")
	}
}

// TestDelegationRefusesWhenTheParentBudgetIsSpent: the fraction is of the
// parent's REMAINING budget, so an exhausted parent delegates nothing.
func TestDelegationRefusesWhenTheParentBudgetIsSpent(t *testing.T) {
	parentProv := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse,
			toolUse(t, "c1", "specialist", `{"prompt":"go"}`)),
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}
	parent := newTestAgent(t, parentProv, nil)
	// Spend the parent's budget before it delegates.
	var spent core.Usage
	spent.SetCost(5.0)
	parent.addUsage(spent)

	called := false
	factory := func(ctx context.Context) (*Agent, error) {
		called = true
		p := &scripted{}
		return NewAgent(core.AgentConfig{Model: testModel(),
			Providers: core.ProviderRegistry{testAPI: p.provider()}})
	}
	_ = parent.RegisterTool(SubagentTool(parent, factory, SubagentOptions{
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

// unusedSchemaRef keeps the schema import honest if the file is trimmed.
var _ = schema.Object
var _ = json.Marshal
