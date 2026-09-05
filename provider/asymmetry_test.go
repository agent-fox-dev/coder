package provider_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/schema"
)

// oneTranscript is a single canonical transcript: a user turn, one assistant
// turn emitting THREE parallel tool calls, and three tool results.
//
// The canonical form carries one ToolResultMessage per call (REQ-LOOP-02).
// Both providers below are handed exactly this, and must produce structurally
// incompatible wire bodies from it.
func oneTranscript(t *testing.T) core.Messages {
	t.Helper()
	mk := func(id, name, args string) core.ToolUseBlock {
		b, err := core.NewToolUse(id, name, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "check three files"}}},
		core.AssistantMessage{
			Content: core.Content{
				core.TextBlock{Text: "Reading them now."},
				mk("call_a", "read_file", `{"path":"a.go"}`),
				mk("call_b", "read_file", `{"path":"b.go"}`),
				mk("call_c", "read_file", `{"path":"c.go"}`),
			},
			StopReason: core.StopReasonToolUse,
			Provider:   "anthropic", API: anthropic.API, Model: "claude-x",
		},
		core.ToolResultMessage{ToolUseID: "call_a", ToolName: "read_file",
			Content: core.Content{core.TextBlock{Text: "contents of a"}}},
		core.ToolResultMessage{ToolUseID: "call_b", ToolName: "read_file",
			Content: core.Content{core.TextBlock{Text: "contents of b"}}},
		core.ToolResultMessage{ToolUseID: "call_c", ToolName: "read_file",
			Content: core.Content{core.TextBlock{Text: "contents of c"}}},
	}
}

func req(t *testing.T) core.Request {
	t.Helper()
	return core.Request{
		System:   []core.ContentBlock{core.TextBlock{Text: "You are a code reader."}},
		Messages: oneTranscript(t),
		Tools: []core.ToolWire{{
			Name: "read_file", Description: "Read a file",
			InputSchema: schema.Object(schema.Prop("path", schema.String("path"))),
		}},
	}
}

// TestToolResultShapeAsymmetry is the empirical demonstration that the PRD's
// original REQ-LOOP-02 was wrong.
//
// The PRD called it a LOOP invariant that "all tool_result blocks from a
// single model turn must be collected into a single user message", and named
// splitting them "the most common implementation mistake in hand-rolled agent
// loops". Half of that is right — for Anthropic. For OpenAI the single-message
// form is NOT REPRESENTABLE: the wire mandates one {"role":"tool"} message per
// result, keyed by tool_call_id.
//
// One canonical transcript. Three tool calls. Anthropic gets ONE user message
// holding three tool_result blocks; OpenAI gets THREE separate tool messages.
// A canonical layer that had baked in the Anthropic shape could not produce
// the OpenAI body at all.
func TestToolResultShapeAsymmetry(t *testing.T) {
	amodel := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 4096}
	omodel := &core.Model{ID: "gpt-x", API: openai.API, Provider: "openai", MaxTokens: 4096}

	abody, _, err := anthropic.BuildRequest(amodel, req(t), core.CacheRetentionShort)
	if err != nil {
		t.Fatal(err)
	}
	obody, _, err := openai.BuildRequest(omodel, req(t))
	if err != nil {
		t.Fatal(err)
	}

	aj, _ := json.Marshal(abody)
	oj, _ := json.Marshal(obody)

	// ---- Anthropic: ONE user message carrying all three tool_result blocks.
	var adec struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(aj, &adec); err != nil {
		t.Fatal(err)
	}
	var resultCarriers int
	var blocksInCarrier int
	for _, m := range adec.Messages {
		n := 0
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				n++
			}
		}
		if n > 0 {
			resultCarriers++
			blocksInCarrier = n
			if m.Role != "user" {
				t.Errorf("Anthropic tool results must ride on a user message, got role %q", m.Role)
			}
		}
	}
	if resultCarriers != 1 {
		t.Errorf("Anthropic: %d messages carry tool_result blocks, want exactly 1.\n"+
			"Splitting them across messages silently degrades parallel tool call quality.",
			resultCarriers)
	}
	if blocksInCarrier != 3 {
		t.Errorf("Anthropic: the carrier holds %d tool_result blocks, want 3", blocksInCarrier)
	}

	// ---- OpenAI: THREE separate role:"tool" messages.
	var odec struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(oj, &odec); err != nil {
		t.Fatal(err)
	}
	var toolMsgs []string
	for _, m := range odec.Messages {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m.ToolCallID)
		}
	}
	if len(toolMsgs) != 3 {
		t.Fatalf("OpenAI: %d role:\"tool\" messages, want 3 — one per result.\n"+
			"Grouping is not representable on this wire, which is why the canonical "+
			"transcript keeps one ToolResultMessage per call (REQ-LOOP-02).", len(toolMsgs))
	}
	if strings.Join(toolMsgs, ",") != "call_a,call_b,call_c" {
		t.Errorf("OpenAI tool_call_id order = %v, want call_a,call_b,call_c", toolMsgs)
	}

	// ---- And the point, stated as an assertion: the two bodies disagree
	// about how many messages the SAME transcript becomes.
	if len(adec.Messages) == len(odec.Messages) {
		t.Errorf("both providers produced %d messages; the asymmetry this test exists "+
			"to demonstrate is absent", len(adec.Messages))
	}
	t.Logf("same transcript: Anthropic %d messages, OpenAI %d messages",
		len(adec.Messages), len(odec.Messages))
}

// TestToolArgumentBytesSurviveBothWires pins REQ-PROV-17 / REQ-TOOL-12: the
// model's own argument bytes reach the wire unchanged, in the model's own key
// order, on both providers.
//
// A decode-and-re-encode round trip would sort the keys. On OpenAI, where
// arguments are a JSON STRING, that changes the literal text the model is
// conditioned on next turn and shifts the prompt-cache prefix — a silent cache
// miss on every subsequent turn, visible only in the bill.
func TestToolArgumentBytesSurviveBothWires(t *testing.T) {
	const args = `{"zeta":1,"alpha":{"yankee":true,"bravo":[{"zulu":"z","charlie":0}]},"mid":"x"}`
	b, err := core.NewToolUse("call_1", "t", json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	msgs := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{b}, StopReason: core.StopReasonToolUse},
		core.ToolResultMessage{ToolUseID: "call_1", ToolName: "t",
			Content: core.Content{core.TextBlock{Text: "ok"}}},
	}
	r := core.Request{Messages: msgs}

	t.Run("anthropic", func(t *testing.T) {
		m := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 100}
		body, _, err := anthropic.BuildRequest(m, r, core.CacheRetentionNone)
		if err != nil {
			t.Fatal(err)
		}
		j, _ := json.Marshal(body)
		if !strings.Contains(string(j), args) {
			t.Fatalf("argument bytes were reordered on the Anthropic wire.\nwant substring: %s\ngot: %s", args, j)
		}
	})

	t.Run("openai", func(t *testing.T) {
		m := &core.Model{ID: "gpt-x", API: openai.API, Provider: "openai", MaxTokens: 100}
		body, _, err := openai.BuildRequest(m, r)
		if err != nil {
			t.Fatal(err)
		}
		j, _ := json.Marshal(body)
		// Arguments ride as a JSON string, so the bytes appear escaped.
		esc, _ := json.Marshal(args)
		if !strings.Contains(string(j), string(esc[1:len(esc)-1])) {
			t.Fatalf("argument bytes were reordered on the OpenAI wire.\nwant escaped: %s\ngot: %s", esc, j)
		}
	})
}

// TestFinishStopWithToolCallsIsReportedAsToolUse pins ruling P-41 and the
// reporting half of REQ-LOOP-01: a gateway that says "stop" while emitting
// tool calls, or says nothing at all, must never be reported as an error.
func TestFinishStopWithToolCallsIsReportedAsToolUse(t *testing.T) {
	cases := []struct {
		finish       string
		hasToolCalls bool
		want         core.StopReason
	}{
		{"tool_calls", true, core.StopReasonToolUse},
		{"stop", true, core.StopReasonToolUse}, // the gateway case
		{"", true, core.StopReasonToolUse},     // never emits finish_reason
		{"", false, core.StopReasonStop},       // ditto, no calls
		{"stop", false, core.StopReasonStop},
		{"length", false, core.StopReasonLength},
		{"content_filter", false, core.StopReasonRefusal},
		{"something_new", false, core.StopReasonStop},
	}
	for _, c := range cases {
		if got := openai.MapFinishReason(c.finish, c.hasToolCalls); got != c.want {
			t.Errorf("MapFinishReason(%q, %v) = %q, want %q", c.finish, c.hasToolCalls, got, c.want)
		}
		if got := openai.MapFinishReason(c.finish, c.hasToolCalls); got == core.StopReasonError {
			t.Errorf("MapFinishReason(%q, %v) reported an error; an unknown or absent "+
				"finish reason must never be one (ruling P-41)", c.finish, c.hasToolCalls)
		}
	}
}

// TestCacheControlRollsForwardEveryRequest pins §6.2a Level 1's rolling
// breakpoint. The marker on the last user message must MOVE as the transcript
// grows — a static prefix-only breakpoint re-pays full input price on the
// whole growing transcript, which is the dominant cost in a multi-turn agent.
func TestCacheControlRollsForwardEveryRequest(t *testing.T) {
	m := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 100}

	markerIndex := func(msgs core.Messages) int {
		body, _, err := anthropic.BuildRequest(m, core.Request{Messages: msgs}, core.CacheRetentionShort)
		if err != nil {
			t.Fatal(err)
		}
		j, _ := json.Marshal(body)
		var dec struct {
			Messages []struct {
				Content []struct {
					CacheControl *struct{} `json:"cache_control"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(j, &dec); err != nil {
			t.Fatal(err)
		}
		for i, mm := range dec.Messages {
			for _, b := range mm.Content {
				if b.CacheControl != nil {
					return i
				}
			}
		}
		return -1
	}

	short := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "one"}}},
	}
	long := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "one"}}},
		core.AssistantMessage{Content: core.Content{core.TextBlock{Text: "reply"}},
			StopReason: core.StopReasonStop},
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "two"}}},
	}

	i1, i2 := markerIndex(short), markerIndex(long)
	if i1 < 0 || i2 < 0 {
		t.Fatalf("no cache_control marker was stamped (%d, %d)", i1, i2)
	}
	if i2 <= i1 {
		t.Fatalf("the breakpoint did not roll forward: index %d then %d.\n"+
			"A static prefix-only breakpoint re-pays full input price on the entire "+
			"growing transcript (§6.2a Level 1).", i1, i2)
	}
}

// TestCacheRetentionNoneStampsNothing is the negative control for the test
// above: without it, an implementation that stamps unconditionally would pass.
func TestCacheRetentionNoneStampsNothing(t *testing.T) {
	m := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 100}
	body, _, err := anthropic.BuildRequest(m, req(t), core.CacheRetentionNone)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(body)
	if strings.Contains(string(j), "cache_control") {
		t.Fatal("cache_control was stamped despite CacheNone")
	}
}

// TestAdjacentUserMessagesAreCoalesced pins ruling P-5, a failure that belongs
// to nobody in the PRD.
//
// Compaction prepends a summary UserMessage to a suffix that, when the cut
// lands on a user message — the case REQ-GO-14's turn-splitting actively tries
// to produce — begins with another UserMessage. Anthropic rejects two adjacent
// user turns, so the first compacted request 400s. REQ-LOOP-02 mandates
// coalescing consecutive tool results and says nothing about this.
func TestAdjacentUserMessagesAreCoalesced(t *testing.T) {
	m := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 100}
	msgs := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "[summary of earlier turns]"}}},
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "the actual next request"}}},
	}
	body, _, err := anthropic.BuildRequest(m, core.Request{Messages: msgs}, core.CacheRetentionNone)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(body)
	var dec struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(j, &dec); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(dec.Messages); i++ {
		if dec.Messages[i].Role == dec.Messages[i-1].Role {
			t.Fatalf("two adjacent %q messages reached the wire; Anthropic rejects that, "+
				"and compaction produces it on the first compacted request (ruling P-5)",
				dec.Messages[i].Role)
		}
	}
}

// TestRepairRunsInsideTheProviderNotTheLoop: a transcript loaded from disk,
// ending on an unanswered tool_use, must be made sendable by the PROVIDER —
// the loop is not running when a session is loaded (REQ-PROV-11).
func TestRepairRunsInsideTheProviderNotTheLoop(t *testing.T) {
	b, err := core.NewToolUse("call_x", "read_file", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// Exactly what a killed process leaves behind.
	msgs := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{b}, StopReason: core.StopReasonToolUse,
			Provider: "anthropic", API: anthropic.API, Model: "claude-x"},
	}
	m := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 100}
	body, rep, err := anthropic.BuildRequest(m, core.Request{Messages: msgs}, core.CacheRetentionNone)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SyntheticResults != 1 {
		t.Fatalf("SyntheticResults = %d, want 1: an unanswered tool_use must be answered "+
			"at send time or the request 400s", rep.SyntheticResults)
	}
	j, _ := json.Marshal(body)
	if !strings.Contains(string(j), "tool_result") {
		t.Fatal("no synthetic tool_result reached the wire")
	}
}
