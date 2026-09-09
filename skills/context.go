package skills

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DefaultMaxContextBytes is Config.MaxContextBytes when it is zero: 256 KiB
// per file. A context file is prose the model reads on EVERY turn; one this
// large is already past what any model attends to, and without a bound a
// repository could make every session start by paying for a gigabyte of
// standing instructions.
const DefaultMaxContextBytes = 256 << 10

// truncationMarker is appended, on its own line, to a body that was cut at
// MaxContextBytes. It is visible to the model so the model knows it holds a
// prefix rather than the file, and it uses no '<' so the text escaper leaves
// it as written.
const truncationMarker = "[agentkit: context file truncated at %d of %d bytes; read the file for the rest]"

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
	// Truncated reports that Body is a prefix of the file, cut at
	// Config.MaxContextBytes on a line boundary and ending in a marker that
	// says so.
	Truncated bool
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
// that they are read and then dropped. And "ancestor" is bounded (see
// trustBoundary): trusting the PROJECT is not trusting /tmp.
func DiscoverContext(cfg Config) ([]ContextFile, []Diagnostic) {
	var out []ContextFile
	var diags []Diagnostic
	limit := cfg.MaxContextBytes
	if limit <= 0 {
		limit = DefaultMaxContextBytes
	}

	if dir, ok := cfg.UserContextDir(); ok {
		switch {
		case cfg.insideUntrustedProject(dir):
			// The same rule as the user skills tier: a global directory that
			// resolves inside the untrusted project would be read as trusted
			// content, and a context file is the whole body, not a name.
			// Reported only when a candidate is actually there.
			if _, _, found := pickCandidate(dir, false); found {
				diags = append(diags, Diagnostic{
					Path: dir, Severity: SeverityWarning,
					Message: "user-global context file skipped: the directory lies inside the untrusted " +
						"working directory and would be read as trusted content (REQ-CTX-03)",
				})
			}
		default:
			// The user's own file follows a symlink: dotfile repositories
			// link their configs into place, and the user controls both ends.
			path, pdiags, found := pickCandidate(dir, false)
			diags = append(diags, pdiags...)
			if found {
				f, fdiags, err := readContextFile(path, true, limit)
				diags = append(diags, fdiags...)
				if err != nil {
					diags = append(diags, Diagnostic{Path: path, Severity: SeverityError, Message: err.Error()})
				} else {
					out = append(out, f)
				}
			}
		}
	}

	if !cfg.TrustProject || cfg.WorkDir == "" {
		return out, diags
	}

	chain := ancestors(cfg.WorkDir)
	chain, bdiags := trustBoundary(chain, cfg.HomeDir)
	diags = append(diags, bdiags...)
	picks := make([]string, len(chain)) // parallel to chain; "" = no file here
	for i, dir := range chain {
		// A repository-authored symlink is rejected here, as a skill's is
		// (REQ-SEC-06): trusting the project is trusting what it contains,
		// not whatever a link inside it points at.
		path, pdiags, found := pickCandidate(dir, true)
		diags = append(diags, pdiags...)
		if found {
			picks[i] = path
		}
	}
	picks, wdiags := suppressWorktreeDuplicate(chain, picks)
	diags = append(diags, wdiags...)

	for _, path := range picks {
		if path == "" {
			continue
		}
		f, fdiags, err := readContextFile(path, false, limit)
		diags = append(diags, fdiags...)
		if err != nil {
			diags = append(diags, Diagnostic{Path: path, Severity: SeverityError, Message: err.Error()})
			continue
		}
		out = append(out, f)
	}
	return out, diags
}

// trustBoundary cuts an ancestor chain (ROOT -> CWD) down to the part the
// project trust decision actually covers.
//
// TrustProject is an answer to "do I trust THIS repository". An ancestor walk
// that runs to the filesystem root reads it as an answer about /home, /tmp
// and /: a file planted at /tmp/AGENTS.md by any local user, or at the top
// of a shared volume, would be injected into every trusted session started
// anywhere beneath it. So the walk starts at the nearer of two anchors:
//
//   - the OUTERMOST enclosing git root, the directory highest on the chain
//     that contains a .git entry (a directory for a repository, a file for a
//     linked worktree), so a nested repository still sees its parent's file
//     (REQ-CTX-05's control case); or
//   - HomeDir itself, when the working directory lies under it, so a
//     project with no repository still gets the user's own ~/AGENTS.md.
//
// "Nearer to the working directory" wins between them. When neither anchor
// is on the chain — no repository, and a working directory outside the home —
// only the working directory itself is read. Directories above the boundary
// are never read; a candidate that exists there is reported so the author of
// an AGENTS.md that stopped loading can see why, and reporting it costs a
// stat, not a read.
func trustBoundary(chain []string, homeDir string) ([]string, []Diagnostic) {
	if len(chain) == 0 {
		return chain, nil
	}
	boundary := -1
	for i, dir := range chain {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			boundary = i // outermost: the first hit walking ROOT -> CWD
			break
		}
	}
	if homeDir != "" && filepath.IsAbs(homeDir) {
		home := filepath.Clean(homeDir)
		for i := boundary + 1; i < len(chain); i++ {
			if chain[i] == home {
				boundary = i // nearer to the working directory than the git root
				break
			}
		}
	}
	if boundary < 0 {
		boundary = len(chain) - 1
	}
	var diags []Diagnostic
	for _, dir := range chain[:boundary] {
		if path, _, found := pickCandidate(dir, false); found {
			diags = append(diags, Diagnostic{
				Path: path, Severity: SeverityWarning,
				Message: fmt.Sprintf("not loaded: above the project trust boundary at %s "+
					"(project trust covers the repository, not its parents)", chain[boundary]),
			})
		}
	}
	return chain[boundary:], diags
}

// pickCandidate applies REQ-CTX-01 within one directory.
//
// A candidate that resolves to a directory falls through to the next name;
// that is stated as a rule about the PATH, so a symlink to a directory must
// fall through too, and a dangling symlink likewise.
//
// With rejectSymlinks a candidate that IS a symlink is skipped with a
// diagnostic and the next name is tried, whatever the link points at. The
// project-tier caller sets it (REQ-SEC-06 parity); the user's own global
// directory does not, because a dotfiles repository linking its configs into
// ~ is the ordinary case there and both ends of the link are the user's.
func pickCandidate(dir string, rejectSymlinks bool) (string, []Diagnostic, bool) {
	var diags []Diagnostic
	for _, name := range ContextCandidates {
		path := filepath.Join(dir, name)
		if rejectSymlinks {
			if li, err := os.Lstat(path); err == nil && li.Mode()&os.ModeSymlink != 0 {
				diags = append(diags, Diagnostic{
					Path: path, Severity: SeverityWarning,
					Message: "symlinked context file rejected (REQ-SEC-06)",
				})
				continue
			}
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		return path, diags, true
	}
	return "", diags, false
}

// readContextFile reads at most limit bytes of body. It reads limit+1 bytes
// from the file, never the whole of it, so a large file costs the limit and
// not its size; the size in the marker comes from stat.
func readContextFile(path string, global bool, limit int) (ContextFile, []Diagnostic, error) {
	f, err := os.Open(path)
	if err != nil {
		return ContextFile{}, nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return ContextFile{}, nil, err
	}
	b = bytes.TrimPrefix(b, []byte(bomPrefix))
	out := ContextFile{Path: path, Global: global}
	if len(b) <= limit {
		out.Body = string(b)
		return out, nil, nil
	}

	// Over the bound. Cut at the last line boundary within it — a cut mid-line
	// leaves half a sentence, or half a code fence, as the last thing the
	// model reads — and say so in the body and in the diagnostics.
	total := int64(len(b))
	if info, serr := f.Stat(); serr == nil && info.Size() > total {
		total = info.Size()
	}
	cut := b[:limit]
	if nl := bytes.LastIndexByte(cut, '\n'); nl > 0 {
		cut = cut[:nl+1]
	}
	body := string(cut)
	if !bytes.HasSuffix(cut, []byte("\n")) {
		body += "\n"
	}
	out.Body = body + fmt.Sprintf(truncationMarker, len(cut), total) + "\n"
	out.Truncated = true
	diags := []Diagnostic{{
		Path: path, Severity: SeverityWarning,
		Message: fmt.Sprintf("context file truncated: %d bytes exceeds the %d-byte limit "+
			"(Config.MaxContextBytes); the first %d bytes were kept", total, limit, len(cut)),
	}}
	return out, diags, nil
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
