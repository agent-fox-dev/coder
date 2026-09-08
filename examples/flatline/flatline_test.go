package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	afspec "github.com/agent-fox-dev/spec"
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/faux"
	"github.com/agentfox/agentkit-go/tools"
)

// Nothing here needs a key or a network. The deterministic half is tested
// directly, and the agent half runs against provider/faux, so the pipeline
// tests drive the REAL Run — the real agent loop, the real file tools, a real
// git repository, the real spec library writing tasks.json — with a scripted
// model.

// ------------------------------------------------------------- fixtures --

const fixtureSpec = "01_widget_counter"

// newRepo makes a git repository holding the fixture pack under .specs/, a
// placeholder-only steering file and an AGENTS.md, with one commit.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	must := func(argv ...string) {
		t.Helper()
		if out, code, err := execRunner(context.Background(), dir, argv); err != nil || code != 0 {
			t.Fatalf("%v: %v %s", argv, err, out)
		}
	}
	must("git", "init", "-q", "-b", "main")
	must("git", "config", "user.email", "flatline@example.test")
	must("git", "config", "user.name", "flatline")
	copyDir(t, filepath.Join("testdata", "specs", fixtureSpec), filepath.Join(dir, ".specs", fixtureSpec))
	write(t, filepath.Join(dir, ".specs", "steering.md"), "<!-- steering:placeholder -->\n<!-- add directives below -->\n")
	write(t, filepath.Join(dir, "AGENTS.md"), "# Agent Instructions\n\nRun make check before committing.\n")
	write(t, filepath.Join(dir, "README.md"), "# fixture\n")
	must("git", "add", "-A")
	must("git", "commit", "-qm", "initial")
	return dir
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dst, e.Name()), string(b))
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadPack(t *testing.T, root string) *Pack {
	t.Helper()
	pack, _, err := LoadPack(root, filepath.Join(root, ".specs", fixtureSpec))
	if err != nil {
		t.Fatalf("LoadPack: %v", err)
	}
	// The fixture's real commands need a Go module; the pipeline tests decide
	// pass and fail with the shell's own true and false.
	pack.Spec.Tasks.TestCommands = afspec.TestCommands{Linter: "true", SpecTests: "true", AllTests: "true"}
	return pack
}

// scriptedBrain wires the REAL agentBrain to a scripted provider. All sessions
// of a run share one faux.Provider, so its turns replay in pipeline order.
type scripted struct {
	*agentBrain
	provider *faux.Provider
}

func scriptedBrain(t *testing.T, dir string, turns ...faux.Turn) *scripted {
	t.Helper()
	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := faux.New(turns...)
	return &scripted{provider: p, agentBrain: &agentBrain{
		base: core.AgentConfig{
			Model:     faux.Model(),
			Providers: core.ProviderRegistry{faux.API: p.APIProvider()},
		},
		workspace: ws,
		progress:  io.Discard,
		maxTurns:  6,
		budgetUSD: 1,
	}}
}

func toolTurn(id, name, args string) faux.Turn {
	return faux.Turn{
		Blocks:     []core.ContentBlock{faux.FauxToolCall(id, name, args)},
		StopReason: core.StopReasonToolUse,
	}
}

func writeTurn(id, path, content string) faux.Turn {
	b, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return toolTurn(id, "write_file", string(b))
}

func submitGroup(id, subject, summary string) faux.Turn {
	b, _ := json.Marshal(map[string]any{
		"summary":        summary,
		"commit_subject": subject,
		"changes":        []map[string]string{{"path": "x", "change": "y"}},
		"gotchas":        []string{"the counter is not goroutine-safe"},
	})
	return toolTurn(id, "submit_group", string(b))
}

func submitGate(id string, passed bool) faux.Turn {
	b, _ := json.Marshal(map[string]any{
		"passed":  passed,
		"results": []map[string]any{{"check": "go test ./...", "passed": passed, "output": "FAIL widget"}},
	})
	return toolTurn(id, "submit_gate", string(b))
}

func submitVerdicts(id, overall string) faux.Turn {
	b, _ := json.Marshal(map[string]any{
		"verdicts":        []map[string]string{{"requirement_id": "01-REQ-1.1", "verdict": overall, "evidence": "TestWidgetAdd"}},
		"overall_verdict": overall,
		"summary":         "checked",
	})
	return toolTurn(id, "submit_verdicts", string(b))
}

// happyTurns scripts the whole fixture: two coder groups, a gate, a coder.
func happyTurns() []faux.Turn {
	return []faux.Turn{
		writeTurn("c1", "counter_test.go", "package widget\n// failing tests\n"),
		submitGroup("c2", "test(counter): add failing spec tests", "Group 1 of widget_counter: wrote the failing tests."),
		writeTurn("c3", "counter.go", "package widget\n// Counter\n"),
		submitGroup("c4", "feat(counter): implement Add and Total", "Group 2 of widget_counter: implemented the counter."),
		submitGate("c5", true),
		writeTurn("c6", "docs/wiring.md", "traced\n"),
		submitGroup("c7", "chore(counter): wiring verification", "Group 4 of widget_counter: traced 01-PATH-1."),
	}
}

func baseOptions(t *testing.T, root string, pack *Pack, brain Brain) Options {
	t.Helper()
	return Options{
		Pack: pack, Git: NewGit(root, execRunner), Brain: brain, Run: execRunner,
		Landing: LandMerge, MaxRetries: 2, CheckTimeout: time.Minute, Out: io.Discard,
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, code, err := execRunner(context.Background(), dir, append([]string{"git"}, args...))
	if err != nil || code != 0 {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(out)
}

func readTasks(t *testing.T, root string) *afspec.TasksV1Json {
	t.Helper()
	spec, err := afspec.LoadSpec(filepath.Join(root, ".specs", fixtureSpec))
	if err != nil {
		t.Fatal(err)
	}
	return spec.Tasks
}

func subtaskState(tasks *afspec.TasksV1Json, id string) afspec.SubtaskState {
	for _, g := range tasks.TaskGroups {
		for _, s := range g.Subtasks {
			if s.Id == id {
				return s.State
			}
		}
	}
	return "missing"
}

func journalSteps(res *Result) []string {
	var out []string
	for _, e := range res.Journal {
		if e.Step != "warning" {
			out = append(out, e.Step)
		}
	}
	return out
}

func assertOrder(t *testing.T, res *Result, steps ...string) {
	t.Helper()
	got := journalSteps(res)
	i := 0
	for _, s := range got {
		if i < len(steps) && s == steps[i] {
			i++
		}
	}
	if i != len(steps) {
		t.Errorf("journal does not contain %v in order:\n  %s", steps, strings.Join(got, "\n  "))
	}
}

// lastUserText is the newest user turn of the newest request.
func lastUserText(t *testing.T, p *faux.Provider) string {
	t.Helper()
	reqs := p.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests were made")
	}
	msgs := reqs[len(reqs)-1].Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if u, ok := msgs[i].(core.UserMessage); ok {
			return u.Content.Text()
		}
	}
	return ""
}

func systemText(req core.Request) string {
	var b strings.Builder
	for _, blk := range req.System {
		if tb, ok := blk.(core.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// ----------------------------------------------------------------- pack --

func TestFixturePackLoadsAndOrders(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)
	groups := pack.Groups()
	if len(groups) != 4 {
		t.Fatalf("groups = %d, want 4", len(groups))
	}
	for i, g := range groups {
		if g.Id != i+1 {
			t.Errorf("group[%d].Id = %d", i, g.Id)
		}
	}
	if ArchetypeFor(groups[2].Kind) != Gate || ArchetypeFor(groups[1].Kind) != Coder {
		t.Errorf("archetypes: checkpoint → %s, standard → %s", ArchetypeFor(groups[2].Kind), ArchetypeFor(groups[1].Kind))
	}
	if pack.Project == "" || !strings.Contains(pack.Project, "make check") {
		t.Errorf("AGENTS.md was not picked up: %q", pack.Project)
	}
	if pack.Steering != "" {
		t.Errorf("placeholder-only steering must be ignored, got %q", pack.Steering)
	}
}

func TestResolveSpecDir(t *testing.T) {
	root := newRepo(t)
	specs := filepath.Join(root, ".specs")
	if err := os.MkdirAll(filepath.Join(specs, "10_other"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"1", "01", fixtureSpec, filepath.Join(specs, fixtureSpec)} {
		got, err := ResolveSpecDir(specs, arg)
		if err != nil || filepath.Base(got) != fixtureSpec {
			t.Errorf("ResolveSpecDir(%q) = %q, %v", arg, got, err)
		}
	}
	if _, err := ResolveSpecDir(specs, "7"); err == nil {
		t.Error("a missing number must fail")
	}
	if _, err := ResolveSpecDir(specs, "nope"); err == nil {
		t.Error("a missing name must fail")
	}
}

func TestLoadPackRefusesDependenciesAndSealedSpecs(t *testing.T) {
	root := newRepo(t)
	specDir := filepath.Join(root, ".specs", fixtureSpec)

	tasksPath := filepath.Join(specDir, "tasks.json")
	b, _ := os.ReadFile(tasksPath)
	withDep := strings.Replace(string(b), `"dependencies": []`,
		`"dependencies": [{"depends_on_spec": "02", "from_group": 1, "to_group": 1, "relationship": "x", "sentinel": false}]`, 1)
	write(t, tasksPath, withDep)
	if _, _, err := LoadPack(root, specDir); err == nil || !strings.Contains(err.Error(), "dependenc") {
		t.Errorf("a pack with dependencies must be refused, got %v", err)
	}
	write(t, tasksPath, string(b))

	prdPath := filepath.Join(specDir, "prd.md")
	p, _ := os.ReadFile(prdPath)
	write(t, prdPath, strings.Replace(string(p), `status: "draft"`, `status: "sealed"`, 1))
	if _, _, err := LoadPack(root, specDir); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Errorf("a sealed pack must be refused, got %v", err)
	}
}

func TestMarkGroupDoneWalksTheStateMachine(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)

	// 2.2 is dropped beforehand and must stay dropped.
	for gi := range pack.Spec.Tasks.TaskGroups {
		for si := range pack.Spec.Tasks.TaskGroups[gi].Subtasks {
			if pack.Spec.Tasks.TaskGroups[gi].Subtasks[si].Id == "2.2" {
				pack.Spec.Tasks.TaskGroups[gi].Subtasks[si].State = afspec.SubtaskStateDropped
			}
		}
	}
	if GroupComplete(pack.Spec.Tasks.TaskGroups[1]) {
		t.Fatal("group 2 is not complete yet")
	}
	if err := pack.MarkGroupDone(2); err != nil {
		t.Fatal(err)
	}
	onDisk := readTasks(t, root)
	if s := subtaskState(onDisk, "2.1"); s != afspec.SubtaskStateDone {
		t.Errorf("2.1 = %s, want done", s)
	}
	if s := subtaskState(onDisk, "2.2"); s != afspec.SubtaskStateDropped {
		t.Errorf("2.2 = %s, want dropped (never touched)", s)
	}
	if s := subtaskState(onDisk, "1.1"); s != afspec.SubtaskStatePending {
		t.Errorf("1.1 = %s, want pending (other groups untouched)", s)
	}
	g, _ := pack.Group(2)
	if !GroupComplete(g) {
		t.Error("group 2 should now be complete")
	}
	// The file is the library's canonical encoding: loading it back works and
	// the key order is the schema's.
	raw, _ := os.ReadFile(filepath.Join(pack.Dir, "tasks.json"))
	if !strings.HasPrefix(string(raw), "{\n  \"$schema\"") {
		t.Errorf("tasks.json is not in canonical form:\n%s", firstLine(string(raw), 60))
	}
}

func TestAssembleContextIsScopedToTheGroup(t *testing.T) {
	root := newRepo(t)
	write(t, filepath.Join(root, ".specs", "steering.md"), "<!-- steering:placeholder -->\nAlways prefer table-driven tests.\n")
	pack := loadPack(t, root)

	ctx := pack.AssembleContext(Coder, 2, []string{"[CONTEXT] task group 1: wrote the tests"})
	for _, want := range []string{
		"## Requirements", "## Test Specification", "## Test Commands", "## Tasks",
		"### 2. Implement the counter", "- [ ] 1. Write failing spec tests", // other groups summarised
		"## Steering Directives", "Always prefer table-driven tests.",
		"## Project Instructions", "## Memory Facts", "[CONTEXT] task group 1",
		"- Linter: `true`",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("coder context lacks %q", want)
		}
	}
	if strings.Contains(ctx, "### 1. Write failing spec tests") {
		t.Error("group 1 must be a one-line summary in group 2's context")
	}
	if !strings.Contains(ctx, "## Architecture\n\n_(Omitted") && pack.Spec.Architecture != "" {
		t.Error("architecture handling")
	}

	gate := pack.AssembleContext(Gate, 3, nil)
	if !strings.Contains(gate, "## Test Specification\n\n_(Omitted — not required for this session.)_") {
		t.Error("the gate must get the omission note for the test spec")
	}
	ver := pack.AssembleContext(Verifier, 4, nil)
	if !strings.Contains(ver, "## Verification Checklist") || !strings.Contains(ver, "| 01-REQ-1.1 | covered | TS-01-1 |") {
		t.Errorf("verifier context lacks the coverage table:\n%s", ver)
	}
}

func TestLoadSteering(t *testing.T) {
	root := t.TempDir()
	if got := LoadSteering(root); got != "" {
		t.Errorf("missing file → %q", got)
	}
	write(t, filepath.Join(root, ".specs", "steering.md"), "<!-- steering:placeholder -->\n<!--\n  help\n-->\n")
	if got := LoadSteering(root); got != "" {
		t.Errorf("placeholder-only → %q", got)
	}
	write(t, filepath.Join(root, ".specs", "steering.md"), "Use pytest parametrize.\n")
	if got := LoadSteering(root); got != "Use pytest parametrize." {
		t.Errorf("content → %q", got)
	}
	write(t, filepath.Join(root, "real.md"), "linked\n")
	os.Remove(filepath.Join(root, ".specs", "steering.md"))
	if err := os.Symlink(filepath.Join(root, "real.md"), filepath.Join(root, ".specs", "steering.md")); err == nil {
		if got := LoadSteering(root); got != "" {
			t.Errorf("symlink must be refused, got %q", got)
		}
	}
}

// -------------------------------------------------------------- prompts --

func TestPromptsCarryRetryAndPreflight(t *testing.T) {
	checks := []Check{{Name: checkLinter, Command: "ruff check"}}
	p := CoderTaskPrompt(2, "widget_counter", afspec.TaskGroupKindStandard, checks, 1, "", "")
	for _, want := range []string{"Implement task group 2 from specification `widget_counter`.",
		"Complete all subtasks in group 2.", "Do not modify tasks.json", "flatline runs `ruff check` after you stop"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "retry attempt") {
		t.Error("a first attempt must not mention a retry")
	}
	p = CoderTaskPrompt(1, "widget_counter", afspec.TaskGroupKindTests, checks, 2, "quality gate failed: ruff exited 1", PreflightSummary(true, &CheckRun{Command: "make test", ExitCode: 2}))
	for _, want := range []string{"**Note:** This is retry attempt 2. The previous attempt failed with:", "ruff exited 1",
		"Please address this error.", "expected to FAIL", "## Preflight State", "Subtask checkboxes: all complete", "fail (`make test` exited 2)"} {
		if !strings.Contains(p, want) {
			t.Errorf("retry prompt lacks %q:\n%s", want, p)
		}
	}
	if g := GateTaskPrompt("s", 3, 1, ""); !strings.Contains(g, "gate role for task group 3") {
		t.Errorf("gate prompt: %s", g)
	}
}

func TestCommitAndSquashMessages(t *testing.T) {
	msg := GroupCommitMessage("widget_counter", 2, "Implement the counter", GroupReport{CommitSubject: "feat(counter): add Add", Summary: "body"})
	if !strings.HasPrefix(msg, "feat(counter): add Add\n\nbody") {
		t.Errorf("well-formed subject not kept:\n%s", msg)
	}
	msg = GroupCommitMessage("widget_counter", 2, "Implement the counter", GroupReport{CommitSubject: "Did stuff!"})
	if !strings.HasPrefix(msg, "feat: implement the counter (widget_counter task group 2)") {
		t.Errorf("malformed subject not replaced:\n%s", msg)
	}
	if got := GroupCommitMessage("s", 1, "Write failing spec tests", GroupReport{}); !strings.HasPrefix(got, "test: ") {
		t.Errorf("group 1 fallback type = %q", firstLine(got, 40))
	}

	sq := SquashMessage("feat(counter): add Add\n\nbody\n", []string{"feat(counter): add Add", "chore: mark task group 2 subtasks done"})
	if sq != "feat(counter): add Add\n\nbody\n" {
		t.Errorf("housekeeping subject must not be listed:\n%q", sq)
	}
	sq = SquashMessage("fix: second\n", []string{"feat: first", "fix: second", "chore: mark task group 2 subtasks done"})
	if !strings.Contains(sq, "Also on this branch:\n- feat: first\n") || strings.Contains(sq, "chore: mark") {
		t.Errorf("squash message:\n%s", sq)
	}
}

// --------------------------------------------------------------- checks --

func TestRunCheckUsesTheShell(t *testing.T) {
	dir := t.TempDir()
	run := func(c string) CheckRun {
		return RunCheck(context.Background(), execRunner, dir, Check{Name: checkLinter, Command: c}, time.Minute)
	}
	if r := run("echo one && echo two"); !r.OK || !strings.Contains(r.Output, "two") {
		t.Errorf("compound command: %+v", r)
	}
	if r := run("echo boom && exit 3"); r.OK || r.ExitCode != 3 || !strings.Contains(r.Output, "boom") {
		t.Errorf("failing command: %+v", r)
	}
	if r := run("   "); !r.Skipped {
		t.Errorf("empty command must be skipped: %+v", r)
	}
	runs := []CheckRun{{Name: checkLinter, OK: true}, {Name: checkSpecTests, ExitCode: 1}, {Name: checkAllTests, ExitCode: 1}}
	if f := GateFor(afspec.TaskGroupKindTests, runs); len(f) != 0 {
		t.Errorf("only the linter gates a group, got %v", f)
	}
	if f := FinalGate(runs); len(f) != 2 {
		t.Errorf("the final gate judges everything, got %d", len(f))
	}
	if got := checkPrograms(afspec.TestCommands{Linter: "ruff check && go vet ./...", AllTests: "uv run pytest -q"}); strings.Join(got, ",") != "ruff,go,uv" {
		t.Errorf("checkPrograms = %v", got)
	}
}

func TestToolGuard(t *testing.T) {
	allow := func(context.Context, core.BeforeToolCallContext) core.BeforeToolCallDecision {
		return core.BeforeToolCallDecision{}
	}
	guard := toolGuard(allow, false, func(string) {})
	ro := toolGuard(allow, true, func(string) {})
	exec := func(cmd string) core.BeforeToolCallContext {
		return core.BeforeToolCallContext{ToolName: "execute", Arguments: map[string]any{"command": cmd}}
	}
	for _, cmd := range []string{"git commit -m x", "git -C . push origin main", "git checkout main", "git branch -D x"} {
		if d := guard(context.Background(), exec(cmd)); !d.Block {
			t.Errorf("%q must be blocked", cmd)
		}
	}
	for _, cmd := range []string{"git status", "git log --oneline -5", "git diff", "go test ./..."} {
		if d := guard(context.Background(), exec(cmd)); d.Block {
			t.Errorf("%q must be allowed: %s", cmd, d.Reason)
		}
	}
	if d := ro(context.Background(), core.BeforeToolCallContext{ToolName: "write_file"}); !d.Block {
		t.Error("write_file must be blocked in a read-only session")
	}
	if d := guard(context.Background(), core.BeforeToolCallContext{ToolName: "write_file"}); d.Block {
		t.Error("write_file must be allowed for the coder")
	}
}

// ------------------------------------------------------------- pipeline --

func TestPipelineEndToEnd(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)
	brain := scriptedBrain(t, root, happyTurns()...)
	initial := gitOut(t, root, "rev-parse", "HEAD")

	res, err := Run(context.Background(), baseOptions(t, root, pack, brain))
	if err != nil {
		t.Fatalf("Run failed at %s: %v", res.Stage, err)
	}
	if res.Stalled || res.Dirty || res.CostLimit {
		t.Fatalf("flags: stalled=%v dirty=%v cost=%v", res.Stalled, res.Dirty, res.CostLimit)
	}
	if len(res.Groups) != 4 {
		t.Fatalf("groups = %d", len(res.Groups))
	}
	for _, g := range res.Groups {
		if g.Status != StatusCompleted || g.Attempts != 1 {
			t.Errorf("group %d: %s after %d attempt(s): %s", g.ID, g.Status, g.Attempts, g.Error)
		}
	}
	if res.Groups[2].Archetype != Gate || !res.Groups[2].Gate.Passed {
		t.Errorf("group 3 should have run as a gate: %+v", res.Groups[2])
	}

	// main carries one squash commit per group that changed something, the
	// gate's landed nothing but the tasks.json update, and the group branches
	// are gone.
	if b := gitOut(t, root, "rev-parse", "--abbrev-ref", "HEAD"); b != "main" {
		t.Errorf("checked out %q, want main", b)
	}
	if got := gitOut(t, root, "rev-parse", "HEAD"); got == initial {
		t.Error("main did not move")
	}
	log := gitOut(t, root, "log", "--format=%s", initial+"..HEAD")
	for _, want := range []string{"test(counter): add failing spec tests", "feat(counter): implement Add and Total",
		"chore: mark task group 3 subtasks done", "chore(counter): wiring verification"} {
		if !strings.Contains(log, want) {
			t.Errorf("main log lacks %q:\n%s", want, log)
		}
	}
	if strings.Count(log, "\n")+1 != 4 {
		t.Errorf("want exactly 4 commits on main, got:\n%s", log)
	}
	if branches := gitOut(t, root, "branch", "--list", "feature/*"); branches != "" {
		t.Errorf("group branches should be deleted after merging: %s", branches)
	}
	if dirty := gitOut(t, root, "status", "--porcelain"); dirty != "" {
		t.Errorf("working tree is dirty after the run:\n%s", dirty)
	}
	for _, f := range []string{"counter_test.go", "counter.go", "docs/wiring.md"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("%s is missing on main", f)
		}
	}

	// tasks.json on main says every subtask is done, through the library.
	tasks := readTasks(t, root)
	for _, id := range []string{"1.1", "2.1", "2.2", "3.1", "4.1"} {
		if s := subtaskState(tasks, id); s != afspec.SubtaskStateDone {
			t.Errorf("subtask %s = %s, want done", id, s)
		}
	}

	// The ordering claims: branch before session, checks before commit, tasks
	// before land, and the final check after the last group.
	assertOrder(t, res, "preflight", "group-1/branch", "group-1/coder", "group-1/checks", "group-1/commit",
		"group-1/tasks", "group-1/land", "group-2/branch", "group-2/coder", "group-3/gate", "group-3/land",
		"group-4/coder", "final-check")
	if len(res.Final) != 3 || !res.FinalOK {
		t.Errorf("final checks: %+v", res.Final)
	}

	// Memory flows forward: group 2's system prompt carries group 1's
	// summary and gotcha, and shows group 1 as done.
	reqs := brain.provider.Requests()
	var group2 core.Request
	for _, r := range reqs {
		if strings.Contains(systemText(r), "### 2. Implement the counter") {
			group2 = r
			break
		}
	}
	sys := systemText(group2)
	for _, want := range []string{"## Memory Facts", "[CONTEXT] task group 1: Group 1 of widget_counter",
		"[GOTCHA] task group 1: the counter is not goroutine-safe", "- [x] 1. Write failing spec tests (1/1 subtasks done)",
		"## Project Instructions", "## Test Commands"} {
		if !strings.Contains(sys, want) {
			t.Errorf("group 2 system prompt lacks %q", want)
		}
	}
	if !strings.HasPrefix(sys, "## Session Rules") {
		t.Errorf("system prompt should start with the coder profile, got %q", firstLine(sys, 40))
	}
}

func TestPipelineRetriesAGateFailureWithTheErrorInThePrompt(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)
	pack.Spec.Tasks.TaskGroups = pack.Spec.Tasks.TaskGroups[:1]
	// The linter passes only once the coder has written ok.txt.
	pack.Spec.Tasks.TestCommands.Linter = "test -f ok.txt"
	brain := scriptedBrain(t, root,
		writeTurn("c1", "counter_test.go", "package widget\n"),
		submitGroup("c2", "test: first try", "first"),
		writeTurn("c3", "counter_test.go", "package widget\n"),
		writeTurn("c4", "ok.txt", "ok\n"),
		submitGroup("c5", "test: second try", "second"),
	)

	res, err := Run(context.Background(), baseOptions(t, root, pack, brain))
	if err != nil {
		t.Fatalf("Run failed at %s: %v", res.Stage, err)
	}
	g := res.Groups[0]
	if g.Status != StatusCompleted || g.Attempts != 2 {
		t.Fatalf("group 1: %s after %d attempts: %s", g.Status, g.Attempts, g.Error)
	}
	// The second attempt was told what went wrong.
	var retryPrompt string
	for _, r := range brain.provider.Requests() {
		for _, m := range r.Messages {
			if u, ok := m.(core.UserMessage); ok && strings.Contains(u.Content.Text(), "retry attempt 2") {
				retryPrompt = u.Content.Text()
			}
		}
	}
	if !strings.Contains(retryPrompt, "quality gate failed after task group 1") || !strings.Contains(retryPrompt, "test -f ok.txt") {
		t.Errorf("retry prompt does not carry the gate failure:\n%s", retryPrompt)
	}
	// The first attempt's branch is gone and only the second landed.
	if branches := gitOut(t, root, "branch", "--list"); strings.Contains(branches, "feature/") {
		t.Errorf("attempt branches left behind: %s", branches)
	}
	log := gitOut(t, root, "log", "--format=%s", "-3")
	if !strings.Contains(log, "test: second try") || strings.Contains(log, "test: first try") {
		t.Errorf("main log:\n%s", log)
	}
	assertOrder(t, res, "group-1/branch", "group-1/checks", "group-1/retry", "group-1/branch", "group-1/checks", "group-1/commit", "group-1/land")
}

func TestPipelineStallsWhenRetriesAreExhausted(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)
	pack.Spec.Tasks.TestCommands.Linter = "false"
	brain := scriptedBrain(t, root,
		writeTurn("c1", "a.go", "package a\n"), submitGroup("c2", "feat: a", "a"),
		writeTurn("c3", "a.go", "package a\n"), submitGroup("c4", "feat: a", "a"),
		writeTurn("c5", "a.go", "package a\n"), submitGroup("c6", "feat: a", "a"),
	)
	initial := gitOut(t, root, "rev-parse", "HEAD")

	res, err := Run(context.Background(), baseOptions(t, root, pack, brain))
	if err != nil {
		t.Fatalf("a stalled run is an outcome, not an error: %v", err)
	}
	if !res.Stalled {
		t.Fatal("Stalled not set")
	}
	if g := res.Groups[0]; g.Status != StatusBlocked || g.Attempts != 3 || g.Branch != "stalled/feature/widget_counter/1" {
		t.Errorf("group 1: %+v", g)
	}
	for _, g := range res.Groups[1:] {
		if g.Status != StatusBlocked || g.Attempts != 0 {
			t.Errorf("group %d should be blocked in cascade without running: %s (%d attempts)", g.ID, g.Status, g.Attempts)
		}
	}
	if len(res.Groups) != 4 {
		t.Errorf("all four groups must be reported, got %d", len(res.Groups))
	}
	if got := gitOut(t, root, "rev-parse", "HEAD"); got != initial {
		t.Error("main must not move when the first group is blocked")
	}
	if b := gitOut(t, root, "rev-parse", "--abbrev-ref", "HEAD"); b != "main" {
		t.Errorf("checked out %q, want main", b)
	}
	if !strings.Contains(gitOut(t, root, "branch", "--list", "stalled/*"), "stalled/feature/widget_counter/1") {
		t.Error("the last attempt must be kept under stalled/")
	}
	if dirty := gitOut(t, root, "status", "--porcelain"); dirty != "" {
		t.Errorf("working tree is dirty:\n%s", dirty)
	}
	if s := subtaskState(readTasks(t, root), "1.1"); s != afspec.SubtaskStatePending {
		t.Errorf("a blocked group's subtasks must stay pending, got %s", s)
	}
}

func TestPipelineSkipsACompletedGroup(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)
	if err := pack.MarkGroupDone(1); err != nil {
		t.Fatal(err)
	}
	gitOut(t, root, "add", "-A")
	gitOut(t, root, "commit", "-qm", "chore: mark task group 1 subtasks done")
	brain := scriptedBrain(t, root, happyTurns()[2:]...)

	res, err := Run(context.Background(), baseOptions(t, root, pack, brain))
	if err != nil {
		t.Fatalf("Run failed at %s: %v", res.Stage, err)
	}
	if g := res.Groups[0]; g.Status != StatusSkipped || g.Attempts != 0 {
		t.Errorf("group 1: %+v", g)
	}
	for _, g := range res.Groups[1:] {
		if g.Status != StatusCompleted {
			t.Errorf("group %d: %s: %s", g.ID, g.Status, g.Error)
		}
	}
	assertOrder(t, res, "preflight", "group-1/preflight", "group-2/branch")
	if brain.provider.Calls() != 5 {
		t.Errorf("expected 5 model calls (groups 2, 3 and 4), got %d", brain.provider.Calls())
	}
}

func TestPipelineLaunchesADoneGroupWhoseTestsFail(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)
	pack.Spec.Tasks.TaskGroups = pack.Spec.Tasks.TaskGroups[:1]
	if err := pack.MarkGroupDone(1); err != nil {
		t.Fatal(err)
	}
	gitOut(t, root, "add", "-A")
	gitOut(t, root, "commit", "-qm", "chore: mark task group 1 subtasks done")
	pack.Spec.Tasks.TestCommands.AllTests = "false"
	brain := scriptedBrain(t, root, writeTurn("c1", "fix.go", "package a\n"), submitGroup("c2", "fix: tests", "fixed"))

	res, err := Run(context.Background(), baseOptions(t, root, pack, brain))
	if err != nil {
		t.Fatalf("Run failed at %s: %v", res.Stage, err)
	}
	if g := res.Groups[0]; g.Status != StatusCompleted || g.Attempts != 1 {
		t.Errorf("group 1: %+v", g)
	}
	if !strings.Contains(lastUserText(t, brain.provider), "## Preflight State") {
		t.Error("the coder must be told the preflight state")
	}
	if !res.Dirty {
		t.Error("all_tests still fails, so the run must be reported dirty")
	}
}

func TestPipelineRefusesADirtyTree(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)
	write(t, filepath.Join(root, "scratch.txt"), "uncommitted\n")
	brain := scriptedBrain(t, root)

	res, err := Run(context.Background(), baseOptions(t, root, pack, brain))
	if err == nil || res.Stage != "preflight" || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("stage=%q err=%v", res.Stage, err)
	}
	if brain.provider.Calls() != 0 {
		t.Error("no model call may happen before preflight passes")
	}
}

func TestPipelineStopsAtTheCostCeiling(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)
	turns := happyTurns()
	turns[1].Usage = core.Usage{CostUSD: 0.75}
	brain := scriptedBrain(t, root, turns...)
	opts := baseOptions(t, root, pack, brain)
	opts.MaxCostUSD = 0.5

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run failed at %s: %v", res.Stage, err)
	}
	if !res.CostLimit {
		t.Fatal("CostLimit not set")
	}
	if res.Groups[0].Status != StatusCompleted {
		t.Errorf("group 1 should have completed before the ceiling was checked: %s", res.Groups[0].Status)
	}
	for _, g := range res.Groups[1:] {
		if g.Status != StatusCostBlock {
			t.Errorf("group %d: %s, want cost_blocked", g.ID, g.Status)
		}
	}
	if brain.provider.Calls() != 2 {
		t.Errorf("model calls = %d, want 2", brain.provider.Calls())
	}
}

func TestPipelineKeepsBranchesWhenAsked(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)
	brain := scriptedBrain(t, root, happyTurns()...)
	opts := baseOptions(t, root, pack, brain)
	opts.Landing = LandBranch
	initial := gitOut(t, root, "rev-parse", "HEAD")

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run failed at %s: %v", res.Stage, err)
	}
	if got := gitOut(t, root, "rev-parse", "main"); got != initial {
		t.Error("main must not move with --land=branch")
	}
	for i := 1; i <= 4; i++ {
		if !NewGit(root, execRunner).LocalBranchExists(context.Background(), GroupBranch("widget_counter", i)) {
			t.Errorf("branch for group %d is missing", i)
		}
	}
	// The chain builds on itself: group 2's branch contains group 1's file.
	if out := gitOut(t, root, "ls-tree", "--name-only", "feature/widget_counter/2"); !strings.Contains(out, "counter_test.go") {
		t.Errorf("group 2's branch does not contain group 1's work:\n%s", out)
	}
	if b := gitOut(t, root, "rev-parse", "--abbrev-ref", "HEAD"); b != "feature/widget_counter/4" {
		t.Errorf("checked out %q, want the last group's branch", b)
	}
	if res.Groups[3].Landed != "" {
		t.Error("nothing is landed in branch mode")
	}
}

func TestGateFailureIsRetriedAndVerifierIsInformational(t *testing.T) {
	root := newRepo(t)
	pack := loadPack(t, root)
	turns := happyTurns()
	// The gate fails once, then passes; the verifier says FAIL.
	turns = append(turns[:4], append([]faux.Turn{submitGate("g0", false)}, turns[4:]...)...)
	turns = append(turns, submitVerdicts("v1", "FAIL"))
	brain := scriptedBrain(t, root, turns...)
	opts := baseOptions(t, root, pack, brain)
	opts.RunVerifier = true

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run failed at %s: %v", res.Stage, err)
	}
	if g := res.Groups[2]; g.Status != StatusCompleted || g.Attempts != 2 || g.Archetype != Gate {
		t.Errorf("gate group: %+v", g)
	}
	if res.Verdicts == nil || res.Verdicts.OverallVerdict != "FAIL" {
		t.Fatalf("verdicts = %+v", res.Verdicts)
	}
	if res.Stalled || res.Dirty {
		t.Error("a FAIL verdict must not change the run's outcome")
	}
	// The verifier got a read-only session with the checklist.
	reqs := brain.provider.Requests()
	last := reqs[len(reqs)-1]
	if !strings.Contains(systemText(last), "## Verification Checklist") {
		t.Error("verifier system prompt lacks the checklist")
	}
	for _, tw := range last.Tools {
		if tw.Name == "write_file" || tw.Name == "edit_file" {
			t.Errorf("the verifier must not have %s", tw.Name)
		}
	}
}

func TestSummaryReportsWhatHappened(t *testing.T) {
	var b strings.Builder
	res := &Result{SpecName: "s", Stalled: true, Groups: []GroupOutcome{{ID: 1, Archetype: Coder, Status: StatusBlocked, Attempts: 3, Branch: "stalled/x"}}}
	Summary(&b, res, nil, false)
	if !strings.Contains(b.String(), "stalled") || !strings.Contains(b.String(), "stalled/x") {
		t.Errorf("summary:\n%s", b.String())
	}
	b.Reset()
	Summary(&b, &Result{SpecName: "s", Dirty: true, Final: []CheckRun{{Name: "all_tests", ExitCode: 1}}}, nil, false)
	if !strings.Contains(b.String(), "DIRTY") || !strings.Contains(b.String(), "fail (exit 1)") {
		t.Errorf("summary:\n%s", b.String())
	}
}
