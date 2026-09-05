package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// This file is REQ-TOOL-05.2 and .3: the ignore SOURCES and their precedence,
// and nested-repository boundaries.
//
// The requirement is blunt about why this matters: the absence of a real
// ignore engine "is the difference between a tool that returns the project's
// files and one that returns node_modules". A single root .gitignore — which
// is what this was — gets the common case right and every layered case wrong,
// and the wrongness is a wall of noise in the model's context rather than an
// error anyone sees.
//
// DECLARED SEMANTICS (REQ-TOOL-05.4 asks for a documented answer):
//
//   - A git repository is NOT required. Ignore files are honoured wherever
//     they are found. A scan root outside any repository still gets the user's
//     global excludes and any .gitignore present in the tree.
//   - Sources, lowest precedence first: the global excludes file, the
//     repository's .git/info/exclude, then every .gitignore from the scan root
//     down to the directory being scanned.
//   - Deeper files override shallower ones. Within one file, later patterns
//     win, which is how `!` re-includes.
//   - A directory containing .git starts a NEW ignore root. The outer
//     repository's .gitignore files stop applying inside it, and its own rules
//     do not leak outward. The user's global excludes still apply, because
//     they are the user's and not the repository's.

// ignorePattern is one parsed gitignore line.
type ignorePattern struct {
	glob    string
	negate  bool
	dirOnly bool
	// anchored patterns match from the layer's directory; unanchored ones
	// match any path segment.
	anchored bool
}

// ignoreLayer is one directory's contribution to the stack.
type ignoreLayer struct {
	// dir is slash-relative to the scan root; "" is the root itself.
	dir  string
	pats []ignorePattern
	// repoStart marks a directory that is its own repository root. Matching a
	// path inside it begins here, so nothing shallower leaks in.
	repoStart bool
}

// ignoreOptions injects the environment the engine reads, so the whole thing
// is testable without mutating process state or requiring a git binary
// (NFR-TEST-04).
type ignoreOptions struct {
	Getenv func(string) string
	Home   func() (string, error)
	// GitConfig returns core.excludesFile. Nil runs `git config` once.
	GitConfig func() string
}

func (o ignoreOptions) getenv(k string) string {
	if o.Getenv != nil {
		return o.Getenv(k)
	}
	return os.Getenv(k)
}

func (o ignoreOptions) home() (string, error) {
	if o.Home != nil {
		return o.Home()
	}
	return os.UserHomeDir()
}

// ignoreEngine holds the stack for one scan.
type ignoreEngine struct {
	root   string
	global []ignorePattern
	layers []ignoreLayer
	seen   map[string]bool
}

// loadIgnore builds the engine for a scan rooted at root.
func loadIgnore(root string) *ignoreEngine { return newIgnoreEngine(root, ignoreOptions{}) }

func newIgnoreEngine(root string, opts ignoreOptions) *ignoreEngine {
	e := &ignoreEngine{root: root, seen: map[string]bool{}}
	e.global = append(e.global, parseIgnoreFile(globalExcludesPath(opts))...)
	// The scan root's own layer, which also carries the repository's
	// .git/info/exclude when there is one.
	e.enter("", root)
	return e
}

// enter loads the ignore files contributed by one directory. It is called as
// the walk descends, once per directory, before that directory's children are
// matched.
func (e *ignoreEngine) enter(rel, abs string) {
	rel = filepath.ToSlash(rel)
	if rel == "." {
		rel = ""
	}
	if e.seen[rel] {
		return
	}
	e.seen[rel] = true

	layer := ignoreLayer{dir: rel}
	if st, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
		// A repository root: its own .git/info/exclude applies here and
		// nothing shallower does. A .git FILE (a worktree or submodule
		// pointer) marks a repository just as a directory does.
		_ = st
		layer.repoStart = true
		layer.pats = append(layer.pats, parseIgnoreFile(filepath.Join(abs, ".git", "info", "exclude"))...)
	}
	layer.pats = append(layer.pats, parseIgnoreFile(filepath.Join(abs, ".gitignore"))...)

	if len(layer.pats) == 0 && !layer.repoStart {
		return // nothing to contribute
	}
	e.layers = append(e.layers, layer)
}

// match reports whether rel (slash-relative to the scan root) is ignored.
//
// Evaluation runs shallowest to deepest with LAST MATCH WINNING, which
// delivers both halves of REQ-TOOL-05.2 in one pass: deeper files override
// shallower ones because they are evaluated later, and `!` re-includes within
// a file for the same reason.
func (e *ignoreEngine) match(rel string, isDir bool) bool {
	rel = filepath.ToSlash(rel)
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return true
	}

	ignored := false
	for _, p := range e.global {
		if p.matches(rel, isDir) {
			ignored = !p.negate
		}
	}

	start := e.repoBoundary(rel)
	for i := start; i < len(e.layers); i++ {
		l := e.layers[i]
		sub, ok := relUnder(l.dir, rel)
		if !ok {
			continue
		}
		for _, p := range l.pats {
			if p.matches(sub, isDir) {
				ignored = !p.negate
			}
		}
	}
	return ignored
}

// repoBoundary returns the index of the innermost repository-root layer
// containing rel, or 0.
//
// This is REQ-TOOL-05.3. Without it, a vendored dependency that is itself a
// git checkout inherits the outer project's .gitignore — so a rule the outer
// project wrote about its own build output silently deletes files from the
// listing of a repository that has never heard of it.
func (e *ignoreEngine) repoBoundary(rel string) int {
	best := 0
	for i, l := range e.layers {
		if !l.repoStart || l.dir == "" {
			continue
		}
		if _, ok := relUnder(l.dir, rel); ok {
			best = i
		}
	}
	return best
}

// relUnder returns rel expressed relative to dir, and whether it is under it.
func relUnder(dir, rel string) (string, bool) {
	if dir == "" {
		return rel, true
	}
	if !strings.HasPrefix(rel, dir+"/") {
		return "", false
	}
	return rel[len(dir)+1:], true
}

func (p ignorePattern) matches(rel string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	if p.anchored {
		return matchSegments(p.glob, rel) || strings.HasPrefix(rel, p.glob+"/")
	}
	// Unanchored: match any segment, so `node_modules` ignores it at any
	// depth, and everything beneath a matched directory goes with it.
	for _, seg := range strings.Split(rel, "/") {
		if matchOne(p.glob, seg) {
			return true
		}
	}
	return false
}

func parseIgnoreFile(path string) []ignorePattern {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []ignorePattern
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := ignorePattern{}
		if strings.HasPrefix(line, "!") {
			p.negate, line = true, line[1:]
		}
		if strings.HasSuffix(line, "/") {
			p.dirOnly, line = true, strings.TrimSuffix(line, "/")
		}
		// A pattern containing a slash anywhere but at the end is anchored to
		// the directory holding the ignore file; one that does not matches at
		// any depth. That asymmetry is gitignore's, not ours.
		p.anchored = strings.Contains(line, "/")
		p.glob = strings.TrimPrefix(line, "/")
		if p.glob == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// globalExcludesPath resolves REQ-TOOL-05.2's first source, in order:
// `git config --path --get core.excludesFile`, then $XDG_CONFIG_HOME/git/ignore,
// then ~/.config/git/ignore.
//
// The git subprocess is the FIRST source rather than a fallback because it is
// the only one that reflects an explicitly configured path; the XDG locations
// are git's own defaults, and reading them first would ignore a user who
// pointed core.excludesFile somewhere else.
func globalExcludesPath(opts ignoreOptions) string {
	if p := gitExcludesFile(opts); p != "" {
		return p
	}
	if x := opts.getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "git", "ignore")
	}
	if h, err := opts.home(); err == nil && h != "" {
		return filepath.Join(h, ".config", "git", "ignore")
	}
	return ""
}

func gitExcludesFile(opts ignoreOptions) string {
	if opts.GitConfig != nil {
		return opts.GitConfig()
	}
	// Bounded, and failure is silent: git may be absent, may not be a
	// repository, or may simply have no such setting. None of those is an
	// error for a file search, and none should cost more than a moment.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "config", "--path", "--get", "core.excludesFile").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
