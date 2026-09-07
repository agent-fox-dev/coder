package wire_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/agentfox/agentkit-go/wire"
)

// The framed readers are the first thing untrusted bytes touch on an MCP
// transport, before Guard or Parse ever sees a message. NFR-TEST-09 property
// 1 — nothing panics, whatever the input — is asserted here for both framings.
// Properties 2 and 3 do not apply: a frame is a byte slice, not a decoded
// value, and FuzzGuardNeverPanics covers the JSON inside it.
//
// The provider package's SSE decoder is the third REQ-SEC-11 reader. It is
// exported (provider.NewSSEReader) but lives outside this package, and wire
// must not take a test-time dependency on the package that depends on it, so
// its target belongs beside it rather than here.
//
// Seed corpora live under testdata/fuzz/<Target>/ and are deliberately small:
// the seeds only have to reach every branch once, and the fuzzer does the
// rest.

// drain reads frames until the reader stops, asserting the invariants that
// hold on every path: a poisoned reader keeps returning the error that tore
// it down, and a clean EOF does not poison it (REQ-SEC-11.4).
func drain(t *testing.T, r *wire.FrameReader, b []byte) {
	t.Helper()
	const maxFrames = 1 << 12 // the limit bounds frame size, not frame count
	for i := 0; i < maxFrames; i++ {
		_, err := r.Next()
		if err == nil {
			continue
		}
		if err == io.EOF {
			if r.Poisoned() {
				t.Fatalf("a clean EOF poisoned the reader on %q", b)
			}
			return
		}
		if !r.Poisoned() {
			t.Fatalf("Next returned %v on %q but the reader does not report itself poisoned", err, b)
		}
		if !errors.Is(err, wire.ErrRejected) && !isIOError(err) {
			t.Fatalf("Next returned %v (%T) on %q; every rejection must match ErrRejected", err, err, b)
		}
		if _, again := r.Next(); again == nil || again.Error() != err.Error() {
			t.Fatalf("a poisoned reader resynchronized on %q: first %v, then %v", b, err, again)
		}
		return
	}
}

// isIOError admits the one non-rejection error the readers can surface: a
// short read from the underlying stream, which is the peer hanging up, not a
// malformed message.
func isIOError(err error) bool {
	return err == io.ErrUnexpectedEOF
}

// FuzzNDJSONFrameReaderNeverPanics is NFR-TEST-09.1 for MCP stdio's framing.
func FuzzNDJSONFrameReaderNeverPanics(f *testing.F) {
	for _, s := range []string{
		"", "\n", "\n\n", `{"a":1}` + "\n", `{"a":1}`, "\r\n", "  \n{}\n",
		`{"a":1}` + "\n" + `{"b":2}` + "\n", "\x00\n", string(bytes.Repeat([]byte("x"), 300)) + "\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r := wire.NewNDJSON(bytes.NewReader(b), wire.Limits{MaxMessageBytes: 256})
		drain(t, r, b)
	})
}

// FuzzContentLengthFrameReaderNeverPanics is NFR-TEST-09.1 for LSP-style
// header framing, where the declared length is the input a peer controls.
func FuzzContentLengthFrameReaderNeverPanics(f *testing.F) {
	for _, s := range []string{
		"", "\r\n", "Content-Length: 2\r\n\r\n{}",
		"Content-Length: 2\r\n\r\n{}Content-Length: 3\r\n\r\n[1]",
		"content-length:0\r\n\r\n", "Content-Length: -1\r\n\r\n",
		"Content-Length: 18446744073709551615\r\n\r\n", "Content-Length: 9223372036854775808\r\n\r\n",
		"Content-Length: 5\r\n\r\n{", "Content-Type: x\r\n\r\n", "no colon\r\n\r\n",
		"Content-Length: 2\nContent-Length: 1\n\n{}", "Content-Length: 300\r\n\r\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r := wire.NewContentLength(bytes.NewReader(b), wire.Limits{MaxMessageBytes: 256})
		drain(t, r, b)
	})
}
