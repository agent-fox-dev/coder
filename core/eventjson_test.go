package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A minimal encoder standing in for the session codec: enough to prove the
// envelope, not the message shape.
func stubEncoder(m Message) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"role": string(m.Role())})
}

// TestEventsSerializeAsADiscriminatedUnion is REQ-OBS-06c: a `type` member
// plus exactly the fields the variant carries — no shared envelope with every
// field optional.
func TestEventsSerializeAsADiscriminatedUnion(t *testing.T) {
	cases := []struct {
		ev   Event
		want []string // keys that must be present, and no others
	}{
		{AgentStartEvent{SessionID: "s", Provider: "p", API: "a", Model: "m"}, []string{"type", "session_id", "provider", "api", "model"}},
		{AgentDoneEvent{Result: RunResult{StopReason: RunStopEndTurn}}, []string{"type", "stop_reason", "last_reason", "turn_count", "error", "run_usage", "messages", "usage"}},
		{TurnStartEvent{TurnIndex: 1}, []string{"type", "turn_index"}},
		{TurnEndEvent{TurnIndex: 1, ToolResults: []ToolResultMessage{{ToolUseID: "x"}}}, []string{"type", "turn_index", "message", "tool_results", "usage"}},
		{MessageStartEvent{}, []string{"type", "message"}},
		{MessageUpdateEvent{}, []string{"type", "message"}},
		{MessageEndEvent{}, []string{"type", "message"}},
		{TextStartEvent{BlockIndex: 0}, []string{"type", "block_index"}},
		{TextDeltaEvent{Delta: "d"}, []string{"type", "block_index", "delta"}},
		{TextEndEvent{Text: "t"}, []string{"type", "block_index", "text"}},
		{ThinkingStartEvent{}, []string{"type", "block_index"}},
		{ThinkingDeltaEvent{Delta: "d"}, []string{"type", "block_index", "delta"}},
		{ThinkingEndEvent{Thinking: "t"}, []string{"type", "block_index", "thinking", "signature", "redacted"}},
		{ToolCallStartEvent{ToolUseID: "1", Name: "n"}, []string{"type", "block_index", "tool_use_id", "name"}},
		{ToolInputDeltaEvent{ToolUseID: "1", Delta: "{"}, []string{"type", "block_index", "tool_use_id", "delta"}},
		{ToolCallEndEvent{Block: ToolUseBlock{ID: "1", Name: "n", Input: json.RawMessage(`{"b":1,"a":2}`)}}, []string{"type", "block_index", "tool_use_id", "name", "input"}},
		{ToolExecutionStartEvent{ToolUseID: "1", Name: "n"}, []string{"type", "tool_use_id", "name"}},
		{ToolExecutionUpdateEvent{ToolUseID: "1", Name: "n", Chunk: "c"}, []string{"type", "tool_use_id", "name", "chunk"}},
		{ToolExecutionEndEvent{ToolUseID: "1", Name: "n"}, []string{"type", "tool_use_id", "name", "is_error", "elapsed_ms"}},
		{ToolResultEvent{}, []string{"type", "message"}},
		{ErrorEvent{Message: "boom", Err: errors.New("boom"), Terminal: true}, []string{"type", "message", "terminal"}},
	}
	for _, c := range cases {
		b, err := MarshalEvent(c.ev, stubEncoder)
		if err != nil {
			t.Fatalf("%T: %v", c.ev, err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%T: not an object: %s", c.ev, b)
		}
		if string(got["type"]) != `"`+string(c.ev.EventType())+`"` {
			t.Errorf("%T: type = %s, want %q", c.ev, got["type"], c.ev.EventType())
		}
		// The type member is FIRST, so a consumer can dispatch before decoding
		// the rest.
		if !strings.HasPrefix(string(b), `{"type":`) {
			t.Errorf("%T: type is not the first member: %s", c.ev, b)
		}
		for _, k := range c.want {
			if _, ok := got[k]; !ok {
				t.Errorf("%T: missing %q in %s", c.ev, k, b)
			}
		}
		if len(got) != len(c.want) {
			t.Errorf("%T: %d members, want exactly %d (%s): no optional-everything envelope", c.ev, len(got), len(c.want), b)
		}
	}
}

// TestToolCallEndCarriesTheModelsOwnBytes: the tool-call input is the
// model's argument bytes verbatim (REQ-TOOL-12), never a re-marshalled map
// with sorted keys.
func TestToolCallEndCarriesTheModelsOwnBytes(t *testing.T) {
	in := json.RawMessage(`{"z":1,"a":{"y":2,"b":3}}`)
	b, err := MarshalEvent(ToolCallEndEvent{Block: ToolUseBlock{ID: "1", Name: "n", Input: in}}, stubEncoder)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), string(in)) {
		t.Fatalf("input bytes were re-encoded: %s", b)
	}
}

func TestMarshalEventRequiresAnEncoder(t *testing.T) {
	if _, err := MarshalEvent(TurnStartEvent{}, nil); err == nil {
		t.Fatal("a nil encoder must be an error, not a panic on the first message event")
	}
}
