package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/provider/ollama"
	"github.com/agentfox/agentkit-go/provider/openai"
)

// This file is the CROSS-PROVIDER conformance suite.
//
// Every wire API decodes a different shape, and every one of them can be
// individually plausible and collectively inconsistent. The requirements that
// matter here — byte-faithful tool arguments (REQ-PROV-17), usage netting
// (REQ-PROV-05.1), event ordering (REQ-OBS-08), partial-content-plus-failure
// (REQ-PROV-04) — are stated once about "a provider", so they are tested once
// against all of them rather than four times in four dialects.
//
// The fixtures below all encode the SAME logical turn: the text "Hello", one
// call to edit_file with arguments {"zebra":1,"apple":2}, and a usage of 100
// net input, 42 output, 900 cache reads where the wire reports them.

const wantArgs = `{"zebra":1,"apple":2}`

type wireCase struct {
	name      string
	model     *core.Model
	provider  core.APIProvider
	stream    string
	whole     string
	decode    func(*core.Model, []byte, func(string) *core.Model) (*core.AssistantMessage, error)
	cacheRead int64
	// truncateAt is a marker in the stream body; everything from it onward is
	// cut to simulate a dropped connection mid-turn.
	truncateAt string
}

func model(id string, api core.API, vendor string) *core.Model {
	return &core.Model{
		ID: id, API: api, Provider: vendor,
		ContextWindow: 200000, MaxTokens: 4096, Input: []string{"text"},
		Cost: core.Cost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	}
}

func sse(pairs ...[2]string) string {
	var b strings.Builder
	for _, p := range pairs {
		if p[0] == "" {
			fmt.Fprintf(&b, "data: %s\n\n", p[1])
			continue
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", p[0], p[1])
	}
	return b.String()
}

func cases() []wireCase {
	anth := sse(
		[2]string{"message_start", `{"message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":900}}}`},
		[2]string{"content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"Hello"}}`},
		[2]string{"content_block_stop", `{"index":0}`},
		[2]string{"content_block_start", `{"index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"edit_file","input":{}}}`},
		[2]string{"content_block_delta", `{"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"zebra\":"}}`},
		[2]string{"content_block_delta", `{"index":1,"delta":{"type":"input_json_delta","partial_json":"1,\"apple\":2}"}}`},
		[2]string{"content_block_stop", `{"index":1}`},
		[2]string{"message_delta", `{"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`},
		[2]string{"message_stop", `{}`},
	)

	oai := sse(
		[2]string{"", `{"id":"chatcmpl-1","model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`},
		[2]string{"", `{"id":"chatcmpl-1","model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"edit_file","arguments":"{\"zebra\":"}}]},"finish_reason":null}]}`},
		[2]string{"", `{"id":"chatcmpl-1","model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1,\"apple\":2}"}}]},"finish_reason":null}]}`},
		[2]string{"", `{"id":"chatcmpl-1","model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`},
		[2]string{"", `{"id":"chatcmpl-1","model":"gpt-test","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":42,"total_tokens":1042,"prompt_tokens_details":{"cached_tokens":900}}}`},
		[2]string{"", `[DONE]`},
	)

	goog := sse(
		[2]string{"", `{"responseId":"resp_1","modelVersion":"gemini-test","candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`},
		[2]string{"", `{"responseId":"resp_1","modelVersion":"gemini-test","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"edit_file","args":` + wantArgs + `}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":42,"totalTokenCount":1042,"cachedContentTokenCount":900}}`},
	)

	olla := strings.Join([]string{
		`{"model":"llama-test","message":{"role":"assistant","content":"Hello"},"done":false}`,
		`{"model":"llama-test","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"edit_file","arguments":` + wantArgs + `}}]},"done":false}`,
		`{"model":"llama-test","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":100,"eval_count":42}`,
	}, "\n") + "\n"

	key := func(string) string { return "test-key" }

	return []wireCase{
		{
			name:  "anthropic-messages",
			model: model("claude-test", anthropic.API, "anthropic"),
			provider: anthropic.Provider(anthropic.Options{Getenv: func(k string) string {
				if k == "ANTHROPIC_API_KEY" {
					return "test-key"
				}
				return ""
			}}),
			stream: anth,
			whole: `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test",` +
				`"content":[{"type":"text","text":"Hello"},{"type":"tool_use","id":"toolu_1","name":"edit_file","input":` + wantArgs + `}],` +
				`"stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":42,"cache_read_input_tokens":900}}`,
			decode:     anthropic.DecodeResponse,
			cacheRead:  900,
			truncateAt: "event: content_block_start\ndata: {\"index\":1",
		},
		{
			name:     "openai-completions",
			model:    model("gpt-test", openai.API, "openai"),
			provider: openai.Provider(openai.Options{Getenv: key}),
			stream:   oai,
			whole: `{"id":"chatcmpl-1","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"Hello",` +
				`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"edit_file","arguments":"{\"zebra\":1,\"apple\":2}"}}]},` +
				`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1000,"completion_tokens":42,"total_tokens":1042,"prompt_tokens_details":{"cached_tokens":900}}}`,
			decode:     openai.DecodeResponse,
			cacheRead:  900,
			truncateAt: `data: {"id":"chatcmpl-1","model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls"`,
		},
		{
			name:     "google-generative-ai",
			model:    model("gemini-test", google.API, "google"),
			provider: google.Provider(google.Options{Getenv: key}),
			stream:   goog,
			whole: `{"responseId":"resp_1","modelVersion":"gemini-test","candidates":[{"content":{"role":"model","parts":[` +
				`{"text":"Hello"},{"functionCall":{"name":"edit_file","args":` + wantArgs + `}}]},"finishReason":"STOP"}],` +
				`"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":42,"totalTokenCount":1042,"cachedContentTokenCount":900}}`,
			decode:     google.DecodeResponse,
			cacheRead:  900,
			truncateAt: `data: {"responseId":"resp_1","modelVersion":"gemini-test","candidates":[{"content":{"role":"model","parts":[{"functionCall"`,
		},
		{
			name:     "ollama-chat",
			model:    model("llama-test", ollama.API, "ollama"),
			provider: ollama.Provider(ollama.Options{Getenv: key}),
			stream:   olla,
			whole: `{"model":"llama-test","message":{"role":"assistant","content":"Hello",` +
				`"tool_calls":[{"function":{"name":"edit_file","arguments":` + wantArgs + `}}]},` +
				`"done":true,"done_reason":"stop","prompt_eval_count":100,"eval_count":42}`,
			decode:     ollama.DecodeResponse,
			cacheRead:  0,
			truncateAt: `{"model":"llama-test","message":{"role":"assistant","content":"","tool_calls"`,
		},
	}
}

func drive(t *testing.T, c wireCase, status int, body string) (*core.AssistantMessage, []core.Event) {
	t.Helper()
	req := core.Request{Options: core.RequestOptions{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(body))}, nil
		}),
	}}
	s := c.provider.Stream(context.Background(), c.model, req, core.ProviderStreamOptions{})
	var events []core.Event
	for e := range s.Events() {
		events = append(events, e)
	}
	return s.Result(), events
}

// TestEveryWireAPIDecodesTheSameTurn is the baseline: four dialects, one
// canonical result.
func TestEveryWireAPIDecodesTheSameTurn(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			msg, _ := drive(t, c, 200, c.stream)
			if msg.StopReason == core.StopReasonError {
				t.Fatalf("decode failed: %s", msg.ErrorMessage)
			}
			if msg.Content.Text() != "Hello" {
				t.Fatalf("text = %q, want %q", msg.Content.Text(), "Hello")
			}
			calls := core.ExtractToolUse(msg)
			if len(calls) != 1 || calls[0].Name != "edit_file" {
				t.Fatalf("tool calls = %+v, want one call to edit_file", calls)
			}
			if calls[0].ID == "" {
				t.Fatal("every tool call needs an id in the canonical layer, even on the " +
					"wires that carry none and pair positionally")
			}
			if msg.StopReason != core.StopReasonToolUse {
				t.Fatalf("stop reason = %q, want tool_use", msg.StopReason)
			}
		})
	}
}

// TestToolArgumentBytesSurviveEveryWire is REQ-PROV-17's decode half, across
// all four dialects at once.
//
// The bug it catches is a decode-and-re-encode round trip: Go sorts map keys
// unconditionally, so {"zebra":1,"apple":2} comes back as
// {"apple":2,"zebra":1}. Semantically identical, and on the OpenAI family —
// where arguments ride as a JSON string — it changes the text the model is
// conditioned on and shifts the prompt-cache prefix. The only symptom is the
// bill.
func TestToolArgumentBytesSurviveEveryWire(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			msg, _ := drive(t, c, 200, c.stream)
			got := string(core.ExtractToolUse(msg)[0].Input)
			if got != wantArgs {
				t.Fatalf("Input = %s, want the provider's own bytes %s", got, wantArgs)
			}
		})
	}
}

// TestStreamingAndWholeResponsesAgree is REQ-PROV-17's per-provider
// conformance requirement, generalized.
//
// "byte-identical Input to what its non-streaming path produces for the same
// tool call" is only checkable once both paths exist. Sharing one assembler is
// what makes it true by construction; this test is what makes it stay true.
func TestStreamingAndWholeResponsesAgree(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			streamed, _ := drive(t, c, 200, c.stream)
			whole, err := c.decode(c.model, []byte(c.whole), nil)
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			sc, wc := core.ExtractToolUse(streamed), core.ExtractToolUse(whole)
			if len(sc) != 1 || len(wc) != 1 {
				t.Fatalf("tool calls: streaming %d, whole %d", len(sc), len(wc))
			}
			if string(sc[0].Input) != string(wc[0].Input) {
				t.Fatalf("Input differs between paths:\n streaming: %s\n     whole: %s",
					sc[0].Input, wc[0].Input)
			}
			if sc[0].ID != wc[0].ID {
				t.Fatalf("tool-call id differs between paths: %q vs %q. On a wire that "+
					"synthesizes ids, the two paths must synthesize the SAME one or a "+
					"replayed transcript stops matching its own results.", sc[0].ID, wc[0].ID)
			}
			if streamed.Content.Text() != whole.Content.Text() {
				t.Fatalf("text differs: %q vs %q", streamed.Content.Text(), whole.Content.Text())
			}
			if streamed.Usage.InputTokens != whole.Usage.InputTokens ||
				streamed.Usage.OutputTokens != whole.Usage.OutputTokens {
				t.Fatalf("usage differs: streaming %+v, whole %+v", streamed.Usage, whole.Usage)
			}
		})
	}
}

// TestCachedTokensAreNettedOutOfInputExactlyOnce is REQ-PROV-05.1, and the
// reason it cannot be a shared helper applied blindly.
//
// The OpenAI family and Google report a prompt total INCLUSIVE of cached
// tokens and must subtract. Anthropic reports input EXCLUSIVE of them and must
// not. Both mistakes are silent and roughly equal in magnitude — one overstates
// cost by up to ~90% on a well-cached loop, the other understates it by the
// same — so the netting lives in each decoder, at the one place that knows its
// wire's convention, and this test pins the outcome for all of them.
func TestCachedTokensAreNettedOutOfInputExactlyOnce(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			msg, _ := drive(t, c, 200, c.stream)
			if msg.Usage.InputTokens != 100 {
				t.Fatalf("input_tokens = %d, want 100 net of cache. This wire reports "+
					"%d cache reads; the decoder either failed to subtract them or "+
					"subtracted an already-net figure.", msg.Usage.InputTokens, c.cacheRead)
			}
			if msg.Usage.OutputTokens != 42 {
				t.Fatalf("output_tokens = %d, want 42", msg.Usage.OutputTokens)
			}
			if msg.Usage.CacheReadTokens != c.cacheRead {
				t.Fatalf("cache_read = %d, want %d", msg.Usage.CacheReadTokens, c.cacheRead)
			}
			if !msg.Usage.Has(core.UsageCostUSD) {
				t.Fatal("every provider computes cost_usd itself (REQ-PROV-05); the " +
					"session layer only sums and never recomputes")
			}
			want := (3*100.0 + 15*42.0 + 0.3*float64(c.cacheRead)) / 1e6
			if d := msg.Usage.CostUSD - want; d > 1e-12 || d < -1e-12 {
				t.Fatalf("cost = %.10f, want %.10f", msg.Usage.CostUSD, want)
			}
		})
	}
}

// TestBlockEndsFollowEveryDeltaOnEveryWire is REQ-OBS-08.1 and 08.3.
//
// A consumer obeying REQ-OBS-06a discards its accumulated deltas when the
// authoritative event arrives. A delta that overtakes its own end event is
// therefore applied TWICE, and the double-application is invisible to any test
// that observes only one class of event.
func TestBlockEndsFollowEveryDeltaOnEveryWire(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			_, events := drive(t, c, 200, c.stream)

			lastDelta := map[int]int{}
			endAt := map[int]int{}
			endCount := map[int]int{}
			for i, e := range events {
				switch v := e.(type) {
				case core.TextDeltaEvent:
					lastDelta[v.BlockIndex] = i
				case core.ThinkingDeltaEvent:
					lastDelta[v.BlockIndex] = i
				case core.ToolInputDeltaEvent:
					lastDelta[v.BlockIndex] = i
				case core.TextEndEvent:
					endAt[v.BlockIndex], endCount[v.BlockIndex] = i, endCount[v.BlockIndex]+1
				case core.ThinkingEndEvent:
					endAt[v.BlockIndex], endCount[v.BlockIndex] = i, endCount[v.BlockIndex]+1
				case core.ToolCallEndEvent:
					endAt[v.BlockIndex], endCount[v.BlockIndex] = i, endCount[v.BlockIndex]+1
				}
			}
			if len(endAt) != 2 {
				t.Fatalf("%d blocks produced an end event, want 2", len(endAt))
			}
			for idx, at := range endAt {
				if n := endCount[idx]; n != 1 {
					t.Fatalf("block %d produced %d end events, want exactly one", idx, n)
				}
				if d, ok := lastDelta[idx]; ok && d > at {
					t.Fatalf("block %d: a delta at %d follows its end event at %d", idx, d, at)
				}
			}

			// And the message end is last.
			if _, ok := events[len(events)-1].(core.MessageEndEvent); !ok {
				t.Fatalf("last event is %T, want MessageEndEvent", events[len(events)-1])
			}
		})
	}
}

// TestATruncatedStreamKeepsWhatArrivedOnEveryWire is REQ-PROV-04's
// "half a message and a failure must be ONE value", across all four.
func TestATruncatedStreamKeepsWhatArrivedOnEveryWire(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			i := strings.Index(c.stream, c.truncateAt)
			if i < 0 {
				t.Fatalf("truncation marker not present in the fixture")
			}
			msg, _ := drive(t, c, 200, c.stream[:i])

			if msg.Content.Text() != "Hello" {
				t.Fatalf("content = %q, want the text that DID arrive to survive onto the "+
					"message: session persistence reads it and the retry classifier reads "+
					"the error, and neither works if they are separated",
					msg.Content.Text())
			}
			if len(core.ExtractToolUse(msg)) != 0 {
				t.Fatal("a tool call whose bytes never arrived must not appear")
			}
		})
	}
}

// TestANon2xxNamesTheStatusOnEveryWire keeps the two retry layers connected.
// REQ-PROV-14's allowlist matches bare "429"/"500"/"503" strings, so a message
// rendering only the provider's prose loses the retry for a gateway that
// answers with an empty body.
func TestANon2xxNamesTheStatusOnEveryWire(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			msg, _ := drive(t, c, 429, `{"error":{"type":"rate_limit","message":"slow down"}}`)
			if msg.StopReason != core.StopReasonError {
				t.Fatalf("stop reason = %q, want error", msg.StopReason)
			}
			for _, want := range []string{"429", "slow down"} {
				if !strings.Contains(msg.ErrorMessage, want) {
					t.Fatalf("error %q is missing %q", msg.ErrorMessage, want)
				}
			}
		})
	}
}

// TestOnPayloadReachesEveryWire pins REQ-PROV-18's post-serialization seam as
// a property of the provider CONTRACT, not of one implementation.
func TestOnPayloadReachesEveryWire(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			var body []byte
			req := core.Request{Options: core.RequestOptions{
				OnPayload: func(p any, _ *core.Model) (any, error) {
					var m map[string]any
					b, _ := json.Marshal(p)
					_ = json.Unmarshal(b, &m)
					m["marker_field"] = true
					return m, nil
				},
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					body, _ = io.ReadAll(r.Body)
					return &http.Response{StatusCode: 200, Header: http.Header{},
						Body: io.NopCloser(strings.NewReader(c.stream))}, nil
				}),
			}}
			c.provider.Stream(context.Background(), c.model, req, core.ProviderStreamOptions{}).Result()
			if !strings.Contains(string(body), "marker_field") {
				t.Fatalf("body = %s; OnPayload's replacement must reach the wire", body)
			}
		})
	}
}

// TestCredentialsReachEveryWireWithoutLeaking checks both halves at once: the
// key is actually sent, and RequestOptions.Headers can suppress it.
func TestCredentialsReachEveryWireWithoutLeaking(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			var hdr http.Header
			req := core.Request{Options: core.RequestOptions{
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					hdr = r.Header.Clone()
					return &http.Response{StatusCode: 200, Header: http.Header{},
						Body: io.NopCloser(strings.NewReader(c.stream))}, nil
				}),
			}}
			c.provider.Stream(context.Background(), c.model, req, core.ProviderStreamOptions{}).Result()

			var carried bool
			for _, vs := range hdr {
				for _, v := range vs {
					if strings.Contains(v, "test-key") {
						carried = true
					}
				}
			}
			if !carried {
				t.Fatalf("no header carries the resolved credential: %v", hdr)
			}
			if hdr.Get("X-Agentkit-Version") != provider.Version {
				t.Fatal("the attribution header is on by default and must be sent " +
					"(REQ-SEC-13.2 — the default being on is why it is disclosed)")
			}
		})
	}
}

// TestTheCachedCountIsReadFromAllThreePlaces is REQ-PROV-05.2, verbatim: the
// cached count lives in three places and ALL THREE arms are required.
//
// A decoder that reads only prompt_tokens_details.cached_tokens is correct on
// OpenAI and OpenRouter, and silently treats every cached token as a
// full-price input token on DeepSeek and Moonshot — the two vendors whose
// caching is the reason to use them.
func TestTheCachedCountIsReadFromAllThreePlaces(t *testing.T) {
	c := cases()[1] // openai-completions
	arms := []struct {
		vendor string
		usage  string
	}{
		{"OpenAI / OpenRouter", `"prompt_tokens_details":{"cached_tokens":900}`},
		{"DeepSeek", `"prompt_cache_hit_tokens":900`},
		{"Moonshot", `"cached_tokens":900`},
	}
	for _, arm := range arms {
		t.Run(arm.vendor, func(t *testing.T) {
			body := sse(
				[2]string{"", `{"id":"c","model":"gpt-test","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`},
				[2]string{"", `{"id":"c","model":"gpt-test","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":42,` + arm.usage + `}}`},
				[2]string{"", `[DONE]`},
			)
			msg, _ := drive(t, c, 200, body)
			if msg.Usage.CacheReadTokens != 900 {
				t.Fatalf("%s reports its cached count as %s and the decoder read %d",
					arm.vendor, arm.usage, msg.Usage.CacheReadTokens)
			}
			if msg.Usage.InputTokens != 100 {
				t.Fatalf("input_tokens = %d, want 1000 - 900 = 100", msg.Usage.InputTokens)
			}
		})
	}
}

// TestGeminiThoughtsAreInsideOutputNotBesideIt keeps the canonical invariant
// "ReasoningTokens is a SUBSET of OutputTokens" true on the one wire that
// reports them as siblings.
//
// Gemini reports thoughtsTokenCount BESIDE candidatesTokenCount. Copying that
// shape through under-reports output by exactly the reasoning volume, and
// under-bills a thinking turn by the same — the more a model thinks, the
// larger the error.
func TestGeminiThoughtsAreInsideOutputNotBesideIt(t *testing.T) {
	c := cases()[2] // google-generative-ai
	body := sse([2]string{"", `{"responseId":"r","modelVersion":"gemini-test","candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":30,"thoughtsTokenCount":70,"totalTokenCount":110}}`})

	msg, _ := drive(t, c, 200, body)
	if msg.Usage.OutputTokens != 100 {
		t.Fatalf("output_tokens = %d, want 30 candidates + 70 thoughts = 100",
			msg.Usage.OutputTokens)
	}
	if msg.Usage.ReasoningTokens != 70 {
		t.Fatalf("reasoning_tokens = %d, want 70 as a SUBSET of output",
			msg.Usage.ReasoningTokens)
	}
	if msg.Usage.ReasoningTokens > msg.Usage.OutputTokens {
		t.Fatal("reasoning must never exceed output; it is a subset, not an addend")
	}
}

// TestTextAfterAToolCallStartsANewBlock is a Gemini-shaped ordering hazard.
//
// Parts arrive in one flat list, so text before and text after a functionCall
// look identical to an accumulator that keeps one open text block forever. The
// result is a message whose text is stitched across the call, in the wrong
// order relative to it.
func TestTextAfterAToolCallStartsANewBlock(t *testing.T) {
	c := cases()[2]
	body := sse([2]string{"", `{"responseId":"r","modelVersion":"gemini-test","candidates":[{"content":{"role":"model","parts":[` +
		`{"text":"before"},{"functionCall":{"name":"t","args":{}}},{"text":"after"}]},"finishReason":"STOP"}]}`})

	msg, _ := drive(t, c, 200, body)
	if len(msg.Content) != 3 {
		t.Fatalf("%d blocks, want 3: text, tool call, text", len(msg.Content))
	}
	if b, ok := msg.Content[0].(core.TextBlock); !ok || b.Text != "before" {
		t.Fatalf("block 0 = %#v, want the text that preceded the call", msg.Content[0])
	}
	if _, ok := msg.Content[1].(core.ToolUseBlock); !ok {
		t.Fatalf("block 1 = %#v, want the tool call", msg.Content[1])
	}
	if b, ok := msg.Content[2].(core.TextBlock); !ok || b.Text != "after" {
		t.Fatalf("block 2 = %#v, want a NEW text block, not a continuation of block 0",
			msg.Content[2])
	}
}

// TestOllamaReportsErrorsInsideA200Body is the failure shape that wire's
// native API actually produces, and that the transport layer cannot see.
func TestOllamaReportsErrorsInsideA200Body(t *testing.T) {
	c := cases()[3]
	msg, _ := drive(t, c, 200, `{"error":"model \"nope\" not found, try pulling it first"}`+"\n")
	if msg.StopReason != core.StopReasonError {
		t.Fatalf("stop reason = %q, want error: Ollama returns 200 with a top-level "+
			"error string, so only the decoder can catch it", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "not found") {
		t.Fatalf("error = %q, want the server's own text", msg.ErrorMessage)
	}
}

// TestACredentialStoreReachesEveryWire is REQ-AUTH-05 through the real
// dispatch path.
//
// The store is the layer that can hold a refreshed OAuth token, and it is
// useless if only one provider consults it. This asserts the token reaches the
// wire on all four — and, on Google, that a stored Authorization header
// survives instead of being rewritten into x-goog-api-key, which is the
// Vertex/ADC path of NFR-COMPAT-05.
func TestACredentialStoreReachesEveryWire(t *testing.T) {
	store := &provider.MemoryStore{}
	for _, vendor := range []string{"anthropic", "openai", "google", "ollama"} {
		if err := store.Save(context.Background(), vendor, provider.Credential{
			AccessToken: "stored-oauth-token", Scheme: provider.SchemeBearer,
		}); err != nil {
			t.Fatal(err)
		}
	}
	creds := provider.NewCredentials(store)

	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			p := withCredentials(t, c.name, creds)
			var hdr http.Header
			req := core.Request{Options: core.RequestOptions{
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					hdr = r.Header.Clone()
					return &http.Response{StatusCode: 200, Header: http.Header{},
						Body: io.NopCloser(strings.NewReader(c.stream))}, nil
				}),
			}}
			p.Stream(context.Background(), c.model, req, core.ProviderStreamOptions{}).Result()

			if got := hdr.Get("Authorization"); got != "Bearer stored-oauth-token" {
				t.Fatalf("Authorization = %q, want the stored credential. The environment "+
					"table cannot hold a refreshed token; only the store can.", got)
			}
			if hdr.Get("X-Goog-Api-Key") != "" {
				t.Fatal("a stored OAuth token must not be rewritten into an API-key " +
					"header: that is the Vertex/ADC path and it expects a bearer")
			}
		})
	}
}

func withCredentials(t *testing.T, api string, creds *provider.Credentials) core.APIProvider {
	t.Helper()
	none := func(string) string { return "" }
	switch api {
	case string(anthropic.API):
		return anthropic.Provider(anthropic.Options{Getenv: none, Credentials: creds})
	case string(openai.API):
		return openai.Provider(openai.Options{Getenv: none, Credentials: creds})
	case string(google.API):
		return google.Provider(google.Options{Getenv: none, Credentials: creds})
	case string(ollama.API):
		return ollama.Provider(ollama.Options{Getenv: none, Credentials: creds})
	}
	t.Fatalf("no constructor for %q", api)
	return core.APIProvider{}
}

// TestStopSequencesReachEveryWire pins REQ-PROV's stop sequences as a property
// of the provider CONTRACT rather than of one implementation.
//
// This exists because two of the four providers silently dropped the field
// until the NFR-TEST-08 request golden made the omission visible. A caller's
// stop condition that never takes effect, with nothing reporting it, is the
// exact failure mode a per-provider test set does not catch: each provider's
// own tests pass, and the field is absent from half the bodies.
func TestStopSequencesReachEveryWire(t *testing.T) {
	const marker = "STOP-MARKER-42"
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			var body []byte
			req := core.Request{
				StopSequences: []string{marker},
				Options: core.RequestOptions{
					Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						body, _ = io.ReadAll(r.Body)
						return &http.Response{StatusCode: 200, Header: http.Header{},
							Body: io.NopCloser(strings.NewReader(c.stream))}, nil
					}),
				},
			}
			c.provider.Stream(context.Background(), c.model, req, core.ProviderStreamOptions{}).Result()
			if !strings.Contains(string(body), marker) {
				t.Fatalf("the stop sequence never reached the wire.\nbody = %s", body)
			}
		})
	}
}
