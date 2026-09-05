package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/faux"
)

// ------------------------------------------------------------------ harness

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// sseBody renders (event, data) pairs as a text/event-stream body.
func sseBody(pairs ...[2]string) string {
	var b strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", p[0], p[1])
	}
	return b.String()
}

func testModel() *core.Model {
	return &core.Model{
		ID: "claude-test", Name: "Claude Test", API: anthropic.API, Provider: "anthropic",
		ContextWindow: 200000, MaxTokens: 4096,
		Input: []string{"text"}, Reasoning: true,
		Cost: core.Cost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	}
}

// run drives the provider against a canned body and collects every event.
func run(t *testing.T, m *core.Model, req core.Request, opts anthropic.Options,
	status int, body string) (*core.AssistantMessage, []core.Event, *http.Request) {
	t.Helper()

	var seen *http.Request
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Clone(r.Context())
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			seen.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		return &http.Response{StatusCode: status, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	req.Options.Transport = rt
	if opts.Getenv == nil {
		opts.Getenv = func(k string) string {
			if k == "ANTHROPIC_API_KEY" {
				return "sk-ant-test-key-abcdefgh"
			}
			return ""
		}
	}

	s := anthropic.Provider(opts).Stream(context.Background(), m, req, core.ProviderStreamOptions{})
	var events []core.Event
	for e := range s.Events() {
		events = append(events, e)
	}
	return s.Result(), events, seen
}

func names(events []core.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, string(e.EventType()))
	}
	return out
}

// collapse folds runs of the same event type into one entry, so a sequence
// with two text deltas compares equal to one with a single delta. Delta counts
// are an optimization detail (REQ-OBS-06a); their ORDER against the
// authoritative events is not.
func collapse(in []string) []string {
	var out []string
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// ------------------------------------------------------------------ fixtures

const toolArgs = `{"zebra":1,"apple":2}`

func streamFixture() string {
	return sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":900,"cache_creation_input_tokens":50}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"edit_file","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"zebra\":"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1,\"apple\":2}"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
}

// wholeFixture is the SAME turn as streamFixture, non-streaming.
const wholeFixture = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test",` +
	`"content":[{"type":"text","text":"Hello"},` +
	`{"type":"tool_use","id":"toolu_1","name":"edit_file","input":` + toolArgs + `}],` +
	`"stop_reason":"tool_use","stop_sequence":null,` +
	`"usage":{"input_tokens":100,"output_tokens":42,"cache_read_input_tokens":900,"cache_creation_input_tokens":50}}`

// ------------------------------------------------------------------ tests

// TestTheEventOrderMatchesFaux is the conformance test faux's package comment
// promises: "a real provider that disagrees with it is the one that is wrong".
//
// faux is the executable specification of the streaming protocol, so pinning
// the real decoder against a golden string would pin it against a second
// opinion. Pinning it against faux pins it against the specification.
func TestTheEventOrderMatchesFaux(t *testing.T) {
	_, events, _ := run(t, testModel(), core.Request{}, anthropic.Options{}, 200, streamFixture())

	call := faux.FauxToolCall("toolu_1", "edit_file", toolArgs)
	fp := faux.New(faux.FauxAssistantMessage(core.StopReasonToolUse,
		faux.FauxText("Hello"), call))
	fs := fp.Stream(context.Background(), faux.Model(), core.Request{}, core.ProviderStreamOptions{})
	var fauxEvents []core.Event
	for e := range fs.Events() {
		fauxEvents = append(fauxEvents, e)
	}

	got := strings.Join(collapse(names(events)), ",")
	want := strings.Join(collapse(names(fauxEvents)), ",")
	if got != want {
		t.Fatalf("event order\n got: %s\nwant: %s\n\nfaux is the normative protocol; a "+
			"real provider that disagrees with it is the one that is wrong.", got, want)
	}
}

// TestBlockEndEventsFollowTheWholeStream is REQ-OBS-08.3.
//
// Emitting a block end from the per-chunk handler is the obvious
// implementation. It produces duplicate ends, and — worse — a message end that
// lacks usage, because usage arrives with the terminal chunk. Both are
// invisible to a test that only checks the final message.
func TestBlockEndEventsFollowTheWholeStream(t *testing.T) {
	_, events, _ := run(t, testModel(), core.Request{}, anthropic.Options{}, 200, streamFixture())

	seq := names(events)
	lastDelta, firstEnd := -1, len(seq)
	ends := 0
	for i, n := range seq {
		switch n {
		case string(core.EvTextDelta), string(core.EvToolInputDelta):
			lastDelta = i
		case string(core.EvTextEnd), string(core.EvToolCallEnd):
			ends++
			if i < firstEnd {
				firstEnd = i
			}
		}
	}
	if ends != 2 {
		t.Fatalf("%d block-end events, want exactly 2 (one per block, once each)", ends)
	}
	if firstEnd < lastDelta {
		t.Fatalf("a block-end at %d precedes the last delta at %d: block ends are emitted "+
			"after the stream has fully ended, in block order", firstEnd, lastDelta)
	}
}

// TestStreamingAndWholeResponseAgreeByteForByte is REQ-PROV-17's per-provider
// conformance test, which could not be written until a decoder existed.
//
// The failure it catches is a decode-and-re-encode round trip on the argument
// bytes. Go sorts map keys unconditionally, so `{"zebra":1,"apple":2}` comes
// back as `{"apple":2,"zebra":1}` — semantically identical, and on OpenAI-style
// wires where arguments ride as a JSON string it changes the text the model is
// conditioned on and shifts the prompt-cache prefix. The only symptom is the
// bill.
func TestStreamingAndWholeResponseAgreeByteForByte(t *testing.T) {
	m := testModel()
	streamed, _, _ := run(t, m, core.Request{}, anthropic.Options{}, 200, streamFixture())
	whole, err := anthropic.DecodeResponse(m, []byte(wholeFixture), nil)
	if err != nil {
		t.Fatal(err)
	}

	sTool := core.ExtractToolUse(streamed)
	wTool := core.ExtractToolUse(whole)
	if len(sTool) != 1 || len(wTool) != 1 {
		t.Fatalf("tool calls: streamed %d, whole %d", len(sTool), len(wTool))
	}
	if string(sTool[0].Input) != string(wTool[0].Input) {
		t.Fatalf("Input differs between paths:\n streaming: %s\n     whole: %s",
			sTool[0].Input, wTool[0].Input)
	}
	if string(sTool[0].Input) != toolArgs {
		t.Fatalf("Input = %s, want the provider's own bytes %s. Key order is not "+
			"cosmetic: re-encoding sorts it and shifts the cache prefix.",
			sTool[0].Input, toolArgs)
	}
	if streamed.Content.Text() != whole.Content.Text() {
		t.Fatalf("text differs: %q vs %q", streamed.Content.Text(), whole.Content.Text())
	}
}

// TestAnthropicInputTokensAreNotNettedAgain is REQ-PROV-05.1 read correctly.
//
// The requirement's subtraction is an OPENAI-FAMILY convention: prompt_tokens
// there includes cached tokens. Anthropic's input_tokens already excludes both
// cache_read_input_tokens and cache_creation_input_tokens, so applying the
// subtraction here understates input by the full cached amount — the same
// magnitude of error as the trap, in the other direction, and just as silent.
func TestAnthropicInputTokensAreNotNettedAgain(t *testing.T) {
	msg, _, _ := run(t, testModel(), core.Request{}, anthropic.Options{}, 200, streamFixture())

	u := msg.Usage
	if u.InputTokens != 100 {
		t.Fatalf("input_tokens = %d, want 100 as reported: Anthropic reports input NET "+
			"of cache reads and writes already", u.InputTokens)
	}
	if u.CacheReadTokens != 900 || u.CacheWriteTokens != 50 {
		t.Fatalf("cache read/write = %d/%d, want 900/50", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.OutputTokens != 42 {
		t.Fatalf("output_tokens = %d, want the message_delta value 42 to win over the "+
			"message_start placeholder of 1", u.OutputTokens)
	}

	// (3*100 + 15*42 + 0.3*900 + 3.75*50) / 1e6
	want := (3*100.0 + 15*42.0 + 0.3*900.0 + 3.75*50.0) / 1e6
	if diff := u.CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cost = %.10f, want %.10f", u.CostUSD, want)
	}
	if !u.Has(core.UsageCostUSD) {
		t.Fatal("cost must be marked as reported, or the session aggregate cannot tell " +
			"a computed zero from an absent one")
	}
}

// TestAFallbackServedModelIsBilledAtItsOwnRates is REQ-PROV-05.5 end to end.
func TestAFallbackServedModelIsBilledAtItsOwnRates(t *testing.T) {
	served := &core.Model{ID: "claude-cheap", Cost: core.Cost{Input: 0.25, Output: 1.25}}
	body := strings.Replace(streamFixture(), `"model":"claude-test"`, `"model":"claude-cheap"`, 1)

	msg, _, _ := run(t, testModel(), core.Request{}, anthropic.Options{
		BillingLookup: func(id string) *core.Model {
			if id == "claude-cheap" {
				return served
			}
			return nil
		},
	}, 200, body)

	if msg.Usage.BilledModel != "claude-cheap" {
		t.Fatalf("billed_model = %q, want the model that actually served the request",
			msg.Usage.BilledModel)
	}
	want := (0.25*100.0 + 1.25*42.0) / 1e6
	if diff := msg.Usage.CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cost = %.10f, want the SERVED model's rates %.10f", msg.Usage.CostUSD, want)
	}
	if msg.Model != "claude-test" {
		t.Fatalf("Model = %q; provenance records the REQUESTED model, or REQ-PROV-11 "+
			"rule 1 strips this turn's own valid signatures on replay", msg.Model)
	}
}

// TestATruncatedStreamKeepsThePartialMessageAndIsRetryable is REQ-PROV-04 and
// its coupling to REQ-PROV-14.
//
// A body that simply stops is a 200 with a complete-looking prefix. The
// transport layer cannot see it. The partial content must survive onto the
// message (session persistence reads it) and the error text must be something
// the semantic classifier recognises (the retry reads that).
func TestATruncatedStreamKeepsThePartialMessageAndIsRetryable(t *testing.T) {
	full := streamFixture()
	cut := full[:strings.Index(full, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1")]

	msg, events, _ := run(t, testModel(), core.Request{}, anthropic.Options{}, 200, cut)

	if msg.StopReason != core.StopReasonError {
		t.Fatalf("stop reason = %q, want error", msg.StopReason)
	}
	if msg.Content.Text() != "Hello" {
		t.Fatalf("content = %q, want the partial text to survive: half a message and a "+
			"failure must be ONE value", msg.Content.Text())
	}
	if !strings.Contains(msg.ErrorMessage, "stream ended before message_stop") {
		t.Fatalf("error = %q, want text the semantic retry classifier matches", msg.ErrorMessage)
	}
	var sawTerminalError bool
	for _, e := range events {
		if ee, ok := e.(core.ErrorEvent); ok && ee.Terminal {
			sawTerminalError = true
		}
	}
	if !sawTerminalError {
		t.Fatal("a failed stream must carry a terminal ErrorEvent (REQ-PROV-04)")
	}
}

func TestANon2xxCarriesTheStatusCodeInTheErrorText(t *testing.T) {
	msg, _, _ := run(t, testModel(), core.Request{}, anthropic.Options{}, 429,
		`{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`)

	if msg.StopReason != core.StopReasonError {
		t.Fatalf("stop reason = %q, want error", msg.StopReason)
	}
	for _, want := range []string{"429", "rate_limit_error", "exceeded your rate limit"} {
		if !strings.Contains(msg.ErrorMessage, want) {
			t.Fatalf("error text %q is missing %q; the semantic classifier matches bare "+
				"status strings AND provider prose, so both must be present",
				msg.ErrorMessage, want)
		}
	}
}

// TestAMidStreamErrorEventFailsTheTurn covers the third failure shape: a 200
// whose body carries an SSE `error` event partway through.
func TestAMidStreamErrorEventFailsTheTurn(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"message":{"id":"m","model":"claude-test","usage":{"input_tokens":5}}}`},
		[2]string{"content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"partial"}}`},
		[2]string{"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
	)
	msg, _, _ := run(t, testModel(), core.Request{}, anthropic.Options{}, 200, body)

	if msg.StopReason != core.StopReasonError {
		t.Fatalf("stop reason = %q, want error", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "overloaded_error") {
		t.Fatalf("error = %q, want the provider's own error type", msg.ErrorMessage)
	}
}

// TestATruncatedToolCallIsRepresentableAndFlaggedByTheStopReason pins the two
// halves of the max_tokens hazard together.
func TestATruncatedToolCallIsRepresentableAndFlaggedByTheStopReason(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"message":{"id":"m","model":"claude-test","usage":{"input_tokens":5}}}`},
		[2]string{"content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"edit_file","input":{}}}`},
		[2]string{"content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\",\"new_string\":\"func mai"}}`},
		[2]string{"content_block_stop", `{"index":0}`},
		[2]string{"message_delta", `{"delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":4096}}`},
		[2]string{"message_stop", `{}`},
	)
	msg, _, _ := run(t, testModel(), core.Request{}, anthropic.Options{}, 200, body)

	calls := core.ExtractToolUse(msg)
	if len(calls) != 1 {
		t.Fatalf("%d tool calls, want 1", len(calls))
	}
	if !json.Valid(calls[0].Input) {
		t.Fatalf("Input is not valid JSON: %s", calls[0].Input)
	}
	if strings.Contains(string(calls[0].Input), "func mai") {
		t.Fatalf("Input = %s; the truncated member must be DROPPED, not closed. A "+
			"half-written new_string that parses applies cleanly and corrupts the file.",
			calls[0].Input)
	}
	if msg.StopReason != core.StopReasonLength {
		t.Fatalf("stop reason = %q, want max_tokens: salvage makes the call "+
			"representable and only the stop reason makes it unsafe to execute",
			msg.StopReason)
	}
}

// TestThinkingBlocksRoundTripWithTheirSignatures is what keeps a reasoning
// turn replayable: REQ-PROV-11 rule 4 demotes an unsigned thinking block, so a
// decoder that drops the signature silently downgrades every reasoning turn.
func TestThinkingBlocksRoundTripWithTheirSignatures(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"message":{"id":"m","model":"claude-test","usage":{"input_tokens":5}}}`},
		[2]string{"content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"step one"}}`},
		[2]string{"content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"SIGBYTES"}}`},
		[2]string{"content_block_stop", `{"index":0}`},
		[2]string{"content_block_start", `{"index":1,"content_block":{"type":"redacted_thinking","data":"OPAQUE"}}`},
		[2]string{"content_block_stop", `{"index":1}`},
		[2]string{"message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`},
		[2]string{"message_stop", `{}`},
	)
	msg, _, _ := run(t, testModel(), core.Request{}, anthropic.Options{}, 200, body)

	if len(msg.Content) != 2 {
		t.Fatalf("%d blocks, want 2", len(msg.Content))
	}
	tb, ok := msg.Content[0].(core.ThinkingBlock)
	if !ok || tb.Thinking != "step one" || tb.Signature != "SIGBYTES" {
		t.Fatalf("block 0 = %#v, want a SIGNED thinking block", msg.Content[0])
	}
	rb, ok := msg.Content[1].(core.ThinkingBlock)
	if !ok || !rb.Redacted || rb.Signature != "OPAQUE" {
		t.Fatalf("block 1 = %#v, want a redacted thinking block carrying its payload",
			msg.Content[1])
	}
}

// TestRequestHeadersAndAuth covers REQ-AUTH-03 and REQ-AUTH-02 through the
// real request path.
func TestRequestHeadersAndAuth(t *testing.T) {
	m := testModel()
	gwKey := "gw-secret"

	req := core.Request{Options: core.RequestOptions{
		Headers: map[string]*string{"x-api-key": nil, "x-gateway-key": &gwKey},
	}}
	_, _, sent := run(t, m, req, anthropic.Options{}, 200, streamFixture())
	if sent == nil {
		t.Fatal("no request was made")
	}
	if got := sent.Header.Get("anthropic-version"); got != anthropic.APIVersion {
		t.Fatalf("anthropic-version = %q, want %q", got, anthropic.APIVersion)
	}
	if got := sent.Header.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key = %q; a present-nil in RequestOptions.Headers must SUPPRESS "+
			"the provider default (REQ-AUTH-02) — that is how a gateway turns off the "+
			"upstream credential", got)
	}
	if got := sent.Header.Get("x-gateway-key"); got != gwKey {
		t.Fatalf("x-gateway-key = %q, want the caller's header to be sent", got)
	}
}

func TestAPIKeyIsSentWhenNotSuppressed(t *testing.T) {
	_, _, sent := run(t, testModel(), core.Request{}, anthropic.Options{}, 200, streamFixture())
	if got := sent.Header.Get("x-api-key"); got != "sk-ant-test-key-abcdefgh" {
		t.Fatalf("x-api-key = %q, want the resolved ANTHROPIC_API_KEY", got)
	}
	if sent.Header.Get("Authorization") != "" {
		t.Fatal("an ANTHROPIC_API_KEY must NOT ride as Authorization: Bearer")
	}
}

// TestOnPayloadCanRewriteTheEncodedRequest is REQ-PROV-18's post-serialization
// seam — the one middleware cannot reach because middleware operates on
// canonical types.
func TestOnPayloadCanRewriteTheEncodedRequest(t *testing.T) {
	var captured map[string]any
	req := core.Request{Options: core.RequestOptions{
		OnPayload: func(p any, _ *core.Model) (any, error) {
			b, _ := json.Marshal(p)
			_ = json.Unmarshal(b, &captured)
			captured["custom_vendor_field"] = true
			return captured, nil
		},
	}}
	_, _, sent := run(t, testModel(), req, anthropic.Options{}, 200, streamFixture())

	body, _ := io.ReadAll(sent.Body)
	if !strings.Contains(string(body), "custom_vendor_field") {
		t.Fatalf("body = %s; OnPayload's replacement must reach the wire", body)
	}
}

func TestOnPayloadErrorPropagatesUnmodified(t *testing.T) {
	sentinel := fmt.Errorf("policy says no")
	req := core.Request{Options: core.RequestOptions{
		OnPayload: func(any, *core.Model) (any, error) { return nil, sentinel },
	}}
	req.Options.Transport = rtFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("no request may be sent after OnPayload refuses")
		return nil, nil
	})
	s := anthropic.Provider(anthropic.Options{}).Stream(
		context.Background(), testModel(), req, core.ProviderStreamOptions{})
	if err := s.Err(); err != sentinel {
		t.Fatalf("err = %v, want the caller's own error UNMODIFIED so errors.Is works "+
			"on their sentinel (REQ-PROV-18)", err)
	}
}

// TestServerCompactionBlocksAreReplayedVerbatim is REQ-PROV-07.
//
// A compaction block is opaque, beta, and load-bearing: it is the state the
// server keeps in place of the history it removed. Dropping it looks safe and
// re-sends the history the compaction was paid to compact.
func TestServerCompactionBlocksAreReplayedVerbatim(t *testing.T) {
	const raw = `{"type":"compaction","id":"cmp_1","payload":{"opaque":"bytes"}}`
	body := sseBody(
		[2]string{"message_start", `{"message":{"id":"m","model":"claude-test","usage":{"input_tokens":5}}}`},
		[2]string{"content_block_start", `{"index":0,"content_block":` + raw + `}`},
		[2]string{"content_block_stop", `{"index":0}`},
		[2]string{"message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`},
		[2]string{"message_stop", `{}`},
	)
	msg, _, _ := run(t, testModel(), core.Request{}, anthropic.Options{
		Betas: []string{anthropic.BetaCompaction}}, 200, body)

	rb, ok := msg.Content[0].(core.RawBlock)
	if !ok {
		t.Fatalf("block 0 = %#v, want a RawBlock retaining the unmodelled type", msg.Content[0])
	}

	// And it must survive the trip back out.
	out, _, err := anthropic.BuildRequest(testModel(), core.Request{
		Messages: core.Messages{core.AssistantMessage{
			Content:  core.Content{rb},
			Provider: "anthropic", API: anthropic.API, Model: "claude-test",
			StopReason: core.StopReasonStop,
		}},
	}, core.CacheRetentionNone)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(out)
	if !strings.Contains(string(encoded), `"opaque":"bytes"`) {
		t.Fatalf("re-encoded request lost the compaction block: %s", encoded)
	}
}

func TestBetaHeaderIsSentWhenRequested(t *testing.T) {
	_, _, sent := run(t, testModel(), core.Request{},
		anthropic.Options{Betas: []string{anthropic.BetaCompaction}}, 200, streamFixture())
	if got := sent.Header.Get("anthropic-beta"); got != anthropic.BetaCompaction {
		t.Fatalf("anthropic-beta = %q, want %q", got, anthropic.BetaCompaction)
	}
}

// TestThinkingIsATriState is REQ-PROV-15's Anthropic arm: undefined omits the
// key, "off" sends {"type":"disabled"}, a mapped level sends a budget.
func TestThinkingIsATriState(t *testing.T) {
	m := testModel()
	budget := "2048"
	m.ThinkingLevelMap = map[core.ThinkingLevel]*string{core.ThinkingHigh: &budget}

	capture := func(level core.ThinkingLevel) map[string]any {
		var got map[string]any
		req := core.Request{ThinkingLevel: level, Options: core.RequestOptions{
			OnPayload: func(p any, _ *core.Model) (any, error) {
				b, _ := json.Marshal(p)
				_ = json.Unmarshal(b, &got)
				return nil, nil
			},
		}}
		run(t, m, req, anthropic.Options{}, 200, streamFixture())
		return got
	}

	if got := capture(core.ThinkingUnset); got["thinking"] != nil {
		t.Fatalf("thinking = %v with an unset level, want the key OMITTED", got["thinking"])
	}
	off := capture(core.ThinkingOff)["thinking"].(map[string]any)
	if off["type"] != "disabled" {
		t.Fatalf("thinking = %v for \"off\", want {\"type\":\"disabled\"}", off)
	}
	on := capture(core.ThinkingHigh)["thinking"].(map[string]any)
	if on["type"] != "enabled" || on["budget_tokens"] != float64(2048) {
		t.Fatalf("thinking = %v, want the catalog row's own budget", on)
	}

	// An unmapped level is OMITTED, never guessed: sending a level the model
	// does not know is a 400, and inventing a budget is worse than not
	// thinking (REQ-PROV-15).
	if got := capture(core.ThinkingMax)["thinking"]; got != nil {
		t.Fatalf("thinking = %v for an unmapped level, want the key omitted", got)
	}
}

func TestThinkingBudgetIsHeldBelowMaxTokens(t *testing.T) {
	m := testModel()
	m.MaxTokens = 1024
	budget := "4096" // larger than max_tokens: Anthropic rejects this outright
	m.ThinkingLevelMap = map[core.ThinkingLevel]*string{core.ThinkingHigh: &budget}

	var got map[string]any
	req := core.Request{ThinkingLevel: core.ThinkingHigh, Options: core.RequestOptions{
		OnPayload: func(p any, _ *core.Model) (any, error) {
			b, _ := json.Marshal(p)
			_ = json.Unmarshal(b, &got)
			return nil, nil
		},
	}}
	run(t, m, req, anthropic.Options{}, 200, streamFixture())

	th := got["thinking"].(map[string]any)
	if th["budget_tokens"].(float64) >= got["max_tokens"].(float64) {
		t.Fatalf("budget_tokens %v is not below max_tokens %v; the BUDGET is the value "+
			"we may lower, because max_tokens has already been clamped against the "+
			"context window", th["budget_tokens"], got["max_tokens"])
	}
}

// TestCancellationProducesAnAbortedTurnNotAnError keeps REQ-LOOP-09's dirty
// marker distinguishable from a provider failure: an aborted turn is terminal
// and is never retried (REQ-PROV-14), where an error may be.
func TestCancellationProducesAnAbortedTurnNotAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := core.Request{Options: core.RequestOptions{
		Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return nil, r.Context().Err()
		}),
	}}
	s := anthropic.Provider(anthropic.Options{
		Getenv: func(string) string { return "k" },
	}).Stream(ctx, testModel(), req, core.ProviderStreamOptions{})

	msg := s.Result()
	if msg.StopReason != core.StopReasonAborted {
		t.Fatalf("stop reason = %q, want aborted", msg.StopReason)
	}
}

func TestPerRequestTimeoutIsIndependentOfTheCallerContext(t *testing.T) {
	one := 1
	req := core.Request{Options: core.RequestOptions{
		TimeoutMs: &one,
		Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}),
	}}
	done := make(chan *core.AssistantMessage, 1)
	go func() {
		done <- anthropic.Provider(anthropic.Options{
			Getenv: func(string) string { return "k" },
		}).Stream(context.Background(), testModel(), req, core.ProviderStreamOptions{}).Result()
	}()

	select {
	case msg := <-done:
		if msg.StopReason == core.StopReasonStop {
			t.Fatal("a timed-out request must not report success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RequestOptions.TimeoutMs did not bound a request whose caller context " +
			"has no deadline")
	}
}
