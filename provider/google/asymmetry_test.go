package google_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/openai"
)

// TestGeminiIsTheThirdShapeForOneTranscript is the empirical argument that
// REQ-LOOP-02's coalescing rule cannot live in the canonical layer.
//
// provider/asymmetry_test.go shows two shapes. Two could be a coincidence —
// one wire being "normal" and the other "the exception". Three is a
// specification: ONE canonical transcript with three parallel tool calls
// becomes
//
//	Anthropic  ONE user MESSAGE holding three tool_result BLOCKS
//	OpenAI     THREE separate role:"tool" MESSAGES, keyed by tool_call_id
//	Gemini     ONE user CONTENT holding three functionResponse PARTS, no ids
//
// and no two of those are the same operation. A canonical layer that had baked
// in any one of them could not produce the other two.
func TestGeminiIsTheThirdShapeForOneTranscript(t *testing.T) {
	r := request(t)

	amodel := &core.Model{ID: "claude-x", API: anthropic.API, Provider: "anthropic", MaxTokens: 4096}
	omodel := &core.Model{ID: "gpt-x", API: openai.API, Provider: "openai", MaxTokens: 4096}

	abody, _, err := anthropic.BuildRequest(amodel, r, core.CacheRetentionNone)
	if err != nil {
		t.Fatal(err)
	}
	obody, _, err := openai.BuildRequest(omodel, r)
	if err != nil {
		t.Fatal(err)
	}
	aj, _ := json.Marshal(abody)
	oj, _ := json.Marshal(obody)
	gj, gw := build(t, model(), r)

	// Anthropic: results are BLOCKS of one user message.
	if !strings.Contains(string(aj), `"type":"tool_result"`) {
		t.Fatalf("anthropic body carries no tool_result blocks: %s", aj)
	}
	// OpenAI: results are their own messages, keyed by id.
	var odec struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(oj, &odec); err != nil {
		t.Fatal(err)
	}
	var openaiToolMsgs int
	for _, m := range odec.Messages {
		if m.Role == "tool" {
			openaiToolMsgs++
		}
	}
	if openaiToolMsgs != 3 {
		t.Fatalf("openai emitted %d tool messages, want 3", openaiToolMsgs)
	}

	// Gemini: results are PARTS of one user content, with no id anywhere.
	var geminiCarriers, geminiParts int
	for _, c := range gw.Contents {
		n := 0
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				n++
			}
		}
		if n > 0 {
			geminiCarriers, geminiParts = geminiCarriers+1, n
		}
	}
	if geminiCarriers != 1 || geminiParts != 3 {
		t.Fatalf("gemini: %d carriers holding %d functionResponse parts, want 1 holding 3",
			geminiCarriers, geminiParts)
	}
	for _, id := range []string{"call_a", "call_b", "call_c"} {
		if strings.Contains(string(gj), id) {
			t.Fatalf("the canonical tool-use id %q reached the Gemini wire; this shape carries "+
				"no ids and pairs results to calls by name and position", id)
		}
	}

	// The three vocabularies do not overlap: each body names its results with
	// a key the other two reject.
	for _, c := range []struct{ name, body, key string }{
		{"anthropic", string(aj), "tool_result"},
		{"openai", string(oj), "tool_call_id"},
		{"gemini", string(gj), "functionResponse"},
	} {
		for _, other := range []struct{ name, body string }{
			{"anthropic", string(aj)}, {"openai", string(oj)}, {"gemini", string(gj)},
		} {
			if other.name == c.name {
				continue
			}
			if strings.Contains(other.body, c.key) {
				t.Errorf("%s's result key %q also appears in the %s body; the asymmetry this "+
					"test exists to demonstrate is absent", c.name, c.key, other.name)
			}
		}
	}
	t.Logf("same transcript: anthropic %d messages, openai %d messages, gemini %d contents",
		strings.Count(string(aj), `"role":`), len(odec.Messages), len(gw.Contents))
}
