// Package jsonx holds order-preserving JSON primitives. It imports nothing
// outside the standard library and nothing inside AgentKit.
//
// It exists because REQ-TOOL-12 makes tool-call argument key order a
// model-visible property, and Go's encoding/json sorts map keys
// unconditionally. Every path that must reproduce bytes goes through here.
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ValueKind discriminates an OrderedValue.
type ValueKind uint8

const (
	KindNull ValueKind = iota
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
)

// OrderedValue is one JSON value with object key order preserved at every
// depth. Scalars are held as the verbatim source bytes, so no value is
// laundered through float64 (NFR-TEST-03 d): 1e3 stays "1e3", 1024.0 stays
// "1024.0", and 9007199254740993 does not lose its low bit.
type OrderedValue struct {
	Kind   ValueKind
	Scalar json.RawMessage // set when Kind <= KindString
	Array  []OrderedValue  // set when Kind == KindArray
	Object OrderedObject   // set when Kind == KindObject
}

// Member is one key/value pair of an OrderedObject.
type Member struct {
	Key   string
	Value OrderedValue
}

// OrderedObject is a JSON object as an ordered slice. MarshalJSON writes
// members in slice order (REQ-TOOL-12).
//
// Duplicate keys in the source collapse to ONE member holding the FIRST
// position and the LAST value. That makes len(ordered) == len(decoded map)
// with identical values, which is what "checkable against the decoded map"
// requires. The choice of position is arbitrary and is therefore pinned by a
// test rather than left to whoever writes the next decoder.
type OrderedObject []Member

var ErrTrailingBytes = errors.New("jsonx: trailing bytes after JSON value")

// DecodeOrdered is the single decode pass. It rejects trailing bytes and
// decodes numbers with UseNumber so literals survive.
func DecodeOrdered(data []byte) (OrderedValue, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v OrderedValue
	if err := v.decode(dec); err != nil {
		return OrderedValue{}, err
	}
	if dec.More() {
		return OrderedValue{}, ErrTrailingBytes
	}
	return v, nil
}

// DecodeOrderedObject requires the top-level value to be an object. It is the
// entry point for tool-call arguments.
func DecodeOrderedObject(data []byte) (OrderedObject, error) {
	v, err := DecodeOrdered(data)
	if err != nil {
		return nil, err
	}
	if v.Kind != KindObject {
		return nil, fmt.Errorf("jsonx: expected object, got kind %d", v.Kind)
	}
	return v.Object, nil
}

func (v *OrderedValue) decode(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return v.decodeFrom(dec, tok)
}

func (v *OrderedValue) decodeFrom(dec *json.Decoder, tok json.Token) error {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v.Kind = KindObject
			v.Object = OrderedObject{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := kt.(string)
				if !ok {
					return fmt.Errorf("jsonx: object key is not a string")
				}
				var mv OrderedValue
				if err := mv.decode(dec); err != nil {
					return err
				}
				v.Object.Set(key, mv) // first position, last value
			}
			_, err := dec.Token() // consume '}'
			return err
		case '[':
			v.Kind = KindArray
			v.Array = []OrderedValue{}
			for dec.More() {
				var ev OrderedValue
				if err := ev.decode(dec); err != nil {
					return err
				}
				v.Array = append(v.Array, ev)
			}
			_, err := dec.Token() // consume ']'
			return err
		}
		return fmt.Errorf("jsonx: unexpected delimiter %v", t)
	case nil:
		v.Kind, v.Scalar = KindNull, json.RawMessage("null")
		return nil
	case bool:
		v.Kind = KindBool
		if t {
			v.Scalar = json.RawMessage("true")
		} else {
			v.Scalar = json.RawMessage("false")
		}
		return nil
	case json.Number:
		v.Kind, v.Scalar = KindNumber, json.RawMessage(t.String())
		return nil
	case string:
		v.Kind = KindString
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		v.Scalar = b
		return nil
	}
	return fmt.Errorf("jsonx: unexpected token %T", tok)
}

func (v OrderedValue) MarshalJSON() ([]byte, error) {
	switch v.Kind {
	case KindNull, KindBool, KindNumber, KindString:
		if len(v.Scalar) == 0 {
			return []byte("null"), nil
		}
		return append([]byte(nil), v.Scalar...), nil
	case KindArray:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, e := range v.Array {
			if i > 0 {
				buf.WriteByte(',')
			}
			b, err := e.MarshalJSON()
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	case KindObject:
		return v.Object.MarshalJSON()
	}
	return nil, fmt.Errorf("jsonx: bad kind %d", v.Kind)
}

func (v *OrderedValue) UnmarshalJSON(b []byte) error {
	got, err := DecodeOrdered(b)
	if err != nil {
		return err
	}
	*v = got
	return nil
}

// MarshalJSON writes members in slice order. A nil OrderedObject marshals as
// {} and never as null.
func (o OrderedObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(m.Key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		v, err := m.Value.MarshalJSON()
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (o *OrderedObject) UnmarshalJSON(b []byte) error {
	got, err := DecodeOrderedObject(b)
	if err != nil {
		return err
	}
	*o = got
	return nil
}

func (o OrderedObject) Index(key string) int {
	for i := range o {
		if o[i].Key == key {
			return i
		}
	}
	return -1
}

func (o OrderedObject) Get(key string) (OrderedValue, bool) {
	if i := o.Index(key); i >= 0 {
		return o[i].Value, true
	}
	return OrderedValue{}, false
}

func (o OrderedObject) Len() int { return len(o) }

// Set replaces in place when the key is present (keeping its position) and
// appends otherwise.
func (o *OrderedObject) Set(key string, v OrderedValue) {
	if i := o.Index(key); i >= 0 {
		(*o)[i].Value = v
		return
	}
	*o = append(*o, Member{Key: key, Value: v})
}

func (o *OrderedObject) Delete(key string) bool {
	i := o.Index(key)
	if i < 0 {
		return false
	}
	*o = append((*o)[:i], (*o)[i+1:]...)
	return true
}

func (o OrderedObject) Clone() OrderedObject {
	if o == nil {
		return nil
	}
	out := make(OrderedObject, len(o))
	for i, m := range o {
		out[i] = Member{Key: m.Key, Value: m.Value.Clone()}
	}
	return out
}

func (v OrderedValue) Clone() OrderedValue {
	out := OrderedValue{Kind: v.Kind}
	if v.Scalar != nil {
		out.Scalar = append(json.RawMessage(nil), v.Scalar...)
	}
	if v.Array != nil {
		out.Array = make([]OrderedValue, len(v.Array))
		for i := range v.Array {
			out.Array[i] = v.Array[i].Clone()
		}
	}
	out.Object = v.Object.Clone()
	return out
}

// Any derives the decoded form from the ordered form with no re-parse.
// Numbers come back as json.Number, NOT float64 (NFR-TEST-03 d).
func (v OrderedValue) Any() any {
	switch v.Kind {
	case KindNull:
		return nil
	case KindBool:
		return bytes.Equal(v.Scalar, []byte("true"))
	case KindNumber:
		return json.Number(v.Scalar)
	case KindString:
		var s string
		_ = json.Unmarshal(v.Scalar, &s)
		return s
	case KindArray:
		out := make([]any, len(v.Array))
		for i := range v.Array {
			out[i] = v.Array[i].Any()
		}
		return out
	case KindObject:
		return v.Object.Map()
	}
	return nil
}

func (o OrderedObject) Map() map[string]any {
	out := make(map[string]any, len(o))
	for _, m := range o {
		out[m.Key] = m.Value.Any()
	}
	return out
}

// OV builds an OrderedValue from an arbitrary Go value by marshalling and
// re-decoding. It panics on non-JSON-able input; use it in tests and in
// combinators, never on a hot path.
func OV(v any) OrderedValue {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("jsonx.OV: %v", err))
	}
	got, err := DecodeOrdered(b)
	if err != nil {
		panic(fmt.Sprintf("jsonx.OV: %v", err))
	}
	return got
}

func OVRaw(b json.RawMessage) OrderedValue {
	got, err := DecodeOrdered(b)
	if err != nil {
		return OrderedValue{Kind: KindNull, Scalar: json.RawMessage("null")}
	}
	return got
}

func OVString(s string) OrderedValue {
	b, _ := json.Marshal(s)
	return OrderedValue{Kind: KindString, Scalar: b}
}

func OVNumber(n json.Number) OrderedValue {
	return OrderedValue{Kind: KindNumber, Scalar: json.RawMessage(n.String())}
}
