package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Hub is everything this program does to GitHub. It is an interface for two
// reasons that both showed up while building it: the end-to-end test needs a
// double that records posts instead of making them, and --dry-run needs a
// decorator that lets reads through while turning writes into printed
// previews. Both are impossible against a package of free functions that shell
// out to `gh`.
type Hub interface {
	// Check fails when the backend cannot be used at all — gh missing, gh not
	// authenticated. It runs in pre-flight, before anything is fetched.
	Check(ctx context.Context) error
	Issue(ctx context.Context, ref IssueRef) (*Issue, error)
	PullRequest(ctx context.Context, ref IssueRef, number int) (*LinkedPR, error)
	// Comment posts an issue comment and returns its URL.
	Comment(ctx context.Context, ref IssueRef, body string) (string, error)
	// CreatePR opens a pull request and returns its URL.
	CreatePR(ctx context.Context, ref IssueRef, pr PullRequestSpec) (string, error)
}

// PullRequestSpec is what CreatePR needs. Base and Head are branch names.
type PullRequestSpec struct {
	Base  string
	Head  string
	Title string
	Body  string
}

// ---------------------------------------------------------------- gh CLI --

// ghHub is the real backend: the `gh` CLI, which already solved
// authentication, enterprise hosts, and token refresh. Re-implementing the
// REST calls here would buy nothing except a second credential path to get
// wrong.
type ghHub struct {
	run Runner // injected so the tests can assert on the argv
}

func newGHHub(r Runner) *ghHub { return &ghHub{run: r} }

func (g *ghHub) Check(ctx context.Context) error {
	out, code, err := g.run(ctx, "", []string{"gh", "auth", "status"})
	if err != nil {
		return fmt.Errorf("gh CLI is required but not usable: %w\n"+
			"  install it (https://cli.github.com) and run: gh auth login", err)
	}
	if code != 0 {
		return fmt.Errorf("gh is not authenticated (gh auth status exited %d):\n%s\n"+
			"  run: gh auth login", code, strings.TrimSpace(out))
	}
	return nil
}

func (g *ghHub) Issue(ctx context.Context, ref IssueRef) (*Issue, error) {
	out, code, err := g.run(ctx, "", []string{
		"gh", "issue", "view", fmt.Sprint(ref.Number),
		"--repo", ref.Slug(),
		"--json", "title,body,labels,author,comments,url",
	})
	if err != nil || code != 0 {
		return nil, fmt.Errorf("fetching %s#%d failed: %s\n"+
			"  • is the issue number right, and does the repository exist?\n"+
			"  • do you have read access? (gh auth status)",
			ref.Slug(), ref.Number, ghErrText(out, code, err))
	}
	var issue Issue
	if err := json.Unmarshal([]byte(out), &issue); err != nil {
		return nil, fmt.Errorf("decoding the issue payload: %w", err)
	}
	if issue.URL == "" {
		issue.URL = ref.URL()
	}
	return &issue, nil
}

func (g *ghHub) PullRequest(ctx context.Context, ref IssueRef, number int) (*LinkedPR, error) {
	out, code, err := g.run(ctx, "", []string{
		"gh", "pr", "view", fmt.Sprint(number),
		"--repo", ref.Slug(),
		"--json", "number,title,body,files",
	})
	if err != nil || code != 0 {
		return nil, fmt.Errorf("fetching %s#%d: %s", ref.Slug(), number, ghErrText(out, code, err))
	}
	var pr LinkedPR
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return nil, fmt.Errorf("decoding the pull-request payload: %w", err)
	}
	return &pr, nil
}

func (g *ghHub) Comment(ctx context.Context, ref IssueRef, body string) (string, error) {
	// --body-file - keeps a multi-kilobyte markdown comment off the argv,
	// where it would be subject to ARG_MAX and to whatever the shell in the
	// middle decides to do with a backtick.
	out, code, err := g.run(ctx, "", []string{
		"gh", "issue", "comment", fmt.Sprint(ref.Number),
		"--repo", ref.Slug(), "--body-file", "-",
	}, body)
	if err != nil || code != 0 {
		return "", fmt.Errorf("posting a comment on %s#%d: %s", ref.Slug(), ref.Number, ghErrText(out, code, err))
	}
	return strings.TrimSpace(lastLine(out)), nil
}

func (g *ghHub) CreatePR(ctx context.Context, ref IssueRef, pr PullRequestSpec) (string, error) {
	out, code, err := g.run(ctx, "", []string{
		"gh", "pr", "create", "--repo", ref.Slug(),
		"--base", pr.Base, "--head", pr.Head,
		"--title", pr.Title, "--body-file", "-",
	}, pr.Body)
	if err != nil || code != 0 {
		return "", fmt.Errorf("creating a pull request for %s: %s", pr.Head, ghErrText(out, code, err))
	}
	return strings.TrimSpace(lastLine(out)), nil
}

func ghErrText(out string, code int, err error) string {
	switch {
	case err != nil:
		return err.Error()
	case strings.TrimSpace(out) == "":
		return fmt.Sprintf("gh exited %d with no output", code)
	default:
		return fmt.Sprintf("gh exited %d: %s", code, strings.TrimSpace(out))
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// -------------------------------------------------------------- dry run --

// dryHub passes reads through to the real backend and turns every write into
// a preview on stderr.
//
// It wraps rather than replaces the backend because a dry run that also
// stubbed the READS would exercise a different program: the analysis the model
// produces is only interesting if it saw the real issue.
type dryHub struct {
	inner Hub
	out   io.Writer
	// posted records what would have been sent, so the final summary can say
	// how many comments were suppressed rather than implying none were due.
	posted int
}

func (d *dryHub) Check(ctx context.Context) error { return d.inner.Check(ctx) }

func (d *dryHub) Issue(ctx context.Context, ref IssueRef) (*Issue, error) {
	return d.inner.Issue(ctx, ref)
}

func (d *dryHub) PullRequest(ctx context.Context, ref IssueRef, n int) (*LinkedPR, error) {
	return d.inner.PullRequest(ctx, ref, n)
}

func (d *dryHub) Comment(_ context.Context, ref IssueRef, body string) (string, error) {
	d.posted++
	fmt.Fprintf(d.out, "\n--- dry run: comment that WOULD be posted to %s#%d ---\n%s\n--- end ---\n",
		ref.Slug(), ref.Number, body)
	return "(dry-run: not posted)", nil
}

func (d *dryHub) CreatePR(_ context.Context, ref IssueRef, pr PullRequestSpec) (string, error) {
	d.posted++
	fmt.Fprintf(d.out, "\n--- dry run: pull request that WOULD be opened on %s ---\n"+
		"base: %s\nhead: %s\ntitle: %s\n\n%s\n--- end ---\n",
		ref.Slug(), pr.Base, pr.Head, pr.Title, pr.Body)
	return "(dry-run: not created)", nil
}

// ------------------------------------------------------------ file-backed --

// fileHub reads the issue from a JSON file instead of from GitHub, and
// refuses to write anything at all.
//
// It exists so the whole pipeline can be exercised — including a real model
// run — on a machine with no gh, no token and no network path to github.com.
// The file it reads is exactly what `gh issue view --json …` prints, so a
// fixture can be captured from a real issue with a shell redirect.
type fileHub struct{ path string }

func (f *fileHub) Check(context.Context) error {
	if _, err := os.Stat(f.path); err != nil {
		return fmt.Errorf("--issue-file %s: %w", f.path, err)
	}
	return nil
}

func (f *fileHub) Issue(_ context.Context, ref IssueRef) (*Issue, error) {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return nil, err
	}
	var issue Issue
	if err := json.Unmarshal(b, &issue); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", f.path, err)
	}
	if issue.URL == "" {
		issue.URL = ref.URL()
	}
	return &issue, nil
}

var errOffline = errors.New("cleaner: --issue-file is offline; GitHub is not reachable in this mode")

func (f *fileHub) PullRequest(context.Context, IssueRef, int) (*LinkedPR, error) {
	return nil, errOffline
}

func (f *fileHub) Comment(context.Context, IssueRef, string) (string, error) {
	return "", errOffline
}

func (f *fileHub) CreatePR(context.Context, IssueRef, PullRequestSpec) (string, error) {
	return "", errOffline
}
