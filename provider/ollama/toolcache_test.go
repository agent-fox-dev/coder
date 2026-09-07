package ollama_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/ollama"
	"github.com/agentfox/agentkit-go/schema"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ndjsonOK is a complete, minimal /api/chat stream. A well-formed response
// matters even when the test only inspects the request: a malformed one makes
// the provider retry and the transport observe the same request twice.
func ndjsonOK() *http.Response {
	const body = `{"message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop"}` + "\n"
	return &http.Response{StatusCode: 200,
		Header: http.Header{"Content-Type": []string{"application/x-ndjson"}},
		Body:   io.NopCloser(strings.NewReader(body))}
}

func prefixTools(n int) []core.ToolWire {
	out := make([]core.ToolWire, n)
	for i := range out {
		name := "tool_" + strconv.Itoa(i)
		out[i] = core.ToolWire{Name: name, Description: name,
			InputSchema: schema.Object(schema.Prop("path", schema.String("path")))}
	}
	return out
}

// TestASteadyStateRequestSerializesNoSchemas is NFR-PERF-03 on this wire, as a
// COUNT rather than a duration — a zero does not flake on a busy runner.
//
// REQ-CACHE-06's cache was implemented, unit-tested and attached to no
// provider, so every request re-serialized every schema. This is the assertion
// that says it is attached here.
func TestASteadyStateRequestSerializesNoSchemas(t *testing.T) {
	req := core.Request{Tools: prefixTools(16)}
	prefix := &provider.ToolPrefix{}

	_, _, first, err := ollama.BuildRequestCached(model(), req, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if first.Marshalled != 16 {
		t.Fatalf("first build serialized %d schemas, want all 16", first.Marshalled)
	}

	_, _, second, err := ollama.BuildRequestCached(model(), req, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if second.Marshalled != 0 {
		t.Fatalf("a steady-state build serialized %d schemas, want 0. The cache is "+
			"present but not on the request path — which is a cache that costs memory "+
			"and saves nothing.", second.Marshalled)
	}
	if second.Invalidated {
		t.Fatalf("an unchanged tool list invalidated the prefix: %s", second.Reason)
	}
}

// TestTheProviderOwnsAPrefixByDefault: the wiring must not require the caller
// to opt in, or the default configuration is the slow one.
func TestTheProviderOwnsAPrefixByDefault(t *testing.T) {
	var reports []provider.SyncReport
	p := ollama.Provider(ollama.Options{
		Getenv:           func(string) string { return "" },
		OnToolPrefixSync: func(r provider.SyncReport) { reports = append(reports, r) },
	})

	req := core.Request{
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		Tools:    prefixTools(8),
		Options: core.RequestOptions{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return ndjsonOK(), nil
			}),
		},
	}
	for i := 0; i < 2; i++ {
		p.Stream(context.Background(), model(), req, core.ProviderStreamOptions{}).Result()
	}

	if len(reports) != 2 {
		t.Fatalf("%d sync reports, want one per request", len(reports))
	}
	if reports[0].Marshalled != 8 {
		t.Fatalf("first request serialized %d schemas, want 8", reports[0].Marshalled)
	}
	if reports[1].Marshalled != 0 {
		t.Fatalf("second request serialized %d schemas, want 0: a provider constructed "+
			"with default Options must cache without being asked", reports[1].Marshalled)
	}
}

// ---------------------------------------------------------------- REQ-CACHE-10

func deferredTranscript(t *testing.T) core.Messages {
	t.Helper()
	return core.Messages{
		core.UserMessage{Content: core.Content{core.TextBlock{Text: "go"}}},
		core.AssistantMessage{Content: core.Content{mk(t, "c1", "read_file", `{}`)},
			Provider: "ollama", API: ollama.API, Model: "qwen3:8b",
			StopReason: core.StopReasonToolUse},
		core.ToolResultMessage{ToolUseID: "c1", ToolName: "read_file",
			Content:        core.Content{core.TextBlock{Text: "ok"}},
			AddedToolNames: []string{"mcp__db__query"}},
	}
}

// TestADeferredToolIsWithheldFromTheToolsArrayAndRedeclaredAfterTheResults is
// REQ-CACHE-10's third arm: /api/chat supports neither defer_loading nor
// additional_tools, so the tool is withheld from the top-level tools array and
// re-declared in a system message placed after the tool-result run.
//
// The property that matters is that everything BEFORE that message is
// byte-identical to the previous turn's request. Adding the tool to the array
// — the obvious implementation — rewrites the prefix instead.
func TestADeferredToolIsWithheldFromTheToolsArrayAndRedeclaredAfterTheResults(t *testing.T) {
	_, w := build(t, model(), core.Request{
		System: []core.ContentBlock{core.TextBlock{Text: "You are a code reader."}},
		// Declared with the NEW tool first, so an implementation that merely
		// preserves caller order fails visibly.
		Tools: []core.ToolWire{
			{Name: "mcp__db__query", Description: "Query the db",
				InputSchema: schema.Object(schema.Prop("sql", schema.String("sql")))},
			{Name: "read_file", Description: "Read a file",
				InputSchema: schema.Object(schema.Prop("path", schema.String("path")))},
		},
		Messages: deferredTranscript(t),
	})

	if len(w.Tools) != 1 || w.Tools[0].Function.Name != "read_file" {
		t.Fatalf("tools = %+v, want only the established tool: a deferred tool declared "+
			"here sits in the cached prefix", w.Tools)
	}

	// The head of the transcript — the system prompt — must be untouched, and
	// the re-declaration must be the LAST message, after the tool result.
	if w.Messages[0].Role != "system" || *w.Messages[0].Content != "You are a code reader." {
		t.Fatalf("messages[0] = %+v; the cached prefix begins here and must not change",
			w.Messages[0])
	}
	last := w.Messages[len(w.Messages)-1]
	if last.Role != "system" {
		t.Fatalf("the re-declaration must be a system message, got role %q", last.Role)
	}
	if w.Messages[len(w.Messages)-2].Role != "tool" {
		t.Fatal("the re-declaration must be placed AFTER the tool-result run")
	}
	if last.Content == nil || !strings.Contains(*last.Content, "mcp__db__query") ||
		!strings.Contains(*last.Content, `"sql"`) {
		t.Fatalf("the re-declaration must carry the tool's own declaration, got %v", last.Content)
	}
}

// TestWithEveryToolDeferredTheyArePromotedBack is REQ-CACHE-10's safety valve:
// with nothing immediate there is no prefix to anchor references against, so
// the deferral buys nothing and costs the model its tools.
func TestWithEveryToolDeferredTheyArePromotedBack(t *testing.T) {
	_, w := build(t, model(), core.Request{
		Tools: []core.ToolWire{{Name: "mcp__db__query", Description: "Query the db",
			InputSchema: schema.Object(schema.Prop("sql", schema.String("sql")))}},
		Messages: deferredTranscript(t),
	})

	if len(w.Tools) != 1 || w.Tools[0].Function.Name != "mcp__db__query" {
		t.Fatalf("tools = %+v, want the promoted tool declared normally", w.Tools)
	}
	for _, m := range w.Messages {
		if m.Content != nil && strings.Contains(*m.Content, "Additional tools") {
			t.Fatal("a promoted tool must not ALSO be re-declared in the transcript")
		}
	}
}

// TestWithNothingDeferredTheRequestIsUnchanged keeps the ordinary case exactly
// as it was: the deferral machinery must be invisible when nothing defers.
func TestWithNothingDeferredTheRequestIsUnchanged(t *testing.T) {
	_, w := build(t, model(), request(t))
	if len(w.Tools) != 1 || w.Tools[0].Function.Name != "read_file" {
		t.Fatalf("tools = %+v", w.Tools)
	}
	for _, m := range w.Messages {
		if m.Role == "system" && m.Content != nil && strings.Contains(*m.Content, "Additional tools") {
			t.Fatal("a re-declaration appeared with nothing deferred")
		}
	}
}
