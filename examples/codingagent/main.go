// Command codingagent is a coding agent with the built-in file and shell
// tools, a workspace root, an execute policy and a live event stream.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/codingagent "Which files define the tool policy?"
//
// The workspace root is the only directory the file tools can reach, and it
// defaults to the current one:
//
//	go run ./examples/codingagent --dir ./tools "Summarise this package."
//	AGENTKIT_MODEL=openai/gpt-5.6-terra go run ./examples/codingagent --dir /tmp/scratch
//
// See examples/README.md for the full environment-variable table.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
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
	"github.com/agentfox/agentkit-go/tools"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// allowedPrograms is the shell allowlist. It is deliberately short: every
// entry is a program whose output the agent reads, none of them writes.
var allowedPrograms = []string{"go", "git", "ls", "cat", "rg"}

func run() error {
	dir := flag.String("dir", ".", "workspace root; the file tools cannot reach outside it")
	flag.Parse()

	task := strings.Join(flag.Args(), " ")
	if task == "" {
		task = "List the Go files in this directory and summarise what the package does."
	}

	model, err := catalog.ResolveModel(modelSpec())
	if err != nil {
		return err
	}

	cfg := core.AgentConfig{Model: model}
	agentkit.RegisterDefaults(&cfg,
		anthropic.Provider(anthropic.Options{}),
		openai.Provider(openai.Options{}),
		openairesponses.Provider(openairesponses.Options{}),
		google.Provider(google.Options{}),
		ollama.Provider(ollama.Options{}),
	)

	// 1. The workspace is the containment boundary, not a convenience. Every
	//    path a file tool is handed is resolved against this root — symlinks
	//    included — so a model that asks for ../../etc/passwd is refused by
	//    the tool rather than by a prompt asking it not to.
	ws, err := tools.NewWorkspace(*dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "workspace: %s\n", ws.Root)

	// 2. All() is the default set: read, write, edit, list, find, search and
	//    the three shell tools. `fetch_url` is NOT in it — a tool that makes
	//    outbound requests on the model's behalf is a different risk class,
	//    and reaching it takes a second affirmative act (tools.FetchTool).
	built, err := tools.All(tools.Options{Workspace: ws})
	if err != nil {
		return err
	}

	// 3. The OQ-8 guard. `execute`, `run_command` and `powershell` are in the
	//    set above, so a nil cfg.BeforeToolCall fails the run on its first
	//    line with core.ErrUnguardedExecute — before a request is sent, and
	//    long before an unrestricted shell shows up on a bill. There are
	//    exactly two ways past it: an interceptor, or agentkit.AllowAllToolCalls,
	//    which is the explicit "yes, this agent runs an unrestricted shell".
	//
	//    RestrictedPolicy is the shipped starting point: an allowlist of
	//    program names plus rejection of shell operators (pipes, `;`, `&&`,
	//    redirection, substitution) whose grammar is POSIX sh. It is a FLOOR,
	//    not a sandbox — `go` alone can run arbitrary code through a test
	//    file or a generator — so an embedder that knows what it is running
	//    should replace it rather than widen it.
	policy := agentkit.RestrictedPolicy(agentkit.RestrictedOptions{
		AllowedPrograms: allowedPrograms,
		// A refusal is fed back to the model as a blocked tool result, and it
		// will usually try a different command. Set TerminateOnBlock to end
		// the run instead, for a deployment where a denied call is a signal
		// that something is wrong rather than a wrong first guess.
		TerminateOnBlock: false,
	})

	// 4. Wrapping the policy rather than replacing it is how an interceptor
	//    gains a side effect — here a line per authorized call, so the
	//    terminal shows what the agent is doing to the filesystem. The
	//    decision itself still comes from the policy: the wrapper reports,
	//    it does not judge.
	cfg.BeforeToolCall = func(ctx context.Context, in core.BeforeToolCallContext) core.BeforeToolCallDecision {
		d := policy(ctx, in)
		if d.Block {
			fmt.Fprintf(os.Stderr, "  blocked %s: %s\n", in.ToolName, d.Reason)
		} else {
			fmt.Fprintf(os.Stderr, "  allow   %s %s\n", in.ToolName, summarize(in.Arguments))
		}
		return d
	}

	// 5. Turns and budget are separate bounds because they fail differently.
	//    A tool-using agent can loop cheaply for a long time (turns catch
	//    that) or spend a lot in three turns over a large file (budget
	//    catches that). StopAny fires on whichever comes first.
	cfg.StopPolicy = agentkit.StopAny(
		agentkit.StopAfterTurns(20),
		agentkit.StopOverBudget(2.00), // dollars, cumulative for the run
	)
	cfg.SystemPrompt = "You are a careful coding assistant. Read before you write. " +
		"Prefer the search and read tools over shell commands. Be concise."

	if err := checkCredentials(model); err != nil {
		return err
	}

	agent, err := agentkit.NewAgent(cfg)
	if err != nil {
		return err
	}
	for _, t := range built {
		if err := agent.RegisterTool(t); err != nil {
			return err
		}
	}

	// 6. Streaming, not Run, because a coding agent is slow and silent
	//    otherwise: the tool events are the only evidence that it is working.
	//    The producer never blocks on this loop, so a slow consumer here
	//    cannot stall the run.
	stream, err := agent.Stream(context.Background(), task)
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
			// Emitted after the interceptor allowed the call and before the
			// handler runs, so this is the moment work actually starts.
			fmt.Fprintf(os.Stderr, "  run     %s\n", v.Name)
		case core.ToolResultEvent:
			fmt.Fprintf(os.Stderr, "  result  %s%s\n",
				errMark(v.Message.IsError), firstLine(v.Message.Content.Text()))
		}
	}

	// 7. RunResult is available whether or not the stream was drained, and it
	//    carries the error the loop ended with. A stream that ended on an
	//    error still yielded every event produced before it.
	res, err := stream.RunResult()
	if err != nil {
		return err
	}

	u := res.Usage
	fmt.Fprintf(os.Stderr, "\n[%s · %d turns · stop %s · in %d / out %d tokens · $%.5f]\n",
		model.ID, res.TurnCount, res.StopReason, u.InputTokens, u.OutputTokens, u.CostUSD)
	return nil
}

// summarize renders tool arguments as one short line. Keys are sorted so two
// runs of the same call print the same way; JSON object order is not stable.
func summarize(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, truncate(fmt.Sprint(args[k]), 60)))
	}
	return strings.Join(parts, " ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return truncate(s, 100)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func errMark(isErr bool) string {
	if isErr {
		return "error: "
	}
	return ""
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
