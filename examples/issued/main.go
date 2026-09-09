// Command issued turns a problem report into a structured GitHub issue.
//
// It is the `af-issue` skill — a ~450-line prompt that asks a coding CLI to
// diagnose a bug and file it — rebuilt as a program. The model still does the
// part that needs a model. Everything the skill could only ask for politely is
// now a mechanism: the read-only mandate is a tool policy, the issue template
// is a JSON schema, the "cite real files" rule is a path check in a tool
// handler, and filing lives outside the agent where no model output can reach
// it.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//
//	go run ./examples/issued "panic: nil map write in loop.go when a tool result arrives after abort"
//	go run ./examples/issued ./crash.log --dir .
//	go run ./examples/issued https://github.com/owner/repo/issues/42 --label af:fix
//	go run ./examples/issued https://github.com/owner/repo/issues/42 -overwrite
//	kubectl logs deploy/api | go run ./examples/issued -
//
// It creates the issue on GitHub unless you pass --dry-run.
//
//	AGENTKIT_MODEL=openai/gpt-5.6-terra go run ./examples/issued ./crash.log
//
// See examples/README.md for the full environment-variable table, and
// examples/issued/README.md for how the pieces fit together.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/agentfox/agentkit-go/tools"
)

// Exit codes: 1 is a failed run, 2 is a usage error — nothing to triage, or
// flags that do not make sense together — before a token is spent.
const (
	exitFailed = 1
	exitUsage  = 2
)

// errUsage marks a failure the operator caused with the command line.
var errUsage = errors.New("usage")

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, errUsage) || errors.Is(err, ErrNoInput) {
			os.Exit(exitUsage)
		}
		os.Exit(exitFailed)
	}
}

type cliConfig struct {
	dir       string
	repo      string
	dryRun    bool
	overwrite bool
	labels    string
	outFile   string
	debug     bool
	verbose   bool
	arg       string
}

func newFlagSet() (*flag.FlagSet, *cliConfig) {
	var cfg cliConfig
	fs := flag.NewFlagSet("issued", flag.ContinueOnError)
	fs.StringVar(&cfg.dir, "dir", ".", "workspace root; the analysis cannot read outside it")
	fs.StringVar(&cfg.repo, "repo", "", "target repository as owner/repo (default: the origin remote of --dir)")
	fs.BoolVar(&cfg.dryRun, "dry-run", false, "make no changes to GitHub; only print the rendered issue")
	fs.BoolVar(&cfg.overwrite, "overwrite", false, "overwrite the input GitHub issue body in place instead of creating a new issue")
	fs.StringVar(&cfg.labels, "label", "", "comma-separated labels for the created issue, e.g. af:fix,bug")
	fs.StringVar(&cfg.outFile, "out", "", "also write the rendered issue to this file")
	fs.BoolVar(&cfg.debug, "debug", false, "stream the model's reasoning text to stderr")
	fs.BoolVar(&cfg.verbose, "verbose", false, "verbose output: input details, tools, execution traces and cost summary")
	fs.Usage = func() {
		printUsage(fs.Output(), fs)
	}
	return fs, &cfg
}

func parseCLI(argv []string) (*cliConfig, error) {
	fs, cfg := newFlagSet()
	operands, err := parseArgs(fs, argv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", errUsage, err)
	}
	cfg.arg = strings.Join(operands, " ")

	// -overwrite rewrites the issue the report came from, so it needs one,
	// and the flags that only shape a NEW issue have nothing to apply to.
	// Refused here rather than silently ignored after a paid run.
	if cfg.overwrite {
		if _, ok := ParseIssueURL(strings.TrimSpace(cfg.arg)); !ok {
			return nil, fmt.Errorf("%w: -overwrite needs a GitHub issue URL as the input; it rewrites that issue in place", errUsage)
		}
		if cfg.labels != "" {
			return nil, fmt.Errorf("%w: -overwrite updates the issue body only; -label applies to a new issue", errUsage)
		}
		if cfg.repo != "" {
			return nil, fmt.Errorf("%w: -overwrite writes to the issue's own repository; -repo applies to a new issue", errUsage)
		}
	}
	return cfg, nil
}

func run() error {
	cfg, err := parseCLI(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}

	// 1. Classify the input before anything expensive happens. An empty
	//    argument is usage, not a run: the skill's "halt until input is
	//    received" is a program that exits 2 (main maps ErrNoInput to it).
	gh := NewGitHub()
	report, err := ResolveInput(cfg.arg, os.Stdin, gh)
	if errors.Is(err, ErrNoInput) {
		usage()
		return fmt.Errorf("nothing to triage: %w", ErrNoInput)
	} else if err != nil {
		return err
	}
	if cfg.verbose {
		fmt.Fprintf(os.Stderr, "[issued] input: %s (%s, %d bytes)\n",
			report.Kind, report.Origin, len(report.Body))
	}

	// 2. The workspace is the containment boundary for the whole analysis.
	//    Every path the read tools are handed is resolved against this root,
	//    symlinks included, and so is every path the model later cites in the
	//    issue.
	ws, err := tools.NewWorkspace(cfg.dir)
	if err != nil {
		return err
	}

	// 3. Resolve the target repository now, so a run that cannot be filed
	//    fails before it is paid for rather than after.
	owner, repo, err := targetRepo(cfg.repo, cfg.dir, report)
	if err != nil && !cfg.dryRun {
		return err
	}
	// The same goes for the token: the GitHub write is the last step, and
	// finding out it cannot happen after the model has been paid is the
	// worst time to find out.
	if !cfg.dryRun && gh.Token == "" {
		return errors.New("filing the issue needs a token: set GITHUB_TOKEN (or GH_TOKEN), or pass --dry-run to only print it")
	}

	model, err := catalog.ResolveModel(modelSpec())
	if err != nil {
		return err
	}
	if err := checkCredentials(model); err != nil {
		return err
	}

	agentCfg := core.AgentConfig{Model: model}
	agentkit.RegisterDefaults(&agentCfg,
		anthropic.Provider(anthropic.Options{}),
		openai.Provider(openai.Options{}),
		openairesponses.Provider(openairesponses.Options{}),
		google.Provider(google.Options{}),
		ollama.Provider(ollama.Options{}),
	)

	triager, err := NewTriager(agentCfg, ws, cfg.verbose, cfg.debug)
	if err != nil {
		return err
	}
	if cfg.verbose {
		fmt.Fprintf(os.Stderr, "[issued] workspace: %s\n[issued] tools: %s\n[issued] analysing…\n",
			ws.Root, strings.Join(triager.ToolNames(), ", "))
	}

	// 4. Run. The issue comes back as a validated struct, not as text to be
	//    parsed out of a transcript.
	issue, res, err := triager.Triage(context.Background(), report)
	if err != nil {
		return err
	}

	body := issue.Render(report.Kind, report.Origin)
	if report.Upstream != nil && !cfg.overwrite {
		body = fmt.Sprintf("Triaged from %s.\n\n%s", report.Upstream.URL(), body)
	}

	fmt.Printf("%s\n\n%s", issue.Title, body)
	if cfg.verbose {
		summarize(os.Stderr, triager, res, model.ID)
	}

	if cfg.outFile != "" {
		if err := os.WriteFile(cfg.outFile, []byte(issue.Title+"\n\n"+body), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[issued] wrote %s\n", cfg.outFile)
	}

	// 5. The side effect, gated on a flag rather than on the model's judgement.
	//    Reaching GitHub is not something the agent can do — there is no tool
	//    for it — so this is the only line in the program that writes anything
	//    to anyone, and it runs after the run has ended.
	return fileOrDryRun(os.Stderr, gh, cfg.dryRun, cfg.overwrite, report.Upstream, owner, repo, issue, body, splitLabels(cfg.labels))
}

type issueClient interface {
	CreateIssue(owner, repo, title, body string, labels []string) (string, error)
	UpdateIssue(owner, repo string, number int, title, body string) (string, error)
}

func fileOrDryRun(w io.Writer, gh issueClient, dryRun, overwrite bool, upstream *IssueRef, owner, repo string, issue Issue, body string, labels []string) error {
	if dryRun {
		if owner == "" {
			fmt.Fprintf(w, "[issued] dry run. Re-run with --repo %s to file it.\n",
				repoLabel(owner, repo))
		} else {
			fmt.Fprintln(w, "[issued] dry run. Re-run without --dry-run to file it.")
		}
		return nil
	}
	if overwrite && upstream != nil {
		url, err := gh.UpdateIssue(upstream.Owner, upstream.Repo, upstream.Number, issue.Title, body)
		if err != nil {
			return fmt.Errorf("%w\n\n(the issue body is above; you can file it by hand)", err)
		}
		fmt.Fprintf(w, "[issued] updated: %s\n", url)
		return nil
	}
	url, err := gh.CreateIssue(owner, repo, issue.Title, body, labels)
	if err != nil {
		return fmt.Errorf("%w\n\n(the issue body is above; you can file it by hand)", err)
	}
	fmt.Fprintf(w, "[issued] filed: %s\n", url)
	return nil
}

// targetRepo resolves --repo, then the issue the report came from, then the
// origin remote of the workspace. The middle one matters: `issued <issue-url>`
// re-triages a report that already lives somewhere, and the obvious place for
// the result is the repository it came from.
func targetRepo(flagVal, dir string, rep Report) (owner, repo string, err error) {
	if flagVal != "" {
		parts := strings.Split(strings.Trim(flagVal, "/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("--repo must be owner/repo, got %q", flagVal)
		}
		return parts[0], parts[1], nil
	}
	if rep.Upstream != nil {
		return rep.Upstream.Owner, rep.Upstream.Repo, nil
	}
	if o, r, ok := DetectRepo(dir); ok {
		return o, r, nil
	}
	return "", "", errors.New("no target repository: pass --repo owner/repo (no origin remote found)")
}

func repoLabel(owner, repo string) string {
	if owner == "" {
		return "owner/repo"
	}
	return owner + "/" + repo
}

func splitLabels(s string) []string {
	var out []string
	for _, l := range strings.Split(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// summarize prints what the run cost and what it took to get a clean issue.
// The rejection count is the interesting number: it is the citation check
// firing, and a run that needed three attempts to name a real file is a run
// whose diagnosis you should read more carefully.
func summarize(w io.Writer, t *Triager, res core.RunResult, modelID string) {
	if n, paths := t.Rejections(); n > 0 {
		fmt.Fprintf(w, "[issued] %d file_issue call(s) rejected for uncited paths: %s\n",
			n, strings.Join(paths, ", "))
	}
	u := res.Usage
	fmt.Fprintf(w, "[issued] %s · %d turns · stop %s · in %d / out %d tokens · $%.5f\n",
		modelID, res.TurnCount, res.StopReason, u.InputTokens, u.OutputTokens, u.CostUSD)
}

// parseArgs interleaves flags and operands: parse, take the next operand,
// parse again from what follows it. A lone "-" is an operand, because the flag
// package stops on any argument shorter than two characters — which is what
// makes `issued -` mean stdin rather than an unknown flag.
func parseArgs(fs *flag.FlagSet, argv []string) ([]string, error) {
	var operands []string
	if err := fs.Parse(argv); err != nil {
		return nil, err
	}
	for rest := fs.Args(); len(rest) > 0; rest = fs.Args() {
		operands = append(operands, rest[0])
		if err := fs.Parse(rest[1:]); err != nil {
			return nil, err
		}
	}
	return operands, nil
}

func usage() {
	fs, _ := newFlagSet()
	printUsage(os.Stderr, fs)
}

func printUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, `issued — triage a problem report into a structured GitHub issue.

Usage:
  issued [flags] <text | file.md | file.txt | github-issue-url | ->

Examples:
  issued "TestResume hangs on a session whose last entry is a tool call"
  issued ./crash.log --dir ./service
  issued https://github.com/owner/repo/issues/42 --label af:fix
  issued https://github.com/owner/repo/issues/42 -overwrite
  issued ./crash.log --dry-run
  kubectl logs deploy/api | issued -

Files an issue to GitHub by default; pass --dry-run to only print the diagnosis.
-overwrite needs an issue URL as the input and cannot be combined with -label or -repo.

Exit codes: 0 filed (or printed) · 1 failed · 2 usage

Flags:
`)
	fs.SetOutput(w)
	fs.PrintDefaults()
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
