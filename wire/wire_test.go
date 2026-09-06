package wire_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/wire"
)

func rejects(t *testing.T, data string, l wire.Limits, want wire.Rule) {
	t.Helper()
	err := wire.Guard([]byte(data), l)
	if err == nil {
		t.Fatalf("Guard accepted %q; want a %s rejection", clip(data), want)
	}
	var we *wire.Error
	if !errors.As(err, &we) {
		t.Fatalf("err = %v (%T), want a *wire.Error", err, err)
	}
	if we.Rule != want {
		t.Fatalf("rule = %s, want %s (%v)", we.Rule, want, err)
	}
	if !errors.Is(err, wire.ErrRejected) {
		t.Fatal("every rejection must match ErrRejected, so a transport can implement " +
			"REQ-SEC-11.4's poisoning without enumerating rules")
	}
	// Parse must agree with Guard: one walk, two entry points.
	if _, perr := wire.Parse([]byte(data), l); perr == nil {
		t.Fatal("Parse accepted what Guard rejected; the two must not disagree about " +
			"what is legal")
	}
}

func clip(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// TestDuplicateKeysAreRejected is REQ-SEC-11.3, and the reason it is a
// rejection rather than a resolution.
//
// Last-wins hands an untrusted peer the choice of which of two values AgentKit
// sees — and the one it does not see is the one a human reviewing the message
// read. encoding/json accepts this silently, which is REQ-SEC-11.5's whole
// point.
func TestDuplicateKeysAreRejected(t *testing.T) {
	for _, data := range []string{
		`{"a":1,"a":2}`,
		`{"method":"tools/list","method":"tools/call"}`,
		`{"x":{"dup":1,"dup":2}}`,
		`[{"a":1},{"b":1,"b":2}]`,
	} {
		rejects(t, data, wire.Limits{}, wire.RuleDuplicateKey)
	}

	// The standard library disagrees, which is exactly why this package exists.
	var m map[string]any
	if err := json.Unmarshal([]byte(`{"a":1,"a":2}`), &m); err != nil {
		t.Fatalf("encoding/json refused a duplicate key, so this test's premise is stale: %v", err)
	}
	if m["a"] != float64(2) {
		t.Fatalf("encoding/json resolved to %v; the premise is that it silently takes the last", m["a"])
	}
}

// TestDuplicateDetectionSurvivesTheMapPromotion: the object scanner uses a
// linear scan for small objects and promotes to a map past a threshold, and a
// duplicate must be caught on both sides of that switch.
func TestDuplicateDetectionSurvivesTheMapPromotion(t *testing.T) {
	for _, n := range []int{2, 16, 17, 64} {
		var b strings.Builder
		b.WriteByte('{')
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, `"k%d":%d,`, i, i)
		}
		fmt.Fprint(b_ptr(&b), `"k0":999}`)
		rejects(t, b.String(), wire.Limits{}, wire.RuleDuplicateKey)
	}
}

func b_ptr(b *strings.Builder) *strings.Builder { return b }

func TestDepthIsBounded(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 200) + `1` + strings.Repeat(`}`, 200)
	rejects(t, deep, wire.Limits{MaxDepth: 64}, wire.RuleDepth)

	arrays := strings.Repeat(`[`, 200) + strings.Repeat(`]`, 200)
	rejects(t, arrays, wire.Limits{MaxDepth: 64}, wire.RuleDepth)

	// Exactly at the limit is legal; the bound is a maximum, not a margin.
	ok := strings.Repeat(`{"a":`, 64) + `1` + strings.Repeat(`}`, 64)
	if err := wire.Guard([]byte(ok), wire.Limits{MaxDepth: 64}); err != nil {
		t.Fatalf("depth 64 with a limit of 64 was rejected: %v", err)
	}
}

func TestContainerLengthIsBounded(t *testing.T) {
	var arr strings.Builder
	arr.WriteByte('[')
	for i := 0; i < 50; i++ {
		if i > 0 {
			arr.WriteByte(',')
		}
		arr.WriteByte('1')
	}
	arr.WriteByte(']')
	rejects(t, arr.String(), wire.Limits{MaxContainerLen: 10}, wire.RuleContainerLen)

	var obj strings.Builder
	obj.WriteByte('{')
	for i := 0; i < 50; i++ {
		if i > 0 {
			obj.WriteByte(',')
		}
		fmt.Fprintf(&obj, `"k%d":1`, i)
	}
	obj.WriteByte('}')
	rejects(t, obj.String(), wire.Limits{MaxContainerLen: 10}, wire.RuleContainerLen)
}

func TestMessageSizeIsBounded(t *testing.T) {
	big := `{"a":"` + strings.Repeat("x", 4096) + `"}`
	rejects(t, big, wire.Limits{MaxMessageBytes: 1024}, wire.RuleMessageBytes)
}

func TestSyntaxErrorsAreRejectionsNotPanics(t *testing.T) {
	for _, data := range []string{
		``, `{`, `[`, `{"a"`, `{"a":}`, `{,}`, `[,]`, `tru`, `nul`, `01`, `-`, `1.`, `1e`,
		`"unterminated`, `{"a":1}{"b":2}`, "\x00", `{"a":"\q"}`, `{"a":"\u00"}`,
		"{\"a\":\"\x01\"}", `[1,]`, `{"a":1,}`,
	} {
		if err := wire.Guard([]byte(data), wire.Limits{}); err == nil {
			t.Fatalf("Guard accepted malformed input %q", data)
		}
	}
}

// ------------------------------------------------------------------ Parse

func TestParsePreservesNumberLiteralsAndMemberOrder(t *testing.T) {
	v, err := wire.Parse([]byte(`{"zebra":1024,"apple":1024.0,"pear":1e3}`), wire.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.Keys, ",") != "zebra,apple,pear" {
		t.Fatalf("keys = %v, want the order the peer sent", v.Keys)
	}
	for k, want := range map[string]string{"zebra": "1024", "apple": "1024.0", "pear": "1e3"} {
		m, _ := v.Get(k)
		if string(m.Number) != want {
			t.Fatalf("%s = %q, want the verbatim literal %q. 1024, 1024.0 and 1e3 are one "+
				"float64 and three different messages.", k, m.Number, want)
		}
	}
}

func TestParseUnescapesStrings(t *testing.T) {
	cases := map[string]string{
		`"plain"`:         "plain",
		`"tab\there"`:     "tab\there",
		`"quote\"inside"`: `quote"inside`,
		`"A"`:             "A",
		`"😀"`:             "\U0001F600",
		`"back\\slash"`:   `back\slash`,
		`"é"`:             "é",
	}
	for in, want := range cases {
		v, err := wire.Parse([]byte(in), wire.Limits{})
		if err != nil {
			t.Fatalf("Parse(%s): %v", in, err)
		}
		if v.String != want {
			t.Fatalf("Parse(%s) = %q, want %q", in, v.String, want)
		}
	}
}

// TestAnUnpairedSurrogateIsReplacedNotRejected: a peer with a UTF-16 runtime
// emits them for legitimate reasons, and rejecting the message would refuse
// valid traffic over a representation detail. encoding/json makes the same
// choice.
func TestAnUnpairedSurrogateIsReplacedNotRejected(t *testing.T) {
	v, err := wire.Parse([]byte(`"\ud83d"`), wire.Limits{})
	if err != nil {
		t.Fatalf("an unpaired surrogate must not reject the message: %v", err)
	}
	if v.String != "�" {
		t.Fatalf("got %q, want the replacement character", v.String)
	}
}

func TestJSONRoundTripPreservesOrderAndLiterals(t *testing.T) {
	const in = `{"zebra":1024,"apple":[1,2.50,3e2],"nested":{"b":null,"a":true}}`
	v, err := wire.Parse([]byte(in), wire.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := v.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round trip = %s, want %s", out, in)
	}
}

// ------------------------------------------------------------------ Bind

type toolCall struct {
	Name      string          `json:"name"`
	Arguments map[string]any  `json:"arguments"`
	Timeout   int             `json:"timeout"`
	Ratio     float64         `json:"ratio"`
	Enabled   bool            `json:"enabled"`
	Raw       json.RawMessage `json:"raw"`
	Hidden    string          `json:"-"`
	unexpo    string
}

// TestAnUnknownPropertyIsARejection is REQ-SEC-12.1.
//
// A peer that can smuggle extra fields past the parser reaches code paths the
// schema was meant to gate, and the smuggled field is invisible in every
// review of the struct that was supposed to describe the message.
func TestAnUnknownPropertyIsARejection(t *testing.T) {
	v, err := wire.Parse([]byte(`{"name":"read","elevate":true}`), wire.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var tc toolCall
	err = wire.Bind(v, &tc)
	var we *wire.Error
	if !errors.As(err, &we) || we.Rule != wire.RuleUnknownField {
		t.Fatalf("err = %v, want an unknown_field rejection", err)
	}
	if !strings.Contains(we.Error(), "elevate") {
		t.Fatalf("the rejection must name the property: %v", we)
	}
	if !strings.Contains(we.Error(), "arguments") {
		t.Fatalf("the rejection should list what IS declared, so the peer's author can "+
			"see the difference: %v", we)
	}
}

// TestAFieldTaggedDashIsUnknownNotIgnored: `json:"-"` means the property does
// not exist, and silently ignoring it is the lenient behaviour this package
// exists to not have.
func TestAFieldTaggedDashIsUnknownNotIgnored(t *testing.T) {
	v, _ := wire.Parse([]byte(`{"Hidden":"x"}`), wire.Limits{})
	var tc toolCall
	if err := wire.Bind(v, &tc); err == nil {
		t.Fatal("a field excluded with json:\"-\" must make the property unknown")
	}
}

// TestFieldMatchingIsCaseSensitive is a hole encoding/json has and this must
// not.
//
// `id` and `Id` are two DISTINCT keys, so duplicate-key rejection does not
// catch them. Case-insensitive field matching then binds both to one field
// with the last one winning — reintroducing, one layer up, precisely the
// last-wins that REQ-SEC-11.3 rejects.
func TestFieldMatchingIsCaseSensitive(t *testing.T) {
	v, err := wire.Parse([]byte(`{"name":"a","Name":"b"}`), wire.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var tc toolCall
	err = wire.Bind(v, &tc)
	var we *wire.Error
	if !errors.As(err, &we) || we.Rule != wire.RuleUnknownField {
		t.Fatalf("err = %v; \"Name\" must be UNKNOWN, not a case-folded alias of \"name\" "+
			"that lets a peer choose which value is bound", err)
	}
}

// TestIntegralFloatsSatisfyIntegerFields is REQ-SEC-12.2.
//
// A peer whose runtime has one number type writes 1.0 where it means 1, and a
// decoder that insists on wire-integers rejects legal messages.
func TestIntegralFloatsSatisfyIntegerFields(t *testing.T) {
	for _, lit := range []string{"30", "30.0", "3e1", "30.000"} {
		v, err := wire.Parse([]byte(`{"timeout":`+lit+`}`), wire.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		var tc toolCall
		if err := wire.Bind(v, &tc); err != nil {
			t.Fatalf("Bind(timeout=%s): %v", lit, err)
		}
		if tc.Timeout != 30 {
			t.Fatalf("timeout = %d from %s, want 30", tc.Timeout, lit)
		}
	}
	// A non-integral value is still a type error.
	v, _ := wire.Parse([]byte(`{"timeout":1.5}`), wire.Limits{})
	var tc toolCall
	if err := wire.Bind(v, &tc); err == nil {
		t.Fatal("1.5 must not satisfy an integer field")
	}
}

// TestIntegersOutsideTheSafeRangeAreRejected is the other half of
// REQ-SEC-12.2's "only within the IEEE-754 safe-integer range".
//
// Past 2^53 a double no longer represents every integer, so 1e19 does not name
// any particular one and accepting it would invent a value the peer did not
// send.
func TestIntegersOutsideTheSafeRangeAreRejected(t *testing.T) {
	v, _ := wire.Parse([]byte(`{"timeout":1e19}`), wire.Limits{})
	var tc toolCall
	err := wire.Bind(v, &tc)
	var we *wire.Error
	if !errors.As(err, &we) || we.Rule != wire.RuleRange {
		t.Fatalf("err = %v, want a range rejection", err)
	}

	// An exact int64 literal is fine even past 2^53: it arrived as an integer
	// and never went through a float.
	v, _ = wire.Parse([]byte(`{"timeout":9007199254740993}`), wire.Limits{})
	tc = toolCall{}
	if err := wire.Bind(v, &tc); err != nil {
		t.Fatalf("an exact integer literal must bind: %v", err)
	}
	if tc.Timeout != 9007199254740993 {
		t.Fatalf("timeout = %d; the literal must not be laundered through a float64",
			tc.Timeout)
	}
}

func TestIntegersSatisfyFloatFields(t *testing.T) {
	v, _ := wire.Parse([]byte(`{"ratio":2}`), wire.Limits{})
	var tc toolCall
	if err := wire.Bind(v, &tc); err != nil {
		t.Fatal(err)
	}
	if tc.Ratio != 2 {
		t.Fatalf("ratio = %v, want 2", tc.Ratio)
	}
}

// TestExplicitNullNeverPanics is REQ-SEC-12.4.
//
// A reflective setter that panics on null means any peer can crash the process
// with a two-word payload.
func TestExplicitNullNeverPanics(t *testing.T) {
	const data = `{"name":null,"arguments":null,"timeout":null,"ratio":null,` +
		`"enabled":null,"raw":null}`
	v, err := wire.Parse([]byte(data), wire.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	tc := toolCall{Name: "prior", Timeout: 7}
	if err := wire.Bind(v, &tc); err != nil {
		t.Fatalf("null must bind as a zero value, not an error: %v", err)
	}
	if tc.Name != "" || tc.Timeout != 0 || tc.Arguments != nil {
		t.Fatalf("null left %+v; it must zero the field", tc)
	}

	// And into an untyped field, which is where the panic actually lives.
	var anyTarget struct {
		X any `json:"x"`
	}
	v, _ = wire.Parse([]byte(`{"x":null}`), wire.Limits{})
	if err := wire.Bind(v, &anyTarget); err != nil {
		t.Fatalf("null into an untyped field: %v", err)
	}
	if anyTarget.X != nil {
		t.Fatalf("x = %v, want nil", anyTarget.X)
	}
}

func TestUntypedFieldsCarryNumberLiterals(t *testing.T) {
	var target struct {
		Args map[string]any `json:"args"`
	}
	v, _ := wire.Parse([]byte(`{"args":{"big":9007199254740993}}`), wire.Limits{})
	if err := wire.Bind(v, &target); err != nil {
		t.Fatal(err)
	}
	n, ok := target.Args["big"].(json.Number)
	if !ok {
		t.Fatalf("big is %T, want json.Number: a float64 round trip turns "+
			"9007199254740993 into ...992 and nothing downstream can tell", target.Args["big"])
	}
	if string(n) != "9007199254740993" {
		t.Fatalf("big = %s, want the literal", n)
	}
}

func TestRawMessageFieldsKeepTheirBytes(t *testing.T) {
	var target struct {
		Raw json.RawMessage `json:"raw"`
	}
	v, _ := wire.Parse([]byte(`{"raw":{"zebra":1,"apple":2}}`), wire.Limits{})
	if err := wire.Bind(v, &target); err != nil {
		t.Fatal(err)
	}
	if string(target.Raw) != `{"zebra":1,"apple":2}` {
		t.Fatalf("raw = %s, want the member order the peer sent", target.Raw)
	}
}

// ---- Validator (REQ-SEC-12.3)

type versioned struct {
	Version string `json:"version"`
}

func (v versioned) Validate() error {
	if v.Version != "2.0" {
		return fmt.Errorf("jsonrpc version must be 2.0, got %q", v.Version)
	}
	return nil
}

type envelope struct {
	Inner versioned `json:"inner"`
}

// TestTheValidatorHookRunsPerStructAtItsOwnPath is REQ-SEC-12.3.
//
// Validating only the root leaves the caller to work out which of forty
// elements was wrong. Running as each struct is filled reports it where it is.
func TestTheValidatorHookRunsPerStructAtItsOwnPath(t *testing.T) {
	v, _ := wire.Parse([]byte(`{"inner":{"version":"1.0"}}`), wire.Limits{})
	var e envelope
	err := wire.Bind(v, &e)
	var we *wire.Error
	if !errors.As(err, &we) || we.Rule != wire.RuleValidator {
		t.Fatalf("err = %v, want a validator rejection", err)
	}
	if we.Path != "$.inner" {
		t.Fatalf("path = %q, want $.inner: the failure belongs at the struct that failed",
			we.Path)
	}
	if !strings.Contains(we.Error(), "2.0") {
		t.Fatalf("the validator's own message must survive: %v", we)
	}

	v, _ = wire.Parse([]byte(`{"inner":{"version":"2.0"}}`), wire.Limits{})
	e = envelope{}
	if err := wire.Bind(v, &e); err != nil {
		t.Fatalf("a valid message must bind: %v", err)
	}
}

func TestBindHandlesNestedAndEmbeddedShapes(t *testing.T) {
	type base struct {
		ID string `json:"id"`
	}
	type req struct {
		base
		Params []struct {
			K string `json:"k"`
		} `json:"params"`
		Meta *struct {
			N int `json:"n"`
		} `json:"meta"`
	}
	v, err := wire.Parse([]byte(`{"id":"1","params":[{"k":"a"},{"k":"b"}],"meta":{"n":3}}`), wire.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var r req
	if err := wire.Bind(v, &r); err != nil {
		t.Fatal(err)
	}
	if r.ID != "1" || len(r.Params) != 2 || r.Params[1].K != "b" || r.Meta == nil || r.Meta.N != 3 {
		t.Fatalf("bound %+v", r)
	}
}

// ------------------------------------------------------------------ framing

func TestNDJSONFramesAreBounded(t *testing.T) {
	body := `{"a":1}` + "\n" + `{"b":2}` + "\n"
	r := wire.NewNDJSON(strings.NewReader(body), wire.Limits{})
	for _, want := range []string{`{"a":1}`, `{"b":2}`} {
		got, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("frame = %s, want %s", got, want)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}

	big := wire.NewNDJSON(strings.NewReader(strings.Repeat("x", 5000)+"\n"), wire.Limits{MaxMessageBytes: 1024})
	if _, err := big.Next(); !errors.Is(err, wire.ErrRejected) {
		t.Fatalf("err = %v, want a rejection", err)
	}
}

// TestAContentLengthIsRangeCheckedInUint64 is REQ-SEC-11.1.
//
// Parse it as int on a 32-bit build and a declared 2^31 wraps NEGATIVE, sails
// past a `> max` check, and panics on a negative slice bound — a remote crash
// from a header field. Parsing wide and narrowing late costs nothing on the
// platform where the bug is invisible.
func TestAContentLengthIsRangeCheckedInUint64(t *testing.T) {
	for _, declared := range []string{
		"2147483648",           // 2^31
		"4294967296",           // 2^32
		"18446744073709551615", // 2^64-1
		"9223372036854775808",  // 2^63, which overflows a signed parse
	} {
		frame := "Content-Length: " + declared + "\r\n\r\n"
		r := wire.NewContentLength(strings.NewReader(frame), wire.Limits{})
		_, err := r.Next()
		var we *wire.Error
		if !errors.As(err, &we) || we.Rule != wire.RuleMessageBytes {
			t.Fatalf("Content-Length %s gave %v, want a message_bytes rejection", declared, err)
		}
	}
	// A negative value is not a number at all in uint64, which is the point.
	r := wire.NewContentLength(strings.NewReader("Content-Length: -1\r\n\r\n"), wire.Limits{})
	if _, err := r.Next(); err == nil {
		t.Fatal("a negative Content-Length must be refused")
	}
}

// TestNoBufferIsPreAllocatedToADeclaredSize is REQ-SEC-11.2.
//
// A peer announcing 16 MiB and sending one byte must cost one byte. Otherwise
// the declared number is a free allocation primitive: a few hundred connections
// each announcing the maximum is gigabytes the peer never had to send.
func TestNoBufferIsPreAllocatedToADeclaredSize(t *testing.T) {
	const declared = 16 << 20
	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n{", declared) // one byte of body
	r := wire.NewContentLength(strings.NewReader(frame), wire.Limits{})

	before := totalAlloc()
	_, err := r.Next()
	after := totalAlloc()

	if err == nil {
		t.Fatal("a truncated frame must be refused")
	}
	if grew := after - before; grew > declared/4 {
		t.Fatalf("reading a 1-byte body under a %d-byte declaration allocated %d bytes; "+
			"the buffer is sized to what the peer DECLARED rather than to what arrived, "+
			"which makes the declared number a free allocation primitive", declared, grew)
	}
}

// TestAReaderIsPoisonedByItsFirstMalformedFrame is REQ-SEC-11.4.
//
// Resynchronizing a framed stream after a parse error means guessing where the
// next frame starts, using framing that has already proved untrustworthy — and
// a peer that can desynchronize the framing then chooses what AgentKit reads as
// a message boundary.
func TestAReaderIsPoisonedByItsFirstMalformedFrame(t *testing.T) {
	body := strings.Repeat("x", 5000) + "\n" + `{"harmless":true}` + "\n"
	r := wire.NewNDJSON(strings.NewReader(body), wire.Limits{MaxMessageBytes: 1024})

	first, err1 := r.Next()
	if err1 == nil {
		t.Fatalf("the oversized frame must be refused, got %q", first)
	}
	if !r.Poisoned() {
		t.Fatal("the reader must report itself poisoned")
	}
	_, err2 := r.Next()
	if err2 == nil {
		t.Fatal("a poisoned reader must not resynchronize onto the next frame")
	}
	if err2.Error() != err1.Error() {
		t.Fatalf("second error %v differs from the first %v; the reader must keep "+
			"returning the failure that tore it down", err2, err1)
	}
}

func TestEOFDoesNotPoisonTheReader(t *testing.T) {
	r := wire.NewNDJSON(strings.NewReader(`{"a":1}`+"\n"), wire.Limits{})
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if r.Poisoned() {
		t.Fatal("a clean end of stream is an ending, not a malformed message")
	}
}

// ------------------------------------------------------------------ fuzz

// FuzzGuardNeverPanics is the property that matters most for this package: it
// reads bytes chosen by somebody else, so no input may reach a panic.
func FuzzGuardNeverPanics(f *testing.F) {
	for _, s := range []string{
		`{"a":1}`, `[1,2,3]`, `"x"`, `null`, `{"a":{"b":[1,{"c":null}]}}`,
		`{"a":1,"a":2}`, `{"a":"😀"}`, `{"a":"\ud800"}`, `1e309`,
		"\x00", `{`, `[`, `{"a":`, `"\u00"`, strings.Repeat(`[`, 100),
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		l := wire.Limits{MaxMessageBytes: 1 << 16, MaxContainerLen: 1000, MaxDepth: 32}
		gerr := wire.Guard(b, l)
		v, perr := wire.Parse(b, l)
		if (gerr == nil) != (perr == nil) {
			t.Fatalf("Guard and Parse disagree on %q: guard=%v parse=%v", b, gerr, perr)
		}
		if perr != nil {
			return
		}
		// A parsed tree must re-encode to something that parses again.
		out, err := v.JSON()
		if err != nil {
			t.Fatalf("re-encoding %q failed: %v", b, err)
		}
		if err := wire.Guard(out, l); err != nil {
			t.Fatalf("re-encoded %q -> %q which no longer parses: %v", b, out, err)
		}
		if _, err := v.Any(); err != nil {
			t.Fatalf("Any() on a parsed tree failed: %v", err)
		}
	})
}

// FuzzBindNeverPanics covers the reflective setter, which is where
// REQ-SEC-12.4's crash lives.
func FuzzBindNeverPanics(f *testing.F) {
	for _, s := range []string{
		`{"name":"x"}`, `{"name":null}`, `{"timeout":1e400}`, `{"arguments":{"a":null}}`,
		`{"raw":[1,2]}`, `{"enabled":"no"}`, `{"ratio":{}}`, `[]`, `null`, `5`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := wire.Parse(b, wire.Limits{MaxMessageBytes: 1 << 16, MaxDepth: 16})
		if err != nil {
			return
		}
		var tc toolCall
		_ = wire.Bind(v, &tc)
		var anyTarget any
		_ = wire.Bind(v, &anyTarget)
	})
}

// BenchmarkGuard measures the cost of the extra scan the providers now pay per
// SSE event, so the decision to add it is on the record with a number rather
// than an assurance (NFR-PERF-09's habit, applied to a security control).
func BenchmarkGuard(b *testing.B) {
	payload := []byte(`{"type":"content_block_delta","index":0,"delta":` +
		`{"type":"text_delta","text":"a chunk of streamed model output, about this long"}}`)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := wire.Guard(payload, wire.Limits{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkUnmarshalSamePayload is the baseline it is compared against.
func BenchmarkUnmarshalSamePayload(b *testing.B) {
	payload := []byte(`{"type":"content_block_delta","index":0,"delta":` +
		`{"type":"text_delta","text":"a chunk of streamed model output, about this long"}}`)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var v struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(payload, &v); err != nil {
			b.Fatal(err)
		}
	}
}
