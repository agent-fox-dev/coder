package toml

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agentfox/agentkit-go/internal/diag"
)

// Diagnostic and Severity are the shared report type (internal/diag), aliased
// so a caller of this package needs one import rather than two.
type (
	Diagnostic = diag.Diagnostic
	Severity   = diag.Severity
)

const (
	SeverityWarning = diag.SeverityWarning
	SeverityError   = diag.SeverityError
)

const bomPrefix = diag.BOMPrefix

// ---------------------------------------------------------------- TOML subset
//
// There is no TOML parser in the Go standard library and REQ-GO-11 forbids a
// dependency, so this is a hand-written parser for the subset a skill manifest
// needs (REQ-SKILL-03). The honest statement of that subset follows; an honest
// subset beats a broken superset, because the failure mode of a "mostly TOML"
// parser is a value silently read as the wrong thing.
//
// SUPPORTED, exhaustively:
//   - '#' comments, on their own line or trailing a value
//   - bare keys ([A-Za-z0-9_-]+) and quoted keys ("k" and 'k')
//   - dotted keys (a.b.c = 1) and table headers [a] and [a.b]
//   - basic strings "..." with the escapes \b \t \n \f \r \" \\ \/ \uXXXX
//     and \UXXXXXXXX
//   - literal strings '...' (no escape processing, per TOML)
//   - booleans true and false
//   - decimal integers with an optional sign and '_' digit separators
//   - arrays of strings, single- or multi-line, trailing comma allowed
//   - a leading UTF-8 BOM
//
// NOT SUPPORTED, and each one is a DIAGNOSTIC THAT SKIPS THE KEY rather than a
// failure of the file. REQ-SKILL-10: a manifest is authored content whose
// consumer is a language model, and a value form we do not read must not
// delete the whole skill.
//   - floats, dates, times and datetimes
//   - multi-line strings (""" and ''')
//   - inline tables { }
//   - arrays that are not arrays of strings (numbers, nested arrays, tables)
//
// SUPPORTED since REQ-MCP-CLIENT-07 needed it:
//   - arrays of tables [[a.b]]
//   - non-decimal integers (0x, 0o, 0b)
//
// NOT SUPPORTED and a HARD ERROR, because after one of these the file's
// structure is unknown and every later key would be filed under the wrong
// table — a silently misplaced key is worse than a rejected manifest:
//   - an unterminated string, array or table header
//   - anything that is not a comment, a table header or `key = value`

// ValueKind enumerates the value types the subset above can produce.
type ValueKind uint8

const (
	KindString ValueKind = iota
	KindBool
	KindInt
	KindStringArray
)

// Value is one parsed TOML scalar or string array. It is a tagged struct
// rather than an `any`, so a manifest reader that asks for the wrong type gets
// a diagnostic instead of a panic.
type Value struct {
	Kind  ValueKind
	Str   string
	Bool  bool
	Int   int64
	Array []string
	// Line is the 1-based line the value was written on, so a diagnostic can
	// point the manifest author at it.
	Line int
}

// Table is a TOML table. It preserves key insertion order, which is what makes
// the unknown-key diagnostics of REQ-SKILL-10 deterministic: a map iteration
// would reorder the warnings between runs and turn a golden test into a flake.
type Table struct {
	name      string
	line      int
	keys      []string
	vals      map[string]Value
	arrays    map[string][]*Table
	arrayKeys []string
	subKeys   []string
	subs      map[string]*Table
}

func newTable(name string, line int) *Table {
	return &Table{name: name, line: line, vals: map[string]Value{}, subs: map[string]*Table{}}
}

// Name is the dotted path of the table, empty for the root table.
func (t *Table) Name() string { return t.name }

// Line is the 1-based line of the table's header, 0 for the root table.
func (t *Table) Line() int { return t.line }

// Keys returns the table's own keys in the order they were written.
func (t *Table) Keys() []string { return append([]string(nil), t.keys...) }

// SubTables returns the names of the table's sub-tables in written order.
func (t *Table) SubTables() []string { return append([]string(nil), t.subKeys...) }

// Get returns the value written for key.
func (t *Table) Get(key string) (Value, bool) {
	if t == nil {
		return Value{}, false
	}
	v, ok := t.vals[key]
	return v, ok
}

// Sub returns the named sub-table.
func (t *Table) Sub(key string) (*Table, bool) {
	if t == nil {
		return nil, false
	}
	s, ok := t.subs[key]
	return s, ok
}

// Array returns the elements of an array of tables ([[key]]), in document
// order.
func (t *Table) Array(key string) ([]*Table, bool) {
	if t == nil {
		return nil, false
	}
	a, ok := t.arrays[key]
	return a, ok
}

// ArrayKeys returns the names of the arrays of tables declared here.
func (t *Table) ArrayKeys() []string {
	if t == nil {
		return nil
	}
	return append([]string(nil), t.arrayKeys...)
}

// appendArray adds one element to an array of tables.
func (t *Table) appendArray(key, qualified string, line int) *Table {
	if t.arrays == nil {
		t.arrays = map[string][]*Table{}
	}
	if _, exists := t.arrays[key]; !exists {
		t.arrayKeys = append(t.arrayKeys, key)
	}
	el := newTable(qualified, line)
	t.arrays[key] = append(t.arrays[key], el)
	return el
}

// Qualify renders a key with its table prefix, for a diagnostic that has to
// name where in the document the problem is.
func (t *Table) Qualify(key string) string {
	if t.name == "" {
		return key
	}
	return t.name + "." + key
}

// set records a value, reporting false when the key was already present.
func (t *Table) set(key string, v Value) bool {
	if _, dup := t.vals[key]; dup {
		t.vals[key] = v
		return false
	}
	t.vals[key] = v
	t.keys = append(t.keys, key)
	return true
}

func (t *Table) ensure(path []string, line int) *Table {
	cur := t
	for _, part := range path {
		// A path segment naming an array of tables descends into its MOST
		// RECENT element. That is what [[a]] followed by [a.b] means in TOML:
		// b belongs to the a that was just declared, not to a fourth table
		// hanging off the root. Walking past the array instead files every key
		// under [a.b] in a table nobody reads — silently, since both spellings
		// parse.
		if arr, ok := cur.arrays[part]; ok && len(arr) > 0 {
			cur = arr[len(arr)-1]
			continue
		}
		nxt, ok := cur.subs[part]
		if !ok {
			nxt = newTable(cur.Qualify(part), line)
			cur.subs[part] = nxt
			cur.subKeys = append(cur.subKeys, part)
		}
		cur = nxt
	}
	return cur
}

// SyntaxError is a structural failure: the parser cannot know where the
// remaining keys belong, so it stops.
type SyntaxError struct {
	Line int
	Msg  string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// ParseTOML parses the documented subset. It returns the root table, the
// diagnostics for keys it deliberately skipped, and an error only for the
// structural failures listed above.
func ParseTOML(src []byte) (*Table, []Diagnostic, error) {
	// A BOM at the head of a manifest is common on Windows editors and is not
	// a key. Written as an escape because a literal BOM is illegal in Go
	// source (REQ-CTX-02 requires the same strip for context files).
	src = []byte(strings.TrimPrefix(string(src), bomPrefix))

	p := &tomlParser{src: src, line: 1}
	p.root = newTable("", 0)
	p.cur = p.root
	if err := p.parse(); err != nil {
		return nil, p.diags, err
	}
	return p.root, p.diags, nil
}

type tomlParser struct {
	src   []byte
	i     int
	line  int
	root  *Table
	cur   *Table
	diags []Diagnostic
}

func (p *tomlParser) errf(f string, a ...any) error {
	return &SyntaxError{Line: p.line, Msg: fmt.Sprintf(f, a...)}
}

func (p *tomlParser) warnf(line int, f string, a ...any) {
	p.diags = append(p.diags, Diagnostic{
		Severity: SeverityWarning,
		Line:     line,
		Message:  fmt.Sprintf(f, a...),
	})
}

func (p *tomlParser) eof() bool { return p.i >= len(p.src) }

func (p *tomlParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.i]
}

func (p *tomlParser) at(off int) byte {
	if p.i+off >= len(p.src) {
		return 0
	}
	return p.src[p.i+off]
}

func (p *tomlParser) advance() byte {
	c := p.src[p.i]
	p.i++
	if c == '\n' {
		p.line++
	}
	return c
}

func (p *tomlParser) skipSpace() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.i++
	}
}

func (p *tomlParser) skipComment() {
	if p.peek() == '#' {
		for !p.eof() && p.peek() != '\n' {
			p.i++
		}
	}
}

// skipBlank consumes whitespace, newlines and comments.
func (p *tomlParser) skipBlank() {
	for !p.eof() {
		switch p.peek() {
		case ' ', '\t', '\r', '\n':
			p.advance()
		case '#':
			p.skipComment()
		default:
			return
		}
	}
}

// endOfLine consumes trailing space, an optional trailing comment and the
// newline. Requiring it is what makes `key = garbage extra` an error rather
// than a value silently truncated at the first space.
func (p *tomlParser) endOfLine() error {
	p.skipSpace()
	p.skipComment()
	if p.eof() {
		return nil
	}
	if p.peek() == '\r' && p.at(1) == '\n' {
		p.i++
	}
	if p.peek() == '\n' {
		p.advance()
		return nil
	}
	return p.errf("unexpected %q; expected end of line", string(p.peek()))
}

func (p *tomlParser) parse() error {
	for {
		p.skipBlank()
		if p.eof() {
			return nil
		}
		if p.peek() == '[' {
			if err := p.parseHeader(); err != nil {
				return err
			}
			continue
		}
		if err := p.parseKeyValue(); err != nil {
			return err
		}
	}
}

func (p *tomlParser) parseHeader() error {
	line := p.line
	p.i++ // '['
	array := false
	if p.peek() == '[' {
		// [[a.b]] — an array of tables. Added for REQ-MCP-CLIENT-07's
		// [[mcp.servers]]; nothing else in the module uses one.
		array = true
		p.i++
	}
	path, err := p.parseKeyPath()
	if err != nil {
		return err
	}
	p.skipSpace()
	if p.peek() != ']' {
		return p.errf("expected ']' to close table header [%s]", strings.Join(path, "."))
	}
	p.i++
	if array {
		if p.peek() != ']' {
			return p.errf("expected ']]' to close array-of-tables header [[%s]]",
				strings.Join(path, "."))
		}
		p.i++
	}
	if err := p.endOfLine(); err != nil {
		return err
	}

	if !array {
		p.cur = p.root.ensure(path, line)
		return nil
	}
	if len(path) == 0 {
		return p.errf("array-of-tables header needs a name")
	}
	parent := p.root.ensure(path[:len(path)-1], line)
	key := path[len(path)-1]
	p.cur = parent.appendArray(key, parent.Qualify(key), line)
	return nil
}

func (p *tomlParser) parseKeyPath() ([]string, error) {
	var parts []string
	for {
		p.skipSpace()
		part, err := p.parseKeyPart()
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
		p.skipSpace()
		if p.peek() == '.' {
			p.i++
			continue
		}
		return parts, nil
	}
}

func isBareKeyByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func (p *tomlParser) parseKeyPart() (string, error) {
	switch p.peek() {
	case '"':
		if p.at(1) == '"' && p.at(2) == '"' {
			return "", p.errf("multi-line strings are not supported as keys")
		}
		return p.parseBasicString()
	case '\'':
		if p.at(1) == '\'' && p.at(2) == '\'' {
			return "", p.errf("multi-line strings are not supported as keys")
		}
		return p.parseLiteralString()
	default:
		start := p.i
		for !p.eof() && isBareKeyByte(p.peek()) {
			p.i++
		}
		if p.i == start {
			return "", p.errf("expected a key")
		}
		return string(p.src[start:p.i]), nil
	}
}

func (p *tomlParser) parseKeyValue() error {
	line := p.line
	path, err := p.parseKeyPath()
	if err != nil {
		return err
	}
	p.skipSpace()
	if p.peek() != '=' {
		return p.errf("expected '=' after key %q", strings.Join(path, "."))
	}
	p.i++
	p.skipSpace()

	v, ok, why, err := p.parseValue()
	if err != nil {
		return err
	}
	if err := p.endOfLine(); err != nil {
		return err
	}

	tbl := p.cur
	if len(path) > 1 {
		tbl = p.cur.ensure(path[:len(path)-1], line)
	}
	key := path[len(path)-1]
	if !ok {
		p.warnf(line, "key %q skipped: %s", tbl.Qualify(key), why)
		return nil
	}
	v.Line = line
	if !tbl.set(key, v) {
		p.warnf(line, "duplicate key %q; the last value wins", tbl.Qualify(key))
	}
	return nil
}

// parseValue returns (value, supported, whyUnsupported, error). An unsupported
// value is still fully CONSUMED before returning, so the parser stays in sync
// and the rest of the manifest still loads.
func (p *tomlParser) parseValue() (Value, bool, string, error) {
	if p.eof() {
		return Value{}, false, "", p.errf("expected a value")
	}
	switch c := p.peek(); {
	case c == '"':
		if p.at(1) == '"' && p.at(2) == '"' {
			if err := p.skipMultilineString('"'); err != nil {
				return Value{}, false, "", err
			}
			return Value{}, false, "multi-line strings are not supported", nil
		}
		s, err := p.parseBasicString()
		if err != nil {
			return Value{}, false, "", err
		}
		return Value{Kind: KindString, Str: s}, true, "", nil

	case c == '\'':
		if p.at(1) == '\'' && p.at(2) == '\'' {
			if err := p.skipMultilineString('\''); err != nil {
				return Value{}, false, "", err
			}
			return Value{}, false, "multi-line strings are not supported", nil
		}
		s, err := p.parseLiteralString()
		if err != nil {
			return Value{}, false, "", err
		}
		return Value{Kind: KindString, Str: s}, true, "", nil

	case c == '[':
		return p.parseArray()

	case c == '{':
		if err := p.skipInlineTable(); err != nil {
			return Value{}, false, "", err
		}
		return Value{}, false, "inline tables are not supported", nil

	default:
		return p.parseBareToken()
	}
}

// bareTokenEnd delimits an unquoted value. ']' and ',' are included so array
// elements terminate correctly.
func isValueDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', ',', ']', '}', '#':
		return true
	}
	return false
}

func (p *tomlParser) parseBareToken() (Value, bool, string, error) {
	start := p.i
	for !p.eof() && !isValueDelim(p.peek()) {
		p.i++
	}
	tok := string(p.src[start:p.i])
	if tok == "" {
		return Value{}, false, "", p.errf("expected a value")
	}
	switch tok {
	case "true":
		return Value{Kind: KindBool, Bool: true}, true, "", nil
	case "false":
		return Value{Kind: KindBool, Bool: false}, true, "", nil
	}

	body := strings.TrimPrefix(strings.TrimPrefix(tok, "+"), "-")
	switch {
	case strings.ContainsAny(tok, ".eE") && !strings.HasPrefix(body, "0x"):
		return Value{}, false, "floats are not supported", nil
	case strings.Contains(tok, ":") || strings.ContainsAny(body, "-") || strings.ContainsAny(body, "Zz"):
		return Value{}, false, "dates and times are not supported", nil
	case strings.HasPrefix(body, "0x"), strings.HasPrefix(body, "0o"), strings.HasPrefix(body, "0b"):
		return Value{}, false, "only decimal integers are supported", nil
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(tok, "_", ""), 10, 64)
	if err != nil {
		return Value{}, false, fmt.Sprintf("unrecognized value %q", tok), nil
	}
	return Value{Kind: KindInt, Int: n}, true, "", nil
}

func (p *tomlParser) parseArray() (Value, bool, string, error) {
	openLine := p.line
	p.i++ // '['
	out := []string{}
	unsupported := ""
	for {
		p.skipBlank()
		if p.eof() {
			// Report the OPENING line. By the time EOF is reached the parser
			// has walked to the end of the file, and pointing an author at the
			// last line of the manifest for a bracket they left open several
			// lines earlier is not a diagnosis.
			p.line = openLine
			return Value{}, false, "", p.errf("unterminated array")
		}
		if p.peek() == ']' {
			p.i++
			break
		}
		ev, ok, why, err := p.parseValue()
		if err != nil {
			return Value{}, false, "", err
		}
		switch {
		case !ok:
			if unsupported == "" {
				unsupported = why
			}
		case ev.Kind != KindString:
			if unsupported == "" {
				unsupported = "only arrays of strings are supported"
			}
		default:
			out = append(out, ev.Str)
		}
		p.skipBlank()
		if p.peek() == ',' {
			p.i++
			continue
		}
		if p.peek() == ']' {
			p.i++
			break
		}
		return Value{}, false, "", p.errf("expected ',' or ']' in array")
	}
	if unsupported != "" {
		return Value{}, false, unsupported, nil
	}
	return Value{Kind: KindStringArray, Array: out}, true, "", nil
}

func (p *tomlParser) parseBasicString() (string, error) {
	openLine := p.line
	p.i++ // '"'
	var b strings.Builder
	for {
		if p.eof() || p.peek() == '\n' {
			p.line = openLine
			return "", p.errf("unterminated string")
		}
		c := p.advance()
		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if p.eof() {
				p.line = openLine
				return "", p.errf("unterminated escape")
			}
			e := p.advance()
			switch e {
			case 'b':
				b.WriteByte('\b')
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			case 'f':
				b.WriteByte('\f')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'u', 'U':
				n := 4
				if e == 'U' {
					n = 8
				}
				if p.i+n > len(p.src) {
					return "", p.errf("truncated \\%c escape", e)
				}
				hex := string(p.src[p.i : p.i+n])
				cp, err := strconv.ParseUint(hex, 16, 32)
				if err != nil {
					return "", p.errf("invalid \\%c escape %q", e, hex)
				}
				p.i += n
				r := rune(cp)
				if !utf8.ValidRune(r) {
					r = utf8.RuneError
				}
				b.WriteRune(r)
			default:
				return "", p.errf("unknown escape \\%c", e)
			}
		default:
			b.WriteByte(c)
		}
	}
}

func (p *tomlParser) parseLiteralString() (string, error) {
	openLine := p.line
	p.i++ // '\''
	start := p.i
	for {
		if p.eof() || p.peek() == '\n' {
			p.line = openLine
			return "", p.errf("unterminated literal string")
		}
		if p.peek() == '\'' {
			s := string(p.src[start:p.i])
			p.i++
			return s, nil
		}
		p.advance()
	}
}

func (p *tomlParser) skipMultilineString(q byte) error {
	openLine := p.line
	p.i += 3
	for {
		if p.eof() {
			p.line = openLine
			return p.errf("unterminated multi-line string")
		}
		if p.peek() == q && p.at(1) == q && p.at(2) == q {
			p.i += 3
			return nil
		}
		p.advance()
	}
}

func (p *tomlParser) skipInlineTable() error {
	openLine := p.line
	depth := 0
	for {
		if p.eof() {
			p.line = openLine
			return p.errf("unterminated inline table")
		}
		switch p.peek() {
		case '{':
			depth++
			p.i++
		case '}':
			depth--
			p.i++
			if depth == 0 {
				return nil
			}
		case '"':
			if _, err := p.parseBasicString(); err != nil {
				return err
			}
		case '\'':
			if _, err := p.parseLiteralString(); err != nil {
				return err
			}
		default:
			p.advance()
		}
	}
}
