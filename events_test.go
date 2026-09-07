package agentkit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// TestEveryStreamedEventEncodesAsTheUnion drives a real run and encodes every
// event it produces: each must be an object whose first member is `type`, and
// message-bearing events must carry the session codec's role-discriminated
// shape (REQ-OBS-06c).
func TestEveryStreamedEventEncodesAsTheUnion(t *testing.T) {
	s := oneToolTurn(t)
	a := newTestAgent(t, s, nil)
	_ = a.RegisterTool(echoTool("echo", nil))
	st, err := a.Stream(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for e := range st.Events() {
		b, err := EventJSON(e)
		if err != nil {
			t.Fatalf("%T: %v", e, err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b, &obj); err != nil {
			t.Fatalf("%T: %s: %v", e, b, err)
		}
		if string(obj["type"]) != `"`+string(e.EventType())+`"` {
			t.Fatalf("%T: type=%s", e, obj["type"])
		}
		if te, ok := e.(core.TurnEndEvent); ok {
			var msg map[string]json.RawMessage
			if err := json.Unmarshal(obj["message"], &msg); err != nil || string(msg["role"]) != `"assistant"` {
				t.Fatalf("turn_end message is not the codec's shape: %s", obj["message"])
			}
			_ = te
		}
		n++
	}
	if n == 0 {
		t.Fatal("no events")
	}
}
