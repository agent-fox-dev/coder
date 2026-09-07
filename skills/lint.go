package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentfox/agentkit-go/plugins"
)

// ErrProhibitedImport rejects a skill whose plugin source breaks REQ-SKILL-09.
//
// It is the SECOND thing that can reject a skill, after ErrNoDescription, and
// the exception to REQ-SKILL-10's leniency is deliberate: leniency exists
// because a manifest is authored content whose consumer is a language model,
// so a typo should cost a warning rather than a skill. A prohibited import is
// not a typo — it is code that would run in the host's process with the host's
// credentials — and there is no degraded mode in which the skill is safe to
// load anyway.
var ErrProhibitedImport = errors.New("skills: plugin source imports a prohibited package")

// lintPluginSource is REQ-SKILL-09.
//
// It runs ONLY for a skill that declares [skill.tools], because that
// declaration is what says "this directory ships plugin code the host is meant
// to link". A skill directory that happens to contain a .go file it never
// offers as a tool is inert, and rejecting a skill over an example file would
// be REQ-SKILL-10's failure mode in a new place.
//
// Honesty, per REQ-SEC-07 and repeated at every level so no reader meets only
// the comfortable half: this is an IMPORT-PATH LINT, NOT A SANDBOX. Go plugins
// link at build time (REQ-PLUGIN-08), so an admitted skill's tools run in this
// process with these privileges. The lint catches a skill reaching for a model
// client or for agentkit internals; it does not confine one that does its
// reaching through net/http.
func lintPluginSource(dir string, m Manifest) ([]Diagnostic, error) {
	if m.Tools.Module == "" && m.Tools.Factory == "" {
		return nil, nil
	}

	src, err := hasGoSource(dir)
	if err != nil || !src {
		// The usual case, and not a defect: [skill.tools] names a module that
		// the HOST links (REQ-PLUGIN-08), so the source is normally not in the
		// skill directory at all. Say so rather than passing silently — a
		// check that quietly did not run is worse than one that fails
		// (the same rule plugins.RuleUnlintable states).
		return []Diagnostic{{
			Path: dir, Severity: SeverityWarning,
			Message: fmt.Sprintf("skill declares [skill.tools] %s.%s but ships no Go source here, "+
				"so REQ-SKILL-09's import lint could not run; the module is linked by the host "+
				"at build time (REQ-PLUGIN-08)", m.Tools.Module, m.Tools.Factory),
		}}, nil
	}

	// The lint itself is plugins.LintSkillImports: the same walk REQ-PLUGIN-09
	// uses, with REQ-SKILL-09's larger prohibited set. One linter, two rule
	// sets — a second implementation here would drift from that one.
	bad, err := plugins.LintSkillImports(dir)
	if err != nil {
		return []Diagnostic{{
			Path: dir, Severity: SeverityWarning,
			Message: "REQ-SKILL-09's import lint could not read the skill source: " + err.Error(),
		}}, nil
	}
	if len(bad) == 0 {
		return nil, nil
	}

	diags := make([]Diagnostic, 0, len(bad))
	for _, b := range bad {
		diags = append(diags, Diagnostic{
			Path: b.File, Severity: SeverityError,
			Message: fmt.Sprintf("imports %q; skill plugin code may not import agentkit internals, "+
				"an LLM client library or a model API package (REQ-SKILL-09)", b.Import),
		})
	}
	return diags, fmt.Errorf("%w: %s", ErrProhibitedImport, bad[0].Import)
}

// hasGoSource reports whether the skill directory ships Go code at all. The
// same subtrees plugins' lint skips are skipped here, so "has source" and
// "was linted" cannot disagree.
func hasGoSource(dir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}
