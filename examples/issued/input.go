package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SourceKind is what the argument turned out to be.
type SourceKind string

const (
	SourceText  SourceKind = "text"
	SourceFile  SourceKind = "file"
	SourceStdin SourceKind = "stdin"
	SourceIssue SourceKind = "github issue"
)

func (s SourceKind) String() string { return string(s) }

// Report is the classified input: the problem text plus where it came from.
type Report struct {
	Kind   SourceKind
	Origin string // the path, the URL, or "argument"
	Body   string

	// Upstream is set for SourceIssue: the issue this report was read from.
	// It is what makes `issued <issue-url>` useful as a re-triage — the filed
	// issue can point back at the raw one it replaces.
	Upstream *IssueRef
}

// IssueRef identifies one GitHub issue.
type IssueRef struct {
	Owner  string
	Repo   string
	Number int
}

func (r IssueRef) String() string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

func (r IssueRef) URL() string {
	return fmt.Sprintf("https://github.com/%s/%s/issues/%d", r.Owner, r.Repo, r.Number)
}

// ErrNoInput is the "halt until input is received" branch of the skill, except
// that a program cannot block on a conversational turn: it exits with usage.
var ErrNoInput = errors.New("no problem report given")

// maxReportBytes bounds what one report can contribute to the prompt. A
// 40MB log pasted at a coding agent is a bill, not an input, and truncating
// here — deterministically, at a line boundary, with a visible marker — is
// better than discovering the context window at request time.
const maxReportBytes = 256 << 10

// ResolveInput classifies the argument and loads it. The classification is
// deliberately done HERE, in Go, and not delegated to the model:
//
//   - it decides whether a network call happens at all, which is a decision an
//     embedder must be able to audit without reading a transcript;
//   - a wrong guess costs a round trip and a filed issue about the wrong thing;
//   - and the rules are dull. "Does this path exist on disk" is not a judgement
//     call, and paying a model to make it is how prompt-shaped programs get
//     slow and non-deterministic for no benefit.
//
// The model is asked to do exactly one thing in this program — diagnose — and
// everything decidable in Go is decided in Go.
func ResolveInput(arg string, stdin io.Reader, gh *GitHub) (Report, error) {
	arg = strings.TrimSpace(arg)
	switch {
	case arg == "":
		return Report{}, ErrNoInput

	case arg == "-":
		b, err := io.ReadAll(io.LimitReader(stdin, maxReportBytes+1))
		if err != nil {
			return Report{}, fmt.Errorf("reading stdin: %w", err)
		}
		body := truncate(string(b))
		if strings.TrimSpace(body) == "" {
			return Report{}, ErrNoInput
		}
		return Report{Kind: SourceStdin, Origin: "stdin", Body: body}, nil

	default:
		if ref, ok := ParseIssueURL(arg); ok {
			return fetchIssueReport(ref, gh)
		}
		if body, ok, err := readIfFile(arg); err != nil {
			return Report{}, err
		} else if ok {
			return Report{Kind: SourceFile, Origin: filepath.Clean(arg), Body: body}, nil
		}
		return Report{Kind: SourceText, Origin: "argument", Body: truncate(arg)}, nil
	}
}

// readIfFile reports whether arg names a readable regular file, and returns
// its contents when it does. A path that exists but is a directory is not an
// error here: it falls through to the text branch, because "src/" is a
// plausible thing to say in a bug report.
func readIfFile(arg string) (string, bool, error) {
	if strings.ContainsAny(arg, "\n") {
		return "", false, nil // multi-line input is a report, not a path
	}
	info, err := os.Stat(arg)
	if err != nil || !info.Mode().IsRegular() {
		return "", false, nil
	}
	f, err := os.Open(arg)
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", arg, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxReportBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", arg, err)
	}
	return truncate(string(b)), true, nil
}

// ParseIssueURL recognizes a GitHub issue or pull-request URL. Anything else —
// including a github.com URL pointing at a file or a repository — is not an
// issue reference and is treated as text.
func ParseIssueURL(s string) (IssueRef, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" {
		return IssueRef{}, false
	}
	if h := strings.TrimPrefix(u.Host, "www."); h != "github.com" {
		return IssueRef{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return IssueRef{}, false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return IssueRef{}, false
	}
	return IssueRef{Owner: parts[0], Repo: parts[1], Number: n}, true
}

// fetchIssueReport renders an issue and its comments as one problem report.
// The comments are included because the diagnosis usually is not in the
// opening post — it is in the third reply, where someone pasted the traceback.
func fetchIssueReport(ref IssueRef, gh *GitHub) (Report, error) {
	if gh == nil {
		return Report{}, fmt.Errorf("cannot read %s: no GitHub client configured", ref)
	}
	issue, comments, err := gh.ReadIssue(ref)
	if err != nil {
		return Report{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub issue %s (state: %s)\nTitle: %s\nAuthor: %s\n\n%s\n",
		ref, issue.State, issue.Title, issue.User.Login, strings.TrimSpace(issue.Body))
	for _, c := range comments {
		fmt.Fprintf(&b, "\n--- comment by %s ---\n%s\n", c.User.Login, strings.TrimSpace(c.Body))
	}
	return Report{
		Kind:     SourceIssue,
		Origin:   ref.URL(),
		Body:     truncate(b.String()),
		Upstream: &ref,
	}, nil
}

// truncate cuts at a line boundary and says so, because a report that ends
// mid-stack-trace with no marker reads to the model as a complete stack trace
// that simply had no more frames.
func truncate(s string) string {
	if len(s) <= maxReportBytes {
		return s
	}
	cut := s[:maxReportBytes]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + "\n\n[... truncated by issued at " + strconv.Itoa(maxReportBytes) + " bytes ...]"
}
