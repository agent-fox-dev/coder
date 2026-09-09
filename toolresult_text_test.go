package agentkit

import (
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
)

// TestAToolsOwnRenderingReachesTheModelVerbatim: a tool that supplies
// ToolResult.Text is read by the model as that text, not as the JSON envelope
// with the text escaped inside it. The mutation this catches is the renderer
// ignoring Text and marshalling ToLLMMap regardless.
func TestAToolsOwnRenderingReachesTheModelVerbatim(t *testing.T) {
	call := core.ToolUseBlock{ID: "tu_1", Name: "read_file"}
	body := "func main() {\n\tfmt.Println(\"hi\")\n}\n"

	rendered := toolResultMessage(call, core.ToolResult{
		OK:   true,
		Data: map[string]any{"content": body},
		Text: body,
	})
	if got := rendered.Content.Text(); got != body {
		t.Fatalf("Text must reach the model verbatim.\n got: %q\nwant: %q", got, body)
	}
	if rendered.IsError {
		t.Fatal("a Text-rendered OK result is not an error")
	}

	// Without Text the REQ-TOOL-08 envelope is unchanged: the model still
	// sees the JSON payload with Data inside it.
	enveloped := toolResultMessage(call, core.ToolResult{
		OK:   true,
		Data: map[string]any{"content": body},
	})
	got := enveloped.Content.Text()
	if !strings.HasPrefix(got, `{"data":{"content":"func main()`) || !strings.Contains(got, `"ok":true`) {
		t.Fatalf("without Text the envelope must be the JSON payload, got %q", got)
	}
	if strings.Contains(got, "\n\t") {
		t.Fatal("the envelope escapes control characters; a raw tab inside it means the wrong path rendered")
	}
}

// TestAnErrorResultKeepsTheEnvelopeUnlessRendered: error results keep the
// compact JSON envelope (the model keys on "error" and "detail"), and a tool
// that renders its own error text is honoured too.
func TestAnErrorResultKeepsTheEnvelopeUnlessRendered(t *testing.T) {
	call := core.ToolUseBlock{ID: "tu_2", Name: "edit_file"}
	m := toolResultMessage(call, core.ErrResult("edit_not_found", "the string to replace was not found"))
	if !m.IsError || m.Content.Text() != `{"detail":"the string to replace was not found","error":"edit_not_found","ok":false}` {
		t.Fatalf("error envelope changed: %q", m.Content.Text())
	}
	r := core.ErrResult("boom", "x")
	r.Text = "error boom: rendered by the tool"
	if got := toolResultMessage(call, r).Content.Text(); got != r.Text {
		t.Fatalf("a rendered error must reach the model verbatim, got %q", got)
	}
}
