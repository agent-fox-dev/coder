package agentkit

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/jsonx"
	"github.com/agentfox/agentkit-go/schema"
)

// Prepared is the output of the argument pipeline. It carries the value in
// three forms, and that is not redundancy:
//
//	Raw    the bytes handed to the handler (REQ-GO-03's pinned signature)
//	Order  the order-preserving form, authoritative for regenerating Raw
//	Args   the decoded map, for interceptors and policies to read
//
// REQ-TOOL-11 specifies the pipeline over map[string]any, but Go maps have no
// order and REQ-GO-03 then requires bytes again — so a literal implementation
// re-encodes a map and SORTS the keys, silently violating REQ-TOOL-12 on every
// tool call (ruling P-7). The ordered form is what the pipeline actually
// operates on; the map is derived for the interceptor's convenience and is
// never re-encoded back into a request.
type Prepared struct {
	Raw   json.RawMessage
	Order jsonx.OrderedObject
	Args  map[string]any

	// Coercions records what step 3 changed, for diagnostics.
	Coercions []schema.Coercion
}

// WithArgs replaces the arguments, as an interceptor may do (REQ-SEC-03.5),
// regenerating Raw through the ordered form so the positions of keys the
// interceptor kept survive.
func (p Prepared) WithArgs(args map[string]any) Prepared {
	order := p.Order.Clone()
	for k, v := range args {
		order.Set(k, jsonx.OV(v))
	}
	for i := 0; i < len(order); {
		if _, ok := args[order[i].Key]; !ok {
			order = append(order[:i], order[i+1:]...)
			continue
		}
		i++
	}
	raw, err := order.MarshalJSON()
	if err != nil {
		raw = p.Raw
	}
	return Prepared{Raw: raw, Order: order, Args: order.Map(), Coercions: p.Coercions}
}

// PrepareArguments runs REQ-TOOL-11's fixed pipeline before the handler. Every
// stage runs inside the caller's panic-recover boundary.
//
//  1. Tool.PrepareArguments — per-tool shape repair, runs FIRST, on a copy.
//  2. Delete explicit nulls for OPTIONAL properties.
//  3. Coerce primitives against the declared schema.
//  4. Validate; a failure echoes the model's own arguments back in the order
//     it wrote them, so the error is self-correcting.
//
// Step 2 is the non-obvious one: constrained sampling forces the model to emit
// every declared property, so optional fields arrive as explicit nulls.
// Treating them as present is a validation failure on well-formed output.
func PrepareArguments(t core.Tool, c core.ToolUseBlock) (Prepared, error) {
	order := c.InputOrder.Clone()
	modified := false

	// ---- 1. Per-tool repair shim. It takes and returns a map because that is
	// the pinned Tool field signature; the result is folded back into the
	// ordered form, preserving the positions of keys it left alone.
	if t.PrepareArguments != nil {
		before := order.Map()
		after := t.PrepareArguments(before)
		if after != nil {
			next := foldIntoOrder(order, after)
			if !orderEqual(next, order) {
				order, modified = next, true
			}
		}
	}

	if t.InputSchema != nil {
		// ---- 2. Optional nulls. A null for a REQUIRED property is left in
		// place so validation reports it as the wrong type, rather than being
		// silently deleted into a "missing property" error that reads as a
		// different bug.
		if next := schema.DeleteOptionalNulls(t.InputSchema, order); !orderEqual(next, order) {
			order, modified = next, true
		}

		// ---- 3. Coerce.
		next, coercions := schema.Coerce(t.InputSchema, order)
		if len(coercions) > 0 {
			order, modified = next, true
		}

		// ---- 4. Validate.
		if err := schema.Validate(t.InputSchema, order); err != nil {
			return Prepared{}, fmt.Errorf("%w\n\narguments as provided:\n%s", err, echoArguments(c.Input))
		}

		raw := c.Input
		if modified {
			if b, err := order.MarshalJSON(); err == nil {
				raw = b
			}
		}
		return Prepared{Raw: raw, Order: order, Args: order.Map(), Coercions: coercions}, nil
	}

	raw := c.Input
	if modified {
		if b, err := order.MarshalJSON(); err == nil {
			raw = b
		}
	}
	return Prepared{Raw: raw, Order: order, Args: order.Map()}, nil
}

// echoArguments renders the model's own bytes back to it, indented.
//
// json.Indent is byte-preserving, so key order and numeric literals come free.
// Re-marshalling a decoded map here would sort the keys and show the model
// something it did not write, defeating the point of a self-correcting error
// (REQ-TOOL-12.3).
func echoArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// foldIntoOrder rebuilds an ordered object from a map, preserving the position
// of every key that was already present. Keys the shim ADDED land at the end
// in lexical order — not in Go's map iteration order, which is randomized and
// would make the resulting bytes irreproducible across runs and break every
// golden test that touches a repaired call.
func foldIntoOrder(prev jsonx.OrderedObject, args map[string]any) jsonx.OrderedObject {
	out := make(jsonx.OrderedObject, 0, len(args))
	seen := make(map[string]bool, len(args))
	for _, m := range prev {
		if v, ok := args[m.Key]; ok {
			out = append(out, jsonx.Member{Key: m.Key, Value: jsonx.OV(v)})
			seen[m.Key] = true
		}
	}
	added := make([]string, 0, len(args))
	for k := range args {
		if !seen[k] {
			added = append(added, k)
		}
	}
	sortStrings(added)
	for _, k := range added {
		out = append(out, jsonx.Member{Key: k, Value: jsonx.OV(args[k])})
	}
	return out
}

func orderEqual(a, b jsonx.OrderedObject) bool {
	if len(a) != len(b) {
		return false
	}
	ab, err1 := a.MarshalJSON()
	bb, err2 := b.MarshalJSON()
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
