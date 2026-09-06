package wire

import (
	"encoding/json"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// Kind is a JSON value's type.
type Kind uint8

const (
	KindNull Kind = iota
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
)

func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindArray:
		return "array"
	}
	return "object"
}

// Value is one decoded JSON value.
//
// Numbers are held as json.Number — the VERBATIM literal — so 1, 1.0 and 1e3
// stay distinguishable. REQ-SEC-12.2 needs that distinction to implement
// cross-language number semantics correctly, and NFR-TEST-06.4 needs it to
// diff a request body without laundering it through a float64.
type Value struct {
	Kind   Kind
	Bool   bool
	Number json.Number
	String string
	Array  []Value
	// Object holds members. Duplicate keys are REJECTED at decode
	// (REQ-SEC-11.3), so a map is safe here: there is never a second value to
	// lose.
	Object map[string]Value
	// Keys is insertion order, for deterministic error messages and for a
	// caller that needs to know what the peer actually sent.
	Keys []string
}

// Get returns a member.
func (v Value) Get(key string) (Value, bool) {
	if v.Kind != KindObject {
		return Value{}, false
	}
	m, ok := v.Object[key]
	return m, ok
}

// Parse decodes a bounded tree. It is the REQ-SEC-12 entry point.
func Parse(data []byte, l Limits) (Value, error) {
	s := &scanner{data: data, lim: l.withDefaults(), build: true}
	if err := s.checkSize(); err != nil {
		return Value{}, err
	}
	v, err := s.value("$", "")
	if err != nil {
		return Value{}, err
	}
	s.ws()
	if s.i != len(s.data) {
		return Value{}, failf(RuleSyntax, "$", "trailing bytes after the JSON value")
	}
	return v, nil
}

// Guard validates a message against REQ-SEC-11's bounds WITHOUT building a
// tree.
//
// It exists because the bounds and the strict binding have different scopes.
// Every untrusted surface needs the bounds — a provider response included —
// but a provider response must stay tolerant of fields a vendor added last
// week, so it is guarded and then decoded with the ordinary lenient path. One
// extra linear scan is the whole cost, and it buys duplicate-key rejection on
// a surface where last-wins would let a compromised gateway choose which of
// two stop_reasons AgentKit sees.
func Guard(data []byte, l Limits) error {
	s := &scanner{data: data, lim: l.withDefaults()}
	if err := s.checkSize(); err != nil {
		return err
	}
	if _, err := s.value("$", ""); err != nil {
		return err
	}
	s.ws()
	if s.i != len(s.data) {
		return failf(RuleSyntax, "$", "trailing bytes after the JSON value")
	}
	return nil
}

type scanner struct {
	data  []byte
	i     int
	lim   Limits
	depth int
	// build is false for Guard: the walk is identical, nothing is
	// materialized, and the two paths cannot disagree about what is legal
	// because there is only one of them.
	build bool
}

func (s *scanner) checkSize() error {
	// int64 throughout. REQ-SEC-11.1 is about a peer-declared length narrowed
	// to int on a 32-bit build; len() of a slice we already hold cannot be
	// negative, but comparing in int64 costs nothing and keeps the whole
	// package's size arithmetic in one width.
	if int64(len(s.data)) > s.lim.MaxMessageBytes {
		return failf(RuleMessageBytes, "$", "message is %d bytes, limit is %d",
			len(s.data), s.lim.MaxMessageBytes)
	}
	return nil
}

func (s *scanner) ws() {
	for s.i < len(s.data) {
		switch s.data[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

// value decodes one value.
//
// It takes the parent path and this value's own segment SEPARATELY, and joins
// them only when it needs a path — on an error, or when recursing into a
// container. A scalar member is the overwhelmingly common case and it now
// allocates nothing for a path string nobody ever reads: joining eagerly cost
// one allocation per member on a control that runs over every streamed event.
func (s *scanner) value(parent, seg string) (Value, error) {
	s.ws()
	if s.i >= len(s.data) {
		return Value{}, failf(RuleSyntax, join(parent, seg), "unexpected end of input")
	}
	switch c := s.data[s.i]; {
	case c == '{':
		return s.object(join(parent, seg))
	case c == '[':
		return s.array(join(parent, seg))
	case c == '"':
		str, err := s.stringValueAt(parent, seg)
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KindString, String: str}, nil
	case c == 't':
		return s.literal(parent, seg, "true", Value{Kind: KindBool, Bool: true})
	case c == 'f':
		return s.literal(parent, seg, "false", Value{Kind: KindBool})
	case c == 'n':
		return s.literal(parent, seg, "null", Value{Kind: KindNull})
	case c == '-' || (c >= '0' && c <= '9'):
		return s.numberAt(parent, seg)
	}
	return Value{}, failf(RuleSyntax, join(parent, seg), "unexpected character %q", s.data[s.i])
}

// join builds a JSON path segment. seg is "" for the root, "[n]" for an array
// element and a bare member name otherwise.
func join(parent, seg string) string {
	switch {
	case seg == "":
		return parent
	case seg[0] == '[':
		return parent + seg
	}
	return parent + "." + seg
}

func (s *scanner) stringValueAt(parent, seg string) (string, error) {
	str, err := s.stringValue("")
	if err != nil {
		return "", repath(err, join(parent, seg))
	}
	return str, nil
}

func (s *scanner) numberAt(parent, seg string) (Value, error) {
	v, err := s.number("")
	if err != nil {
		return Value{}, repath(err, join(parent, seg))
	}
	return v, nil
}

// repath fills in the path an inner scan deferred computing.
func repath(err error, path string) error {
	if e, ok := err.(*Error); ok && e.Path == "" {
		e.Path = path
	}
	return err
}

func (s *scanner) literal(parent, seg, lit string, v Value) (Value, error) {
	if s.i+len(lit) > len(s.data) || string(s.data[s.i:s.i+len(lit)]) != lit {
		return Value{}, failf(RuleSyntax, join(parent, seg), "expected %s", lit)
	}
	s.i += len(lit)
	return v, nil
}

func (s *scanner) enter(path string) error {
	s.depth++
	if s.depth > s.lim.MaxDepth {
		return failf(RuleDepth, path, "nesting exceeds %d", s.lim.MaxDepth)
	}
	return nil
}

func (s *scanner) object(path string) (Value, error) {
	if err := s.enter(path); err != nil {
		return Value{}, err
	}
	defer func() { s.depth-- }()

	s.i++ // '{'
	out := Value{Kind: KindObject}
	if s.build {
		// NOT sized to anything the peer declared (REQ-SEC-11.2). JSON has no
		// declared length, but the same principle rules out sizing to
		// MaxContainerLen "just in case": the map grows to what actually
		// arrives.
		out.Object = map[string]Value{}
	}
	// seen is separate from out.Object because Guard does not build one, and
	// duplicate rejection must hold on both paths.
	var seen map[string]struct{}
	var seenSmall []string

	s.ws()
	if s.i < len(s.data) && s.data[s.i] == '}' {
		s.i++
		return out, nil
	}

	n := 0
	for {
		s.ws()
		if s.i >= len(s.data) || s.data[s.i] != '"' {
			return Value{}, failf(RuleSyntax, path, "expected a member name")
		}
		key, err := s.stringValueAt(path, "")
		if err != nil {
			return Value{}, err
		}

		// REQ-SEC-11.3: duplicates are REJECTED, never resolved last-wins.
		// Last-wins hands an untrusted peer the choice of which of two values
		// AgentKit sees — and the one it does not see is the one a reviewer
		// read.
		if seen == nil && len(seenSmall) >= 16 {
			seen = make(map[string]struct{}, len(seenSmall)*2)
			for _, k := range seenSmall {
				seen[k] = struct{}{}
			}
			seenSmall = nil
		}
		if seen != nil {
			if _, dup := seen[key]; dup {
				return Value{}, failf(RuleDuplicateKey, join(path, key), "member %q appears twice", key)
			}
			seen[key] = struct{}{}
		} else {
			for _, k := range seenSmall {
				if k == key {
					return Value{}, failf(RuleDuplicateKey, join(path, key), "member %q appears twice", key)
				}
			}
			seenSmall = append(seenSmall, key)
		}

		s.ws()
		if s.i >= len(s.data) || s.data[s.i] != ':' {
			return Value{}, failf(RuleSyntax, join(path, key), "expected ':'")
		}
		s.i++

		v, err := s.value(path, key)
		if err != nil {
			return Value{}, err
		}

		n++
		if n > s.lim.MaxContainerLen {
			return Value{}, failf(RuleContainerLen, path, "object has more than %d members",
				s.lim.MaxContainerLen)
		}
		if s.build {
			out.Object[key] = v
			out.Keys = append(out.Keys, key)
		}

		s.ws()
		if s.i >= len(s.data) {
			return Value{}, failf(RuleSyntax, path, "unterminated object")
		}
		switch s.data[s.i] {
		case ',':
			s.i++
		case '}':
			s.i++
			return out, nil
		default:
			return Value{}, failf(RuleSyntax, path, "expected ',' or '}'")
		}
	}
}

func (s *scanner) array(path string) (Value, error) {
	if err := s.enter(path); err != nil {
		return Value{}, err
	}
	defer func() { s.depth-- }()

	s.i++ // '['
	out := Value{Kind: KindArray}

	s.ws()
	if s.i < len(s.data) && s.data[s.i] == ']' {
		s.i++
		return out, nil
	}

	n := 0
	for {
		v, err := s.value(path, "["+strconv.Itoa(n)+"]")
		if err != nil {
			return Value{}, err
		}
		n++
		if n > s.lim.MaxContainerLen {
			return Value{}, failf(RuleContainerLen, path, "array has more than %d elements",
				s.lim.MaxContainerLen)
		}
		if s.build {
			out.Array = append(out.Array, v)
		}

		s.ws()
		if s.i >= len(s.data) {
			return Value{}, failf(RuleSyntax, path, "unterminated array")
		}
		switch s.data[s.i] {
		case ',':
			s.i++
		case ']':
			s.i++
			return out, nil
		default:
			return Value{}, failf(RuleSyntax, path, "expected ',' or ']'")
		}
	}
}

func (s *scanner) number(path string) (Value, error) {
	start := s.i
	if s.i < len(s.data) && s.data[s.i] == '-' {
		s.i++
	}
	digits := func() int {
		n := 0
		for s.i < len(s.data) && s.data[s.i] >= '0' && s.data[s.i] <= '9' {
			s.i++
			n++
		}
		return n
	}
	// JSON forbids a leading zero: `01` is two tokens, not one number. A
	// scanner that swallows both accepts a message the peer's own encoder
	// could not have produced, which means it is decoding something other than
	// the protocol.
	if s.i < len(s.data) && s.data[s.i] == '0' {
		s.i++
		if s.i < len(s.data) && s.data[s.i] >= '0' && s.data[s.i] <= '9' {
			return Value{}, failf(RuleSyntax, path, "number has a leading zero")
		}
	} else if n := digits(); n == 0 {
		return Value{}, failf(RuleSyntax, path, "malformed number")
	}
	if s.i < len(s.data) && s.data[s.i] == '.' {
		s.i++
		if digits() == 0 {
			return Value{}, failf(RuleSyntax, path, "malformed number: no digits after '.'")
		}
	}
	if s.i < len(s.data) && (s.data[s.i] == 'e' || s.data[s.i] == 'E') {
		s.i++
		if s.i < len(s.data) && (s.data[s.i] == '+' || s.data[s.i] == '-') {
			s.i++
		}
		if digits() == 0 {
			return Value{}, failf(RuleSyntax, path, "malformed number: no digits in exponent")
		}
	}
	if !s.build {
		return Value{}, nil
	}
	return Value{Kind: KindNumber, Number: json.Number(s.data[start:s.i])}, nil
}

// stringValue scans and unescapes a string.
//
// The fast path returns without allocating when there is no escape, which is
// the overwhelmingly common case and matters because every object member name
// comes through here.
func (s *scanner) stringValue(path string) (string, error) {
	s.i++ // opening quote
	start := s.i
	for s.i < len(s.data) {
		c := s.data[s.i]
		switch {
		case c == '"':
			// Materialized on BOTH paths: Guard needs member names too, for
			// duplicate detection.
			str := string(s.data[start:s.i])
			s.i++
			return str, nil
		case c == '\\':
			return s.stringSlow(path, start)
		case c < 0x20:
			return "", failf(RuleSyntax, path, "unescaped control character %#x in string", c)
		default:
			s.i++
		}
	}
	return "", failf(RuleSyntax, path, "unterminated string")
}

func (s *scanner) stringSlow(path string, start int) (string, error) {
	buf := make([]byte, 0, (s.i-start)*2)
	buf = append(buf, s.data[start:s.i]...)

	for s.i < len(s.data) {
		c := s.data[s.i]
		switch {
		case c == '"':
			s.i++
			return string(buf), nil
		case c < 0x20:
			return "", failf(RuleSyntax, path, "unescaped control character %#x in string", c)
		case c != '\\':
			buf = append(buf, c)
			s.i++
			continue
		}

		s.i++ // backslash
		if s.i >= len(s.data) {
			return "", failf(RuleSyntax, path, "unterminated escape")
		}
		switch s.data[s.i] {
		case '"', '\\', '/':
			buf = append(buf, s.data[s.i])
			s.i++
		case 'b':
			buf = append(buf, '\b')
			s.i++
		case 'f':
			buf = append(buf, '\f')
			s.i++
		case 'n':
			buf = append(buf, '\n')
			s.i++
		case 'r':
			buf = append(buf, '\r')
			s.i++
		case 't':
			buf = append(buf, '\t')
			s.i++
		case 'u':
			r, err := s.unicodeEscape(path)
			if err != nil {
				return "", err
			}
			buf = utf8.AppendRune(buf, r)
		default:
			return "", failf(RuleSyntax, path, "unknown escape \\%c", s.data[s.i])
		}
	}
	return "", failf(RuleSyntax, path, "unterminated string")
}

// unicodeEscape decodes \uXXXX, pairing surrogates.
//
// An unpaired surrogate becomes U+FFFD rather than an error, matching what
// encoding/json does: a peer with a UTF-16 runtime emits them for legitimate
// reasons, and rejecting the message would refuse valid traffic over a
// representation detail.
func (s *scanner) unicodeEscape(path string) (rune, error) {
	s.i++ // 'u'
	hi, err := s.hex4(path)
	if err != nil {
		return 0, err
	}
	if !utf16.IsSurrogate(rune(hi)) {
		return rune(hi), nil
	}
	if s.i+1 < len(s.data) && s.data[s.i] == '\\' && s.data[s.i+1] == 'u' {
		save := s.i
		s.i += 2
		lo, err := s.hex4(path)
		if err != nil {
			return 0, err
		}
		if r := utf16.DecodeRune(rune(hi), rune(lo)); r != utf8.RuneError {
			return r, nil
		}
		s.i = save
	}
	return utf8.RuneError, nil
}

func (s *scanner) hex4(path string) (uint16, error) {
	if s.i+4 > len(s.data) {
		return 0, failf(RuleSyntax, path, "truncated \\u escape")
	}
	var v uint16
	for k := 0; k < 4; k++ {
		c := s.data[s.i+k]
		var d uint16
		switch {
		case c >= '0' && c <= '9':
			d = uint16(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint16(c-'A') + 10
		default:
			return 0, failf(RuleSyntax, path, "invalid hex digit %q in \\u escape", c)
		}
		v = v<<4 | d
	}
	s.i += 4
	return v, nil
}
