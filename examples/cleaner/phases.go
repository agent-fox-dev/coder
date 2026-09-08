package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/tools"
)

// Brain is the model-driven half of the pipeline: the two steps where
// judgment is required and nothing else.
//
// Everything on the other side of this interface is deterministic Go — URL
// parsing, git, the verification run, the commit, the push. That split is the
// main design claim this example makes, and having it be an INTERFACE is what
// makes it checkable: the end-to-end test swaps in a scripted provider and the
// rest of the program does not notice.
type Brain interface {
	Analyze(ctx context.Context, in AnalysisInput) (Analysis, RunStats, error)
	Implement(ctx context.Context, in ImplementInput) (Implementation, RunStats, error)
}

type AnalysisInput struct {
	Ref           IssueRef
	Issue         *Issue
	LinkedPRs     []*LinkedPR
	Baseline      VerifyResult
	VerifyCommand string
}

type ImplementInput struct {
	Ref           IssueRef
	Issue         *Issue
	Analysis      Analysis
	Baseline      VerifyResult
	VerifyCommand string
	Branch        string
}

// agentBrain runs both phases as AgentKit agents against the configured model.
type agentBrain struct {
	base       core.AgentConfig // Model and Providers are already set
	workspace  *tools.Workspace
	progress   io.Writer
	progressMu sync.Mutex
	maxTurns   int
	budgetUSD  float64
	// extraPrograms widens the implementation phase's shell allowlist — the
	// verify command's program lands here, plus whatever the operator adds.
	extraPrograms []string
	showText      bool
	verbose       bool
}

// readOnlyTools are the tools the analysis phase gets. write_file and
// edit_file are absent, which is the containment: the analysis phase cannot
// modify the checkout even if the model decides mid-run that it would rather
// start fixing.
var readOnlyTools = []string{"read_file", "list_files", "find_files", "search_files", "execute"}

// writeTools adds the two mutating file tools for the implementation phase.
var writeTools = []string{"read_file", "write_file", "edit_file", "list_files", "find_files", "search_files", "execute", "run_command"}

// readOnlyPrograms is the analysis phase's shell allowlist: programs that
// report and do not change anything.
var readOnlyPrograms = []string{"git", "ls", "cat", "head", "tail", "wc", "rg", "grep", "find", "file"}

// buildPrograms is what an implementation phase needs on top of that: the
// toolchains that compile, format and test. The verify command's own program
// is appended at run time, because a phase that cannot run the suite it will
// be judged by is a phase set up to fail.
var buildPrograms = []string{"go", "gofmt", "make", "npm", "npx", "node", "python", "python3", "pytest", "uv", "cargo", "mkdir", "cp", "mv", "sed", "awk", "diff"}

// --------------------------------------------------------------- analysis --

func (b *agentBrain) Analyze(ctx context.Context, in AnalysisInput) (Analysis, RunStats, error) {
	var out Analysis

	agent, err := b.newAgent(phaseSpec{
		name:         "analyze",
		system:       analysisSystemPrompt,
		toolNames:    readOnlyTools,
		programs:     readOnlyPrograms,
		readOnly:     true,
		terminalTool: "submit_analysis",
		custom:       []core.Tool{submitAnalysisTool(&out)},
	})
	if err != nil {
		return out, RunStats{}, err
	}

	stats, err := b.drive(ctx, agent, "analyze", analysisPrompt(in))
	if err != nil {
		return out, stats, err
	}
	if out.Summary == "" {
		return out, stats, fmt.Errorf("the analysis phase ended (%s) without calling submit_analysis; "+
			"raise --max-turns or --budget, or narrow the issue", stats.StopReason)
	}
	if !out.Classification.Valid() {
		out.Classification = ClassBug
	}
	return out, stats, nil
}

// ---------------------------------------------------------- implementation --

func (b *agentBrain) Implement(ctx context.Context, in ImplementInput) (Implementation, RunStats, error) {
	var out Implementation

	programs := append(append([]string{}, readOnlyPrograms...), buildPrograms...)
	programs = append(programs, b.extraPrograms...)

	agent, err := b.newAgent(phaseSpec{
		name:         "implement",
		system:       implementSystemPrompt,
		toolNames:    writeTools,
		programs:     programs,
		readOnly:     false,
		terminalTool: "submit_implementation",
		custom:       []core.Tool{submitImplementationTool(&out)},
	})
	if err != nil {
		return out, RunStats{}, err
	}

	stats, err := b.drive(ctx, agent, "implement", implementPrompt(in))
	if err != nil {
		return out, stats, err
	}
	if out.Summary == "" {
		return out, stats, fmt.Errorf("the implementation phase ended (%s) without calling "+
			"submit_implementation; the working tree may hold partial changes — inspect the branch",
			stats.StopReason)
	}
	return out, stats, nil
}

// --------------------------------------------------------------- plumbing --

type phaseSpec struct {
	name         string
	system       string
	toolNames    []string
	programs     []string
	readOnly     bool
	terminalTool string
	custom       []core.Tool
}

func (b *agentBrain) newAgent(spec phaseSpec) (*agentkit.Agent, error) {
	cfg := b.base
	cfg.SystemPrompt = spec.system
	cfg.SessionID = "cleaner-" + spec.name

	// Three bounds, because they fail differently. Turns catch a model that
	// loops cheaply; the budget catches one that reads large files a few
	// expensive times; the sentinel tool is the INTENDED ending, and the other
	// two are what happens when the model never reaches it.
	cfg.StopPolicy = agentkit.StopAny(
		agentkit.StopAfterTurns(b.maxTurns),
		agentkit.StopOverBudget(b.budgetUSD),
		agentkit.StopWhenToolCalled(spec.terminalTool),
	)

	// The shipped restricted policy is the floor: an allowlist of program
	// names, and no shell operators. On top of it sits toolGuard, which knows
	// two things this program cares about and a generic policy cannot: that
	// git mutations belong to the pipeline rather than to the model, and that
	// nothing the model does should reach GitHub without passing through the
	// audited comment path.
	base := agentkit.RestrictedPolicy(agentkit.RestrictedOptions{
		AllowedPrograms:  spec.programs,
		TerminateOnBlock: false,
	})
	cfg.BeforeToolCall = toolGuard(base, spec.readOnly, func(msg string) {
		if b.verbose {
			b.printf("  blocked %s\n", msg)
		}
	})
	cfg.Middleware = append(append([]core.Middleware(nil), b.base.Middleware...),
		agentkit.RetryMiddleware(agentkit.RetryOptions{MaxAttempts: 3}),
	)

	built, err := tools.All(tools.Options{Workspace: b.workspace})
	if err != nil {
		return nil, err
	}

	agent, err := agentkit.NewAgent(cfg)
	if err != nil {
		return nil, err
	}
	for _, t := range append(selectTools(built, spec.toolNames...), spec.custom...) {
		if err := agent.RegisterTool(t); err != nil {
			return nil, err
		}
	}
	return agent, nil
}

// drive runs one phase and renders it as progress on stderr.
func (b *agentBrain) drive(ctx context.Context, agent *agentkit.Agent, phase, prompt string) (RunStats, error) {
	start := time.Now()

	stream, err := agent.Stream(ctx, prompt)
	if err != nil {
		return RunStats{Phase: phase}, err
	}
	for event := range stream.Events() {
		switch e := event.(type) {
		case core.TextDeltaEvent:
			if b.showText {
				b.print(e.Delta)
			}
		case core.TextEndEvent:
			if b.showText {
				b.println()
			}
		case core.ToolCallStartEvent:
			if b.verbose {
				b.printf("  → %s", e.Name)
			}
		case core.ToolCallEndEvent:
			if b.verbose {
				b.printf(" %s\n", firstLine(string(e.Block.Input), 90))
			}
		case core.ToolExecutionEndEvent:
			if b.verbose {
				status := "ok"
				if e.IsError {
					status = "ERROR"
				}
				b.printf("  ← %s: %s (%dms)\n", e.Name, status, e.ElapsedMS)
			}
		case core.ErrorEvent:
			b.printf("  [stream error] %s\n", e.Message)
		}
	}

	res, err := stream.RunResult()
	stats := RunStats{
		Phase:      phase,
		Turns:      res.TurnCount,
		StopReason: res.StopReason,
		Usage:      res.Usage,
		CostUSD:    res.Usage.CostUSD,
		Elapsed:    time.Since(start),
	}
	stats.ElapsedMS = stats.Elapsed.Milliseconds()
	return stats, err
}

func selectTools(all []core.Tool, names ...string) []core.Tool {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := make([]core.Tool, 0, len(names))
	for _, t := range all {
		if want[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

// mutatingGit are the git subcommands the agent may not run. Reading history
// is how you understand a bug; writing it is how a run stops being
// reproducible.
var mutatingGit = map[string]bool{
	"commit": true, "push": true, "merge": true, "rebase": true, "reset": true,
	"checkout": true, "switch": true, "branch": true, "cherry-pick": true,
	"revert": true, "stash": true, "clean": true, "tag": true, "am": true,
	"apply": true, "restore": true, "mv": true, "rm": true, "worktree": true,
	"remote": true, "config": true, "gc": true, "update-ref": true, "filter-branch": true,
}

func (b *agentBrain) print(args ...any) {
	b.progressMu.Lock()
	defer b.progressMu.Unlock()
	fmt.Fprint(b.progress, args...)
}

func (b *agentBrain) println(args ...any) {
	b.progressMu.Lock()
	defer b.progressMu.Unlock()
	fmt.Fprintln(b.progress, args...)
}

func (b *agentBrain) printf(format string, args ...any) {
	b.progressMu.Lock()
	defer b.progressMu.Unlock()
	fmt.Fprintf(b.progress, format, args...)
}

// toolGuard is the second half of the authorization boundary: the policy that
// knows about THIS application. It runs before the shipped restricted policy
// and can only narrow it.
func toolGuard(base core.BeforeToolCall, readOnly bool, log func(string)) core.BeforeToolCall {
	block := func(name, reason string) core.BeforeToolCallDecision {
		log(name + ": " + reason)
		return core.BeforeToolCallDecision{Block: true, Reason: reason}
	}
	return func(ctx context.Context, in core.BeforeToolCallContext) core.BeforeToolCallDecision {
		switch in.ToolName {
		case "write_file", "edit_file":
			if readOnly {
				return block(in.ToolName, "this phase is read-only: analyse the code, do not change it")
			}
		case "execute", "run_command":
			argv := commandWords(in)
			if len(argv) == 0 {
				break
			}
			switch baseName(argv[0]) {
			case "gh":
				return block(in.ToolName,
					"gh is not available to the agent: cleaner posts the analysis, the summary "+
						"and the pull request itself, so every write to the issue is audited")
			case "git":
				if sub := firstSubcommand(argv[1:]); mutatingGit[sub] {
					return block(in.ToolName, "git "+sub+" is the pipeline's job, not the agent's; "+
						"read-only git (status, log, diff, show, blame) is allowed")
				}
			}
		}
		return base(ctx, in)
	}
}

// commandWords normalizes the two shell tools into one argument vector.
func commandWords(in core.BeforeToolCallContext) []string {
	switch in.ToolName {
	case "execute":
		cmd, _ := in.Arguments["command"].(string)
		return strings.Fields(cmd)
	case "run_command":
		raw, _ := in.Arguments["argv"].([]any)
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// firstSubcommand skips git's global flags (`git -C dir commit`) to find the
// verb. A guard that read argv[1] blindly would wave that call straight
// through.
func firstSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a
		}
		if a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" {
			i++ // this flag takes a value
		}
	}
	return ""
}

// maxLen sets the schema's upper length bound. The combinators cover the
// common keywords; the struct is exported for the rest, which is the seam an
// application uses rather than a fork.
func maxLen(s *schema.Schema, n int) *schema.Schema {
	s.MaxLength = &n
	return s
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// ---------------------------------------------------- the terminating tools --

// submitAnalysisTool ends the analysis phase and hands back a validated
// struct.
//
// The alternative — asking for JSON in the final message and unmarshalling it
// — fails in a way this cannot: a model that wraps the object in a code fence,
// adds a sentence before it, or renames a field produces text that parses into
// nothing, and the failure surfaces as an empty analysis rather than as a
// correction the model can act on. Here the schema is enforced by the loop,
// and a bad call comes back to the model as a validation error it can fix on
// the next turn.
func submitAnalysisTool(out *Analysis) core.Tool {
	return core.Tool{
		Name: "submit_analysis",
		Description: "Submit the finished diagnosis and plan, ending the analysis phase. " +
			"Call this once, alone, when you understand the issue and know what to change.",
		InputSchema: schema.Object(
			schema.Prop("classification", schema.Enum("What kind of issue this is",
				"bug", "feature", "refactor", "performance")),
			schema.Prop("confidence", schema.Enum(
				"confirmed = you traced the exact code path; probable = mechanism identified, trigger not fully verified; suspected = hypothesis consistent with the evidence",
				"confirmed", "probable", "suspected")),
			schema.Prop("summary", maxLen(schema.String(
				"One line, imperative mood, under 72 characters, no trailing period — it becomes the commit subject"), 72)),
			schema.Prop("root_cause", schema.String(
				"Markdown. For a bug: why it happens, naming files, functions and line ranges. "+
					"For a feature: what is missing and where it belongs.")),
			schema.Prop("approach", schema.String(
				"Markdown. What you will change and why that is the right fix rather than a band-aid.")),
			schema.Prop("files", schema.Array(schema.Object(
				schema.Prop("path", schema.String("Repository-relative path")),
				schema.Prop("change", schema.String("What changes in this file and why")),
			), "The files you expect to modify, tests included").MinItemsN(1)),
			schema.Opt("assumptions", schema.Array(schema.String(),
				"Judgment calls you made where the issue was ambiguous")),
			schema.Opt("clarification", schema.Object(
				schema.Prop("question", schema.String("The one question that has to be answered")),
				schema.Prop("option_a", schema.String("First reading, and the fix it implies")),
				schema.Prop("option_b", schema.String("Second reading, and the fix it implies")),
			).Describe("Set ONLY when two readings of the issue lead to opposite fixes and the "+
				"codebase cannot choose between them. Setting this stops the run before any code changes.")),
		),
		PromptGuidelines: []string{
			"Finish the analysis by calling submit_analysis; a diagnosis written as prose is not a result.",
		},
		Execute: func(_ context.Context, in json.RawMessage) core.ToolResult {
			if err := json.Unmarshal(in, out); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			res := core.OKResult(map[string]any{"received": true})
			res.Terminate = true
			return res
		},
	}
}

func submitImplementationTool(out *Implementation) core.Tool {
	return core.Tool{
		Name: "submit_implementation",
		Description: "Report the finished change and end the implementation phase. " +
			"Call this once, alone, after the code is written and the project's checks pass.",
		InputSchema: schema.Object(
			schema.Prop("summary", schema.String("One to three sentences: what you changed and why")),
			schema.Prop("commit_subject", maxLen(schema.String(
				"Conventional-commits subject line, e.g. `fix(session): expire cached tokens`. "+
					"Under 72 characters, imperative mood, no trailing period."), 72)),
			schema.Prop("changes", schema.Array(schema.Object(
				schema.Prop("path", schema.String("Repository-relative path")),
				schema.Prop("change", schema.String("What you changed in it")),
			), "Every file you touched").MinItemsN(1)),
			schema.Opt("tests", schema.Array(schema.Object(
				schema.Prop("path", schema.String("Test file")),
				schema.Prop("covers", schema.String("What the test proves")),
			), "Tests you added or updated")),
			schema.Opt("limitations", schema.Array(schema.String(),
				"What this change does NOT fix, and anything a reviewer should check by hand")),
		),
		PromptGuidelines: []string{
			"Finish by calling submit_implementation; describe only what you actually changed.",
		},
		Execute: func(_ context.Context, in json.RawMessage) core.ToolResult {
			if err := json.Unmarshal(in, out); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			res := core.OKResult(map[string]any{"received": true})
			res.Terminate = true
			return res
		},
	}
}
