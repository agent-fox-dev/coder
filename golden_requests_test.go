package agentkit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider/anthropic"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/provider/ollama"
	"github.com/agentfox/agentkit-go/provider/openai"
	"github.com/agentfox/agentkit-go/provider/openairesponses"
	"github.com/agentfox/agentkit-go/schema"
)

// NFR-TEST-08(b): the per-provider request body.
//
// One canonical request drives every wire API, so a golden diff shows what a
// provider does DIFFERENTLY rather than what its own fixture happened to
// contain. The body captured is the real one — the bytes the RoundTripper saw
// — not a re-marshalled struct, because the thing that drifts is serialization.

type goldenCase struct {
	name string
	body string
}

// canonicalRequest exercises the parts that translate differently per wire:
// a system prompt, multi-turn history, an image, a tool call and its result,
// a tool schema, and the sampling knobs.
func canonicalRequest(t *testing.T) core.Request {
	t.Helper()
	call, err := core.NewToolUse("call_1", "find_files", json.RawMessage(`{"pattern":"**/*.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	temp, topP := 0.2, 0.9
	maxTok := 1024

	return core.Request{
		System: []core.ContentBlock{core.TextBlock{Text: "You are a release engineer."}},
		Messages: core.Messages{
			core.UserMessage{Content: core.Content{
				core.TextBlock{Text: "Which Go files changed?"},
				// A 1x1 PNG: small enough to read in a golden, real enough to
				// exercise each wire's image encoding.
				core.ImageBlock{MimeType: "image/png", Data: onePixelPNG},
			}},
			core.AssistantMessage{
				Content:    core.Content{core.TextBlock{Text: "Let me look."}, call},
				StopReason: core.StopReasonToolUse,
			},
			core.ToolResultMessage{
				ToolUseID: "call_1", ToolName: "find_files",
				Content: core.Content{core.TextBlock{
					Text: `{"ok":true,"data":{"entries":["main.go"]}}`}},
			},
		},
		Tools: core.ToolWires([]core.Tool{{
			Name:        "find_files",
			Description: "Find files by glob pattern.",
			InputSchema: schema.Object(
				schema.Prop("pattern", schema.String("Glob pattern")),
				schema.Opt("limit", schema.Int("Maximum results")),
			),
		}}),
		ToolChoice:    core.ToolChoiceAuto,
		MaxTokens:     &maxTok,
		Temperature:   &temp,
		TopP:          &topP,
		StopSequences: []string{"\n\nHuman:"},
	}
}

// onePixelPNG is a 1x1 transparent PNG.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk" +
	"YPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func goldenModel(id string, api core.API, vendor string) *core.Model {
	return &core.Model{
		ID: id, Name: id, API: api, Provider: vendor,
		ContextWindow: 200000, MaxTokens: 8192,
		Input:     []string{"text", "image"},
		Reasoning: false,
		Cost:      core.Cost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// capture drives one provider and returns the request body it sent.
func capture(t *testing.T, p core.APIProvider, m *core.Model, req core.Request) string {
	t.Helper()
	var body []byte
	req.Options.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ = io.ReadAll(r.Body)
		// A minimal well-formed response: the stream's outcome is irrelevant,
		// only the request matters, but a malformed one would make the
		// provider retry and capture twice.
		return &http.Response{StatusCode: 200,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(strings.NewReader("{}"))}, nil
	})
	req.Options.Env = map[string]string{
		"ANTHROPIC_API_KEY": "test-key", "OPENAI_API_KEY": "test-key",
		"GEMINI_API_KEY": "test-key", "GOOGLE_API_KEY": "test-key",
	}
	p.Stream(context.Background(), m, req, core.ProviderStreamOptions{}).Result()
	if len(body) == 0 {
		t.Fatal("no request body was captured")
	}
	return indentJSON(t, body)
}

// indentJSON reformats for a REVIEWABLE golden.
//
// The diff a reviewer reads is the point of the artifact, and a 4 KB single
// line diffs as one changed line. Key order is preserved by using a decoder
// over the raw bytes rather than a map.
func indentJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		t.Fatalf("provider sent a body that is not valid JSON: %v\n%s", err, raw)
	}
	out.WriteByte('\n')
	return out.String()
}

func goldenRequestCases(t *testing.T) []goldenCase {
	t.Helper()
	req := canonicalRequest(t)
	return []goldenCase{
		{"anthropic", capture(t,
			anthropic.Provider(anthropic.Options{BaseURL: "https://example.invalid"}),
			goldenModel("claude-test", core.API("anthropic-messages"), "anthropic"), req)},
		{"openai", capture(t,
			openai.Provider(openai.Options{BaseURL: "https://example.invalid"}),
			goldenModel("gpt-test", core.API("openai-completions"), "openai"), req)},
		{"google", capture(t,
			google.Provider(google.Options{BaseURL: "https://example.invalid"}),
			goldenModel("gemini-test", core.API("google-generative-ai"), "google"), req)},
		{"openai_responses", capture(t,
			openairesponses.Provider(openairesponses.Options{BaseURL: "https://example.invalid"}),
			goldenModel("gpt-resp-test", core.API("openai-responses"), "openai"), req)},
		{"ollama", capture(t,
			ollama.Provider(ollama.Options{BaseURL: "https://example.invalid"}),
			goldenModel("llama-test", core.API("ollama"), "ollama"), req)},
	}
}
