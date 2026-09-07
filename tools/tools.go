package tools

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/imagex"
	"github.com/agentfox/agentkit-go/schema"
)

// Options configures the built-in tool set.
type Options struct {
	Workspace *Workspace
	// SpillDir is where oversized subprocess output is streamed in full
	// (REQ-TOOL-09d). Empty means a per-workspace subdirectory of
	// os.TempDir(), created on first spill. Spill files are the EMBEDDER'S to
	// clean up: the SDK never deletes one, because the path is handed to the
	// model and to the audit trail, and either may still need it after the
	// call that produced it has returned.
	SpillDir string
	// DisableSpill turns spilling off entirely, for a caller that wants no
	// file left behind. Without it the requirement's "additionally streamed
	// to a temp file" is on by default, since a default that silently
	// dropped the full output would make the marker name a file that does
	// not exist.
	DisableSpill bool
	// Env replaces the subprocess environment. Nil means ReducedEnv(nil)
	// (REQ-SEC-08) — for every constructor here, not only All(): a tool built
	// by hand with a nil Env must not inherit the parent's credentials just
	// because it skipped the aggregate.
	Env []string
	// Ignore injects the environment the ignore engine reads. The zero value
	// reads the real one; NoGlobalExcludes() pins an empty global layer
	// (NFR-TEST-04).
	Ignore IgnoreOptions
}

// withDefaults applies the documented zero-value meanings. Every constructor
// calls it, so the defaults hold for a tool built outside All() too.
func (o Options) withDefaults() Options {
	if o.Env == nil {
		o.Env = ReducedEnv(nil)
	}
	if o.DisableSpill {
		o.SpillDir = ""
	} else if o.SpillDir == "" {
		o.SpillDir = defaultSpillDir(workspaceRoot(o))
	}
	return o
}

// defaultSpillDir names the per-workspace spill directory under os.TempDir().
// The workspace is identified by a hash rather than its basename so two
// checkouts called "app" do not share a directory, and so the name is safe on
// every platform in the NFR-COMPAT-06 matrix.
func defaultSpillDir(root string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(root))
	return filepath.Join(os.TempDir(), fmt.Sprintf("agentkit-spill-%08x", h.Sum32()))
}

// All returns the default built-in tool set.
//
// `fetch_url` is deliberately NOT here (REQ-TOOL-07). Reaching it takes two
// affirmative acts, not one:
//
//	cfg.ToolPolicy.CustomTools = append(cfg.ToolPolicy.CustomTools,
//	    tools.FetchTool(tools.FetchOptions{}))
//	// and, if an allowlist is in use, name it there too:
//	cfg.ToolPolicy.ToolNames = append(cfg.ToolPolicy.ToolNames, "fetch_url")
//
// A tool that makes outbound requests on the model's behalf is a different
// risk class from one that reads a file inside a workspace root, and an
// embedder should have to say so.
func All(opts Options) ([]core.Tool, error) {
	if opts.Workspace == nil {
		ws, err := NewWorkspace("")
		if err != nil {
			return nil, err
		}
		opts.Workspace = ws
	}
	opts = opts.withDefaults()
	fs := newFileTools(opts)
	return []core.Tool{
		fs.readFile(),
		fs.writeFile(),
		fs.editFile(),
		fs.listFiles(),
		fs.findFiles(),
		fs.searchFiles(),
		executeTool(opts),
		runCommandTool(opts),
		PowerShell(opts),
	}, nil
}

// FileNavigationTools names REQ-TOOL-04e's opt-in trio.
//
// They are in All() because All() is the DEFAULT set, and the requirement's
// "opt-in" is about the tool policy of REQ-TOOL-10 rather than about this
// constructor: an embedder scopes a run down with ToolPolicy, and a set that
// omitted them by default would make the common case the one you have to
// remember. What the requirement actually turns on is their ABSENCE from the
// resolved set, which is the prompt builder's business, not this list's.
func FileNavigationTools() []string {
	return []string{"list_files", "find_files", "search_files"}
}

// ExecuteFallbackGuideline is REQ-TOOL-04e's sentence, verbatim.
//
// It is not a PromptGuidelines entry on any tool, because its condition is
// that those tools are MISSING — a per-tool field can only fire when its tool
// is present, which is the opposite of what the requirement asks for. The
// prompt builder emits it.
const ExecuteFallbackGuideline = "Use execute for file operations like ls, rg, find."

// ---------------------------------------------------------------- path locks

// pathLocks is REQ-LOOP-12's file mutation queue: a REFCOUNTED per-path mutex
// keyed on the SYMLINK-RESOLVED absolute path.
//
// Keying on the resolved path is what makes two spellings of one file share a
// lock — `./x.go`, `x.go` and a symlink pointing at it are one file and must
// serialize, while operations on genuinely different files stay concurrent.
//
// Entries are released at refcount zero so the table cannot grow unbounded
// over a long session.
type pathLocks struct {
	mu sync.Mutex
	m  map[string]*lockEntry
}

type lockEntry struct {
	mu  sync.Mutex
	ref int
}

func newPathLocks() *pathLocks { return &pathLocks{m: map[string]*lockEntry{}} }

func (p *pathLocks) acquire(path string) func() {
	p.mu.Lock()
	e, ok := p.m[path]
	if !ok {
		e = &lockEntry{}
		p.m[path] = e
	}
	e.ref++
	p.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		p.mu.Lock()
		e.ref--
		if e.ref == 0 {
			delete(p.m, path)
		}
		p.mu.Unlock()
	}
}

// lockKey resolves a path for locking. For a file that does not exist yet it
// resolves the PARENT and rejoins the base, so a not-yet-created file under a
// symlinked directory still shares a lock with its other spellings (ruling
// P-48).
//
// Only NOT-EXIST falls back to the absolute path (REQ-LOOP-12). Any other
// resolution error — permission denied, a link loop, an I/O fault — is
// returned, because a key computed from a path that could not be resolved is
// a key two spellings of one file might not share, which is the lost update
// the lock exists to prevent.
func lockKey(path string) (string, error) {
	r, err := filepath.EvalSymlinks(path)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("tools: cannot resolve %s for locking: %w", path, err)
	}
	dir, base := filepath.Split(path)
	r, err = filepath.EvalSymlinks(filepath.Clean(dir))
	if err == nil {
		return filepath.Join(r, base), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("tools: cannot resolve %s for locking: %w", path, err)
	}
	return path, nil
}

// ---------------------------------------------------------------- file tools

type fileTools struct {
	ws    *Workspace
	locks *pathLocks
	// ig carries the ignore environment with the `git config` lookup memoized
	// per workspace, so find_files and search_files do not spawn git per call.
	ig IgnoreOptions
}

func newFileTools(opts Options) *fileTools {
	return &fileTools{ws: opts.Workspace, locks: newPathLocks(), ig: opts.Ignore.cached()}
}

func (f *fileTools) readFile() core.Tool {
	return core.Tool{
		Name: "read_file",
		Description: "Read a file. Text is returned as at most 2000 lines or 50KB, " +
			"whichever comes first; an image is returned as a note plus the image itself.",
		Builtin: true,
		InputSchema: schema.Object(
			schema.Prop("path", schema.String("Path to the file (relative to the workspace, or absolute)")),
			schema.Opt("offset", schema.Int("1-based line to start from (default 1)")),
			schema.Opt("limit", schema.Int("Maximum lines to return")),
		),
		PromptGuidelines: []string{"Read a file before editing it."},
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			abs, err := f.ws.Resolve(a.Path)
			if err != nil {
				return core.ErrResult("path_not_allowed", err.Error())
			}
			fh, err := os.Open(abs)
			if err != nil {
				return core.ErrResult("read_failed", err.Error())
			}
			defer fh.Close()
			br := bufio.NewReaderSize(fh, 64<<10)

			// REQ-TOOL-14.6: images are detected by MAGIC BYTES, never by
			// extension. `screenshot.txt` is still a PNG if its first eight
			// bytes say so, and splitting one into "lines" hands the model
			// several kilobytes of mojibake. Only the header is peeked; the
			// whole file is loaded for an image alone, whose size the
			// normalizer bounds.
			head, _ := br.Peek(imageSniffBytes)
			if mime, isImage := imagex.Sniff(head); isImage {
				data, err := io.ReadAll(br)
				if err != nil {
					return core.ErrResult("read_failed", err.Error())
				}
				return readImage(abs, a.Path, data, mime)
			}

			// 1-based, with 0 aliased to 1 (ruling P-21).
			from := a.Offset
			if from <= 0 {
				from = 1
			}
			limit := a.Limit
			if limit <= 0 || limit > ReadLineLimit {
				limit = ReadLineLimit
			}
			page, err := readLines(br, from, limit, DefaultByteLimit)
			if err != nil {
				return core.ErrResult("read_failed", err.Error())
			}
			// An empty file has no lines, and reading it from the start is
			// not an error: the answer is an empty content, not a rejection.
			if from > page.total && !(from == 1 && page.total == 0) {
				return core.ErrResult("offset_past_end",
					fmt.Sprintf("offset %d is past the end of the file (%d lines)", from, page.total))
			}

			shown := f.ws.Rel(abs)
			body := make([]string, 0, len(page.lines)+2)
			for _, l := range page.lines {
				if l.long {
					// REQ-TOOL-09c: a single line over the byte limit is
					// replaced IN PLACE by a marker naming the shell
					// workaround, and the read continues past it. Cutting the
					// line would hand the model a fragment with no way to tell
					// where it ended.
					body = append(body, LongLineMarker(l.n, l.size, DefaultByteLimit, shown))
					continue
				}
				body = append(body, l.text)
			}
			md := &core.ToolMetadata{TotalLines: int64(page.total), TotalBytes: page.totalBytes}
			if page.truncatedBy != "" {
				md.Truncated = true
				md.TruncatedBy = string(page.truncatedBy)
			}
			if page.to < page.total {
				// One marker, carrying the REAL continuation offset, whichever
				// limit fired (REQ-TOOL-09b). The byte cut is taken on whole
				// lines, so the offset it names is the next unseen line.
				body = append(body, ReadOffsetMarker(from, page.to, page.total))
			}
			r := core.OKResult(map[string]any{"content": strings.Join(body, "\n"), "encoding": "utf-8"})
			r.Metadata = md
			return r
		},
	}
}

// imageSniffBytes is how much of a file's head imagex.Sniff needs. The longest
// signature it knows (RIFF....WEBP) is twelve bytes.
const imageSniffBytes = 16

// readPage is what readLines returns: the selected window, and the counts the
// markers need.
type readPage struct {
	lines       []readLine
	to          int // last line number shown, 0 if none
	total       int
	totalBytes  int64
	truncatedBy TruncatedBy
}

type readLine struct {
	n    int
	text string
	// long marks a line whose size alone exceeds the byte budget. Its text is
	// not kept; the caller emits REQ-TOOL-09c's marker in its place.
	long bool
	size int64
}

// readLines STREAMS the file, keeping only the lines in [from, from+limit)
// that fit within maxBytes, and counting the rest. Memory is bounded by the
// window plus one buffer, not by the file: the previous implementation loaded
// the whole file and split it, which made a read of the first ten lines of a
// gigabyte log a gigabyte allocation.
//
// Lines are counted the way an editor counts them: "a\nb\n" is two lines,
// not three. strings.Split's trailing empty element used to inflate every
// total by one and reject offset=N for an N-line file (REQ-TOOL-09b's marker
// named a line that did not exist).
//
// The byte budget is applied on WHOLE lines, so no line is ever cut mid-rune
// and the continuation offset the marker names is exactly the first unseen
// line. A single line over the budget is reported as `long` rather than
// shown; the caller replaces it with REQ-TOOL-09c's marker and continues.
func readLines(br *bufio.Reader, from, limit, maxBytes int) (readPage, error) {
	var (
		page readPage
		used int
		done bool // the window is closed; only counting continues
	)
	for {
		// Only a line that could be shown needs its bytes retained; a line
		// before the window or after it is measured, not kept — retaining
		// even a few bytes of each is an allocation per line, which over a
		// million-line log is the file all over again.
		keep := 0
		if !done && page.total+1 >= from {
			keep = maxBytes + 1
		}
		line, size, terminated, err := readLineBounded(br, keep)
		if err != nil {
			return readPage{}, err
		}
		if !terminated && size == 0 {
			break // EOF after a terminated line: no trailing line
		}
		page.total++
		page.totalBytes += size
		if terminated {
			page.totalBytes++
		}
		n := page.total
		if !done && n >= from {
			switch {
			case n-from >= limit:
				page.truncatedBy = TruncatedByLines
				done = true
			case size > int64(maxBytes):
				page.lines = append(page.lines, readLine{n: n, long: true, size: size})
				page.to = n
				page.truncatedBy = TruncatedByBytes
			case used+len(line)+1 > maxBytes && len(page.lines) > 0:
				page.truncatedBy = TruncatedByBytes
				done = true
			default:
				page.lines = append(page.lines, readLine{n: n, text: string(line)})
				page.to = n
				used += len(line) + 1
			}
		}
		if !terminated {
			break
		}
	}
	if page.truncatedBy == TruncatedByLines && page.to >= page.total {
		page.truncatedBy = "" // the limit landed exactly on the last line
	}
	return page, nil
}

// readLineBounded reads one line, keeping at most keep bytes of it and
// counting all of them. A line longer than the reader's buffer arrives in
// ErrBufferFull-terminated slices, which is what lets a multi-megabyte line be
// measured without being held.
func readLineBounded(br *bufio.Reader, keep int) (line []byte, size int64, terminated bool, err error) {
	for {
		chunk, rerr := br.ReadSlice('\n')
		size += int64(len(chunk))
		if room := keep - len(line); room > 0 {
			line = append(line, chunk[:min(room, len(chunk))]...)
		}
		switch {
		case rerr == nil:
			size-- // the terminator is not part of the line
			if len(line) > 0 && line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			return line, size, true, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case errors.Is(rerr, io.EOF):
			return line, size, false, nil
		default:
			return nil, 0, false, rerr
		}
	}
}

// readImage returns REQ-TOOL-14.6's "text note plus an ImageBlock".
//
// The note matters as much as the block: without it the model sees an image
// appear with no statement of what was read, and cannot tell a screenshot it
// asked for from one a previous turn left in history.
func readImage(abs, shown string, data []byte, mime string) core.ToolResult {
	// WebP is forwarded UNTOUCHED (REQ-TOOL-14). Providers accept it, but the
	// standard library cannot decode it, so the normalizer reports it as
	// unsupported — and refusing it here would turn "this build cannot
	// downscale it" into "the model cannot see it". Dimensions are unknown;
	// the note says so rather than inventing them. The one thing that can be
	// checked is the byte budget, since an oversized WebP cannot be shrunk.
	if mime == imagex.MIMEWebP {
		if !imagex.FitsBudget(len(data)) {
			return core.ErrResult("unsupported_image", fmt.Sprintf(
				"%s: WebP image of %d bytes exceeds the provider's inline limit and "+
					"cannot be downscaled by this build; re-encode it smaller", shown, len(data)))
		}
		out := core.OKResult(map[string]any{
			"note":      fmt.Sprintf("[%s: %s image, dimensions unknown]", shown, mime),
			"mime_type": mime,
		})
		out.Blocks = []core.ContentBlock{core.ImageBlock{
			Data: base64.StdEncoding.EncodeToString(data), MimeType: mime}}
		return out
	}
	// Formats providers reject are refused HERE, with a message naming the
	// problem, rather than forwarded. Forwarded, the failure lands on the next
	// provider request — by which time the image is in history and every
	// subsequent request fails the same way.
	//
	// Normalize validates before it does anything else, so there is no
	// separate Validate call: a second one would be unreachable code that
	// looks like a safety check.
	res, err := imagex.Normalize(data, mime)
	if err != nil {
		return core.ErrResult("unsupported_image", err.Error())
	}

	note := fmt.Sprintf("[%s: %s image, %d×%d]", shown, res.MIMEType, res.Width, res.Height)
	if res.Changed {
		note = fmt.Sprintf("[%s: %s image, downscaled to %d×%d for the provider's inline limit]",
			shown, res.MIMEType, res.Width, res.Height)
	}
	out := core.OKResult(map[string]any{
		"note":      note,
		"mime_type": res.MIMEType,
		"width":     res.Width,
		"height":    res.Height,
	})
	out.Blocks = []core.ContentBlock{core.ImageBlock{Data: res.Base64(), MimeType: res.MIMEType}}
	return out
}

func (f *fileTools) writeFile() core.Tool {
	return core.Tool{
		Name:        "write_file",
		Description: "Write a file, creating or replacing it.",
		Builtin:     true,
		// Sequential: a write has workspace-wide side effects, and one
		// Sequential tool demotes the whole batch (REQ-LOOP-05a).
		ExecutionMode: core.Sequential,
		InputSchema: schema.Object(
			schema.Prop("path", schema.String("Path to write")),
			schema.Prop("content", schema.String("Full file content")),
		),
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			abs, err := f.ws.Resolve(a.Path)
			if err != nil {
				return core.ErrResult("path_not_allowed", err.Error())
			}
			key, err := lockKey(abs)
			if err != nil {
				return core.ErrResult("lock_failed", err.Error())
			}
			release := f.locks.acquire(key)
			defer release()

			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return core.ErrResult("write_failed", err.Error())
			}
			// Re-checked under the lock, immediately before the open
			// (REQ-SEC-01): a link planted here since Resolve would be
			// followed by WriteFile.
			if err := f.ws.CheckWriteTarget(abs); err != nil {
				return core.ErrResult("path_not_allowed", err.Error())
			}
			if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
				return core.ErrResult("write_failed", err.Error())
			}
			return core.OKResult(map[string]any{"written": true, "bytes": len(a.Content)})
		},
	}
}

func (f *fileTools) editFile() core.Tool {
	return core.Tool{
		Name:    "edit_file",
		Builtin: true,
		Description: "Apply one or more exact-match edits to a file. " +
			"Every old_string is matched against the ORIGINAL file content, not against " +
			"the result of an earlier edit in the same call. Each old_string must appear " +
			"exactly once; if it appears more than once the call is rejected, so include " +
			"enough surrounding context to make it unique.",
		ExecutionMode: core.Sequential,
		InputSchema: schema.Object(
			schema.Prop("path", schema.String("Path to edit")),
			schema.Prop("edits", schema.Array(
				schema.Object(
					schema.Prop("old_string", schema.String("Exact text to replace; must be unique in the file")),
					schema.Prop("new_string", schema.String("Replacement text")),
				), "Edits to apply, all matched against the original content")),
		),
		// PrepareArguments repairs the three shapes models actually emit
		// (REQ-TOOL-11.1). Each was observed, not imagined.
		PrepareArguments: repairEditArgs,
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				Path  string `json:"path"`
				Edits []Edit `json:"edits"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			abs, err := f.ws.Resolve(a.Path)
			if err != nil {
				return core.ErrResult("path_not_allowed", err.Error())
			}
			key, err := lockKey(abs)
			if err != nil {
				return core.ErrResult("lock_failed", err.Error())
			}
			release := f.locks.acquire(key)
			defer release()

			raw, err := os.ReadFile(abs)
			if err != nil {
				return core.ErrResult("read_failed", err.Error())
			}
			content, bom, ending := NormalizeForEdit(string(raw))

			out, n, err := ApplyEdits(content, a.Edits)
			if err != nil {
				var ee *EditError
				if ok := asEditError(err, &ee); ok {
					return core.ErrResult("edit_"+ee.Phase, ee.Text)
				}
				return core.ErrResult("edit_failed", err.Error())
			}
			// Same re-check as write_file, for the same reason (REQ-SEC-01).
			if err := f.ws.CheckWriteTarget(abs); err != nil {
				return core.ErrResult("path_not_allowed", err.Error())
			}
			if err := os.WriteFile(abs, []byte(Restore(out, bom, ending)), 0o644); err != nil {
				return core.ErrResult("write_failed", err.Error())
			}
			r := core.OKResult(map[string]any{"edits_applied": n})
			r.Metadata = &core.ToolMetadata{LineEnding: string(ending)}
			return r
		},
	}
}

func asEditError(err error, out **EditError) bool {
	if e, ok := err.(*EditError); ok {
		*out = e
		return true
	}
	return false
}

// repairEditArgs handles the three malformed shapes models emit for edit_file
// (REQ-TOOL-11.1):
//
//  1. edits delivered as a JSON STRING instead of an array
//  2. a bare {old_string, new_string} object instead of a one-element array
//  3. legacy top-level old_string/new_string keys
//
// Repairing them here rather than rejecting saves a turn each time, and the
// alternative — widening the schema to accept all three — would make the
// schema itself lie about the contract.
func repairEditArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}

	// (1) a JSON string where an array belongs
	if s, ok := out["edits"].(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			out["edits"] = parsed
		}
	}
	// (2) a bare object instead of a one-element array
	if m, ok := out["edits"].(map[string]any); ok {
		out["edits"] = []any{m}
	}
	// (3) legacy top-level keys
	if _, hasEdits := out["edits"]; !hasEdits {
		oldS, hasOld := out["old_string"]
		newS, hasNew := out["new_string"]
		if hasOld && hasNew {
			out["edits"] = []any{map[string]any{"old_string": oldS, "new_string": newS}}
			delete(out, "old_string")
			delete(out, "new_string")
		}
	}
	return out
}

func (f *fileTools) listFiles() core.Tool {
	return core.Tool{
		Name:        "list_files",
		Description: "List the entries of a directory.",
		Builtin:     true,
		InputSchema: schema.Object(
			schema.Opt("path", schema.String("Directory to list (default the workspace root)")),
			schema.Opt("limit", schema.Int("Maximum entries")),
		),
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				Path  string `json:"path"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			if a.Path == "" {
				a.Path = "."
			}
			abs, err := f.ws.Resolve(a.Path)
			if err != nil {
				return core.ErrResult("path_not_allowed", err.Error())
			}
			des, err := os.ReadDir(abs)
			if err != nil {
				return core.ErrResult("list_failed", err.Error())
			}
			limit := a.Limit
			if limit <= 0 || limit > ListEntryCap {
				limit = ListEntryCap
			}
			entries := make([]string, 0, len(des))
			for _, de := range des {
				n := de.Name()
				if de.IsDir() {
					n += "/"
				}
				entries = append(entries, n)
			}
			sort.Strings(entries)
			truncated := len(entries) > limit
			r := core.OKResult(map[string]any{"entries": entries, "truncated": truncated})
			if truncated {
				entries = entries[:limit]
				r.Data["entries"] = entries
				// The marker is a separate key, as in find_files. Appended to
				// `entries` it was indistinguishable from a file called
				// "[500 entries limit reached...]" (REQ-TOOL-09b).
				r.Data["note"] = ListMarker(limit)
				r.Metadata = &core.ToolMetadata{Truncated: true, TruncatedBy: string(TruncatedByLines)}
			}
			return r
		},
	}
}

func (f *fileTools) findFiles() core.Tool {
	return core.Tool{
		Name:        "find_files",
		Description: "Find files by glob pattern, skipping .gitignored paths.",
		Builtin:     true,
		InputSchema: schema.Object(
			schema.Prop("pattern", schema.String("Glob pattern, e.g. **/*.go")),
			schema.Opt("path", schema.String("Directory to search from (default the workspace root)")),
			schema.Opt("file_type", schema.String("What to match: \"file\" (default), \"dir\" or \"any\"")),
			schema.Opt("limit", schema.Int("Maximum results")),
		),
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				Pattern  string `json:"pattern"`
				Path     string `json:"path"`
				FileType string `json:"file_type"`
				Limit    int    `json:"limit"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			if a.Path == "" {
				a.Path = "."
			}
			// REQ-TOOL-04's table: file_type string (default "file").
			// "directory" is accepted as a spelling of "dir" because models
			// emit both.
			wantFiles, wantDirs := true, false
			switch strings.ToLower(strings.TrimSpace(a.FileType)) {
			case "", "file":
			case "dir", "directory":
				wantFiles, wantDirs = false, true
			case "any":
				wantDirs = true
			default:
				return core.ErrResult("invalid_arguments",
					fmt.Sprintf("file_type must be \"file\", \"dir\" or \"any\", not %q", a.FileType))
			}
			root, err := f.ws.Resolve(a.Path)
			if err != nil {
				return core.ErrResult("path_not_allowed", err.Error())
			}
			limit := a.Limit
			if limit <= 0 || limit > FindResultCap {
				limit = FindResultCap
			}
			ig := newIgnoreEngine(root, f.ig)
			var found []string
			truncated := false
			add := func(rel string) error {
				if len(found) >= limit {
					truncated = true
					return filepath.SkipAll
				}
				found = append(found, rel)
				return nil
			}
			err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return nil // unreadable entries are skipped, not fatal
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				rel, rerr := filepath.Rel(root, p)
				if rerr != nil || rel == "." {
					return nil
				}
				if ig.match(rel, d.IsDir()) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					// Load this directory's own ignore files before its
					// children are matched (REQ-TOOL-05.2). WalkDir visits a
					// directory before descending, which is what makes a
					// single forward pass sufficient — there is no need to
					// pre-scan for .gitignore files that may not exist.
					ig.enter(rel, p)
					if wantDirs && MatchGlob(a.Pattern, rel) {
						// Suffixed like list_files, so a directory is
						// distinguishable from a file under file_type=any.
						return add(rel + "/")
					}
					return nil
				}
				if wantFiles && MatchGlob(a.Pattern, rel) {
					return add(rel)
				}
				return nil
			})
			if err != nil && ctx.Err() != nil {
				return core.ErrResult("aborted", "Operation aborted")
			}
			sort.Strings(found)
			data := map[string]any{"files": found, "truncated": truncated}
			r := core.OKResult(data)
			if truncated {
				data["marker"] = FindMarker(limit)
				r.Metadata = &core.ToolMetadata{Truncated: true, TruncatedBy: string(TruncatedByLines)}
			}
			return r
		},
	}
}

// ------------------------------------------------------------------- execute

func executeTool(opts Options) core.Tool {
	opts = opts.withDefaults()
	return core.Tool{
		Name:    "execute",
		Builtin: true,
		Description: "Run a shell command. Pipes, redirection, && and $() all work. " +
			"Output is truncated from the END if it is large, so the tail of a failing " +
			"build is preserved.",
		// Sequential: a command has process-wide side effects.
		ExecutionMode: core.Sequential,
		InputSchema: schema.Object(
			schema.Prop("command", schema.String("The shell command to run")),
			schema.Opt("timeout_s", schema.Int("Seconds before the process tree is killed")),
		),
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				Command  string `json:"command"`
				TimeoutS *int   `json:"timeout_s"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			if strings.TrimSpace(a.Command) == "" {
				return core.ErrResult("invalid_arguments", "command is empty")
			}
			timeout, err := timeoutArg(a.TimeoutS)
			if err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			res, err := Run(ctx, a.Command, ExecOptions{
				Dir:      workspaceRoot(opts),
				Timeout:  timeout,
				MaxBytes: DefaultByteLimit,
				SpillDir: opts.SpillDir,
				Env:      opts.Env,
			})
			if err != nil {
				return core.ErrResult("exec_failed", err.Error())
			}
			return execResultToTool(res)
		},
	}
}

// timeoutArg validates REQ-TOOL-06's timeout_s: optional with NO default, and
// when supplied it must be POSITIVE. The argument is a pointer so that a
// supplied 0 is distinguishable from an absent one; with a plain int a model
// that sent timeout_s=0 — or a negative number — silently got "no timeout",
// which is the one thing a caller who typed a timeout did not ask for.
func timeoutArg(s *int) (time.Duration, error) {
	if s == nil {
		return 0, nil
	}
	if *s <= 0 {
		return 0, fmt.Errorf("timeout_s must be positive when supplied, got %d", *s)
	}
	return time.Duration(*s) * time.Second, nil
}

// execResultToTool is the REQ-TOOL-08 envelope for a subprocess result.
//
// Shared by execute, run_command and powershell: the envelope is a property of
// having run a subprocess, not of how the command was spelled, and three
// copies would drift on the next field added to ToolMetadata.
func execResultToTool(res ExecResult) core.ToolResult {
	code := res.ExitCode
	md := &core.ToolMetadata{
		Truncated:  res.Truncated,
		TotalBytes: res.TotalBytes,
		SpillPath:  res.SpillPath,
		DurationMS: res.Duration.Milliseconds(),
		ExitCode:   &code,
		Outcome:    string(res.Outcome),
	}
	if res.Truncated {
		md.TruncatedBy = string(TruncatedByBytes)
	}
	out := core.ToolResult{
		OK:       res.Outcome == OutcomeOK,
		Data:     map[string]any{"output": res.Output, "exit_code": code, "outcome": string(res.Outcome)},
		Metadata: md,
	}
	if !out.OK {
		out.Error = "command_" + string(res.Outcome)
	}
	return out
}

// runCommandTool is REQ-TOOL-06's structured variant.
//
// The model picks between this and `execute` by NAME, which is the point: a
// shell-features argument on one tool would be a choice the model gets wrong
// silently. Here there is no shell at all, so a path with a space or a
// semicolon in it is just an argument.
func runCommandTool(opts Options) core.Tool {
	opts = opts.withDefaults()
	return core.Tool{
		Name:    "run_command",
		Builtin: true,
		Description: "Run a program with an explicit argument list and NO shell. " +
			"Pipes, redirection, globs and $() do not work here — use execute for those. " +
			"Prefer this when arguments come from data, since nothing is re-parsed.",
		ExecutionMode: core.Sequential,
		InputSchema: schema.Object(
			schema.Prop("argv", schema.Array(schema.String(),
				"Program and arguments, e.g. [\"git\", \"commit\", \"-m\", \"a message\"]")),
			schema.Opt("timeout_s", schema.Int("Seconds before the process tree is killed")),
		),
		PromptGuidelines: []string{
			"Use run_command when an argument contains spaces or shell metacharacters; " +
				"nothing is re-parsed by a shell.",
		},
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				Argv     []string `json:"argv"`
				TimeoutS *int     `json:"timeout_s"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			if len(a.Argv) == 0 || strings.TrimSpace(a.Argv[0]) == "" {
				return core.ErrResult("invalid_arguments",
					"argv must name a program as its first element")
			}
			timeout, err := timeoutArg(a.TimeoutS)
			if err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			res, err := RunArgv(ctx, a.Argv, ExecOptions{
				Dir:      workspaceRoot(opts),
				Timeout:  timeout,
				MaxBytes: DefaultByteLimit,
				SpillDir: opts.SpillDir,
				Env:      opts.Env,
			})
			if err != nil {
				return core.ErrResult("exec_failed", err.Error())
			}
			return execResultToTool(res)
		},
	}
}
