package core

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/schema"
)

func toolUse(t *testing.T, id, name, args string) ToolUseBlock {
	t.Helper()
	b, err := NewToolUse(id, name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("NewToolUse: %v", err)
	}
	return b
}

func TestPreparedArgumentsPreserveKeyOrder(t *testing.T) {
	tool := Tool{
		Name: "t", InputSchema: schema.Object(
			schema.Prop("zeta", schema.String()), schema.Opt("alpha", schema.String())),
	}
	c := toolUse(t, "c1", "t", `{"zeta":"1","alpha":"2"}`)
	p, err := PrepareArguments(tool, c)
	if err != nil {
		t.Fatal(err)
	}
	if string(p.Raw) != `{"zeta":"1","alpha":"2"}` {
		t.Fatalf("Raw = %s; the model's own bytes must pass through untouched when "+
			"nothing changed (REQ-PROV-17)", p.Raw)
	}
}

func TestOptionalNullsAreDeletedNotRejected(t *testing.T) {
	// Constrained sampling forces the model to emit every declared property,
	// so optional fields arrive as explicit nulls.
	tool := Tool{
		Name: "t", InputSchema: schema.Object(
			schema.Prop("path", schema.String()), schema.Opt("limit", schema.Int())),
	}
	c := toolUse(t, "c1", "t", `{"path":"/x","limit":null}`)
	p, err := PrepareArguments(tool, c)
	if err != nil {
		t.Fatalf("an explicit null for an OPTIONAL property must be deleted, not rejected "+
			"(REQ-TOOL-11.2): %v", err)
	}
	if _, present := p.Args["limit"]; present {
		t.Fatal("the optional null was not deleted")
	}
}

func TestValidationErrorEchoesTheModelsOwnKeyOrder(t *testing.T) {
	tool := Tool{
		Name: "t", InputSchema: schema.Object(schema.Prop("path", schema.String())),
	}
	c := toolUse(t, "c1", "t", `{"zeta":1,"alpha":2}`)
	_, err := PrepareArguments(tool, c)
	if err == nil {
		t.Fatal("want a validation error for a missing required property")
	}
	msg := err.Error()
	zi, ai := strings.Index(msg, "zeta"), strings.Index(msg, "alpha")
	if zi < 0 || ai < 0 || zi > ai {
		t.Fatalf("error text did not echo the model's own key order (REQ-TOOL-12.3):\n%s", msg)
	}
}
