package skills

import "github.com/agentfox/agentkit-go/internal/toml"

// The hand-rolled TOML subset moved to internal/toml when `plugins` needed it
// too (REQ-PLUGIN-05). It is aliased back here rather than re-exported by
// value so that a *Table produced by either package is the same type — a
// parallel definition would compile and then fail at the one call site that
// crosses between them.
//
// It stays INTERNAL. It is a deliberate subset of TOML, not an implementation
// of it, and exporting it would promise a completeness it does not have
// (REQ-SKILL-10 decodes locally authored manifests leniently, which is the
// only contract it is built to meet).

type (
	Table       = toml.Table
	Value       = toml.Value
	ValueKind   = toml.ValueKind
	SyntaxError = toml.SyntaxError
)

const (
	KindString      = toml.KindString
	KindBool        = toml.KindBool
	KindInt         = toml.KindInt
	KindStringArray = toml.KindStringArray
)

// ParseTOML parses the subset.
func ParseTOML(src []byte) (*Table, []Diagnostic, error) { return toml.ParseTOML(src) }
