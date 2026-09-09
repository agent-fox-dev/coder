package ollama_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/ollama"
	"github.com/agentfox/agentkit-go/schema"
)

// ---------------------------------------------------------------- fixtures

func mk(t *testing.T, id, name, args string) core.ToolUseBlock {
	t.Helper()
	b, err := core.NewToolUse(id, name, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func model() *core.Model {
	return &core.Model{ID: "qwen3:8b", API: ollama.API, Provider: "ollama",
		MaxTokens: 4096, Input: []string{"text", "image"}}
}

// transcript is the canonical shape: a user turn, an assistant turn emitting
// THREE parallel tool calls, and three separate ToolResultMessages
// (REQ-LOOP-02).
func transcript(t *testing.T) core.Messages {
	t.Helper()
	return core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "check three files"}}},
		core.AssistantMessage{
			Content: core.Content{
				core.TextBlock{Text: "Reading them now."},
				mk(t, "call_a", "read_file", `{"path":"a.go"}`),
				mk(t, "call_b", "read_file", `{"path":"b.go"}`),
				mk(t, "call_c", "read_file", `{"path":"c.go"}`),
			},
			StopReason: core.StopReasonToolUse,
			Provider:   "ollama", API: ollama.API, Model: "qwen3:8b",
		},
		core.ToolResultMessage{ToolUseID: "call_a", ToolName: "read_file",
			Content: core.Content{core.TextBlock{Text: "contents of a"}}},
		core.ToolResultMessage{ToolUseID: "call_b", ToolName: "read_file",
			Content: core.Content{core.TextBlock{Text: "contents of b"}}},
		core.ToolResultMessage{ToolUseID: "call_c", ToolName: "read_file",
			Content: core.Content{core.TextBlock{Text: "contents of c"}}},
	}
}

func request(t *testing.T) core.Request {
	t.Helper()
	return core.Request{
		System:   []core.ContentBlock{core.TextBlock{Text: "You are a code reader."}},
		Messages: transcript(t),
		Tools: []core.ToolWire{{
			Name: "read_file", Description: "Read a file",
			InputSchema: schema.Object(schema.Prop("path", schema.String("path"))),
		}},
	}
}

type wire struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Think    *bool  `json:"think"`
	Messages []struct {
		Role      string   `json:"role"`
		Content   *string  `json:"content"`
		Thinking  string   `json:"thinking"`
		Images    []string `json:"images"`
		ToolName  string   `json:"tool_name"`
		ToolCalls []struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	Options *struct {
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		NumPredict  *int     `json:"num_predict"`
	} `json:"options"`
}

func build(t *testing.T, m *core.Model, r core.Request) ([]byte, wire) {
	t.Helper()
	body, _, err := ollama.BuildRequest(m, r)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var w wire
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	return raw, w
}

// ------------------------------------------------------------------- shape

// TestToolResultsAreOneToolMessageEachAndCarryNoToolCallID is REQ-LOOP-02's
// Ollama half plus the fidelity limitation of this wire: the results follow
// the OpenAI-compatible SHAPE, but there is no tool_call_id, so the server
// pairs them to the calls POSITIONALLY. Order is therefore load-bearing.
func TestToolResultsAreOneToolMessageEachAndCarryNoToolCallID(t *testing.T) {
	raw, w := build(t, model(), request(t))

	var results []string
	for _, m := range w.Messages {
		if m.Role == "tool" {
			if m.Content == nil {
				t.Fatal("a tool message reached the wire with no content field")
			}
			results = append(results, *m.Content)
		}
	}
	if len(results) != 3 {
		t.Fatalf("%d role:\"tool\" messages, want 3 — one per result", len(results))
	}
	if strings.Join(results, "|") != "contents of a|contents of b|contents of c" {
		t.Fatalf("tool message order = %v; results are matched to calls POSITIONALLY on this "+
			"wire, so any other order silently swaps two answers", results)
	}
	if strings.Contains(string(raw), "tool_call_id") {
		t.Fatalf("tool_call_id reached the native /api/chat wire; it has no such field "+
			"(that is the OpenAI-compatible shim's shape).\nbody: %s", raw)
	}
}

// TestToolCallArgumentsAreAJSONObjectNotAString is the difference from Chat
// Completions that a reader coming from provider/openai will get wrong:
// arguments is an OBJECT here. Sending the stringified form makes the server
// hand the model a quoted blob as its own arguments.
func TestToolCallArgumentsAreAJSONObjectNotAString(t *testing.T) {
	raw, w := build(t, model(), request(t))

	var calls int
	for _, m := range w.Messages {
		for _, tc := range m.ToolCalls {
			calls++
			var obj map[string]any
			if err := json.Unmarshal(tc.Function.Arguments, &obj); err != nil {
				t.Fatalf("tool_calls[].function.arguments = %s, which is not a JSON object: %v",
					tc.Function.Arguments, err)
			}
		}
	}
	if calls != 3 {
		t.Fatalf("%d tool calls reached the wire, want 3", calls)
	}
	if strings.Contains(string(raw), `"arguments":"`) {
		t.Fatalf("arguments were stringified (the Chat Completions shape): %s", raw)
	}
}

// TestToolArgumentBytesSurviveUnchanged pins REQ-PROV-17 / REQ-TOOL-12: the
// model's own bytes, in the model's own key order. A decode-and-re-encode
// round trip would sort them and change the text the model is conditioned on.
func TestToolArgumentBytesSurviveUnchanged(t *testing.T) {
	const args = `{"zeta":1,"alpha":{"yankee":true,"bravo":[{"zulu":"z","charlie":0}]},"mid":"x"}`
	msgs := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{mk(t, "call_1", "t", args)},
			StopReason: core.StopReasonToolUse},
		core.ToolResultMessage{ToolUseID: "call_1", ToolName: "t",
			Content: core.Content{core.TextBlock{Text: "ok"}}},
	}
	raw, _ := build(t, model(), core.Request{Messages: msgs})
	if !strings.Contains(string(raw), args) {
		t.Fatalf("argument bytes were reordered on the Ollama wire.\nwant substring: %s\ngot: %s", args, raw)
	}
}

// TestSystemPromptIsALeadingSystemMessage: this wire has no separate
// instruction field, unlike Gemini.
func TestSystemPromptIsALeadingSystemMessage(t *testing.T) {
	_, w := build(t, model(), request(t))
	if len(w.Messages) == 0 || w.Messages[0].Role != "system" {
		t.Fatalf("first message role = %q, want system", w.Messages[0].Role)
	}
	if *w.Messages[0].Content != "You are a code reader." {
		t.Fatalf("system content = %q", *w.Messages[0].Content)
	}
}

// TestAssistantMessageAlwaysCarriesAContentField: content is required on this
// wire, including on a turn that is nothing but tool calls. Omitting it — the
// natural result of `json:"content,omitzero"` — is a 400.
func TestAssistantMessageAlwaysCarriesAContentField(t *testing.T) {
	msgs := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{mk(t, "call_1", "t", `{}`)},
			StopReason: core.StopReasonToolUse},
		core.ToolResultMessage{ToolUseID: "call_1", ToolName: "t",
			Content: core.Content{core.TextBlock{Text: "ok"}}},
	}
	raw, _ := build(t, model(), core.Request{Messages: msgs})

	var body struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for i, m := range body.Messages {
		if _, ok := m["content"]; !ok {
			t.Fatalf("messages[%d] (%s) has no content field; it is required on /api/chat",
				i, m["role"])
		}
	}
}

// ------------------------------------------------------------------ options

// TestSamplingParametersLiveUnderOptionsAndAnExplicitZeroSurvives pins two
// failures at once: a top-level temperature is IGNORED by this server (there
// is no 400 to notice), and `omitempty` on a bare float64 would drop an
// explicit 0, silently restoring the model's default of 0.8.
func TestSamplingParametersLiveUnderOptionsAndAnExplicitZeroSurvives(t *testing.T) {
	zero, cap := 0.0, 256
	r := request(t)
	r.Temperature = &zero
	r.MaxTokens = &cap

	raw, w := build(t, model(), r)

	if w.Options == nil || w.Options.Temperature == nil {
		t.Fatalf("an explicit temperature of 0 was dropped: %s", raw)
	}
	if *w.Options.Temperature != 0 {
		t.Fatalf("options.temperature = %v, want 0", *w.Options.Temperature)
	}
	if w.Options.NumPredict == nil || *w.Options.NumPredict != 256 {
		t.Fatalf("options.num_predict = %v, want 256", w.Options.NumPredict)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"temperature", "top_p", "max_tokens",
		"max_completion_tokens", "num_predict"} {
		if _, ok := top[forbidden]; ok {
			t.Errorf("%q was emitted as a TOP-LEVEL field; /api/chat reads sampling parameters "+
				"only under \"options\" and ignores it silently", forbidden)
		}
	}
	if !w.Stream {
		t.Error("stream was not requested")
	}
}

// TestImagesRideAsBareBase64OnTheMessage: not data: URLs, and not content
// parts. A data: prefix is not rejected — it is decoded as image bytes and
// fails deep inside the vision model.
func TestImagesRideAsBareBase64OnTheMessage(t *testing.T) {
	const b64 = "iVBORw0KGgoAAAANSUhEUg=="
	msgs := core.Messages{core.UserMessage{Content: core.Content{
		core.TextBlock{Text: "what is this?"},
		core.ImageBlock{MimeType: "image/png", Data: b64},
	}}}
	raw, w := build(t, model(), core.Request{Messages: msgs})

	last := w.Messages[len(w.Messages)-1]
	if len(last.Images) != 1 || last.Images[0] != b64 {
		t.Fatalf("images = %v, want one bare base64 string", last.Images)
	}
	if *last.Content != "what is this?" {
		t.Fatalf("content = %q, want the text only", *last.Content)
	}
	if strings.Contains(string(raw), "data:image") {
		t.Fatalf("a data: URL reached the wire: %s", raw)
	}
}

// ----------------------------------------------------------------- thinking

// TestThinkIsATriState pins REQ-PROV-16.1 on this wire's one toggle: absent,
// false and true are three different requests, and a bare bool can express
// only two of them.
func TestThinkIsATriState(t *testing.T) {
	base := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}}}

	t.Run("unset emits nothing", func(t *testing.T) {
		raw, w := build(t, model(), base)
		if w.Think != nil {
			t.Fatalf("think=%v was invented for a caller who set no level: %s", *w.Think, raw)
		}
		if strings.Contains(string(raw), `"think"`) {
			t.Fatalf("think key present: %s", raw)
		}
	})
	t.Run("off emits an explicit false", func(t *testing.T) {
		r := base
		r.ThinkingLevel = core.ThinkingOff
		raw, w := build(t, model(), r)
		if w.Think == nil || *w.Think {
			t.Fatalf("think = %v, want an explicit false: %s", w.Think, raw)
		}
	})
	t.Run("a level emits true", func(t *testing.T) {
		r := base
		r.ThinkingLevel = core.ThinkingHigh
		_, w := build(t, model(), r)
		if w.Think == nil || !*w.Think {
			t.Fatalf("think = %v, want true", w.Think)
		}
	})
	t.Run("a model without thinking support gets no think field", func(t *testing.T) {
		m := model()
		m.Compat = json.RawMessage(`{"supports_think":false}`)
		r := base
		r.ThinkingLevel = core.ThinkingHigh
		_, w := build(t, m, r)
		if w.Think != nil {
			t.Fatalf("think=%v was sent to a model that does not support thinking; the server "+
				"rejects the request outright", *w.Think)
		}
	})
}

// ------------------------------------------------------------------ compat

// TestToolNameIsWithheldUnlessTheProfileEnablesIt: tool_name is a recent
// field, and an older server rejects unknown message fields rather than
// ignoring them (REQ-PROV-12: a flag per named failure). Turning it on is the
// only partial answer to positional matching.
func TestToolNameIsWithheldUnlessTheProfileEnablesIt(t *testing.T) {
	raw, _ := build(t, model(), request(t))
	if strings.Contains(string(raw), "tool_name") {
		t.Fatalf("tool_name was emitted by default: %s", raw)
	}

	m := model()
	m.Compat = json.RawMessage(`{"supports_tool_name":true}`)
	_, w := build(t, m, request(t))
	var named int
	for _, msg := range w.Messages {
		if msg.Role == "tool" && msg.ToolName == "read_file" {
			named++
		}
	}
	if named != 3 {
		t.Fatalf("%d tool messages carried tool_name, want 3 once the profile enables it", named)
	}
}

// TestToolChoiceNoneWithholdsTheTools: /api/chat has no tool_choice field, so
// the only honest way to honour REQ-TOOL-16's "none" is to send no tools.
// Forwarding them and hoping the model declines is the invented-selection
// failure the requirement is about.
func TestToolChoiceNoneWithholdsTheTools(t *testing.T) {
	r := request(t)
	r.ToolChoice = core.ToolChoiceNone
	_, w := build(t, model(), r)
	if len(w.Tools) != 0 {
		t.Fatalf("%d tools were sent under ToolChoiceNone", len(w.Tools))
	}

	r.ToolChoice = core.ToolChoiceUnset
	_, w = build(t, model(), r)
	if len(w.Tools) != 1 || w.Tools[0].Type != "function" || w.Tools[0].Function.Name != "read_file" {
		t.Fatalf("tools = %+v, want one function tool", w.Tools)
	}
}

// TestNoProviderSideCacheMarkersAreStamped: Ollama has no provider-side prompt
// caching (NFR-COMPAT-04, §6.2a). Porting Anthropic's breakpoint stamping here
// would emit fields the server rejects.
func TestNoProviderSideCacheMarkersAreStamped(t *testing.T) {
	raw, _ := build(t, model(), request(t))
	for _, forbidden := range []string{"cache_control", "cache", "prompt_cache_key"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("%q reached the Ollama wire; this API has no provider-side caching: %s",
				forbidden, raw)
		}
	}
}

// TestUnansweredToolUseIsRepairedAtSendTime: a transcript loaded from disk
// ending on an unanswered tool call — what every Ctrl-C leaves behind — is
// made sendable by the PROVIDER (REQ-PROV-11). On this wire it matters twice:
// with positional matching, a missing result would misalign every later one.
func TestUnansweredToolUseIsRepairedAtSendTime(t *testing.T) {
	msgs := core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{mk(t, "call_x", "read_file", `{}`)},
			StopReason: core.StopReasonToolUse,
			Provider:   "ollama", API: ollama.API, Model: "qwen3:8b"},
	}
	body, rep, err := ollama.BuildRequest(model(), core.Request{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if rep.SyntheticResults != 1 {
		t.Fatalf("SyntheticResults = %d, want 1", rep.SyntheticResults)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "No result provided") {
		t.Fatalf("no synthetic tool message reached the wire: %s", raw)
	}
	if !strings.Contains(string(raw), "Error: No result provided") {
		t.Fatalf("the synthetic result's is_error was lost entirely; this wire has no is_error "+
			"field, so it has to be rendered into the text: %s", raw)
	}
}

// ------------------------------------------------------------ done reasons

// TestMapStopReasonReportsToolUseForStopWithToolCalls: Ollama has no
// "tool_calls" done_reason at all — it says "stop" on a turn that called
// tools. A loop gated on the reason would execute none of them (REQ-LOOP-01),
// and no input may be reported as an error (ruling P-41).
func TestMapStopReasonReportsToolUseForStopWithToolCalls(t *testing.T) {
	cases := []struct {
		reason   string
		hasCalls bool
		want     core.StopReason
	}{
		{"stop", true, core.StopReasonToolUse}, // the whole point
		{"stop", false, core.StopReasonStop},
		{"length", false, core.StopReasonLength},
		{"", true, core.StopReasonToolUse},
		{"", false, core.StopReasonStop},
		{"load", false, core.StopReasonStop},
		{"unload", false, core.StopReasonStop},
		{"something_new", false, core.StopReasonStop},
		{"something_new", true, core.StopReasonToolUse},
	}
	for _, c := range cases {
		got := ollama.MapStopReason(c.reason, c.hasCalls)
		if got != c.want {
			t.Errorf("MapStopReason(%q, %v) = %q, want %q", c.reason, c.hasCalls, got, c.want)
		}
		if got == core.StopReasonError {
			t.Errorf("MapStopReason(%q, %v) reported an error; StopReasonError short-circuits "+
				"the loop and drops the turn in repair rule 2 (ruling P-41)", c.reason, c.hasCalls)
		}
		if alias := ollama.MapFinishReason(c.reason, c.hasCalls); alias != got {
			t.Errorf("MapFinishReason disagrees with MapStopReason for (%q, %v)", c.reason, c.hasCalls)
		}
	}
}

// TestNormalizeToolCallID pins the shape rule 5 uses. The id never reaches
// this wire, but rule 6's synthetic results are keyed by it.
func TestNormalizeToolCallID(t *testing.T) {
	if got := ollama.NormalizeToolCallID("call_abc-123"); got != "call_abc-123" {
		t.Errorf("NormalizeToolCallID mangled a legal id: %q", got)
	}
	if got := ollama.NormalizeToolCallID("call/with spaces"); got != "call_with_spaces" {
		t.Errorf("got %q", got)
	}
	if got := ollama.NormalizeToolCallID(strings.Repeat("x", 100)); len(got) != 64 {
		t.Errorf("id length = %d, want 64", len(got))
	}
	if got := ollama.NormalizeToolCallID(""); got != "" {
		t.Errorf("an empty id must stay empty, got %q", got)
	}
}

// TestThinkIsClampedAgainstTheRowsMap is REQ-PROV-15 on a wire whose only
// thinking control is a boolean: think:true is sent when the row prices some
// level reachable from the request, and omitted when it prices none — a row
// that maps nothing above off is a model whose server rejects think:true.
func TestThinkIsClampedAgainstTheRowsMap(t *testing.T) {
	base := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}}}
	on := "true"

	m := model()
	m.Reasoning = true
	m.ThinkingLevelMap = map[core.ThinkingLevel]*string{core.ThinkingHigh: &on}
	r := base
	r.ThinkingLevel = core.ThinkingMax
	_, w := build(t, m, r)
	if w.Think == nil || !*w.Think {
		t.Fatalf("think = %v for max on a row that prices high, want true (clamped down)", w.Think)
	}

	m.ThinkingLevelMap = map[core.ThinkingLevel]*string{core.ThinkingHigh: nil}
	raw, w := build(t, m, r)
	if w.Think != nil {
		t.Fatalf("think = %v on a row that prices no level, want the key omitted: %s", *w.Think, raw)
	}
}

// ---------------------------------------------------------------- streaming

// drive runs one streamed turn against a canned NDJSON body with no server
// (NFR-TEST-01) and returns the message and the URL the transport saw.
func drive(t *testing.T, env map[string]string, ndjson string) (*core.AssistantMessage, string) {
	t.Helper()
	var url string
	req := core.Request{
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		Options: core.RequestOptions{
			Env: env,
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				url = r.URL.String()
				return &http.Response{StatusCode: 200, Header: http.Header{},
					Body: io.NopCloser(strings.NewReader(ndjson))}, nil
			}),
		},
	}
	msg := ollama.Provider(ollama.Options{Getenv: func(string) string { return "" }}).
		Stream(context.Background(), model(), req, core.ProviderStreamOptions{}).Result()
	return msg, url
}

// TestAStreamWithoutDoneIsTruncated is REQ-PROV-04 with the case that
// matters: the connection drops after a tool call and before done:true. The
// done object is this wire's terminal signal; a stream that ends without one
// stopped before the model did, and reporting it as tool_use would execute a
// call from a turn the model never finished.
func TestAStreamWithoutDoneIsTruncated(t *testing.T) {
	cut := `{"model":"qwen3:8b","message":{"role":"assistant","content":"Hello"},"done":false}` + "\n" +
		`{"model":"qwen3:8b","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"delete_file","arguments":{"path":"/etc"}}}]},"done":false}` + "\n"
	msg, _ := drive(t, nil, cut)
	if msg.StopReason != core.StopReasonError {
		t.Fatalf("stop = %q (%q), want error: no done:true arrived, so the call must not run",
			msg.StopReason, msg.ErrorMessage)
	}
	if !strings.Contains(msg.ErrorMessage, "stream ended before") {
		t.Fatalf("error = %q, want the truncation text the REQ-PROV-14 allowlist retries on",
			msg.ErrorMessage)
	}
	if msg.Content.Text() != "Hello" {
		t.Fatalf("content = %q, want the partial text kept alongside the failure", msg.Content.Text())
	}

	msg, _ = drive(t, nil, cut+`{"model":"qwen3:8b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`+"\n")
	if msg.StopReason != core.StopReasonToolUse {
		t.Fatalf("stop = %q (%q) with done:true, want tool_use", msg.StopReason, msg.ErrorMessage)
	}
}

// TestThinkingIsMarkedAndReplayedUnderItsOwnKeyNeverAsText: the wire carries
// no signature, so the decoder's unsigned block was demoted to text by
// REQ-PROV-11 rule 4 and the model's reasoning re-sent as its own visible
// prose on every later turn. Marked, it survives rule 4 on a same-model replay
// and goes back under `thinking`, the field the server documents for it.
func TestThinkingIsMarkedAndReplayedUnderItsOwnKeyNeverAsText(t *testing.T) {
	stream := `{"model":"qwen3:8b","message":{"role":"assistant","content":"","thinking":"let me "},"done":false}` + "\n" +
		`{"model":"qwen3:8b","message":{"role":"assistant","content":"","thinking":"think"},"done":false}` + "\n" +
		`{"model":"qwen3:8b","message":{"role":"assistant","content":"Hello"},"done":true,"done_reason":"stop"}` + "\n"
	msg, _ := drive(t, nil, stream)
	var think *core.ThinkingBlock
	for _, b := range msg.Content {
		if tb, ok := b.(core.ThinkingBlock); ok {
			think = &tb
		}
	}
	if think == nil || think.Thinking != "let me think" {
		t.Fatalf("thinking = %+v, want the deltas accumulated", think)
	}
	if think.Signature != ollama.ThinkingSignature {
		t.Fatalf("signature = %q, want the marker that keeps rule 4 from demoting it", think.Signature)
	}

	replay := core.Request{Messages: core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}, *msg,
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "more"}}},
	}}
	_, w := build(t, model(), replay)
	assistant := w.Messages[1]
	if assistant.Thinking != "let me think" {
		t.Fatalf("assistant.thinking = %q, want the reasoning replayed under its own key", assistant.Thinking)
	}
	if *assistant.Content != "Hello" {
		t.Fatalf("assistant.content = %q: the reasoning must never ride as answer text", *assistant.Content)
	}

	// Cross-model, rule 3 downgrades it to text: another model sees context.
	other := model()
	other.ID = "llama3"
	_, w = build(t, other, replay)
	if assistant := w.Messages[1]; assistant.Thinking != "" || !strings.Contains(*assistant.Content, "let me think") {
		t.Fatalf("on ANOTHER model the reasoning must degrade to text (rule 3): %+v", assistant)
	}
}

// TestSynthesizedCallIDsAreScopedToTheResponse: the fallback minted
// "<model>-0" on every turn, and the repair pass — which keys results by id —
// could answer one turn's call with another's result. created_at is the
// response's own identity on this wire.
func TestSynthesizedCallIDsAreScopedToTheResponse(t *testing.T) {
	at := func(ts string) string {
		return `{"model":"qwen3:8b","created_at":"` + ts + `","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"t","arguments":{}}}]},"done":true,"done_reason":"stop"}` + "\n"
	}
	a, _ := drive(t, nil, at("2026-01-01T12:00:00.000000001Z"))
	b, _ := drive(t, nil, at("2026-01-01T12:00:00.000000002Z"))
	ida, idb := core.ExtractToolUse(a)[0].ID, core.ExtractToolUse(b)[0].ID
	if ida == idb {
		t.Fatalf("two responses minted the same id %q", ida)
	}
	if !strings.HasPrefix(ida, "2026-01-01T12:00:00.000000001Z-") {
		t.Fatalf("id = %q, want it scoped to the response's created_at", ida)
	}
	// The whole-response path agrees with the streaming one for the same
	// response (REQ-PROV-17): same created_at, same id.
	whole, err := ollama.DecodeResponse(model(), []byte(at("2026-01-01T12:00:00.000000001Z")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := core.ExtractToolUse(whole)[0].ID; got != ida {
		t.Fatalf("whole-response id %q differs from the streamed %q", got, ida)
	}
}

// TestAHostWithoutASchemeGetsHTTP: OLLAMA_HOST is Ollama's own variable and its
// CLI accepts "host:port", so that is what operators export. Handed to
// net/http as-is it is not a URL, and the failure names a missing protocol
// scheme rather than the variable.
func TestAHostWithoutASchemeGetsHTTP(t *testing.T) {
	ok := `{"model":"qwen3:8b","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop"}` + "\n"
	msg, url := drive(t, map[string]string{"OLLAMA_HOST": "gpu-box:11434"}, ok)
	if msg.StopReason == core.StopReasonError {
		t.Fatalf("the turn failed: %s", msg.ErrorMessage)
	}
	if url != "http://gpu-box:11434/api/chat" {
		t.Fatalf("url = %q, want http:// prefixed onto the bare host", url)
	}
	_, url = drive(t, map[string]string{"OLLAMA_HOST": "https://ollama.example/"}, ok)
	if url != "https://ollama.example/api/chat" {
		t.Fatalf("url = %q, want an explicit scheme left alone", url)
	}
}

// TestAToolWithNoSchemaSendsAnEmptyObjectNotNull: `parameters: null` is
// rejected by the server.
func TestAToolWithNoSchemaSendsAnEmptyObjectNotNull(t *testing.T) {
	_, w := build(t, model(), core.Request{
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		Tools:    []core.ToolWire{{Name: "ping", Description: "no arguments"}},
	})
	var params map[string]any
	if err := json.Unmarshal(w.Tools[0].Function.Parameters, &params); err != nil || params["type"] != "object" {
		t.Fatalf("parameters = %s, want an empty object schema, never null", w.Tools[0].Function.Parameters)
	}
}
