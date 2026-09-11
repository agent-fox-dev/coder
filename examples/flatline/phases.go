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
	"github.com/agentfox/agentkit-go/compaction"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/stop"
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
		stop.AfterTurns(b.maxTurns),
		stop.OverBudget(b.budgetUSD),
		stop.WhenToolCalled(spec.terminalTool),
	}
	if b.timeout > 0 {
		policies = append(policies, stop.AfterDuration(b.timeout))
	}
	cfg.StopPolicy = stop.Any(policies...)

	// The shipped restricted policy is the floor — an allowlist of program
	// names. The coder may use shell operators (the pack's own test commands
	// are shell lines with `&&` in them); the read-only sessions may not,
	// because a redirection is a write. On top of the policy sits toolGuard:
	// git mutations belong to the pipeline, a read-only phase stays
	// read-only, and every simple command on a line is checked, not only the
	// first.
	base := agentkit.RestrictedPolicy(agentkit.RestrictedOptions{
		AllowedPrograms:     spec.programs,
		AllowShellOperators: !spec.readOnly,
		TerminateOnBlock:    false,
	})
	cfg.BeforeToolCall = toolGuard(base, spec.readOnly, func(msg string) {
		if b.verbose {
			b.printf("  blocked %s\n", msg)
		}
	})
	cfg.Middleware = append(append([]core.Middleware(nil), b.base.Middleware...),
		agentkit.RetryMiddleware(agentkit.RetryOptions{MaxAttempts: 3}),
	)

	// Compaction. A coder session over a large group fills the context
	// window before it reaches submit_group, and the session then ends on a
	// provider error rather than on a result. The transform binds the
	// agent's own history, so the history is made first and handed to the
	// constructor.
	history := core.NewConversationHistory()
	installCompaction(&cfg, history, func(err error) {
		b.printf("  [compaction] %v\n", err)
	})

	built, err := tools.All(tools.Options{Workspace: b.workspace})
	if err != nil {
		return nil, err
	}
	agent, err := agentkit.NewAgentWithHistory(cfg, history)
	if err != nil {
		return nil, err
	}
	for _, t := range append(selectTools(built, spec.toolNames...), spec.custom...) {
		if t.Name == "execute" && spec.readOnly {
			// The shipped description advertises operators the read-only
			// policy refuses; a model told they work wastes turns finding out.
			t.Description = "Run one plain command. Pipes, redirection, &&, ; and $() are " +
				"refused in this session, so run one program per call. Output is truncated " +
				"from the END if it is large, so the tail of a failing build is preserved."
		}
		if err := agent.RegisterTool(t); err != nil {
			return nil, err
		}
	}
	return agent, nil
}

// compactionReserveTokens bounds the summary a compaction writes: its
// max_tokens is 0.8 × this, clamped to the model's own ceiling.
const compactionReserveTokens = 8000

// installCompaction sets cfg.TransformContext to summarize the transcript in
// place once it passes 60% of the model's context window. The summarizer
// calls the provider directly, off the middleware path, which is why it needs
// the provider rather than the agent. A model with no registered provider
// gets no compaction — the run fails on the missing provider anyway.
func installCompaction(cfg *core.AgentConfig, history *core.ConversationHistory, onError func(error)) {
	if cfg.Model == nil || cfg.Providers == nil {
		return
	}
	p, ok := cfg.Providers.Get(cfg.Model.API)
	if !ok {
		return
	}
	client := core.ClientFunc(p.Stream)
	cfg.TransformContext = compaction.NewContextTransform(compaction.Deps{
		Strategy:       compaction.Summarization{ThresholdFraction: 0.6},
		Summarizer:     compaction.ModelSummarizer(client, cfg.Model, compactionReserveTokens),
		TurnSummarizer: compaction.ModelTurnSummarizer(client, cfg.Model, compactionReserveTokens),
		History:        history,
		Model:          cfg.Model,
		OnError:        onError,
	})
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

// readOnlyGit are the git subcommands the agent may run: the ones that
// report. agent-fox tells its coder not to switch branches, rebase, merge or
// push and lets it commit; here even the commit is the pipeline's, so the
// summary's "committed as" means exactly one thing. It is an allowlist rather
// than a list of mutating verbs because git grows verbs (and `pull`, `fetch`,
// `clone`, `bisect` and `notes` were all missing from the first denylist).
// `branch`, `remote` and `config` have read-only forms and are handled by
// gitReadOnly.
var readOnlyGit = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "blame": true,
	"rev-parse": true, "rev-list": true, "ls-files": true, "ls-tree": true,
	"grep": true, "cat-file": true, "describe": true, "shortlog": true, "name-rev": true,
}

// readOnlyGitFlags are the arguments under which `branch`, `remote` and
// `config` only read.
var readOnlyGitFlags = map[string]map[string]bool{
	"branch": {"-a": true, "--all": true, "-r": true, "--remotes": true, "-l": true, "--list": true,
		"-v": true, "-vv": true, "--verbose": true, "--show-current": true, "--no-color": true},
	"remote": {"-v": true, "--verbose": true, "show": true, "get-url": true},
	"config": {"--get": true, "--get-all": true, "--get-regexp": true, "--list": true, "-l": true},
}

const readOnlyGitHint = "read-only git is allowed: status, log, diff, show, blame, rev-parse, " +
	"ls-files, grep, cat-file, describe, shortlog, name-rev, branch --list, remote -v, config --get"

// gitReadOnly decides one git invocation (argv without the leading "git").
// It returns the reason when the call is refused.
func gitReadOnly(args []string) (reason string, ok bool) {
	sub, i := gitSubcommand(args)
	for _, a := range args {
		if strings.HasPrefix(a, "--output") {
			return "git --output writes a file; run the command without it", false
		}
	}
	for _, a := range args[:i] {
		// `-c core.fsmonitor=…`, `--config-env` and `--exec-path` make git
		// run a program of the model's choosing before any subcommand does.
		if a == "-c" || strings.HasPrefix(a, "--config-env") || strings.HasPrefix(a, "--exec-path") {
			return "git " + a + " is not allowed; " + readOnlyGitHint, false
		}
	}
	if readOnlyGit[sub] {
		return "", true
	}
	if flags, known := readOnlyGitFlags[sub]; known {
		rest := args[i+1:]
		switch sub {
		case "remote":
			if len(rest) == 0 || flags[rest[0]] {
				return "", true
			}
		case "config":
			if len(rest) > 0 && flags[rest[0]] {
				return "", true
			}
		default: // branch: listing only
			listing := true
			for _, a := range rest {
				listing = listing && flags[a]
			}
			if listing {
				return "", true
			}
		}
	}
	if sub == "" {
		sub = "(no subcommand)"
	}
	return "git " + sub + " is flatline's job, not the agent's; " + readOnlyGitHint, false
}

// findWriteFlags turn find into a write tool.
var findWriteFlags = []string{"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls"}

// gitSubcommand skips git's global flags (`git -C dir commit`) to find the
// verb and its index.
func gitSubcommand(args []string) (string, int) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a, i
		}
		if a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" {
			i++ // this flag takes a value
		}
	}
	return "", len(args)
}

// toolGuard is the application-specific half of the authorization boundary.
// It runs before the shipped restricted policy and can only narrow it.
//
// An `execute` command is judged one simple command at a time: `ls; git push`
// is two commands, and the second is the one that matters. The shipped policy
// only looks at the first program once operators are allowed, so it too is
// applied to every segment.
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
			for _, argv := range commandVectors(in) {
				if reason, blocked := guardProgram(argv); blocked {
					return block(in.ToolName, reason)
				}
			}
			if d := base(ctx, in); d.Block {
				return d
			}
			if in.ToolName == "execute" {
				cmd, _ := in.Arguments["command"].(string)
				if segs := shellSegments(cmd); len(segs) > 1 {
					for _, seg := range segs[1:] {
						if d := base(ctx, withCommand(in, seg)); d.Block {
							return d
						}
					}
				}
			}
			return core.BeforeToolCallDecision{}
		}
		return base(ctx, in)
	}
}

// guardProgram applies the application's rules to one argument vector.
func guardProgram(argv []string) (reason string, blocked bool) {
	if len(argv) == 0 {
		return "", false
	}
	switch baseName(argv[0]) {
	case "git":
		if reason, ok := gitReadOnly(argv[1:]); !ok {
			return reason, true
		}
	case "find":
		for _, a := range argv[1:] {
			for _, f := range findWriteFlags {
				if a == f {
					return "find " + f + " changes or runs things; use find_files to look, and the file tools to change", true
				}
			}
		}
	}
	return "", false
}

// commandVectors normalizes the two shell tools into argument vectors, one
// per simple command. Leading NAME=value assignments are dropped, the way the
// shell (and the shipped policy's firstProgram) drop them: `GIT_AUTHOR_NAME=x
// git commit` runs git, and a guard that read argv[0] would see an
// environment variable.
func commandVectors(in core.BeforeToolCallContext) [][]string {
	switch in.ToolName {
	case "execute":
		cmd, _ := in.Arguments["command"].(string)
		var out [][]string
		for _, seg := range shellSegments(cmd) {
			if argv := commandWords(strings.Fields(seg)); len(argv) > 0 {
				out = append(out, argv)
			}
		}
		return out
	case "run_command":
		raw, _ := in.Arguments["argv"].([]any)
		words := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok {
				words = append(words, s)
			}
		}
		if argv := commandWords(words); len(argv) > 0 {
			return [][]string{argv}
		}
	}
	return nil
}

// commandWords drops leading environment assignments and the quoting a
// segment may carry, leaving the words the guard classifies.
func commandWords(words []string) []string {
	var out []string
	for _, w := range words {
		if out == nil {
			if i := strings.IndexByte(w, '='); i > 0 && !strings.ContainsAny(w[:i], "/") {
				continue
			}
		}
		w = strings.TrimRight(strings.Trim(w, `"'`), ")")
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

// shellSegments splits a POSIX-sh command line into its simple commands at
// the unquoted `;`, `|`, `||`, `&&`, `&`, newline, subshell and command
// substitution boundaries. Redirections (`2>&1`, `&>`) are not boundaries.
// Quoting follows the shipped policy's grammar: nothing splits inside single
// quotes; inside double quotes only backtick and `$(` start a new command.
//
// It is a classifier, not a parser: an odd construct yields a fragment that
// looks like a program name and gets refused, which is the safe direction.
func shellSegments(cmd string) []string {
	var segs []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segs = append(segs, s)
		}
		cur.Reset()
	}
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		var next byte
		if i+1 < len(cmd) {
			next = cmd[i+1]
		}
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
			cur.WriteByte(c)
		case inDouble:
			switch c {
			case '"':
				inDouble = false
				cur.WriteByte(c)
			case '\\':
				cur.WriteByte(c)
				if i+1 < len(cmd) {
					i++
					cur.WriteByte(cmd[i])
				}
			case '`':
				flush()
			case '$':
				if next == '(' {
					flush()
					i++
				} else {
					cur.WriteByte(c)
				}
			default:
				cur.WriteByte(c)
			}
		default:
			switch c {
			case '\'':
				inSingle = true
				cur.WriteByte(c)
			case '"':
				inDouble = true
				cur.WriteByte(c)
			case '\\':
				cur.WriteByte(c)
				if i+1 < len(cmd) {
					i++
					cur.WriteByte(cmd[i])
				}
			case ';', '\n', '|', '(', ')', '`':
				flush()
			case '&':
				var prev byte
				if i > 0 {
					prev = cmd[i-1]
				}
				if prev == '>' || prev == '<' || next == '>' {
					cur.WriteByte(c) // a redirection, not a list operator
				} else {
					flush()
				}
			case '$':
				if next == '(' {
					flush()
					i++
				} else {
					cur.WriteByte(c)
				}
			default:
				cur.WriteByte(c)
			}
		}
	}
	flush()
	return segs
}

// withCommand is the interceptor context for one segment of a command.
func withCommand(in core.BeforeToolCallContext, cmd string) core.BeforeToolCallContext {
	args := make(map[string]any, len(in.Arguments))
	for k, v := range in.Arguments {
		args[k] = v
	}
	args["command"] = cmd
	in.Arguments = args
	return in
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
