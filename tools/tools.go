package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
)

// Options configures the built-in tool set.
type Options struct {
	Workspace *Workspace
	// SpillDir is where oversized execute output is streamed. Empty disables
	// spilling.
	SpillDir string
	// Env replaces the subprocess environment. Nil means ReducedEnv(nil).
	Env []string
}

// All returns the default built-in tool set.
//
// `fetch_url` is deliberately NOT here: REQ-TOOL-07 puts it outside the
// default set, reachable only by naming it in ToolPolicy.ToolNames.
func All(opts Options) ([]core.Tool, error) {
	if opts.Workspace == nil {
		ws, err := NewWorkspace("")
		if err != nil {
			return nil, err
		}
		opts.Workspace = ws
	}
	if opts.Env == nil {
		opts.Env = ReducedEnv(nil)
	}
	fs := &fileTools{ws: opts.Workspace, locks: newPathLocks()}
	return []core.Tool{
		fs.readFile(),
		fs.writeFile(),
		fs.editFile(),
		fs.listFiles(),
		fs.findFiles(),
		executeTool(opts),
	}, nil
}

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
func lockKey(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	dir, base := filepath.Split(path)
	if r, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		return filepath.Join(r, base)
	}
	return path
}

// ---------------------------------------------------------------- file tools

type fileTools struct {
	ws    *Workspace
	locks *pathLocks
}

func (f *fileTools) readFile() core.Tool {
	return core.Tool{
		Name:        "read_file",
		Description: "Read a text file. Returns at most 2000 lines or 50KB, whichever comes first.",
		Builtin:     true,
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
			data, err := os.ReadFile(abs)
			if err != nil {
				return core.ErrResult("read_failed", err.Error())
			}

			lines := strings.Split(string(data), "\n")
			total := len(lines)
			// 1-based, with 0 aliased to 1 (ruling P-21).
			from := a.Offset
			if from <= 0 {
				from = 1
			}
			if from > total {
				return core.ErrResult("offset_past_end",
					fmt.Sprintf("offset %d is past the end of the file (%d lines)", from, total))
			}
			limit := a.Limit
			if limit <= 0 || limit > ReadLineLimit {
				limit = ReadLineLimit
			}
			to := from + limit - 1
			if to > total {
				to = total
			}
			body := strings.Join(lines[from-1:to], "\n")

			md := &core.ToolMetadata{TotalLines: int64(total), TotalBytes: int64(len(data))}
			if to < total {
				md.Truncated = true
				md.TruncatedBy = string(TruncatedByLines)
				body += "\n" + ReadOffsetMarker(from, to, total)
			}
			if len(body) > DefaultByteLimit {
				body = body[:DefaultByteLimit] + "\n" +
					fmt.Sprintf("[truncated at %s]", humanBytes(int64(DefaultByteLimit)))
				md.Truncated = true
				md.TruncatedBy = string(TruncatedByBytes)
			}
			r := core.OKResult(map[string]any{"content": body, "encoding": "utf-8"})
			r.Metadata = md
			return r
		},
	}
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
			release := f.locks.acquire(lockKey(abs))
			defer release()

			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return core.ErrResult("write_failed", err.Error())
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
			release := f.locks.acquire(lockKey(abs))
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
		PromptGuidelines: []string{"Use execute for file operations like ls, rg, find."},
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
			if truncated {
				entries = entries[:limit]
				entries = append(entries, ListMarker(limit))
			}
			r := core.OKResult(map[string]any{"entries": entries, "truncated": truncated})
			if truncated {
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
			schema.Opt("limit", schema.Int("Maximum results")),
		),
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
				Limit   int    `json:"limit"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			if a.Path == "" {
				a.Path = "."
			}
			root, err := f.ws.Resolve(a.Path)
			if err != nil {
				return core.ErrResult("path_not_allowed", err.Error())
			}
			limit := a.Limit
			if limit <= 0 || limit > FindResultCap {
				limit = FindResultCap
			}
			ig := loadIgnore(root)
			var found []string
			truncated := false
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
					return nil
				}
				if MatchGlob(a.Pattern, rel) {
					if len(found) >= limit {
						truncated = true
						return filepath.SkipAll
					}
					found = append(found, rel)
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
		PromptGuidelines: []string{"Use execute for file operations like ls, rg, find."},
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			var a struct {
				Command  string `json:"command"`
				TimeoutS int    `json:"timeout_s"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return core.ErrResult("invalid_arguments", err.Error())
			}
			if strings.TrimSpace(a.Command) == "" {
				return core.ErrResult("invalid_arguments", "command is empty")
			}
			dir := ""
			if opts.Workspace != nil {
				dir = opts.Workspace.Root
			}
			res, err := Run(ctx, a.Command, ExecOptions{
				Dir:      dir,
				Timeout:  time.Duration(a.TimeoutS) * time.Second,
				MaxBytes: DefaultByteLimit,
				SpillDir: opts.SpillDir,
				Env:      opts.Env,
			})
			if err != nil {
				return core.ErrResult("exec_failed", err.Error())
			}
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
		},
	}
}
