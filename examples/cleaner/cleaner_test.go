package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/faux"
	"github.com/agentfox/agentkit-go/tools"
)

// This file is the answer to "how do I know it works?" for a program whose
// happy path costs money and mutates a GitHub repository. Nothing here needs a
// key, a network or the gh CLI: the deterministic half is tested directly, and
// the agent half runs against provider/faux, so the LAST test drives the real
// pipeline — the real agent loop, the real tools, a real git repository — with
// a scripted model.

// ------------------------------------------------------------------ urls --

func TestParseIssueURL(t *testing.T) {
	valid := map[string]IssueRef{
		"https://github.com/acme/widgets/issues/42":                   {"acme", "widgets", 42},
		"https://github.com/acme/widgets/issues/42/":                  {"acme", "widgets", 42},
		"https://github.com/acme/widgets/issues/42#issuecomment-9911": {"acme", "widgets", 42},
		"  https://github.com/agent-fox-dev/coder/issues/7  ":         {"agent-fox-dev", "coder", 7},
		"https://github.com/a.b/c_d.e/issues/1":                       {"a.b", "c_d.e", 1},
	}
	for in, want := range valid {
		got, err := ParseIssueURL(in)
		if err != nil {
			t.Errorf("ParseIssueURL(%q) failed: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseIssueURL(%q) = %+v, want %+v", in, got, want)
		}
	}

	invalid := []string{
		"",
		"acme/widgets#42",
		"http://github.com/acme/widgets/issues/42",        // not https
		"https://github.com/acme/widgets/pull/42",         // a PR is a different object
		"https://github.com/acme/widgets/issues/",         // no number
		"https://github.com/acme/widgets/issues/abc",      // not a number
		"https://github.com/acme/widgets/issues/0",        // there is no issue 0
		"https://gitlab.com/acme/widgets/issues/42",       // not GitHub
		"https://github.com/acme/widgets/issues/42/extra", // trailing path
		"https://github.com/acme/sub/widgets/issues/42",   // extra path segment
	}
	for _, in := range invalid {
		if got, err := ParseIssueURL(in); err == nil {
			t.Errorf("ParseIssueURL(%q) = %+v, want an error", in, got)
		} else if !errors.Is(err, ErrBadIssueURL) {
			t.Errorf("ParseIssueURL(%q) error %v does not wrap ErrBadIssueURL", in, err)
		}
	}
}

func TestLinkedPRNumbersOnlyThisRepoAndDeduplicated(t *testing.T) {
	ref := IssueRef{"acme", "widgets", 42}
	issue := &Issue{
		Body: "regressed in https://github.com/acme/widgets/pull/10 and " +
			"https://github.com/other/repo/pull/99",
		Comments: []Comment{
			{Body: "see https://github.com/acme/widgets/pull/10 again"},
			{Body: "and https://github.com/acme/widgets/pull/12"},
		},
	}
	got := issue.LinkedPRNumbers(ref)
	if len(got) != 2 || got[0] != 10 || got[1] != 12 {
		t.Fatalf("LinkedPRNumbers = %v, want [10 12]", got)
	}
}

// --------------------------------------------------------------- branches --

func TestBranchSlug(t *testing.T) {
	cases := map[string]string{
		"Fix the bug in session token refresh": "session-token-refresh",
		"Add a feature for the CLI":            "cli",
		"Fix the bug":                          "issue", // all stop words
		"CRLF handling in edit_file is wrong":  "crlf-handling-edit-file-wrong",
		"A very long title about the resolution of relative workspace paths": "very-long-title-about-resolution",
	}
	for title, want := range cases {
		if got := BranchSlug(title); got != want {
			t.Errorf("BranchSlug(%q) = %q, want %q", title, got, want)
		}
	}
	if got := BranchSlug("!!! ???"); got == "" || strings.Contains(got, "!") {
		t.Errorf("BranchSlug of punctuation = %q, want a usable slug", got)
	}
	if got := BranchName(ClassBug, 42, "Fix the bug in session token refresh"); got != "fix/issue-42-session-token-refresh" {
		t.Errorf("BranchName = %q", got)
	}
	if got := BranchName(ClassFeature, 7, "Support YAML config"); got != "feature/issue-7-support-yaml-config" {
		t.Errorf("BranchName = %q", got)
	}
}

// ---------------------------------------------------------------- commits --

func TestCommitMessage(t *testing.T) {
	ref := IssueRef{"acme", "widgets", 42}

	// A well-formed subject from the model is kept verbatim.
	msg := CommitMessage(ClassBug, "fix(session): expire cached tokens", "ignored", ref, "body text")
	if !strings.HasPrefix(msg, "fix(session): expire cached tokens\n") {
		t.Errorf("well-formed subject was not kept:\n%s", msg)
	}
	if !strings.Contains(msg, "Fixes https://github.com/acme/widgets/issues/42") {
		t.Errorf("closing keyword missing:\n%s", msg)
	}
	if !strings.Contains(msg, "body text") {
		t.Errorf("body missing:\n%s", msg)
	}

	// A malformed one is replaced from the classification and the analysis
	// summary rather than committed as-is.
	msg = CommitMessage(ClassFeature, "Fixed the thing!", "Support YAML config files", ref, "")
	if got, want := strings.SplitN(msg, "\n", 2)[0], "feat: support YAML config files"; got != want {
		t.Errorf("subject = %q, want %q", got, want)
	}

	// An acronym must not be lowercased into nonsense.
	msg = CommitMessage(ClassBug, "", "HTTP client leaks connections", ref, "")
	if got := strings.SplitN(msg, "\n", 2)[0]; got != "fix: HTTP client leaks connections" {
		t.Errorf("subject = %q", got)
	}

	// The subject is bounded, whatever the model sent.
	long := "fix(scope): " + strings.Repeat("x", 200)
	if got := strings.SplitN(CommitMessage(ClassBug, long, "s", ref, ""), "\n", 2)[0]; len(got) > 72 {
		t.Errorf("subject is %d chars, want <= 72", len(got))
	}
}

// ---------------------------------------------------------- honest reports --

func TestVerificationLinesCannotOverclaim(t *testing.T) {
	changed := []string{"a.go"}

	got := verificationLines(VerifyResult{Skipped: true}, VerifyResult{Skipped: true}, changed)
	if !strings.Contains(got, "unverified") {
		t.Errorf("a run with no verification command must say so, got:\n%s", got)
	}
	if strings.Contains(got, "✅") {
		t.Errorf("an unverified run must not print a green tick:\n%s", got)
	}

	got = verificationLines(
		VerifyResult{Command: "make check", OK: true},
		VerifyResult{Command: "make check", ExitCode: 2},
		changed)
	if !strings.Contains(got, "❌") || !strings.Contains(got, "exit 2") {
		t.Errorf("a failing verification must be reported as failing:\n%s", got)
	}

	got = verificationLines(
		VerifyResult{Command: "make check", ExitCode: 1},
		VerifyResult{Command: "make check", OK: true},
		changed)
	if !strings.Contains(got, "already failing") {
		t.Errorf("a red baseline must be disclosed:\n%s", got)
	}
}

func TestFileTableEscapesPipes(t *testing.T) {
	got := fileTable([]FileChange{{Path: "a.go", Change: "handle a|b\nand c"}})
	rows := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(rows) != 3 {
		t.Fatalf("a newline in a description broke the table into %d rows:\n%s", len(rows), got)
	}
	if !strings.Contains(rows[2], `a\|b`) {
		t.Errorf("the pipe was not escaped:\n%s", rows[2])
	}
	if n := strings.Count(strings.ReplaceAll(rows[2], `\|`, ""), "|"); n != 3 {
		t.Errorf("row has %d cell delimiters, want 3:\n%s", n, rows[2])
	}
}

// ---------------------------------------------------------------- verify --

func TestDetectVerifyCommand(t *testing.T) {
	cases := []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"Makefile": "check:\n\tgo test ./...\n", "go.mod": "module x\n"}, "make check"},
		{map[string]string{"Makefile": "build:\n\tgo build\ntest:\n\tgo test\n"}, "make test"},
		{map[string]string{"go.mod": "module x\n"}, "go test ./..."},
		{map[string]string{"package.json": `{"scripts":{"test":"jest"}}`}, "npm test"},
		{map[string]string{"pyproject.toml": "[project]\n", "uv.lock": ""}, "uv run pytest -q"},
		{map[string]string{"Cargo.toml": "[package]\n"}, "cargo test"},
		{map[string]string{"README.md": "hi"}, ""},
	}
	for _, c := range cases {
		dir := t.TempDir()
		for name, body := range c.files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := DetectVerifyCommand(dir); got != c.want {
			t.Errorf("DetectVerifyCommand(%v) = %q, want %q", keys(c.files), got, c.want)
		}
	}
}

func TestVerifyReportsExitCodeAndSkips(t *testing.T) {
	dir := t.TempDir()
	if got := Verify(context.Background(), execRunner, dir, "", time.Minute); !got.Skipped {
		t.Errorf("an empty command must be reported as skipped, got %+v", got)
	}
	if got := Verify(context.Background(), execRunner, dir, "false", time.Minute); got.OK {
		t.Errorf("a failing command must not be OK: %+v", got)
	}
	if got := Verify(context.Background(), execRunner, dir, "true", time.Minute); !got.OK {
		t.Errorf("a passing command must be OK: %+v", got)
	}
}

// ----------------------------------------------------------- the guard --

func TestToolGuard(t *testing.T) {
	allow := func(context.Context, core.BeforeToolCallContext) core.BeforeToolCallDecision {
		return core.BeforeToolCallDecision{}
	}
	cases := []struct {
		name      string
		readOnly  bool
		tool      string
		args      map[string]any
		wantBlock bool
	}{
		{"write during analysis", true, "write_file", map[string]any{"path": "a.go"}, true},
		{"write during implementation", false, "write_file", map[string]any{"path": "a.go"}, false},
		{"read during analysis", true, "read_file", map[string]any{"path": "a.go"}, false},
		{"git log", false, "execute", map[string]any{"command": "git log --oneline -5"}, false},
		{"git commit", false, "execute", map[string]any{"command": "git commit -m x"}, true},
		{"git commit behind a global flag", false, "execute", map[string]any{"command": "git -C . commit -m x"}, true},
		{"git push via run_command", false, "run_command", map[string]any{"argv": []any{"git", "push"}}, true},
		{"gh comment", false, "execute", map[string]any{"command": "gh issue comment 1 --body hi"}, true},
		{"go test", false, "execute", map[string]any{"command": "go test ./..."}, false},
	}
	for _, c := range cases {
		guard := toolGuard(allow, c.readOnly, func(string) {})
		got := guard(context.Background(), core.BeforeToolCallContext{ToolName: c.tool, Arguments: c.args})
		if got.Block != c.wantBlock {
			t.Errorf("%s: Block = %v, want %v (%s)", c.name, got.Block, c.wantBlock, got.Reason)
		}
	}
}

// --------------------------------------------------------- the pipeline --

// recordingHub is a Hub that keeps what would have been posted.
type recordingHub struct {
	issue    *Issue
	comments []string
	prs      []PullRequestSpec
	failPR   bool
}

func (h *recordingHub) Check(context.Context) error { return nil }

func (h *recordingHub) Issue(_ context.Context, ref IssueRef) (*Issue, error) {
	cp := *h.issue
	if cp.URL == "" {
		cp.URL = ref.URL()
	}
	return &cp, nil
}

func (h *recordingHub) PullRequest(context.Context, IssueRef, int) (*LinkedPR, error) {
	return nil, errors.New("no linked PRs in this fixture")
}

func (h *recordingHub) Comment(_ context.Context, _ IssueRef, body string) (string, error) {
	h.comments = append(h.comments, body)
	return "https://github.test/comment/1", nil
}

func (h *recordingHub) CreatePR(_ context.Context, _ IssueRef, pr PullRequestSpec) (string, error) {
	if h.failPR {
		return "", errors.New("simulated failure")
	}
	h.prs = append(h.prs, pr)
	return "https://github.test/pr/1", nil
}

// newRepo makes a git repository with one commit in it.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, argv := range [][]string{
		{"git", "init", "-q", "-b", "main"},
		{"git", "config", "user.email", "cleaner@example.test"},
		{"git", "config", "user.name", "cleaner"},
	} {
		if out, code, err := execRunner(context.Background(), dir, argv); err != nil || code != 0 {
			t.Fatalf("%v: %v %s", argv, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code, err := execRunner(context.Background(), dir, []string{"git", "add", "-A"}); err != nil || code != 0 {
		t.Fatalf("git add: %v %s", err, out)
	}
	if out, code, err := execRunner(context.Background(), dir, []string{"git", "commit", "-qm", "initial"}); err != nil || code != 0 {
		t.Fatalf("git commit: %v %s", err, out)
	}
	return dir
}

const analysisArgs = `{
  "classification": "bug",
  "confidence": "confirmed",
  "summary": "expire cached tokens before reuse",
  "root_cause": "` + "`session.go`" + ` reuses a cached token without checking its expiry.",
  "approach": "Check the expiry in the cache read path and refresh when it has passed.",
  "files": [{"path": "session.go", "change": "check expiry on the cached-token path"}],
  "assumptions": ["The clock is monotonic enough for a second-level comparison."]
}`

const implementationArgs = `{
  "summary": "The cached-token path now checks expiry and refreshes.",
  "commit_subject": "fix(session): expire cached tokens before reuse",
  "changes": [{"path": "session.go", "change": "expiry check added"}],
  "tests": [{"path": "session_test.go", "covers": "an expired cached token is refreshed"}],
  "limitations": []
}`

// scriptedBrain wires the REAL agentBrain to a scripted provider. The two
// phases share one faux.Provider, so its turns are replayed in pipeline order:
// the analysis submission first, then the implementation's write and
// submission.
func scriptedBrain(t *testing.T, dir string, turns ...faux.Turn) *agentBrain {
	t.Helper()
	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := faux.New(turns...)
	return &agentBrain{
		base: core.AgentConfig{
			Model:     faux.Model(),
			Providers: core.ProviderRegistry{faux.API: p.APIProvider()},
		},
		workspace: ws,
		progress:  io.Discard,
		maxTurns:  6,
		budgetUSD: 1,
	}
}

func toolTurn(id, name, args string) faux.Turn {
	return faux.Turn{
		Blocks:     []core.ContentBlock{faux.FauxToolCall(id, name, args)},
		StopReason: core.StopReasonToolUse,
	}
}

func baseOptions(t *testing.T, dir string, hub Hub, brain Brain) Options {
	t.Helper()
	return Options{
		Ref: IssueRef{"acme", "widgets", 42}, Dir: dir, Hub: hub,
		Git: NewGit(dir, execRunner), Brain: brain, Run: execRunner,
		VerifyCommand: "true", VerifyTimeout: time.Minute,
		Landing: LandNone, PushAttempts: 1, Out: io.Discard,
	}
}

func fixtureIssue() *Issue {
	return &Issue{
		Title:  "Cached session tokens are reused after they expire",
		Body:   "Requests fail with 401 after an hour.",
		Author: Author{Login: "reporter"},
		Labels: []Label{{Name: "bug"}},
	}
}

// TestPipelineEndToEnd drives the whole program: pre-flight, the real agent
// loop over the real file tools, a real branch and commit in a real git
// repository, and both issue comments.
func TestPipelineEndToEnd(t *testing.T) {
	dir := newRepo(t)
	hub := &recordingHub{issue: fixtureIssue()}
	brain := scriptedBrain(t, dir,
		toolTurn("c1", "submit_analysis", analysisArgs),
		toolTurn("c2", "write_file", `{"path":"session.go","content":"package session\n\n// expiry checked\n"}`),
		toolTurn("c3", "submit_implementation", implementationArgs),
	)

	res, err := Run(context.Background(), baseOptions(t, dir, hub, brain))
	if err != nil {
		t.Fatalf("Run failed at %s: %v", res.Stage, err)
	}

	if res.Branch != "fix/issue-42-cached-session-tokens-are-reused" {
		t.Errorf("branch = %q", res.Branch)
	}
	if res.Commit == "" {
		t.Error("nothing was committed")
	}
	if got := res.Changed; len(got) != 1 || got[0] != "session.go" {
		t.Errorf("changed files = %v, want [session.go]", got)
	}
	if !res.Verification.OK {
		t.Errorf("verification = %+v", res.Verification)
	}

	// The branch is real, the commit is on it, and the file is in the commit.
	g := NewGit(dir, execRunner)
	if branch, _ := g.CurrentBranch(context.Background()); branch != res.Branch {
		t.Errorf("checked-out branch = %q, want %q", branch, res.Branch)
	}
	if dirty, _ := g.DirtyFiles(context.Background()); len(dirty) != 0 {
		t.Errorf("working tree is dirty after the run: %v", dirty)
	}
	out, _, _ := execRunner(context.Background(), dir, []string{"git", "show", "--stat", "--format=%s"})
	if !strings.Contains(out, "fix(session): expire cached tokens before reuse") || !strings.Contains(out, "session.go") {
		t.Errorf("commit does not look right:\n%s", out)
	}

	// Two comments: the analysis before the work, the summary after it.
	if len(hub.comments) != 2 {
		t.Fatalf("posted %d comments, want 2:\n%s", len(hub.comments), strings.Join(hub.comments, "\n---\n"))
	}
	if !strings.Contains(hub.comments[0], "## Analysis") || !strings.Contains(hub.comments[0], res.Branch) {
		t.Errorf("first comment is not the analysis:\n%s", hub.comments[0])
	}
	if !strings.Contains(hub.comments[1], "## Fix implemented") || !strings.Contains(hub.comments[1], "✅") {
		t.Errorf("second comment is not the summary:\n%s", hub.comments[1])
	}

	// And the ordering claim this program makes: the analysis comment is
	// posted only after the branch exists, and the summary only after the
	// verification ran.
	assertOrder(t, res, "preflight", "fetch-issue", "baseline", "analyze", "branch",
		"post-analysis", "implement", "verify", "commit", "post-summary")
}

// TestPipelineStopsOnAmbiguity checks the one path that halts on purpose.
func TestPipelineStopsOnAmbiguity(t *testing.T) {
	dir := newRepo(t)
	hub := &recordingHub{issue: fixtureIssue()}
	args := strings.TrimSuffix(strings.TrimSpace(analysisArgs), "}") +
		`, "clarification": {"question": "Refresh or fail?", "option_a": "refresh", "option_b": "fail"}}`
	brain := scriptedBrain(t, dir, toolTurn("c1", "submit_analysis", args))

	res, err := Run(context.Background(), baseOptions(t, dir, hub, brain))
	if err != nil {
		t.Fatalf("a clarification is not an error: %v", err)
	}
	if !res.NeedsClarification {
		t.Fatal("NeedsClarification not set")
	}
	if res.Branch != "" || res.Commit != "" {
		t.Errorf("nothing may be created before the question is answered: branch=%q commit=%q", res.Branch, res.Commit)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "Clarification needed") {
		t.Fatalf("comments = %v", hub.comments)
	}
	if branch, _ := NewGit(dir, execRunner).CurrentBranch(context.Background()); branch != "main" {
		t.Errorf("still on %q, want main", branch)
	}
}

// TestPipelineRefusesToLandAFailingChange is the property the whole "verify in
// Go, not by asking the model" split exists for.
func TestPipelineRefusesToLandAFailingChange(t *testing.T) {
	dir := newRepo(t)
	hub := &recordingHub{issue: fixtureIssue()}
	brain := scriptedBrain(t, dir,
		toolTurn("c1", "submit_analysis", analysisArgs),
		toolTurn("c2", "write_file", `{"path":"session.go","content":"package session\n"}`),
		toolTurn("c3", "submit_implementation", implementationArgs),
	)

	opts := baseOptions(t, dir, hub, brain)
	opts.VerifyCommand = "false" // the suite fails after the change
	opts.SkipBaseline = true

	res, err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("a run whose checks fail must not report success")
	}
	if res.Stage != "verify" {
		t.Errorf("Stage = %q, want verify", res.Stage)
	}
	if res.Commit != "" {
		t.Error("a failing change must not be committed")
	}
	last := hub.comments[len(hub.comments)-1]
	if !strings.Contains(last, "Automated fix attempt failed") {
		t.Errorf("the failure was not reported on the issue:\n%s", last)
	}
}

// TestPipelineRefusesAnEmptyChange covers the model that says it fixed
// something and changed nothing.
func TestPipelineRefusesAnEmptyChange(t *testing.T) {
	dir := newRepo(t)
	hub := &recordingHub{issue: fixtureIssue()}
	brain := scriptedBrain(t, dir,
		toolTurn("c1", "submit_analysis", analysisArgs),
		toolTurn("c2", "submit_implementation", implementationArgs),
	)

	res, err := Run(context.Background(), baseOptions(t, dir, hub, brain))
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v, want a complaint about an empty diff", err)
	}
	if res.Stage != "verify" {
		t.Errorf("Stage = %q, want verify", res.Stage)
	}
}

// TestPipelineRefusesADirtyTree covers the pre-flight that af-fix runs too
// late: here it happens before anything is fetched or posted.
func TestPipelineRefusesADirtyTree(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := &recordingHub{issue: fixtureIssue()}
	brain := scriptedBrain(t, dir, toolTurn("c1", "submit_analysis", analysisArgs))

	res, err := Run(context.Background(), baseOptions(t, dir, hub, brain))
	if err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("err = %v, want a dirty-tree refusal", err)
	}
	if res.Stage != "preflight" {
		t.Errorf("Stage = %q, want preflight", res.Stage)
	}
	if len(hub.comments) != 0 {
		t.Errorf("nothing may be posted when the run never started: %v", hub.comments)
	}
}

// TestPipelineSurvivesAFailedPullRequest: the branch is pushed and the change
// is verified, so a pull request that could not be opened is a warning, not a
// failed run.
func TestPipelineDegradesWhenThePullRequestFails(t *testing.T) {
	dir := newRepo(t)
	hub := &recordingHub{issue: fixtureIssue(), failPR: true}
	brain := scriptedBrain(t, dir,
		toolTurn("c1", "submit_analysis", analysisArgs),
		toolTurn("c2", "write_file", `{"path":"session.go","content":"package session\n"}`),
		toolTurn("c3", "submit_implementation", implementationArgs),
	)

	opts := baseOptions(t, dir, hub, brain)
	opts.Landing = LandPR
	opts.DryRun = true // no remote in a temp repository, so do not really push

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run failed at %s: %v", res.Stage, err)
	}
	if res.PRURL != "" {
		t.Errorf("PRURL = %q, want empty", res.PRURL)
	}
	if len(res.Warnings) == 0 {
		t.Error("a failed pull request must be warned about")
	}
}

func assertOrder(t *testing.T, res *Result, want ...string) {
	t.Helper()
	var got []string
	for _, e := range res.Journal {
		if e.Step != "warning" {
			got = append(got, e.Step)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("step order:\n got %v\nwant %v", got, want)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestTokenTimingFormatting(t *testing.T) {
	formatted := FormatTokenTiming(1500*time.Millisecond, 100, 50)
	if formatted != "(2s) · 100↑ 50↓" {
		t.Errorf("FormatTokenTiming = %q, want %q", formatted, "(2s) · 100↑ 50↓")
	}

	// Sub-second duration (< 1s) formatted in milliseconds
	if subSec := FormatTokenTiming(500*time.Millisecond, 100, 50); subSec != "(500ms) · 100↑ 50↓" {
		t.Errorf("FormatTokenTiming (sub-second) = %q, want %q", subSec, "(500ms) · 100↑ 50↓")
	}

	// Minutes duration with spaced units and k-scale tokens
	if minTiming := FormatTokenTiming(28*time.Minute+16*time.Second, 153, 60600); minTiming != "(28m 16s) · 153↑ 60.6k↓" {
		t.Errorf("FormatTokenTiming (minute + k) = %q, want %q", minTiming, "(28m 16s) · 153↑ 60.6k↓")
	}

	// Seconds rounded and M-scale tokens
	if mTiming := FormatTokenTiming(65*time.Second, 1_200_000, 2_500_000); mTiming != "(1m 5s) · 1.2M↑ 2.5M↓" {
		t.Errorf("FormatTokenTiming (M tokens) = %q, want %q", mTiming, "(1m 5s) · 1.2M↑ 2.5M↓")
	}

	stats := RunStats{
		Phase:      "analyze",
		Turns:      2,
		StopReason: core.RunStopToolTerminate,
		Usage: core.Usage{
			InputTokens:  120,
			OutputTokens: 80,
		},
		CostUSD: 0.0012,
		Elapsed: 1200 * time.Millisecond,
	}

	if got := stats.TokenTiming(); got != "(1s) · 120↑ 80↓" {
		t.Errorf("TokenTiming() = %q, want %q", got, "(1s) · 120↑ 80↓")
	}

	costFree := stats.TimingWithoutCost()
	if strings.Contains(costFree, "$") {
		t.Errorf("TimingWithoutCost() contains dollar sign: %q", costFree)
	}
	if !strings.Contains(costFree, "analyze") || !strings.Contains(costFree, "2 turns") || !strings.Contains(costFree, "(1s) · 120↑ 80↓") || !strings.Contains(costFree, "tool_terminate") {
		t.Errorf("TimingWithoutCost() missing expected metrics: %q", costFree)
	}

	verboseStr := stats.String()
	if !strings.Contains(verboseStr, "$") || !strings.Contains(verboseStr, "0.00120") {
		t.Errorf("String() missing cost figures: %q", verboseStr)
	}
}

func TestSpinnerAndPhaseTimingNonVerbose(t *testing.T) {
	dir := newRepo(t)
	hub := &recordingHub{issue: fixtureIssue()}
	brain := scriptedBrain(t, dir,
		toolTurn("c1", "submit_analysis", analysisArgs),
		toolTurn("c2", "write_file", `{"path":"session.go","content":"package session\n\n// expiry checked\n"}`),
		toolTurn("c3", "submit_implementation", implementationArgs),
	)

	var buf bytes.Buffer
	opts := baseOptions(t, dir, hub, brain)
	opts.Out = &buf
	opts.Verbose = false

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	_ = res

	out := buf.String()

	// In non-verbose terminal mode (buf is *bytes.Buffer, detected as terminal):
	// 1. Initial spinner frame '|' and stop cleanup '\b \b' should appear
	if !strings.Contains(out, "|") || !strings.Contains(out, "\b \b") {
		t.Errorf("expected spinner frames and cleanup in output, got: %q", out)
	}

	// 2. Completion timing line with tokens should appear
	if !strings.Contains(out, "[cleaner] preflight") || !strings.Contains(out, "0↑ 0↓") {
		t.Errorf("missing preflight timing line in output: %q", out)
	}
	if !strings.Contains(out, "[cleaner] analyze") || !strings.Contains(out, "↑") || !strings.Contains(out, "↓") {
		t.Errorf("missing analyze timing line in output: %q", out)
	}

	// 3. No checkmarks (✓) or dollar costs ($) in non-verbose step output
	if strings.Contains(out, "✓") {
		t.Errorf("non-verbose step output should suppress checkmark details (✓), got: %q", out)
	}
	if strings.Contains(out, "$") {
		t.Errorf("non-verbose step output should not contain dollar figures ($), got: %q", out)
	}

	// 4. Non-terminal stream should suppress spinner characters
	var nonTermBuf bytes.Buffer
	nonTermWriter := struct{ io.Writer }{&nonTermBuf}
	dir2 := newRepo(t)
	hub2 := &recordingHub{issue: fixtureIssue()}
	brain2 := scriptedBrain(t, dir2,
		toolTurn("c1", "submit_analysis", analysisArgs),
		toolTurn("c2", "write_file", `{"path":"session.go","content":"package session\n\n// expiry checked\n"}`),
		toolTurn("c3", "submit_implementation", implementationArgs),
	)
	opts2 := baseOptions(t, dir2, hub2, brain2)
	opts2.Out = nonTermWriter
	opts2.Verbose = false

	if _, err := Run(context.Background(), opts2); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	nonTermOut := nonTermBuf.String()
	if strings.Contains(nonTermOut, "|") || strings.Contains(nonTermOut, "\b") {
		t.Errorf("non-terminal stream should not contain spinner characters, got: %q", nonTermOut)
	}
	if !strings.Contains(nonTermOut, "[cleaner] preflight (") {
		t.Errorf("non-terminal stream missing phase timing line, got: %q", nonTermOut)
	}
}

func TestAgentBrainRetriesOnTransient503Error(t *testing.T) {
	dir := newRepo(t)

	// Analyze phase retries on transient 503 error.
	brainAnalyze := scriptedBrain(t, dir,
		faux.Turn{Err: errors.New("google: HTTP 503: UNAVAILABLE: The service is currently unavailable.")},
		toolTurn("c1", "submit_analysis", analysisArgs),
	)
	analysis, stats, err := brainAnalyze.Analyze(context.Background(), AnalysisInput{
		Ref:   IssueRef{"acme", "widgets", 42},
		Issue: fixtureIssue(),
	})
	if err != nil {
		t.Fatalf("Analyze failed: %v", err)
	}
	if analysis.Summary == "" {
		t.Error("expected non-empty analysis summary")
	}
	if stats.StopReason != core.RunStopToolTerminate {
		t.Errorf("StopReason = %s, want %s", stats.StopReason, core.RunStopToolTerminate)
	}

	// Implement phase retries on transient 503 error.
	brainImplement := scriptedBrain(t, dir,
		faux.Turn{Err: errors.New("google: HTTP 503: UNAVAILABLE: The service is currently unavailable.")},
		toolTurn("c1", "submit_implementation", implementationArgs),
	)
	imp, statsImp, err := brainImplement.Implement(context.Background(), ImplementInput{
		Ref:   IssueRef{"acme", "widgets", 42},
		Issue: fixtureIssue(),
	})
	if err != nil {
		t.Fatalf("Implement failed: %v", err)
	}
	if imp.Summary == "" {
		t.Error("expected non-empty implementation summary")
	}
	if statsImp.StopReason != core.RunStopToolTerminate {
		t.Errorf("StopReason = %s, want %s", statsImp.StopReason, core.RunStopToolTerminate)
	}
}

func TestAgentBrainVerboseVsNonVerbose(t *testing.T) {
	dir := newRepo(t)
	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}

	makeBrain := func(verbose bool, out *bytes.Buffer) *agentBrain {
		p := faux.New(
			toolTurn("c0", "execute", `{"command":"git commit -m wip"}`),
			toolTurn("c1", "submit_analysis", analysisArgs),
		)
		return &agentBrain{
			base: core.AgentConfig{
				Model:     faux.Model(),
				Providers: core.ProviderRegistry{faux.API: p.APIProvider()},
			},
			workspace: ws,
			progress:  out,
			maxTurns:  6,
			budgetUSD: 1,
			verbose:   verbose,
		}
	}

	// Non-verbose suppresses tool call logs (→ and ←) and guard blocked logs
	var nonVerboseBuf bytes.Buffer
	bNonVerbose := makeBrain(false, &nonVerboseBuf)
	_, _, err = bNonVerbose.Analyze(context.Background(), AnalysisInput{
		Ref:   IssueRef{"acme", "widgets", 42},
		Issue: fixtureIssue(),
	})
	if err != nil {
		t.Fatalf("Analyze failed: %v", err)
	}
	if strings.Contains(nonVerboseBuf.String(), "→") || strings.Contains(nonVerboseBuf.String(), "←") || strings.Contains(nonVerboseBuf.String(), "blocked") {
		t.Errorf("non-verbose brain output should suppress tool calls and blocked notifications, got: %q", nonVerboseBuf.String())
	}

	// Verbose preserves tool call logs (→ and ←) and guard blocked logs
	var verboseBuf bytes.Buffer
	bVerbose := makeBrain(true, &verboseBuf)
	_, _, err = bVerbose.Analyze(context.Background(), AnalysisInput{
		Ref:   IssueRef{"acme", "widgets", 42},
		Issue: fixtureIssue(),
	})
	if err != nil {
		t.Fatalf("Analyze failed: %v", err)
	}
	if !strings.Contains(verboseBuf.String(), "→ submit_analysis") || !strings.Contains(verboseBuf.String(), "← submit_analysis") {
		t.Errorf("verbose brain output should retain tool calls, got: %q", verboseBuf.String())
	}
	if !strings.Contains(verboseBuf.String(), "blocked execute") {
		t.Errorf("verbose brain output should retain blocked notifications, got: %q", verboseBuf.String())
	}
}

func TestVerboseFlagOutputRetention(t *testing.T) {
	dir := newRepo(t)
	hub := &recordingHub{issue: fixtureIssue()}
	brain := scriptedBrain(t, dir,
		toolTurn("c1", "submit_analysis", analysisArgs),
		toolTurn("c2", "write_file", `{"path":"session.go","content":"package session\n\n// expiry checked\n"}`),
		toolTurn("c3", "submit_implementation", implementationArgs),
	)

	var buf bytes.Buffer
	opts := baseOptions(t, dir, hub, brain)
	opts.Out = &buf
	opts.Verbose = true

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	_ = res

	out := buf.String()
	// In verbose mode, checkmark details are preserved
	if !strings.Contains(out, "✓") {
		t.Errorf("verbose output should retain checkmark details (✓), got: %q", out)
	}
	// And spinner characters are not printed
	if strings.Contains(out, "\b \b") {
		t.Errorf("verbose output should not display spinner, got: %q", out)
	}
}

func TestSummaryVerboseVsNonVerbose(t *testing.T) {
	res := &Result{
		Ref:    IssueRef{"acme", "widgets", 42},
		Branch: "fix/issue-42-test",
		Commit: "abcdef",
		Stats: []RunStats{
			{
				Phase:      "analyze",
				Turns:      2,
				StopReason: core.RunStopToolTerminate,
				Usage: core.Usage{
					InputTokens:  100,
					OutputTokens: 50,
				},
				CostUSD: 0.005,
				Elapsed: 1500 * time.Millisecond,
			},
		},
	}

	var nonVerboseBuf bytes.Buffer
	summaryTo(&nonVerboseBuf, res, nil, false)
	nonVerboseOut := nonVerboseBuf.String()

	if strings.Contains(nonVerboseOut, "$") {
		t.Errorf("non-verbose summary should omit dollar cost, got: %q", nonVerboseOut)
	}
	if strings.Contains(nonVerboseOut, "cost:") {
		t.Errorf("non-verbose summary should omit cost line, got: %q", nonVerboseOut)
	}
	if !strings.Contains(nonVerboseOut, "100↑ 50↓") {
		t.Errorf("non-verbose summary missing tokens: %q", nonVerboseOut)
	}

	var verboseBuf bytes.Buffer
	summaryTo(&verboseBuf, res, nil, true)
	verboseOut := verboseBuf.String()

	if !strings.Contains(verboseOut, "$0.00500") {
		t.Errorf("verbose summary should include detailed stats with dollar cost, got: %q", verboseOut)
	}
	if !strings.Contains(verboseOut, "cost:     $0.0050") {
		t.Errorf("verbose summary should include cost line, got: %q", verboseOut)
	}
}
