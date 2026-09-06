// Package imagex normalizes images for a provider's inline-image limits
// (REQ-TOOL-14). It depends on the standard library only.
//
// Nothing here knows about ContentBlock or about tools. The rule the
// requirement states — every image entering history is re-processed, whichever
// tool produced it — is enforced at the history boundary; this package is the
// part that actually resizes and re-encodes, so a tool that wants to validate
// an image before returning it can use the same code.
//
// WebP is the visible gap: the standard library has no WebP decoder and
// REQ-GO-11 forbids the module that does. A WebP image is reported as
// unsupported, which under REQ-TOOL-14.5 means the original block is kept
// rather than dropped — the same outcome as a decode failure, and better than
// pulling in a dependency to shrink a format providers already accept.
package imagex

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
)

// MaxDimension is REQ-TOOL-14.3's 2000×2000.
const MaxDimension = 2000

// MaxBase64Bytes is the 4.5 MB budget, measured on the BASE64 text rather than
// the decoded bytes.
//
// Base64 is what actually crosses the wire, and it is 4/3 the size of what it
// encodes. Budgeting the decoded bytes would ship an image a third larger than
// the limit it was checked against — and the requirement's own reason for
// headroom is that an image at exactly the provider limit is rejected once and
// then poisons every later request in the session.
const MaxBase64Bytes = 4608 * 1024 // 4.5 MiB

// QualityLadder is REQ-TOOL-14.3's fixed ladder.
//
// Fixed, not searched: a binary search for the largest quality that fits costs
// several encodes of a large image to save a few percent of bytes, on a path
// that runs inside a tool call the model is waiting on.
var QualityLadder = []int{85, 70, 55, 40}

// maxHalvings bounds the downscale loop after the quality ladder is spent.
// 2000px halved four times is 125px; an image that still does not fit is not
// going to become useful by getting smaller.
const maxHalvings = 4

// Errors reported for formats providers reject (REQ-TOOL-14.6). They are
// refused at the tool with a clear message rather than forwarded.
var (
	ErrCMYKJPEG = errors.New("imagex: CMYK JPEG is not accepted by providers; " +
		"re-save it as RGB")
	ErrAnimatedPNG = errors.New("imagex: animated PNG (APNG) is not accepted by " +
		"providers; export a single frame")
	ErrNonIHDRPNG  = errors.New("imagex: malformed PNG: the first chunk is not IHDR")
	ErrUnsupported = errors.New("imagex: unsupported image format")
	ErrNotAnImage  = errors.New("imagex: not a recognised image")
)

// MIME types this package recognises.
const (
	MIMEJPEG = "image/jpeg"
	MIMEPNG  = "image/png"
	MIMEGIF  = "image/gif"
	MIMEWebP = "image/webp"
)

// Sniff identifies an image by MAGIC BYTES, never by file extension
// (REQ-TOOL-14.6).
//
// A tool reading a file cannot trust the name: `screenshot.txt` is still a PNG
// if its first eight bytes say so, and `notes.png` written by a text editor is
// not one.
func Sniff(b []byte) (string, bool) {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return MIMEJPEG, true
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return MIMEPNG, true
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return MIMEGIF, true
	case len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return MIMEWebP, true
	}
	return "", false
}

// DecodeBase64 is REQ-TOOL-14.4's LENIENT decode.
//
// Tool-supplied base64 arrives wrapped in newlines from shell pipelines, in
// base64url from anything that passed it through a URL, and with padding
// stripped by encoders that consider it optional. A strict decoder rejects all
// three, and the tool's output is then discarded for a reason that has nothing
// to do with the image.
func DecodeBase64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
	// A data: URL prefix is common enough in tool output to be worth stripping
	// rather than failing on.
	if i := strings.Index(s, ";base64,"); i >= 0 {
		s = s[i+len(";base64,"):]
	}
	if strings.ContainsAny(s, "-_") {
		s = strings.NewReplacer("-", "+", "_", "/").Replace(s)
	}
	s = strings.TrimRight(s, "=")
	return base64.RawStdEncoding.DecodeString(s)
}

// Validate refuses the formats providers reject, without re-encoding.
//
// Refusing at the tool is REQ-TOOL-14.6: forwarding one of these produces a
// provider error on the NEXT request and every request after it, because by
// then the image is already in history.
func Validate(data []byte, mime string) error {
	if mime == "" {
		var ok bool
		if mime, ok = Sniff(data); !ok {
			return ErrNotAnImage
		}
	}
	switch mime {
	case MIMEPNG:
		return validatePNG(data)
	case MIMEJPEG:
		return validateJPEG(data)
	case MIMEGIF:
		return nil
	case MIMEWebP:
		return fmt.Errorf("%w: webp cannot be decoded by this build", ErrUnsupported)
	}
	return fmt.Errorf("%w: %s", ErrUnsupported, mime)
}

// validatePNG checks chunk structure directly.
//
// Go's PNG decoder happily decodes the first frame of an APNG and never
// mentions it, so an animation reaches the provider looking like a still and
// is rejected there. The chunk walk is the only place the difference is
// visible.
func validatePNG(data []byte) error {
	const sig = "\x89PNG\r\n\x1a\n"
	if !bytes.HasPrefix(data, []byte(sig)) {
		return ErrNotAnImage
	}
	rest := data[len(sig):]
	first := true
	for len(rest) >= 8 {
		length := int(uint32(rest[0])<<24 | uint32(rest[1])<<16 | uint32(rest[2])<<8 | uint32(rest[3]))
		typ := string(rest[4:8])
		if first {
			// The PNG spec requires IHDR first. Anything else is either
			// corrupt or a file pretending to be a PNG.
			if typ != "IHDR" {
				return ErrNonIHDRPNG
			}
			first = false
		}
		switch typ {
		case "acTL":
			// The animation control chunk, which by spec precedes IDAT.
			return ErrAnimatedPNG
		case "IDAT", "IEND":
			// Past the header chunks: an acTL after this point is not an
			// animation the decoder would honour.
			return nil
		}
		if length < 0 || len(rest) < 8+length+4 {
			return nil // truncated: let the decoder report it
		}
		rest = rest[8+length+4:]
	}
	return nil
}

// validateJPEG decodes to find the colour model.
//
// There is no header field that says "CMYK" without parsing the frame markers,
// and Go's decoder already does that — it returns *image.CMYK. Decoding is the
// cheap, correct check.
func validateJPEG(data []byte) error {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("imagex: %w", err)
	}
	if _, isCMYK := img.(*image.CMYK); isCMYK {
		return ErrCMYKJPEG
	}
	return nil
}

// Result is a normalization outcome.
type Result struct {
	Data     []byte
	MIMEType string
	Width    int
	Height   int
	// Changed is false when the input already fit and was returned verbatim.
	Changed bool
}

// Base64 renders the result for a provider payload.
func (r Result) Base64() string { return base64.StdEncoding.EncodeToString(r.Data) }

// Normalize downscales and re-encodes until the image fits both budgets.
//
// An image that already fits is returned BYTE-FOR-BYTE with Changed false. Re-
// encoding a conforming image would spend CPU to make it slightly worse, and
// it would make the normalizer non-idempotent — a session that reloads its
// history would recompress every image it had ever seen, once per reload.
func Normalize(data []byte, mime string) (Result, error) {
	if mime == "" {
		var ok bool
		if mime, ok = Sniff(data); !ok {
			return Result{}, ErrNotAnImage
		}
	}
	if err := Validate(data, mime); err != nil {
		return Result{}, err
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("imagex: %w", err)
	}
	if cfg.Width <= MaxDimension && cfg.Height <= MaxDimension && fitsBudget(len(data)) {
		return Result{Data: data, MIMEType: mime, Width: cfg.Width, Height: cfg.Height}, nil
	}

	img, err := decode(data, mime)
	if err != nil {
		return Result{}, fmt.Errorf("imagex: %w", err)
	}

	img = fitWithin(img, MaxDimension)
	for halving := 0; ; halving++ {
		for _, q := range QualityLadder {
			out, encMime, err := encode(img, mime, q)
			if err != nil {
				return Result{}, err
			}
			if fitsBudget(len(out)) {
				b := img.Bounds()
				return Result{Data: out, MIMEType: encMime,
					Width: b.Dx(), Height: b.Dy(), Changed: true}, nil
			}
		}
		if halving >= maxHalvings {
			return Result{}, fmt.Errorf(
				"imagex: cannot fit %dx%d within %d base64 bytes after the quality "+
					"ladder and %d halvings", cfg.Width, cfg.Height, MaxBase64Bytes, maxHalvings)
		}
		b := img.Bounds()
		img = scaleTo(img, max(1, b.Dx()/2), max(1, b.Dy()/2))
	}
}

// FitsBudget reports whether n decoded bytes fit the budget once base64-encoded.
//
// Exported because the rule is easy to get wrong in exactly one direction, and
// wrong quietly: base64 is 4/3 the size of what it encodes, so a caller
// comparing DECODED bytes against MaxBase64Bytes ships an image a third larger
// than the limit it believes it checked. Nothing about the resulting provider
// error names base64.
func FitsBudget(n int) bool { return base64.StdEncoding.EncodedLen(n) <= MaxBase64Bytes }

func fitsBudget(n int) bool { return FitsBudget(n) }

func decode(data []byte, mime string) (image.Image, error) {
	switch mime {
	case MIMEJPEG:
		return jpeg.Decode(bytes.NewReader(data))
	case MIMEPNG:
		return png.Decode(bytes.NewReader(data))
	case MIMEGIF:
		// The first frame. A multi-frame GIF is not rejected the way an APNG
		// is — providers accept GIF — but only one frame can be sent.
		return gif.Decode(bytes.NewReader(data))
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, mime)
}

// encode re-encodes at a quality step.
//
// PNG and GIF have no quality knob, so a PNG that must shrink becomes a JPEG:
// that IS the ladder for them. The alpha channel is lost, which is why it
// happens only after the size budget has already been missed — a PNG that fits
// stays a PNG.
func encode(img image.Image, srcMime string, quality int) ([]byte, string, error) {
	var buf bytes.Buffer
	if srcMime == MIMEPNG || srcMime == MIMEGIF {
		if quality == QualityLadder[0] {
			if err := png.Encode(&buf, img); err != nil {
				return nil, "", err
			}
			if fitsBudget(buf.Len()) {
				return buf.Bytes(), MIMEPNG, nil
			}
			buf.Reset()
		}
	}
	if err := jpeg.Encode(&buf, flatten(img), &jpeg.Options{Quality: quality}); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), MIMEJPEG, nil
}

// flatten composites onto white before a JPEG encode.
//
// JPEG has no alpha. Encoding a transparent PNG directly leaves Go to drop the
// channel, which renders transparent pixels BLACK — a screenshot with a
// transparent background arrives as a black rectangle, and the model reports
// it cannot see anything.
func flatten(img image.Image) image.Image {
	if op, ok := img.(interface{ Opaque() bool }); ok && op.Opaque() {
		return img
	}
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Over)
	return out
}

// fitWithin scales so neither side exceeds limit, preserving aspect ratio.
func fitWithin(img image.Image, limit int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= limit && h <= limit {
		return img
	}
	if w >= h {
		return scaleTo(img, limit, max(1, h*limit/w))
	}
	return scaleTo(img, max(1, w*limit/h), limit)
}

// scaleTo resamples with a BOX FILTER: each destination pixel averages the
// source pixels it covers.
//
// Nearest-neighbour is three lines shorter and produces aliasing that destroys
// exactly what these images usually carry — a screenshot of text. Averaging is
// the cheapest filter that keeps small type legible, which is the whole reason
// the image is being sent.
func scaleTo(src image.Image, dw, dh int) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if dw <= 0 || dh <= 0 || sw <= 0 || sh <= 0 {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))

	for y := 0; y < dh; y++ {
		y0 := sb.Min.Y + y*sh/dh
		y1 := sb.Min.Y + (y+1)*sh/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0 := sb.Min.X + x*sw/dw
			x1 := sb.Min.X + (x+1)*sw/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					pr, pg, pb, pa := src.At(sx, sy).RGBA()
					r += uint64(pr)
					g += uint64(pg)
					b += uint64(pb)
					a += uint64(pa)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.Set(x, y, color.RGBA64{
				R: uint16(r / n), G: uint16(g / n), B: uint16(b / n), A: uint16(a / n),
			})
		}
	}
	return dst
}
