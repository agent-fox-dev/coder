package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Runner runs one external command and returns its combined output, its exit
// code, and an error for the failures that are NOT an exit code — the program
// is missing, the context was cancelled, the binary could not be executed.
type Runner func(ctx context.Context, dir string, argv []string) (string, int, error)

// execRunner is the production Runner.
func execRunner(ctx context.Context, dir string, argv []string) (string, int, error) {
	if len(argv) == 0 {
		return "", -1, fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), ee.ExitCode(), nil
		}
		return string(out), -1, err
	}
	return string(out), 0, nil
}

// Git is the explicit list of git operations this program performs. There is
// no "run whatever the model says" path: every branch, commit and merge in a
// flatline run is issued from this file at a point in the pipeline that knows
// why — which is what lets the summary say "landed as <sha>" and mean it.
type Git struct {
	Dir   string
	run   Runner
	sleep func(time.Duration)
}

func NewGit(dir string, r Runner) *Git {
	return &Git{Dir: dir, run: r, sleep: time.Sleep}
}

func (g *Git) git(ctx context.Context, args ...string) (string, int, error) {
	out, code, err := g.run(ctx, g.Dir, append([]string{"git"}, args...))
	return strings.TrimRight(out, "\n"), code, err
}

func (g *Git) mustGit(ctx context.Context, args ...string) (string, error) {
	out, code, err := g.git(ctx, args...)
	if err != nil {
		return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		return out, fmt.Errorf("git %s exited %d: %s", strings.Join(args, " "), code, out)
	}
	return out, nil
}

func (g *Git) IsRepo(ctx context.Context) bool {
	out, code, err := g.git(ctx, "rev-parse", "--is-inside-work-tree")
	return err == nil && code == 0 && strings.TrimSpace(out) == "true"
}

// DirtyFiles returns the porcelain status lines. Empty means a clean tree.
func (g *Git) DirtyFiles(ctx context.Context) ([]string, error) {
	out, err := g.mustGit(ctx, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSpace(out), "\n"), nil
}

func (g *Git) Head(ctx context.Context) (string, error) {
	return g.mustGit(ctx, "rev-parse", "--short", "HEAD")
}

func (g *Git) CurrentBranch(ctx context.Context) (string, error) {
	return g.mustGit(ctx, "rev-parse", "--abbrev-ref", "HEAD")
}

func (g *Git) LocalBranchExists(ctx context.Context, name string) bool {
	_, code, err := g.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil && code == 0
}

func (g *Git) HasRemote(ctx context.Context, name string) bool {
	out, code, err := g.git(ctx, "remote")
	if err != nil || code != 0 {
		return false
	}
	for _, r := range strings.Split(out, "\n") {
		if strings.TrimSpace(r) == name {
			return true
		}
	}
	return false
}

// CreateBranch makes `name` at `start` and checks it out. agent-fox does the
// same with `git worktree add`; without worktrees the checkout IS the
// workspace, so the branch is created in place.
func (g *Git) CreateBranch(ctx context.Context, name, start string) error {
	_, err := g.mustGit(ctx, "checkout", "-q", "-b", name, start)
	return err
}

func (g *Git) Checkout(ctx context.Context, name string) error {
	_, err := g.mustGit(ctx, "checkout", "-q", name)
	return err
}

func (g *Git) DeleteBranch(ctx context.Context, name string) error {
	_, err := g.mustGit(ctx, "branch", "-q", "-D", name)
	return err
}

// ResetHardClean returns the checkout to `ref` and removes everything the
// agent left behind. It is the retry path's equivalent of agent-fox destroying
// a failed attempt's worktree and creating a fresh one.
func (g *Git) ResetHardClean(ctx context.Context, ref string) error {
	if _, err := g.mustGit(ctx, "reset", "-q", "--hard", ref); err != nil {
		return err
	}
	_, err := g.mustGit(ctx, "clean", "-q", "-fd")
	return err
}

// ChangedFiles lists the paths that differ from a commit, staged or not,
// including untracked files.
func (g *Git) ChangedFiles(ctx context.Context, since string) ([]string, error) {
	tracked, err := g.mustGit(ctx, "diff", "--name-only", since)
	if err != nil {
		return nil, err
	}
	untracked, err := g.mustGit(ctx, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, block := range []string{tracked, untracked} {
		for _, line := range strings.Split(block, "\n") {
			if line = strings.TrimSpace(line); line != "" && !seen[line] {
				seen[line] = true
				out = append(out, line)
			}
		}
	}
	return out, nil
}

// CommitAll stages everything and commits. It returns the short sha.
func (g *Git) CommitAll(ctx context.Context, message string) (string, error) {
	if _, err := g.mustGit(ctx, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := g.mustGit(ctx, "commit", "-q", "-m", message); err != nil {
		return "", err
	}
	return g.Head(ctx)
}

// CommitPaths commits only the named paths — agent-fox's
// `git add <spec>/tasks.json && git commit` for the state update. It returns
// "" and no error when the paths have no changes, which is agent-fox's
// issue-681 rule: nothing to commit is not a failure.
func (g *Git) CommitPaths(ctx context.Context, message string, paths ...string) (string, error) {
	if _, err := g.mustGit(ctx, append([]string{"add", "--"}, paths...)...); err != nil {
		return "", err
	}
	if _, code, err := g.git(ctx, "diff", "--cached", "--quiet"); err != nil {
		return "", err
	} else if code == 0 {
		return "", nil
	}
	if _, err := g.mustGit(ctx, "commit", "-q", "-m", message); err != nil {
		return "", err
	}
	return g.Head(ctx)
}

// LastMessage returns the full message of the branch tip.
func (g *Git) LastMessage(ctx context.Context, ref string) (string, error) {
	return g.mustGit(ctx, "log", "-1", "--format=%B", ref)
}

// Subjects lists the commit subjects on `branch` that are not on `base`,
// oldest first.
func (g *Git) Subjects(ctx context.Context, base, branch string) ([]string, error) {
	out, err := g.mustGit(ctx, "log", "--reverse", "--format=%s", base+".."+branch)
	if err != nil {
		return nil, err
	}
	var subjects []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			subjects = append(subjects, l)
		}
	}
	return subjects, nil
}

// SquashMergeInto lands `branch` on `base` as one commit with `message`, the
// way agent-fox's harvest does (`git merge --squash`, then commit). The
// checkout is left on `base`.
func (g *Git) SquashMergeInto(ctx context.Context, base, branch, message string) (string, error) {
	if err := g.Checkout(ctx, base); err != nil {
		return "", err
	}
	out, code, err := g.git(ctx, "merge", "--squash", "--", branch)
	if err != nil {
		return "", err
	}
	if code != 0 {
		// agent-fox hands a conflict to a merge agent. In a serial pass over
		// one spec the base branch cannot have moved under the group, so a
		// conflict here means something outside this run touched the base
		// branch; leaving it to a human is the honest outcome.
		_, _, _ = g.git(ctx, "reset", "-q", "--merge")
		return "", fmt.Errorf("squash merge of %s into %s conflicted; the branch is kept for a human:\n%s", branch, base, out)
	}
	if _, code, err := g.git(ctx, "diff", "--cached", "--quiet"); err != nil {
		return "", err
	} else if code == 0 {
		return "", nil // nothing to land
	}
	if _, err := g.mustGit(ctx, "commit", "-q", "-m", message); err != nil {
		return "", err
	}
	return g.Head(ctx)
}

// Push publishes a branch, retrying the transient half of the failure space
// with exponential backoff, as agent-fox's `_push_with_retry` does.
func (g *Git) Push(ctx context.Context, branch string, attempts int, log func(string)) error {
	delay := 2 * time.Second
	var last error
	for i := 1; i <= attempts; i++ {
		out, code, err := g.git(ctx, "push", "-u", "origin", branch)
		if err == nil && code == 0 {
			return nil
		}
		last = fmt.Errorf("git push exited %d: %s", code, out)
		if err != nil {
			last = err
		}
		if i == attempts || ctx.Err() != nil || nonRetryablePush(out) {
			break
		}
		log(fmt.Sprintf("push failed, retrying in %s (attempt %d/%d)", delay, i, attempts))
		g.sleep(delay)
		delay *= 2
	}
	return last
}

// nonRetryablePush is agent-fox's list of stderr patterns after which another
// attempt cannot succeed.
var nonRetryablePush = func(out string) bool {
	l := strings.ToLower(out)
	for _, p := range []string{"authentication failed", "permission denied", "could not resolve host",
		"connection refused", "connection timed out", "repository not found",
		"no anonymous write access", "terminal prompts disabled"} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------- branch names --

var nonBranch = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// GroupBranch is agent-fox's `feature/{spec}/{group}`.
func GroupBranch(specName string, group int) string {
	return fmt.Sprintf("feature/%s/%d", nonBranch.ReplaceAllString(specName, "-"), group)
}

// UniqueBranchName appends a numeric suffix until the name is free. agent-fox
// force-deletes a stale branch of the same name; a fresh name keeps whatever a
// previous run left behind readable instead.
func UniqueBranchName(ctx context.Context, g *Git, name string) string {
	candidate := name
	for i := 2; i < 50; i++ {
		if !g.LocalBranchExists(ctx, candidate) {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", name, i)
	}
	return fmt.Sprintf("%s-%d", name, time.Now().Unix())
}
