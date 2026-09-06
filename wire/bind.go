package wire

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// Validator is REQ-SEC-12.3's hook: it runs as soon as each struct is filled,
// for constraints the Go type shape cannot express — minLength, minimum,
// literal unions.
//
// It runs INSIDE the bind rather than after it, so a nested struct that fails
// its own constraint is reported at its own path. Validating only the root
// leaves the caller to find which of forty elements was wrong.
type Validator interface {
	Validate() error
}

// SafeInteger is REQ-SEC-12.2's bound: the largest integer an IEEE-754 double
// represents exactly.
const SafeInteger = 1<<53 - 1

// Bind maps a decoded tree onto a Go value, strictly (REQ-SEC-12).
//
// Strict means an unknown property is a REJECTION. A peer that can smuggle
// extra fields past the parser reaches code paths the schema was meant to
// gate, and the smuggled field is invisible in every review of the struct that
// was supposed to describe the message.
//
// Field names match EXACTLY — no case folding. encoding/json matches
// case-insensitively, and on this surface that is a hole rather than a
// convenience: `id` and `Id` are two distinct keys, so duplicate-key rejection
// does not catch them, and case-insensitive matching then binds both to one
// field with the last one winning. That is precisely the last-wins REQ-SEC-11.3
// exists to prevent, reintroduced one layer up.
func Bind(v Value, target any) error {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return failf(RuleType, "$", "Bind needs a non-nil pointer, got %T", target)
	}
	return bindValue(v, rv.Elem(), "$")
}

func bindValue(v Value, dst reflect.Value, path string) error {
	// REQ-SEC-12.4: an explicit null must never panic a reflective setter. A
	// peer that can crash the process by sending `null` for a tool input has
	// found a denial of service with a two-word payload.
	if v.Kind == KindNull {
		if dst.CanSet() {
			dst.Set(reflect.Zero(dst.Type()))
		}
		return nil
	}

	// json.RawMessage and []byte-shaped targets take the re-encoded value, so
	// a caller can carry an opaque payload through without the binder having
	// to model it.
	if dst.Type() == rawMessageType {
		b, err := v.JSON()
		if err != nil {
			return failf(RuleType, path, "%v", err)
		}
		dst.SetBytes(b)
		return nil
	}

	switch dst.Kind() {
	case reflect.Pointer:
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		return bindValue(v, dst.Elem(), path)

	case reflect.Interface:
		// An untyped field takes the tree as ordinary Go values. It is the one
		// place strictness does not apply, because there is no schema to be
		// strict against — which is why REQ-SEC-12 also wants a Validator.
		if dst.NumMethod() != 0 {
			return failf(RuleType, path, "cannot bind into a non-empty interface %s", dst.Type())
		}
		a, err := v.Any()
		if err != nil {
			return failf(RuleType, path, "%v", err)
		}
		dst.Set(reflect.ValueOf(a))
		return nil

	case reflect.Struct:
		return bindStruct(v, dst, path)

	case reflect.Slice:
		if v.Kind != KindArray {
			return typeErr(path, "array", v)
		}
		out := reflect.MakeSlice(dst.Type(), len(v.Array), len(v.Array))
		for i := range v.Array {
			if err := bindValue(v.Array[i], out.Index(i), path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
		dst.Set(out)
		return nil

	case reflect.Map:
		if v.Kind != KindObject {
			return typeErr(path, "object", v)
		}
		if dst.Type().Key().Kind() != reflect.String {
			return failf(RuleType, path, "only string-keyed maps can be bound, got %s", dst.Type())
		}
		out := reflect.MakeMapWithSize(dst.Type(), len(v.Keys))
		elemType := dst.Type().Elem()
		for _, k := range v.Keys {
			elem := reflect.New(elemType).Elem()
			if err := bindValue(v.Object[k], elem, path+"."+k); err != nil {
				return err
			}
			out.SetMapIndex(reflect.ValueOf(k).Convert(dst.Type().Key()), elem)
		}
		dst.Set(out)
		return nil

	case reflect.String:
		if v.Kind != KindString {
			return typeErr(path, "string", v)
		}
		dst.SetString(v.String)
		return nil

	case reflect.Bool:
		if v.Kind != KindBool {
			return typeErr(path, "bool", v)
		}
		dst.SetBool(v.Bool)
		return nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := integerFrom(v, path)
		if err != nil {
			return err
		}
		if dst.OverflowInt(n) {
			return failf(RuleRange, path, "%d overflows %s", n, dst.Type())
		}
		dst.SetInt(n)
		return nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := integerFrom(v, path)
		if err != nil {
			return err
		}
		if n < 0 {
			return failf(RuleRange, path, "%d is negative for %s", n, dst.Type())
		}
		if dst.OverflowUint(uint64(n)) {
			return failf(RuleRange, path, "%d overflows %s", n, dst.Type())
		}
		dst.SetUint(uint64(n))
		return nil

	case reflect.Float32, reflect.Float64:
		if v.Kind != KindNumber {
			return typeErr(path, "number", v)
		}
		// REQ-SEC-12.2's other direction: an INTEGER satisfies a float field.
		// A peer with a single number type writes 1 where it means 1.0, and a
		// decoder that refuses it rejects legal messages.
		f, err := v.Number.Float64()
		if err != nil {
			return failf(RuleType, path, "malformed number %q", v.Number)
		}
		if dst.OverflowFloat(f) {
			return failf(RuleRange, path, "%v overflows %s", f, dst.Type())
		}
		dst.SetFloat(f)
		return nil
	}
	return failf(RuleType, path, "cannot bind into %s", dst.Type())
}

// integerFrom implements REQ-SEC-12.2's integer direction.
//
// An INTEGRAL FLOAT satisfies an integer field — a peer whose runtime has one
// number type emits 1.0 for 1 — but only inside the IEEE-754 safe-integer
// range. Outside it a double no longer represents every integer, so `1e19`
// does not mean any particular integer and accepting it would invent one.
func integerFrom(v Value, path string) (int64, error) {
	if v.Kind != KindNumber {
		return 0, typeErr(path, "number", v)
	}
	lit := string(v.Number)
	if n, err := strconv.ParseInt(lit, 10, 64); err == nil {
		return n, nil
	}
	f, err := v.Number.Float64()
	if err != nil {
		return 0, failf(RuleType, path, "malformed number %q", lit)
	}
	if math.Trunc(f) != f || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, failf(RuleType, path, "%s is not an integer", lit)
	}
	if f > SafeInteger || f < -SafeInteger {
		return 0, failf(RuleRange, path,
			"%s is outside the IEEE-754 safe-integer range, so it does not name an exact integer", lit)
	}
	return int64(f), nil
}

func typeErr(path, want string, got Value) error {
	return failf(RuleType, path, "expected %s, got %s", want, got.Kind)
}

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

func bindStruct(v Value, dst reflect.Value, path string) error {
	if v.Kind != KindObject {
		return typeErr(path, "object", v)
	}
	fields := structFields(dst.Type())

	for _, key := range v.Keys {
		f, ok := fields[key]
		if !ok {
			// REQ-SEC-12.1.
			return failf(RuleUnknownField, path+"."+key,
				"unknown property %q; %s declares %s", key, dst.Type(), knownList(fields))
		}
		target := dst
		for _, i := range f.index {
			if target.Kind() == reflect.Pointer {
				if target.IsNil() {
					target.Set(reflect.New(target.Type().Elem()))
				}
				target = target.Elem()
			}
			target = target.Field(i)
		}
		if err := bindValue(v.Object[key], target, path+"."+key); err != nil {
			return err
		}
	}

	// REQ-SEC-12.3: the hook runs as soon as THIS struct is filled, so a
	// nested failure is reported at its own path.
	if val, ok := validatorOf(dst); ok {
		if err := val.Validate(); err != nil {
			return &Error{Rule: RuleValidator, Path: path, Msg: err.Error(), Err: err}
		}
	}
	return nil
}

func validatorOf(v reflect.Value) (Validator, bool) {
	if val, ok := v.Interface().(Validator); ok {
		return val, true
	}
	if v.CanAddr() {
		if val, ok := v.Addr().Interface().(Validator); ok {
			return val, true
		}
	}
	return nil, false
}

type fieldInfo struct {
	name  string
	index []int
}

func knownList(fields map[string]fieldInfo) string {
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	// Sorted, so the message is the same on every run and diffable in a log.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	if len(names) == 0 {
		return "no properties"
	}
	return strings.Join(names, ", ")
}

// structFields collects bindable fields, flattening anonymous embedding.
func structFields(t reflect.Type) map[string]fieldInfo {
	out := map[string]fieldInfo{}
	var walk func(reflect.Type, []int)
	walk = func(t reflect.Type, prefix []int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			idx := append(append([]int(nil), prefix...), i)

			tag := f.Tag.Get("json")
			name, _, _ := strings.Cut(tag, ",")
			if name == "-" {
				// Explicitly excluded, so the property is UNKNOWN rather than
				// silently ignored — which is the whole difference between
				// strict and lenient.
				continue
			}

			if f.Anonymous && name == "" {
				ft := f.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					walk(ft, idx)
					continue
				}
			}
			if !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			if _, exists := out[name]; !exists {
				out[name] = fieldInfo{name: name, index: idx}
			}
		}
	}
	walk(t, nil)
	return out
}

// ---------------------------------------------------------------- projection

// Any materializes the tree as ordinary Go values: map[string]any, []any,
// json.Number, string, bool, nil.
//
// Numbers come back as json.Number, never float64. A tool argument of
// 9007199254740993 that round-trips through a float64 comes back as
// 9007199254740992, and nothing downstream can tell.
func (v Value) Any() (any, error) {
	switch v.Kind {
	case KindNull:
		return nil, nil
	case KindBool:
		return v.Bool, nil
	case KindNumber:
		return v.Number, nil
	case KindString:
		return v.String, nil
	case KindArray:
		out := make([]any, len(v.Array))
		for i := range v.Array {
			a, err := v.Array[i].Any()
			if err != nil {
				return nil, err
			}
			out[i] = a
		}
		return out, nil
	case KindObject:
		out := make(map[string]any, len(v.Keys))
		for _, k := range v.Keys {
			a, err := v.Object[k].Any()
			if err != nil {
				return nil, err
			}
			out[k] = a
		}
		return out, nil
	}
	return nil, failf(RuleType, "$", "unknown kind %d", v.Kind)
}

// JSON re-encodes the tree, preserving member order and number literals.
func (v Value) JSON() ([]byte, error) {
	var b strings.Builder
	if err := v.writeJSON(&b); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func (v Value) writeJSON(b *strings.Builder) error {
	switch v.Kind {
	case KindNull:
		b.WriteString("null")
	case KindBool:
		if v.Bool {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case KindNumber:
		if v.Number == "" {
			b.WriteString("0")
			return nil
		}
		b.WriteString(string(v.Number))
	case KindString:
		enc, err := json.Marshal(v.String)
		if err != nil {
			return err
		}
		b.Write(enc)
	case KindArray:
		b.WriteByte('[')
		for i := range v.Array {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := v.Array[i].writeJSON(b); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case KindObject:
		b.WriteByte('{')
		for i, k := range v.Keys {
			if i > 0 {
				b.WriteByte(',')
			}
			enc, err := json.Marshal(k)
			if err != nil {
				return err
			}
			b.Write(enc)
			b.WriteByte(':')
			if err := v.Object[k].writeJSON(b); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	}
	return nil
}
