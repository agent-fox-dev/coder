package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
)

// SearchMatch is one hit (REQ-TOOL-05).
type SearchMatch struct {
	File   string   `json:"file"`
	Line   int      `json:"line"`
	Text   string   `json:"text"`
	Before []string `json:"before,omitzero"`
	After  []string `json:"after,omitzero"`
}

// SearchParams is the declared parameter set.
type SearchParams struct {
	Pattern      string `json:"pattern"`
	Path         string `json:"path"`
	ContextLines int    `json:"context_lines"`
	FileGlob     string `json:"file_glob"`
	// CaseSensitive is a POINTER because absent and false are different
	// answers. Absent means smart-case; false means explicitly insensitive.
	// A plain bool would make "the caller said nothing" indistinguishable
	// from "the caller asked for insensitive", and smart-case is the
	// behaviour a user typing a lowercase pattern expects.
	CaseSensitive *bool `json:"case_sensitive"`
	MaxMatches    int   `json:"max_matches"`
}

// SearchResult is the returned envelope.
type SearchResult struct {
	Matches       []SearchMatch `json:"matches"`
	Truncated     bool          `json:"truncated"`
	FilesSearched int           `json:"files_searched"`
}

// binarySniffBytes is how much of a file is examined for a NUL byte.
//
// ripgrep reads a similar prefix. A file whose first 8 KiB are clean and which
// turns binary later is searched as text by both, so the tools agree — which
// matters more here than either answer being independently ideal.
const binarySniffBytes = 8 << 10

// MaxSearchContextLines bounds context_lines.
//
// Context multiplies the result: 100 matches with 50 lines either side is
// 10,000 lines the model pays for and did not ask for.
const MaxSearchContextLines = 20

// SearchBackend names which implementation answered, for the parity test and
// for anyone debugging a disagreement.
type SearchBackend string

const (
	BackendRipgrep SearchBackend = "ripgrep"
	BackendNative  SearchBackend = "native"
)

// searchFiles is REQ-TOOL-05.
//
// Declared semantics, since the two backends do not agree out of the box and
// the requirement is that the fallback matches OURS:
//
//   - case_sensitive absent means SMART-CASE: an all-lowercase pattern matches
//     insensitively, anything with an uppercase rune matches sensitively.
//     ripgrep defaults to sensitive, so the accelerated path is asked for
//     --smart-case explicitly.
//   - Binary files are SKIPPED, decided by a NUL byte in the first 8 KiB.
//   - file_glob uses AgentKit's glob dialect (MatchGlob), applied to the path
//     RELATIVE to the search root.
//   - The pattern is Go's regexp (RE2). ripgrep is asked for those semantics
//     too, so a pattern that works on one works on the other.
//   - files_searched counts the files SELECTED for search — everything left
//     after the ignore rules, the hidden-entry rule and file_glob. A binary
//     file is counted as selected and then skipped, because whether a file
//     turns out to be binary is not something the caller can predict from the
//     query. ripgrep cannot supply this number (its --stats "searches" counts
//     files that MATCHED, and reports 0 for a query that scanned a hundred
//     files), so both backends take it from the same walk. The count does not
//     change when the result is truncated: a truncated native search stops
//     READING files but finishes the walk, because a number that meant
//     "selected" on one query and "examined before we gave up" on another
//     would be a number nobody can use. The walk is readdir plus pattern
//     matching; the file reads are what truncation exists to avoid.
//   - HIDDEN entries — any path component beginning with "." — are skipped,
//     which is ripgrep's default and also what keeps `.git/` internals and
//     `.env` out of a result the model reads.
//   - A git repository is NOT required. Ignore rules are applied wherever they
//     are found; with no .gitignore anywhere, every non-hidden, non-binary
//     file is searched. ripgrep only honours .gitignore inside a repository,
//     so the accelerated path is asked for --no-require-git.
func (f *fileTools) searchFiles() core.Tool {
	return core.Tool{
		Name: "search_files",
		Description: "Search file contents by regular expression, skipping .gitignored " +
			"and binary files.",
		Builtin: true,
		PromptGuidelines: []string{
			"Prefer search_files over execute+grep: it respects .gitignore and returns structured matches.",
		},
		InputSchema: schema.Object(
			schema.Prop("pattern", schema.String("Regular expression (RE2 syntax)")),
			schema.Opt("path", schema.String("Directory to search from (default the workspace root)")),
			schema.Opt("context_lines", schema.Int("Lines of context either side of a match")),
			schema.Opt("file_glob", schema.String("Only search files matching this glob, e.g. **/*.go")),
			schema.Opt("case_sensitive", schema.Bool("Omit for smart-case: a lowercase pattern matches any case")),
			schema.Opt("max_matches", schema.Int("Maximum matches to return")),
		),
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a SearchParams
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			if strings.TrimSpace(a.Pattern) == "" {
				return core.ErrResult("invalid_arguments", "pattern is required")
			}
			if a.Path == "" {
				a.Path = "."
			}
			root, err := f.ws.Resolve(a.Path)
			if err != nil {
				return core.ErrResult("path_not_allowed", err.Error())
			}
			if a.ContextLines < 0 || a.ContextLines > MaxSearchContextLines {
				return core.ErrResult("invalid_arguments", fmt.Sprintf(
					"context_lines must be between 0 and %d", MaxSearchContextLines))
			}

			res, _, err := Search(ctx, root, a)
			if err != nil {
				if ctx.Err() != nil {
					return core.ErrResult("aborted", "Operation aborted")
				}
				var perr *SearchPatternError
				if errors.As(err, &perr) {
					return core.ErrResult("invalid_arguments", perr.Error())
				}
				return core.ErrResult("search_failed", err.Error())
			}

			out := core.OKResult(map[string]any{
				"matches":        res.Matches,
				"truncated":      res.Truncated,
				"files_searched": res.FilesSearched,
			})
			if res.Truncated {
				out.Data["note"] = FindMarker(effectiveMax(a.MaxMatches))
				out.Metadata = &core.ToolMetadata{
					Truncated: true, TruncatedBy: string(TruncatedByLines),
				}
			}
			return out
		},
	}
}

// SearchPatternError is a bad regular expression, separated so the tool can
// report it as invalid arguments rather than as a failed search — the caller
// has to change the pattern, not retry.
type SearchPatternError struct{ Err error }

func (e *SearchPatternError) Error() string { return "invalid pattern: " + e.Err.Error() }
func (e *SearchPatternError) Unwrap() error { return e.Err }

func effectiveMax(n int) int {
	if n <= 0 || n > SearchMatchCap {
		return SearchMatchCap
	}
	return n
}

// Search runs the search and reports which backend answered.
//
// It is exported so the parity test can drive both backends over one tree, and
// so an embedder can search without going through the tool envelope.
func Search(ctx context.Context, root string, p SearchParams) (SearchResult, SearchBackend, error) {
	if _, err := compilePattern(p); err != nil {
		// Compiled up front even on the ripgrep path, so an invalid pattern is
		// one error message rather than two depending on what is installed.
		return SearchResult{}, "", err
	}
	if path, ok := ripgrepPath(); ok {
		res, err := searchRipgrep(ctx, path, root, p)
		if err == nil {
			return res, BackendRipgrep, nil
		}
		if ctx.Err() != nil {
			return SearchResult{}, BackendRipgrep, err
		}
		// A ripgrep that is present but fails — a version whose JSON shape
		// moved, a sandbox that blocks exec — must not take the tool down with
		// it. The native path is a complete implementation, not a stub, so
		// falling through costs correctness nothing.
		res, nerr := searchNative(ctx, root, p)
		if nerr != nil {
			return SearchResult{}, BackendNative, nerr
		}
		return res, BackendNative, nil
	}
	res, err := searchNative(ctx, root, p)
	return res, BackendNative, err
}

// ripgrepPath locates rg.
var ripgrepPath = func() (string, bool) {
	path, err := exec.LookPath("rg")
	if err != nil {
		return "", false
	}
	return path, true
}

// SetRipgrepLookup overrides ripgrep discovery and returns a restore func.
//
// It exists so the parity test of REQ-TOOL-05 can drive BOTH backends over one
// tree in one process. Without it the native path is only reachable on a
// machine without ripgrep — which is to say, it would ship untested on every
// machine that could compare it against the thing it has to agree with.
func SetRipgrepLookup(f func() (string, bool)) func() {
	prev := ripgrepPath
	ripgrepPath = f
	return func() { ripgrepPath = prev }
}

// compilePattern applies the smart-case rule.
func compilePattern(p SearchParams) (*regexp.Regexp, error) {
	pat := p.Pattern
	if !caseSensitive(p) {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, &SearchPatternError{Err: err}
	}
	return re, nil
}

// caseSensitive resolves the tri-state.
func caseSensitive(p SearchParams) bool {
	if p.CaseSensitive != nil {
		return *p.CaseSensitive
	}
	// Smart-case: any uppercase rune in the pattern makes it sensitive.
	return p.Pattern != strings.ToLower(p.Pattern)
}

// ---------------------------------------------------------------- native

func searchNative(ctx context.Context, root string, p SearchParams) (SearchResult, error) {
	re, err := compilePattern(p)
	if err != nil {
		return SearchResult{}, err
	}
	max := effectiveMax(p.MaxMatches)

	out := SearchResult{Matches: []SearchMatch{}}
	ig := loadIgnore(root)

	walkErr := filepath.WalkDir(root, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, rerr := filepath.Rel(root, abs)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if isHidden(d.Name()) || ig.match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			ig.enter(rel, abs)
			return nil
		}
		if !d.Type().IsRegular() {
			// A symlink, socket or device is not a file to grep. Following a
			// symlink here is also how a search inside a workspace reads
			// outside one.
			return nil
		}
		if p.FileGlob != "" && !MatchGlob(p.FileGlob, rel) {
			return nil
		}

		out.FilesSearched++
		if out.Truncated {
			return nil // counted, but the result is already full
		}
		found, ferr := searchFile(abs, rel, re, p.ContextLines, max-len(out.Matches))
		if ferr != nil {
			return nil // an unreadable file is skipped
		}
		if found.skipped {
			return nil // binary: selected and counted, but not scanned
		}
		out.Matches = append(out.Matches, found.matches...)
		if len(out.Matches) >= max {
			out.Truncated = true
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return SearchResult{}, ctx.Err()
	}
	if len(out.Matches) > max {
		out.Matches, out.Truncated = out.Matches[:max], true
	}
	return out, nil
}

// isHidden reports a dotfile or dot-directory.
func isHidden(name string) bool {
	return strings.HasPrefix(name, ".") && name != "." && name != ".."
}

type fileMatches struct {
	matches []SearchMatch
	skipped bool // binary
}

func searchFile(abs, rel string, re *regexp.Regexp, contextLines, budget int) (fileMatches, error) {
	if budget <= 0 {
		return fileMatches{}, nil
	}
	fh, err := os.Open(abs)
	if err != nil {
		return fileMatches{}, err
	}
	defer fh.Close()

	br := bufio.NewReaderSize(fh, binarySniffBytes)
	head, err := br.Peek(binarySniffBytes)
	if err != nil && !errors.Is(err, bufio.ErrBufferFull) && len(head) == 0 && err.Error() != "EOF" {
		return fileMatches{}, err
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return fileMatches{skipped: true}, nil
	}

	// The whole file is read line by line, keeping only a ring of `before`
	// lines. Reading it into memory would be simpler and would also mean a
	// 2 GB log file is a 2 GB allocation.
	var (
		out    fileMatches
		before = make([]string, 0, contextLines)
		// pending holds matches still collecting their `after` lines.
		pending []*SearchMatch
		lineNo  int
	)
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for sc.Scan() {
		lineNo++
		line := sc.Text()

		for i := 0; i < len(pending); {
			m := pending[i]
			if len(m.After) < contextLines {
				m.After = append(m.After, capLine(line))
				i++
				continue
			}
			pending = append(pending[:i], pending[i+1:]...)
		}

		if re.MatchString(line) && len(out.matches) < budget {
			out.matches = append(out.matches, SearchMatch{
				File: rel, Line: lineNo, Text: capLine(line),
				Before: append([]string(nil), before...),
			})
			if contextLines > 0 {
				pending = append(pending, &out.matches[len(out.matches)-1])
			}
		}

		if contextLines > 0 {
			before = append(before, capLine(line))
			if len(before) > contextLines {
				before = before[1:]
			}
		}
		if len(out.matches) >= budget && len(pending) == 0 {
			break
		}
	}
	if err := sc.Err(); err != nil {
		// A line past the scanner's bound: report what was found rather than
		// discarding the file's other matches.
		return out, nil
	}
	return out, nil
}

// capLine bounds one returned line (REQ-TOOL-09's per-line cap).
func capLine(s string) string {
	if len(s) <= SearchLineChars {
		return s
	}
	return s[:SearchLineChars] + "…"
}

// CountCandidates counts the files a search would select: everything left
// after the ignore rules, the hidden-entry rule and file_glob.
func CountCandidates(ctx context.Context, root string, p SearchParams) (int, error) {
	n := 0
	ig := loadIgnore(root)
	err := filepath.WalkDir(root, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, rerr := filepath.Rel(root, abs)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if isHidden(d.Name()) || ig.match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			ig.enter(rel, abs)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if p.FileGlob != "" && !MatchGlob(p.FileGlob, rel) {
			return nil
		}
		n++
		return nil
	})
	return n, err
}

// ---------------------------------------------------------------- ripgrep

// searchRipgrep is the accelerated path.
//
// The rg invocation is INTERNAL to the tool and does not pass through any
// command policy (REQ-TOOL-05). That is safe only because nothing the model
// supplies reaches a shell: the argv is built here, exec.Command takes it as a
// vector, and every model-supplied value is a separate argument. `--` before
// the pattern is what stops a pattern beginning with `-` from becoming a flag.
func searchRipgrep(ctx context.Context, rg, root string, p SearchParams) (SearchResult, error) {
	max := effectiveMax(p.MaxMatches)

	args := []string{
		"--json",
		"--regex-size-limit", "10M",
		// ripgrep only honours .gitignore inside a git repository; our declared
		// semantics do not require one, and without this a search under a
		// plain directory returns node_modules on the accelerated path and not
		// on the native one.
		"--no-require-git",
		// Deterministic order, at the cost of ripgrep's parallelism. It is
		// what makes truncation mean the same thing on both paths: "the first
		// N by path then line" rather than "whichever N finished first".
		"--sort", "path",
		// The summary event carries the searched-file count, which cannot be
		// derived from the match events — they only mention files that matched.
		"--stats",
		// RE2 semantics, so a pattern that compiles for the native backend
		// behaves the same here. Without this, rg's default engine accepts
		// constructs Go's regexp rejects and the two backends diverge on
		// exactly the patterns a caller would notice.
		"--engine", "default",
	}
	if p.CaseSensitive != nil {
		if !*p.CaseSensitive {
			args = append(args, "--ignore-case")
		} else {
			args = append(args, "--case-sensitive")
		}
	} else {
		args = append(args, "--smart-case")
	}
	if p.ContextLines > 0 {
		args = append(args, "--context", fmt.Sprint(p.ContextLines))
	}
	// file_glob is deliberately NOT passed to rg.
	//
	// It would be the cheaper thing to do — rg would skip the excluded files
	// without reading them — but rg's glob dialect is not ours, and handing it
	// the pattern lets it NARROW the file set. A post-filter can only remove
	// matches, never recover a file rg was told not to open, so wherever our
	// dialect is broader than rg's the accelerated path silently returns less
	// than the native one. Ours is the declared dialect (smart-case globs,
	// among other differences), so it is the only one that gets to decide.
	args = append(args, "--", p.Pattern, ".")

	cmd := exec.CommandContext(ctx, rg, args...)
	cmd.Dir = root
	// An empty environment, like every other subprocess here (REQ-SEC-08). rg
	// reads RIPGREP_CONFIG_PATH, and a config file picked up from the ambient
	// environment would change the tool's declared semantics invisibly.
	cmd.Env = []string{}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return SearchResult{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return SearchResult{}, err
	}

	res, parseErr := parseRipgrepJSON(stdout, p, max)

	// Drain whatever is left so rg is not killed by a broken pipe mid-write,
	// which would turn a successful truncated search into an error.
	_, _ = stdout.Read(make([]byte, 0))
	waitErr := cmd.Wait()

	if parseErr != nil {
		return SearchResult{}, parseErr
	}
	// One extra walk, with no file contents read: readdir plus ignore matching
	// is cheap next to the content scan rg just did, and it is the only way the
	// two backends report the same number.
	n, cerr := CountCandidates(ctx, root, p)
	if cerr != nil && ctx.Err() != nil {
		return SearchResult{}, cerr
	}
	res.FilesSearched = n

	if waitErr != nil {
		var ee *exec.ExitError
		// Exit 1 is "no matches", which is a result and not a failure. Exit 2
		// is a real error and carries a reason on stderr.
		if errors.As(waitErr, &ee) && ee.ExitCode() == 1 && len(res.Matches) == 0 {
			return res, nil
		}
		if len(res.Matches) > 0 {
			return res, nil // we stopped reading early; rg died of the pipe
		}
		return SearchResult{}, fmt.Errorf("ripgrep: %w: %s", waitErr,
			strings.TrimSpace(stderr.String()))
	}
	return res, nil
}

// rgEvent is the subset of rg's --json stream this needs.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Stats      struct {
			Searches int `json:"searches"`
		} `json:"stats"`
	} `json:"data"`
}

func parseRipgrepJSON(r interface{ Read([]byte) (int, error) }, p SearchParams, max int) (SearchResult, error) {
	out := SearchResult{Matches: []SearchMatch{}}

	// Context lines arrive as their own events, before and after the match
	// they belong to, so they are buffered per file and attached at the end.
	type ctxLine struct {
		n    int
		text string
	}
	var (
		pendingCtx []ctxLine
		curFile    string
	)
	attach := func() {
		if p.ContextLines == 0 {
			pendingCtx = nil
			return
		}
		for i := range out.Matches {
			m := &out.Matches[i]
			if m.File != curFile {
				continue
			}
			for _, c := range pendingCtx {
				switch {
				case c.n < m.Line && c.n >= m.Line-p.ContextLines:
					m.Before = append(m.Before, c.text)
				case c.n > m.Line && c.n <= m.Line+p.ContextLines:
					m.After = append(m.After, c.text)
				}
			}
		}
		pendingCtx = nil
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var ev rgEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return SearchResult{}, fmt.Errorf("ripgrep json: %w", err)
		}
		switch ev.Type {
		case "begin":
			attach()
			curFile = normalizeRGPath(ev.Data.Path.Text)
		case "end":
			attach()
		case "summary":
			// Deliberately ignored. rg's --stats "searches" counts files that
			// matched, not files searched, so it disagrees with the native
			// backend on every query — including reporting 0 for a search that
			// scanned the whole tree. CountCandidates supplies the number.
		case "match":
			file := normalizeRGPath(ev.Data.Path.Text)
			if p.FileGlob != "" && !MatchGlob(p.FileGlob, file) {
				// rg's glob dialect is not ours; ours is the declared one.
				continue
			}
			text := capLine(trimEOL(ev.Data.Lines.Text))
			// A match line is also CONTEXT for any adjacent match. ripgrep
			// emits each line once, as a match or as context but never both,
			// so without this two matches a line apart each lose the other
			// from their context — where reading the file directly, as the
			// native backend does, shows it.
			pendingCtx = append(pendingCtx, ctxLine{n: ev.Data.LineNumber, text: text})
			if len(out.Matches) >= max {
				out.Truncated = true
				continue
			}
			out.Matches = append(out.Matches, SearchMatch{
				File: file, Line: ev.Data.LineNumber, Text: text,
			})
		case "context":
			file := normalizeRGPath(ev.Data.Path.Text)
			if p.FileGlob != "" && !MatchGlob(p.FileGlob, file) {
				continue
			}
			pendingCtx = append(pendingCtx, ctxLine{
				n: ev.Data.LineNumber, text: capLine(trimEOL(ev.Data.Lines.Text))})
		}
	}
	attach()
	if err := sc.Err(); err != nil {
		return SearchResult{}, err
	}
	sortMatches(out.Matches)
	return out, nil
}

// normalizeRGPath strips the leading "./" rg emits for a relative search.
func normalizeRGPath(p string) string {
	p = filepath.ToSlash(p)
	return strings.TrimPrefix(p, "./")
}

func trimEOL(s string) string {
	return strings.TrimRight(s, "\r\n")
}

// sortMatches puts results in a deterministic order.
//
// The two backends walk in different orders — rg parallelizes across files —
// so without this the parity test compares two correct answers and fails, and
// a caller diffing two runs sees noise.
func sortMatches(m []SearchMatch) {
	sort.SliceStable(m, func(i, j int) bool {
		if m[i].File != m[j].File {
			return m[i].File < m[j].File
		}
		return m[i].Line < m[j].Line
	})
}
