// Command cleaner is an autonomous GitHub issue fixer: give it an issue URL
// and it diagnoses the problem, writes the fix on a branch, verifies it with
// the project's own checks, and opens a pull request — reporting what it did
// on the issue as it goes.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/cleaner https://github.com/acme/widgets/issues/42
//
// It is the AgentKit implementation of a coding-CLI skill (`/af-fix`): the
// same workflow, with the judgment left to a model and everything else —
// argument parsing, git, verification, landing — kept in Go where it can be
// tested. See examples/cleaner/README.md for the mapping, and for the four
// places where the skill's own ordering had to be corrected.
//
// Nothing is written to GitHub or to the repository until it has something to
// say, and --dry-run stops it writing at all:
//
//	go run ./examples/cleaner --dry-run https://github.com/acme/widgets/issues/42
//	go run ./examples/cleaner --land=branch --budget=2 https://github.com/acme/widgets/issues/42
//	go run ./examples/cleaner --issue-file ./examples/cleaner/testdata/issue-1.json \
//	    --dir /tmp/checkout --dry-run https://github.com/acme/widgets/issues/1
//
// See examples/README.md for the full environment-variable table.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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

// Exit codes. An autonomous tool is usually run by something else, and "it
// failed" is not enough for that something else to decide what to do next.
const (
	exitOK            = 0
	exitFailed        = 1
	exitUsage         = 2
	exitClarification = 3 // stopped on purpose: the issue is ambiguous
	exitUnverified    = 4 // a change was written but the checks do not pass
)

func main() {
	os.Exit(run())
}

func run() int {
	dir := flag.String("dir", ".", "the repository to work in; the agent's file tools cannot reach outside it")
	land := flag.String("land", "pr", "how to land the fix: pr, branch, merge or none")
	dryRun := flag.Bool("dry-run", false, "make no remote changes: no push, no pull request, no comments. The branch and the commit are still made locally, because the implementation phase edits real files")
	modelSpec := flag.String("model", "", "model spec, e.g. anthropic/claude-sonnet-5 (default $AGENTKIT_MODEL)")
	maxTurns := flag.Int("max-turns", 100, "per-phase turn ceiling")
	budget := flag.Float64("budget", 5.0, "per-phase spend ceiling in dollars")
	verifyCmd := flag.String("verify", "", "the command that decides success (default: detected from the repository)")
	noVerify := flag.Bool("no-verify", false, "do not run any verification command (the result is reported as unverified)")
	verifyTimeout := flag.Duration("verify-timeout", 10*time.Minute, "timeout for one verification run")
	issueFile := flag.String("issue-file", "", "read the issue from this JSON file instead of GitHub (offline; implies no writes to GitHub)")
	journal := flag.String("journal", "", "append a JSONL record of every step to this file")
	allow := flag.String("allow", "", "extra programs the implementation phase may run, comma-separated")
	showText := flag.Bool("show-text", false, "print the model's prose as well as its tool calls")
	pushAttempts := flag.Int("push-attempts", 4, "how many times to try pushing before giving up")
	verbose := flag.Bool("verbose", false, "verbose output: tool calls, timing and cost diagnostics")

	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		usage()
		return exitUsage
	}
	ref, err := ParseIssueURL(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		usage()
		return exitUsage
	}
	landing, err := ParseLanding(*land)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return exitUsage
	}

	// Ctrl-C cancels the context, which aborts the in-flight model request and
	// the running verification command. Whatever the agent already wrote stays
	// on the branch: an interrupted run is not a rolled-back one, and
	// pretending otherwise would be worse than saying so.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The workspace is the containment boundary for the file tools: every path
	// the model asks for is resolved against this root, symlinks included.
	ws, err := tools.NewWorkspace(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return exitFailed
	}

	verify := *verifyCmd
	if *noVerify {
		verify = ""
	} else if verify == "" {
		verify = DetectVerifyCommand(ws.Root)
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

	var hub Hub = newGHHub(execRunner)
	if *issueFile != "" {
		// An offline issue implies a dry run: there is no backend to post to,
		// and discovering that one comment at a time is not a useful mode.
		hub = &fileHub{path: *issueFile}
		*dryRun = true
	}
	if *dryRun {
		hub = &dryHub{inner: hub, out: os.Stderr}
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
		extraPrograms: extraPrograms(*allow, verify),
		showText:      *showText,
		verbose:       *verbose,
	}

	// The verification command is repository code and runs without the
	// credentials in this process's environment; git and gh keep theirs.
	opts := Options{
		Ref: ref, Dir: ws.Root, Hub: hub, Git: NewGit(ws.Root, execRunner), Brain: brain,
		Run: reducedEnvRunner, VerifyCommand: verify, VerifyTimeout: *verifyTimeout,
		Landing: landing, DryRun: *dryRun, PushAttempts: *pushAttempts,
		Out: os.Stderr, JournalPath: *journal, Verbose: *verbose,
	}

	if *verbose {
		banner(opts, model.ID)
	}
	res, runErr := Run(ctx, opts)
	summary(res, runErr, *verbose)

	switch {
	case res.NeedsClarification && runErr == nil:
		// "Stopped on purpose" is only true when the question reached the
		// issue; a question that could not be posted is a failed run.
		return exitClarification
	case runErr != nil && res.Stage == "verify":
		return exitUnverified // code was written, and the checks reject it
	case runErr != nil:
		return exitFailed
	default:
		return exitOK
	}
}

func banner(o Options, modelID string) {
	fmt.Fprintf(os.Stderr, "cleaner: %s#%d\n", o.Ref.Slug(), o.Ref.Number)
	fmt.Fprintf(os.Stderr, "  repo:   %s\n", o.Dir)
	fmt.Fprintf(os.Stderr, "  model:  %s\n", modelID)
	if o.VerifyCommand == "" {
		fmt.Fprintf(os.Stderr, "  verify: (none detected — the result will be reported as unverified)\n")
	} else {
		fmt.Fprintf(os.Stderr, "  verify: %s\n", o.VerifyCommand)
	}
	mode := string(o.Landing)
	if o.DryRun {
		mode += " (dry run: local only — nothing is pushed, opened or posted)"
	}
	fmt.Fprintf(os.Stderr, "  land:   %s\n", mode)
}

// summary is the last thing printed, and it reports what happened rather than
// what was attempted. af-fix's final banner says "✅ fixed and merged to main"
// unconditionally, including on the paths where it skipped the push and the
// pull request.
func summary(res *Result, err error, verbose bool) {
	summaryTo(os.Stderr, res, err, verbose)
}

func summaryTo(w io.Writer, res *Result, err error, verbose bool) {
	fmt.Fprintln(w)
	switch {
	case res.NeedsClarification && err == nil:
		fmt.Fprintf(w, "[cleaner] ? issue #%d is ambiguous — a question was posted and nothing was changed.\n", res.Ref.Number)
	case res.NeedsClarification:
		fmt.Fprintf(w, "[cleaner] ✗ issue #%d is ambiguous, and the question could not be posted; nothing was changed.\n", res.Ref.Number)
	case err != nil:
		fmt.Fprintf(w, "[cleaner] ✗ issue #%d NOT fixed (failed during %s).\n", res.Ref.Number, res.Stage)
	default:
		fmt.Fprintf(w, "[cleaner] ✓ issue #%d fixed.\n", res.Ref.Number)
	}

	line := func(k, v string) {
		if v != "" {
			fmt.Fprintf(w, "  %-9s %s\n", k+":", v)
		}
	}
	line("branch", res.Branch)
	line("commit", res.Commit)
	if res.WIPCommit != "" {
		line("wip", fmt.Sprintf("%s on %s (unverified; the checkout is back on %s)", res.WIPCommit, res.Branch, res.BaseBranch))
	}
	line("pr", res.PRURL)
	if res.Verification.Command != "" {
		line("verify", fmt.Sprintf("%s — %s", res.Verification.Command, res.Verification.Status()))
	}
	if len(res.Changed) > 0 {
		line("files", fmt.Sprintf("%d changed: %s", len(res.Changed), strings.Join(res.Changed, ", ")))
	}
	for _, s := range res.Stats {
		if verbose {
			fmt.Fprintf(w, "  %s\n", s)
		} else {
			fmt.Fprintf(w, "  %s\n", s.TimingWithoutCost())
		}
	}
	if verbose {
		if c := res.CostUSD(); c > 0 {
			line("cost", fmt.Sprintf("$%.4f", c))
		}
	}
	for _, wMsg := range res.Warnings {
		fmt.Fprintf(w, "  ! %s\n", wMsg)
	}
	if err != nil {
		fmt.Fprintf(w, "\nerror: %v\n", err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cleaner — fix a GitHub issue, autonomously.

usage: cleaner [flags] https://github.com/{owner}/{repo}/issues/{number}

example:
  cleaner --dir ~/src/widgets https://github.com/acme/widgets/issues/42

flags:
`)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `
exit codes: 0 fixed · 1 failed · 2 usage · 3 needs clarification · 4 written but unverified
`)
}

// extraPrograms widens the implementation phase's shell allowlist with the
// verification command's own program and anything the operator named.
func extraPrograms(allow, verifyCommand string) []string {
	var out []string
	for _, s := range strings.Split(allow, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if p := verifyProgram(verifyCommand); p != "" {
		out = append(out, p)
	}
	return out
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

// checkCredentials fails BEFORE the first request, with a message naming the
// variable to set — rather than after a 401 that names none of them.
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
