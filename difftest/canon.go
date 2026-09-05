// Package difftest is the NFR-TEST-06 wire-level differential harness.
//
// It compares the exact request body a provider constructs against an
// independently produced reference body for the same scenario. The distinction
// it exists to draw is the one NFR-TEST-06 opens with: a mock provider tests
// the LOOP against the abstraction, and nothing in the unit suite tests the
// ABSTRACTION against the provider.
//
// Both sides capture through OnPayload (REQ-PROV-18), which stores the payload
// and returns an error, so the harness aborts before the first byte and needs
// no API key and no network.
package difftest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/agentfox/agentkit-go/jsonx"
)

// Kind classifies one difference. The set is closed because the ledger matches
// on it: a free-form string would let an accepted divergence quietly widen to
// cover a different defect at the same path.
type Kind string

const (
	// KindMissing: present in the reference, absent from ours.
	KindMissing Kind = "missing"
	// KindExtra: present in ours, absent from the reference.
	KindExtra Kind = "extra"
	// KindValue: same type, different value — including a different NUMBER
	// LITERAL, which is deliberately not normalized.
	KindValue Kind = "value"
	KindType  Kind = "type"
	// KindLength: arrays of different length.
	KindLength Kind = "length"
	// KindOrder: object key order differs at a path the scenario declared
	// order-sensitive.
	KindOrder Kind = "order"
)

// Difference is one structural disagreement.
type Difference struct {
	Path      string `json:"path"`
	Kind      Kind   `json:"kind"`
	Reference string `json:"reference,omitempty"`
	Actual    string `json:"actual,omitempty"`
}

func (d Difference) String() string {
	return fmt.Sprintf("%s\t%s\tref=%s\tgot=%s", d.Path, d.Kind, d.Reference, d.Actual)
}

// Compare implements NFR-TEST-06.4 and .5.
//
// NORMALIZED, on purpose (a difference here is hidden):
//
//   - object key order, except at orderSensitive paths;
//   - string escaping — "A" and "A" are the same string;
//   - insignificant whitespace.
//
// NOT NORMALIZED, on purpose (a difference here FAILS):
//
//   - number literal text. The trees are decoded with UseNumber and the
//     literal is diffed, so 1024 vs 1024.0 vs 1e3 stays visible instead of
//     being laundered through a float64 that makes all three equal.
//   - array order, ever.
//   - key sets.
//   - null versus absent. This is the one that makes REQ-PROV-16 enforceable:
//     `omitempty` on a field whose zero value is meaningful produces "absent"
//     where the reference produces an explicit value, and every comparison
//     that treats the two as the same passes the exact bug the requirement
//     exists to prevent.
func Compare(reference, actual []byte, orderSensitive []string) ([]Difference, error) {
	ref, err := jsonx.DecodeOrdered(reference)
	if err != nil {
		return nil, fmt.Errorf("difftest: decoding reference: %w", err)
	}
	act, err := jsonx.DecodeOrdered(actual)
	if err != nil {
		return nil, fmt.Errorf("difftest: decoding actual: %w", err)
	}

	c := &comparer{sensitive: compilePaths(orderSensitive)}
	c.walk("$", ref, act)
	sort.SliceStable(c.diffs, func(i, j int) bool { return c.diffs[i].Path < c.diffs[j].Path })
	return c.diffs, nil
}

type comparer struct {
	diffs     []Difference
	sensitive map[string]bool
}

func (c *comparer) add(d Difference) { c.diffs = append(c.diffs, d) }

func (c *comparer) walk(path string, ref, act jsonx.OrderedValue) {
	if ref.Kind != act.Kind {
		c.add(Difference{Path: path, Kind: KindType,
			Reference: kindName(ref.Kind), Actual: kindName(act.Kind)})
		return
	}

	switch ref.Kind {
	case jsonx.KindObject:
		c.walkObject(path, ref.Object, act.Object)
	case jsonx.KindArray:
		if len(ref.Array) != len(act.Array) {
			c.add(Difference{Path: path, Kind: KindLength,
				Reference: fmt.Sprint(len(ref.Array)), Actual: fmt.Sprint(len(act.Array))})
		}
		// Array ORDER is never normalized, so elements are compared by index.
		n := min(len(ref.Array), len(act.Array))
		for i := 0; i < n; i++ {
			c.walk(fmt.Sprintf("%s[%d]", path, i), ref.Array[i], act.Array[i])
		}
	case jsonx.KindString:
		// Escaping IS normalized: decode both and compare the runes.
		var rs, as string
		_ = json.Unmarshal(ref.Scalar, &rs)
		_ = json.Unmarshal(act.Scalar, &as)
		if rs != as {
			c.add(Difference{Path: path, Kind: KindValue, Reference: rs, Actual: as})
		}
	default:
		// Numbers, booleans and null compare as their VERBATIM source bytes.
		if string(ref.Scalar) != string(act.Scalar) {
			c.add(Difference{Path: path, Kind: KindValue,
				Reference: string(ref.Scalar), Actual: string(act.Scalar)})
		}
	}
}

func (c *comparer) walkObject(path string, ref, act jsonx.OrderedObject) {
	// Key ORDER is moved to a side channel rather than discarded (NFR-TEST-06.5):
	// it fails only where the scenario declared that insertion order is
	// observable to the model or the provider — chat-template arguments,
	// model-authored tool-call arguments, reasoning blocks replayed verbatim.
	if c.sensitive[normalizePath(path)] {
		rk, ak := keysOf(ref), keysOf(act)
		if strings.Join(rk, ",") != strings.Join(ak, ",") {
			c.add(Difference{Path: path, Kind: KindOrder,
				Reference: strings.Join(rk, ","), Actual: strings.Join(ak, ",")})
		}
	}

	// Objects are walked by SORTED key so both sides traverse identical paths
	// regardless of the order either side wrote them in.
	seen := map[string]bool{}
	var keys []string
	for _, m := range ref {
		keys = append(keys, m.Key)
		seen[m.Key] = true
	}
	for _, m := range act {
		if !seen[m.Key] {
			keys = append(keys, m.Key)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		rv, rok := ref.Get(k)
		av, aok := act.Get(k)
		child := path + "." + k
		switch {
		case rok && !aok:
			c.add(Difference{Path: child, Kind: KindMissing, Reference: render(rv)})
		case !rok && aok:
			c.add(Difference{Path: child, Kind: KindExtra, Actual: render(av)})
		default:
			c.walk(child, rv, av)
		}
	}
}

func keysOf(o jsonx.OrderedObject) []string {
	out := make([]string, len(o))
	for i, m := range o {
		out[i] = m.Key
	}
	return out
}

// KeyOrderLines is NFR-TEST-06.5's side channel: one line per object, as
// `<path>\t<keys in original order>`, walking objects by sorted key so both
// sides traverse identical paths.
//
// It is emitted for BOTH sides whether or not any path is order-sensitive,
// because the point of a side channel is that the information is still there
// when someone needs to look. Discarding key order and then discovering that a
// provider cares about it leaves nothing to diff.
func KeyOrderLines(body []byte) ([]string, error) {
	v, err := jsonx.DecodeOrdered(body)
	if err != nil {
		return nil, err
	}
	var out []string
	var rec func(path string, v jsonx.OrderedValue)
	rec = func(path string, v jsonx.OrderedValue) {
		switch v.Kind {
		case jsonx.KindObject:
			out = append(out, path+"\t"+strings.Join(keysOf(v.Object), ","))
			keys := keysOf(v.Object)
			sorted := append([]string(nil), keys...)
			sort.Strings(sorted)
			for _, k := range sorted {
				cv, _ := v.Object.Get(k)
				rec(path+"."+k, cv)
			}
		case jsonx.KindArray:
			for i, e := range v.Array {
				rec(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	rec("$", v)
	return out, nil
}

func render(v jsonx.OrderedValue) string {
	b, err := v.MarshalJSON()
	if err != nil {
		return "<unrenderable>"
	}
	if len(b) > 200 {
		return string(b[:200]) + "…"
	}
	return string(b)
}

func kindName(k jsonx.ValueKind) string {
	switch k {
	case jsonx.KindNull:
		return "null"
	case jsonx.KindBool:
		return "bool"
	case jsonx.KindNumber:
		return "number"
	case jsonx.KindString:
		return "string"
	case jsonx.KindArray:
		return "array"
	}
	return "object"
}

var arrayIndex = regexp.MustCompile(`\[\d+\]`)

// normalizePath collapses concrete array indices so a declared path can name
// every element of an array without enumerating them:
// `$.messages[].content[].input` matches `$.messages[3].content[0].input`.
func normalizePath(p string) string { return arrayIndex.ReplaceAllString(p, "[]") }

func compilePaths(ps []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range ps {
		out[normalizePath(p)] = true
	}
	return out
}
