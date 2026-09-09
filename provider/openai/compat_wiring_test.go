package openai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/schema"
)

// These pin the three REQ-PROV-12 flags that were resolved and consumed by
// nothing — ThinkingFormat, ThinkingTokenBudgetField and
// AllowsUserAfterToolResult — plus REQ-CACHE-06/NFR-PERF-03 and REQ-CACHE-10's
// third arm on this wire.

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// drive runs one streamed turn against a canned SSE body and returns the
// assistant message, so a replay test starts from what the DECODER produced
// rather than from a hand-built block.
func drive(t *testing.T, m *core.Model, sse string) *core.AssistantMessage {
	t.Helper()
	req := userReq()
	req.Options.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(sse))}, nil
	})
	msg := openai.Provider(openai.Options{Getenv: func(string) string { return "k" }}).
		Stream(context.Background(), m, req, core.ProviderStreamOptions{}).Result()
	if msg == nil {
		t.Fatal("no message")
	}
	return msg
}

func messagesOf(t *testing.T, got map[string]any) []map[string]any {
	t.Helper()
	raw, ok := got["messages"].([]any)
	if !ok {
		t.Fatalf("no messages in %v", got)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}

const reasoningSSE = "data: " +
	`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"weighing it"}}]}` +
	"\n\n" + "data: " +
	`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}` +
	"\n\ndata: [DONE]\n\n"

// TestOnlyTheDeepSeekProfileReplaysReasoningContent is REQ-PROV-12's
// ThinkingFormat, end to end: decode a reasoning turn, then replay it.
//
// DeepSeek is the one profile that documents the round trip, so it is the one
// profile that emits reasoning_content back. Everywhere else the block is
// DROPPED on a same-model replay: inventing a field the vendor does not read
// is worse than dropping — the chain looks replayed and is not — and folding
// the reasoning into `content` is worse still, because the model is then
// conditioned on its own chain as visible prose, re-sent and re-billed on
// every later turn.
func TestOnlyTheDeepSeekProfileReplaysReasoningContent(t *testing.T) {
	ds := model("deepseek", "deepseek-reasoner", "https://api.deepseek.com")
	msg := drive(t, ds, reasoningSSE)

	req := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}, *msg}}
	assistant := messagesOf(t, body(t, ds, req))[1]
	if assistant["reasoning_content"] != "weighing it" {
		t.Fatalf("assistant = %v, want reasoning_content echoed back on the profile that "+
			"documents the round trip (REQ-PROV-12 ThinkingFormat: deepseek)", assistant)
	}
	if assistant["content"] != "answer" {
		t.Fatalf("assistant content = %v, want the answer text unchanged", assistant["content"])
	}

	// Same transcript, a profile with no documented replay shape.
	oa := model("openai", "gpt-4o", "https://api.openai.com/v1")
	msg = drive(t, oa, reasoningSSE)
	var think *core.ThinkingBlock
	for _, b := range msg.Content {
		if tb, ok := b.(core.ThinkingBlock); ok {
			think = &tb
		}
	}
	if think == nil || think.Signature != openai.ReasoningMarkerSignature {
		t.Fatalf("decoded thinking = %+v, want the block carrying the marker signature that "+
			"keeps REQ-PROV-11 rule 4 from demoting it to text", think)
	}
	req = core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}, *msg}}
	assistant = messagesOf(t, body(t, oa, req))[1]
	if _, present := assistant["reasoning_content"]; present {
		t.Fatalf("assistant = %v: reasoning_content was invented for a profile that does "+
			"not read it", assistant)
	}
	text, _ := assistant["content"].(string)
	if strings.Contains(text, "weighing it") {
		t.Fatalf("assistant content = %q: the model's reasoning was re-sent as its own "+
			"visible prose", text)
	}
	if text != "answer" {
		t.Fatalf("assistant content = %q, want the answer alone", text)
	}

	// Cross-model, rule 3 still applies: another model gets the text.
	other := model("openai", "gpt-4o-mini", "https://api.openai.com/v1")
	assistant = messagesOf(t, body(t, other, req))[1]
	if text, _ := assistant["content"].(string); !strings.Contains(text, "weighing it") {
		t.Fatalf("assistant content = %q on ANOTHER model, want the reasoning downgraded "+
			"to text by REQ-PROV-11 rule 3", text)
	}
}

// TestAStreamCutBeforeItsTerminalSignalIsTruncated is REQ-PROV-04 with the
// case that matters: the connection drops mid tool call. Salvage turns the
// partial arguments into valid JSON, and a stream reported as a normal
// tool_use turn would then EXECUTE that call with whatever survived the cut.
// No [DONE] and no finish_reason on any choice is a stream the model did not
// finish; a gateway that omits [DONE] but sends finish_reason is fine.
func TestAStreamCutBeforeItsTerminalSignalIsTruncated(t *testing.T) {
	oa := model("openai", "gpt-4o", "https://api.openai.com/v1")
	cut := "data: " +
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"Hello"}}]}` + "\n\n" +
		"data: " +
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"delete_file","arguments":"{\"path\":\"/etc/pa"}}]}}]}` + "\n\n"
	msg := drive(t, oa, cut)
	if msg.StopReason != core.StopReasonError {
		t.Fatalf("stop = %q (%q), want error: neither [DONE] nor a finish_reason arrived, and "+
			"reporting tool_use would execute a call cut off mid-argument",
			msg.StopReason, msg.ErrorMessage)
	}
	if !strings.Contains(msg.ErrorMessage, "stream ended before") {
		t.Fatalf("error = %q, want the truncation text the REQ-PROV-14 allowlist retries on",
			msg.ErrorMessage)
	}
	if msg.Content.Text() != "Hello" {
		t.Fatalf("content = %q, want the partial text kept alongside the failure", msg.Content.Text())
	}

	// finish_reason without [DONE] is a complete turn.
	noDone := "data: " +
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":"stop"}]}` + "\n\n"
	msg = drive(t, oa, noDone)
	if msg.StopReason != core.StopReasonStop {
		t.Fatalf("stop = %q (%q), want stop: a gateway that omits [DONE] but sends "+
			"finish_reason delivered a whole turn", msg.StopReason, msg.ErrorMessage)
	}

	// A profile whose finish_reason cannot be trusted has no terminal signal
	// to demand; content stays the evidence there.
	untrusted := model("openai", "gpt-4o", "https://api.openai.com/v1")
	untrusted.Compat = json.RawMessage(`{"supports_finish_reason": false}`)
	msg = drive(t, untrusted, "data: "+
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"Hello"}}]}`+"\n\n")
	if msg.StopReason != core.StopReasonStop {
		t.Fatalf("stop = %q on a profile that never emits finish_reason, want stop", msg.StopReason)
	}
}

// TestAToolCallIsDecodedOnceNotOnEveryChunk is NFR-PERF on the per-chunk
// snapshot: every MessageUpdateEvent used to re-parse and re-salvage every
// tool call from its first byte, so a long argument stream cost O(n²). The
// snapshot now carries the last decoded value — an empty call until the
// arguments close, the real one after — and decodes once.
func TestAToolCallIsDecodedOnceNotOnEveryChunk(t *testing.T) {
	oa := model("openai", "gpt-4o", "https://api.openai.com/v1")
	chunk := func(args string) string {
		return "data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"edit","arguments":"` + args + `"}}]}}]}` + "\n\n"
	}
	sse := chunk(`{\"path\":`) + chunk(`\"a.go\",`) + chunk(`\"body\":\"x}y\"}`) +
		"data: " + `{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	req := userReq()
	req.Options.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(sse))}, nil
	})
	s := openai.Provider(openai.Options{Getenv: func(string) string { return "k" }}).
		Stream(context.Background(), oa, req, core.ProviderStreamOptions{})
	var inputs []string
	for e := range s.Events() {
		if u, ok := e.(core.MessageUpdateEvent); ok {
			for _, b := range u.Message.Content {
				if tu, ok := b.(core.ToolUseBlock); ok {
					inputs = append(inputs, string(tu.Input))
				}
			}
		}
	}
	if len(inputs) != 3 {
		t.Fatalf("%d snapshots carried the call, want one per chunk: %v", len(inputs), inputs)
	}
	// The first two snapshots carry the value as last decoded — nothing yet,
	// so an empty call — and NOT a salvage of the partial bytes.
	for _, in := range inputs[:2] {
		if in != "{}" {
			t.Fatalf("snapshot input = %s before the arguments closed, want {} — the last "+
				"decoded value, not a per-chunk re-parse of the partial bytes", in)
		}
	}
	if inputs[2] != `{"path":"a.go","body":"x}y"}` {
		t.Fatalf("snapshot input = %s once the object closed, want the whole call: a brace "+
			"inside a string must not close it early", inputs[2])
	}
	final := core.ExtractToolUse(s.Result())
	if len(final) != 1 || string(final[0].Input) != `{"path":"a.go","body":"x}y"}` {
		t.Fatalf("final call = %+v", final)
	}
}

// TestTheProfileFollowsTheEnvironmentsBaseURL is REQ-PROV-12 read with
// provider.ResolveBaseURL's precedence: OPENAI_BASE_URL beats the catalog row,
// so the profile must be inferred from the host the request actually reaches.
// A row that names api.openai.com pointed at DeepSeek through the environment
// was sending `store` and the developer role, which DeepSeek rejects.
func TestTheProfileFollowsTheEnvironmentsBaseURL(t *testing.T) {
	m := model("openai", "deepseek-chat", "https://api.openai.com/v1")
	var got string
	req := core.Request{
		System:   []core.ContentBlock{core.TextBlock{Text: "sys"}},
		Messages: userReq().Messages,
		Options: core.RequestOptions{
			Env: map[string]string{"OPENAI_API_KEY": "k", "OPENAI_BASE_URL": "https://api.deepseek.com"},
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				b, _ := io.ReadAll(r.Body)
				got = string(b)
				return &http.Response{StatusCode: 200, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
			}),
		},
	}
	openai.Provider(openai.Options{}).Stream(context.Background(), m, req, core.ProviderStreamOptions{}).Result()
	if got == "" {
		t.Fatal("no request was sent")
	}
	if strings.Contains(got, `"store"`) {
		t.Fatalf("store was sent to the DeepSeek host the environment selected: %s", got)
	}
	if strings.Contains(got, `"developer"`) {
		t.Fatalf("the developer role was sent to the DeepSeek host the environment selected: %s", got)
	}
	c, err := openai.CompatForBase(m, "https://api.deepseek.com")
	if err != nil {
		t.Fatal(err)
	}
	if !c.UseMaxTokens || c.ThinkingFormat != "deepseek" {
		t.Fatalf("CompatForBase = %+v, want the DeepSeek row", c)
	}
}

// TestAToolWithNoSchemaSendsAnEmptyObjectNotNull: `parameters: null` is a 400.
func TestAToolWithNoSchemaSendsAnEmptyObjectNotNull(t *testing.T) {
	req := userReq()
	req.Tools = []core.ToolWire{{Name: "ping", Description: "no arguments"}}
	got := body(t, model("openai", "gpt-4o", "https://api.openai.com/v1"), req)
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	params, ok := fn["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Fatalf("parameters = %#v, want an empty object schema, never null", fn["parameters"])
	}
}

// TestTheReasoningBudgetIsEmittedUnderTheProfilesFieldName is REQ-PROV-15's
// last bullet: an endpoint that shares max_tokens between the reasoning and
// the answer needs an explicit budget, or a reasoning-heavy turn consumes the
// whole response and emits no answer. The FIELD NAME is per-vendor
// (REQ-PROV-12 ThinkingTokenBudgetField).
func TestTheReasoningBudgetIsEmittedUnderTheProfilesFieldName(t *testing.T) {
	budgeted := func(vendor string) *core.Model {
		m := model(vendor, "qwen3", "http://gpu-host:8000/v1")
		lo, hi := "1024", "4096"
		m.Reasoning = true
		m.ThinkingLevelMap = map[core.ThinkingLevel]*string{
			core.ThinkingLow: &lo, core.ThinkingHigh: &hi,
		}
		return m
	}
	req := userReq()
	req.ThinkingLevel = core.ThinkingHigh

	for _, tc := range []struct{ vendor, field string }{
		{"vllm", "thinking_token_budget"},
		{"llamacpp", "thinking_budget_tokens"},
		{"qwen", "thinking_budget"},
	} {
		got := body(t, budgeted(tc.vendor), req)
		if got[tc.field] != float64(4096) {
			t.Fatalf("%s: %s = %v, want the resolved level's budget 4096 — without it a "+
				"reasoning-heavy turn spends max_tokens thinking and answers nothing",
				tc.vendor, tc.field, got[tc.field])
		}
		if _, present := got["reasoning_effort"]; present {
			t.Fatalf("%s: reasoning_effort = %v was sent to an endpoint that takes a budget, "+
				"not an effort string", tc.vendor, got["reasoning_effort"])
		}
	}

	// api.openai.com prices its levels as effort STRINGS and names no budget
	// field: it must get neither an invented budget nor a numeric effort.
	oa := model("openai", "o3", "https://api.openai.com/v1")
	medium := "medium"
	oa.Reasoning = true
	oa.ThinkingLevelMap = map[core.ThinkingLevel]*string{core.ThinkingMedium: &medium}
	req.ThinkingLevel = core.ThinkingMedium
	got := body(t, oa, req)
	if got["reasoning_effort"] != "medium" {
		t.Fatalf("reasoning_effort = %v, want the clamped wire value", got["reasoning_effort"])
	}
	for _, f := range []string{"thinking_token_budget", "thinking_budget", "thinking_budget_tokens"} {
		if _, present := got[f]; present {
			t.Fatalf("%s was emitted to a profile that names no budget field: %v", f, got)
		}
	}
}

// TestAUserMessageAfterAToolResultGetsASyntheticAssistantTurn is REQ-PROV-12's
// AllowsUserAfterToolResult. On OpenRouter's anthropic/* routes a role:"tool"
// message becomes a tool_result on a USER turn upstream, so a user message
// straight after one is two consecutive user turns and a hard 400.
func TestAUserMessageAfterAToolResultGetsASyntheticAssistantTurn(t *testing.T) {
	call, err := core.NewToolUse("call_1", "read_file", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{call}, StopReason: core.StopReasonToolUse,
			Provider: "openrouter", API: openai.API, Model: "anthropic/claude-sonnet-4-5"},
		core.ToolResultMessage{ToolUseID: "call_1", ToolName: "read_file",
			Content: core.Content{core.TextBlock{Text: "ok"}}},
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "and now?"}}},
	}}

	or := model("openrouter", "anthropic/claude-sonnet-4-5", "https://openrouter.ai/api/v1")
	var roles []string
	for _, m := range messagesOf(t, body(t, or, req)) {
		roles = append(roles, m["role"].(string))
	}
	want := []string{"user", "assistant", "tool", "assistant", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("roles = %v, want %v: a user message directly after a tool result needs a "+
			"synthetic assistant turn between them on this profile", roles, want)
	}
	if got := messagesOf(t, body(t, or, req))[3]["content"]; got != openai.SyntheticAssistantText {
		t.Fatalf("bridge content = %v, want %q — the gateways that need the bridge reject an "+
			"empty assistant turn too", got, openai.SyntheticAssistantText)
	}

	// The default profile allows the adjacency and must stay untouched.
	roles = nil
	for _, m := range messagesOf(t, body(t, model("openai", "gpt-4o", "https://api.openai.com/v1"), req)) {
		roles = append(roles, m["role"].(string))
	}
	if strings.Join(roles, ",") != "user,assistant,tool,user" {
		t.Fatalf("roles = %v on the default profile, want no bridge", roles)
	}
}

// ---------------------------------------------------- REQ-CACHE-06/NFR-PERF-03

func prefixTools(n int) []core.ToolWire {
	out := make([]core.ToolWire, n)
	for i := 0; i < n; i++ {
		name := "tool_" + string(rune('a'+i))
		out[i] = core.ToolWire{Name: name, Description: name,
			InputSchema: schema.Object(schema.Prop("q", schema.String("q")))}
	}
	return out
}

// TestASteadyStateChatCompletionsRequestSerializesNoSchemas is NFR-PERF-03 as
// a COUNT, the same shape the Anthropic adapter is pinned by: a cache that is
// implemented, unit-tested and attached to no provider costs memory and saves
// nothing.
func TestASteadyStateChatCompletionsRequestSerializesNoSchemas(t *testing.T) {
	m := model("openai", "gpt-4o", "https://api.openai.com/v1")
	req := core.Request{Tools: prefixTools(16)}
	prefix := &provider.ToolPrefix{}

	_, _, first, err := openai.BuildRequestCached(m, req, core.CacheRetentionShort, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if first.Marshalled != 16 {
		t.Fatalf("first build serialized %d schemas, want all 16", first.Marshalled)
	}
	second, _, srep, err := openai.BuildRequestCached(m, req, core.CacheRetentionShort, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if srep.Marshalled != 0 {
		t.Fatalf("a steady-state build serialized %d schemas, want 0", srep.Marshalled)
	}
	if srep.Invalidated {
		t.Fatalf("an unchanged tool list invalidated the prefix: %s", srep.Reason)
	}
	if len(second.Tools) != 16 {
		t.Fatalf("%d tools on the wire, want 16 served from the prefix", len(second.Tools))
	}
	raw, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"properties"`) {
		t.Fatalf("the cached schemas did not reach the wire: %s", raw)
	}
}

// TestTheChatCompletionsProviderOwnsAPrefixByDefault: the wiring must not
// require the caller to opt in, or the default configuration is the slow one.
func TestTheChatCompletionsProviderOwnsAPrefixByDefault(t *testing.T) {
	var reports []provider.SyncReport
	p := openai.Provider(openai.Options{
		Getenv:           func(string) string { return "k" },
		OnToolPrefixSync: func(r provider.SyncReport) { reports = append(reports, r) },
	})
	req := core.Request{Tools: prefixTools(8)}
	req.Options.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	})
	m := model("openai", "gpt-4o", "https://api.openai.com/v1")
	for i := 0; i < 2; i++ {
		p.Stream(context.Background(), m, req, core.ProviderStreamOptions{}).Result()
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

func deferredRequest() core.Request {
	call, _ := core.NewToolUse("call_1", "read_file", json.RawMessage(`{}`))
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
				Provider: "openai", API: openai.API, Model: "gpt-4o"},
			core.ToolResultMessage{ToolUseID: "call_1", ToolName: "read_file",
				Content:        core.Content{core.TextBlock{Text: "ok"}},
				AddedToolNames: []string{"mcp__db__query"}},
			core.UserMessage{Content: core.Content{core.TextBlock{Text: "now query"}}},
		},
	}
}

// TestADeferredToolIsWithheldFromTheArrayAndDeclaredAfterTheToolResultRun is
// REQ-CACHE-10's third arm: this wire has neither Anthropic's defer_loading
// nor the Responses API's additional_tools, so a tool that appeared
// mid-session is withheld from `tools` — whose bytes are the head of the
// cached prefix — and re-declared in a system message at the transcript
// position where it appeared.
func TestADeferredToolIsWithheldFromTheArrayAndDeclaredAfterTheToolResultRun(t *testing.T) {
	got := body(t, model("openai", "gpt-4o", "https://api.openai.com/v1"), deferredRequest())

	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("%d tools in the array, want only the established one: prepending a late "+
			"arrival rewrites the cached prefix", len(tools))
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read_file" {
		t.Fatalf("tools[0] = %v, want read_file", fn)
	}

	ms := messagesOf(t, got)
	var roles []string
	for _, m := range ms {
		roles = append(roles, m["role"].(string))
	}
	// developer is this profile's system role; the declaration lands AFTER the
	// tool-result run and BEFORE the user message that follows it.
	want := "user,assistant,tool,developer,user"
	if strings.Join(roles, ",") != want {
		t.Fatalf("roles = %v, want %v: the declaration belongs at the transcript position "+
			"where the tool appeared", roles, want)
	}
	decl, _ := ms[3]["content"].(string)
	if !strings.Contains(decl, "mcp__db__query") || !strings.Contains(decl, "sql") {
		t.Fatalf("declaration = %q, want the withheld tool and its parameters", decl)
	}
	if strings.Contains(decl, "read_file") {
		t.Fatalf("declaration = %q, want only the DEFERRED tools re-declared", decl)
	}

	// On a profile that emits breakpoints, the declaration carries none: it
	// sits past the prefix the deferral exists to keep cached, which is the
	// same rule as Anthropic's deferred tools.
	or := model("openrouter", "anthropic/claude-sonnet-4-5", "https://openrouter.ai/api/v1")
	found := false
	for _, m := range messagesOf(t, body(t, or, deferredRequest())) {
		if m["role"] != "system" && m["role"] != "developer" {
			continue
		}
		// A stamped message's content is a PART ARRAY carrying cache_control;
		// an unstamped one is still the plain string this encoder wrote.
		if !strings.Contains(string(mustJSON(t, m["content"])), "mcp__db__query") {
			continue
		}
		found = true
		if _, stamped := m["content"].([]any); stamped {
			t.Fatalf("the deferred declaration carries a breakpoint: %v", m["content"])
		}
	}
	if !found {
		t.Fatal("the declaration message was not found on the breakpoint-emitting profile")
	}
}

// TestWhenEveryToolWouldBeDeferredTheyArePromoted is REQ-CACHE-10's safety
// valve: with nothing immediate there is no prefix to anchor references
// against, so the deferral buys nothing.
func TestWhenEveryToolWouldBeDeferredTheyArePromoted(t *testing.T) {
	req := deferredRequest()
	req.Tools = req.Tools[:1] // the deferred one only
	got := body(t, model("openai", "gpt-4o", "https://api.openai.com/v1"), req)

	if tools, _ := got["tools"].([]any); len(tools) != 1 {
		t.Fatalf("%d tools in the array, want the promoted one", len(tools))
	}
	for _, m := range messagesOf(t, got) {
		if role := m["role"].(string); role == "developer" || role == "system" {
			t.Fatalf("a declaration message was emitted for a promoted tool: %v", m)
		}
	}
}
