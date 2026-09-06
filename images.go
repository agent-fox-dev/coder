package agentkit

import (
	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/imagex"
)

// normalizeImages is REQ-TOOL-14 at the history boundary.
//
// A failure KEEPS THE ORIGINAL BLOCK (REQ-TOOL-14.5). Dropping it would delete
// the tool's output over a re-encode that did not work — the model asked for a
// screenshot and would get silence, with nothing in the transcript to say why.
// The oversized image may still be refused by the provider, but that is a
// visible error the caller can act on, where a silent deletion is not.
func (a *Agent) normalizeImages(msg *core.ToolResultMessage) {
	for i, blk := range msg.Content {
		img, ok := blk.(core.ImageBlock)
		if !ok {
			continue
		}
		normalized, err := normalizeImageBlock(img)
		if err != nil {
			a.fireError(&ImageNormalizationError{
				ToolName: msg.ToolName, ToolUseID: msg.ToolUseID, Err: err})
			continue
		}
		msg.Content[i] = normalized
	}
}

// normalizeImageBlock is the pure part, separated so it can be tested without
// an Agent.
func normalizeImageBlock(img core.ImageBlock) (core.ImageBlock, error) {
	raw, err := imagex.DecodeBase64(img.Data)
	if err != nil {
		return img, err
	}
	res, err := imagex.Normalize(raw, img.MimeType)
	if err != nil {
		return img, err
	}
	if !res.Changed {
		// Byte-identical input: keep the block as it was, so normalization is
		// idempotent and a reloaded session does not recompress its history.
		return img, nil
	}
	return core.ImageBlock{Data: res.Base64(), MimeType: res.MIMEType}, nil
}

// ImageNormalizationError is surfaced through the error hook rather than
// failing the tool call, because the original block is still there and the run
// can continue.
type ImageNormalizationError struct {
	ToolName  string
	ToolUseID string
	Err       error
}

func (e *ImageNormalizationError) Error() string {
	return "agentkit: could not normalize an image from " + e.ToolName +
		" (the original block was kept): " + e.Err.Error()
}

func (e *ImageNormalizationError) Unwrap() error { return e.Err }
