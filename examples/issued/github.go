package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// GitHub is a four-call REST client: read an issue, read its comments, create
// an issue. It is ~100 lines of net/http because that is all this program
// needs, and a dependency-free example should stay dependency-free.
//
// It lives OUTSIDE the agent on purpose. There is no `create_issue` tool, and
// the model has no network reach of any kind: `fetch_url` is deliberately not
// in tools.All(), and this client is called by main() before and after the
// run, never during it. The consequence is worth stating plainly — no sequence
// of model outputs can cause this program to write to GitHub. The human
// suppresses the write with `--dry-run` rather than enabling it with `--create`.
type GitHub struct {
	Token   string
	BaseURL string // https://api.github.com, or a GitHub Enterprise host
	client  *http.Client
}

func NewGitHub() *GitHub {
	base := strings.TrimSuffix(env("GITHUB_API_URL", "https://api.github.com"), "/")
	return &GitHub{
		Token:   env("GITHUB_TOKEN", os.Getenv("GH_TOKEN")),
		BaseURL: base,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

type ghUser struct {
	Login string `json:"login"`
}

type ghIssue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	User    ghUser `json:"user"`
}

type ghComment struct {
	Body string `json:"body"`
	User ghUser `json:"user"`
}

// ReadIssue fetches an issue and up to one page of its comments. Reading a
// public issue needs no token; a private one needs `repo` scope, and the 404
// GitHub returns in that case is indistinguishable from a genuinely missing
// issue, so the error message names both possibilities rather than guessing.
func (g *GitHub) ReadIssue(ref IssueRef) (ghIssue, []ghComment, error) {
	var issue ghIssue
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", ref.Owner, ref.Repo, ref.Number)
	if err := g.do(http.MethodGet, path, nil, &issue); err != nil {
		if strings.Contains(err.Error(), "404") && g.Token == "" {
			return issue, nil, fmt.Errorf("%w (set GITHUB_TOKEN if the repository is private)", err)
		}
		return issue, nil, err
	}
	var comments []ghComment
	if err := g.do(http.MethodGet, path+"/comments?per_page=100", nil, &comments); err != nil {
		// A readable issue with unreadable comments is still a usable report.
		fmt.Fprintf(os.Stderr, "[issued] warning: could not read comments: %v\n", err)
	}
	return issue, comments, nil
}

// CreateIssue files the issue and returns its URL.
func (g *GitHub) CreateIssue(owner, repo, title, body string, labels []string) (string, error) {
	if g.Token == "" {
		return "", fmt.Errorf("creating an issue needs a token: set GITHUB_TOKEN (or GH_TOKEN)")
	}
	payload := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		payload["labels"] = labels
	}
	var created ghIssue
	if err := g.do(http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues", owner, repo), payload, &created); err != nil {
		return "", err
	}
	return created.HTMLURL, nil
}

func (g *GitHub) do(method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, g.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "agentkit-issued-example")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, firstLine(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// DetectRepo reads `git remote get-url origin` and parses owner/repo out of
// it, handling both the SSH and HTTPS spellings. A repository with no origin
// is not an error — it means --repo is required, and saying that is more
// useful than failing here.
func DetectRepo(dir string) (owner, repo string, ok bool) {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", "", false
	}
	return ParseRemote(strings.TrimSpace(string(out)))
}

// ParseRemote handles git@github.com:owner/repo.git, https://github.com/owner/repo
// and ssh://git@github.com/owner/repo.git.
func ParseRemote(remote string) (owner, repo string, ok bool) {
	s := strings.TrimSuffix(remote, ".git")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if at := strings.Index(s, "@"); at >= 0 {
			s = s[at+1:]
		}
	} else if i := strings.Index(s, ":"); i >= 0 && strings.Contains(s[:i], "@") {
		s = s[strings.Index(s, "@")+1:] // git@host:owner/repo
		s = strings.Replace(s, ":", "/", 1)
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 3 {
		return "", "", false
	}
	owner, repo = parts[len(parts)-2], parts[len(parts)-1]
	if owner == "" || repo == "" {
		return "", "", false
	}
	return owner, repo, true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
