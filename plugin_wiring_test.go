package agentkit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/plugins"
)

type votingHook struct {
	plugins.BaseEventHook
	name    string
	verdict core.PluginDecision
	seen    *[]string
	panics  bool
}

func (h *votingHook) PluginName() string { return h.name }
func (h *votingHook) OnToolUse(_ context.Context, tool string, _ json.RawMessage) core.PluginDecision {
	if h.seen != nil {
		*h.seen = append(*h.seen, h.name+":"+tool)
	}
	if h.panics {
		panic("a badly written plugin")
	}
	return h.verdict
}

func registryWith(hs ...core.EventHookPlugin) *plugins.Registry {
	r := plugins.NewRegistry()
	for _, h := range hs {
		r.Register(h)
	}
	return r
}

func oneToolTurn(t *testing.T) *scripted {
	t.Helper()
	return &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", "echo", `{"v":"x"}`)),
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}
}

// TestAPluginHookCanBlockAToolCall is REQ-PLUGIN-04 through the real batch
// executor.
func TestAPluginHookCanBlockAToolCall(t *testing.T) {
	var ran int
	s := oneToolTurn(t)
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Plugins = registryWith(&votingHook{name: "guard", verdict: core.PluginBlock})
	})
	if err := a.RegisterTool(countingTool("echo", &ran)); err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if ran != 0 {
		t.Fatal("the handler ran despite a plugin block")
	}

	msg := findToolResult(t, res.Messages, "c1")
	if !msg.IsError {
		t.Fatal("a blocked call must come back as an error result, so the model can " +
			"react rather than wait for a reply that never comes")
	}
	if !strings.Contains(msg.Content.Text(), "guard") {
		t.Fatalf("result = %q; the refusal must name the plugin that made it",
			msg.Content.Text())
	}
}

// TestAPluginHookCannotOverturnTheInterceptor is REQ-PLUGIN-04 as amended by
// REQ-SEC-03.5.
//
// The embedder's BeforeToolCall is the authorization boundary and may both
// widen and narrow. Hooks compose with it and may only narrow FURTHER. A hook
// returning "allow" over an interceptor's Block would make a third-party
// plugin able to overrule the host's own policy — which is the opposite of
// what a plugin system is for.
func TestAPluginHookCannotOverturnTheInterceptor(t *testing.T) {
	var ran int
	var seen []string
	s := oneToolTurn(t)
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Plugins = registryWith(&votingHook{name: "eager", verdict: core.PluginAllow, seen: &seen})
		c.BeforeToolCall = func(context.Context, core.BeforeToolCallContext) core.BeforeToolCallDecision {
			return core.BeforeToolCallDecision{Block: true, Reason: "host policy"}
		}
	})
	if err := a.RegisterTool(countingTool("echo", &ran)); err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if ran != 0 {
		t.Fatal("a plugin's \"allow\" overturned the host's interceptor")
	}
	if len(seen) != 0 {
		t.Fatalf("hooks ran %v; a call the interceptor already refused must not reach a "+
			"plugin at all — otherwise the plugin's author will come to believe its "+
			"vote matters there", seen)
	}
	if msg := findToolResult(t, res.Messages, "c1"); !strings.Contains(msg.Content.Text(), "host policy") {
		t.Fatalf("result = %q, want the interceptor's own reason", msg.Content.Text())
	}
}

// TestHooksRunAfterTheInterceptorAndSeeTheCoercedArguments: the interceptor may
// widen or rewrite arguments (REQ-SEC-03.5), and a hook inspecting the
// pre-rewrite bytes would be gating something other than what runs.
func TestHooksRunAfterTheInterceptorAndSeeTheCoercedArguments(t *testing.T) {
	var got json.RawMessage
	s := oneToolTurn(t)
	inspect := &inspectingHook{name: "inspect", capture: &got}
	var ran int
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Plugins = registryWith(inspect)
		c.BeforeToolCall = func(context.Context, core.BeforeToolCallContext) core.BeforeToolCallDecision {
			return core.BeforeToolCallDecision{Arguments: map[string]any{"v": "rewritten"}}
		}
	})
	if err := a.RegisterTool(countingTool("echo", &ran)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "rewritten") {
		t.Fatalf("the hook saw %s; it must see what will actually run, not the model's "+
			"original bytes", got)
	}
}

type inspectingHook struct {
	plugins.BaseEventHook
	name    string
	capture *json.RawMessage
}

func (h *inspectingHook) PluginName() string { return h.name }
func (h *inspectingHook) OnToolUse(_ context.Context, _ string, in json.RawMessage) core.PluginDecision {
	*h.capture = append(json.RawMessage(nil), in...)
	return core.PluginNoOpinion
}

// TestAPanickingPluginDoesNotTakeTheRunWithIt: a plugin is third-party code in
// this process, and letting it panic the loop would make every plugin a
// liveness risk for the host.
func TestAPanickingPluginDoesNotTakeTheRunWithIt(t *testing.T) {
	var ran int
	s := oneToolTurn(t)
	a := newTestAgent(t, s, func(c *core.AgentConfig) {
		c.Plugins = registryWith(&votingHook{name: "crashy", panics: true})
	})
	if err := a.RegisterTool(countingTool("echo", &ran)); err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("a panicking plugin must not fail the run: %v", err)
	}
	if ran != 1 {
		t.Fatalf("the tool ran %d times; a hook that panicked cast no vote, so the call "+
			"proceeds", ran)
	}
	if res.FinalText() != "done" {
		t.Fatalf("final text = %q", res.FinalText())
	}
}

func TestNoRegistryMeansNoGate(t *testing.T) {
	var ran int
	s := oneToolTurn(t)
	a := newTestAgent(t, s, nil)
	if err := a.RegisterTool(countingTool("echo", &ran)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Fatalf("the tool ran %d times with no plugins configured, want 1", ran)
	}
}

func countingTool(name string, n *int) core.Tool {
	return core.Tool{
		Name: name, Description: name, InputSchema: emptySchema(),
		Handler: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			*n++
			return json.RawMessage(`{"ok":true}`), nil
		},
	}
}

func findToolResult(t *testing.T, msgs core.Messages, id string) core.ToolResultMessage {
	t.Helper()
	for _, m := range msgs {
		if r, ok := m.(core.ToolResultMessage); ok && r.ToolUseID == id {
			return r
		}
	}
	t.Fatalf("no tool result for %q in %d messages", id, len(msgs))
	return core.ToolResultMessage{}
}
