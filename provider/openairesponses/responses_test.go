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
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/provider/openairesponses"
	"github.com/agentfox/agentkit-go/schema"
)

// The cross-provider conformance suite covers what this wire shares with the
// other four. These cover what makes it a SEPARATE implementation rather than
// openai-completions with a flag: the item model, the composite tool-call
// identity, reasoning replay, the caching parameters, and post-hoc service-tier
// billing.

func model() *core.Model {
	return &core.Model{
		ID: "gpt-resp", API: openairesponses.API, Provider: "openai",
		ContextWindow: 200000, MaxTokens: 4096, Input: []string{"text", "image"},
		Cost: core.Cost{Input: 3, Output: 15, CacheRead: 0.3},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// send drives the provider and returns the request body it produced.
func send(t *testing.T, opts openairesponses.Options, req core.Request, stream string) map[string]any {
	t.Helper()
	var raw []byte
	req.Options.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ = io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	req.Options.Env = map[string]string{"OPENAI_API_KEY": "test-key"}
	openairesponses.Provider(opts).Stream(context.Background(), model(), req,
		core.ProviderStreamOptions{}).Result()
	if len(raw) == 0 {
		t.Fatal("no request body was sent")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, raw)
	}
	return body
}

func items(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	in, _ := body["input"].([]any)
	out := make([]map[string]any, 0, len(in))
	for _, v := range in {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("input element is %T, want an object", v)
		}
		out = append(out, m)
	}
	return out
}

// ---- the item model

// TestOneAssistantMessageBecomesSeveralItems is the difference §4 names first.
//
// On the Chat Completions wire an assistant turn is ONE message with a content
// string and a tool_calls array. Here text and each tool call are separate
// top-level items, and their order is the order the model produced them.
func TestOneAssistantMessageBecomesSeveralItems(t *testing.T) {
	call, err := core.NewToolUse("call_1|fc_1", "edit_file", json.RawMessage(`{"path":"a.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	body := send(t, openairesponses.Options{}, core.Request{
		Messages: core.Messages{
			core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}},
			core.AssistantMessage{Content: core.Content{
				core.TextBlock{Text: "Editing."}, call}},
		},
	}, completedStream)

	// Four, not three: the transcript ends on an unanswered tool call, so
	// REQ-PROV-11's repair appends a synthetic result. That is the repair
	// layer doing its job, and asserting three here would be asserting it did
	// not.
	got := items(t, body)
	if len(got) != 4 {
		t.Fatalf("want user, assistant text, function_call and the synthetic result; "+
			"got %d: %v", len(got), got)
	}
	if got[1]["type"] != "message" || got[1]["role"] != "assistant" {
		t.Fatalf("item 1 = %v, want the assistant message", got[1])
	}
	if got[2]["type"] != "function_call" {
		t.Fatalf("item 2 = %v, want a SIBLING function_call item, not a field on the "+
			"assistant message as the Chat Completions wire has it", got[2])
	}
	if got[3]["type"] != "function_call_output" {
		t.Fatalf("item 3 = %v, want the synthetic result REQ-PROV-11 inserts", got[3])
	}
	// The text and the call are separate items, so neither carries the other.
	if _, nested := got[1]["tool_calls"]; nested {
		t.Fatal("this wire has no tool_calls field on a message")
	}
}

// TestTheSystemPromptIsInstructionsNotAnItem. It is a top-level string on this
// wire; sending it as a message item consumes an item and, on the vendors that
// validate, is refused.
func TestTheSystemPromptIsInstructionsNotAnItem(t *testing.T) {
	body := send(t, openairesponses.Options{}, core.Request{
		System:   []core.ContentBlock{core.TextBlock{Text: "Be terse."}},
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
	}, completedStream)

	if body["instructions"] != "Be terse." {
		t.Fatalf("instructions = %v", body["instructions"])
	}
	for _, it := range items(t, body) {
		if it["role"] == "system" {
			t.Fatal("the system prompt must not also appear as an input item")
		}
	}
}

// TestAToolIsFlatNotNested. The Chat Completions wire nests name, description
// and parameters under `function`; this one does not, and reusing that shape
// is a 400 whose message does not say so.
func TestAToolIsFlatNotNested(t *testing.T) {
	body := send(t, openairesponses.Options{}, core.Request{
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		Tools: core.ToolWires([]core.Tool{{
			Name: "edit_file", Description: "edit", InputSchema: schema.Object(),
		}}),
	}, completedStream)

	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %v", body["tools"])
	}
	tl := tools[0].(map[string]any)
	if tl["name"] != "edit_file" {
		t.Fatalf("name must be flat on the tool; got %v", tl)
	}
	if _, nested := tl["function"]; nested {
		t.Fatal("this wire does not nest the tool under `function`")
	}
}

// ---- the composite tool-call identity

// TestTheToolCallIdentityIsComposite is §4's `callId|itemId`.
//
// Both halves are load-bearing and not interchangeable: call_id is what a
// function_call_output references, and the item id is what a reasoning replay
// lines up against. A canonical ToolUseBlock has one id, so they travel joined.
func TestTheToolCallIdentityIsComposite(t *testing.T) {
	if got := openairesponses.JoinID("call_1", "fc_1"); got != "call_1|fc_1" {
		t.Fatalf("JoinID = %q", got)
	}
	callID, itemID := openairesponses.SplitID("call_1|fc_1")
	if callID != "call_1" || itemID != "fc_1" {
		t.Fatalf("SplitID = %q, %q", callID, itemID)
	}
	// A bare id is what a tool call replayed from another provider looks like.
	callID, itemID = openairesponses.SplitID("toolu_abc")
	if callID != "toolu_abc" || itemID != "" {
		t.Fatalf("a separator-less id must round-trip as the call id: %q, %q", callID, itemID)
	}
	if got := openairesponses.JoinID("toolu_abc", ""); got != "toolu_abc" {
		t.Fatalf("an empty item id must not introduce a separator: %q", got)
	}
}

// TestAFunctionCallOutputReferencesOnlyTheCallID. Sending the composite id
// where the API expects call_id is rejected, and the error names neither half.
func TestAFunctionCallOutputReferencesOnlyTheCallID(t *testing.T) {
	call, err := core.NewToolUse("call_1|fc_1", "edit_file", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body := send(t, openairesponses.Options{}, core.Request{
		Messages: core.Messages{
			core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}},
			core.AssistantMessage{Content: core.Content{call}, StopReason: core.StopReasonToolUse},
			core.ToolResultMessage{ToolUseID: "call_1|fc_1", ToolName: "edit_file",
				Content: core.Content{core.TextBlock{Text: "done"}}},
		},
	}, completedStream)

	var sawOutput bool
	for _, it := range items(t, body) {
		switch it["type"] {
		case "function_call":
			if it["call_id"] != "call_1" || it["id"] != "fc_1" {
				t.Fatalf("the call item must carry both halves separately: %v", it)
			}
		case "function_call_output":
			sawOutput = true
			if it["call_id"] != "call_1" {
				t.Fatalf("the output must reference the CALL id alone; got %v", it["call_id"])
			}
		}
	}
	if !sawOutput {
		t.Fatal("no function_call_output item was emitted")
	}
}

// TestTheDecoderProducesACompositeID.
func TestTheDecoderProducesACompositeID(t *testing.T) {
	msg := drive(t, toolStream)
	calls := core.ExtractToolUse(msg)
	if len(calls) != 1 {
		t.Fatalf("want 1 tool call, got %d (%v)", len(calls), msg.Content)
	}
	if calls[0].ID != "call_1|fc_1" {
		t.Fatalf("id = %q, want the composite of call_id and item id", calls[0].ID)
	}
}

// ---- reasoning replay

// TestReasoningIsReplayedWithItsItemIDAndEncryptedContent.
//
// Without this the model's chain is silently lost between turns: the item
// round-trips as a ThinkingBlock with a Signature, and the Signature is the
// only place an item id and an encrypted blob can live.
func TestReasoningIsReplayedWithItsItemIDAndEncryptedContent(t *testing.T) {
	msg := drive(t, reasoningStream)

	var think *core.ThinkingBlock
	for _, b := range msg.Content {
		if tb, ok := b.(core.ThinkingBlock); ok {
			think = &tb
		}
	}
	if think == nil {
		t.Fatalf("no thinking block was decoded: %v", msg.Content)
	}
	if think.Signature == "" {
		t.Fatal("a reasoning item with no signature cannot be replayed at all")
	}

	// Replay it and check both fields survive the round trip.
	body := send(t, openairesponses.Options{}, core.Request{
		Messages: core.Messages{
			core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}},
			core.AssistantMessage{Content: core.Content{*think}, Provider: "openai",
				API: openairesponses.API, Model: "gpt-resp"},
		},
	}, completedStream)

	var found bool
	for _, it := range items(t, body) {
		if it["type"] != "reasoning" {
			continue
		}
		found = true
		if it["id"] != "rs_1" {
			t.Fatalf("reasoning item id = %v, want rs_1", it["id"])
		}
		if it["encrypted_content"] != "ENCRYPTED" {
			t.Fatalf("encrypted_content = %v, want it replayed verbatim", it["encrypted_content"])
		}
	}
	if !found {
		t.Fatalf("the reasoning item was not replayed: %v", items(t, body))
	}
}

// TestEncryptedReasoningIsRequested. The response carries no encrypted_content
// unless `include` asks for it, and a stateless caller then has nothing to
// replay — silently, since the turn otherwise succeeds.
func TestEncryptedReasoningIsRequested(t *testing.T) {
	body := send(t, openairesponses.Options{}, core.Request{
		Messages:      core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		ThinkingLevel: core.ThinkingMedium,
	}, completedStream)

	inc, _ := body["include"].([]any)
	var found bool
	for _, v := range inc {
		if v == "reasoning.encrypted_content" {
			found = true
		}
	}
	if !found {
		t.Fatalf("include = %v, want reasoning.encrypted_content", body["include"])
	}
	r, _ := body["reasoning"].(map[string]any)
	if r["effort"] != "medium" {
		t.Fatalf("reasoning = %v, want effort medium", r)
	}
}

// TestAForeignThinkingSignatureIsNotReplayedAsAnItem. A signature issued by
// another provider names an item this server never created; sending it is
// rejected, and REQ-PROV-11 rule 3 strips it on cross-model replay anyway.
func TestAForeignThinkingSignatureIsNotReplayedAsAnItem(t *testing.T) {
	body := send(t, openairesponses.Options{}, core.Request{
		Messages: core.Messages{
			core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}},
			core.AssistantMessage{
				Content:  core.Content{core.ThinkingBlock{Thinking: "hmm", Signature: "anthropic-sig-bytes"}},
				Provider: "openai", API: openairesponses.API, Model: "gpt-resp"},
		},
	}, completedStream)

	for _, it := range items(t, body) {
		if it["type"] == "reasoning" && it["id"] != nil && it["id"] != "" {
			t.Fatalf("a foreign signature must not become an item id: %v", it)
		}
	}
}

// ---- caching parameters

// TestTheCachingParametersAreThisWiresOwn. store defaults to false — keeping
// the full prompt and completion on the vendor's side is a decision an SDK
// must not make for an embedder — and the session id becomes prompt_cache_key.
func TestTheCachingParametersAreThisWiresOwn(t *testing.T) {
	body := send(t, openairesponses.Options{}, core.Request{
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		Options:  core.RequestOptions{SessionID: "sess-abc"},
	}, completedStream)

	if body["store"] != false {
		t.Fatalf("store = %v, want false: storing server-side is the embedder's call",
			body["store"])
	}
	if body["prompt_cache_key"] != "sess-abc" {
		t.Fatalf("prompt_cache_key = %v", body["prompt_cache_key"])
	}
}

// TestTheCompatProfileKeysAreDisjointFromChatCompletions is §4.2's rule.
//
// A catalog row's `compat` blob is a bag of keys. A name shared between two
// API profiles would let a flag written for one wire be silently read by the
// other, where it means something else or nothing.
func TestTheCompatProfileKeysAreDisjointFromChatCompletions(t *testing.T) {
	shared := intersectJSONKeys(t, openairesponses.DefaultCompat(), openai.DefaultCompat())
	if len(shared) > 0 {
		t.Fatalf("the two profiles share keys %v; a compat blob written for one wire "+
			"would be read by the other", shared)
	}
}

func intersectJSONKeys(t *testing.T, a, b any) []string {
	t.Helper()
	keys := func(v any) map[string]bool {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for k := range m {
			out[k] = true
		}
		return out
	}
	ka, kb := keys(a), keys(b)
	var shared []string
	for k := range ka {
		if kb[k] {
			shared = append(shared, k)
		}
	}
	return shared
}

// ---- billing

// TestTheServiceTierMultiplierIsAppliedPostHoc is §4's billing difference.
//
// The tier scales the COMPUTED cost rather than selecting a different rate
// table, so it composes with REQ-PROV-05.4's threshold tiering instead of
// replacing it.
func TestTheServiceTierMultiplierIsAppliedPostHoc(t *testing.T) {
	base := driveWith(t, openairesponses.Options{}, usageStream)
	flex := driveWith(t, openairesponses.Options{
		ServiceTier: provider.ServiceTier{Name: "flex", Multiplier: 0.5},
	}, usageStream)

	if base.Usage.CostUSD <= 0 {
		t.Fatalf("no cost was computed: %+v", base.Usage)
	}
	if got, want := flex.Usage.CostUSD, base.Usage.CostUSD*0.5; !closeEnough(got, want) {
		t.Fatalf("flex cost = %v, want half of %v", got, base.Usage.CostUSD)
	}
}

func closeEnough(a, b float64) bool {
	d := a - b
	return d < 1e-12 && d > -1e-12
}

// ---- usage

// TestCachedTokensAreNettedOutOfInput. This wire names the field input_tokens
// rather than prompt_tokens, and it INCLUDES the cached portion — so assigning
// it straight across double-counts every cached token.
func TestCachedTokensAreNettedOutOfInput(t *testing.T) {
	msg := drive(t, usageStream)
	if msg.Usage.InputTokens != 100 {
		t.Fatalf("input_tokens = %d, want 100 net of the 900 cache reads",
			msg.Usage.InputTokens)
	}
	if msg.Usage.CacheReadTokens != 900 {
		t.Fatalf("cache_read = %d, want 900", msg.Usage.CacheReadTokens)
	}
}

// ---- helpers

func drive(t *testing.T, stream string) *core.AssistantMessage {
	t.Helper()
	return driveWith(t, openairesponses.Options{}, stream)
}

func driveWith(t *testing.T, opts openairesponses.Options, stream string) *core.AssistantMessage {
	t.Helper()
	req := core.Request{Options: core.RequestOptions{
		Env: map[string]string{"OPENAI_API_KEY": "test-key"},
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(stream))}, nil
		}),
	}}
	return openairesponses.Provider(opts).Stream(context.Background(), model(), req,
		core.ProviderStreamOptions{}).Result()
}

func ev(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

var completedStream = ev("response.completed",
	`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-resp","status":"completed"}}`)

var toolStream = ev("response.output_item.added",
	`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"edit_file","arguments":""}}`) +
	ev("response.function_call_arguments.delta",
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"path\":\"a.go\"}"}`) +
	ev("response.output_item.done",
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"edit_file","arguments":"{\"path\":\"a.go\"}"}}`) +
	ev("response.completed",
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-resp","status":"completed"}}`)

var reasoningStream = ev("response.output_item.added",
	`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"ENCRYPTED","summary":[]}}`) +
	ev("response.reasoning_summary_text.delta",
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"weighing options"}`) +
	ev("response.output_item.done",
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"ENCRYPTED","summary":[{"type":"summary_text","text":"weighing options"}]}}`) +
	ev("response.completed",
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-resp","status":"completed"}}`)

var usageStream = ev("response.output_item.added",
	`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`) +
	ev("response.output_text.delta",
		`{"type":"response.output_text.delta","output_index":0,"delta":"Hello"}`) +
	ev("response.completed",
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-resp","status":"completed","usage":{"input_tokens":1000,"output_tokens":42,"total_tokens":1042,"input_tokens_details":{"cached_tokens":900}}}}`)

// TestAPlaceholderArgumentStringIsNotSeededIntoTheDeltas.
//
// output_item.added announces a function_call whose `arguments` is a
// placeholder — "" from the OpenAI endpoint, "{}" from gateways that fill the
// field in. Seeding the accumulator from it and then appending the real deltas
// produces `{}{"path":"a.go"}`: invalid JSON, which the salvage pass then
// repairs down to the placeholder. The tool is called with no arguments and
// nothing reports why.
//
// The Anthropic decoder had exactly this bug against `"input":{}`.
func TestAPlaceholderArgumentStringIsNotSeededIntoTheDeltas(t *testing.T) {
	stream := ev("response.output_item.added",
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"edit_file","arguments":"{}"}}`) +
		ev("response.function_call_arguments.delta",
			`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"path\":"}`) +
		ev("response.function_call_arguments.delta",
			`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"\"a.go\"}"}`) +
		ev("response.completed",
			`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-resp","status":"completed"}}`)

	msg := drive(t, stream)
	calls := core.ExtractToolUse(msg)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d: %v", len(calls), msg.Content)
	}
	if got := string(calls[0].Input); got != `{"path":"a.go"}` {
		t.Fatalf("arguments = %s, want the deltas alone. A placeholder seeded ahead "+
			"of them makes the concatenation invalid, and the salvage pass then "+
			"keeps the placeholder.", got)
	}
}

// TestATruncatedStreamIsReportedRatherThanReturnedAsSuccess.
//
// A stream that stops before response.completed has delivered a partial turn.
// Returning it as a normal result hands the loop a truncated assistant message
// it cannot distinguish from a complete one — and the retry layer, which would
// have re-run it, never sees a reason to.
func TestATruncatedStreamIsReportedRatherThanReturnedAsSuccess(t *testing.T) {
	truncated := ev("response.output_item.added",
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`) +
		ev("response.output_text.delta",
			`{"type":"response.output_text.delta","output_index":0,"delta":"Hel"}`)

	msg := drive(t, truncated)
	if msg.StopReason != core.StopReasonError {
		t.Fatalf("stop reason = %q, want error: the stream ended before "+
			"response.completed", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "response.completed") {
		t.Fatalf("error = %q, want it to name the missing terminal event",
			msg.ErrorMessage)
	}
	// What arrived is still kept (REQ-PROV-04: partial content AND a failure).
	if msg.Content.Text() != "Hel" {
		t.Fatalf("content = %q, want the partial text preserved", msg.Content.Text())
	}
}
