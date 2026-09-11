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
	"github.com/agentfox/agentkit-go/compaction"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/middleware"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/stop"
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
	// Instructions is the project's AGENTS.md or CLAUDE.md when one exists
	// and is small enough to inline; empty otherwise. See projectInstructions.
	Instructions string
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
// report and do not change anything. `find` is not here on purpose: its
// -exec and -delete make it a write tool, and find_files covers the reading
// half. (toolGuard refuses those flags anyway, for a phase that adds it back
// through --allow.)
var readOnlyPrograms = []string{"git", "ls", "cat", "head", "tail", "wc", "rg", "grep", "file"}

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
	cfg.StopPolicy = stop.Any(
		stop.AfterTurns(b.maxTurns),
		stop.OverBudget(b.budgetUSD),
		stop.WhenToolCalled(spec.terminalTool),
	)

	// The shipped restricted policy is the floor: an allowlist of program
	// names. The implementation phase may use shell operators — a build that
	// cannot pipe into grep or redirect a log is a build the model fights —
	// and the allowlist is documented as a floor rather than a sandbox. The
	// read-only phase gets no operators, because a redirection is a write.
	// On top of the policy sits toolGuard, which knows two things this program
	// cares about and a generic policy cannot: that git mutations belong to
	// the pipeline rather than to the model, and that nothing the model does
	// should reach GitHub without passing through the audited comment path.
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
		middleware.Retry(middleware.RetryOptions{MaxAttempts: 3}),
	)

	// Compaction. A phase that reads a dozen large files fills the context
	// window before it reaches its terminating tool, and the run then ends on
	// a provider error rather than on a result. The transform binds the
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
				"refused in this phase, so run one program per call. Output is truncated " +
				"from the END if it is large, so the tail of a long listing is preserved."
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

// readOnlyGit are the git subcommands the agent may run: the ones that
// report. Reading history is how you understand a bug; writing it is how a
// run stops being reproducible. It is an allowlist rather than a list of
// mutating verbs because git grows verbs (and `pull`, `fetch`, `clone`,
// `bisect` and `notes` were all missing from the first denylist). `branch`,
// `remote` and `config` have read-only forms and are handled by gitReadOnly.
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
	return "git " + sub + " is the pipeline's job, not the agent's; " + readOnlyGitHint, false
}

// findWriteFlags turn find into a write tool.
var findWriteFlags = []string{"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls"}

// gitSubcommand skips git's global flags (`git -C dir commit`) to find the
// verb and its index. A guard that read argv[0] blindly would wave that call
// straight through.
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
				return block(in.ToolName, "this phase is read-only: analyse the code, do not change it")
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
	case "gh":
		return "gh is not available to the agent: cleaner posts the analysis, the summary " +
			"and the pull request itself, so every write to the issue is audited", true
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
