package session

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// roundTrip writes one message entry through a real store and reads it back,
// which is the assertion NFR-TEST-03 actually asks for: on raw bytes through
// the store, not on a value passed to a codec in memory.
func roundTrip(t *testing.T, m core.Message) core.Message {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	s := mustCreate(t, path, testOptions("e"))
	if err := s.Append(NewMessageEntry(m)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	es := mustLoad(t, path).Entries()
	if len(es) != 1 || es[0].Message == nil {
		t.Fatalf("loaded %d entries", len(es))
	}
	return es[0].Message.Message
}

func TestEveryContentBlockSubtypeSurvivesTheLog(t *testing.T) {
	// NFR-TEST-03(a). One message carrying every block kind, including one
	// this build does not model.
	in := core.UserMessage{
		Timestamp: fixedTime,
		Content: core.Content{
			core.TextBlock{Text: "plain"},
			core.ThinkingBlock{Thinking: "hmm", Signature: "sig-abc", Redacted: true},
			mustToolUse(t, "tu_1", "search", `{"q":"x","limit":10}`),
			core.ToolResultBlock{
				ToolUseID: "tu_1",
				IsError:   true,
				Content:   core.Content{core.TextBlock{Text: "boom"}},
			},
			core.ImageBlock{Data: "aGk=", MimeType: "image/png"},
			core.RawBlock{Type: "video", Raw: json.RawMessage(`{"type":"video","url":"u","frames":3}`)},
		},
	}
	got, ok := roundTrip(t, in).(core.UserMessage)
	if !ok {
		t.Fatalf("round-tripped to %T", got)
	}
	if len(got.Content) != len(in.Content) {
		t.Fatalf("got %d blocks, want %d", len(got.Content), len(in.Content))
	}
	for i := range in.Content {
		if got.Content[i].BlockType() != in.Content[i].BlockType() {
			t.Errorf("block %d type = %q, want %q", i, got.Content[i].BlockType(), in.Content[i].BlockType())
		}
	}
	if !reflect.DeepEqual(got.Content[1], in.Content[1]) {
		t.Errorf("thinking block\n got %+v\nwant %+v", got.Content[1], in.Content[1])
	}
	if !reflect.DeepEqual(got.Content[3], in.Content[3]) {
		t.Errorf("tool_result block\n got %+v\nwant %+v", got.Content[3], in.Content[3])
	}
	if !reflect.DeepEqual(got.Content[4], in.Content[4]) {
		t.Errorf("image block\n got %+v\nwant %+v", got.Content[4], in.Content[4])
	}
	raw, ok := got.Content[5].(core.RawBlock)
	if !ok {
		t.Fatalf("unmodelled block came back as %T, want core.RawBlock", got.Content[5])
	}
	if !json.Valid(raw.Raw) || !strings.Contains(string(raw.Raw), `"frames":3`) {
		t.Errorf("unmodelled block lost its payload: %s", raw.Raw)
	}
}

func TestToolUseInputIsCompareByByteEqualityNotByDecodedValue(t *testing.T) {
	// NFR-TEST-03: for ToolUseBlock the assertion must be byte equality on
	// Input, because DeepEqual passes on a map that lost its key order. The
	// fixture has no key in sorted position at three nesting depths — top
	// level, inside a nested object, and inside an object in an array — so
	// sorting anywhere shows up as different bytes.
	const args = `{"zeta":1,"alpha":{"yankee":true,"bravo":[{"zulu":"z","charlie":0}],` +
		`"delta":2},"nums":{"big":9007199254740993,"exp":1e3,"trailing":1.10}}`

	in := core.AssistantMessage{
		Content:    core.Content{mustToolUse(t, "tu_1", "search", args)},
		StopReason: core.StopReasonToolUse,
		Timestamp:  fixedTime,
	}
	got := roundTrip(t, in).(core.AssistantMessage)
	block, ok := got.Content[0].(core.ToolUseBlock)
	if !ok {
		t.Fatalf("block is %T", got.Content[0])
	}
	if !bytes.Equal(block.Input, []byte(args)) {
		t.Errorf("tool_use input bytes\n got %s\nwant %s", block.Input, args)
	}
	if block.InputOrder.Len() != 3 {
		t.Errorf("InputOrder has %d members, want 3", block.InputOrder.Len())
	}
	if k := block.InputOrder[0].Key; k != "zeta" {
		t.Errorf("InputOrder lost its order: first key is %q, want zeta", k)
	}
	// The decoded map is the negative control: it cannot see any of this.
	var m1, m2 map[string]any
	if err := json.Unmarshal([]byte(args), &m1); err != nil {
		t.Fatal(err)
	}
	sorted, _ := json.Marshal(m1)
	if err := json.Unmarshal(sorted, &m2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m1, m2) {
		t.Fatal("the negative control is broken")
	}
	if bytes.Equal(sorted, []byte(args)) {
		t.Fatal("the fixture does not discriminate: a map round-trip reproduced the bytes")
	}
}

func TestProvenanceAndOpaqueSignaturesSurviveTheLog(t *testing.T) {
	// NFR-TEST-03(b). Every one of these is read by REQ-PROV-11 on replay;
	// losing any of them downgrades or corrupts a replayed turn.
	in := core.AssistantMessage{
		Content: core.Content{
			core.ThinkingBlock{Thinking: "step", Signature: "sig-xyz"},
			core.ThinkingBlock{Thinking: "", Redacted: true},
			core.ToolUseBlock{ID: "tu", Name: "n", Input: json.RawMessage(`{}`),
				ThoughtSignature: "thought-sig"},
		},
		StopReason:    core.StopReasonToolUse,
		RawStopReason: "tool_use_provider_spelling",
		ErrorMessage:  "",
		Provider:      "anthropic",
		API:           core.APIAnthropicMessages,
		Model:         "claude-opus-4-5",
		ResponseModel: "claude-opus-4-5-20251101",
		ResponseID:    "resp_1",
		ThinkingLevel: core.ThinkingHigh,
		Timestamp:     fixedTime,
	}
	got := roundTrip(t, in).(core.AssistantMessage)

	if got.Provider != in.Provider || got.API != in.API || got.Model != in.Model {
		t.Errorf("provenance triple lost: %q %q %q", got.Provider, got.API, got.Model)
	}
	if got.ResponseModel != in.ResponseModel || got.ResponseID != in.ResponseID {
		t.Errorf("response provenance lost: %q %q", got.ResponseModel, got.ResponseID)
	}
	if got.RawStopReason != in.RawStopReason || got.StopReason != in.StopReason {
		t.Errorf("stop reasons lost: %q %q", got.StopReason, got.RawStopReason)
	}
	if got.ThinkingLevel != in.ThinkingLevel {
		t.Errorf("thinking level lost: %q", got.ThinkingLevel)
	}
	if s := got.Content[0].(core.ThinkingBlock).Signature; s != "sig-xyz" {
		t.Errorf("thinking signature = %q", s)
	}
	if !got.Content[1].(core.ThinkingBlock).Redacted {
		t.Error("thinking.redacted lost")
	}
	if s := got.Content[2].(core.ToolUseBlock).ThoughtSignature; s != "thought-sig" {
		t.Errorf("thought_signature = %q", s)
	}
	if !got.Timestamp.Equal(fixedTime) {
		t.Errorf("timestamp = %v", got.Timestamp)
	}
}

func TestUsagePresenceSurvivesTheLog(t *testing.T) {
	// REQ-PROV-16.4 as applied to the session log (P-11): an explicit 0 and an
	// absent field are different, and a codec that emits every field or drops
	// zero fields destroys the distinction on the way to disk.
	var u core.Usage
	u.SetField(core.UsageInputTokens, 0) // explicitly reported zero
	u.SetField(core.UsageOutputTokens, 42)
	u.SetCost(0.0125)
	u.BilledModel = "claude-opus-4-5"

	in := core.AssistantMessage{Content: core.Content{core.TextBlock{Text: "x"}}, Usage: u, Timestamp: fixedTime}
	got := roundTrip(t, in).(core.AssistantMessage)

	if !got.Usage.Has(core.UsageInputTokens) || got.Usage.InputTokens != 0 {
		t.Error("an explicitly reported zero came back as absent")
	}
	if got.Usage.Has(core.UsageCacheReadTokens) {
		t.Error("an absent field came back as reported")
	}
	if got.Usage.OutputTokens != 42 || got.Usage.CostUSD != 0.0125 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if got.Usage.BilledModel != "claude-opus-4-5" {
		t.Errorf("billed model = %q", got.Usage.BilledModel)
	}

	// And an entirely unreported usage stays unreported: REQ-GO-15 forbids
	// anchoring on an all-zero response, which it can only detect this way.
	plain := roundTrip(t, core.AssistantMessage{
		Content: core.Content{core.TextBlock{Text: "x"}}, Timestamp: fixedTime,
	}).(core.AssistantMessage)
	if plain.Usage.Reported() {
		t.Errorf("unreported usage came back reported: %+v", plain.Usage)
	}
}

func TestToolResultMessageKeepsItsFirstClassFields(t *testing.T) {
	usage := core.Usage{}
	usage.SetField(core.UsageOutputTokens, 7)
	in := core.ToolResultMessage{
		ToolUseID:      "tu_1",
		ToolName:       "read_file",
		Content:        core.Content{core.TextBlock{Text: "file body"}},
		IsError:        true,
		AddedToolNames: []string{"grep", "edit_file"},
		Usage:          &usage,
		Timestamp:      fixedTime,
	}
	got, ok := roundTrip(t, in).(core.ToolResultMessage)
	if !ok {
		t.Fatalf("round-tripped to %T; tool_result is a first-class role", got)
	}
	if got.ToolUseID != in.ToolUseID || got.ToolName != in.ToolName || !got.IsError {
		t.Errorf("got %+v", got)
	}
	if !reflect.DeepEqual(got.AddedToolNames, in.AddedToolNames) {
		t.Errorf("added_tool_names = %v", got.AddedToolNames)
	}
	if got.Usage == nil || got.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

func TestUnmodelledMessageKeysAreRetained(t *testing.T) {
	// The same argument REQ-SESS-05.2 makes for entry types, one level down.
	line := `{"id":"e1","parent_id":"","type":"message","timestamp":"2024-03-01T12:00:00Z",` +
		`"message":{"role":"user","content":[{"type":"text","text":"hi"}],"future_field":{"a":1}}}`
	path := writeLog(t, hdrLine, line)

	e := mustLoad(t, path).Entries()[0]
	um := e.Message.Message.(core.UserMessage)
	if um.Unknown.Len() != 1 || um.Unknown[0].Key != "future_field" {
		t.Fatalf("unknown message keys = %v", um.Unknown)
	}
	// Rebuilt from the modelled fields (no Raw), the unknown key comes back.
	e.Raw = nil
	out, err := EncodeEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"future_field":{"a":1}`) {
		t.Errorf("re-encoded entry dropped the unknown key: %s", out)
	}
}

func TestEncodeEntryRefusesAnUnknownTypeWithNothingToWrite(t *testing.T) {
	_, err := EncodeEntry(core.Entry{ID: "x", Type: "from_the_future"})
	if err == nil {
		t.Fatal("an unmodelled entry with no Raw carries nothing; writing a payload-less line is worse than refusing")
	}
}

func TestTimestampsRoundTripAsRFC3339(t *testing.T) {
	ts := time.Date(2024, 3, 1, 12, 0, 0, 500_000_000, time.UTC)
	m := core.UserMessage{Content: core.Content{core.TextBlock{Text: "x"}}, Timestamp: ts}
	got := roundTrip(t, m).(core.UserMessage)
	if !got.Timestamp.Equal(ts) {
		t.Fatalf("timestamp = %v, want %v", got.Timestamp, ts)
	}
}

func mustToolUse(t *testing.T, id, name, args string) core.ToolUseBlock {
	t.Helper()
	b, err := core.NewToolUse(id, name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("NewToolUse: %v", err)
	}
	return b
}
