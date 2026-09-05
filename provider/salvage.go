package provider

import "encoding/json"

// SalvageJSON repairs the argument bytes of a tool call whose stream was cut
// off, so that a truncated call is REPRESENTABLE rather than undecodable.
//
// It returns the input UNCHANGED, and repaired=false, whenever the input
// already parses. That is not an optimization — it is REQ-PROV-17. The bytes a
// provider streamed are authoritative for every replay and fingerprint, so the
// salvage path must be a no-op on the overwhelmingly common case; a repair
// that "normalizes" valid input would reorder keys on every turn and shift the
// prompt-cache prefix.
//
// WHAT IT DOES WITH A TRUNCATED VALUE, and why: it DROPS the incomplete
// member rather than closing it.
//
// Closing an open string is the friendlier-looking choice and it is the wrong
// one here. `{"path":"/etc/pas` becomes a syntactically perfect call to delete
// a file nobody named, indistinguishable from a complete one — the file-
// corrupting case REQ-LOOP-10 exists for, now wearing valid JSON. Dropping the
// member instead makes the truncation SURVIVE as a missing required property,
// where REQ-TOOL-11's schema validation rejects it and the caller is told what
// happened. Complete members before the cut are kept; only the partial one is
// lost.
//
// This is a safety net under REQ-LOOP-10, never a substitute for it: the loop
// still refuses to execute any tool call from a `max_tokens` turn, because a
// call can be truncated at a point where the remaining JSON is both valid and
// wrong.
func SalvageJSON(b []byte) (json.RawMessage, bool) {
	if json.Valid(b) {
		return b, false
	}
	if len(trim(b)) == 0 {
		return json.RawMessage("{}"), true
	}

	type frame struct {
		kind byte // '{' or '['
		// lastGood is one past the end of the last COMPLETE member of this
		// container. Truncating here always yields a prefix we can close.
		lastGood int
		// inValue is meaningful for objects: a string that arrives while
		// false is a key, and a key with no value is not a complete member.
		inValue bool
	}

	var stack []frame
	// complete marks a value as finished at index end (exclusive), attributing
	// it to the enclosing container.
	complete := func(end int) {
		if n := len(stack); n > 0 {
			f := &stack[n-1]
			if f.kind == '[' || f.inValue {
				f.lastGood = end
			}
		}
	}

	i := 0
	for i < len(b) {
		c := b[i]
		switch {
		case c == '{' || c == '[':
			stack = append(stack, frame{kind: c, lastGood: i + 1})
			i++
		case c == '}' || c == ']':
			if len(stack) == 0 {
				return json.RawMessage("{}"), true
			}
			stack = stack[:len(stack)-1]
			complete(i + 1)
			i++
		case c == ':':
			if n := len(stack); n > 0 {
				stack[n-1].inValue = true
			}
			i++
		case c == ',':
			// Everything before the comma is complete; the comma itself is
			// dropped by truncating AT it.
			if n := len(stack); n > 0 {
				stack[n-1].lastGood = i
				stack[n-1].inValue = false
			}
			i++
		case c == '"':
			end, ok := scanString(b, i)
			if !ok {
				i = len(b) // unterminated: nothing after it can complete
				break
			}
			complete(end)
			i = end
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		default:
			end := scanBare(b, i)
			if end == len(b) {
				// A bare token running to EOF cannot be known complete: `12`
				// may be the head of `1234`, and `tru` is not `true`.
				i = end
				break
			}
			complete(end)
			i = end
		}
	}

	if len(stack) == 0 {
		// A bare truncated scalar at the top level. There is no partial object
		// to salvage, and a tool call's arguments are an object by contract.
		return json.RawMessage("{}"), true
	}

	out := append([]byte(nil), b[:stack[len(stack)-1].lastGood]...)
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].kind == '{' {
			out = append(out, '}')
		} else {
			out = append(out, ']')
		}
	}
	if !json.Valid(out) {
		// Belt and braces: a repair that does not parse is worse than an empty
		// object, because it fails later and somewhere else.
		return json.RawMessage("{}"), true
	}
	return out, true
}

// scanString returns one past the closing quote of the string starting at i,
// or ok=false if it is unterminated.
func scanString(b []byte, i int) (int, bool) {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++ // skip the escaped byte; a trailing backslash falls out of the loop
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}

// scanBare returns one past the end of a bare token (number, true, false,
// null) starting at i.
func scanBare(b []byte, i int) int {
	for j := i; j < len(b); j++ {
		switch b[j] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return j
		}
	}
	return len(b)
}

func trim(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpaceByte(b[i]) {
		i++
	}
	for j > i && isSpaceByte(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
