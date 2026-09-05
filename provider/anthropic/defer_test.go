package anthropic_test

import (
	"encoding/json"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/schema"
)

func toolWire(name string) core.ToolWire {
	return core.ToolWire{Name: name, Description: name,
		InputSchema: schema.Object(schema.Opt("x", schema.String("x")))}
}

type wireTool struct {
	Name         string          `json:"name"`
	DeferLoading *bool           `json:"defer_loading"`
	CacheControl json.RawMessage `json:"cache_control"`
}

func encodeTools(t *testing.T, m *core.Model, req core.Request, r core.CacheRetention) []wireTool {
	t.Helper()
	body, _, err := anthropic.BuildRequest(m, req, r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Tools []wireTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out.Tools
}

// TestADeferredToolIsDeclaredAfterThePrefixAndCarriesNoBreakpoint is
// REQ-CACHE-10's Anthropic arm.
//
// Two things have to be true together and each is easy to get alone: the
// deferred tool must come AFTER every immediate one, so the cached prefix is
// byte-identical to the previous turn's, and it must carry no cache_control,
// because a breakpoint there sits past the very content the deferral exists to
// keep cached.
func TestADeferredToolIsDeclaredAfterThePrefixAndCarriesNoBreakpoint(t *testing.T) {
	m := testModel()
	req := core.Request{
		// Declared in an order that puts the NEW tool first, so a
		// implementation that preserves caller order fails visibly.
		Tools: []core.ToolWire{toolWire("mcp__db__query"), toolWire("read_file")},
		Messages: core.Messages{
			core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
			core.AssistantMessage{Content: core.Content{mustToolUse(t, "c1", "read_file")},
				Provider: "anthropic", API: anthropic.API, Model: "claude-test",
				StopReason: core.StopReasonToolUse},
			core.ToolResultMessage{ToolUseID: "c1", ToolName: "read_file",
				Content:        core.Content{core.TextBlock{Text: "ok"}},
				AddedToolNames: []string{"mcp__db__query"}},
		},
	}

	tools := encodeTools(t, m, req, core.CacheRetentionShort)
	if len(tools) != 2 {
		t.Fatalf("%d tools on the wire, want 2", len(tools))
	}
	if tools[0].Name != "read_file" {
		t.Fatalf("tools[0] = %q, want read_file first: appending late arrivals is what "+
			"keeps the cached prefix byte-identical", tools[0].Name)
	}
	if tools[0].DeferLoading != nil {
		t.Fatal("an established tool must not be marked deferred")
	}
	if len(tools[0].CacheControl) == 0 {
		t.Fatal("the breakpoint belongs on the last IMMEDIATE tool")
	}
	if tools[1].DeferLoading == nil || !*tools[1].DeferLoading {
		t.Fatalf("tools[1] = %+v, want defer_loading: true", tools[1])
	}
	if len(tools[1].CacheControl) != 0 {
		t.Fatal("a deferred tool carries NO cache_control: a breakpoint there sits past " +
			"the prefix the deferral exists to preserve")
	}
}

// TestWithNothingDeferredTheBreakpointIsOnTheLastTool keeps the ordinary case
// unchanged — the deferral machinery must be invisible when nothing defers.
func TestWithNothingDeferredTheBreakpointIsOnTheLastTool(t *testing.T) {
	tools := encodeTools(t, testModel(), core.Request{
		Tools: []core.ToolWire{toolWire("a"), toolWire("b")},
	}, core.CacheRetentionShort)

	if len(tools) != 2 || tools[0].Name != "a" || tools[1].Name != "b" {
		t.Fatalf("tools = %+v, want caller order preserved", tools)
	}
	if len(tools[0].CacheControl) != 0 {
		t.Fatal("only the LAST tool carries a breakpoint; the marker covers everything " +
			"before it, and the four-breakpoint budget must not be spent on the tool list")
	}
	if len(tools[1].CacheControl) == 0 {
		t.Fatal("the last tool carries the breakpoint")
	}
	for _, tl := range tools {
		if tl.DeferLoading != nil {
			t.Fatalf("%s is marked deferred with nothing to defer", tl.Name)
		}
	}
}

func mustToolUse(t *testing.T, id, name string) core.ToolUseBlock {
	t.Helper()
	b, err := core.NewToolUse(id, name, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
