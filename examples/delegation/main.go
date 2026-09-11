// Command delegation runs an orchestrator that answers by handing work to
// named specialists rather than doing it itself.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/delegation "Which Go files in this directory define the agent loop, and what does each do?"
//
// With no task argument it uses a default one that needs both specialists.
// Any model in the catalog works:
//
//	AGENTKIT_MODEL=openai/gpt-5.6-terra go run ./examples/delegation
//
// Two specialists are registered, and the difference between them is the
// point: the researcher may read and search files inside the working
// directory, the summarizer may call no tool at all. Each is a name the
// orchestrator sees as a tool, and each call to that tool runs a brand new
// child agent with empty history.
//
// The model decides when to delegate here. To fan delegations out from Go
// code instead — one child per item in a slice, run concurrently, results
// returned in input order with a per-item error — use agentkit.RunParallel.
// Either way the delegation tools are safe to call in parallel, because every
// call constructs its own agent value; nothing is shared between siblings.
//
// See examples/README.md for the full environment-variable table.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/catalog"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/provider/ollama"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/provider/openairesponses"
	"github.com/agentfox/agentkit-go/stop"
	"github.com/agentfox/agentkit-go/tools"
)

// maxBudgetUSD caps the whole delegation tree: it is the parent's stop policy
// AND the number each specialist's BudgetFraction is a fraction of. Keeping it
// in one place is deliberate — two different ceilings would let the children
// outspend the run they belong to.
const maxBudgetUSD = 1.00

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	task := strings.Join(os.Args[1:], " ")
	if task == "" {
		task = "Find out what this directory's Go code does with subagents, " +
			"then give me a three-sentence summary a newcomer could follow."
	}

	// 1. Resolve the model. This is what supplies the wire API, base URL,
	//    context window, pricing and compatibility profile — none of which the
	//    model-ID string carries. Every specialist inherits this model unless
	//    its definition names one of its own, which is how a cheap child can
	//    serve an expensive orchestrator.
	model, err := catalog.ResolveModel(modelSpec())
	if err != nil {
		return err
	}

	// 2. Register the wire APIs. Nothing is registered by import side effect,
	//    so a program that only wants the loop never drags net/http in. All
	//    five are registered here so any AGENTKIT_MODEL resolves; a real
	//    application registers only the ones it uses. Children inherit this
	//    registry from the parent's config, so the specialists need no
	//    provider wiring of their own.
	cfg := core.AgentConfig{Model: model}
	agentkit.RegisterDefaults(&cfg,
		anthropic.Provider(anthropic.Options{}),
		openai.Provider(openai.Options{}),
		openairesponses.Provider(openairesponses.Options{}),
		google.Provider(google.Options{}),
		ollama.Provider(ollama.Options{}),
	)

	// 3. The orchestrator is told it HAS specialists, because a model that is
	//    merely able to delegate mostly will not: answering directly is always
	//    the shorter path. Naming them and forbidding the direct answer is
	//    what makes delegation happen.
	cfg.SystemPrompt = strings.Join([]string{
		"You are an orchestrator. You do not answer questions yourself.",
		"You have two specialists, each callable as a tool:",
		"  researcher  — can read and search files in the working directory.",
		"  summarizer  — has no tools; it only rewrites text you give it.",
		"Delegate. A specialist sees NONE of this conversation, so every call",
		"must carry the full task and any findings it needs. Send independent",
		"research in a single turn so the calls run in parallel. When the",
		"research is in, hand it to the summarizer and return what it says.",
	}, "\n")

	// 4. Parallel tool execution is what makes two delegations in one turn
	//    actually concurrent; without it the batch is serialized and the same
	//    program is correct but slower.
	cfg.ParallelTools = true

	// 5. A stop policy is not optional for a delegating agent. Turns bound the
	//    orchestration loop; the budget bounds the whole tree, since a child's
	//    spend lands in the parent's usage as the delegation tool returns.
	cfg.StopPolicy = stop.Any(
		stop.AfterTurns(12),
		stop.OverBudget(maxBudgetUSD),
	)

	if err := checkCredentials(model); err != nil {
		return err
	}

	parent, err := agentkit.NewAgent(cfg)
	if err != nil {
		return err
	}

	// 6. The built-in tool set, scoped down per specialist below. It is built
	//    once and handed to a definition rather than registered on the parent:
	//    the orchestrator itself gets no file access at all, so the only way
	//    anything reaches the filesystem is through a named specialist.
	ws, err := tools.NewWorkspace("")
	if err != nil {
		return err
	}
	builtins, err := tools.All(tools.Options{Workspace: ws})
	if err != nil {
		return err
	}

	// 7. A registry of named specialists. It is a value this program owns, not
	//    a package global, and a duplicate name is an error — the name is
	//    exactly what the orchestrator model sees as a tool, so a silent
	//    replacement would change the tool surface without changing any code
	//    that reads like it did.
	registry := agentkit.NewAgentRegistry()

	// The researcher is handed the whole built-in set but ALLOWED only two of
	// it. ToolPolicy resolution — not the Tools slice — decides what exists,
	// and it is also what the unguarded-shell check consults: scoping to
	// read_file and search_files is why this specialist needs no tool
	// interceptor, while a child that kept `execute` would refuse to run
	// without one.
	//
	// BudgetFraction is a CONFIG FIELD, not something smuggled through
	// context.Context. A budget carried in a ctx value is invisible to the
	// type system and silently absent the moment any caller passes a bare
	// context.Background(); here a missing budget is a visible zero in a
	// struct literal you are already reading.
	if err := registry.Register(agentkit.AgentDefinition{
		Name: "researcher",
		Description: "Investigate a question against the files in the working " +
			"directory and report concrete findings with file paths.",
		SystemPrompt: "You are a researcher. Use read_file and search_files to find " +
			"evidence in the working directory. Report what you actually found, with " +
			"file paths and short quotes. Never guess; say so when the files do not answer.",
		Tools:          builtins,
		ToolPolicy:     core.ToolPolicy{ToolNames: []string{"read_file", "search_files"}},
		StopPolicy:     stop.AfterTurns(8),
		BudgetFraction: 0.30, // of whatever the parent has left when called
	}); err != nil {
		return err
	}

	// The summarizer is the other extreme: NoToolsAll empties the resolved set
	// outright, custom tools included, so no allowlist has to be kept in sync
	// as the built-in set grows. A specialist whose whole job is prose should
	// not be one prompt injection away from a file read.
	if err := registry.Register(agentkit.AgentDefinition{
		Name:        "summarizer",
		Description: "Turn research notes into a short, plain-prose summary.",
		SystemPrompt: "You are a summarizer. You have no tools and no way to check " +
			"anything: work only from the text you are given. Answer in plain prose, " +
			"no preamble, no bullet lists.",
		ToolPolicy:     core.ToolPolicy{NoTools: core.NoToolsAll},
		StopPolicy:     stop.AfterTurns(2),
		BudgetFraction: 0.20,
	}); err != nil {
		return err
	}

	// 8. One delegation tool per specialist. Each is backed by a FACTORY, so
	//    every call builds a fresh child from the definition plus the parent's
	//    providers, credentials and tracer. That is not a style preference: a
	//    single shared child would take the run slot on the first call and
	//    return ErrBusy on the second one in the same parallel batch — failing
	//    under exactly the condition delegation exists for.
	//
	//    The fresh child also starts with EMPTY history, always. Passing the
	//    parent's transcript down would be both a prompt-injection surface —
	//    anything a tool result put in the parent's history would reach the
	//    child's context — and a bill for re-sending the whole conversation to
	//    every specialist, which is the cost delegation was meant to avoid.
	//    The price is that the orchestrator must write self-contained prompts,
	//    which is what its system prompt above insists on.
	for _, t := range registry.Tools(parent, maxBudgetUSD) {
		if err := parent.RegisterTool(t); err != nil {
			return err
		}
	}

	// 9. Streaming, so the delegations are visible as they happen rather than
	//    as one silent pause. Events are consumed as produced; the loop never
	//    blocks on this goroutine, and the result stays available from
	//    RunResult afterwards.
	stream, err := parent.Stream(context.Background(), task)
	if err != nil {
		return err
	}
	for e := range stream.Events() {
		switch v := e.(type) {
		case core.TextDeltaEvent:
			fmt.Print(v.Delta)
		case core.TextEndEvent:
			fmt.Println()
		case core.ToolExecutionStartEvent:
			// The tool name IS the specialist name.
			fmt.Fprintf(os.Stderr, "  → delegating to %s\n", v.Name)
		case core.ToolResultEvent:
			fmt.Fprintf(os.Stderr, "  ← %s: %s\n", v.Message.ToolName, summarize(v.Message))
		}
	}

	res, err := stream.RunResult()
	if err != nil {
		return err
	}

	// Usage is the whole tree's: a child's tokens are metered against the same
	// parent the budget fractions were carved out of.
	u := res.Usage
	fmt.Fprintf(os.Stderr, "\n[%s · %d turns · in %d / out %d tokens · cached %d · $%.5f]\n",
		model.ID, res.TurnCount, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CostUSD)
	return nil
}

// summarize renders one delegation result for the progress log. A subagent
// returns the child's final text and its turn count; a child that FAILED comes
// back as an error result rather than an error from the parent's run, so the
// orchestrator can try something else instead of the whole run ending.
func summarize(m core.ToolResultMessage) string {
	text := strings.TrimSpace(m.Content.Text())
	if m.IsError {
		return "failed: " + oneLine(text)
	}
	var out struct {
		Result string `json:"result"`
		Turns  int    `json:"turns"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return oneLine(text)
	}
	return fmt.Sprintf("%d turns, %s", out.Turns, oneLine(out.Result))
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 100 {
		return s[:100] + "…"
	}
	return s
}

// modelSpec is "vendor/model-id", or a bare id when it is unambiguous.
func modelSpec() string {
	if s := os.Getenv("AGENTKIT_MODEL"); s != "" {
		return s
	}
	return "anthropic/claude-sonnet-5"
}

// checkCredentials fails BEFORE the request with a message naming the variable
// to set, rather than after a 401 that names none of them.
//
// The three-state check matters: a deployment using an instance role or ADC
// has no key this process can read and a transport that will nonetheless
// authenticate, so "ambient" must pass a pre-flight that "none" fails.
func checkCredentials(m *core.Model) error {
	auth := provider.ResolveAuth(authFor(m), provider.Env{})
	if auth.State != provider.CredentialNone {
		return nil
	}
	return fmt.Errorf("no credential for vendor %q: set one of %s (see examples/README.md)",
		m.Provider, strings.Join(varNames(authFor(m)), ", "))
}

func authFor(m *core.Model) provider.VendorAuth {
	switch m.API {
	case anthropic.API:
		return anthropic.VendorAuth
	case google.API:
		return google.VendorAuth
	case ollama.API:
		return ollama.VendorAuth
	default:
		// openai-completions and openai-responses share one per-vendor table,
		// keyed on the VENDOR: "openai", "openrouter", "groq", "deepseek"…
		return openai.AuthFor(m.Provider)
	}
}

func varNames(v provider.VendorAuth) []string {
	out := make([]string, 0, len(v.Vars)+1)
	for _, e := range v.Vars {
		out = append(out, e.Name)
	}
	if v.BaseURLVar != "" {
		out = append(out, v.BaseURLVar+" (for a gateway or a local server)")
	}
	if len(out) == 0 {
		out = append(out, "a vendor-specific API key")
	}
	return out
}
