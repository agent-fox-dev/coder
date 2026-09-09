package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Landing is what happens to the commit once the change is verified.
//
// af-fix does BOTH of the first two — it opens a pull request that says
// "Closes #N", then squash-merges the branch into main locally and pushes
// main. Those are alternatives, not steps: the squash commit has a different
// hash from the branch, so GitHub cannot see it as the pull request's merge,
// and what is left behind is an open pull request whose changes are already on
// the default branch. Choosing one is the fix.
type Landing string

const (
	LandPR     Landing = "pr"     // commit, push the branch, open a pull request
	LandBranch Landing = "branch" // commit and push the branch, no pull request
	LandMerge  Landing = "merge"  // commit and squash-merge into the base branch locally
	LandNone   Landing = "none"   // commit on the branch and stop
)

func ParseLanding(s string) (Landing, error) {
	switch l := Landing(strings.TrimSpace(s)); l {
	case LandPR, LandBranch, LandMerge, LandNone:
		return l, nil
	default:
		return "", fmt.Errorf("unknown --land value %q (want pr, branch, merge or none)", s)
	}
}

// Options is one run of the pipeline. Every collaborator is injected, so the
// end-to-end test constructs this struct with a scripted brain, a recording
// hub and a temporary git repository, and runs the real Run below.
type Options struct {
	Ref           IssueRef
	Dir           string
	Hub           Hub
	Git           *Git
	Brain         Brain
	Run           Runner
	VerifyCommand string
	VerifyTimeout time.Duration
	Landing       Landing
	DryRun        bool
	SkipBaseline  bool
	PushAttempts  int
	Out           io.Writer
	JournalPath   string
	Verbose       bool
}

// Result is what the run produced, whether or not it finished.
type Result struct {
	Ref IssueRef
	// BaseBranch is where the fix lands and what a pull request targets. It
	// is captured in pre-flight, before the feature branch exists.
	BaseBranch string
	Branch     string
	Commit     string
	// WIPCommit is set when the checks failed after the change: the work is
	// committed on Branch under a `wip:` subject so the checkout can return to
	// BaseBranch clean, and nothing is landed.
	WIPCommit      string
	PRURL          string
	Analysis       Analysis
	Implementation Implementation
	Baseline       VerifyResult
	Verification   VerifyResult
	Changed        []string
	Stats          []RunStats
	Warnings       []string
	Journal        []JournalEntry
	// Stage names the step that failed, and is empty on success.
	Stage string
	// NeedsClarification is the "stopped on purpose" outcome: no code was
	// written, and the issue carries a question.
	NeedsClarification bool
}

func (r *Result) CostUSD() float64 {
	var sum float64
	for _, s := range r.Stats {
		sum += s.CostUSD
	}
	return sum
}

// JournalEntry is one line of the run log. The log exists because an
// autonomous run is something you read AFTER it happened, usually because
// something is wrong, and a terminal scrollback is not evidence.
type JournalEntry struct {
	Time    time.Time `json:"time"`
	Step    string    `json:"step"`
	Status  string    `json:"status"` // ok | warn | fail | skip
	Detail  string    `json:"detail,omitempty"`
	Elapsed int64     `json:"elapsed_ms"`
}

type runner struct {
	o     Options
	res   *Result
	log   *os.File
	outMu sync.Mutex
}

// Run executes the pipeline and returns what happened. A returned error means
// the run did not complete; res is still populated up to the failure, and the
// caller decides the exit code.
//
// The ORDER here is the part worth reading, and it differs from the af-fix
// skill in three places, each marked below.
func Run(ctx context.Context, o Options) (*Result, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	r := &runner{o: o, res: &Result{Ref: o.Ref}}
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

	// 1. Pre-flight. Everything that can refuse the run happens here, BEFORE
	//    the issue is fetched and long before anything is posted.
	//
	//    af-fix checks the working tree in its step 6, after it has already
	//    posted an analysis comment to the issue — so the common failure
	//    ("you have uncommitted changes") leaves a public comment describing
	//    work that never started.
	if err := r.step(ctx, "preflight", func() (string, error) {
		if !o.Git.IsRepo(ctx) {
			return "", fmt.Errorf("%s is not a git repository", o.Dir)
		}
		dirty, err := o.Git.DirtyFiles(ctx)
		if err != nil {
			return "", err
		}
		if len(dirty) > 0 {
			return "", fmt.Errorf("the working tree has %d uncommitted change(s):\n  %s\n"+
				"commit or stash them first — this run needs to be able to attribute every "+
				"change on the branch to itself", len(dirty), strings.Join(dirty, "\n  "))
		}
		if err := o.Hub.Check(ctx); err != nil {
			return "", err
		}
		head, err := o.Git.Head(ctx)
		if err != nil {
			return "", err
		}
		// The base branch is decided HERE, while the branch the operator
		// checked out is still the one checked out. Asked later, after the
		// feature branch exists, git would answer with the feature branch on
		// every repository whose origin does not advertise a default.
		r.res.BaseBranch = o.Git.BaseBranch(ctx)
		return "clean tree at " + head + " on " + r.res.BaseBranch, nil
	}); err != nil {
		return err
	}

	// 2. Fetch the issue and any pull request it references.
	var issue *Issue
	var linked []*LinkedPR
	if err := r.step(ctx, "fetch-issue", func() (string, error) {
		var err error
		issue, err = o.Hub.Issue(ctx, o.Ref)
		if err != nil {
			return "", err
		}
		for _, n := range issue.LinkedPRNumbers(o.Ref) {
			pr, err := o.Hub.PullRequest(ctx, o.Ref, n)
			if err != nil {
				// A linked pull request is context, not a prerequisite.
				r.warn(fmt.Sprintf("linked PR #%d could not be fetched: %v", n, err))
				continue
			}
			linked = append(linked, pr)
		}
		return fmt.Sprintf("%q by @%s, %d comment(s), %d linked PR(s)",
			issue.Title, issue.Author.Login, len(issue.Comments), len(linked)), nil
	}); err != nil {
		return err
	}

	// 3. Baseline. A suite that is already red changes the meaning of every
	//    later result, so it is measured rather than assumed.
	if err := r.step(ctx, "baseline", func() (string, error) {
		if o.SkipBaseline || o.VerifyCommand == "" {
			r.res.Baseline = VerifyResult{Command: o.VerifyCommand, Skipped: true}
			return "skipped", nil
		}
		r.res.Baseline = Verify(ctx, o.Run, o.Dir, o.VerifyCommand, o.VerifyTimeout)
		if !r.res.Baseline.OK {
			r.warn(fmt.Sprintf("%s was already failing before this run", o.VerifyCommand))
		}
		return fmt.Sprintf("`%s` %s in %s", o.VerifyCommand, r.res.Baseline.Status(),
			r.res.Baseline.Elapsed.Round(time.Millisecond)), nil
	}); err != nil {
		return err
	}

	// 4. Analysis: read-only, and the only phase allowed to stop the run by
	//    asking a question.
	if err := r.step(ctx, "analyze", func() (string, error) {
		a, stats, err := o.Brain.Analyze(ctx, AnalysisInput{
			Ref: o.Ref, Issue: issue, LinkedPRs: linked,
			Baseline: r.res.Baseline, VerifyCommand: o.VerifyCommand,
		})
		r.res.Stats = append(r.res.Stats, stats)
		if err != nil {
			return "", err
		}
		r.res.Analysis = a
		return fmt.Sprintf("%s (%s): %s", a.Classification, a.Confidence, a.Summary), nil
	}); err != nil {
		return err
	}

	if c := r.res.Analysis.Clarification; c != nil && strings.TrimSpace(c.Question) != "" {
		r.res.NeedsClarification = true
		return r.step(ctx, "clarify", func() (string, error) {
			url, err := o.Hub.Comment(ctx, o.Ref, ClarificationComment(*c))
			if err != nil {
				return "", err
			}
			return "asked on the issue: " + url, nil
		})
	}

	// 5. Branch. Created before the first comment and after the analysis, so
	//    its name can carry the classification and so nothing is posted until
	//    there is somewhere for the work to go.
	if err := r.step(ctx, "branch", func() (string, error) {
		// The branch is created even in a dry run. The implementation phase
		// writes to the working tree with real file tools — that is not
		// something --dry-run can suppress without running a different
		// program — so the honest choice is to contain those edits on a
		// branch you can delete, rather than to leave them loose on whatever
		// was checked out.
		name := UniqueBranchName(ctx, o.Git, BranchName(r.res.Analysis.Classification, o.Ref.Number, issue.Title))
		if err := o.Git.CreateBranch(ctx, name); err != nil {
			return "", err
		}
		r.res.Branch = name
		return name, nil
	}); err != nil {
		return err
	}

	// 6. The first external side effect of the whole run.
	if err := r.step(ctx, "post-analysis", func() (string, error) {
		url, err := o.Hub.Comment(ctx, o.Ref, AnalysisComment(r.res.Analysis, r.res.Branch, r.res.Baseline))
		if err != nil {
			// Non-fatal by design: the analysis is on stdout and in the
			// journal either way, and failing here would throw away a run
			// that has not gone wrong.
			r.warn("could not post the analysis comment: " + err.Error())
			return "not posted", nil
		}
		return url, nil
	}); err != nil {
		return err
	}

	// 7. Implementation.
	if err := r.step(ctx, "implement", func() (string, error) {
		imp, stats, err := o.Brain.Implement(ctx, ImplementInput{
			Ref: o.Ref, Issue: issue, Analysis: r.res.Analysis,
			Baseline: r.res.Baseline, VerifyCommand: o.VerifyCommand, Branch: r.res.Branch,
			Instructions: projectInstructions(o.Dir),
		})
		r.res.Stats = append(r.res.Stats, stats)
		if err != nil {
			return "", err
		}
		r.res.Implementation = imp
		return fmt.Sprintf("%d file(s) reported changed", len(imp.Changes)), nil
	}); err != nil {
		return r.fail(ctx, "implement", err)
	}

	// 8. The diff. A run that changed nothing has not fixed anything, however
	//    confident the summary sounds. It is its own stage so that "nothing
	//    was written" and "something was written and the checks fail" reach
	//    the caller as different exit codes.
	if err := r.step(ctx, "diff", func() (string, error) {
		changed, err := o.Git.ChangedFiles(ctx, "HEAD")
		if err != nil {
			return "", err
		}
		r.res.Changed = changed
		if len(changed) == 0 {
			return "", fmt.Errorf("the implementation phase reported a fix but the working tree is "+
				"identical to %s — nothing was changed", mustHead(ctx, o.Git))
		}
		return fmt.Sprintf("%d file(s) changed", len(changed)), nil
	}); err != nil {
		return r.fail(ctx, "diff", err)
	}

	// 9. Verification, by this program rather than by the model's report of
	//    it. A failure parks the work as a WIP commit on the branch and
	//    returns the checkout to the base branch, so the next run's pre-flight
	//    finds a clean tree rather than this run's leftovers.
	if err := r.step(ctx, "verify", func() (string, error) {
		changed := r.res.Changed
		r.res.Verification = Verify(ctx, o.Run, o.Dir, o.VerifyCommand, o.VerifyTimeout)
		switch {
		case r.res.Verification.Skipped:
			r.warn("no verification command: this change is UNVERIFIED")
		case !r.res.Verification.OK && !r.res.Baseline.OK && !r.res.Baseline.Skipped:
			// Red before, red after. Not necessarily this run's fault, but
			// not something to land on a green-looking summary either.
			return "", fmt.Errorf("`%s` still fails (exit %d); the suite was already red before "+
				"this run, so the branch is left for a human:\n%s",
				r.res.Verification.Command, r.res.Verification.ExitCode, r.res.Verification.Output)
		case !r.res.Verification.OK:
			return "", fmt.Errorf("`%s` fails after the change (exit %d):\n%s",
				r.res.Verification.Command, r.res.Verification.ExitCode, r.res.Verification.Output)
		}
		return fmt.Sprintf("%d file(s) changed, `%s` %s", len(changed),
			r.res.Verification.Command, r.res.Verification.Status()), nil
	}); err != nil {
		r.park(ctx)
		return r.fail(ctx, "verify", err)
	}

	// 10. Land it.
	if err := r.landing(ctx); err != nil {
		return r.fail(ctx, "land", err)
	}

	// 11. Report. Posted last, so the comment can name the pull request that
	//     now exists — af-fix posts its summary before creating the PR, and
	//     then has no link to include.
	return r.step(ctx, "post-summary", func() (string, error) {
		body := SummaryComment(r.res.Implementation, r.res.Analysis.Classification,
			r.res.Branch, r.res.PRURL, r.res.Baseline, r.res.Verification, r.res.Changed)
		url, err := o.Hub.Comment(ctx, o.Ref, body)
		if err != nil {
			r.warn("could not post the summary comment: " + err.Error())
			return "not posted", nil
		}
		return url, nil
	})
}

func (r *runner) landing(ctx context.Context) error {
	o := r.o
	msg := CommitMessage(r.res.Analysis.Classification, r.res.Implementation.CommitSubject,
		r.res.Analysis.Summary, o.Ref, r.res.Implementation.Summary)
	subject := firstLine(msg, 72)

	if err := r.step(ctx, "commit", func() (string, error) {
		sha, err := o.Git.CommitAll(ctx, msg)
		if err != nil {
			return "", err
		}
		r.res.Commit = sha
		return sha + " " + subject, nil
	}); err != nil {
		return err
	}

	switch {
	case o.Landing == LandNone:
		return nil

	case o.Landing == LandMerge:
		// A squash merge onto the base branch is local, but it is not
		// contained the way a branch is: undoing it takes a reset. So a dry
		// run stops at the commit here rather than landing it quietly.
		if o.DryRun {
			r.outMu.Lock()
			fmt.Fprintf(o.Out, "  ~ dry run: would squash-merge %s into %s\n",
				r.res.Branch, r.res.BaseBranch)
			r.outMu.Unlock()
			return nil
		}
		return r.step(ctx, "merge", func() (string, error) {
			base := r.res.BaseBranch
			if err := o.Git.SquashMergeInto(ctx, base, r.res.Branch, msg); err != nil {
				return "", err
			}
			return "squash-merged into " + base +
				" (not pushed: pushing a default branch is the operator's call)", nil
		})
	}

	if err := r.step(ctx, "push", func() (string, error) {
		if o.DryRun {
			return "dry run: would push " + r.res.Branch, nil
		}
		if err := o.Git.Push(ctx, r.res.Branch, o.PushAttempts, r.warn); err != nil {
			return "", err
		}
		return "pushed " + r.res.Branch, nil
	}); err != nil {
		return err
	}

	if o.Landing != LandPR {
		return nil
	}
	return r.step(ctx, "pull-request", func() (string, error) {
		url, err := o.Hub.CreatePR(ctx, o.Ref, PullRequestSpec{
			Base: r.res.BaseBranch, Head: r.res.Branch, Title: subject,
			Body: PullRequestBody(r.res.Implementation, o.Ref, r.res.Baseline, r.res.Verification, r.res.Changed),
		})
		if err != nil {
			// The branch is pushed and the change is verified; a missing pull
			// request is recoverable by hand and does not justify failing the
			// run.
			r.warn("could not open a pull request: " + err.Error())
			return "not created", nil
		}
		r.res.PRURL = url
		return url, nil
	})
}

// fail records the stage, tells the issue what happened, and returns the
// original error. Reporting the failure never replaces it.
func (r *runner) fail(ctx context.Context, stage string, cause error) error {
	r.res.Stage = stage
	body := FailureComment(stage, cause, r.res.Branch, r.res.Commit, r.res.WIPCommit, r.res.Verification)
	if _, err := r.o.Hub.Comment(ctx, r.o.Ref, body); err != nil {
		r.warn("could not post the failure comment: " + err.Error())
	}
	return cause
}

// park keeps an unverified change without landing it: a WIP commit on the
// feature branch, then the base branch checked out again.
//
// Leaving the edits loose in the working tree would fail the NEXT run's
// pre-flight ("uncommitted changes") on a tree this program dirtied itself,
// and leaving the feature branch checked out would make the operator's next
// `git pull` land on it. Neither failure is worth reporting on the issue, so
// this never returns an error; what it could not do is warned about.
func (r *runner) park(ctx context.Context) {
	o := r.o
	_ = r.step(ctx, "park", func() (string, error) {
		msg := fmt.Sprintf("wip: unverified fix for #%d\n\n`%s` did not pass after this change; "+
			"see the issue for the output. Not for landing as is.\n", o.Ref.Number, r.res.Verification.Command)
		sha, err := o.Git.CommitAll(ctx, msg)
		if err != nil {
			r.warn("could not commit the unverified change; it is left uncommitted on " + r.res.Branch + ": " + err.Error())
			return "", err
		}
		r.res.WIPCommit = sha
		if err := o.Git.Checkout(ctx, r.res.BaseBranch); err != nil {
			r.warn("could not return to " + r.res.BaseBranch + ": " + err.Error())
			return "", err
		}
		return fmt.Sprintf("committed %s on %s as a WIP; back on %s", sha, r.res.Branch, r.res.BaseBranch), nil
	})
}

// step runs one pipeline step, prints it, and journals the outcome.
func (r *runner) step(ctx context.Context, name string, fn func() (string, error)) error {
	start := time.Now()
	prevStats := len(r.res.Stats)

	if r.o.Verbose {
		r.outMu.Lock()
		fmt.Fprintf(r.o.Out, "\n[cleaner] %s\n", name)
		r.outMu.Unlock()

		detail, err := fn()
		entry := JournalEntry{Time: start, Step: name, Status: "ok", Detail: detail, Elapsed: time.Since(start).Milliseconds()}
		if err != nil {
			entry.Status, entry.Detail = "fail", err.Error()
			r.res.Stage = name
		}
		r.record(entry)

		r.outMu.Lock()
		if err != nil {
			fmt.Fprintf(r.o.Out, "  ✗ %s\n", indent(err.Error()))
		} else if detail != "" {
			fmt.Fprintf(r.o.Out, "  ✓ %s\n", indent(detail))
		}
		r.outMu.Unlock()

		return err
	}

	r.outMu.Lock()
	fmt.Fprintf(r.o.Out, "\n[cleaner] %s ", name)
	r.outMu.Unlock()

	var sp *spinner
	if isTerminal(r.o.Out) {
		sp = startSpinner(r.o.Out, &r.outMu)
	}

	detail, err := fn()
	elapsed := time.Since(start)

	if sp != nil {
		sp.Stop()
	}

	entry := JournalEntry{Time: start, Step: name, Status: "ok", Detail: detail, Elapsed: elapsed.Milliseconds()}
	if err != nil {
		entry.Status, entry.Detail = "fail", err.Error()
		r.res.Stage = name
	}
	r.record(entry)

	var inTokens, outTokens int
	for i := prevStats; i < len(r.res.Stats); i++ {
		inTokens += int(r.res.Stats[i].Usage.InputTokens)
		outTokens += int(r.res.Stats[i].Usage.OutputTokens)
	}

	r.outMu.Lock()
	fmt.Fprintf(r.o.Out, "%s\n", FormatTokenTiming(elapsed, inTokens, outTokens))
	if err != nil {
		fmt.Fprintf(r.o.Out, "  ✗ %s\n", indent(err.Error()))
	}
	r.outMu.Unlock()

	return err
}

var spinnerFrames = []byte{'|', '/', '-', '\\'}

type spinner struct {
	w       io.Writer
	mu      *sync.Mutex
	ticker  *time.Ticker
	done    chan struct{}
	stopped chan struct{}
}

func startSpinner(w io.Writer, mu *sync.Mutex) *spinner {
	s := &spinner{
		w:       w,
		mu:      mu,
		ticker:  time.NewTicker(100 * time.Millisecond),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	if s.mu != nil {
		s.mu.Lock()
	}
	_, _ = s.w.Write([]byte{spinnerFrames[0]})
	if s.mu != nil {
		s.mu.Unlock()
	}

	go s.run()
	return s
}

func (s *spinner) run() {
	defer close(s.stopped)
	frameIdx := 1
	for {
		select {
		case <-s.done:
			return
		case <-s.ticker.C:
			if s.mu != nil {
				s.mu.Lock()
			}
			_, _ = s.w.Write([]byte{'\b', spinnerFrames[frameIdx]})
			if s.mu != nil {
				s.mu.Unlock()
			}
			frameIdx = (frameIdx + 1) % len(spinnerFrames)
		}
	}
}

func (s *spinner) Stop() {
	s.ticker.Stop()
	close(s.done)
	<-s.stopped
	if s.mu != nil {
		s.mu.Lock()
	}
	_, _ = s.w.Write([]byte{'\b', ' ', '\b'})
	if s.mu != nil {
		s.mu.Unlock()
	}
}

func isTerminal(w io.Writer) bool {
	if w == nil || w == io.Discard {
		return false
	}
	if td, ok := w.(interface{ IsTerminal() bool }); ok {
		return td.IsTerminal()
	}
	if _, ok := w.(*bytes.Buffer); ok {
		return true
	}
	if f, ok := w.(*os.File); ok {
		stat, err := f.Stat()
		if err != nil {
			return false
		}
		return (stat.Mode() & os.ModeCharDevice) != 0
	}
	return false
}

func (r *runner) warn(msg string) {
	r.res.Warnings = append(r.res.Warnings, msg)
	r.outMu.Lock()
	fmt.Fprintf(r.o.Out, "  ! %s\n", msg)
	r.outMu.Unlock()
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

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

func mustHead(ctx context.Context, g *Git) string {
	h, err := g.Head(ctx)
	if err != nil {
		return "HEAD"
	}
	return h
}
