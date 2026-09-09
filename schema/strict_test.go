package schema_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/schema"
)

func strictErr(t *testing.T, s *schema.Schema) *schema.StrictRewriteError {
	t.Helper()
	_, err := schema.StrictSubset(s)
	if err == nil {
		t.Fatal("StrictSubset: want an error, got nil")
	}
	var se *schema.StrictRewriteError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a *StrictRewriteError", err)
	}
	if !errors.Is(err, schema.ErrInvalidSchema) {
		t.Fatalf("error %v is not ErrInvalidSchema", err)
	}
	return se
}

func marshal(t *testing.T, s *schema.Schema) string {
	t.Helper()
	b, err := s.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// REQ-TOOL-03. A dictionary object has no strict form: strict requires every
// key enumerated and additionalProperties:false, and a map of arbitrary keys
// is what cannot be enumerated. The rewrite used to overwrite the value
// schema with `false` and call the result strict — a tool the model could not
// call with any key at all. It is a rejection, so `prefer` falls back and
// `require` fails with the reason.
func TestADictionaryObjectIsRejectedByTheStrictRewriteNotSilentlyClosed(t *testing.T) {
	dict := &schema.Schema{Type: schema.TypeObject,
		AdditionalProperties: &schema.AdditionalProperties{Allowed: true, Schema: schema.String()}}
	s := schema.Object(schema.Prop("name", schema.String()), schema.Prop("labels", dict))

	se := strictErr(t, s)
	if se.Keyword != "additionalProperties" || se.Path != "/labels" {
		t.Fatalf("error = %+v, want additionalProperties at /labels", se)
	}
	if !strings.Contains(se.Error(), "dictionary") {
		t.Fatalf("Error() = %q, want the reason spelled out", se.Error())
	}
	if schema.StrictSubsetOK(s) {
		t.Fatal("StrictSubsetOK must report the rejection")
	}
	// The probe never mutates the shared schema.
	if s.Properties["labels"].AdditionalProperties.Schema == nil {
		t.Fatal("the probe closed the caller's own schema")
	}
}

// A bare additionalProperties:true is NOT a dictionary; it is an open object
// that the rewrite closes, as before.
func TestABareOpenObjectIsStillClosedByTheRewrite(t *testing.T) {
	s := schema.Object(schema.Prop("name", schema.String()))
	s.AdditionalProperties = &schema.AdditionalProperties{Allowed: true}
	out, err := schema.StrictSubset(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(marshal(t, out), `"additionalProperties":false`) {
		t.Fatalf("not closed: %s", marshal(t, out))
	}
}

// A widened optional property carried its description TWICE — on the anyOf
// wrapper and on the inner copy — so the model read it once per level.
func TestAWidenedOptionalPropertyCarriesItsDescriptionOnce(t *testing.T) {
	s := schema.Object(schema.Opt("note", schema.String("a free-text note")))
	out, err := schema.StrictSubset(s)
	if err != nil {
		t.Fatal(err)
	}
	j := marshal(t, out)
	if n := strings.Count(j, "a free-text note"); n != 1 {
		t.Fatalf("description appears %d times, want once:\n%s", n, j)
	}
	if !strings.Contains(j, `"note":{"description":"a free-text note","anyOf":[{"type":"string"},{"type":"null"}]}`) {
		t.Fatalf("wrapper shape drifted:\n%s", j)
	}
	// The caller's schema is untouched by the rewrite.
	if s.Properties["note"].Description != "a free-text note" {
		t.Fatal("the rewrite cleared the description on the caller's schema")
	}
}

// A schema with properties and no type is an object to every reader; the
// rewrite must close it and pin the type, not leave it open because the word
// "object" was missing.
func TestATypelessSchemaWithPropertiesIsRewrittenAsAnObject(t *testing.T) {
	s := &schema.Schema{Properties: map[string]*schema.Schema{"a": schema.String()}}
	out, err := schema.StrictSubset(s)
	if err != nil {
		t.Fatal(err)
	}
	j := marshal(t, out)
	for _, want := range []string{`"type":"object"`, `"additionalProperties":false`, `"required":["a"]`} {
		if !strings.Contains(j, want) {
			t.Fatalf("missing %s:\n%s", want, j)
		}
	}
	if s.Type != schema.TypeNone {
		t.Fatal("the rewrite pinned the type on the caller's schema")
	}
}

// Array(nil) marshals as a bare {"type":"array"}, which strict mode rejects
// on the wire with the whole request. Reject it before sending instead.
func TestAnArrayWithoutItemsIsRejectedUnderStrict(t *testing.T) {
	s := schema.Object(schema.Prop("tags", schema.Array(nil)))
	se := strictErr(t, s)
	if se.Keyword != "items" || se.Path != "/tags" {
		t.Fatalf("error = %+v, want items at /tags", se)
	}
	if strings.Contains(se.Error(), "forbids") {
		t.Fatalf("Error() = %q reads as if items were forbidden; it is required", se.Error())
	}
	ok := schema.Object(schema.Prop("tags", schema.Array(schema.String())))
	if !schema.StrictSubsetOK(ok) {
		t.Fatal("an array with items is strict-convertible")
	}
}
