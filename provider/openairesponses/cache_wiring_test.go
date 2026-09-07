package openairesponses_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/openairesponses"
	"github.com/agentfox/agentkit-go/schema"
)

// These pin §6.2a Level 3 on this wire: REQ-CACHE-06/NFR-PERF-03's schema
// cache and REQ-CACHE-10's Responses arm.

func prefixTools(n int) []core.ToolWire {
	out := make([]core.ToolWire, n)
	for i := 0; i < n; i++ {
		name := "tool_" + string(rune('a'+i))
		out[i] = core.ToolWire{Name: name, Description: name,
			InputSchema: schema.Object(schema.Prop("q", schema.String("q")))}
	}
	return out
}

// TestASteadyStateResponsesRequestSerializesNoSchemas is NFR-PERF-03 as a
// COUNT, the same shape the Anthropic adapter is pinned by: a cache that is
// implemented, unit-tested and attached to no provider costs memory and saves
// nothing.
func TestASteadyStateResponsesRequestSerializesNoSchemas(t *testing.T) {
	m := model()
	req := core.Request{Tools: prefixTools(16)}
	prefix := &provider.ToolPrefix{}

	_, _, first, err := openairesponses.BuildRequestCached(m, req, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if first.Marshalled != 16 {
		t.Fatalf("first build serialized %d schemas, want all 16", first.Marshalled)
	}
	out, _, second, err := openairesponses.BuildRequestCached(m, req, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if second.Marshalled != 0 {
		t.Fatalf("a steady-state build serialized %d schemas, want 0", second.Marshalled)
	}
	if second.Invalidated {
		t.Fatalf("an unchanged tool list invalidated the prefix: %s", second.Reason)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"properties"`) != 16 {
		t.Fatalf("the cached schemas did not all reach the wire: %s", raw)
	}
}

// TestTheResponsesProviderOwnsAPrefixByDefault: the wiring must not require
// the caller to opt in, or the default configuration is the slow one.
func TestTheResponsesProviderOwnsAPrefixByDefault(t *testing.T) {
	var reports []provider.SyncReport
	p := openairesponses.Provider(openairesponses.Options{
		Getenv:           func(string) string { return "k" },
		OnToolPrefixSync: func(r provider.SyncReport) { reports = append(reports, r) },
	})
	req := core.Request{Tools: prefixTools(8)}
	req.Options.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(ev("response.completed", `{"response":{}}`)))}, nil
	})
	for i := 0; i < 2; i++ {
		p.Stream(context.Background(), model(), req, core.ProviderStreamOptions{}).Result()
	}
	if len(reports) != 2 {
		t.Fatalf("%d sync reports, want one per request", len(reports))
	}
	if reports[0].Marshalled != 8 || reports[1].Marshalled != 0 {
		t.Fatalf("marshalled %d then %d, want 8 then 0: a provider constructed with default "+
			"Options must cache without being asked", reports[0].Marshalled, reports[1].Marshalled)
	}
}

// ---------------------------------------------------------------- REQ-CACHE-10

func deferredRequest(t *testing.T) core.Request {
	t.Helper()
	call, err := core.NewToolUse("call_1|item_1", "read_file", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return core.Request{
		// Declared with the NEW tool first, so an implementation that keeps
		// caller order fails visibly.
		Tools: []core.ToolWire{
			{Name: "mcp__db__query", Description: "Query the db.",
				InputSchema: schema.Object(schema.Prop("sql", schema.String("sql")))},
			{Name: "read_file", Description: "Read a file.",
				InputSchema: schema.Object(schema.Prop("path", schema.String("path")))},
		},
		Messages: core.Messages{
			core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
			core.AssistantMessage{Content: core.Content{call}, StopReason: core.StopReasonToolUse,
				Provider: "openai", API: openairesponses.API, Model: "gpt-resp"},
			core.ToolResultMessage{ToolUseID: "call_1|item_1", ToolName: "read_file",
				Content:        core.Content{core.TextBlock{Text: "ok"}},
				AddedToolNames: []string{"mcp__db__query"}},
		},
	}
}

func toolNames(t *testing.T, body map[string]any, key string) []string {
	t.Helper()
	raw, _ := body[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(map[string]any)["name"].(string))
	}
	return out
}

// TestADeferredToolRidesInAdditionalTools is REQ-CACHE-10's Responses arm.
//
// `tools` is the head of the cached prompt prefix, so a tool that appeared
// mid-session must not be prepended to it: declaring it in additional_tools
// leaves the prefix byte-identical to the previous turn's, where prepending
// costs the whole provider-side cache over one added tool.
func TestADeferredToolRidesInAdditionalTools(t *testing.T) {
	body := send(t, openairesponses.Options{}, deferredRequest(t),
		ev("response.completed", `{"response":{}}`))

	if got := toolNames(t, body, "tools"); len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("tools = %v, want only the established tool: a late arrival in the prefix "+
			"invalidates the cache", got)
	}
	if got := toolNames(t, body, "additional_tools"); len(got) != 1 || got[0] != "mcp__db__query" {
		t.Fatalf("additional_tools = %v, want the tool the transcript introduced", got)
	}
}

// TestWhenEveryToolWouldBeDeferredTheyArePromoted is REQ-CACHE-10's safety
// valve: with nothing immediate there is no prefix to anchor references
// against, so the deferral buys nothing and the cache wipe is accepted.
func TestWhenEveryToolWouldBeDeferredTheyArePromoted(t *testing.T) {
	req := deferredRequest(t)
	req.Tools = req.Tools[:1] // the deferred one only
	body := send(t, openairesponses.Options{}, req, ev("response.completed", `{"response":{}}`))

	if got := toolNames(t, body, "tools"); len(got) != 1 || got[0] != "mcp__db__query" {
		t.Fatalf("tools = %v, want the promoted tool", got)
	}
	if _, present := body["additional_tools"]; present {
		t.Fatalf("additional_tools = %v, want nothing deferred once the valve fires",
			body["additional_tools"])
	}
}
