// Command flatline implements one spec pack, task group by task group, the
// way agent-fox's `af code` does in its simplest configuration: one spec, no
// cross-spec dependencies, no parallelism — a flat line through the plan.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/flatline --dir ~/src/widgets .specs/03_widget_counter
//
// For every task group in tasks.json, in order, it creates a branch, runs a
// coder session (or a read-only gate session for a `checkpoint` group) with
// the spec rendered and scoped to that group, runs the pack's own test
// commands, commits, marks the group's subtasks done in tasks.json, and
// squash-merges the branch into the branch it started from. A group whose
// gates fail is retried with the error in the prompt; one that exhausts its
// retries stops the pass and keeps its branch under stalled/. With
// --land=branch the group branches are kept instead, and the finished work
// ends up on one named after the spec — feature/{spec_id}_{spec_name} — so
// the branch to merge is not something to work out by counting. After the
// last group the three test commands run once more (agent-fox's post-merge
// `make check`) and an informational verifier session records PASS/FAIL
// verdicts per requirement.
//
// See examples/flatline/README.md for the mapping to agent-fox and for the
// places where this program deliberately differs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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

// Exit codes follow agent-fox's `af code` where the outcomes coincide.
const (
	exitCompleted   = 0
	exitFailed      = 1 // a step errored, or a task group exhausted its retries (agent-fox: stalled)
	exitUsage       = 2
	exitCostLimit   = 3 // agent-fox: cost_limit
	exitDirty       = 4 // every group landed, but the final checks fail (agent-fox: completed_dirty)
	exitInterrupted = 130
)

func main() {
	os.Exit(run())
}

func run() int {
	dir := flag.String("dir", ".", "the repository to work in; the agent's file tools cannot reach outside it")
	specsDir := flag.String("specs-dir", "", "where NN_name spec directories live (default: <dir>/.specs)")
	land := flag.String("land", "merge", "what to do with each task group's branch once it passes: merge (squash into the current branch) or branch (keep it)")
	push := flag.Bool("push", false, "push what the run lands: the base branch after each merge, or the final branch when the groups are kept")
	finalBranch := flag.String("final-branch", "", "name for the branch carrying the finished work with --land=branch (default: feature/{spec_id}_{spec_name})")
	modelSpec := flag.String("model", "", "model spec, e.g. anthropic/claude-sonnet-5 (default $AGENTKIT_MODEL)")
	maxTurns := flag.Int("max-turns", 300, "per-session turn ceiling (agent-fox coder default)")
	budget := flag.Float64("budget", 20.0, "per-session spend ceiling in dollars (agent-fox max_budget_usd)")
	maxCost := flag.Float64("max-cost", 0, "run spend ceiling in dollars; 0 means none (agent-fox orchestrator.max_cost)")
	maxRetries := flag.Int("max-retries", 2, "retries per task group after the first attempt (agent-fox max_retries)")
	sessionTimeout := flag.Duration("session-timeout", 45*time.Minute, "wall-clock ceiling per session (agent-fox session_timeout)")
	checkTimeout := flag.Duration("check-timeout", 10*time.Minute, "timeout for one test command")
	noVerifier := flag.Bool("no-verifier", false, "skip the informational verifier session after the last group")
	assumeDeps := flag.Bool("assume-deps", false, "treat the pack's cross-spec dependencies as already implemented instead of refusing the run")
	journal := flag.String("journal", "", "append a JSONL record of every step to this file")
	allow := flag.String("allow", "", "extra programs the coder's shell may run, comma-separated")
	showText := flag.Bool("show-text", false, "print the model's prose as well as its tool calls")
	pushAttempts := flag.Int("push-attempts", 4, "how many times to try pushing before giving up")
	verbose := flag.Bool("verbose", false, "verbose output: tool calls, timing and cost diagnostics")

	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		usage()
		return exitUsage
	}
	landing, err := ParseLanding(*land)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return exitUsage
	}

	// Ctrl-C cancels the context, which aborts the in-flight model request
	// and the running test command. Whatever the current group's session
	// already wrote stays on its branch.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ws, err := tools.NewWorkspace(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitFailed
	}
	sd := *specsDir
	if sd == "" {
		sd = filepath.Join(ws.Root, ".specs")
	}
	specDir, err := ResolveSpecDir(sd, flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return exitUsage
	}
	pack, warnings, err := LoadPackWith(ws.Root, specDir, *assumeDeps)
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "  ! spec warning: %s\n", w)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitUsage
	}

	model, err := resolveModel(*modelSpec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitFailed
	}
	if err := checkCredentials(model); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitFailed
	}

	cfg := core.AgentConfig{Model: model}
	agentkit.RegisterDefaults(&cfg,
		anthropic.Provider(anthropic.Options{}),
		openai.Provider(openai.Options{}),
		openairesponses.Provider(openairesponses.Options{}),
		google.Provider(google.Options{}),
		ollama.Provider(ollama.Options{}),
	)

	brain := &agentBrain{
		base:          cfg,
		workspace:     ws,
		progress:      os.Stderr,
		maxTurns:      *maxTurns,
		budgetUSD:     *budget,
		timeout:       *sessionTimeout,
		extraPrograms: extraPrograms(*allow, pack),
		showText:      *showText,
		verbose:       *verbose,
	}

	final := strings.TrimSpace(*finalBranch)
	if final == "" {
		final = FinalBranchName(pack.Spec.SpecID, pack.Spec.SpecName)
	}

	opts := Options{
		Pack: pack, Git: NewGit(ws.Root, execRunner), Brain: brain, Run: execRunner,
		Landing: landing, FinalBranch: final, Push: *push, PushAttempts: *pushAttempts,
		MaxRetries: *maxRetries, MaxCostUSD: *maxCost, CheckTimeout: *checkTimeout,
		RunVerifier: !*noVerifier, Out: os.Stderr, JournalPath: *journal, Verbose: *verbose,
	}

	if *verbose {
		banner(opts, model.ID)
	}
	res, runErr := Run(ctx, opts)
	Summary(os.Stderr, res, runErr, *verbose)

	switch {
	case errors.Is(runErr, context.Canceled):
		return exitInterrupted
	case runErr != nil:
		return exitFailed
	case res.CostLimit:
		return exitCostLimit
	case res.Stalled:
		return exitFailed
	case res.Dirty:
		return exitDirty
	default:
		return exitCompleted
	}
}

func banner(o Options, modelID string) {
	fmt.Fprintf(os.Stderr, "flatline: %s\n", o.Pack.Spec.SpecName)
	fmt.Fprintf(os.Stderr, "  spec:   %s\n", o.Pack.Dir)
	fmt.Fprintf(os.Stderr, "  repo:   %s\n", o.Pack.Root)
	fmt.Fprintf(os.Stderr, "  model:  %s\n", modelID)
	tc := o.Pack.Spec.Tasks.TestCommands
	fmt.Fprintf(os.Stderr, "  checks: linter=%q spec_tests=%q all_tests=%q\n", tc.Linter, tc.SpecTests, tc.AllTests)
	fmt.Fprintf(os.Stderr, "  land:   %s\n", o.Landing)
	if o.Landing == LandBranch {
		fmt.Fprintf(os.Stderr, "  final:  %s\n", o.FinalBranch)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `flatline — implement one spec pack, task group by task group.

usage: flatline [flags] <spec>

  <spec> is a spec directory, or a number / NN_name resolved under --specs-dir.

example:
  flatline --dir ~/src/widgets 3
  flatline --dir ~/src/widgets .specs/03_widget_counter --land=branch --max-cost=15

flags:
`)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `
exit codes: 0 completed · 1 failed or stalled · 2 usage · 3 cost limit · 4 completed but final checks fail · 130 interrupted
`)
}

// extraPrograms widens the coder's shell allowlist with the programs the
// pack's own test commands use and anything the operator named.
func extraPrograms(allow string, pack *Pack) []string {
	var out []string
	for _, s := range strings.Split(allow, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return append(out, checkPrograms(pack.Spec.Tasks.TestCommands)...)
}

func resolveModel(spec string) (*core.Model, error) {
	if spec == "" {
		spec = os.Getenv("AGENTKIT_MODEL")
	}
	if spec == "" {
		spec = "anthropic/claude-sonnet-5"
	}
	return catalog.ResolveModel(spec)
}

// checkCredentials fails BEFORE the first request, naming the variable to set.
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
