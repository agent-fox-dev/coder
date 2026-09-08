package main

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// IssueRef is the (owner, repo, number) triple every later step is keyed on.
type IssueRef struct {
	Owner  string
	Repo   string
	Number int
}

func (r IssueRef) Slug() string { return r.Owner + "/" + r.Repo }

func (r IssueRef) URL() string {
	return fmt.Sprintf("https://github.com/%s/%s/issues/%d", r.Owner, r.Repo, r.Number)
}

// ErrBadIssueURL is returned by ParseIssueURL for anything that is not a
// GitHub issue URL. It is a sentinel so main can map it to its own exit code:
// a malformed argument is a usage error, not a failed run.
var ErrBadIssueURL = errors.New("cleaner: not a GitHub issue URL")

// issueURLRe is af-fix's regex with the owner/repo character classes made
// explicit. GitHub names are ASCII letters, digits, hyphen, underscore and
// dot; `[^/]+` would also match a query string or a fragment, which is how a
// URL copied out of a browser ("…/issues/42#issuecomment-99") ends up being
// parsed as issue number "42#issuecomment-99" and failing later, further from
// the cause.
var issueURLRe = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/issues/([0-9]+)$`)

// linkedPRRe finds pull-request URLs in issue prose. It is deliberately not
// anchored: these appear inside sentences.
var linkedPRRe = regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]+)`)

// ParseIssueURL validates and splits a GitHub issue URL.
//
// Two normalizations happen before the match, and both exist because the URL
// arrives from a human's clipboard: a trailing slash is dropped, and a
// fragment or query string is cut. Everything else is rejected — in
// particular a `/pull/` URL, which is a different object with a different API
// and would otherwise be "fixed" as if it were an issue.
func ParseIssueURL(raw string) (IssueRef, error) {
	s := strings.TrimSpace(raw)
	if i := strings.IndexAny(s, "#?"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, "/")

	m := issueURLRe.FindStringSubmatch(s)
	if m == nil {
		return IssueRef{}, fmt.Errorf("%w: %q", ErrBadIssueURL, raw)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 {
		return IssueRef{}, fmt.Errorf("%w: issue number %q", ErrBadIssueURL, m[3])
	}
	return IssueRef{Owner: m[1], Repo: m[2], Number: n}, nil
}

// Issue is the subset of `gh issue view --json …` this program reads. The
// field names are gh's, so the JSON decodes without a translation layer and a
// canned fixture in testdata/ is a real gh response rather than a lookalike.
type Issue struct {
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	URL      string    `json:"url"`
	Labels   []Label   `json:"labels"`
	Author   Author    `json:"author"`
	Comments []Comment `json:"comments"`
}

type Label struct {
	Name string `json:"name"`
}

type Author struct {
	Login string `json:"login"`
}

type Comment struct {
	Author Author `json:"author"`
	Body   string `json:"body"`
}

// LinkedPR is the context a pull request referenced from the issue adds.
type LinkedPR struct {
	Number int           `json:"number"`
	Title  string        `json:"title"`
	Body   string        `json:"body"`
	Files  []ChangedFile `json:"files"`
}

type ChangedFile struct {
	Path string `json:"path"`
}

func (i *Issue) LabelNames() []string {
	out := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		out = append(out, l.Name)
	}
	return out
}

// LinkedPRNumbers scans the issue body and every comment for pull-request
// URLs in the SAME repository, de-duplicated and sorted.
//
// Same-repository is a real restriction, not an oversight: a PR link to
// another repo is context this program cannot fetch under the issue's
// credentials, and fetching it would be a second authorization question.
func (i *Issue) LinkedPRNumbers(ref IssueRef) []int {
	seen := map[int]bool{}
	scan := func(s string) {
		for _, m := range linkedPRRe.FindAllStringSubmatch(s, -1) {
			if !strings.EqualFold(m[1], ref.Owner) || !strings.EqualFold(m[2], ref.Repo) {
				continue
			}
			if n, err := strconv.Atoi(m[3]); err == nil && n > 0 {
				seen[n] = true
			}
		}
	}
	scan(i.Body)
	for _, c := range i.Comments {
		scan(c.Body)
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
