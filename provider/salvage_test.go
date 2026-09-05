package provider_test

import (
	"encoding/json"
	"testing"

	"github.com/agentfox/agentkit-go/provider"
)

// TestValidJSONIsReturnedByteForByte is REQ-PROV-17 at the salvage boundary.
//
// The overwhelmingly common case is a complete tool call, and the salvage path
// must be a NO-OP on it. A repair that "normalizes" valid input would reorder
// keys on every replayed turn, shift the provider's prompt-cache prefix, and
// show up only in the bill.
func TestValidJSONIsReturnedByteForByte(t *testing.T) {
	for _, in := range []string{
		`{"zebra":1,"apple":2}`,
		`{ "spaced" : [ 1 , 2 ] }`,
		`{"nested":{"b":1,"a":2}}`,
		`{}`,
	} {
		out, repaired := provider.SalvageJSON([]byte(in))
		if repaired {
			t.Fatalf("SalvageJSON(%s) reported a repair on valid input", in)
		}
		if string(out) != in {
			t.Fatalf("SalvageJSON(%s) = %s; valid bytes must come back unchanged, "+
				"key order included", in, out)
		}
	}
}

// TestATruncatedMemberIsDroppedNotClosed is the safety decision, spelled out.
//
// Closing the open string is friendlier-looking and wrong: `{"path":"/etc/pas`
// becomes a syntactically perfect call naming a file the model never finished
// writing, indistinguishable from a complete one. Dropping the member makes
// the truncation survive as a missing required property, which schema
// validation rejects and the caller is told about.
func TestATruncatedMemberIsDroppedNotClosed(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"open string value", `{"path":"/etc/pas`, `{}`},
		{"earlier members survive", `{"a":1,"path":"/etc/pas`, `{"a":1}`},
		{"dangling key", `{"a":1,"path"`, `{"a":1}`},
		{"dangling colon", `{"a":1,"path":`, `{"a":1}`},
		{"trailing comma", `{"a":1,`, `{"a":1}`},
		{"open object", `{"a":1`, `{}`},
		{"nested open", `{"a":{"b":"x","c":"y`, `{"a":{"b":"x"}}`},
		{"array element", `{"a":[1,2`, `{"a":[1]}`},
		{"empty input", ``, `{}`},
		{"bare open brace", `{`, `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, repaired := provider.SalvageJSON([]byte(c.in))
			if !repaired {
				t.Fatalf("SalvageJSON(%q) reported no repair", c.in)
			}
			if string(out) != c.want {
				t.Fatalf("SalvageJSON(%q) = %s, want %s", c.in, out, c.want)
			}
			if !json.Valid(out) {
				t.Fatalf("SalvageJSON(%q) produced invalid JSON: %s", c.in, out)
			}
		})
	}
}

// FuzzSalvageAlwaysProducesValidJSON is the property that matters: whatever
// arrives, the result parses. A salvage pass that can itself emit broken JSON
// moves the failure later and somewhere else.
func FuzzSalvageAlwaysProducesValidJSON(f *testing.F) {
	for _, s := range []string{
		`{"a":1}`, `{"a":"\u00`, `{"a":"x\\`, `[[[[`, `{"a":tru`,
		`{"a":1e`, `{"":""`, `{"a":[{"b":`, "\x00", `{"a":"\"`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		out, _ := provider.SalvageJSON(b)
		if !json.Valid(out) {
			t.Fatalf("SalvageJSON(%q) produced invalid JSON: %q", b, out)
		}
	})
}
