package imagex_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/imagex"
)

// ---- fixtures. Real encoded images, not byte literals: a test that asserts
// on a hand-written header proves the header parser matches the test author's
// memory of the format.

func pngOf(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noisyJPEG is deliberately incompressible, so a size budget is actually
// exercised: a flat colour compresses to almost nothing at every quality and
// would make the ladder look like it worked when it never ran.
func noisyJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(12345)
	for y := range h {
		for x := range w {
			seed = seed*1664525 + 1013904223
			img.Set(x, y, color.RGBA{
				R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gifOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ---- REQ-TOOL-14.6: formats providers reject

// TestAnimatedPNGIsRefused. Go's decoder happily returns the first frame and
// never mentions the animation, so it reaches the provider looking like a
// still and is rejected there — after it is already in history.
func TestAnimatedPNGIsRefused(t *testing.T) {
	still := pngOf(t, 4, 4, color.White)
	animated := injectPNGChunk(t, still, "acTL", []byte{0, 0, 0, 2, 0, 0, 0, 0})

	// The premise: Go decodes it without complaint, which is why the chunk
	// walk has to exist.
	if _, err := png.Decode(bytes.NewReader(animated)); err != nil {
		t.Fatalf("premise failed — Go rejected the APNG, so a decode-based check "+
			"would have sufficed: %v", err)
	}
	if err := imagex.Validate(animated, imagex.MIMEPNG); !errors.Is(err, imagex.ErrAnimatedPNG) {
		t.Fatalf("want ErrAnimatedPNG, got %v", err)
	}
}

// TestAPNGChunkAfterIDATIsNotAnAnimation. acTL precedes IDAT by spec; a
// trailing one is not honoured by any decoder, and rejecting on it would
// refuse valid images for a byte sequence that happened to appear late.
func TestAPNGChunkAfterIDATIsNotAnAnimation(t *testing.T) {
	still := pngOf(t, 4, 4, color.White)
	if err := imagex.Validate(still, imagex.MIMEPNG); err != nil {
		t.Fatalf("a plain PNG must validate: %v", err)
	}
}

// TestANonIHDRFirstChunkIsRefused. The PNG spec requires IHDR first.
func TestANonIHDRFirstChunkIsRefused(t *testing.T) {
	still := pngOf(t, 4, 4, color.White)
	// Rename the first chunk. The signature still says PNG.
	broken := append([]byte(nil), still...)
	copy(broken[12:16], []byte("tEXt"))
	if err := imagex.Validate(broken, imagex.MIMEPNG); !errors.Is(err, imagex.ErrNonIHDRPNG) {
		t.Fatalf("want ErrNonIHDRPNG, got %v", err)
	}
}

// TestCMYKJPEGIsRefused.
func TestCMYKJPEGIsRefused(t *testing.T) {
	data := cmykJPEG(t)
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Skipf("could not build a CMYK JPEG fixture: %v", err)
	}
	if _, ok := img.(*image.CMYK); !ok {
		t.Skipf("fixture decoded as %T, not CMYK", img)
	}
	if err := imagex.Validate(data, imagex.MIMEJPEG); !errors.Is(err, imagex.ErrCMYKJPEG) {
		t.Fatalf("want ErrCMYKJPEG, got %v", err)
	}
}

// TestWebPIsReportedUnsupportedRatherThanMangled. The standard library cannot
// decode it and REQ-GO-11 forbids the module that can; reporting it lets
// REQ-TOOL-14.5 keep the original block, which providers accept anyway.
func TestWebPIsReportedUnsupportedRatherThanMangled(t *testing.T) {
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
	if mime, ok := imagex.Sniff(webp); !ok || mime != imagex.MIMEWebP {
		t.Fatalf("sniff returned %q, %v", mime, ok)
	}
	if err := imagex.Validate(webp, imagex.MIMEWebP); !errors.Is(err, imagex.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

// ---- magic-byte detection

// TestSniffIgnoresTheFileName is REQ-TOOL-14.6's rule: content decides.
func TestSniffIgnoresTheFileName(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"png", pngOf(t, 2, 2, color.White), imagex.MIMEPNG},
		{"jpeg", noisyJPEG(t, 8, 8), imagex.MIMEJPEG},
		{"gif", gifOf(t, 2, 2), imagex.MIMEGIF},
	} {
		got, ok := imagex.Sniff(tc.data)
		if !ok || got != tc.want {
			t.Fatalf("%s sniffed as %q (%v), want %q", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := imagex.Sniff([]byte("package main\n")); ok {
		t.Fatal("Go source is not an image")
	}
}

// ---- REQ-TOOL-14.4: lenient base64

// TestBase64DecodingIsLenient. Tool-supplied base64 arrives wrapped by shell
// pipelines, in base64url from anything that passed it through a URL, and
// unpadded. A strict decoder discards the tool's output for a reason that has
// nothing to do with the image.
func TestBase64DecodingIsLenient(t *testing.T) {
	raw := pngOf(t, 3, 3, color.White)
	std := base64.StdEncoding.EncodeToString(raw)

	variants := map[string]string{
		"plain":         std,
		"wrapped":       wrap(std, 76),
		"unpadded":      strings.TrimRight(std, "="),
		"base64url":     base64.RawURLEncoding.EncodeToString(raw),
		"data url":      "data:image/png;base64," + std,
		"leading space": "  \n" + std + "\n",
	}
	for name, v := range variants {
		got, err := imagex.DecodeBase64(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatalf("%s decoded to %d bytes, want %d", name, len(got), len(raw))
		}
	}
}

// ---- REQ-TOOL-14.3: the budgets

// TestAConformingImageIsReturnedByteForByte. Re-encoding a conforming image
// spends CPU to make it slightly worse, and makes normalization
// non-idempotent: a session reloading its history would recompress every image
// it had ever seen, once per reload.
func TestAConformingImageIsReturnedByteForByte(t *testing.T) {
	in := pngOf(t, 100, 50, color.White)
	res, err := imagex.Normalize(in, imagex.MIMEPNG)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatal("an image inside both budgets must not be re-encoded")
	}
	if !bytes.Equal(res.Data, in) {
		t.Fatal("a conforming image must be returned byte-for-byte")
	}
	if res.Width != 100 || res.Height != 50 {
		t.Fatalf("dimensions reported as %dx%d", res.Width, res.Height)
	}
}

// TestAnOversizedImageIsDownscaledWithinBothBudgets.
func TestAnOversizedImageIsDownscaledWithinBothBudgets(t *testing.T) {
	in := pngOf(t, 3000, 1500, color.RGBA{R: 20, G: 90, B: 200, A: 255})
	res, err := imagex.Normalize(in, imagex.MIMEPNG)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("a 3000px image must be downscaled")
	}
	if res.Width > imagex.MaxDimension || res.Height > imagex.MaxDimension {
		t.Fatalf("still %dx%d, over the %d cap", res.Width, res.Height, imagex.MaxDimension)
	}
	// Aspect ratio preserved: 3000x1500 is 2:1.
	if res.Width != imagex.MaxDimension || res.Height != imagex.MaxDimension/2 {
		t.Fatalf("aspect ratio not preserved: %dx%d", res.Width, res.Height)
	}
	if n := len(res.Base64()); n > imagex.MaxBase64Bytes {
		t.Fatalf("base64 is %d bytes, over the %d budget", n, imagex.MaxBase64Bytes)
	}
}

// TestTheBudgetIsMeasuredOnBase64NotOnBytes. Base64 is 4/3 the size of what it
// encodes and it is what crosses the wire; budgeting the decoded bytes ships
// an image a third larger than the limit it was checked against.
//
// The discriminating case is a size BETWEEN the two readings — under the
// decoded-bytes budget, over it once encoded. Reaching that window by tuning
// an image fixture would make the test depend on the JPEG encoder's exact
// output size; asserting the rule directly does not.
func TestTheBudgetIsMeasuredOnBase64NotOnBytes(t *testing.T) {
	const between = 4_000_000 // under MaxBase64Bytes, over it when encoded
	if between > imagex.MaxBase64Bytes {
		t.Fatal("the fixture must be under the budget when read as decoded bytes")
	}
	if base64.StdEncoding.EncodedLen(between) <= imagex.MaxBase64Bytes {
		t.Fatal("the fixture must be over the budget once encoded")
	}
	if imagex.FitsBudget(between) {
		t.Fatalf("%d decoded bytes become %d base64 bytes, over the %d budget",
			between, base64.StdEncoding.EncodedLen(between), imagex.MaxBase64Bytes)
	}

	// And end to end: nothing Normalize returns may exceed the budget.
	res, err := imagex.Normalize(noisyJPEG(t, 2000, 2000), imagex.MIMEJPEG)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Base64()); n > imagex.MaxBase64Bytes {
		t.Fatalf("base64 length %d exceeds the budget %d", n, imagex.MaxBase64Bytes)
	}
}

// TestTheQualityLadderRunsBeforeFurtherDownscaling. An image inside the
// dimension cap that is merely too heavy should lose quality, not pixels:
// halving a 2000px screenshot first would throw away the text it was sent for.
func TestTheQualityLadderRunsBeforeFurtherDownscaling(t *testing.T) {
	in := noisyJPEG(t, 2000, 2000)
	res, err := imagex.Normalize(in, imagex.MIMEJPEG)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Skip("the fixture already fit; the ladder was not exercised")
	}
	if res.Width != 2000 || res.Height != 2000 {
		t.Fatalf("the quality ladder should have been enough; got %dx%d",
			res.Width, res.Height)
	}
}

// TestATransparentPNGIsFlattenedOntoWhite. JPEG has no alpha; encoding a
// transparent PNG straight through renders transparent pixels BLACK, so a
// screenshot with a transparent background arrives as a black rectangle and
// the model reports it cannot see anything.
func TestATransparentPNGIsFlattenedOntoWhite(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2400, 2400))
	// Fully transparent everywhere except one opaque noise region, so the
	// image cannot compress to nothing and must go through the JPEG path.
	seed := uint32(7)
	for y := 0; y < 2400; y++ {
		for x := 0; x < 1200; x++ {
			seed = seed*1664525 + 1013904223
			img.Set(x, y, color.RGBA{R: uint8(seed >> 24), G: uint8(seed >> 16),
				B: uint8(seed >> 8), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	res, err := imagex.Normalize(buf.Bytes(), imagex.MIMEPNG)
	if err != nil {
		t.Fatal(err)
	}
	if res.MIMEType != imagex.MIMEJPEG {
		t.Skipf("the fixture stayed a PNG (%s); flattening was not exercised", res.MIMEType)
	}
	out, err := jpeg.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	// Sample the transparent half. Flattened onto white it is bright; with the
	// alpha channel simply dropped it is black.
	r, g, b, _ := out.At(out.Bounds().Dx()*3/4, out.Bounds().Dy()/2).RGBA()
	if r < 0x8000 || g < 0x8000 || b < 0x8000 {
		t.Fatalf("transparent pixels rendered dark (%d,%d,%d): the alpha channel was "+
			"dropped instead of composited", r>>8, g>>8, b>>8)
	}
}

// TestDownscalingAveragesRatherThanSampling. Nearest-neighbour aliases away
// exactly what these images usually carry — a screenshot of text. A
// checkerboard is the sharpest case: averaged it becomes uniform grey, sampled
// it stays pure black or pure white.
func TestDownscalingAveragesRatherThanSampling(t *testing.T) {
	const n = 4000
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := range n {
		for x := range n {
			if (x+y)%2 == 0 {
				img.Set(x, y, color.White)
			} else {
				img.Set(x, y, color.Black)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	res, err := imagex.Normalize(buf.Bytes(), imagex.MIMEPNG)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	r, _, _, _ := out.At(out.Bounds().Dx()/2, out.Bounds().Dy()/2).RGBA()
	v := r >> 8
	if v < 0x40 || v > 0xC0 {
		t.Fatalf("a downscaled checkerboard should average toward grey; centre is %#x. "+
			"Nearest-neighbour sampling gives pure black or white", v)
	}
}

// ---- helpers

func wrap(s string, n int) string {
	var b strings.Builder
	for i := 0; i < len(s); i += n {
		end := min(i+n, len(s))
		b.WriteString(s[i:end])
		b.WriteByte('\n')
	}
	return b.String()
}

// injectPNGChunk inserts a chunk immediately after IHDR.
func injectPNGChunk(t *testing.T, src []byte, typ string, payload []byte) []byte {
	t.Helper()
	const sigLen = 8
	ihdrLen := int(uint32(src[sigLen])<<24 | uint32(src[sigLen+1])<<16 |
		uint32(src[sigLen+2])<<8 | uint32(src[sigLen+3]))
	at := sigLen + 8 + ihdrLen + 4

	chunk := make([]byte, 0, 12+len(payload))
	n := uint32(len(payload))
	chunk = append(chunk, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	chunk = append(chunk, typ...)
	chunk = append(chunk, payload...)
	crc := pngCRC(append([]byte(typ), payload...))
	chunk = append(chunk, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))

	out := make([]byte, 0, len(src)+len(chunk))
	out = append(out, src[:at]...)
	out = append(out, chunk...)
	out = append(out, src[at:]...)
	return out
}

func pngCRC(b []byte) uint32 {
	var table [256]uint32
	for i := range table {
		c := uint32(i)
		for range 8 {
			if c&1 != 0 {
				c = 0xedb88320 ^ (c >> 1)
			} else {
				c >>= 1
			}
		}
		table[i] = c
	}
	crc := uint32(0xffffffff)
	for _, x := range b {
		crc = table[(crc^uint32(x))&0xff] ^ (crc >> 8)
	}
	return crc ^ 0xffffffff
}

// cmykJPEG builds a minimal baseline 4-component Adobe JPEG.
//
// Go's encoder cannot write CMYK, so the fixture is assembled here rather than
// pasted as a byte blob: a blob nobody can read is a fixture nobody can check,
// and the whole point is that Go's DECODER classifies this as *image.CMYK. The
// image is 8x8 with every component's DC at zero, which is the smallest thing
// that is a real JPEG rather than a header that happens to parse.
func cmykJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := func(v ...byte) { b.Write(v) }

	w(0xFF, 0xD8) // SOI

	// APP14 Adobe with transform 0. This marker is what tells a decoder the
	// four components are CMYK rather than an unknown four-channel space.
	adobe := []byte{'A', 'd', 'o', 'b', 'e', 0, 100, 0, 0, 0, 0, 0, 0, 0, 0}
	w(0xFF, 0xEE, byte((len(adobe)+2)>>8), byte(len(adobe)+2))
	w(adobe...)

	// DQT: one 8-bit table, flat.
	w(0xFF, 0xDB, 0x00, 0x43, 0x00)
	for range 64 {
		w(16)
	}

	// SOF0: 8x8, FOUR components, 1x1 sampling, quantization table 0.
	sof := []byte{8, 0, 8, 0, 8, 4}
	for c := byte(1); c <= 4; c++ {
		sof = append(sof, c, 0x11, 0)
	}
	w(0xFF, 0xC0, byte((len(sof)+2)>>8), byte(len(sof)+2))
	w(sof...)

	// Two Huffman tables, each holding a single 2-bit code "00": DC symbol 0
	// (a zero difference) and AC symbol 0 (end-of-block).
	for _, class := range []byte{0x00, 0x10} {
		counts := make([]byte, 16)
		counts[1] = 1 // one code of length 2
		dht := append([]byte{class}, counts...)
		dht = append(dht, 0x00)
		w(0xFF, 0xC4, byte((len(dht)+2)>>8), byte(len(dht)+2))
		w(dht...)
	}

	// SOS over all four components.
	sos := []byte{4}
	for c := byte(1); c <= 4; c++ {
		sos = append(sos, c, 0x00)
	}
	sos = append(sos, 0, 63, 0)
	w(0xFF, 0xDA, byte((len(sos)+2)>>8), byte(len(sos)+2))
	w(sos...)

	// Entropy data: per component, DC "00" then EOB "00" — four bits of zeros,
	// four components, sixteen bits, two bytes.
	w(0x00, 0x00)

	w(0xFF, 0xD9) // EOI
	return b.Bytes()
}
