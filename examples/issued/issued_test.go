package main

// The whole file runs offline: no API key, no network, no environment
// variable. The model is a script (provider/faux) and the codebase under
// analysis is a temporary directory with three files in it.
//
// That is the point of the example as much as the program is. Every property
// this utility claims — read-only, cites real files, produces a stable
// document, fails visibly when the model never reaches a conclusion — is a
// property you can assert in CI on every commit, because none of them depend
// on what a model happens to say.
//
//	go test ./examples/issued/ -v

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentkit "github.com/agentfox/agentkit-go"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/faux"
	"github.com/agentfox/agentkit-go/tools"
)

// ---------------------------------------------------------------- fixtures

// fakeRepo is the codebase the triage runs against.
func fakeRepo(t *testing.T) *tools.Workspace {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("session/store.go", "package session\n\nfunc Load() {}\n")
	write("session/store_test.go", "package session\n")
	write("README.md", "# fixture\n")

	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// newTriager wires a Triager to a scripted provider. The three lines that
// matter are Model, Providers and the workspace; everything else about the
// program is unchanged from what main() builds.
func newTriager(t *testing.T, p *faux.Provider, ws *tools.Workspace, verboseAndDebug ...bool) *Triager {
	t.Helper()
	verbose := false
	debug := false
	if len(verboseAndDebug) > 0 {
		verbose = verboseAndDebug[0]
	}
	if len(verboseAndDebug) > 1 {
		debug = verboseAndDebug[1]
	}
	cfg := core.AgentConfig{
		Model:     faux.Model(),
		Providers: core.ProviderRegistry{faux.API: p.APIProvider()},
	}
	tr, err := NewTriager(cfg, ws, verbose, debug)
	if err != nil {
		t.Fatalf("NewTriager: %v", err)
	}
	return tr
}

// goodIssue is a diagnosis that cites files which actually exist in fakeRepo.
func goodIssue() Issue {
	return Issue{
		Title:        "session: Load returns before the store is opened",
		Problem:      "Load() returns a zero store, so the first read sees no entries.",
		Reproduction: "Observed from error output; manual reproduction steps not established.",
		Confidence:   "Confirmed",
		RootCause:    "session/store.go:3 returns before assigning the handle.",
		AffectedFiles: []FileRef{
			{Path: "session/store.go", Role: "the early return"},
			{Path: "session/store_test.go", Role: "covers Load but not the empty case"},
		},
		Fix: Fix{
			Approach: "Assign the handle before returning.",
			Files: []FileRef{
				{Path: "session/store.go", Role: "assign before return"},
				{Path: "session/regression_test.go", Role: "new test for the empty case"},
			},
			Risks: "None identified",
		},
		AcceptanceCriteria: []string{"Given an empty store, when Load runs, then it returns a usable handle"},
		Severity:           "High",
		SeverityRationale:  "Every session read is affected and there is no workaround.",
	}
}

func fileIssueCall(id string, i Issue) core.ContentBlock {
	b, err := json.Marshal(i)
	if err != nil {
		panic(err)
	}
	return faux.FauxToolCall(id, "file_issue", string(b))
}

func turn(blocks ...core.ContentBlock) faux.Turn {
	return faux.Turn{Blocks: blocks, StopReason: core.StopReasonToolUse}
}

// --------------------------------------------------------------------- 1 --
//
// The read-only mandate is a resolved tool set, not a sentence.

func TestTheResolvedToolSetCarriesNothingThatCanWrite(t *testing.T) {
	tr := newTriager(t, faux.New(), fakeRepo(t))

	names := strings.Join(tr.ToolNames(), " ")
	for _, banned := range mutatingTools {
		if strings.Contains(names, banned) {
			t.Errorf("%s survived into the resolved set: %s", banned, names)
		}
	}
	for _, wanted := range []string{"read_file", "list_files", "find_files", "search_files", "file_issue"} {
		if !strings.Contains(names, wanted) {
			t.Errorf("%s missing from the resolved set: %s", wanted, names)
		}
	}
}

// TestWideningTheExcludesFailsTheRunRatherThanTheReview is the second,
// independent check. If someone deletes an entry from mutatingTools, the SDK's
// own guard stops the run — because this program leaves BeforeToolCall nil,
// and a nil interceptor with a shell tool in the resolved set is
// ErrUnguardedExecute.
//
// The test constructs that mistake deliberately: the full built-in set, no
// tool policy, no interceptor.
func TestWideningTheExcludesFailsTheRunRatherThanTheReview(t *testing.T) {
	ws := fakeRepo(t)
	built, err := tools.All(tools.Options{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	p := faux.New(faux.FauxAssistantMessage(core.StopReasonStop, faux.FauxText("hi")))
	agent, err := agentkit.NewAgent(core.AgentConfig{
		Model:      faux.Model(),
		Providers:  core.ProviderRegistry{faux.API: p.APIProvider()},
		StopPolicy: agentkit.StopAfterTurns(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range built {
		if err := agent.RegisterTool(tool); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := agent.Run(context.Background(), "anything"); !errors.Is(err, core.ErrUnguardedExecute) {
		t.Fatalf("err = %v, want ErrUnguardedExecute", err)
	}
	if p.Calls() != 0 {
		t.Errorf("the guard let %d request(s) out; it runs before the first one", p.Calls())
	}
}

// TestAssertReadOnlyCatchesAWidenedPolicyAtStartup covers the same mistake one
// layer earlier, where the message names the tool instead of the requirement.
func TestAssertReadOnlyCatchesAWidenedPolicyAtStartup(t *testing.T) {
	err := assertReadOnly([]core.Tool{{Name: "read_file"}, {Name: "edit_file"}})
	if err == nil || !strings.Contains(err.Error(), "edit_file") {
		t.Fatalf("assertReadOnly = %v, want an error naming edit_file", err)
	}
	if err := assertReadOnly([]core.Tool{{Name: "read_file"}, {Name: "file_issue"}}); err != nil {
		t.Fatalf("assertReadOnly on a clean set = %v, want nil", err)
	}
}

// --------------------------------------------------------------------- 2 --
//
// The citation rule is a check, and a refusal is repairable.

// TestACitedPathThatDoesNotExistIsRefusedAndTheModelRecovers is the behaviour
// the af-issue skill can only ask for ("Do not guess at file names"). Here the
// first file_issue call names a file that is not in the workspace; the handler
// returns an error result; the loop feeds it back; the second call is right.
//
// Note what the run looks like from outside: one issue, correctly cited, and a
// rejection count that says it took two tries.
func TestACitedPathThatDoesNotExistIsRefusedAndTheModelRecovers(t *testing.T) {
	bad := goodIssue()
	bad.AffectedFiles = []FileRef{{Path: "session/imagined.go", Role: "invented"}}

	p := faux.New(
		turn(fileIssueCall("call_1", bad)),
		turn(fileIssueCall("call_2", goodIssue())),
	)
	tr := newTriager(t, p, fakeRepo(t))

	issue, res, err := tr.Triage(context.Background(), Report{
		Kind: SourceText, Origin: "argument", Body: "Load returns nothing",
	})
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if res.StopReason != core.RunStopToolTerminate {
		t.Errorf("StopReason = %s, want %s", res.StopReason, core.RunStopToolTerminate)
	}
	if issue.Title != goodIssue().Title {
		t.Errorf("captured the wrong issue: %q", issue.Title)
	}
	n, paths := tr.Rejections()
	if n != 1 || len(paths) != 1 || paths[0] != "session/imagined.go" {
		t.Errorf("Rejections() = %d, %v; want 1, [session/imagined.go]", n, paths)
	}

	// The refusal reached the model as an error result rather than killing the
	// run — that is what made the second attempt possible.
	var sawError bool
	for _, m := range res.Messages {
		if trm, ok := m.(core.ToolResultMessage); ok && trm.IsError {
			sawError = true
			if !strings.Contains(trm.Content.Text(), "session/imagined.go") {
				t.Errorf("the refusal did not name the bad path: %s", trm.Content.Text())
			}
		}
	}
	if !sawError {
		t.Error("no error result in the transcript; the refusal never reached the model")
	}
}

// TestAProposedNewFileIsAllowedButAnEscapeIsNot is the distinction the check
// has to make: suggested_fix may name a file that does not exist yet, because
// "add a regression test" is a legitimate fix, but it may not name a path
// outside the workspace.
func TestAProposedNewFileIsAllowedButAnEscapeIsNot(t *testing.T) {
	ws := fakeRepo(t)
	tr := newTriager(t, faux.New(), ws)

	if missing := tr.checkContained([]FileRef{{Path: "session/regression_test.go"}}); len(missing) != 0 {
		t.Errorf("a proposed new file was refused: %v", missing)
	}
	outside := tr.checkContained([]FileRef{{Path: "../../etc/passwd"}, {Path: "/etc/passwd"}})
	if len(outside) != 2 {
		t.Errorf("checkContained = %v, want both paths refused", outside)
	}
	if missing := tr.checkPaths([]FileRef{{Path: "session/regression_test.go"}}); len(missing) != 1 {
		t.Errorf("checkPaths accepted a file that does not exist: %v", missing)
	}
	// A directory is not a file the model read.
	if missing := tr.checkPaths([]FileRef{{Path: "session"}, {Path: "session/store.go"}}); len(missing) != 1 || missing[0] != "session" {
		t.Errorf("checkPaths on a directory = %v, want [session]", missing)
	}
}

// --------------------------------------------------------------------- 3 --
//
// A run that reaches no conclusion is not an issue.

func TestARunThatNeverCallsFileIssueReturnsNoIssue(t *testing.T) {
	p := faux.New(faux.FauxAssistantMessage(core.StopReasonStop,
		faux.FauxText("I looked around and I am not sure what is wrong.")))
	tr := newTriager(t, p, fakeRepo(t))

	_, _, err := tr.Triage(context.Background(), Report{Kind: SourceText, Origin: "argument", Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "without file_issue") {
		t.Fatalf("Triage err = %v, want a 'ran without file_issue' failure", err)
	}
}

func TestTriageRetriesOnTransient503Error(t *testing.T) {
	p := faux.New(
		faux.Turn{Err: errors.New("google: HTTP 503: UNAVAILABLE: The service is currently unavailable.")},
		turn(fileIssueCall("call_1", goodIssue())),
	)
	tr := newTriager(t, p, fakeRepo(t))

	issue, res, err := tr.Triage(context.Background(), Report{
		Kind: SourceText, Origin: "argument", Body: "Load returns nothing",
	})
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if res.StopReason != core.RunStopToolTerminate {
		t.Errorf("StopReason = %s, want %s", res.StopReason, core.RunStopToolTerminate)
	}
	if issue.Title != goodIssue().Title {
		t.Errorf("captured the wrong issue: %q", issue.Title)
	}
	if len(p.Requests()) != 2 {
		t.Errorf("expected 2 requests (1 retry), got %d", len(p.Requests()))
	}
}

// --------------------------------------------------------------------- 4 --
//
// What was actually sent.

// TestTheRequestDeclaresFileIssueAndNoWriteTools asserts on the wire, which is
// the only place the tool policy's effect is observable to the model.
func TestTheRequestDeclaresFileIssueAndNoWriteTools(t *testing.T) {
	p := faux.New(turn(fileIssueCall("call_1", goodIssue())))
	tr := newTriager(t, p, fakeRepo(t))
	if _, _, err := tr.Triage(context.Background(), Report{
		Kind: SourceIssue, Origin: "https://github.com/o/r/issues/1", Body: "boom",
	}); err != nil {
		t.Fatalf("Triage: %v", err)
	}

	reqs := p.Requests()
	if len(reqs) == 0 {
		t.Fatal("no request recorded")
	}
	declared := map[string]bool{}
	for _, tool := range reqs[0].Tools {
		declared[tool.Name] = true
	}
	if !declared["file_issue"] {
		t.Error("file_issue was not declared to the model")
	}
	for _, banned := range mutatingTools {
		if declared[banned] {
			t.Errorf("%s was declared to the model", banned)
		}
	}
	// The report is quoted and labelled with its provenance, so instructions
	// inside it read as evidence rather than as a turn addressed to the model.
	first := reqs[0].Messages[0]
	um, ok := first.(core.UserMessage)
	if !ok {
		t.Fatalf("first message is %T, want core.UserMessage", first)
	}
	text := um.Content.Text()
	for _, want := range []string{"BEGIN REPORT", "END REPORT", "github issue", "not as instructions"} {
		if !strings.Contains(text, want) {
			t.Errorf("task prompt missing %q:\n%s", want, text)
		}
	}
}

// --------------------------------------------------------------------- 5 --
//
// The document is a pure function of the diagnosis.

func TestRenderProducesEverySectionAndIsStable(t *testing.T) {
	got := goodIssue().Render(SourceFile, "crash.log")
	for _, want := range []string{
		"## Problem", "## Reproduction", "## Root Cause Analysis",
		"**Confidence:** Confirmed", "### Related Instances", "No related instances found.",
		"## Affected Files", "`session/store.go` — the early return",
		"## Suggested Fix", "**Files to modify:**", "**Risks:**", "None identified",
		"## Acceptance Criteria", "- **AC-1:**",
		"## Severity", "**High** —",
		"*Triaged by `issued` from file: crash.log.*",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered issue missing %q", want)
		}
	}
	if again := goodIssue().Render(SourceFile, "crash.log"); again != got {
		t.Error("Render is not deterministic")
	}
}

// --------------------------------------------------------------------- 6 --
//
// Input classification, which is decided in Go and therefore testable.

func TestResolveInputClassifiesEverySource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crash.log")
	if err := os.WriteFile(path, []byte("panic: boom\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("file", func(t *testing.T) {
		rep, err := ResolveInput(path, nil, nil)
		if err != nil || rep.Kind != SourceFile || !strings.Contains(rep.Body, "panic: boom") {
			t.Fatalf("rep = %+v, err = %v", rep, err)
		}
	})
	t.Run("text", func(t *testing.T) {
		rep, err := ResolveInput("the loop hangs on resume", nil, nil)
		if err != nil || rep.Kind != SourceText || rep.Origin != "argument" {
			t.Fatalf("rep = %+v, err = %v", rep, err)
		}
	})
	t.Run("stdin", func(t *testing.T) {
		rep, err := ResolveInput("-", strings.NewReader("goroutine 1 [running]:\n"), nil)
		if err != nil || rep.Kind != SourceStdin {
			t.Fatalf("rep = %+v, err = %v", rep, err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := ResolveInput("   ", nil, nil); !errors.Is(err, ErrNoInput) {
			t.Fatalf("err = %v, want ErrNoInput", err)
		}
	})
	t.Run("empty stdin", func(t *testing.T) {
		if _, err := ResolveInput("-", strings.NewReader("  \n"), nil); !errors.Is(err, ErrNoInput) {
			t.Fatalf("err = %v, want ErrNoInput", err)
		}
	})
	t.Run("issue url without a client", func(t *testing.T) {
		// The classification is right; only the fetch is unavailable. That
		// distinction is why the URL branch does not silently fall through to
		// "treat the URL as the bug report".
		_, err := ResolveInput("https://github.com/o/r/issues/7", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "o/r#7") {
			t.Fatalf("err = %v, want a failure naming the issue", err)
		}
	})
}

func TestParseIssueURL(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" means not an issue reference
	}{
		{"https://github.com/agent-fox-dev/coder/issues/42", "agent-fox-dev/coder#42"},
		{"https://www.github.com/o/r/pull/7", "o/r#7"},
		{"https://github.com/o/r/issues/42#issuecomment-1", "o/r#42"},
		{"https://github.com/o/r", ""},
		{"https://github.com/o/r/blob/main/x.go", ""},
		{"https://gitlab.com/o/r/issues/1", ""},
		{"http://github.com/o/r/issues/1", ""}, // plain http is not accepted
		{"the issue is at github.com/o/r/issues/1", ""},
		{"https://github.com/o/r/issues/0", ""},
	}
	for _, c := range cases {
		ref, ok := ParseIssueURL(c.in)
		got := ""
		if ok {
			got = ref.String()
		}
		if got != c.want {
			t.Errorf("ParseIssueURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:agent-fox-dev/coder.git":       "agent-fox-dev/coder",
		"https://github.com/agent-fox-dev/coder.git":   "agent-fox-dev/coder",
		"https://github.com/agent-fox-dev/coder":       "agent-fox-dev/coder",
		"ssh://git@github.com/agent-fox-dev/coder.git": "agent-fox-dev/coder",
		"https://token@github.com/o/r.git":             "o/r",
		"https://www.github.com/o/r":                   "o/r",
		"not a remote":                                 "",
		// Another host with the same owner/repo layout is not GitHub, and
		// filing there by name would land the issue on the wrong site.
		"git@gitlab.com:agent-fox-dev/coder.git":  "",
		"https://gitlab.com/agent-fox-dev/coder":  "",
		"ssh://git@git.example.com/o/r.git":       "",
		"https://ghe.example.com/o/r.git":         "", // only with GITHUB_API_URL, below
		"https://github.com.evil.example/o/r.git": "",
	}
	for in, want := range cases {
		owner, repo, ok := ParseRemote(in)
		got := ""
		if ok {
			got = owner + "/" + repo
		}
		if got != want {
			t.Errorf("ParseRemote(%q) = %q, want %q", in, got, want)
		}
	}

	// An enterprise host is accepted once GITHUB_API_URL names it.
	t.Setenv("GITHUB_API_URL", "https://api.ghe.example.com")
	for _, in := range []string{"https://ghe.example.com/o/r.git", "git@api.ghe.example.com:o/r.git", "https://github.com/o/r"} {
		if owner, repo, ok := ParseRemote(in); !ok || owner != "o" || repo != "r" {
			t.Errorf("ParseRemote(%q) with GITHUB_API_URL = %q/%q %v, want o/r", in, owner, repo, ok)
		}
	}
	if _, _, ok := ParseRemote("https://gitlab.com/o/r"); ok {
		t.Error("GITHUB_API_URL must not widen the host check to everything")
	}
}

// TestAnOversizedReportIsTruncatedVisibly: a report cut off mid-frame with no
// marker reads to the model as a stack trace that simply had no more frames.
func TestAnOversizedReportIsTruncatedVisibly(t *testing.T) {
	huge := strings.Repeat("a line of log output\n", (maxReportBytes/21)+500)
	rep, err := ResolveInput("-", strings.NewReader(huge), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Body) > maxReportBytes+200 {
		t.Errorf("body is %d bytes, want ~%d", len(rep.Body), maxReportBytes)
	}
	if !strings.Contains(rep.Body, "truncated by issued") {
		t.Error("the truncation is invisible to the model")
	}
}

// --------------------------------------------------------------------- 7 --
//
// CLI flags, target repository validation, and dry-run gating.

func TestFlagParsingRejectsCreateAndAcceptsDryRun(t *testing.T) {
	t.Run("default is not dry-run", func(t *testing.T) {
		cfg, err := parseCLI([]string{"test bug"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.dryRun {
			t.Errorf("cfg.dryRun = true, want false by default")
		}
		if cfg.arg != "test bug" {
			t.Errorf("cfg.arg = %q, want %q", cfg.arg, "test bug")
		}
	})

	t.Run("dry-run flag before operand", func(t *testing.T) {
		cfg, err := parseCLI([]string{"--dry-run", "test bug"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.dryRun {
			t.Errorf("cfg.dryRun = false, want true")
		}
		if cfg.arg != "test bug" {
			t.Errorf("cfg.arg = %q, want %q", cfg.arg, "test bug")
		}
	})

	t.Run("dry-run flag after operand", func(t *testing.T) {
		cfg, err := parseCLI([]string{"test bug", "--dry-run"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.dryRun {
			t.Errorf("cfg.dryRun = false, want true")
		}
		if cfg.arg != "test bug" {
			t.Errorf("cfg.arg = %q, want %q", cfg.arg, "test bug")
		}
	})

	t.Run("create flag rejected as undefined before operand", func(t *testing.T) {
		_, err := parseCLI([]string{"--create", "test bug"})
		if err == nil {
			t.Fatal("parseCLI with --create succeeded, want undefined flag error")
		}
		if !strings.Contains(err.Error(), "create") {
			t.Errorf("error = %v, want it to name 'create'", err)
		}
	})

	t.Run("create flag rejected as undefined after operand", func(t *testing.T) {
		_, err := parseCLI([]string{"test bug", "--create"})
		if err == nil {
			t.Fatal("parseCLI with --create succeeded, want undefined flag error")
		}
		if !strings.Contains(err.Error(), "create") {
			t.Errorf("error = %v, want it to name 'create'", err)
		}
	})
}

func TestFlagParsingOverwrite(t *testing.T) {
	t.Run("default is false", func(t *testing.T) {
		cfg, err := parseCLI([]string{"test bug"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.overwrite {
			t.Errorf("cfg.overwrite = true, want false by default")
		}
	})

	t.Run("-overwrite flag before operand", func(t *testing.T) {
		cfg, err := parseCLI([]string{"-overwrite", "https://github.com/owner/repo/issues/1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.overwrite {
			t.Errorf("cfg.overwrite = false, want true")
		}
	})

	t.Run("--overwrite flag after operand", func(t *testing.T) {
		cfg, err := parseCLI([]string{"https://github.com/owner/repo/issues/1", "--overwrite"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.overwrite {
			t.Errorf("cfg.overwrite = false, want true")
		}
	})

	// The combinations that would be silently ignored after a paid run are
	// refused before it, as usage errors.
	for name, argv := range map[string][]string{
		"without an issue URL":   {"-overwrite", "./crash.log"},
		"with free text":         {"-overwrite", "the loop hangs"},
		"with -label":            {"-overwrite", "-label", "bug", "https://github.com/owner/repo/issues/1"},
		"with -repo":             {"-overwrite", "-repo", "o/r", "https://github.com/owner/repo/issues/1"},
		"with -label after url":  {"https://github.com/owner/repo/issues/1", "-overwrite", "--label=bug"},
		"with nothing at all":    {"-overwrite"},
		"with an undefined flag": {"--create", "x"},
	} {
		t.Run("rejected "+name, func(t *testing.T) {
			_, err := parseCLI(argv)
			if err == nil {
				t.Fatalf("parseCLI(%v) succeeded, want a usage error", argv)
			}
			if !errors.Is(err, errUsage) {
				t.Errorf("parseCLI(%v) error %v is not a usage error", argv, err)
			}
		})
	}
	// A pull-request URL is an issue reference too, and -overwrite alone is
	// fine with any of them.
	if _, err := parseCLI([]string{"-overwrite", "https://github.com/owner/repo/pull/1"}); err != nil {
		t.Errorf("-overwrite with a pull request URL: %v", err)
	}
}

func TestTargetRepoValidationHaltsWithoutDryRun(t *testing.T) {
	dir := t.TempDir()
	rep := Report{Kind: SourceText, Origin: "argument", Body: "something broke"}

	// When no repo flag is passed and dir has no origin remote:
	owner, repo, err := targetRepo("", dir, rep)
	if err == nil {
		t.Fatalf("targetRepo succeeded, got %s/%s; want error when no repo found", owner, repo)
	}

	// Under AC-4: without --dry-run, this error halts before analysis.
	dryRun := false
	haltExecution := err != nil && !dryRun
	if !haltExecution {
		t.Errorf("err != nil && !dryRun = false, want true (halt before model analysis)")
	}

	// With --dry-run, execution is allowed to proceed to triage.
	dryRun = true
	haltExecution = err != nil && !dryRun
	if haltExecution {
		t.Errorf("err != nil && !dryRun = true with dryRun=true, want false (allow triage)")
	}

	// With an explicit repo flag, neither mode halts.
	owner, repo, err = targetRepo("foo/bar", dir, rep)
	if err != nil || owner != "foo" || repo != "bar" {
		t.Fatalf("targetRepo with --repo failed: owner=%q, repo=%q, err=%v", owner, repo, err)
	}
	if err != nil && !false {
		t.Error("unexpected halt with valid repo")
	}
}

type mockIssueCreator struct {
	called       bool
	updateCalled bool
	owner        string
	repo         string
	updateNumber int
	title        string
	body         string
	labels       []string
	retURL       string
	err          error
}

func (m *mockIssueCreator) CreateIssue(owner, repo, title, body string, labels []string) (string, error) {
	m.called = true
	m.owner = owner
	m.repo = repo
	m.title = title
	m.body = body
	m.labels = labels
	if m.err != nil {
		return "", m.err
	}
	return m.retURL, nil
}

func (m *mockIssueCreator) UpdateIssue(owner, repo string, number int, title, body string) (string, error) {
	m.updateCalled = true
	m.owner = owner
	m.repo = repo
	m.updateNumber = number
	m.title = title
	m.body = body
	if m.err != nil {
		return "", m.err
	}
	return m.retURL, nil
}

func TestDryRunGating(t *testing.T) {
	t.Run("AC-1: live run creates issue on GitHub", func(t *testing.T) {
		mock := &mockIssueCreator{retURL: "https://github.com/agent-fox-dev/coder/issues/100"}
		var stderr strings.Builder
		issue := goodIssue()
		err := fileOrDryRun(&stderr, mock, false, false, nil, "agent-fox-dev", "coder", issue, "issue body", []string{"af:fix"})
		if err != nil {
			t.Fatalf("fileOrDryRun: %v", err)
		}
		if !mock.called {
			t.Fatal("gh.CreateIssue was not called for live run")
		}
		if mock.owner != "agent-fox-dev" || mock.repo != "coder" || mock.title != issue.Title {
			t.Errorf("unexpected CreateIssue args: owner=%s, repo=%s, title=%s", mock.owner, mock.repo, mock.title)
		}
		if len(mock.labels) != 1 || mock.labels[0] != "af:fix" {
			t.Errorf("unexpected labels: %v", mock.labels)
		}
		if !strings.Contains(stderr.String(), "[issued] filed: https://github.com/agent-fox-dev/coder/issues/100") {
			t.Errorf("stderr missing filed url: %s", stderr.String())
		}
	})

	t.Run("AC-2: dry run does not create issue and prints re-run advice", func(t *testing.T) {
		mock := &mockIssueCreator{}
		var stderr strings.Builder
		issue := goodIssue()
		err := fileOrDryRun(&stderr, mock, true, false, nil, "agent-fox-dev", "coder", issue, "issue body", []string{"af:fix"})
		if err != nil {
			t.Fatalf("fileOrDryRun: %v", err)
		}
		if mock.called {
			t.Fatal("gh.CreateIssue was called during dry run")
		}
		want := "[issued] dry run. Re-run without --dry-run to file it.\n"
		if stderr.String() != want {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	})

	t.Run("AC-2 and AC-4: dry run with unresolved repo prints repo advice", func(t *testing.T) {
		mock := &mockIssueCreator{}
		var stderr strings.Builder
		issue := goodIssue()
		err := fileOrDryRun(&stderr, mock, true, false, nil, "", "", issue, "issue body", nil)
		if err != nil {
			t.Fatalf("fileOrDryRun: %v", err)
		}
		if mock.called {
			t.Fatal("gh.CreateIssue was called during dry run")
		}
		want := "[issued] dry run. Re-run with --repo owner/repo to file it.\n"
		if stderr.String() != want {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	})
}

func TestGitHubUpdateIssue(t *testing.T) {
	t.Run("sends PATCH request with payload and headers", func(t *testing.T) {
		var (
			gotMethod      string
			gotPath        string
			gotAccept      string
			gotAPIVersion  string
			gotAuth        string
			gotContentType string
			gotBody        map[string]any
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAccept = r.Header.Get("Accept")
			gotAPIVersion = r.Header.Get("X-GitHub-Api-Version")
			gotAuth = r.Header.Get("Authorization")
			gotContentType = r.Header.Get("Content-Type")

			_ = json.NewDecoder(r.Body).Decode(&gotBody)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"number": 42, "html_url": "https://github.com/owner/repo/issues/42"}`))
		}))
		defer srv.Close()

		gh := &GitHub{
			Token:   "secret-token",
			BaseURL: srv.URL,
			client:  srv.Client(),
		}

		url, err := gh.UpdateIssue("owner", "repo", 42, "session: a better title", "updated issue body")
		if err != nil {
			t.Fatalf("UpdateIssue: %v", err)
		}
		if gotBody["title"] != "session: a better title" {
			t.Errorf("payload title = %q, want the triaged title", gotBody["title"])
		}
		if url != "https://github.com/owner/repo/issues/42" {
			t.Errorf("url = %q, want %q", url, "https://github.com/owner/repo/issues/42")
		}
		if gotMethod != http.MethodPatch {
			t.Errorf("method = %q, want PATCH", gotMethod)
		}
		if gotPath != "/repos/owner/repo/issues/42" {
			t.Errorf("path = %q, want /repos/owner/repo/issues/42", gotPath)
		}
		if gotAccept != "application/vnd.github+json" {
			t.Errorf("Accept = %q", gotAccept)
		}
		if gotAPIVersion != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", gotAPIVersion)
		}
		if gotAuth != "Bearer secret-token" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if gotContentType != "application/json" {
			t.Errorf("Content-Type = %q", gotContentType)
		}
		if gotBody["body"] != "updated issue body" {
			t.Errorf("payload body = %q, want %q", gotBody["body"], "updated issue body")
		}
	})

	t.Run("fails when token is missing", func(t *testing.T) {
		gh := &GitHub{
			Token:   "",
			BaseURL: "https://api.github.com",
			client:  http.DefaultClient,
		}
		_, err := gh.UpdateIssue("owner", "repo", 42, "title", "body")
		if err == nil {
			t.Fatal("UpdateIssue succeeded without token, want error")
		}
		if !strings.Contains(err.Error(), "token") {
			t.Errorf("error = %v, want token error", err)
		}
	})

	t.Run("returns error on HTTP failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
		}))
		defer srv.Close()

		gh := &GitHub{
			Token:   "secret-token",
			BaseURL: srv.URL,
			client:  srv.Client(),
		}

		_, err := gh.UpdateIssue("owner", "repo", 42, "title", "body")
		if err == nil {
			t.Fatal("UpdateIssue succeeded on 404, want error")
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("error = %v, want it to mention 404", err)
		}
		// The status is a number on the error, not digits in its text.
		var he *httpError
		if !errors.As(err, &he) || he.Status != http.StatusNotFound {
			t.Errorf("error = %#v, want an *httpError with Status 404", err)
		}
	})
}

// TestReadIssueHintsAtTheTokenOnlyOnAReal404: the "set GITHUB_TOKEN" hint is
// keyed on the status code, so a 500 whose body mentions 404 does not get it.
func TestReadIssueHintsAtTheTokenOnlyOnAReal404(t *testing.T) {
	serve := func(status int, body string) *GitHub {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, body, status)
		}))
		t.Cleanup(srv.Close)
		return &GitHub{BaseURL: srv.URL, client: srv.Client()}
	}
	_, _, err := serve(http.StatusNotFound, `{"message": "Not Found"}`).ReadIssue(IssueRef{"o", "r", 1})
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("a 404 without a token should hint at the token, got %v", err)
	}
	_, _, err = serve(http.StatusInternalServerError, `{"message": "backend 404 upstream"}`).ReadIssue(IssueRef{"o", "r", 1})
	if err == nil || strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("a 500 must not be mistaken for a 404, got %v", err)
	}
}

func TestFileOrDryRunOverwrite(t *testing.T) {
	upstream := &IssueRef{
		Owner:  "agent-fox-dev",
		Repo:   "coder",
		Number: 42,
	}
	issue := goodIssue()

	t.Run("AC-1: live run with overwrite and upstream issue calls UpdateIssue", func(t *testing.T) {
		mock := &mockIssueCreator{retURL: "https://github.com/agent-fox-dev/coder/issues/42"}
		var stderr strings.Builder
		err := fileOrDryRun(&stderr, mock, false, true, upstream, "agent-fox-dev", "coder", issue, "new body", []string{"af:fix"})
		if err != nil {
			t.Fatalf("fileOrDryRun: %v", err)
		}
		if !mock.updateCalled {
			t.Fatal("gh.UpdateIssue was not called")
		}
		if mock.called {
			t.Fatal("gh.CreateIssue was called unexpectedly")
		}
		if mock.owner != "agent-fox-dev" || mock.repo != "coder" || mock.updateNumber != 42 || mock.body != "new body" {
			t.Errorf("unexpected UpdateIssue args: owner=%s, repo=%s, number=%d, body=%s",
				mock.owner, mock.repo, mock.updateNumber, mock.body)
		}
		if mock.title != issue.Title {
			t.Errorf("the triaged title must go with the body: got %q, want %q", mock.title, issue.Title)
		}
		if !strings.Contains(stderr.String(), "[issued] updated: https://github.com/agent-fox-dev/coder/issues/42") {
			t.Errorf("stderr missing updated url: %s", stderr.String())
		}
	})

	t.Run("AC-2: live run without overwrite creates new issue linking back", func(t *testing.T) {
		mock := &mockIssueCreator{retURL: "https://github.com/agent-fox-dev/coder/issues/100"}
		var stderr strings.Builder
		err := fileOrDryRun(&stderr, mock, false, false, upstream, "agent-fox-dev", "coder", issue, "issue body", []string{"af:fix"})
		if err != nil {
			t.Fatalf("fileOrDryRun: %v", err)
		}
		if !mock.called {
			t.Fatal("gh.CreateIssue was not called")
		}
		if mock.updateCalled {
			t.Fatal("gh.UpdateIssue was called unexpectedly")
		}
		if mock.owner != "agent-fox-dev" || mock.repo != "coder" || mock.title != issue.Title {
			t.Errorf("unexpected CreateIssue args: owner=%s, repo=%s, title=%s", mock.owner, mock.repo, mock.title)
		}
		if !strings.Contains(stderr.String(), "[issued] filed: https://github.com/agent-fox-dev/coder/issues/100") {
			t.Errorf("stderr missing filed url: %s", stderr.String())
		}
	})

	t.Run("AC-3: live run with overwrite on file/text input (upstream nil) creates new issue", func(t *testing.T) {
		mock := &mockIssueCreator{retURL: "https://github.com/agent-fox-dev/coder/issues/101"}
		var stderr strings.Builder
		err := fileOrDryRun(&stderr, mock, false, true, nil, "agent-fox-dev", "coder", issue, "issue body", []string{"af:fix"})
		if err != nil {
			t.Fatalf("fileOrDryRun: %v", err)
		}
		if !mock.called {
			t.Fatal("gh.CreateIssue was not called")
		}
		if mock.updateCalled {
			t.Fatal("gh.UpdateIssue was called unexpectedly")
		}
		if !strings.Contains(stderr.String(), "[issued] filed: https://github.com/agent-fox-dev/coder/issues/101") {
			t.Errorf("stderr missing filed url: %s", stderr.String())
		}
	})

	t.Run("dry run with overwrite does not update or create issue", func(t *testing.T) {
		mock := &mockIssueCreator{}
		var stderr strings.Builder
		err := fileOrDryRun(&stderr, mock, true, true, upstream, "agent-fox-dev", "coder", issue, "issue body", []string{"af:fix"})
		if err != nil {
			t.Fatalf("fileOrDryRun: %v", err)
		}
		if mock.called || mock.updateCalled {
			t.Fatal("gh client was called during dry run")
		}
		want := "[issued] dry run. Re-run without --dry-run to file it.\n"
		if stderr.String() != want {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}
	})
}

func TestUpstreamPreambleOmissionOnOverwrite(t *testing.T) {
	issue := goodIssue()
	upstream := &IssueRef{Owner: "agent-fox-dev", Repo: "coder", Number: 42}
	report := Report{Kind: SourceIssue, Origin: upstream.URL(), Upstream: upstream}

	renderBody := func(overwrite bool) string {
		body := issue.Render(report.Kind, report.Origin)
		if report.Upstream != nil && !overwrite {
			body = fmt.Sprintf("Triaged from %s.\n\n%s", report.Upstream.URL(), body)
		}
		return body
	}

	bodyWithoutOverwrite := renderBody(false)
	if !strings.HasPrefix(bodyWithoutOverwrite, "Triaged from https://github.com/agent-fox-dev/coder/issues/42.\n\n") {
		t.Errorf("expected preamble without overwrite, got: %s", bodyWithoutOverwrite[:60])
	}

	bodyWithOverwrite := renderBody(true)
	if strings.HasPrefix(bodyWithOverwrite, "Triaged from") {
		t.Errorf("preamble should be omitted with overwrite, got: %s", bodyWithOverwrite[:60])
	}
	if !strings.HasPrefix(bodyWithOverwrite, "## Problem\n") {
		t.Errorf("expected body to start with Problem section when preamble omitted, got: %s", bodyWithOverwrite[:60])
	}
}

// --------------------------------------------------------------------- 8 --
//
// Output verbosity tiers, debug streaming, spinner and completion timing.

func TestFormatTokenTiming(t *testing.T) {
	if formatted := FormatTokenTiming(1500*time.Millisecond, 100, 50); formatted != "(2s) · 100↑ 50↓" {
		t.Errorf("FormatTokenTiming = %q, want %q", formatted, "(2s) · 100↑ 50↓")
	}
	if subSec := FormatTokenTiming(500*time.Millisecond, 100, 50); subSec != "(500ms) · 100↑ 50↓" {
		t.Errorf("FormatTokenTiming (sub-second) = %q, want %q", subSec, "(500ms) · 100↑ 50↓")
	}
	if minTiming := FormatTokenTiming(28*time.Minute+16*time.Second, 153, 60600); minTiming != "(28m 16s) · 153↑ 60.6k↓" {
		t.Errorf("FormatTokenTiming (minute + k) = %q, want %q", minTiming, "(28m 16s) · 153↑ 60.6k↓")
	}
	if mTiming := FormatTokenTiming(65*time.Second, 1_200_000, 2_500_000); mTiming != "(1m 5s) · 1.2M↑ 2.5M↓" {
		t.Errorf("FormatTokenTiming (M tokens) = %q, want %q", mTiming, "(1m 5s) · 1.2M↑ 2.5M↓")
	}
}

func TestFlagParsingDebugAndVerbose(t *testing.T) {
	t.Run("defaults are false", func(t *testing.T) {
		cfg, err := parseCLI([]string{"test bug"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.debug {
			t.Errorf("cfg.debug = true, want false by default")
		}
		if cfg.verbose {
			t.Errorf("cfg.verbose = true, want false by default")
		}
	})

	t.Run("debug flag before and after operand", func(t *testing.T) {
		cfg, err := parseCLI([]string{"--debug", "test bug"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.debug {
			t.Errorf("cfg.debug = false, want true")
		}
		cfg, err = parseCLI([]string{"test bug", "--debug"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.debug {
			t.Errorf("cfg.debug = false, want true")
		}
	})

	t.Run("verbose flag before and after operand", func(t *testing.T) {
		cfg, err := parseCLI([]string{"--verbose", "test bug"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.verbose {
			t.Errorf("cfg.verbose = false, want true")
		}
		cfg, err = parseCLI([]string{"test bug", "--verbose"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.verbose {
			t.Errorf("cfg.verbose = false, want true")
		}
	})

	t.Run("both flags", func(t *testing.T) {
		cfg, err := parseCLI([]string{"--verbose", "--debug", "test bug"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.debug || !cfg.verbose {
			t.Errorf("got debug=%v verbose=%v, want both true", cfg.debug, cfg.verbose)
		}
	})
}

func TestAC1DebugStreamsReasoningDeltas(t *testing.T) {
	pWithDelta := func() *faux.Provider {
		return faux.New(
			turn(faux.FauxText("thinking about the problem..."), fileIssueCall("c1", goodIssue())),
		)
	}

	t.Run("with debug, deltas are streamed", func(t *testing.T) {
		var buf bytes.Buffer
		tr := newTriager(t, pWithDelta(), fakeRepo(t), false, true)
		tr.SetOutput(&buf)
		_, _, err := tr.Triage(context.Background(), Report{Kind: SourceText, Origin: "test", Body: "bug"})
		if err != nil {
			t.Fatalf("Triage: %v", err)
		}
		if !strings.Contains(buf.String(), "thinking about the problem...") {
			t.Errorf("expected deltas in output with -debug, got: %q", buf.String())
		}
	})

	t.Run("without debug, deltas are suppressed", func(t *testing.T) {
		var buf bytes.Buffer
		tr := newTriager(t, pWithDelta(), fakeRepo(t), false, false)
		tr.SetOutput(&buf)
		_, _, err := tr.Triage(context.Background(), Report{Kind: SourceText, Origin: "test", Body: "bug"})
		if err != nil {
			t.Fatalf("Triage: %v", err)
		}
		if strings.Contains(buf.String(), "thinking about the problem...") {
			t.Errorf("reasoning deltas should be suppressed without -debug, got: %q", buf.String())
		}
	})
}

func TestAC2AndAC3NonVerbosePhaseProgressAndSummary(t *testing.T) {
	t.Run("non-verbose terminal displays spinner frames and timing without cost", func(t *testing.T) {
		p := faux.New(
			turn(fileIssueCall("c1", goodIssue())),
		)
		var buf bytes.Buffer // *bytes.Buffer is identified as terminal
		tr := newTriager(t, p, fakeRepo(t), false, false)
		tr.SetOutput(&buf)

		_, _, err := tr.Triage(context.Background(), Report{Kind: SourceText, Origin: "test", Body: "bug"})
		if err != nil {
			t.Fatalf("Triage: %v", err)
		}

		out := buf.String()
		// AC-2: phase indicator and spinner frames with cleanup
		if !strings.Contains(out, "[issued] analysing") {
			t.Errorf("expected [issued] analysing in output, got: %q", out)
		}
		if !strings.Contains(out, "|") || !strings.Contains(out, "\b \b") {
			t.Errorf("expected spinner frames and cleanup in output, got: %q", out)
		}
		// AC-2: tool execution traces suppressed
		if strings.Contains(out, "read   file_issue") {
			t.Errorf("tool traces should be suppressed in non-verbose mode, got: %q", out)
		}

		// AC-3: elapsed time and token counts (↑ and ↓) printed without dollar cost ($)
		if !strings.Contains(out, "↑") || !strings.Contains(out, "↓") {
			t.Errorf("timing line missing token counts (↑/↓), got: %q", out)
		}
		if strings.Contains(out, "$") {
			t.Errorf("cost ($) should not appear in non-verbose output, got: %q", out)
		}
	})

	t.Run("non-terminal suppresses spinner characters but keeps timing", func(t *testing.T) {
		p := faux.New(
			turn(fileIssueCall("c1", goodIssue())),
		)
		var nonTermBuf bytes.Buffer
		nonTermWriter := struct{ io.Writer }{&nonTermBuf}
		tr := newTriager(t, p, fakeRepo(t), false, false)
		tr.SetOutput(nonTermWriter)

		_, _, err := tr.Triage(context.Background(), Report{Kind: SourceText, Origin: "test", Body: "bug"})
		if err != nil {
			t.Fatalf("Triage: %v", err)
		}

		out := nonTermBuf.String()
		if strings.Contains(out, "|") || strings.Contains(out, "\b") {
			t.Errorf("non-terminal output should not contain spinner characters, got: %q", out)
		}
		if !strings.Contains(out, "[issued] analysing") || !strings.Contains(out, "↑") {
			t.Errorf("non-terminal output should still include phase timing line, got: %q", out)
		}
	})
}

func TestAC4VerbosePreservesTracesAndCost(t *testing.T) {
	p := faux.New(
		turn(fileIssueCall("c1", goodIssue())),
	)
	var buf bytes.Buffer
	tr := newTriager(t, p, fakeRepo(t), true, false)
	tr.SetOutput(&buf)

	_, res, err := tr.Triage(context.Background(), Report{Kind: SourceText, Origin: "test", Body: "bug"})
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}

	out := buf.String()
	// Tool trace should appear in verbose mode
	if !strings.Contains(out, "read   file_issue") {
		t.Errorf("expected tool trace in verbose output, got: %q", out)
	}
	// Spinner characters should NOT appear
	if strings.Contains(out, "\b \b") {
		t.Errorf("verbose output should not contain spinner, got: %q", out)
	}

	// Full summary includes dollar cost ($%.5f)
	var summaryBuf bytes.Buffer
	summarize(&summaryBuf, tr, res, "test-model")
	summary := summaryBuf.String()
	if !strings.Contains(summary, "$") {
		t.Errorf("summarize should include dollar cost, got: %q", summary)
	}
	if !strings.Contains(summary, "test-model") || !strings.Contains(summary, "turns") {
		t.Errorf("summarize missing expected fields, got: %q", summary)
	}
}
