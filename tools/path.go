// Package tools provides AgentKit's built-in tool set.
//
// The file tool set is deliberately minimal (REQ-TOOL-04): read, write, edit,
// find and list. A tool earns a slot only when it has semantics `execute`
// cannot express — truncation, unique-match editing, deterministic ignore
// handling. Deletion, renaming, appending and stat are one shell word each and
// ship no tool. Their absence is the requirement.
package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Errors returned by the path guard. They are sentinels so a policy can tell
// "outside the workspace" from "malformed" without matching on strings.
var (
	ErrPathNotAllowed = errors.New("tools: path_not_allowed")
	ErrPathMalformed  = errors.New("tools: path_malformed")
	ErrNoHome         = errors.New("tools: home directory could not be resolved")
)

// Workspace is the containment boundary for the built-in FILE tools
// (REQ-SEC-01).
//
// It does NOT constrain `execute`. That is not an oversight, and it is stated
// here rather than left to be discovered: a shell command can read and write
// anything the process can, and pretending otherwise would be worse than
// admitting it. The boundary for `execute` is the BeforeToolCall interceptor
// (REQ-SEC-03). See TestExecuteIsNotPathContained, whose name is the
// documentation.
type Workspace struct {
	// Root is the absolute, symlink-resolved workspace root.
	Root string
}

// NewWorkspace resolves root to its absolute canonical form.
func NewWorkspace(root string) (*Workspace, error) {
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Resolve the root once. If it does not exist yet that is fine; containment
	// still works against the lexical form.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &Workspace{Root: abs}, nil
}

// Resolve normalizes and then contains a model-supplied path.
//
// ORDER IS LOAD-BEARING. Normalization — `~` expansion, `file://` unwrapping,
// stripping a leading `@` or quote the model sometimes emits — happens BEFORE
// canonicalization, never after (REQ-SEC-01). A guard that canonicalizes first
// and expands later checks a string that is not the string it will open.
//
// Symlinks are resolved against the deepest existing ancestor, so a path whose
// final component does not exist yet is still contained by its parent — the
// case a naive EvalSymlinks-the-whole-path guard gets wrong for every file the
// agent is about to create.
func (w *Workspace) Resolve(p string) (string, error) {
	n, err := Normalize(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(n) {
		n = filepath.Join(w.Root, n)
	}
	n = filepath.Clean(n)

	resolved, err := resolveExistingPrefix(n)
	if err != nil {
		return "", err
	}
	if !within(w.Root, resolved) {
		return "", fmt.Errorf("%w: %s resolves outside the workspace root %s",
			ErrPathNotAllowed, p, w.Root)
	}
	return resolved, nil
}

// resolveExistingPrefix walks up until it finds an existing ancestor, resolves
// THAT through symlinks, and rejoins the missing tail. This is what makes
// containment correct for a file that does not exist yet, including one whose
// parent directory is a symlink out of the workspace.
//
// A DANGLING symlink is the case the naive walk gets wrong (REQ-SEC-01,
// NFR-SEC-02): EvalSymlinks fails on `ws/link -> /outside/newfile` exactly as
// it fails on a missing file, and resolving the parent and rejoining the base
// yields `ws/link` — inside the root — while os.WriteFile then follows the link
// and creates /outside/newfile. So when EvalSymlinks fails, the component is
// Lstat'ed; if it is a link its target is substituted and containment
// continues on the TARGET, which is the path the write would actually reach.
func resolveExistingPrefix(abs string) (string, error) {
	tail := ""
	cur := abs
	// Bounded so a link cycle (a -> b -> a, both dangling through the cycle)
	// terminates. The number is what the platform resolvers use.
	for hops := 0; hops < maxSymlinkHops; hops++ {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(cur)
			if err != nil {
				return "", fmt.Errorf("%w: %s: %v", ErrPathMalformed, cur, err)
			}
			if !filepath.IsAbs(target) {
				// A relative target is relative to the directory holding the
				// link, not to the process working directory.
				target = filepath.Join(filepath.Dir(cur), target)
			}
			cur = filepath.Clean(target)
			continue
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without finding anything that exists.
			return filepath.Join(cur, tail), nil
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
	return "", fmt.Errorf("%w: too many levels of symbolic links in %s", ErrPathMalformed, abs)
}

// maxSymlinkHops bounds resolveExistingPrefix's link following.
const maxSymlinkHops = 40

// CheckWriteTarget re-checks abs IMMEDIATELY before a write touches it
// (REQ-SEC-01). Resolve already returned a contained path, but a link created
// between the check and the open — by a concurrent command, or by a hostile
// repository's own build step — would be followed by the open.
//
// The WHOLE path is re-resolved, not only its last component. An earlier
// version looked at the leaf alone, and the leaf is not where the swap
// happens: `ws/sub` replaced by `ws/sub -> /elsewhere` leaves
// `ws/sub/file.txt` looking like a plain file (or an absent one) while the
// write lands in /elsewhere — and the MkdirAll that precedes a write would
// build the missing directories there too. So the callers run this BEFORE
// MkdirAll, and it walks the existing prefix exactly as Resolve did.
func (w *Workspace) CheckWriteTarget(abs string) error {
	resolved, err := resolveExistingPrefix(abs)
	if err != nil {
		return err
	}
	if within(w.Root, resolved) {
		return nil
	}
	if fi, lerr := os.Lstat(abs); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink whose target %s lies outside the workspace root %s",
			ErrPathNotAllowed, w.Rel(abs), resolved, w.Root)
	}
	return fmt.Errorf("%w: %s now resolves to %s, outside the workspace root %s",
		ErrPathNotAllowed, w.Rel(abs), resolved, w.Root)
}

// within reports whether target is root or is inside it. It compares path
// SEGMENTS, never string prefixes: a prefix test says /work/space is inside
// /work/s.
func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Normalize applies the pre-canonicalization rewrites.
//
// `~user` is REJECTED rather than expanded. Expanding it needs os/user, which
// is cgo-backed on most platforms — exactly the dependency the REQ-GO-13 gate
// exists to catch, and it would silently break cross-compilation for the
// NFR-COMPAT-06 matrix (ruling P-46).
//
// An unresolvable HOME is also a rejection, not a fallback to a relative path.
// A relative path resolves against the process working directory — whatever
// repository the agent happens to be run in — so falling back would let a
// hostile repo's files stand in for the user's own. Fail closed.
func Normalize(p string) (string, error) {
	s := strings.TrimSpace(p)
	if s == "" {
		return "", fmt.Errorf("%w: empty path", ErrPathMalformed)
	}

	// A model that has seen shell transcripts sometimes emits @file or "file".
	s = strings.TrimPrefix(s, "@")
	s = strings.Trim(s, `"'`)

	if rest, ok := strings.CutPrefix(s, "file://"); ok {
		// file:///abs/path -> /abs/path ; file://host/path is not local.
		if strings.HasPrefix(rest, "/") {
			s = rest
		} else {
			return "", fmt.Errorf("%w: non-local file URL %q", ErrPathMalformed, p)
		}
	}

	switch {
	case s == "~":
		home, err := homeDir()
		if err != nil {
			return "", err
		}
		s = home
	case strings.HasPrefix(s, "~/"):
		home, err := homeDir()
		if err != nil {
			return "", err
		}
		s = filepath.Join(home, s[2:])
	case strings.HasPrefix(s, "~"):
		return "", fmt.Errorf(
			"%w: ~user expansion is not supported (it requires a cgo-backed user "+
				"lookup, which would break cross-compilation); use an absolute path", ErrPathMalformed)
	}

	if strings.ContainsRune(s, 0) {
		return "", fmt.Errorf("%w: path contains a NUL byte", ErrPathMalformed)
	}
	return s, nil
}

func homeDir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		// Ordinary in containers, CI, systemd units and cron.
		return "", fmt.Errorf("%w: refusing to fall back to a relative path, which "+
			"would resolve against the current repository", ErrNoHome)
	}
	return h, nil
}

// Rel renders an absolute path relative to the workspace for display. It never
// fails: an unrelatable path is shown as-is.
func (w *Workspace) Rel(abs string) string {
	if rel, err := filepath.Rel(w.Root, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return abs
}
