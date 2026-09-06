package skills

import (
	"errors"
	"fmt"
	"strings"

	"github.com/agentfox/agentkit-go/internal/diag"
)

// Severity and Diagnostic are the shared report type (internal/diag), aliased
// here because they are part of this package's public surface and were before
// the move.
type Severity = diag.Severity

const (
	SeverityWarning = diag.SeverityWarning
	SeverityError   = diag.SeverityError
)

// Diagnostic is a non-fatal report from parsing or discovery. Skills are
// authored content, so almost everything that goes wrong here is a warning the
// embedder may log; only SeverityError means something was not loaded.
type Diagnostic = diag.Diagnostic

// ErrNoDescription is the ONLY manifest content that rejects a skill
// (REQ-SKILL-10). Without a description the skill cannot be offered to the
// model at all — progressive disclosure gives the model nothing else to
// choose on (REQ-SKILL-06) — so a description-less skill is not a degraded
// skill, it is an unusable one.
var ErrNoDescription = errors.New("skills: manifest has no description")

// Manifest is the parsed skill.toml of REQ-SKILL-03.
//
// The `injection`, `keywords`, `prompt_file` and `prompt_position` fields are
// deliberately absent: REQ-SKILL-06 removed them. They are recognized by name
// in the diagnostics so an author who copies an old manifest is told what
// happened rather than getting a bare "unknown key".
type Manifest struct {
	Name          string
	Version       string
	Description   string
	Author        string
	SDKMinVersion string
	Archetypes    []string
	// DisableModelInvocation removes the skill from the <available_skills>
	// block (REQ-SKILL-06.4). It does not unload the skill: its tools still
	// merge, because a skill can be a tool bundle the host activates itself.
	DisableModelInvocation bool
	// Overrides names the tools this skill is permitted to replace. It is not
	// in REQ-SKILL-03's field list but REQ-SKILL-07 requires it ("unless one
	// skill declares overrides"), and a permission with no way to express it
	// is not a permission.
	Overrides []string

	Tools    ToolsSection
	Security SecuritySection
	Session  SessionSection
	Subagent SubagentSection
}

// ToolsSection is [skill.tools]. The module/factory pair is carried, not
// executed: Go plugins link at build time (REQ-PLUGIN-08), so the embedder
// supplies the tool values to MergeTools.
type ToolsSection struct {
	Module  string
	Factory string
}

// SecuritySection is [skill.security]. Extensions are additive only
// (REQ-SEC-06); nothing here can remove a permission the host granted.
type SecuritySection struct {
	AllowlistExtend []string
}

// SessionSection is [skill.session].
type SessionSection struct {
	MaxTurnsAdd int
}

// SubagentSection is [skill.subagent]. Spawning is declarative only
// (REQ-SKILL-08): this package parses the declaration and never acts on it.
type SubagentSection struct {
	Archetype      string
	Mode           string
	PromptTemplate string
	ResultKey      string
	// OnFailure is "abort", "warn" or "skip", defaulting to "warn" per the
	// PRD's OQ ruling: pre-analysis is enrichment, not a hard dependency, and
	// authored content failing should degrade rather than reject
	// (consistent with REQ-SKILL-10).
	OnFailure string
}

// removedFields maps a field REQ-SKILL-06 deleted to the reason, so the
// diagnostic explains the design instead of accusing the author of a typo.
var removedFields = map[string]string{
	"injection":       "removed by REQ-SKILL-06: a skill body is never injected; the model reads the file itself",
	"keywords":        "removed by REQ-SKILL-06: the model selects a skill from its description, not from a keyword match",
	"prompt_file":     "removed by REQ-SKILL-06: the prompt file is always prompt.md in the skill directory",
	"prompt_position": "removed by REQ-SKILL-06: replace_section would let discovered content delete part of the host's own prompt",
}

// ParseManifest parses skill.toml.
//
// It is LENIENT BY REQUIREMENT (REQ-SKILL-10): an unknown key is a warning and
// the skill still loads; a value of the wrong type is a warning and the field
// keeps its zero value. The only rejection is a missing description.
//
// Do not "fix" this into strict decoding. Strict unknown-key rejection belongs
// to WIRE boundaries — provider responses, MCP payloads (REQ-SEC-12) — where an
// unknown property means a peer smuggled a field past the schema. A manifest is
// local authored content whose consumer is a language model; deleting a whole
// skill over a typo'd key is the worse of the two failures.
func ParseManifest(path string, src []byte) (Manifest, []Diagnostic, error) {
	root, diags, err := ParseTOML(src)
	for i := range diags {
		diags[i].Path = path
	}
	if err != nil {
		return Manifest{}, diags, err
	}

	r := &tableReader{tbl: root, path: path, used: map[string]bool{}}
	var m Manifest

	// [skill] is accepted as an alias table for the top-level metadata keys.
	// REQ-SKILL-03 writes them at the root next to [skill.tools], which reads
	// as an invitation to write [skill] for the metadata too; accepting both
	// costs nothing and turns a whole class of honest authoring mistakes into
	// a loaded skill instead of an ErrNoDescription rejection (REQ-SKILL-10).
	// The root wins where both define a key.
	var alias *tableReader
	if sub, ok := root.Sub("skill"); ok {
		alias = &tableReader{tbl: sub, path: path, used: map[string]bool{}}
	}

	m.Name = pick(r.str(&diags, "name"), alias.strOpt(&diags, "name"))
	m.Version = pick(r.str(&diags, "version"), alias.strOpt(&diags, "version"))
	m.Description = pick(r.str(&diags, "description"), alias.strOpt(&diags, "description"))
	m.Author = pick(r.str(&diags, "author"), alias.strOpt(&diags, "author"))
	m.SDKMinVersion = pick(r.str(&diags, "sdk_min_version"), alias.strOpt(&diags, "sdk_min_version"))
	m.Archetypes = pickList(r.strs(&diags, "archetypes"), alias.strsOpt(&diags, "archetypes"))
	m.Overrides = pickList(r.strs(&diags, "overrides"), alias.strsOpt(&diags, "overrides"))
	// Both are read unconditionally: `||` would short-circuit, leaving the
	// alias key unconsumed and then reported as unknown.
	rootDisable := r.boolean(&diags, "disable_model_invocation")
	aliasDisable := alias.boolOpt(&diags, "disable_model_invocation")
	m.DisableModelInvocation = rootDisable || aliasDisable

	skillTbl, _ := root.Sub("skill")
	if tools, ok := skillTbl.Sub("tools"); ok {
		tr := &tableReader{tbl: tools, path: path, used: map[string]bool{}}
		m.Tools.Module = tr.str(&diags, "module")
		m.Tools.Factory = tr.str(&diags, "factory")
		tr.reportUnknown(&diags)
	}
	if sec, ok := skillTbl.Sub("security"); ok {
		sr := &tableReader{tbl: sec, path: path, used: map[string]bool{}}
		m.Security.AllowlistExtend = sr.strs(&diags, "allowlist_extend")
		sr.reportUnknown(&diags)
	}
	if ses, ok := skillTbl.Sub("session"); ok {
		sr := &tableReader{tbl: ses, path: path, used: map[string]bool{}}
		m.Session.MaxTurnsAdd = int(sr.integer(&diags, "max_turns_add"))
		sr.reportUnknown(&diags)
	}
	if sa, ok := skillTbl.Sub("subagent"); ok {
		ar := &tableReader{tbl: sa, path: path, used: map[string]bool{}}
		m.Subagent.Archetype = ar.str(&diags, "archetype")
		m.Subagent.Mode = ar.str(&diags, "mode")
		m.Subagent.PromptTemplate = ar.str(&diags, "prompt_template")
		m.Subagent.ResultKey = ar.str(&diags, "result_key")
		m.Subagent.OnFailure = ar.str(&diags, "on_failure")
		ar.reportUnknown(&diags)
	}
	switch m.Subagent.OnFailure {
	case "":
		m.Subagent.OnFailure = "warn"
	case "abort", "warn", "skip":
	default:
		diags = append(diags, Diagnostic{
			Path: path, Severity: SeverityWarning, Line: subagentLine(skillTbl),
			Message: fmt.Sprintf("skill.subagent.on_failure %q is not abort|warn|skip; treating it as warn",
				m.Subagent.OnFailure),
		})
		m.Subagent.OnFailure = "warn"
	}

	// [skill] is a known table at the root; its contents are checked through
	// the alias reader below, not here.
	r.used["skill"] = true
	r.reportUnknown(&diags)
	if alias != nil {
		// The [skill] alias table legitimately holds the four known
		// sub-tables; they are consumed above, not unknown.
		alias.used["tools"] = true
		alias.used["security"] = true
		alias.used["session"] = true
		alias.used["subagent"] = true
		alias.reportUnknown(&diags)
	}

	if strings.TrimSpace(m.Description) == "" {
		return m, diags, ErrNoDescription
	}
	return m, diags, nil
}

func subagentLine(skillTbl *Table) int {
	if sub, ok := skillTbl.Sub("subagent"); ok {
		return sub.Line()
	}
	return 0
}

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func pickList(a, b []string) []string {
	if a != nil {
		return a
	}
	return b
}

// tableReader reads known keys out of a Table and remembers which it consumed,
// so the leftovers can be reported in written order.
type tableReader struct {
	tbl  *Table
	path string
	used map[string]bool
}

func (r *tableReader) typeWarn(diags *[]Diagnostic, key string, v Value, want string) {
	*diags = append(*diags, Diagnostic{
		Path: r.path, Line: v.Line, Severity: SeverityWarning,
		Message: fmt.Sprintf("%q is not %s; ignoring it", r.tbl.Qualify(key), want),
	})
}

func (r *tableReader) str(diags *[]Diagnostic, key string) string {
	if r == nil {
		return ""
	}
	r.used[key] = true
	v, ok := r.tbl.Get(key)
	if !ok {
		return ""
	}
	if v.Kind != KindString {
		r.typeWarn(diags, key, v, "a string")
		return ""
	}
	return v.Str
}

func (r *tableReader) strs(diags *[]Diagnostic, key string) []string {
	if r == nil {
		return nil
	}
	r.used[key] = true
	v, ok := r.tbl.Get(key)
	if !ok {
		return nil
	}
	if v.Kind != KindStringArray {
		r.typeWarn(diags, key, v, "an array of strings")
		return nil
	}
	return v.Array
}

func (r *tableReader) boolean(diags *[]Diagnostic, key string) bool {
	if r == nil {
		return false
	}
	r.used[key] = true
	v, ok := r.tbl.Get(key)
	if !ok {
		return false
	}
	if v.Kind != KindBool {
		r.typeWarn(diags, key, v, "a boolean")
		return false
	}
	return v.Bool
}

func (r *tableReader) integer(diags *[]Diagnostic, key string) int64 {
	if r == nil {
		return 0
	}
	r.used[key] = true
	v, ok := r.tbl.Get(key)
	if !ok {
		return 0
	}
	if v.Kind != KindInt {
		r.typeWarn(diags, key, v, "an integer")
		return 0
	}
	return v.Int
}

// The *Opt forms are nil-receiver-safe so the optional [skill] alias table can
// be read without a branch at every call site.
func (r *tableReader) strOpt(diags *[]Diagnostic, key string) string {
	if r == nil {
		return ""
	}
	return r.str(diags, key)
}

func (r *tableReader) strsOpt(diags *[]Diagnostic, key string) []string {
	if r == nil {
		return nil
	}
	return r.strs(diags, key)
}

func (r *tableReader) boolOpt(diags *[]Diagnostic, key string) bool {
	if r == nil {
		return false
	}
	return r.boolean(diags, key)
}

// reportUnknown warns about every key and sub-table the reader did not consume.
// Order follows the file, not map iteration, so the warnings are stable.
func (r *tableReader) reportUnknown(diags *[]Diagnostic) {
	if r == nil {
		return
	}
	for _, k := range r.tbl.Keys() {
		if r.used[k] {
			continue
		}
		v, _ := r.tbl.Get(k)
		msg := fmt.Sprintf("unknown key %q; ignoring it", r.tbl.Qualify(k))
		if why, removed := removedFields[k]; removed {
			msg = fmt.Sprintf("%q is %s", r.tbl.Qualify(k), why)
		}
		*diags = append(*diags, Diagnostic{
			Path: r.path, Line: v.Line, Severity: SeverityWarning, Message: msg,
		})
	}
	for _, k := range r.tbl.SubTables() {
		if r.used[k] {
			continue
		}
		sub, _ := r.tbl.Sub(k)
		*diags = append(*diags, Diagnostic{
			Path: r.path, Line: sub.Line(), Severity: SeverityWarning,
			Message: fmt.Sprintf("unknown table [%s]; ignoring it", sub.Name()),
		})
	}
}
