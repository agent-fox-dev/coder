package agentkit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/imagex"
	"github.com/agentfox/agentkit-go/schema"
)

// bigPNG is over the dimension cap, so normalization must change it.
func bigPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2400, 1200))
	for y := 0; y < 1200; y++ {
		for x := 0; x < 2400; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// imageTool returns whatever blocks it is given, so a test can put an image
// into history through the ordinary tool path.
func imageTool(name string, blocks func() []core.ContentBlock) core.Tool {
	return core.Tool{
		Name:        name,
		Description: "returns an image",
		InputSchema: schema.Object(),
		Execute: func(context.Context, json.RawMessage) core.ToolResult {
			r := core.OKResult(map[string]any{"note": "here"})
			r.Blocks = blocks()
			return r
		},
	}
}

// runWithImageTool drives one tool-calling turn and returns the resulting
// history.
func runWithImageTool(t *testing.T, tool core.Tool, tweak func(*core.AgentConfig)) core.Messages {
	t.Helper()
	s := &scripted{turns: []core.AssistantMessage{
		assistantWithTools(core.StopReasonToolUse, toolUse(t, "c1", tool.Name, `{}`)),
		{Content: core.Content{core.TextBlock{Text: "done"}}, StopReason: core.StopReasonStop},
	}}
	a := newTestAgent(t, s, tweak)
	if err := a.RegisterTool(tool); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	return a.History().Messages()
}

// imagesIn extracts every ImageBlock from a transcript.
func imagesIn(msgs core.Messages) []core.ImageBlock {
	var out []core.ImageBlock
	for _, m := range msgs {
		tr, ok := m.(core.ToolResultMessage)
		if !ok {
			continue
		}
		for _, b := range tr.Content {
			if img, ok := b.(core.ImageBlock); ok {
				out = append(out, img)
			}
		}
	}
	return out
}

// TestAnImageFromAnyToolIsNormalizedAtTheHistoryBoundary is REQ-TOOL-14.1.
//
// The tool here is a plain custom tool that knows nothing about normalization
// — which is the point: doing this inside individual tools would miss custom
// tools, MCP-bridged results, and anything a hook injected.
func TestAnImageFromAnyToolIsNormalizedAtTheHistoryBoundary(t *testing.T) {
	raw := bigPNG(t)
	tool := imageTool("shot", func() []core.ContentBlock {
		return []core.ContentBlock{core.ImageBlock{
			Data: base64.StdEncoding.EncodeToString(raw), MimeType: imagex.MIMEPNG}}
	})

	imgs := imagesIn(runWithImageTool(t, tool, nil))
	if len(imgs) != 1 {
		t.Fatalf("want one image in history, got %d", len(imgs))
	}
	decoded, err := base64.StdEncoding.DecodeString(imgs[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width > imagex.MaxDimension || cfg.Height > imagex.MaxDimension {
		t.Fatalf("the image reached history at %dx%d, over the %d cap",
			cfg.Width, cfg.Height, imagex.MaxDimension)
	}
}

// TestNormalizationRunsAfterThePostToolHook is REQ-TOOL-14.2. A hook that
// injects an image must have it normalized too — otherwise the one path that
// can add an image without any tool knowing is the one path that skips the
// rule.
func TestNormalizationRunsAfterThePostToolHook(t *testing.T) {
	raw := bigPNG(t)
	plain := imageTool("noop", func() []core.ContentBlock { return nil })

	msgs := runWithImageTool(t, plain, func(c *core.AgentConfig) {
		c.AfterToolCall = func(_ context.Context, in core.AfterToolCallContext) core.AfterToolCallDecision {
			in.Result.Content = append(in.Result.Content, core.ImageBlock{
				Data: base64.StdEncoding.EncodeToString(raw), MimeType: imagex.MIMEPNG})
			return core.AfterToolCallDecision{}
		}
	})

	imgs := imagesIn(msgs)
	if len(imgs) != 1 {
		t.Fatalf("want the hook-injected image, got %d", len(imgs))
	}
	decoded, _ := base64.StdEncoding.DecodeString(imgs[0].Data)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width > imagex.MaxDimension {
		t.Fatalf("a hook-injected image reached history at %dx%d, unnormalized",
			cfg.Width, cfg.Height)
	}
}

// TestANormalizationFailureKeepsTheOriginalBlock is REQ-TOOL-14.5.
//
// Dropping it would delete the tool's output over a re-encode that did not
// work: the model asked for a screenshot and gets silence, with nothing in the
// transcript to say why.
func TestANormalizationFailureKeepsTheOriginalBlock(t *testing.T) {
	// A WebP header. Sniffed correctly, and undecodable by this build.
	webp := base64.StdEncoding.EncodeToString(
		append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 32)...))
	tool := imageTool("shot", func() []core.ContentBlock {
		return []core.ContentBlock{core.ImageBlock{Data: webp, MimeType: imagex.MIMEWebP}}
	})

	var errs []error
	msgs := runWithImageTool(t, tool, func(c *core.AgentConfig) {
		c.Hooks.OnError = func(err error) { errs = append(errs, err) }
	})

	imgs := imagesIn(msgs)
	if len(imgs) != 1 {
		t.Fatalf("the original block must survive a failed normalization; got %d images",
			len(imgs))
	}
	if imgs[0].Data != webp {
		t.Fatal("the original block must be kept verbatim, not replaced")
	}
	var ine *ImageNormalizationError
	var found bool
	for _, e := range errs {
		if errors.As(e, &ine) {
			found = true
		}
	}
	if !found {
		t.Fatalf("a kept-original must still be reported, or it is silent; got %v", errs)
	}
}

// TestAConformingImagePassesThroughUnchanged. Normalization must be
// idempotent: a session reloading its history would otherwise recompress every
// image it had ever seen, once per reload.
func TestAConformingImagePassesThroughUnchanged(t *testing.T) {
	small := image.NewRGBA(image.Rect(0, 0, 32, 32))
	var buf bytes.Buffer
	if err := png.Encode(&buf, small); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	tool := imageTool("shot", func() []core.ContentBlock {
		return []core.ContentBlock{core.ImageBlock{Data: encoded, MimeType: imagex.MIMEPNG}}
	})

	imgs := imagesIn(runWithImageTool(t, tool, nil))
	if len(imgs) != 1 || imgs[0].Data != encoded {
		t.Fatal("an image inside both budgets must reach history byte-for-byte")
	}
}

// TestToolSuppliedBase64IsDecodedLeniently is REQ-TOOL-14.4 through the real
// path: a tool that wrapped its base64 in newlines, as any shell pipeline
// would, must not have its output discarded.
func TestToolSuppliedBase64IsDecodedLeniently(t *testing.T) {
	raw := bigPNG(t)
	wrapped := wrapBase64(base64.StdEncoding.EncodeToString(raw), 76)
	tool := imageTool("shot", func() []core.ContentBlock {
		return []core.ContentBlock{core.ImageBlock{Data: wrapped, MimeType: imagex.MIMEPNG}}
	})

	imgs := imagesIn(runWithImageTool(t, tool, nil))
	if len(imgs) != 1 {
		t.Fatalf("got %d images", len(imgs))
	}
	if strings.Contains(imgs[0].Data, "\n") {
		t.Fatal("the wrapped payload was never re-encoded, so the lenient decode did " +
			"not run")
	}
	if _, err := base64.StdEncoding.DecodeString(imgs[0].Data); err != nil {
		t.Fatalf("what reached history is not valid base64: %v", err)
	}
}

func wrapBase64(s string, n int) string {
	var b strings.Builder
	for i := 0; i < len(s); i += n {
		end := min(i+n, len(s))
		b.WriteString(s[i:end])
		b.WriteByte('\n')
	}
	return b.String()
}
