package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	afspec "github.com/agent-fox-dev/spec"
)

// Landing is what happens to a task group's branch once its gates pass.
//
// agent-fox's default (`merge_strategy = "direct"`) squash-merges every group
// into the integration branch as soon as it completes, so `main` gains one
// commit per task group and the next group starts from it. `branch` keeps
// the group branches: each one is created from the previous group's tip, so
// the chain still builds on itself, and nothing touches the base branch.
type Landing string

const (
	LandMerge  Landing = "merge"
	LandBranch Landing = "branch"
)

func ParseLanding(s string) (Landing, error) {
	switch l := Landing(strings.TrimSpace(s)); l {
	case LandMerge, LandBranch:
		return l, nil
	default:
		return "", fmt.Errorf("unknown --land value %q (want merge or branch)", s)
	}
}

// Options is one run. Every collaborator is injected, so the end-to-end test
// constructs this with a scripted brain and a temporary repository and runs
// the real Run below.
type Options struct {
	Pack         *Pack
	Git          *Git
	Brain        Brain
	Run          Runner
	Landing      Landing
	Push         bool
	PushAttempts int
	MaxRetries   int           // agent-fox orchestrator.max_retries: 2 → three attempts
	MaxCostUSD   float64       // agent-fox orchestrator.max_cost; 0 means no ceiling
	CheckTimeout time.Duration // per test command
	RunVerifier  bool
	Out          io.Writer
	JournalPath  string
	Verbose      bool
}

type runner struct {
	o      Options
	res    *Result
	log    *os.File
	outMu  sync.Mutex
	memory []string // facts from completed groups, injected into later ones
}

// Run executes the pass and returns what happened. A returned error means the
// run did not complete; res is populated up to the failure and the caller
// decides the exit code. A stalled run (a group exhausted its retries) and a
// cost-limited run return a nil error with the flag set — those are outcomes
// agent-fox reports as statuses, not exceptions.
func Run(ctx context.Context, o Options) (*Result, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.PushAttempts < 1 {
		o.PushAttempts = 1
	}
	r := &runner{o: o, res: &Result{SpecDir: o.Pack.Dir, SpecName: o.Pack.Spec.SpecName}}
	if o.JournalPath != "" {
		f, err := os.OpenFile(o.JournalPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return r.res, fmt.Errorf("opening the journal %s: %w", o.JournalPath, err)
		}
		defer f.Close()
		r.log = f
	}
	return r.res, r.run(ctx)
}

func (r *runner) run(ctx context.Context) error {
	o := r.o
	pack := o.Pack

	// 1. Pre-flight. Everything that can refuse the run happens before a
	//    branch exists or a model is called.
	if err := r.step(ctx, "preflight", func() (string, error) {
		if !o.Git.IsRepo(ctx) {
			return "", fmt.Errorf("%s is not a git repository", pack.Root)
		}
		if rel, err := filepath.Rel(pack.Root, pack.Dir); err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("spec %s is not inside the repository %s", pack.Dir, pack.Root)
		}
		dirty, err := o.Git.DirtyFiles(ctx)
		if err != nil {
			return "", err
		}
		if len(dirty) > 0 {
			return "", fmt.Errorf("the working tree has %d uncommitted change(s):\n  %s\n"+
				"commit or stash them first — every change on a group branch must be attributable to its session",
				len(dirty), strings.Join(dirty, "\n  "))
		}
		base, err := o.Git.CurrentBranch(ctx)
		if err != nil {
			return "", err
		}
		if base == "HEAD" {
			return "", errors.New("HEAD is detached; check out the branch the work should land on")
		}
		r.res.Base = base
		groups := pack.Groups()
		done := 0
		for _, g := range groups {
			if GroupComplete(g) {
				done++
			}
		}
		return fmt.Sprintf("%s (%s) on %s: %d task group(s), %d already complete",
			pack.Spec.SpecName, pack.Spec.Status, base, len(groups), done), nil
	}); err != nil {
		return err
	}

	// 2. The chain. `head` is where the next group branches from: the base
	//    branch when groups are merged as they land, the previous group's
	//    branch when they are kept.
	head := r.res.Base
	checks := Checks(pack.Spec.Tasks.TestCommands)
	groups := pack.Groups()
	for i, g := range groups {
		outcome := &GroupOutcome{ID: g.Id, Kind: string(g.Kind), Title: g.Title, Archetype: ArchetypeFor(g.Kind), Status: StatusPending}
		r.res.Groups = append(r.res.Groups, *outcome)
		slot := &r.res.Groups[len(r.res.Groups)-1]

		if o.MaxCostUSD > 0 && r.res.CostUSD() >= o.MaxCostUSD {
			// agent-fox's circuit breaker: checked before every dispatch, so
			// a run stops between groups, never in the middle of one.
			r.res.CostLimit = true
			for _, rest := range groups[i:] {
				r.markRest(rest.Id, StatusCostBlock)
			}
			r.record(JournalEntry{Time: time.Now(), Step: "cost-limit", Status: "fail",
				Detail: fmt.Sprintf("$%.4f spent, ceiling $%.2f", r.res.CostUSD(), o.MaxCostUSD)})
			r.printf("\n[flatline] cost ceiling reached ($%.4f ≥ $%.2f); stopping before task group %d\n",
				r.res.CostUSD(), o.MaxCostUSD, g.Id)
			return nil
		}

		next, err := r.runGroup(ctx, g, slot, head, checks)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if slot.Status == StatusBlocked {
			// The chain stops here: in agent-fox the dependents of a blocked
			// node are blocked in cascade, and on a path that is everything
			// after it.
			r.res.Stalled = true
			for _, rest := range groups[i+1:] {
				r.markRest(rest.Id, StatusBlocked)
			}
			return nil
		}
		head = next
	}

	// 3. The final check — agent-fox's post-merge `make check`, here the
	//    pack's own three commands. It decides completed vs completed-dirty.
	if err := r.step(ctx, "final-check", func() (string, error) {
		r.res.Final = RunChecks(ctx, o.Run, pack.Root, checks, o.CheckTimeout)
		failed := FinalGate(r.res.Final)
		r.res.FinalOK = len(failed) == 0
		r.res.Dirty = !r.res.FinalOK
		if r.res.Dirty {
			r.warn("final checks fail; the run is reported as completed DIRTY:\n" + indent(checkFailureText(failed)))
		}
		return checkStatusLine(r.res.Final), nil
	}); err != nil {
		return err
	}

	// 4. The verifier. Informational in agent-fox and informational here: it
	//    runs after everything landed, and its verdicts are printed and
	//    journaled, never acted on.
	if o.RunVerifier {
		if o.MaxCostUSD > 0 && r.res.CostUSD() >= o.MaxCostUSD {
			r.warn("verifier skipped: the cost ceiling is reached")
			return nil
		}
		last := groups[len(groups)-1]
		if err := r.step(ctx, "verifier", func() (string, error) {
			v, stats, err := o.Brain.Verify(ctx, SessionInput{
				Group:   last,
				Context: pack.AssembleContext(Verifier, last.Id, r.memory),
				Task:    VerifierTaskPrompt(pack.Spec.SpecName),
			})
			r.res.Stats = append(r.res.Stats, stats)
			if err != nil {
				r.warn("verifier did not finish: " + err.Error())
				return "no verdict", nil
			}
			r.res.Verdicts = &v
			fails := 0
			for _, d := range v.Verdicts {
				if d.Verdict != "PASS" {
					fails++
				}
			}
			return fmt.Sprintf("%s (%d verdict(s), %d FAIL) — informational", v.OverallVerdict, len(v.Verdicts), fails), nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// runGroup drives one task group to completed, skipped or blocked, and
// returns the ref the next group should branch from.
func (r *runner) runGroup(ctx context.Context, g afspec.TaskGroup, out *GroupOutcome, head string, checks []Check) (string, error) {
	o := r.o
	pack := o.Pack
	prefix := fmt.Sprintf("group-%d", g.Id)

	// Preflight, agent-fox's per-coder-group gate: a group whose subtasks are
	// all done AND whose test baseline passes is skipped without a session;
	// one whose subtasks are done but whose tests fail is launched with a
	// note saying so.
	var preflight string
	allDone := GroupComplete(g)
	if allDone {
		var baseline CheckRun
		if err := r.step(ctx, prefix+"/preflight", func() (string, error) {
			baseline = RunCheck(ctx, o.Run, pack.Root, checkNamed(checks, checkAllTests), o.CheckTimeout)
			if baseline.OK || baseline.Skipped {
				out.Status = StatusSkipped
				return "every subtask is done and " + baseline.Status() + "; skipped", nil
			}
			preflight = PreflightSummary(true, &baseline)
			return "every subtask is done but `" + baseline.Command + "` " + baseline.Status() + "; launching", nil
		}); err != nil {
			return "", err
		}
		if out.Status == StatusSkipped {
			return head, nil
		}
	}

	var previousError string
	for attempt := 1; attempt <= o.MaxRetries+1; attempt++ {
		out.Attempts = attempt
		out.Status = StatusInProgress
		out.Checks = nil
		out.Error = ""

		// A fresh branch per attempt from the same starting point: the
		// equivalent of agent-fox destroying a failed attempt's worktree.
		var branch string
		if err := r.step(ctx, prefix+"/branch", func() (string, error) {
			branch = UniqueBranchName(ctx, o.Git, GroupBranch(pack.Spec.SpecName, g.Id))
			if err := o.Git.CreateBranch(ctx, branch, head); err != nil {
				return "", err
			}
			out.Branch = branch
			return fmt.Sprintf("%s from %s (attempt %d/%d)", branch, head, attempt, o.MaxRetries+1), nil
		}); err != nil {
			return "", err
		}

		var failure error
		switch out.Archetype {
		case Gate:
			failure = r.gateAttempt(ctx, g, out, prefix, attempt, previousError)
		default:
			failure = r.coderAttempt(ctx, g, out, prefix, checks, attempt, previousError, preflight)
		}
		if failure == nil {
			break
		}
		if ctx.Err() != nil {
			out.Status = StatusBlocked
			out.Error = failure.Error()
			return "", failure
		}
		out.Error = failure.Error()
		previousError = failure.Error()

		// Undo the attempt: back to the starting point, branch gone. On the
		// last attempt the branch is kept under stalled/ instead, which is
		// agent-fox's `rename_stalled` disposition — the work is evidence.
		if attempt <= o.MaxRetries {
			if err := r.step(ctx, prefix+"/retry", func() (string, error) {
				if err := o.Git.ResetHardClean(ctx, "HEAD"); err != nil {
					return "", err
				}
				if err := o.Git.Checkout(ctx, head); err != nil {
					return "", err
				}
				if err := o.Git.DeleteBranch(ctx, branch); err != nil {
					return "", err
				}
				return fmt.Sprintf("attempt %d failed; retrying with the error in the prompt", attempt), nil
			}); err != nil {
				return "", err
			}
			continue
		}
		out.Status = StatusBlocked
		if err := r.step(ctx, prefix+"/block", func() (string, error) {
			if err := o.Git.ResetHardClean(ctx, "HEAD"); err != nil {
				return "", err
			}
			if err := o.Git.Checkout(ctx, head); err != nil {
				return "", err
			}
			stalled := "stalled/" + branch
			if _, err := o.Git.mustGit(ctx, "branch", "-m", branch, stalled); err != nil {
				return "", err
			}
			out.Branch = stalled
			return fmt.Sprintf("%d attempt(s) failed; the last one is kept on %s", attempt, stalled), fmt.Errorf(
				"task group %d blocked after %d attempt(s): %s", g.Id, attempt, firstLine(failure.Error(), 200))
		}); err != nil {
			// step recorded the failure; the caller reads out.Status.
			_ = err
		}
		return head, nil
	}

	// Mark the group's subtasks done and commit that alone, as agent-fox does
	// (`chore: mark task group N subtasks done`, skipped when nothing changed).
	if err := r.step(ctx, prefix+"/tasks", func() (string, error) {
		if err := pack.MarkGroupDone(g.Id); err != nil {
			return "", err
		}
		rel, err := pack.TasksRelPath()
		if err != nil {
			return "", err
		}
		sha, err := o.Git.CommitPaths(ctx, TasksDoneMessage(g.Id), rel)
		if err != nil {
			return "", err
		}
		if sha == "" {
			return "subtasks already done; nothing to commit", nil
		}
		return sha + " " + TasksDoneMessage(g.Id), nil
	}); err != nil {
		return "", err
	}

	// Land it.
	next := out.Branch
	if err := r.step(ctx, prefix+"/land", func() (string, error) {
		if o.Landing == LandBranch {
			return "kept on " + out.Branch + " (--land=branch)", nil
		}
		last, err := o.Git.LastMessage(ctx, out.Branch)
		if err != nil {
			return "", err
		}
		if isHousekeeping(strings.SplitN(strings.TrimSpace(last), "\n", 2)[0]) && out.Commit != "" {
			last, err = o.Git.LastMessage(ctx, out.Commit)
			if err != nil {
				return "", err
			}
		}
		subjects, err := o.Git.Subjects(ctx, r.res.Base, out.Branch)
		if err != nil {
			return "", err
		}
		sha, err := o.Git.SquashMergeInto(ctx, r.res.Base, out.Branch, SquashMessage(last, subjects))
		if err != nil {
			return "", err
		}
		if err := o.Git.DeleteBranch(ctx, out.Branch); err != nil {
			return "", err
		}
		next = r.res.Base
		if sha == "" {
			return "nothing to land on " + r.res.Base, nil
		}
		out.Landed = sha
		if o.Push {
			if err := o.Git.Push(ctx, r.res.Base, o.PushAttempts, r.warn); err != nil {
				r.warn("could not push " + r.res.Base + ": " + err.Error())
			}
		}
		return fmt.Sprintf("squash-merged into %s as %s", r.res.Base, sha), nil
	}); err != nil {
		return "", err
	}
	out.Status = StatusCompleted
	return next, nil
}

// coderAttempt is one coder session plus its gates. It returns nil when the
// attempt succeeded and the group's commit (if any) is on the branch.
func (r *runner) coderAttempt(ctx context.Context, g afspec.TaskGroup, out *GroupOutcome, prefix string, checks []Check, attempt int, previousError, preflight string) error {
	o := r.o
	pack := o.Pack

	var report GroupReport
	if err := r.step(ctx, prefix+"/coder", func() (string, error) {
		var stats RunStats
		var err error
		report, stats, err = o.Brain.Implement(ctx, SessionInput{
			Group:   g,
			Context: pack.AssembleContext(Coder, g.Id, r.memory),
			Task:    CoderTaskPrompt(g.Id, pack.Spec.SpecName, g.Kind, checks, attempt, previousError, preflight),
		})
		r.res.Stats = append(r.res.Stats, stats)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d file(s) reported changed: %s", len(report.Changes), firstLine(report.Summary, 80)), nil
	}); err != nil {
		return err
	}

	var changed []string
	if err := r.step(ctx, prefix+"/checks", func() (string, error) {
		var err error
		changed, err = o.Git.ChangedFiles(ctx, "HEAD")
		if err != nil {
			return "", err
		}
		out.Changed = changed
		if len(changed) == 0 {
			r.warn(fmt.Sprintf("task group %d: the coder reported a result but changed no files", g.Id))
		}
		out.Checks = RunChecks(ctx, o.Run, pack.Root, checks, o.CheckTimeout)
		if failed := GateFor(g.Kind, out.Checks); len(failed) > 0 {
			return "", fmt.Errorf("quality gate failed after task group %d:\n%s", g.Id, checkFailureText(failed))
		}
		return fmt.Sprintf("%d file(s) changed; %s", len(changed), checkStatusLine(out.Checks)), nil
	}); err != nil {
		return err
	}

	if err := r.step(ctx, prefix+"/commit", func() (string, error) {
		if len(changed) == 0 {
			return "nothing to commit", nil
		}
		msg := GroupCommitMessage(pack.Spec.SpecName, g.Id, g.Title, report)
		sha, err := o.Git.CommitAll(ctx, msg)
		if err != nil {
			return "", err
		}
		out.Commit = sha
		return sha + " " + firstLine(msg, 72), nil
	}); err != nil {
		return err
	}
	out.Report = report
	r.memory = append(r.memory, report.MemoryFacts(g.Id)...)
	return nil
}

// gateAttempt is one checkpoint session. A gate that reports a failed check
// fails the attempt; one that changed the tree is reset, because it had no
// business doing so.
func (r *runner) gateAttempt(ctx context.Context, g afspec.TaskGroup, out *GroupOutcome, prefix string, attempt int, previousError string) error {
	o := r.o
	pack := o.Pack
	return r.step(ctx, prefix+"/gate", func() (string, error) {
		gr, stats, err := o.Brain.Gate(ctx, SessionInput{
			Group:   g,
			Context: pack.AssembleContext(Gate, g.Id, r.memory),
			Task:    GateTaskPrompt(pack.Spec.SpecName, g.Id, attempt, previousError),
		})
		r.res.Stats = append(r.res.Stats, stats)
		if err != nil {
			return "", err
		}
		out.Gate = gr
		if changed, _ := o.Git.ChangedFiles(ctx, "HEAD"); len(changed) > 0 {
			r.warn(fmt.Sprintf("gate for task group %d left %d changed file(s); reset", g.Id, len(changed)))
			if err := o.Git.ResetHardClean(ctx, "HEAD"); err != nil {
				return "", err
			}
		}
		if !gr.Passed {
			var b strings.Builder
			fmt.Fprintf(&b, "checkpoint %d failed:", g.Id)
			for _, c := range gr.Results {
				if !c.Passed {
					fmt.Fprintf(&b, "\n- %s\n%s", c.Check, indent(c.Output))
				}
			}
			return "", errors.New(b.String())
		}
		return fmt.Sprintf("%d check(s) passed", len(gr.Results)), nil
	})
}

// ------------------------------------------------------------- plumbing --

func checkNamed(checks []Check, name string) Check {
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	return Check{Name: name}
}

func checkStatusLine(runs []CheckRun) string {
	parts := make([]string, 0, len(runs))
	for _, c := range runs {
		parts = append(parts, c.Name+" "+c.Status())
	}
	return strings.Join(parts, ", ")
}

func (r *runner) markRest(id int, status GroupStatus) {
	for i := range r.res.Groups {
		if r.res.Groups[i].ID == id {
			if r.res.Groups[i].Status == StatusPending || r.res.Groups[i].Status == StatusInProgress {
				r.res.Groups[i].Status = status
			}
			return
		}
	}
	g, _ := r.o.Pack.Group(id)
	r.res.Groups = append(r.res.Groups, GroupOutcome{ID: id, Kind: string(g.Kind), Title: g.Title,
		Archetype: ArchetypeFor(g.Kind), Status: status})
}

// step runs one pipeline step, prints it, and journals the outcome.
func (r *runner) step(ctx context.Context, name string, fn func() (string, error)) error {
	start := time.Now()
	prevStats := len(r.res.Stats)
	r.printf("\n[flatline] %s ", name)

	detail, err := fn()
	elapsed := time.Since(start)

	entry := JournalEntry{Time: start, Step: name, Status: "ok", Detail: detail, Elapsed: elapsed.Milliseconds()}
	if err != nil {
		entry.Status, entry.Detail = "fail", err.Error()
		r.res.Stage = name
	}
	r.record(entry)

	var in, outTok int64
	for i := prevStats; i < len(r.res.Stats); i++ {
		in += r.res.Stats[i].Usage.InputTokens
		outTok += r.res.Stats[i].Usage.OutputTokens
	}
	timing := "(" + formatDuration(elapsed) + ")"
	if in > 0 || outTok > 0 {
		timing = fmt.Sprintf("(%s · %d↑ %d↓)", formatDuration(elapsed), in, outTok)
	}
	if err != nil {
		r.printf("%s\n  ✗ %s\n", timing, indent(err.Error()))
	} else if detail != "" {
		r.printf("%s\n  ✓ %s\n", timing, indent(detail))
	} else {
		r.printf("%s\n", timing)
	}
	return err
}

func (r *runner) warn(msg string) {
	r.res.Warnings = append(r.res.Warnings, msg)
	r.printf("  ! %s\n", indent(msg))
	r.record(JournalEntry{Time: time.Now(), Step: "warning", Status: "warn", Detail: msg})
}

func (r *runner) record(e JournalEntry) {
	r.res.Journal = append(r.res.Journal, e)
	if r.log == nil {
		return
	}
	if b, err := json.Marshal(e); err == nil {
		fmt.Fprintln(r.log, string(b))
	}
}

func (r *runner) printf(format string, args ...any) {
	r.outMu.Lock()
	defer r.outMu.Unlock()
	fmt.Fprintf(r.o.Out, format, args...)
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}
