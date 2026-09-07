package google_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/provider"
	"github.com/agentfox/agentkit-go/provider/google"
	"github.com/agentfox/agentkit-go/schema"
)

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
// The cost here is larger than a marshal: ConvertSchema TRANSLATES every
// schema into Gemini's dialect (upper-cased types, stripped keywords, rewritten
// nullability), and without the cache that whole walk is re-paid on every model
// call for bytes that did not change. REQ-CACHE-06's cache existed and was
// attached to no provider; this is the assertion that says it is attached.
func TestASteadyStateRequestSerializesNoSchemas(t *testing.T) {
	req := core.Request{Tools: prefixTools(16)}
	prefix := &provider.ToolPrefix{}

	_, _, first, err := google.BuildRequestCached(model(), req, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if first.Marshalled != 16 {
		t.Fatalf("first build serialized %d schemas, want all 16", first.Marshalled)
	}

	body, _, second, err := google.BuildRequestCached(model(), req, prefix)
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

	// The cached bytes must still be THIS dialect's: a cache that served the
	// canonical JSON Schema would hand Gemini a 400 on every tool-carrying
	// request instead of a saved marshal.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"type":"OBJECT"`) {
		t.Fatalf("the cached schema is not in Gemini's dialect: %s", raw)
	}
}

// TestTheProviderOwnsAPrefixByDefault: the wiring must not require the caller
// to opt in, or the default configuration is the slow one.
func TestTheProviderOwnsAPrefixByDefault(t *testing.T) {
	var reports []provider.SyncReport
	p := google.Provider(google.Options{
		Getenv:           func(string) string { return "" },
		OnToolPrefixSync: func(r provider.SyncReport) { reports = append(reports, r) },
	})

	req := core.Request{
		Messages: core.Messages{core.UserMessage{Content: core.Content{core.TextBlock{Text: "hi"}}}},
		Tools:    prefixTools(8),
		Options: core.RequestOptions{
			Env: map[string]string{"GEMINI_API_KEY": "k"},
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return sseOK(), nil
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
			Provider: "google", API: google.API, Model: "gemini-x",
			StopReason: core.StopReasonToolUse},
		core.ToolResultMessage{ToolUseID: "c1", ToolName: "read_file",
			Content:        core.Content{core.TextBlock{Text: "ok"}},
			AddedToolNames: []string{"mcp__db__query"}},
	}
}

// TestADeferredToolIsWithheldFromTheDeclarationsAndRedeclaredInPlace is
// REQ-CACHE-10's third arm: this wire supports neither defer_loading nor
// additional_tools, so the tool is withheld from the declarations and
// re-declared at the transcript position where it appeared.
//
// The property that matters is that everything BEFORE that position is
// byte-identical to the previous turn's request. Prepending the new tool to
// functionDeclarations — the obvious implementation — rewrites the cached
// prefix and costs the entire provider-side cache over one added tool.
func TestADeferredToolIsWithheldFromTheDeclarationsAndRedeclaredInPlace(t *testing.T) {
	_, w := build(t, model(), core.Request{
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

	if len(w.Tools) != 1 || len(w.Tools[0].FunctionDeclarations) != 1 ||
		w.Tools[0].FunctionDeclarations[0].Name != "read_file" {
		t.Fatalf("functionDeclarations = %+v, want only the established tool: a deferred "+
			"tool declared here sits in the cached prefix", w.Tools)
	}

	// The re-declaration rides in the LAST content, after the functionResponse
	// parts of the run that introduced it — not before them, and not in
	// systemInstruction, which is the head of the cached prefix.
	last := w.Contents[len(w.Contents)-1]
	if last.Role != "user" {
		t.Fatalf("the re-declaration landed on a %q content", last.Role)
	}
	var sawResult bool
	var note string
	for _, p := range last.Parts {
		if p.FunctionResponse != nil {
			sawResult = true
		}
		if p.Text != "" {
			if !sawResult {
				t.Fatal("the re-declaration must come AFTER the tool-result run")
			}
			note = p.Text
		}
	}
	if note == "" {
		t.Fatalf("no re-declaration in the transcript: %+v", last.Parts)
	}
	if !strings.Contains(note, "mcp__db__query") || !strings.Contains(note, `"sql"`) {
		t.Fatalf("the re-declaration must carry the tool's own declaration, got %q", note)
	}
	if w.SystemInstruction != nil && strings.Contains(w.SystemInstruction.Parts[0].Text, "mcp__db__query") {
		t.Fatal("the re-declaration must not be folded into systemInstruction, which is " +
			"the very head of the cached prefix")
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

	if len(w.Tools) != 1 || len(w.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("functionDeclarations = %+v, want the promoted tool declared normally", w.Tools)
	}
	for _, ct := range w.Contents {
		for _, p := range ct.Parts {
			if strings.Contains(p.Text, "Additional tools") {
				t.Fatal("a promoted tool must not ALSO be re-declared in the transcript")
			}
		}
	}
}

// TestWithNothingDeferredTheRequestIsUnchanged keeps the ordinary case exactly
// as it was: the deferral machinery must be invisible when nothing defers.
func TestWithNothingDeferredTheRequestIsUnchanged(t *testing.T) {
	r := request(t)
	_, w := build(t, model(), r)
	if len(w.Tools) != 1 || len(w.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("functionDeclarations = %+v", w.Tools)
	}
	for _, ct := range w.Contents {
		for _, p := range ct.Parts {
			if strings.Contains(p.Text, "Additional tools") {
				t.Fatal("a re-declaration appeared with nothing deferred")
			}
		}
	}
}
