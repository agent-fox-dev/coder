package agentkit

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/session"
	"github.com/agentfox/agentkit-go/stop"
)

func openTestSession(t *testing.T, path string) (*session.Store, *session.Resume) {
	t.Helper()
	store, r, err := session.OpenOrCreate(path, session.Options{Durability: session.DurabilityPerEntry})
	if err != nil {
		t.Fatalf("OpenOrCreate: %v", err)
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
		StopPolicy:   stop.AfterTurns(5),
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

var _ = json.Marshal
