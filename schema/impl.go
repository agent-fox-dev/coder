package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/agentfox/agentkit-go/jsonx"
)

// forbiddenStrict is REQ-TOOL-03's rejection list. It is checked against
// modelled fields AND against Extra keys, because Extra is exactly how $ref
// gets in.
var forbiddenStrict = []string{
	"$ref", "$defs", "allOf", "oneOf", "not",
	"patternProperties", "prefixItems", "if", "then", "else", "dependentSchemas",
}

func marshalSchema(s *Schema) ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	kv := func(k string, raw []byte) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		km, _ := json.Marshal(k)
		b.Write(km)
		b.WriteByte(':')
		b.Write(raw)
	}
	str := func(k, v string) {
		if v == "" {
			return
		}
		vm, _ := json.Marshal(v)
		kv(k, vm)
	}

	if s.Type != TypeNone {
		if s.Nullable {
			arr, _ := json.Marshal([]string{string(s.Type), string(TypeNull)})
			kv("type", arr)
		} else {
			tm, _ := json.Marshal(string(s.Type))
			kv("type", tm)
		}
	}
	str("title", s.Title)
	str("description", s.Description)

	if s.Type == TypeObject || len(s.Properties) > 0 {
		var pb bytes.Buffer
		pb.WriteByte('{')
		for i, name := range s.PropertyList() {
			if i > 0 {
				pb.WriteByte(',')
			}
			nm, _ := json.Marshal(name)
			pb.Write(nm)
			pb.WriteByte(':')
			sub, err := marshalSchema(s.Properties[name])
			if err != nil {
				return nil, err
			}
			pb.Write(sub)
		}
		pb.WriteByte('}')
		kv("properties", pb.Bytes())
	}
	if s.Required != nil {
		rm, _ := json.Marshal(s.Required)
		kv("required", rm)
	}
	if s.AdditionalProperties != nil {
		if s.AdditionalProperties.Schema != nil {
			sub, err := marshalSchema(s.AdditionalProperties.Schema)
			if err != nil {
				return nil, err
			}
			kv("additionalProperties", sub)
		} else {
			kv("additionalProperties", []byte(strconv.FormatBool(s.AdditionalProperties.Allowed)))
		}
	}
	if s.Items != nil {
		sub, err := marshalSchema(s.Items)
		if err != nil {
			return nil, err
		}
		kv("items", sub)
	}
	if len(s.Enum) > 0 {
		var eb bytes.Buffer
		eb.WriteByte('[')
		for i, e := range s.Enum {
			if i > 0 {
				eb.WriteByte(',')
			}
			eb.Write(e)
		}
		eb.WriteByte(']')
		kv("enum", eb.Bytes())
	}
	if s.HasConst {
		kv("const", s.Const)
	}
	for _, g := range []struct {
		k string
		v []*Schema
	}{{"anyOf", s.AnyOf}, {"oneOf", s.OneOf}, {"allOf", s.AllOf}} {
		if len(g.v) == 0 {
			continue
		}
		var ab bytes.Buffer
		ab.WriteByte('[')
		for i, a := range g.v {
			if i > 0 {
				ab.WriteByte(',')
			}
			sub, err := marshalSchema(a)
			if err != nil {
				return nil, err
			}
			ab.Write(sub)
		}
		ab.WriteByte(']')
		kv(g.k, ab.Bytes())
	}
	for _, n := range []struct {
		k string
		v *float64
	}{{"minimum", s.Minimum}, {"maximum", s.Maximum},
		{"exclusiveMinimum", s.ExclusiveMinimum}, {"exclusiveMaximum", s.ExclusiveMaximum},
		{"multipleOf", s.MultipleOf}} {
		if n.v != nil {
			kv(n.k, []byte(strconv.FormatFloat(*n.v, 'g', -1, 64)))
		}
	}
	for _, n := range []struct {
		k string
		v *int
	}{{"minLength", s.MinLength}, {"maxLength", s.MaxLength},
		{"minItems", s.MinItems}, {"maxItems", s.MaxItems}} {
		if n.v != nil {
			kv(n.k, []byte(strconv.Itoa(*n.v)))
		}
	}
	str("pattern", s.Pattern)
	str("format", s.Format)
	if s.UniqueItems != nil {
		kv("uniqueItems", []byte(strconv.FormatBool(*s.UniqueItems)))
	}
	// Extra last, in authored order.
	for _, m := range s.Extra {
		raw, err := m.Value.MarshalJSON()
		if err != nil {
			return nil, err
		}
		kv(m.Key, raw)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func strictSubset(s *Schema) (*Schema, error) {
	c := s.Clone()
	if err := strictCheck(c, ""); err != nil {
		return nil, err
	}
	strictRewrite(c)
	return c, nil
}

func strictCheck(s *Schema, path string) error {
	if s == nil {
		return nil
	}
	if len(s.AllOf) > 0 {
		return &StrictRewriteError{Path: pathOr(path), Keyword: "allOf"}
	}
	if len(s.OneOf) > 0 {
		return &StrictRewriteError{Path: pathOr(path), Keyword: "oneOf"}
	}
	for _, m := range s.Extra {
		for _, f := range forbiddenStrict {
			if m.Key == f {
				return &StrictRewriteError{Path: pathOr(path), Keyword: f}
			}
		}
	}
	for _, name := range s.PropertyList() {
		if err := strictCheck(s.Properties[name], path+"/"+name); err != nil {
			return err
		}
	}
	if err := strictCheck(s.Items, path+"/items"); err != nil {
		return err
	}
	for i, a := range s.AnyOf {
		if err := strictCheck(a, path+"/anyOf/"+strconv.Itoa(i)); err != nil {
			return err
		}
	}
	return nil
}

func pathOr(p string) string {
	if p == "" {
		return "(root)"
	}
	return p
}

func strictRewrite(s *Schema) {
	if s == nil {
		return
	}
	if s.Type == TypeObject {
		s.AdditionalProperties = &AdditionalProperties{Allowed: false}
		all := s.PropertyList()
		for _, name := range all {
			if !s.IsRequired(name) {
				p := s.Properties[name]
				// Widen a formerly-optional, non-nullable property so the
				// model can still omit it.
				if p != nil && !p.Nullable {
					s.Properties[name] = &Schema{
						Description: p.Description,
						AnyOf:       []*Schema{p, {Type: TypeNull}},
					}
				}
			}
		}
		s.Required = append([]string(nil), all...)
	}
	for _, name := range s.PropertyList() {
		strictRewrite(s.Properties[name])
	}
	strictRewrite(s.Items)
	for _, a := range s.AnyOf {
		strictRewrite(a)
	}
}

func deleteOptionalNulls(s *Schema, in jsonx.OrderedObject) jsonx.OrderedObject {
	out := make(jsonx.OrderedObject, 0, len(in))
	for _, m := range in {
		if s != nil && m.Value.Kind == jsonx.KindNull && !s.IsRequired(m.Key) {
			if _, declared := s.Properties[m.Key]; declared {
				continue
			}
		}
		v := m.Value
		var sub *Schema
		if s != nil {
			sub = s.Properties[m.Key]
		}
		out = append(out, jsonx.Member{Key: m.Key, Value: descendNulls(sub, v)})
	}
	return out
}

func descendNulls(s *Schema, v jsonx.OrderedValue) jsonx.OrderedValue {
	switch v.Kind {
	case jsonx.KindObject:
		v.Object = deleteOptionalNulls(s, v.Object)
	case jsonx.KindArray:
		var item *Schema
		if s != nil {
			item = s.Items
		}
		arr := make([]jsonx.OrderedValue, len(v.Array))
		for i := range v.Array {
			arr[i] = descendNulls(item, v.Array[i])
		}
		v.Array = arr
	}
	return v
}

func coerce(s *Schema, in jsonx.OrderedObject) (jsonx.OrderedObject, []Coercion) {
	var log []Coercion
	out := make(jsonx.OrderedObject, len(in))
	for i, m := range in {
		var sub *Schema
		if s != nil {
			sub = s.Properties[m.Key]
		}
		v, l := coerceValue(sub, m.Value, m.Key)
		log = append(log, l...)
		out[i] = jsonx.Member{Key: m.Key, Value: v}
	}
	return out, log
}

func coerceValue(s *Schema, v jsonx.OrderedValue, path string) (jsonx.OrderedValue, []Coercion) {
	if s == nil {
		return v, nil
	}
	switch v.Kind {
	case jsonx.KindObject:
		o, l := coerce(s, v.Object)
		v.Object = o
		return v, l
	case jsonx.KindArray:
		var log []Coercion
		arr := make([]jsonx.OrderedValue, len(v.Array))
		for i := range v.Array {
			e, l := coerceValue(s.Items, v.Array[i], path+"/"+strconv.Itoa(i))
			arr[i] = e
			log = append(log, l...)
		}
		v.Array = arr
		return v, log
	case jsonx.KindString:
		var str string
		_ = json.Unmarshal(v.Scalar, &str)
		switch s.Type {
		case TypeInteger, TypeNumber:
			if _, err := strconv.ParseFloat(str, 64); err == nil {
				return jsonx.OrderedValue{Kind: jsonx.KindNumber, Scalar: json.RawMessage(str)},
					[]Coercion{{Path: path, From: TypeString, To: s.Type}}
			}
		case TypeBoolean:
			if b, err := strconv.ParseBool(str); err == nil {
				lit := "false"
				if b {
					lit = "true"
				}
				return jsonx.OrderedValue{Kind: jsonx.KindBool, Scalar: json.RawMessage(lit)},
					[]Coercion{{Path: path, From: TypeString, To: TypeBoolean}}
			}
		}
	case jsonx.KindNumber:
		if s.Type == TypeString {
			q, _ := json.Marshal(string(v.Scalar))
			return jsonx.OrderedValue{Kind: jsonx.KindString, Scalar: q},
				[]Coercion{{Path: path, From: TypeNumber, To: TypeString}}
		}
	}
	return v, nil
}

func validate(s *Schema, in jsonx.OrderedObject) error {
	var issues []Issue
	validateObject(s, in, "", &issues)
	if len(issues) == 0 {
		return nil
	}
	return &ValidationError{Issues: issues, Args: in}
}

func validateObject(s *Schema, in jsonx.OrderedObject, path string, issues *[]Issue) {
	if s == nil {
		return
	}
	for _, req := range s.Required {
		if _, ok := in.Get(req); !ok {
			*issues = append(*issues, Issue{Path: join(path, req), Message: "required property is missing"})
		}
	}
	for _, m := range in {
		sub := s.Properties[m.Key]
		if sub == nil {
			continue
		}
		validateValue(sub, m.Value, join(path, m.Key), issues)
	}
}

func validateValue(s *Schema, v jsonx.OrderedValue, path string, issues *[]Issue) {
	switch s.Type {
	case TypeObject:
		if v.Kind != jsonx.KindObject {
			*issues = append(*issues, Issue{Path: path, Message: "expected object, got " + kindName(v.Kind)})
			return
		}
		validateObject(s, v.Object, path, issues)
	case TypeArray:
		if v.Kind != jsonx.KindArray {
			*issues = append(*issues, Issue{Path: path, Message: "expected array, got " + kindName(v.Kind)})
			return
		}
		if s.MinItems != nil && len(v.Array) < *s.MinItems {
			*issues = append(*issues, Issue{Path: path,
				Message: fmt.Sprintf("expected at least %d items, got %d", *s.MinItems, len(v.Array))})
		}
		if s.Items != nil {
			for i := range v.Array {
				validateValue(s.Items, v.Array[i], path+"/"+strconv.Itoa(i), issues)
			}
		}
	case TypeString:
		if v.Kind != jsonx.KindString && !(v.Kind == jsonx.KindNull && s.Nullable) {
			*issues = append(*issues, Issue{Path: path, Message: "expected string, got " + kindName(v.Kind)})
		}
	case TypeInteger, TypeNumber:
		if v.Kind != jsonx.KindNumber && !(v.Kind == jsonx.KindNull && s.Nullable) {
			*issues = append(*issues, Issue{Path: path, Message: "expected " + string(s.Type) + ", got " + kindName(v.Kind)})
		}
	case TypeBoolean:
		if v.Kind != jsonx.KindBool && !(v.Kind == jsonx.KindNull && s.Nullable) {
			*issues = append(*issues, Issue{Path: path, Message: "expected boolean, got " + kindName(v.Kind)})
		}
	}
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "/" + k
}

func kindName(k jsonx.ValueKind) string {
	switch k {
	case jsonx.KindNull:
		return "null"
	case jsonx.KindBool:
		return "boolean"
	case jsonx.KindNumber:
		return "number"
	case jsonx.KindString:
		return "string"
	case jsonx.KindArray:
		return "array"
	}
	return "object"
}

// renderValidationError echoes the model's OWN arguments in the model's own
// key order, which is what makes the correction self-serving (REQ-TOOL-12.3).
func renderValidationError(e *ValidationError) string {
	var b strings.Builder
	b.WriteString("Invalid arguments:\n")
	for _, is := range e.Issues {
		b.WriteString("  - ")
		b.WriteString(is.Path)
		b.WriteString(": ")
		b.WriteString(is.Message)
		b.WriteByte('\n')
	}
	b.WriteString("Arguments received:\n")
	raw, err := e.Args.MarshalJSON()
	if err == nil {
		b.Write(raw)
	}
	return b.String()
}
