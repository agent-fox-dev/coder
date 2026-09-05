package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// bomPrefix is the UTF-8 byte order mark, written as an escape because a
// literal BOM is illegal in Go source. REQ-CTX-02 requires it stripped from
// every context file; left in, it becomes the first character of the injected
// prose and the first thing the model reads.
const bomPrefix = "\ufeff"

// ContextCandidates are the per-directory candidate filenames of REQ-CTX-01,
// in priority order. The FIRST match in a directory wins: an override file
// REPLACES that directory's other candidates rather than adding to them, so a
// developer can neutralize a checked-in AGENTS.md locally without editing it.
var ContextCandidates = []string{"AGENTS.override.md", "AGENTS.md", "CLAUDE.md"}

// ContextFile is one loaded project context file (§6.5a).
//
// A context file is strictly MORE POWERFUL than a skill's metadata and is
// therefore gated at least as hard (REQ-CTX-03). A skill contributes a name, a
// description and a path; a context file contributes ITS ENTIRE BODY as
// standing instructions that compete with the user's own for the session. It
// has no manifest, no opt-in and no per-turn decision by the model: it is
// simply always on. That is why the trust gate governs the whole ancestor
// walk, not just the reading of any one file.
type ContextFile struct {
	// Path is absolute; it is interpolated into the prompt (escaped) so the
	// model can cite which file an instruction came from.
	Path string
	// Body is the file with any leading BOM stripped.
	Body string
	// Global marks the user's own ~/.nightshift file, which is trusted by
	// origin and therefore loaded whether or not the project is trusted.
	Global bool
}

// UserContextDir returns the user-global config directory, and false when
// there is no resolvable home (REQ-SKILL-12.3).
func (c Config) UserContextDir() (string, bool) {
	if c.HomeDir == "" || !filepath.IsAbs(c.HomeDir) {
		return "", false
	}
	return filepath.Join(c.HomeDir, GlobalDirName), true
}

// DiscoverContext loads the project context files of §6.5a.
//
// Order is REQ-CTX-02's: the user-global file first, then every ancestor of
// the working directory ordered ROOT -> CWD. The most specific file is
// therefore last in the prompt and most recent in the model's attention, which
// is the whole reason the order is specified rather than left to the walk.
//
// The ancestor walk is gated on Config.TrustProject in its entirety
// (REQ-CTX-03, REQ-SEC-10). Untrusted means the files are not read at all, not
// that they are read and then dropped.
func DiscoverContext(cfg Config) ([]ContextFile, []Diagnostic) {
	var out []ContextFile
	var diags []Diagnostic

	if dir, ok := cfg.UserContextDir(); ok {
		if path, found := pickCandidate(dir); found {
			f, err := readContextFile(path, true)
			if err != nil {
				diags = append(diags, Diagnostic{Path: path, Severity: SeverityError, Message: err.Error()})
			} else {
				out = append(out, f)
			}
		}
	}

	if !cfg.TrustProject || cfg.WorkDir == "" {
		return out, diags
	}

	chain := ancestors(cfg.WorkDir)
	picks := make([]string, len(chain)) // parallel to chain; "" = no file here
	for i, dir := range chain {
		if path, found := pickCandidate(dir); found {
			picks[i] = path
		}
	}
	picks, wdiags := suppressWorktreeDuplicate(chain, picks)
	diags = append(diags, wdiags...)

	for _, path := range picks {
		if path == "" {
			continue
		}
		f, err := readContextFile(path, false)
		if err != nil {
			diags = append(diags, Diagnostic{Path: path, Severity: SeverityError, Message: err.Error()})
			continue
		}
		out = append(out, f)
	}
	return out, diags
}

// pickCandidate applies REQ-CTX-01 within one directory.
//
// os.Stat, not Lstat: a candidate that resolves to a directory falls through
// to the next name, and that is stated as a rule about the PATH, so a symlink
// to a directory must fall through too. A dangling symlink fails Stat and also
// falls through.
func pickCandidate(dir string) (string, bool) {
	for _, name := range ContextCandidates {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		return path, true
	}
	return "", false
}

func readContextFile(path string, global bool) (ContextFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ContextFile{}, err
	}
	return ContextFile{
		Path:   path,
		Body:   strings.TrimPrefix(string(b), bomPrefix),
		Global: global,
	}, nil
}

// ancestors returns cfg.WorkDir and every parent, ordered ROOT -> CWD
// (REQ-CTX-02).
func ancestors(dir string) []string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = filepath.Clean(dir)
	}
	var chain []string
	for {
		chain = append(chain, abs)
		parent := filepath.Dir(abs)
		if parent == abs {
			break
		}
		abs = parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// suppressWorktreeDuplicate implements REQ-CTX-05.
//
// A linked git worktree checked out INSIDE its own main repository puts both
// copies of a tracked context file on one ancestor chain. They are the same
// logical repository scope, so loading both applies the same instructions
// twice, which measurably degrades instruction-following. The worktree's copy
// is the one the developer is working in, so the MAIN repository's copy is the
// one dropped.
//
// The discriminator is the shape of `.git`: a regular FILE (a gitdir pointer)
// marks a linked worktree, a DIRECTORY marks the main repository. Only the
// same filename is suppressed — an AGENTS.md in the worktree does not silence
// a CLAUDE.md above it, which is a different file with different content.
func suppressWorktreeDuplicate(chain, picks []string) ([]string, []Diagnostic) {
	worktree := -1
	for i, dir := range chain {
		if info, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && info.Mode().IsRegular() {
			worktree = i // deepest wins
		}
	}
	if worktree < 0 || picks[worktree] == "" {
		return picks, nil
	}

	var diags []Diagnostic
	base := filepath.Base(picks[worktree])
	for i := 0; i < worktree; i++ {
		if picks[i] == "" || filepath.Base(picks[i]) != base {
			continue
		}
		info, err := os.Lstat(filepath.Join(chain[i], ".git"))
		if err != nil || !info.IsDir() {
			continue
		}
		diags = append(diags, Diagnostic{
			Path: picks[i], Severity: SeverityWarning,
			Message: fmt.Sprintf(
				"skipped: the linked worktree at %s carries the same %s (REQ-CTX-05)",
				chain[worktree], base),
		})
		picks[i] = ""
	}
	return picks, diags
}
