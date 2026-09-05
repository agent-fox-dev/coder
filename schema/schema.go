// Package schema is the structured, transformable tool-schema value type of
// REQ-TOOL-02 / REQ-GO-07. It contains no runtime reflection and no generated
// code; internal/policy asserts both.
package schema

import (
	"encoding/json"
	"errors"

	"github.com/agentfox/agentkit-go/jsonx"
)

type Type string

const (
	TypeNone    Type = ""
	TypeObject  Type = "object"
	TypeArray   Type = "array"
	TypeString  Type = "string"
	TypeInteger Type = "integer"
	TypeNumber  Type = "number"
	TypeBoolean Type = "boolean"
	TypeNull    Type = "null"
)

// Schema is a JSON Schema value AgentKit can rewrite, not a blob it can only
// forward. PropertyOrder is authoritative for emission order: a bare map
// cannot carry it because Go marshals maps with sorted keys unconditionally
// (REQ-PROV-16.5), and property order is model-visible.
type Schema struct {
	Type        Type
	Description string
	Title       string

	Properties    map[string]*Schema
	PropertyOrder []string
	Required      []string
	// AdditionalProperties is nil for absent; non-nil distinguishes
	// false / true / a subschema.
	AdditionalProperties *AdditionalProperties

	Items *Schema

	// Enum values are verbatim bytes in declaration order.
	Enum []json.RawMessage
	// Const + HasConst distinguish `const: null` from an absent const. This is
	// the case reflection cannot express, and it is REQ-TOOL-02's stated
	// reason for rejecting reflection.
	Const    json.RawMessage
	HasConst bool

	// Nullable on a concrete type marshals as a type array, e.g.
	// "type":["string","null"]. JSON Schema has no `nullable` keyword (that is
	// OpenAPI 3.0) and the PRD does not pin this; it is pinned by a golden.
	Nullable bool

	AnyOf []*Schema
	OneOf []*Schema
	AllOf []*Schema

	Minimum          *float64
	Maximum          *float64
	ExclusiveMinimum *float64
	ExclusiveMaximum *float64
	MultipleOf       *float64

	MinLength *int
	MaxLength *int
	Pattern   string
	Format    string

	MinItems    *int
	MaxItems    *int
	UniqueItems *bool

	// Extra carries passthrough keywords in authored order — the other case
	// reflection cannot express. It is also how a forbidden keyword such as
	// $ref sneaks past a strict rewrite that only inspects modelled fields,
	// which is why StrictSubset scans it.
	Extra jsonx.OrderedObject
}

type AdditionalProperties struct {
	Allowed bool
	Schema  *Schema // nil => a bare boolean
}

// Field is one entry of an Object(...) call.
type Field struct {
	Name     string
	Schema   *Schema
	Required bool
}

func Object(fields ...Field) *Schema {
	s := &Schema{Type: TypeObject, Properties: map[string]*Schema{}}
	for _, f := range fields {
		if _, dup := s.Properties[f.Name]; !dup {
			s.PropertyOrder = append(s.PropertyOrder, f.Name)
		}
		s.Properties[f.Name] = f.Schema
		if f.Required && !contains(s.Required, f.Name) {
			s.Required = append(s.Required, f.Name)
		}
	}
	return s
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func Prop(name string, s *Schema) Field { return Field{Name: name, Schema: s, Required: true} }
func Opt(name string, s *Schema) Field  { return Field{Name: name, Schema: s} }

// desc is variadic so both String() and String("...") read well; the §7
// sketches use both spellings. Only the first element is used.
func String(desc ...string) *Schema { return prim(TypeString, desc) }
func Int(desc ...string) *Schema    { return prim(TypeInteger, desc) }
func Number(desc ...string) *Schema { return prim(TypeNumber, desc) }
func Bool(desc ...string) *Schema   { return prim(TypeBoolean, desc) }

func prim(t Type, desc []string) *Schema {
	s := &Schema{Type: t}
	if len(desc) > 0 {
		s.Description = desc[0]
	}
	return s
}

func Array(items *Schema, desc ...string) *Schema {
	s := prim(TypeArray, desc)
	s.Items = items
	return s
}

func Enum(desc string, values ...string) *Schema {
	s := &Schema{Type: TypeString, Description: desc}
	for _, v := range values {
		b, _ := json.Marshal(v)
		s.Enum = append(s.Enum, b)
	}
	return s
}

func AnyOf(alts ...*Schema) *Schema { return &Schema{AnyOf: alts} }
func OneOf(alts ...*Schema) *Schema { return &Schema{OneOf: alts} }

func Const(v any) *Schema {
	b, _ := json.Marshal(v)
	return &Schema{Const: b, HasConst: true}
}

// ConstNull is the case a bool cannot express.
func ConstNull() *Schema { return &Schema{Const: json.RawMessage("null"), HasConst: true} }

// Modifiers mutate and return the receiver. That is safe because every
// combinator above returns a freshly allocated Schema.
func (s *Schema) Nullable_() *Schema        { s.Nullable = true; return s }
func (s *Schema) Describe(d string) *Schema { s.Description = d; return s }
func (s *Schema) Min(v float64) *Schema     { s.Minimum = &v; return s }
func (s *Schema) Max(v float64) *Schema     { s.Maximum = &v; return s }
func (s *Schema) MinItemsN(n int) *Schema   { s.MinItems = &n; return s }
func (s *Schema) Closed() *Schema {
	s.AdditionalProperties = &AdditionalProperties{Allowed: false}
	return s
}
func (s *Schema) WithExtra(key string, v any) *Schema {
	s.Extra.Set(key, jsonx.OV(v))
	return s
}

func (s *Schema) IsRequired(name string) bool { return contains(s.Required, name) }

// PropertyList returns PropertyOrder completed with any property absent from
// it (appended in sorted order) so a hand-built Schema still marshals
// deterministically.
func (s *Schema) PropertyList() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Properties))
	seen := map[string]bool{}
	for _, k := range s.PropertyOrder {
		if _, ok := s.Properties[k]; ok && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range s.Properties {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sortStrings(rest)
	return append(out, rest...)
}

func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

// Clone is a full deep copy. It is mandatory rather than convenient: the
// strict rewrite, per-provider dialect translation and the REQ-CACHE-06 schema
// cache all operate on a copy, and a Tool's schema is shared across turns and
// across agents.
func (s *Schema) Clone() *Schema {
	if s == nil {
		return nil
	}
	out := *s
	if s.Properties != nil {
		out.Properties = make(map[string]*Schema, len(s.Properties))
		for k, v := range s.Properties {
			out.Properties[k] = v.Clone()
		}
	}
	out.PropertyOrder = append([]string(nil), s.PropertyOrder...)
	out.Required = append([]string(nil), s.Required...)
	out.Items = s.Items.Clone()
	out.Extra = s.Extra.Clone()
	for _, alts := range []struct{ src, dst *[]*Schema }{
		{&s.AnyOf, &out.AnyOf}, {&s.OneOf, &out.OneOf}, {&s.AllOf, &out.AllOf},
	} {
		if *alts.src != nil {
			c := make([]*Schema, len(*alts.src))
			for i, a := range *alts.src {
				c[i] = a.Clone()
			}
			*alts.dst = c
		}
	}
	return &out
}

// MarshalJSON emits a fixed keyword order with `properties` written in
// PropertyList order, then Extra in authored order. Pinned by a golden.
func (s *Schema) MarshalJSON() ([]byte, error) { return marshalSchema(s) }

// --- strict subset (REQ-TOOL-03) ---

var ErrInvalidSchema = errors.New("agentkit: schema cannot be converted")

// StrictRewriteError names the keyword that made a schema unconvertible.
// REQ-TOOL-03's "require" mode must fail the request WITH the specific
// rejection reason, so the reason is a value, not a log line.
type StrictRewriteError struct {
	Path    string
	Keyword string
}

func (e *StrictRewriteError) Error() string {
	return "agentkit: schema at " + e.Path + " uses " + e.Keyword + ", which the strict subset forbids"
}
func (e *StrictRewriteError) Is(target error) bool { return target == ErrInvalidSchema }

// StrictSubset rewrites s into the target API's strict subset, on a Clone, so
// a probe never mutates the tool's shared schema.
func StrictSubset(s *Schema) (*Schema, error) { return strictSubset(s) }

// StrictSubsetOK is the pre-send probe REQ-TOOL-03 requires.
func StrictSubsetOK(s *Schema) bool { _, err := StrictSubset(s); return err == nil }

// --- validation (REQ-TOOL-11 steps 2-4) ---

var ErrArgumentValidation = errors.New("agentkit: tool arguments failed validation")

type Issue struct {
	Path    string
	Message string
}

type Coercion struct {
	Path string
	From Type
	To   Type
}

// ValidationError carries the model's OWN arguments in the order it wrote
// them so Error() can re-serialize them and the message is self-correcting
// (REQ-TOOL-11.4, REQ-TOOL-12.3).
type ValidationError struct {
	Issues []Issue
	Args   jsonx.OrderedObject
}

func (e *ValidationError) Error() string        { return renderValidationError(e) }
func (e *ValidationError) Is(target error) bool { return target == ErrArgumentValidation }

// DeleteOptionalNulls is REQ-TOOL-11 step 2. Constrained sampling forces the
// model to emit every declared property, so optional fields arrive as explicit
// nulls; treating them as present is a validation failure on well-formed
// output. Returns a copy.
func DeleteOptionalNulls(s *Schema, in jsonx.OrderedObject) jsonx.OrderedObject {
	return deleteOptionalNulls(s, in)
}

// Coerce is REQ-TOOL-11 step 3: string->number, string->bool, number->string,
// against the declared schema only. Never guesses without a declared type.
func Coerce(s *Schema, in jsonx.OrderedObject) (jsonx.OrderedObject, []Coercion) {
	return coerce(s, in)
}

// Validate is REQ-TOOL-11 step 4. It reports ALL issues, not the first, so one
// round trip fixes the whole call.
func Validate(s *Schema, in jsonx.OrderedObject) error { return validate(s, in) }
