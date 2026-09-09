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
	// TruncatedBy names which limit fired when Truncated is set: the match
	// cap ("lines") or the 50 KB byte cap ("bytes") (REQ-TOOL-09).
	TruncatedBy TruncatedBy `json:"truncated_by,omitzero"`
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
		Description: "Search file contents by regular expression, skipping .gitignored, " +
			"hidden (dot-prefixed) and binary files. Returns at most max_matches (<= 100) " +
			"matches with context_lines (<= 20) lines either side.",
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

			res, _, err := SearchIn(ctx, root, a, f.ig)
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
			note := ""
			if res.Truncated {
				// The marker names THIS tool's parameter, max_matches, and its
				// cap of 100 — not find_files' `limit` (REQ-TOOL-09b).
				note = SearchMarker(effectiveMax(a.MaxMatches))
				if res.TruncatedBy == TruncatedByBytes {
					note = SearchBytesMarker(len(res.Matches), DefaultByteLimit)
				}
				out.Data["note"] = note
				out.Metadata = &core.ToolMetadata{
					Truncated: true, TruncatedBy: string(res.TruncatedBy),
				}
			}
			out.Text = RenderSearchText(res, note)
			return out
		},
	}
}

// RenderSearchText is the model-facing rendering of a search result: grep
// style, grouped by file.
//
//	path/to/file.go
//	  12- context before
//	  13: the matched line
//	  14- context after
//
//	other/file.go
//	  7: another match
//	[marker]
//
// It replaces the JSON envelope for the model (core.ToolResult.Text). Per
// match the envelope repeats the file name and the four keys, and every line
// of code inside it is JSON-escaped; grep's shape says the file once per
// group and the line once, unescaped, which for a typical result is a third
// fewer bytes and a better tokenization of the code itself. Data keeps the
// structured form for programmatic consumers.
//
// Within a file each line number is printed once: a line that is both a match
// and another match's context is shown as the match.
func RenderSearchText(res SearchResult, marker string) string {
	if len(res.Matches) == 0 {
		return fmt.Sprintf("No matches (%d files searched).", res.FilesSearched)
	}
	type line struct {
		text  string
		match bool
	}
	var (
		b       strings.Builder
		files   []string
		perFile = map[string]map[int]line{}
	)
	for _, m := range res.Matches {
		lines, ok := perFile[m.File]
		if !ok {
			lines = map[int]line{}
			perFile[m.File] = lines
			files = append(files, m.File)
		}
		put := func(n int, text string, match bool) {
			if prev, seen := lines[n]; seen && prev.match && !match {
				return
			}
			lines[n] = line{text, match}
		}
		for i, t := range m.Before {
			put(m.Line-len(m.Before)+i, t, false)
		}
		put(m.Line, m.Text, true)
		for i, t := range m.After {
			put(m.Line+1+i, t, false)
		}
	}
	for fi, f := range files {
		if fi > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(f)
		b.WriteByte('\n')
		lines := perFile[f]
		nums := make([]int, 0, len(lines))
		for n := range lines {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			l := lines[n]
			sep := '-'
			if l.match {
				sep = ':'
			}
			fmt.Fprintf(&b, "  %d%c %s\n", n, sep, l.text)
		}
	}
	if marker != "" {
		b.WriteString(marker)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
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
// so an embedder can search without going through the tool envelope. It reads
// the real ignore environment; SearchIn takes an explicit one.
func Search(ctx context.Context, root string, p SearchParams) (SearchResult, SearchBackend, error) {
	return SearchIn(ctx, root, p, IgnoreOptions{})
}

// SearchIn is Search with an injected ignore environment (NFR-TEST-04), so a
// test can pin an empty global excludes layer instead of inheriting the
// developer's.
func SearchIn(ctx context.Context, root string, p SearchParams, ig IgnoreOptions) (SearchResult, SearchBackend, error) {
	res, backend, err := searchIn(ctx, root, p, ig)
	if err != nil {
		return res, backend, err
	}
	// REQ-TOOL-09: the 50 KB byte limit composes with the match cap on BOTH
	// backends. 100 matches with 20 lines of context either side at 500
	// chars a line is two megabytes; without this the model paid for it.
	capSearchBytes(&res, DefaultByteLimit)
	return res, backend, nil
}

// capSearchBytes applies the head-mode byte budget over the assembled matches,
// dropping whole matches from the end until the payload fits, and records
// which limit fired.
func capSearchBytes(res *SearchResult, budget int) {
	if res.Truncated {
		res.TruncatedBy = TruncatedByLines
	}
	used := 0
	for i, m := range res.Matches {
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		// +1 for the separator; the envelope's own keys are a rounding error
		// next to the budget.
		used += len(b) + 1
		if used > budget && i > 0 {
			res.Matches = res.Matches[:i]
			res.Truncated, res.TruncatedBy = true, TruncatedByBytes
			return
		}
	}
}

func searchIn(ctx context.Context, root string, p SearchParams, ig IgnoreOptions) (SearchResult, SearchBackend, error) {
	if _, err := compilePattern(p); err != nil {
		// Compiled up front even on the ripgrep path, so an invalid pattern is
		// one error message rather than two depending on what is installed.
		return SearchResult{}, "", err
	}
	if path, ok := ripgrepPath(); ok {
		res, err := searchRipgrep(ctx, path, root, p, ig)
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
		res, nerr := searchNative(ctx, root, p, ig)
		if nerr != nil {
			return SearchResult{}, BackendNative, nerr
		}
		return res, BackendNative, nil
	}
	res, err := searchNative(ctx, root, p, ig)
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

func searchNative(ctx context.Context, root string, p SearchParams, igOpts IgnoreOptions) (SearchResult, error) {
	re, err := compilePattern(p)
	if err != nil {
		return SearchResult{}, err
	}
	max := effectiveMax(p.MaxMatches)

	out := SearchResult{Matches: []SearchMatch{}}
	ig := newIgnoreEngine(root, igOpts)

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
//
// The cap is 500 CHARACTERS, cut on a rune boundary. A byte slice at 500
// lands inside a multi-byte rune one time in a few, and the result is
// invalid UTF-8 that a JSON encoder replaces with U+FFFD — a line the model
// then cannot match back against the file.
func capLine(s string) string {
	if len(s) <= SearchLineChars {
		return s // at most 500 bytes is at most 500 runes
	}
	n := 0
	for i := range s {
		if n == SearchLineChars {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// CountCandidates counts the files a search would select: everything left
// after the ignore rules, the hidden-entry rule and file_glob.
func CountCandidates(ctx context.Context, root string, p SearchParams) (int, error) {
	return countCandidates(ctx, root, p, IgnoreOptions{})
}

func countCandidates(ctx context.Context, root string, p SearchParams, igOpts IgnoreOptions) (int, error) {
	n := 0
	ig := newIgnoreEngine(root, igOpts)
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
func searchRipgrep(ctx context.Context, rg, root string, p SearchParams, ig IgnoreOptions) (SearchResult, error) {
	max := effectiveMax(p.MaxMatches)

	args := []string{
		"--json",
		"--regex-size-limit", "10M",
		// ripgrep only honours .gitignore inside a git repository; our declared
		// semantics do not require one, and without this a search under a
		// plain directory returns node_modules on the accelerated path and not
		// on the native one.
		"--no-require-git",
		// Parity with the native engine's ignore SOURCES (REQ-TOOL-05.2): it
		// reads the global excludes file, .git/info/exclude and the .gitignore
		// files from the search root DOWN. rg additionally walks .gitignore
		// files in the root's PARENT directories and honours .ignore files,
		// neither of which the native path reads — so a search rooted in a
		// subdirectory returned different files depending on which backend
		// answered. The global layer is passed explicitly below, so rg's own
		// lookup of it is turned off too.
		"--no-ignore-parent", "--no-ignore-dot", "--no-ignore-global",
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
	// The global excludes layer is handed to rg EXPLICITLY. rg runs with an
	// empty environment (below), so it cannot locate core.excludesFile or
	// ~/.config/git/ignore itself — which meant the accelerated path silently
	// applied no global layer while the native path did, and an injected
	// layer (NFR-TEST-04) reached one backend but not the other. --ignore-file
	// has the lowest precedence in rg's ignore stack, matching ours.
	if g := globalExcludesPath(ig); g != "" {
		if _, err := os.Stat(g); err == nil {
			args = append(args, "--ignore-file", g)
		}
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

	// rg's own context, so it can be stopped the moment the result is full.
	// Without this a search for a common word over a large tree read every
	// match rg could find, at max_matches=1.
	rgCtx, stopRG := context.WithCancel(ctx)
	defer stopRG()
	cmd := exec.CommandContext(rgCtx, rg, args...)
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

	res, stopped, parseErr := parseRipgrepJSON(stdout, p, max)
	if stopped || parseErr != nil {
		// Done reading early: kill rg rather than let it finish a search
		// nobody will read. Wait then closes the pipe; the rule that reads
		// must finish before Wait is about not LOSING output, and here the
		// rest of the output is unwanted by construction.
		stopRG()
	}
	waitErr := cmd.Wait()

	if parseErr != nil {
		return SearchResult{}, parseErr
	}
	// One extra walk, with no file contents read: readdir plus ignore matching
	// is cheap next to the content scan rg just did, and it is the only way the
	// two backends report the same number.
	n, cerr := countCandidates(ctx, root, p, ig)
	if cerr != nil && ctx.Err() != nil {
		return SearchResult{}, cerr
	}
	res.FilesSearched = n

	if waitErr != nil {
		if stopped {
			return res, nil // killed by us, at max_matches: the result is complete
		}
		var ee *exec.ExitError
		// Exit 1 is "no matches", which is a result and not a failure. Exit 2
		// is an error and carries a reason on stderr — but rg reports 2 when
		// ANY file could not be read even if others matched, and the native
		// path skips an unreadable file rather than failing, so matches that
		// arrived alongside the error are kept.
		if errors.As(waitErr, &ee) && (ee.ExitCode() == 1 && len(res.Matches) == 0 ||
			ee.ExitCode() == 2 && len(res.Matches) > 0) {
			return res, nil
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

// parseRipgrepJSON reads rg's event stream. It stops READING once max matches
// are in hand and the file they came from has ended — the after-context for
// the last match arrives before that file's "end" event — and reports that it
// stopped, so the caller can kill rg rather than wait for it.
func parseRipgrepJSON(r interface{ Read([]byte) (int, error) }, p SearchParams, max int) (res SearchResult, stopped bool, err error) {
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
	for !stopped && sc.Scan() {
		var ev rgEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return SearchResult{}, false, fmt.Errorf("ripgrep json: %w", err)
		}
		switch ev.Type {
		case "begin":
			attach()
			curFile = normalizeRGPath(ev.Data.Path.Text)
			if out.Truncated {
				stopped = true // the full result's last file has ended
			}
		case "end":
			attach()
			if out.Truncated {
				stopped = true
			}
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
				if p.ContextLines == 0 {
					stopped = true // nothing further to attach
				}
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
		return SearchResult{}, false, err
	}
	sortMatches(out.Matches)
	return out, stopped, nil
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
