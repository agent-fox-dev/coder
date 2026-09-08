package main

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Runner runs one external command and returns its combined output, its exit
// code, and an error for the failures that are NOT an exit code — the program
// is missing, the context was cancelled, the binary could not be executed.
//
// The two are separated because they need different handling: a non-zero exit
// from `git status` is information, while "git: executable file not found" is
// a broken environment, and a single error return conflates them.
type Runner func(ctx context.Context, dir string, argv []string, stdin ...string) (string, int, error)

// execRunner is the production Runner. Output is combined because these
// commands are read by humans in a log, and interleaved stderr is what makes a
// failing command's message appear next to the command that produced it.
func execRunner(ctx context.Context, dir string, argv []string, stdin ...string) (string, int, error) {
	if len(argv) == 0 {
		return "", -1, fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if len(stdin) > 0 {
		cmd.Stdin = strings.NewReader(strings.Join(stdin, ""))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			return string(out), ee.ExitCode(), nil
		}
		return string(out), -1, err
	}
	return string(out), 0, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// Git is a thin, explicit wrapper over the git commands this program runs.
//
// There is no "run whatever the model says" path here on purpose: every git
// mutation in a cleaner run is issued by this file, from Go, at a point in the
// pipeline that knows why. The agent's shell is separately forbidden from
// running mutating git subcommands (see toolGuard), so the branch, the commit
// and the push mean exactly what the summary says they mean.
type Git struct {
	Dir   string
	run   Runner
	sleep func(time.Duration) // injectable so the push-retry test does not sleep
}

func NewGit(dir string, r Runner) *Git {
	return &Git{Dir: dir, run: r, sleep: time.Sleep}
}

func (g *Git) git(ctx context.Context, args ...string) (string, int, error) {
	out, code, err := g.run(ctx, g.Dir, append([]string{"git"}, args...))
	return strings.TrimRight(out, "\n"), code, err
}

// mustGit is for commands whose failure is fatal to the step that called them.
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

// BaseBranch is what a pull request will target: origin's default branch when
// the remote advertises one, else the branch that is checked out now.
//
// Hardcoding "main" — which the af-fix skill does, in three separate commands —
// is wrong on every repository that still uses `master`, on a fork whose
// default is a release branch, and on any repo where the work is landing on a
// long-lived integration branch.
func (g *Git) BaseBranch(ctx context.Context) string {
	if out, code, err := g.git(ctx, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && code == 0 {
		if b := strings.TrimPrefix(strings.TrimSpace(out), "origin/"); b != "" {
			return b
		}
	}
	if b, err := g.CurrentBranch(ctx); err == nil && b != "" && b != "HEAD" {
		return b
	}
	return "main"
}

func (g *Git) LocalBranchExists(ctx context.Context, name string) bool {
	_, code, err := g.git(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil && code == 0
}

func (g *Git) RemoteBranchExists(ctx context.Context, name string) bool {
	out, code, err := g.git(ctx, "ls-remote", "--heads", "origin", name)
	return err == nil && code == 0 && strings.TrimSpace(out) != ""
}

func (g *Git) CreateBranch(ctx context.Context, name string) error {
	_, err := g.mustGit(ctx, "checkout", "-b", name)
	return err
}

func (g *Git) Checkout(ctx context.Context, name string) error {
	_, err := g.mustGit(ctx, "checkout", name)
	return err
}

// ChangedFiles lists the paths that differ from a commit, staged or not,
// including untracked files. It is how the pipeline verifies that the
// implementation phase actually changed something before it reports success.
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

func (g *Git) CommitAll(ctx context.Context, message string) (string, error) {
	if _, err := g.mustGit(ctx, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := g.mustGit(ctx, "commit", "-m", message); err != nil {
		return "", err
	}
	return g.Head(ctx)
}

// Push publishes the branch, retrying the transient half of the failure space.
//
// The retry is bounded and reported rather than silent: a push that needed
// three attempts is a fact the run summary should carry, because it usually
// means the next person to run this will wait too.
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
		if i == attempts || ctx.Err() != nil {
			break
		}
		log(fmt.Sprintf("push failed, retrying in %s (attempt %d/%d)", delay, i, attempts))
		g.sleep(delay)
		delay *= 2
	}
	return last
}

// SquashMergeInto lands the branch on base as a single commit, reusing the
// branch tip's message. It is the --land=merge path.
func (g *Git) SquashMergeInto(ctx context.Context, base, branch, message string) error {
	if err := g.Checkout(ctx, base); err != nil {
		return err
	}
	if out, code, err := g.git(ctx, "merge", "--squash", branch); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("squash merge of %s into %s conflicted; resolve it by hand:\n%s", branch, base, out)
	}
	_, err := g.mustGit(ctx, "commit", "-m", message)
	return err
}

// ------------------------------------------------------------ branch names --

// stopWords are dropped from a branch slug. This is af-fix's list.
var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "for": true, "with": true, "of": true,
	"to": true, "in": true, "is": true, "fix": true, "add": true, "bug": true,
	"feature": true,
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// BranchSlug turns an issue title into the tail of a branch name:
// lowercase, non-alphanumerics collapsed to hyphens, stop words dropped, the
// first five surviving words kept, truncated to 40 characters at a hyphen
// boundary.
//
// It never returns the empty string: a title made entirely of stop words
// ("Fix the bug") would otherwise produce the branch name "fix/issue-42-",
// which git accepts and no human can read.
func BranchSlug(title string) string {
	words := strings.Split(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	kept := make([]string, 0, 5)
	for _, w := range words {
		if w == "" || stopWords[w] {
			continue
		}
		kept = append(kept, w)
		if len(kept) == 5 {
			break
		}
	}
	if len(kept) == 0 {
		return "issue"
	}
	slug := strings.Join(kept, "-")
	if len(slug) > 40 {
		slug = slug[:40]
		if i := strings.LastIndex(slug, "-"); i > 0 {
			slug = slug[:i]
		}
	}
	return strings.Trim(slug, "-")
}

// BranchName is the full name: a prefix chosen by classification, the issue
// number so the branch is traceable, and the slug.
func BranchName(kind Classification, number int, title string) string {
	return fmt.Sprintf("%s/issue-%d-%s", kind.BranchPrefix(), number, BranchSlug(title))
}

// UniqueBranchName appends a numeric suffix until the name is free both
// locally and on origin.
//
// af-fix halts here and asks the operator whether to force-push over the
// existing branch — inside a workflow whose first paragraph promises not to
// stop for confirmation. A second attempt at the same issue is normal (the
// first run's fix was rejected in review, say), so the resolution that keeps
// the promise is a fresh name, and force-pushing over someone else's branch is
// never something to do on a guess.
func UniqueBranchName(ctx context.Context, g *Git, name string) string {
	candidate := name
	for i := 2; i < 50; i++ {
		if !g.LocalBranchExists(ctx, candidate) && !g.RemoteBranchExists(ctx, candidate) {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", name, i)
	}
	return fmt.Sprintf("%s-%d", name, time.Now().Unix())
}
