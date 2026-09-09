package jsonx

import (
	"bytes"
	"encoding/json"
	"testing"
)

const fixture = `{"zeta":1,"alpha":{"yankee":true,"bravo":[{"zulu":"z","charlie":0},{"delta":null}]},"beta":"b","nums":{"big":9007199254740993,"exp":1e3,"trailing":1.10}}`

func TestThreeDepthByteIdentity(t *testing.T) {
	v, err := DecodeOrdered([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(fixture)) {
		t.Fatalf("not byte identical:\n want %s\n  got %s", fixture, got)
	}
}

// The negative control: proves the fixture actually discriminates. If this
// PASSES (bytes equal via map) the fixture is worthless.
func TestMapRouteLosesOrderAndNumbers(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(fixture), &m); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(m)
	if bytes.Equal(got, []byte(fixture)) {
		t.Fatal("fixture does not discriminate: map route reproduced it")
	}
	t.Logf("map route (what we must NOT do): %s", got)
}

func TestFixedPoint(t *testing.T) {
	once, _ := DecodeOrdered([]byte(fixture))
	b1, _ := once.MarshalJSON()
	twice, _ := DecodeOrdered(b1)
	b2, _ := twice.MarshalJSON()
	if !bytes.Equal(b1, b2) {
		t.Fatalf("not a fixed point:\n %s\n %s", b1, b2)
	}
}

func TestDuplicateKeyFirstPositionLastValue(t *testing.T) {
	o, err := DecodeOrderedObject([]byte(`{"a":1,"b":2,"a":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if o.Len() != 2 {
		t.Fatalf("want 2 members, got %d", o.Len())
	}
	if o[0].Key != "a" {
		t.Fatalf("first position not kept: %q", o[0].Key)
	}
	b, _ := o.MarshalJSON()
	if string(b) != `{"a":3,"b":2}` {
		t.Fatalf("want first-position/last-value, got %s", b)
	}
}

func TestMapMatchesOrderedMembers(t *testing.T) {
	o, _ := DecodeOrderedObject([]byte(fixture))
	m := o.Map()
	if len(m) != o.Len() {
		t.Fatalf("map has %d keys, ordered has %d", len(m), o.Len())
	}
	if _, ok := m["nums"].(map[string]any)["big"].(json.Number); !ok {
		t.Fatalf("big is %T, want json.Number", m["nums"].(map[string]any)["big"])
	}
}

// TestTrailingBytesAreRejected. The trailing-bytes check used dec.More, which
// reports whether another ELEMENT follows and answers false for a stray `]`
// or `}` exactly as it does for a clean end of input — so `{"a":1}]` decoded
// as `{"a":1}`. Anything after the value is a rejection.
func TestTrailingBytesAreRejected(t *testing.T) {
	for _, in := range []string{`{"a":1}]`, `{"a":1}}`, `[1]}`, `{"a":1} x`, `1 2`, `"s" "t"`, `{"a":1}{}`} {
		if _, err := DecodeOrdered([]byte(in)); err == nil {
			t.Errorf("%s was accepted; the bytes after the value are not JSON", in)
		} else if err != ErrTrailingBytes {
			t.Errorf("%s: err = %v, want ErrTrailingBytes", in, err)
		}
	}
	for _, in := range []string{`{"a":1}`, ` {"a":1} `, "[1]\n", `1`, `"s"`, `null`} {
		if _, err := DecodeOrdered([]byte(in)); err != nil {
			t.Errorf("%q was rejected: %v", in, err)
		}
	}
}
