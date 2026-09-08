package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	afspec "github.com/agent-fox-dev/spec"
	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/tools"
)

// Brain is the model-driven part of the run: the three archetype sessions.
// Everything on the other side of this interface is deterministic Go, and the
// end-to-end test swaps in a scripted provider without the pipeline noticing.
type Brain interface {
	Implement(ctx context.Context, in SessionInput) (GroupReport, RunStats, error)
	Gate(ctx context.Context, in SessionInput) (GateReport, RunStats, error)
	Verify(ctx context.Context, in SessionInput) (Verdicts, RunStats, error)
}

// SessionInput is what one session is given. Context is the assembled
// `## Context` section; Task is the user turn.
type SessionInput struct {
	Group   afspec.TaskGroup
	Context string
	Task    string
}

// agentBrain runs the sessions as AgentKit agents against the configured
// model.
type agentBrain struct {
	base       core.AgentConfig // Model and Providers are already set
	workspace  *tools.Workspace
	progress   io.Writer
	progressMu sync.Mutex
	maxTurns   int
	budgetUSD  float64
	timeout    time.Duration
	// extraPrograms widens the coder's shell allowlist — the pack's own test
	// commands land here, plus whatever the operator adds.
	extraPrograms []string
	showText      bool
	verbose       bool
}

// writeTools is the coder's set. readOnlyTools is what a gate and a verifier
// get: the tools to write are not registered, which is the containment.
var (
	writeTools    = []string{"read_file", "write_file", "edit_file", "list_files", "find_files", "search_files", "execute", "run_command"}
	readOnlyTools = []string{"read_file", "list_files", "find_files", "search_files", "execute"}
)

// coderPrograms is agent-fox's default Bash allowlist (core/security.py), the
// set its coder gets, minus `rm`, `chmod`, `curl` and `wget`: the first two
// are what the retry path's reset exists to undo, and a tool that makes
// outbound requests on the model's behalf is a different risk class.
var coderPrograms = []string{
	"git", "python", "python3", "uv", "pip", "pytest", "ruff", "mypy", "npm", "npx", "node",
	"make", "cargo", "go", "gofmt", "rustc", "gcc", "ls", "cat", "mkdir", "cp", "mv", "find",
	"grep", "rg", "sed", "awk", "echo", "head", "tail", "wc", "sort", "diff", "touch", "tar",
	"gzip", "which", "printenv", "date", "pwd", "test", "true", "false",
}

// readOnlyPrograms is agent-fox's reviewer allowlist, plus the test runners a
// gate and a verifier exist to run.
var readOnlyPrograms = []string{"ls", "cat", "git", "grep", "rg", "find", "head", "tail", "wc",
	"make", "go", "uv", "pytest", "npm", "npx", "cargo", "python", "python3", "ruff", "mypy", "true", "false", "test"}

func (b *agentBrain) Implement(ctx context.Context, in SessionInput) (GroupReport, RunStats, error) {
	var out GroupReport
	programs := append(append([]string{}, coderPrograms...), b.extraPrograms...)
	agent, err := b.newAgent(phaseSpec{
		name: "coder", system: SystemPrompt(coderProfile, in.Context),
		toolNames: writeTools, programs: programs, readOnly: false,
		terminalTool: "submit_group", custom: []core.Tool{submitGroupTool(&out)},
	})
	if err != nil {
		return out, RunStats{}, err
	}
	stats, err := b.drive(ctx, agent, fmt.Sprintf("coder %d", in.Group.Id), in.Task)
	if err != nil {
		return out, stats, err
	}
	if strings.TrimSpace(out.Summary) == "" {
		return out, stats, fmt.Errorf("the coder session ended (%s) without calling submit_group", stats.StopReason)
	}
	return out, stats, nil
}

func (b *agentBrain) Gate(ctx context.Context, in SessionInput) (GateReport, RunStats, error) {
	var out GateReport
	called := false
	programs := append(append([]string{}, readOnlyPrograms...), b.extraPrograms...)
	agent, err := b.newAgent(phaseSpec{
		name: "gate", system: SystemPrompt(gateProfile, in.Context),
		toolNames: readOnlyTools, programs: programs, readOnly: true,
		terminalTool: "submit_gate", custom: []core.Tool{submitGateTool(&out, &called)},
	})
	if err != nil {
		return out, RunStats{}, err
	}
	stats, err := b.drive(ctx, agent, fmt.Sprintf("gate %d", in.Group.Id), in.Task)
	if err != nil {
		return out, stats, err
	}
	if !called {
		return out, stats, fmt.Errorf("the gate session ended (%s) without calling submit_gate", stats.StopReason)
	}
	return out, stats, nil
}

func (b *agentBrain) Verify(ctx context.Context, in SessionInput) (Verdicts, RunStats, error) {
	var out Verdicts
	programs := append(append([]string{}, readOnlyPrograms...), b.extraPrograms...)
	agent, err := b.newAgent(phaseSpec{
		name: "verifier", system: SystemPrompt(verifierProfile, in.Context),
		toolNames: readOnlyTools, programs: programs, readOnly: true,
		terminalTool: "submit_verdicts", custom: []core.Tool{submitVerdictsTool(&out)},
	})
	if err != nil {
		return out, RunStats{}, err
	}
	stats, err := b.drive(ctx, agent, "verifier", in.Task)
	if err != nil {
		return out, stats, err
	}
	if out.OverallVerdict == "" {
		return out, stats, fmt.Errorf("the verifier session ended (%s) without calling submit_verdicts", stats.StopReason)
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
	cfg.SessionID = "flatline-" + spec.name

	// Four bounds, because they fail differently. Turns catch a model that
	// loops cheaply; the budget catches one that reads large files a few
	// expensive times; the duration is agent-fox's session timeout; the
	// sentinel tool is the INTENDED ending.
	policies := []core.StopPolicy{
		agentkit.StopAfterTurns(b.maxTurns),
		agentkit.StopOverBudget(b.budgetUSD),
		agentkit.StopWhenToolCalled(spec.terminalTool),
	}
	if b.timeout > 0 {
		policies = append(policies, agentkit.StopAfterDuration(b.timeout))
	}
	cfg.StopPolicy = agentkit.StopAny(policies...)

	// The shipped restricted policy is the floor — an allowlist of program
	// names and no shell operators, which is also agent-fox's Bash rule. On
	// top of it sits toolGuard: git mutations belong to the pipeline, and a
	// read-only phase stays read-only.
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

// drive runs one session and renders it as progress on stderr.
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
			b.printf("\n  [stream error] %s\n", e.Message)
		}
	}
	res, err := stream.RunResult()
	stats := RunStats{
		Phase: phase, Turns: res.TurnCount, StopReason: res.StopReason,
		Usage: res.Usage, CostUSD: res.Usage.CostUSD, Elapsed: time.Since(start),
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

// mutatingGit are the git subcommands the agent may not run. agent-fox tells
// its coder not to switch branches, rebase, merge or push and lets it commit;
// here even the commit is the pipeline's, so the summary's "committed as"
// means exactly one thing.
var mutatingGit = map[string]bool{
	"commit": true, "push": true, "merge": true, "rebase": true, "reset": true,
	"checkout": true, "switch": true, "branch": true, "cherry-pick": true,
	"revert": true, "stash": true, "clean": true, "tag": true, "am": true,
	"apply": true, "restore": true, "mv": true, "rm": true, "worktree": true,
	"remote": true, "config": true, "gc": true, "update-ref": true, "filter-branch": true,
}

// toolGuard is the application-specific half of the authorization boundary.
// It runs before the shipped restricted policy and can only narrow it.
func toolGuard(base core.BeforeToolCall, readOnly bool, log func(string)) core.BeforeToolCall {
	block := func(name, reason string) core.BeforeToolCallDecision {
		log(name + ": " + reason)
		return core.BeforeToolCallDecision{Block: true, Reason: reason}
	}
	return func(ctx context.Context, in core.BeforeToolCallContext) core.BeforeToolCallDecision {
		switch in.ToolName {
		case "write_file", "edit_file":
			if readOnly {
				return block(in.ToolName, "this session is read-only: run the checks, do not change anything")
			}
		case "execute", "run_command":
			argv := commandWords(in)
			if len(argv) == 0 {
				break
			}
			if baseName(argv[0]) == "git" {
				if sub := firstSubcommand(argv[1:]); mutatingGit[sub] {
					return block(in.ToolName, "git "+sub+" is flatline's job, not the agent's; "+
						"read-only git (status, log, diff, show, blame) is allowed")
				}
			}
		}
		return base(ctx, in)
	}
}

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

// firstSubcommand skips git's global flags (`git -C dir commit`).
func firstSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a
		}
		if a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" {
			i++
		}
	}
	return ""
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func maxLen(s *schema.Schema, n int) *schema.Schema {
	s.MaxLength = &n
	return s
}

// ---------------------------------------------------- the terminating tools --

// submitGroupTool ends a coder session with a validated report. It is
// agent-fox's `.agent-fox/session-summary.json` — the file the coder is asked
// to write and the orchestrator reads — as a tool call the loop validates.
func submitGroupTool(out *GroupReport) core.Tool {
	return core.Tool{
		Name: "submit_group",
		Description: "Report the finished task group and end the session. " +
			"Call this once, alone, after the subtasks are implemented and the quality gates pass — " +
			"or when you have to stop, saying what is unaddressed.",
		InputSchema: schema.Object(
			schema.Prop("summary", schema.String(
				"500-1000 characters: what was surprising or non-obvious — edge cases, API quirks, "+
					"design decisions. Include the task group and spec name.")),
			schema.Prop("commit_subject", maxLen(schema.String(
				"Conventional-commits subject line, e.g. `feat(counter): add Counter.Add`. "+
					"Under 72 characters, imperative mood, no trailing period."), 72)),
			schema.Prop("changes", schema.Array(schema.Object(
				schema.Prop("path", schema.String("Repository-relative path")),
				schema.Prop("change", schema.String("What you changed in it")),
			), "Every file you touched").MinItemsN(1)),
			schema.Opt("tests_added_or_modified", schema.Array(schema.Object(
				schema.Prop("path", schema.String("Test file")),
				schema.Prop("description", schema.String("What the test proves")),
			), "Test files changed; omit when none")),
			schema.Opt("gotchas", schema.Array(schema.String(), "Fragile patterns, race conditions, serialization quirks")),
			schema.Opt("assumptions", schema.Array(schema.String(), "Things that might not hold for later groups")),
			schema.Opt("rejected_approaches", schema.Array(schema.Object(
				schema.Prop("approach", schema.String()),
				schema.Prop("reason", schema.String()),
			), "Dead ends, so future coders skip them")),
			schema.Opt("unaddressed", schema.Array(schema.String(),
				"Subtasks or checks you could not complete, and why")),
		),
		PromptGuidelines: []string{
			"Finish by calling submit_group; a summary written as prose is not a result.",
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

func submitGateTool(out *GateReport, called *bool) core.Tool {
	return core.Tool{
		Name: "submit_gate",
		Description: "Report the checkpoint's results and end the session. " +
			"Call this once, alone, after every verification command has been run.",
		InputSchema: schema.Object(
			schema.Prop("passed", schema.Bool("true only if every check passed")),
			schema.Prop("results", schema.Array(schema.Object(
				schema.Prop("check", schema.String("The check or command, as listed in the subtask")),
				schema.Prop("passed", schema.Bool()),
				schema.Prop("output", schema.String("The relevant tail of the output; empty when it passed")),
			), "One entry per check").MinItemsN(1)),
			schema.Opt("notes", schema.String("Anything a human should know")),
		),
		PromptGuidelines: []string{"Finish by calling submit_gate; a verdict written as prose is not a result."},
		Execute: func(_ context.Context, in json.RawMessage) core.ToolResult {
			if err := json.Unmarshal(in, out); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			*called = true
			res := core.OKResult(map[string]any{"received": true})
			res.Terminate = true
			return res
		},
	}
}

func submitVerdictsTool(out *Verdicts) core.Tool {
	return core.Tool{
		Name:        "submit_verdicts",
		Description: "Submit the verification verdicts and end the session. Call this once, alone.",
		InputSchema: schema.Object(
			schema.Prop("verdicts", schema.Array(schema.Object(
				schema.Prop("requirement_id", schema.String("e.g. 05-REQ-1.1")),
				schema.Prop("verdict", schema.Enum("PASS or FAIL", "PASS", "FAIL")),
				schema.Prop("evidence", schema.String("For PASS: which test proves it. For FAIL: what is wrong and what needs to change.")),
			), "One per requirement in the checklist").MinItemsN(1)),
			schema.Prop("overall_verdict", schema.Enum("FAIL if any individual verdict is FAIL", "PASS", "FAIL")),
			schema.Prop("summary", schema.String("One or two sentences")),
		),
		PromptGuidelines: []string{"Finish by calling submit_verdicts; verdicts written as prose are not a result."},
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
