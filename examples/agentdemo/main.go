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
	"strings"

	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/faux"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/schema"
)

func main() {
	demoStreamingLoop()
	demoStopReasonTrap()
	demoTruncatedToolCalls()
	demoWireAsymmetry()
	demoTranscriptRepair()
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
