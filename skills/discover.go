package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Directory and file names of REQ-SKILL-02 and REQ-SKILL-04.
const (
	// GlobalDirName is the per-user and per-project config directory name.
	GlobalDirName = ".nightshift"
	// SkillsDirName is the skills subdirectory inside it.
	SkillsDirName = "skills"
	// ManifestName is the skill manifest (REQ-SKILL-02).
	ManifestName = "skill.toml"
	// PromptName is the skill's prompt file. REQ-SKILL-06 removed
	// `prompt_file` from the manifest, so this name is fixed: the path in the
	// system prompt is derived, never author-supplied.
	PromptName = "prompt.md"
)

// Tier is where a skill came from. It is the trust boundary, not a label.
type Tier uint8

const (
	// TierBuiltin is the SDK's own `_skills/` directory.
	TierBuiltin Tier = iota + 1
	// TierUser is ~/.nightshift/skills — the user's own content.
	TierUser
	// TierProject is <cwd>/.nightshift/skills — UNTRUSTED, gated on
	// Config.TrustProject (REQ-SKILL-12).
	TierProject
)

func (t Tier) String() string {
	switch t {
	case TierBuiltin:
		return "builtin"
	case TierUser:
		return "user"
	case TierProject:
		return "project"
	}
	return "unknown"
}

// Trusted reports whether the tier's content is trusted by origin
// (REQ-SKILL-04). Project-local content never is; it is admitted by an
// embedder's affirmative act, which is a different thing from being trusted by
// where it came from.
func (t Tier) Trusted() bool { return t == TierBuiltin || t == TierUser }

// precedence implements REQ-SKILL-04's collision rule: user-global >
// project-local > SDK built-in. Project-local never overrides a user's own
// skill — otherwise a cloned repository could shadow the user's own tooling by
// naming a skill the same thing.
func (t Tier) precedence() int {
	switch t {
	case TierUser:
		return 3
	case TierProject:
		return 2
	case TierBuiltin:
		return 1
	}
	return 0
}

// Config selects the three discovery roots and holds the trust gate.
//
// Every path is explicit. There is no "resolve it later" field, because the
// one thing this package must never do is resolve a root RELATIVELY: a bare
// ".nightshift/skills" resolves against the process working directory, which
// is the untrusted repository, and would let it impersonate the user's own
// global config (REQ-SKILL-12.3).
type Config struct {
	// BuiltinDir is the SDK's `_skills/` directory. Empty skips the tier.
	BuiltinDir string
	// HomeDir is the resolved home directory. EMPTY IS A VALID AND EXPECTED
	// STATE — containers, CI, systemd units and cron routinely have no
	// resolvable home — and it skips the user-global tier entirely rather than
	// falling back to anything (REQ-SKILL-12.3).
	HomeDir string
	// WorkDir is the project root to scan. Empty skips the project tier.
	WorkDir string
	// TrustProject is the gate of REQ-SKILL-12 / REQ-SEC-10. Its zero value is
	// false, so an embedder that never mentions trust is untrusted BY
	// CONSTRUCTION. Nothing in this package infers it; silence is not consent.
	TrustProject bool
}

// UserSkillsDir returns the user-global skills directory, and false when there
// is no resolvable home. Callers must treat "no global directory" as a valid
// state (REQ-SKILL-12.3).
func (c Config) UserSkillsDir() (string, bool) {
	if c.HomeDir == "" || !filepath.IsAbs(c.HomeDir) {
		return "", false
	}
	return filepath.Join(c.HomeDir, GlobalDirName, SkillsDirName), true
}

// ProjectSkillsDir returns the project-local skills directory, and false when
// the project is untrusted or unset.
func (c Config) ProjectSkillsDir() (string, bool) {
	if !c.TrustProject || c.WorkDir == "" {
		return "", false
	}
	abs, err := filepath.Abs(c.WorkDir)
	if err != nil {
		return "", false
	}
	return filepath.Join(abs, GlobalDirName, SkillsDirName), true
}

// ResolveHome resolves the user's home directory, reporting false when it
// cannot be established.
//
// The non-absolute check is not paranoia: os.UserHomeDir returns $HOME
// verbatim, and a process started with HOME=. or HOME=relative/path would
// otherwise hand this package a root that resolves inside the repository being
// scanned. Fail closed (REQ-SKILL-12.3).
func ResolveHome() (string, bool) {
	h, err := os.UserHomeDir()
	if err != nil || h == "" || !filepath.IsAbs(h) {
		return "", false
	}
	return h, true
}

// Skill is a discovered, loaded skill.
type Skill struct {
	Manifest
	// Dir is the absolute skill directory.
	Dir string
	// PromptPath is the absolute path to prompt.md. It is what REQ-SKILL-06
	// puts in the system prompt, and it is also the base for the relative-path
	// resolution rule the block carries.
	PromptPath string
	Tier       Tier
}

// Registry is the result of discovery.
type Registry struct {
	cfg    Config
	skills []Skill
	diags  []Diagnostic
}

// Discover scans the three tiers of REQ-SKILL-04 in trust order.
//
// The trust gate is applied at DISCOVERY, not at selection: an untrusted
// project directory is never read, so untrusted bytes never enter the process
// and cannot be leaked by a later code path that forgot to filter. That is a
// deliberate strengthening of REQ-SKILL-05, which puts the gate in
// LoadForSession.
func Discover(cfg Config) *Registry {
	r := &Registry{cfg: cfg}

	type root struct {
		dir  string
		tier Tier
	}
	var roots []root
	if cfg.BuiltinDir != "" {
		roots = append(roots, root{cfg.BuiltinDir, TierBuiltin})
	}
	if dir, ok := cfg.UserSkillsDir(); ok {
		roots = append(roots, root{dir, TierUser})
	}
	if dir, ok := cfg.ProjectSkillsDir(); ok {
		roots = append(roots, root{dir, TierProject})
	}

	winners := map[string]Skill{}
	for _, rt := range roots {
		found, diags := scanTier(rt.dir, rt.tier)
		r.diags = append(r.diags, diags...)
		for _, s := range found {
			prev, clash := winners[s.Name]
			if clash && prev.Tier.precedence() >= s.Tier.precedence() {
				r.diags = append(r.diags, Diagnostic{
					Path: s.Dir, Severity: SeverityWarning,
					Message: fmt.Sprintf("skill %q is shadowed by the %s skill at %s",
						s.Name, prev.Tier, prev.Dir),
				})
				continue
			}
			if clash {
				r.diags = append(r.diags, Diagnostic{
					Path: prev.Dir, Severity: SeverityWarning,
					Message: fmt.Sprintf("skill %q is shadowed by the %s skill at %s",
						prev.Name, s.Tier, s.Dir),
				})
			}
			winners[s.Name] = s
		}
	}

	r.skills = make([]Skill, 0, len(winners))
	for _, s := range winners {
		r.skills = append(r.skills, s)
	}
	// Sorted by name, not by filesystem order. os.ReadDir sorts, but the tiers
	// interleave and the winners come out of a map; without this the assembled
	// prompt would reorder between runs and invalidate the provider's cache
	// prefix on every session for no reason.
	sort.Slice(r.skills, func(i, j int) bool { return r.skills[i].Name < r.skills[j].Name })
	return r
}

// scanTier loads every skill directory directly under dir. A missing directory
// is the normal case, not a diagnostic.
func scanTier(dir string, tier Tier) ([]Skill, []Diagnostic) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, []Diagnostic{{Path: dir, Severity: SeverityError, Message: err.Error()}}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []Diagnostic{{Path: abs, Severity: SeverityError, Message: err.Error()}}
	}

	var out []Skill
	var diags []Diagnostic
	for _, e := range entries {
		path := filepath.Join(abs, e.Name())
		// REQ-SEC-06: symlinked skill directories and prompt files are
		// rejected. e.Type() is the LSTAT mode, so a symlink to a directory
		// reports ModeSymlink here and IsDir() would report true — checking
		// IsDir first would admit exactly what this rejects.
		if e.Type()&os.ModeSymlink != 0 {
			diags = append(diags, Diagnostic{
				Path: path, Severity: SeverityError,
				Message: "symlinked skill directory rejected (REQ-SEC-06)",
			})
			continue
		}
		if !e.IsDir() {
			continue
		}
		s, sdiags, err := loadSkill(path, tier)
		diags = append(diags, sdiags...)
		if err != nil {
			diags = append(diags, Diagnostic{
				Path: path, Severity: SeverityError,
				Message: fmt.Sprintf("skill not loaded: %v", err),
			})
			continue
		}
		out = append(out, s)
	}
	return out, diags
}

func loadSkill(dir string, tier Tier) (Skill, []Diagnostic, error) {
	manifestPath := filepath.Join(dir, ManifestName)
	src, err := os.ReadFile(manifestPath)
	if err != nil {
		// A directory with no skill.toml is not a skill; say so plainly rather
		// than surfacing a bare ENOENT.
		if os.IsNotExist(err) {
			return Skill{}, nil, fmt.Errorf("no %s", ManifestName)
		}
		return Skill{}, nil, err
	}

	m, diags, err := ParseManifest(manifestPath, src)
	if err != nil {
		return Skill{}, diags, err
	}
	if m.Name == "" {
		// A nameless skill is still usable — the directory names it — so this
		// is a warning, not a rejection (REQ-SKILL-10).
		m.Name = filepath.Base(dir)
		diags = append(diags, Diagnostic{
			Path: manifestPath, Severity: SeverityWarning,
			Message: fmt.Sprintf("manifest has no name; using the directory name %q", m.Name),
		})
	}

	promptPath := filepath.Join(dir, PromptName)
	info, err := os.Lstat(promptPath)
	if err != nil {
		// REQ-SKILL-06 puts this path in the system prompt with an instruction
		// to read it. A skill whose prompt file is absent would make the SDK
		// tell the model to read a file that does not exist, and the model
		// would burn a turn discovering that. Reject instead.
		return Skill{}, diags, fmt.Errorf("no %s", PromptName)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Skill{}, diags, fmt.Errorf("symlinked %s rejected (REQ-SEC-06)", PromptName)
	}
	if !info.Mode().IsRegular() {
		return Skill{}, diags, fmt.Errorf("%s is not a regular file", PromptName)
	}

	return Skill{Manifest: m, Dir: dir, PromptPath: promptPath, Tier: tier}, diags, nil
}

// Skills returns every loaded skill, ordered by name.
func (r *Registry) Skills() []Skill { return append([]Skill(nil), r.skills...) }

// Diagnostics returns everything discovery skipped or warned about.
func (r *Registry) Diagnostics() []Diagnostic { return append([]Diagnostic(nil), r.diags...) }

// Names returns the loaded skill names, for the session audit event of
// REQ-SKILL-11. Emitting the event is the session layer's job; this package
// only supplies the list.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s.Name)
	}
	return out
}

// LoadForSession is REQ-SKILL-05's selector.
//
// Two deviations from the literal signature, both deliberate:
//
//  1. There is no config parameter. The trust gate ran at discovery, so the
//     registry already holds only material the embedder admitted. Taking a
//     config here would imply the gate could be re-decided after the untrusted
//     bytes were already read, which is the wrong shape for a security check.
//
//  2. taskPrompt is accepted and IGNORED, and that is the point of
//     REQ-SKILL-06. Keyword triggering was removed from the manifest, so
//     nothing here matches the task text against the skill: the MODEL chooses,
//     from the description, at the moment it needs the skill. The parameter
//     stays so the call site reads as the PRD writes it and so a future
//     ranking pass has somewhere to live.
func (r *Registry) LoadForSession(archetype, taskPrompt string) []Skill {
	_ = taskPrompt
	out := make([]Skill, 0, len(r.skills))
	for _, s := range r.skills {
		if !matchesArchetype(s, archetype) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// matchesArchetype applies REQ-SKILL-05's archetype filter. A skill that
// declares no archetypes is universal; a skill that declares some is offered
// only for those, including when the session names no archetype at all — a
// skill that scoped itself has said it is not for the general case.
func matchesArchetype(s Skill, archetype string) bool {
	if len(s.Archetypes) == 0 {
		return true
	}
	for _, a := range s.Archetypes {
		if a == archetype {
			return true
		}
	}
	return false
}
