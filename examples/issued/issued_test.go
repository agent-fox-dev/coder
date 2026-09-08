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
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
func newTriager(t *testing.T, p *faux.Provider, ws *tools.Workspace) *Triager {
	t.Helper()
	cfg := core.AgentConfig{
		Model:     faux.Model(),
		Providers: core.ProviderRegistry{faux.API: p.APIProvider()},
	}
	tr, err := NewTriager(cfg, ws, false)
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
		"not a remote":                                 "",
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
