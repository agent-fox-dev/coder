// Command agentdemo runs AgentKit end to end with no API key and no network.
//
//	go run ./examples/agentdemo
//
// It drives the real loop against the scripted faux provider, so every
// behaviour it prints is the shipped implementation, not a mock of it. Read
// the output alongside the source: each section demonstrates one requirement
// that the PRD got wrong before it was corrected.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/faux"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/session"
)

func main() {
	demoStreamingLoop()
	demoStopReasonTrap()
	demoTruncatedToolCalls()
	demoWireAsymmetry()
	demoTranscriptRepair()
	demoKillAndResume()
	demoDelegation()
}

func rule(title string) {
	fmt.Printf("\n\033[1m── %s %s\033[0m\n", title, strings.Repeat("─", max(0, 62-len(title))))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// wordCount is a real tool: a handler, a schema built with the combinators,
// and a result the model can read.
func wordCount() core.Tool {
	return core.Tool{
		Name:        "word_count",
		Description: "Count the words in a piece of text",
		InputSchema: schema.Object(
			schema.Prop("text", schema.String("The text to count words in")),
			schema.Opt("unique", schema.Bool("Count distinct words only")),
		),
		PromptGuidelines: []string{"Use word_count rather than counting by hand."},
		Handler: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			var args struct {
				Text   string `json:"text"`
				Unique bool   `json:"unique"`
			}
			if err := json.Unmarshal(in, &args); err != nil {
				return nil, err
			}
			words := strings.Fields(args.Text)
			n := len(words)
			if args.Unique {
				seen := map[string]bool{}
				for _, w := range words {
					seen[strings.ToLower(w)] = true
				}
				n = len(seen)
			}
			return json.Marshal(map[string]any{"count": n})
		},
	}
}

// ---------------------------------------------------------------------------

func demoStreamingLoop() {
	rule("1. The loop, streamed")
	fmt.Println("A two-turn run: the model calls a tool, reads the result, then answers.")
	fmt.Print("Events arrive as they are produced; the producer never blocks on us.\n\n")

	p := faux.New(
		faux.Turn{
			Blocks: []core.ContentBlock{
				faux.FauxText("Let me count those."),
				faux.FauxToolCall("call_1", "word_count", `{"text":"the quick brown fox jumps over the lazy dog"}`),
			},
			StopReason: core.StopReasonToolUse,
		},
		faux.Turn{
			Blocks:     []core.ContentBlock{faux.FauxText("That sentence has 9 words.")},
			StopReason: core.StopReasonStop,
		},
	)
	p.ChunkSize = 8 // split text into deltas, so streaming is visible

	agent := newAgent(p)
	if err := agent.RegisterTool(wordCount()); err != nil {
		fail(err)
	}

	stream, err := agent.Stream(context.Background(), "How many words in that sentence?")
	if err != nil {
		fail(err)
	}

	for e := range stream.Events() {
		switch v := e.(type) {
		case core.TextDeltaEvent:
			fmt.Print(v.Delta)
		case core.TextEndEvent:
			fmt.Println()
		case core.ToolCallStartEvent:
			fmt.Printf("  \033[36m→ calling %s (%s)\033[0m\n", v.Name, v.ToolUseID)
		case core.ToolResultEvent:
			fmt.Printf("  \033[32m← %s\033[0m\n", strings.TrimSpace(v.Message.Content.Text()))
		case core.TurnEndEvent:
			fmt.Printf("  \033[90m[turn %d ended: %s]\033[0m\n", v.TurnIndex, v.Message.StopReason)
		}
	}

	res, err := stream.RunResult()
	if err != nil {
		fail(err)
	}
	fmt.Printf("\nfinal: %q\nturns: %d, stop: %s\n", res.FinalText(), res.TurnCount, res.StopReason)
}

// ---------------------------------------------------------------------------

func demoStopReasonTrap() {
	rule("2. Why the loop ignores stop_reason (REQ-LOOP-01)")
	fmt.Println("Gemini and several OpenAI-compatible gateways return a STOP-family finish")
	fmt.Println("reason ALONGSIDE tool calls. A loop that gates iteration on stop_reason")
	fmt.Print("drops them silently — and passes every Anthropic-only test.\n\n")

	for _, reason := range []core.StopReason{core.StopReasonToolUse, core.StopReasonStop, ""} {
		p := faux.New(
			faux.Turn{
				Blocks:        []core.ContentBlock{faux.FauxToolCall("c1", "word_count", `{"text":"a b c"}`)},
				StopReason:    reason,
				RawStopReason: "STOP",
			},
			faux.Turn{Blocks: []core.ContentBlock{faux.FauxText("done")}, StopReason: core.StopReasonStop},
		)
		agent := newAgent(p)
		_ = agent.RegisterTool(wordCount())
		res, err := agent.Run(context.Background(), "count")
		if err != nil {
			fail(err)
		}
		var ran int
		for _, m := range res.Messages {
			if _, ok := m.(core.ToolResultMessage); ok {
				ran++
			}
		}
		label := string(reason)
		if label == "" {
			label = "(none)"
		}
		fmt.Printf("  stop_reason %-14s → tool executed: %v\n", label, ran == 1)
	}
	fmt.Println("\nAll three iterate, because the predicate is the PRESENCE of tool_use blocks.")
}

// ---------------------------------------------------------------------------

func demoTruncatedToolCalls() {
	rule("3. A truncated tool call is never executed (REQ-LOOP-10)")
	fmt.Println("Streamed arguments are salvage-repaired into valid JSON, so a truncated")
	fmt.Println("edit passes schema validation and would apply cleanly — corrupting a file.")
	fmt.Print("Only the stop reason can catch it, so max_tokens + tool calls runs NOTHING.\n\n")

	p := faux.New(
		faux.Turn{
			Blocks: []core.ContentBlock{
				faux.FauxToolCall("c1", "word_count", `{"text":"truncated mid-arg"}`),
			},
			StopReason: core.StopReasonLength, // the response hit the output cap
		},
		faux.Turn{Blocks: []core.ContentBlock{faux.FauxText("Re-issued and done.")},
			StopReason: core.StopReasonStop},
	)
	agent := newAgent(p)
	_ = agent.RegisterTool(wordCount())
	res, err := agent.Run(context.Background(), "count")
	if err != nil {
		fail(err)
	}
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok {
			fmt.Printf("  is_error=%v\n  %s\n", tr.IsError, indent(tr.Content.Text()))
		}
	}
	fmt.Printf("\n  ...and the loop CONTINUED: %d turns, so the model can re-issue.\n", res.TurnCount)
}

func indent(s string) string {
	var out struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal([]byte(s), &out)
	if out.Detail == "" {
		return s
	}
	return strings.ReplaceAll(out.Detail, "\n", "\n  ")
}

// ---------------------------------------------------------------------------

func demoWireAsymmetry() {
	rule("4. One transcript, two incompatible wire shapes (REQ-LOOP-02)")
	fmt.Println("The PRD called \"all tool results in one user message\" a LOOP invariant.")
	fmt.Println("It is an Anthropic WIRE rule. OpenAI mandates one message per result, so")
	fmt.Print("the single-message form is not representable there at all.\n\n")

	mk := func(id, args string) core.ContentBlock {
		b, err := core.NewToolUse(id, "word_count", json.RawMessage(args))
		if err != nil {
			fail(err)
		}
		return b
	}
	msgs := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "count all three"}}},
		core.AssistantMessage{
			Content: core.Content{
				mk("call_a", `{"text":"one"}`),
				mk("call_b", `{"text":"two"}`),
				mk("call_c", `{"text":"three"}`),
			},
			StopReason: core.StopReasonToolUse,
		},
		core.ToolResultMessage{ToolUseID: "call_a", ToolName: "word_count",
			Content: core.Content{core.TextBlock{Text: `{"count":1}`}}},
		core.ToolResultMessage{ToolUseID: "call_b", ToolName: "word_count",
			Content: core.Content{core.TextBlock{Text: `{"count":1}`}}},
		core.ToolResultMessage{ToolUseID: "call_c", ToolName: "word_count",
			Content: core.Content{core.TextBlock{Text: `{"count":1}`}}},
	}
	req := core.Request{Messages: msgs}

	am := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 1024}
	om := &core.Model{ID: "gpt-x", API: openai.API, Provider: "openai", MaxTokens: 1024}

	ab, _, err := anthropic.BuildRequest(am, req, core.CacheRetentionNone)
	if err != nil {
		fail(err)
	}
	ob, _, err := openai.BuildRequest(om, req)
	if err != nil {
		fail(err)
	}

	fmt.Println("  Anthropic — 3 tool_result blocks in ONE user message:")
	printRoles(ab)
	fmt.Println("\n  OpenAI — 3 separate role:\"tool\" messages:")
	printRoles(ob)
}

func printRoles(body any) {
	j, _ := json.Marshal(body)
	var dec struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(j, &dec)
	for i, m := range dec.Messages {
		var detail string
		var n int
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				n++
			}
		}
		switch {
		case n > 0:
			detail = fmt.Sprintf("  (%d tool_result blocks)", n)
		case m.ToolCallID != "":
			detail = "  (" + m.ToolCallID + ")"
		}
		fmt.Printf("    [%d] role=%-9s%s\n", i, m.Role, detail)
	}
}

// ---------------------------------------------------------------------------

func demoTranscriptRepair() {
	rule("5. Repairing what a killed process leaves behind (REQ-PROV-11)")
	fmt.Println("Ctrl-C during a tool batch leaves an aborted assistant turn whose tool_use")
	fmt.Println("blocks have no results — and, worse, results whose tool_use was dropped.")
	fmt.Println("Both are 400s. The provider repairs the view at send time; the log stays")
	fmt.Print("complete.\n\n")

	b, err := core.NewToolUse("call_1", "word_count", json.RawMessage(`{"text":"x"}`))
	if err != nil {
		fail(err)
	}
	damaged := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		// An aborted turn: partial content, one unanswered call.
		core.AssistantMessage{
			Content:    core.Content{core.TextBlock{Text: "I'll co"}, b},
			StopReason: core.StopReasonAborted,
			Provider:   "anthropic", API: anthropic.API, Model: "claude-x",
		},
		// ...and the result that DID land before the process died. Rule 2 drops
		// the turn above; without rule 2b this result is orphaned and the
		// request is rejected.
		core.ToolResultMessage{ToolUseID: "call_1", ToolName: "word_count",
			Content: core.Content{core.TextBlock{Text: `{"count":1}`}}},
	}

	am := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 1024}
	body, rep, err := anthropic.BuildRequest(am, core.Request{Messages: damaged}, core.CacheRetentionNone)
	if err != nil {
		fail(err)
	}
	fmt.Printf("  repairs: %s\n", rep)
	fmt.Println("  resulting wire messages:")
	printRoles(body)
	fmt.Println("\n  The durable log still holds all three messages; only the VIEW was repaired.")
}

// ---------------------------------------------------------------------------

func newAgent(p *faux.Provider) *agentkit.Agent {
	cfg := core.AgentConfig{
		Model:        faux.Model(),
		SystemPrompt: "You are a helpful assistant.",
		StopPolicy: agentkit.StopAny(
			agentkit.StopAfterTurns(8),
			agentkit.StopOverBudget(0.50),
		),
		ParallelTools: true,
		Providers:     core.ProviderRegistry{faux.API: p.APIProvider()},
	}
	a, err := agentkit.NewAgent(cfg)
	if err != nil {
		fail(err)
	}
	return a
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "agentdemo: %v\n", err)
	os.Exit(1)
}

// ---------------------------------------------------------------------------

func demoKillAndResume() {
	rule("6. A session survives the process (REQ-SESS-01/02)")
	fmt.Println("The durable unit is an append-only JSONL log, not a serialized message")
	fmt.Println("array. Resuming is a FOLD over that log: it recovers the model, the")
	fmt.Print("reasoning level and the branch, and the agent is CONSTRUCTED from them.\n\n")

	dir, err := os.MkdirTemp("", "agentkit-demo-*")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "session.jsonl")

	// ---- Process 1.
	store1, _, err := agentkit.OpenSession(path, session.Options{Durability: session.DurabilityPerEntry})
	if err != nil {
		fail(err)
	}
	p1 := faux.New(faux.Turn{
		Blocks:     []core.ContentBlock{faux.FauxText("Paris.")},
		StopReason: core.StopReasonStop,
	})
	a1 := newAgentWithStore(p1, store1)
	if _, err := a1.Run(context.Background(), "What is the capital of France?"); err != nil {
		fail(err)
	}
	fmt.Printf("  process 1 asked a question and got: %q\n", "Paris.")
	_ = store1.Close()
	fmt.Print("  ...process 1 exits.\n\n")

	// ---- Process 2 reopens the same path.
	store2, resume, err := agentkit.OpenSession(path, session.Options{Durability: session.DurabilityPerEntry})
	if err != nil {
		fail(err)
	}
	defer store2.Close()

	fmt.Printf("  process 2 folded the log: %d messages recovered\n", len(resume.Messages))
	fmt.Printf("  recovered provenance triple: provider=%q api=%q model=%q\n",
		resume.Provider, resume.API, resume.ModelID)
	fmt.Println("    (all THREE matter: two of them makes same_model false on the first")
	fmt.Println("     post-resume request and silently strips every signed thinking block)")

	p2 := faux.New(faux.Turn{
		Blocks:     []core.ContentBlock{faux.FauxText("It has about 2.1 million people.")},
		StopReason: core.StopReasonStop,
	})
	cfg := baseConfig(p2)
	cfg.SessionStore = store2
	a2, err := agentkit.NewAgentFromSession(cfg, resume,
		func(string, core.API, string) (*core.Model, error) { return faux.Model(), nil })
	if err != nil {
		fail(err)
	}
	res, err := a2.Run(context.Background(), "And its population?")
	if err != nil {
		fail(err)
	}
	fmt.Printf("\n  process 2 answered a follow-up: %q\n", res.FinalText())

	sent := p2.Requests()[0].Messages
	fmt.Printf("  the resumed request carried %d messages — the earlier turns survived.\n", len(sent))
}

// ---------------------------------------------------------------------------

func demoDelegation() {
	rule("7. Delegation hands out a FRESH child per call (REQ-MULTI-02/04)")
	fmt.Println("SubagentTool takes a FACTORY, not an agent. A single shared child looks")
	fmt.Println("correct and fails under exactly the condition delegation exists for: two")
	fmt.Print("parallel calls, where the second finds the run slot taken.\n\n")

	parent := faux.New(
		faux.Turn{
			Blocks: []core.ContentBlock{
				faux.FauxToolCall("c1", "researcher", `{"prompt":"look up A"}`),
				faux.FauxToolCall("c2", "researcher", `{"prompt":"look up B"}`),
				faux.FauxToolCall("c3", "researcher", `{"prompt":"look up C"}`),
			},
			StopReason: core.StopReasonToolUse,
		},
		faux.Turn{Blocks: []core.ContentBlock{faux.FauxText("All three done.")},
			StopReason: core.StopReasonStop},
	)
	cfg := baseConfig(parent)
	cfg.ParallelTools = true
	a, err := agentkit.NewAgent(cfg)
	if err != nil {
		fail(err)
	}

	var childPrompts []string
	factory := func(ctx context.Context) (*agentkit.Agent, error) {
		p := faux.New(faux.Turn{
			Blocks:     []core.ContentBlock{faux.FauxText("found it")},
			StopReason: core.StopReasonStop,
		})
		child, err := agentkit.NewAgent(baseConfig(p))
		if err != nil {
			return nil, err
		}
		return child, nil
	}
	if err := a.RegisterTool(agentkit.SubagentTool(a, factory,
		agentkit.SubagentOptions{Name: "researcher"})); err != nil {
		fail(err)
	}

	res, err := a.Run(context.Background(), "research three things in parallel")
	if err != nil {
		fail(err)
	}
	for _, m := range res.Messages {
		if tr, ok := m.(core.ToolResultMessage); ok {
			status := "ok"
			if tr.IsError {
				status = "ERROR"
			}
			childPrompts = append(childPrompts, fmt.Sprintf("%s=%s", tr.ToolUseID, status))
		}
	}
	fmt.Printf("  three concurrent delegations: %s\n", strings.Join(childPrompts, "  "))
	fmt.Printf("  parent: %q\n", res.FinalText())
	fmt.Println("\n  With a shared child agent the second and third would be ErrBusy.")
}

func baseConfig(p *faux.Provider) core.AgentConfig {
	return core.AgentConfig{
		Model:        faux.Model(),
		SystemPrompt: "You are a helpful assistant.",
		StopPolicy:   agentkit.StopAfterTurns(8),
		Providers:    core.ProviderRegistry{faux.API: p.APIProvider()},
	}
}

func newAgentWithStore(p *faux.Provider, store core.SessionStore) *agentkit.Agent {
	cfg := baseConfig(p)
	cfg.SessionStore = store
	a, err := agentkit.NewAgent(cfg)
	if err != nil {
		fail(err)
	}
	return a
}
