package provider_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/agentfox/agentkit-go/provider"
)

// FuzzSSEReaderNeverPanics is NFR-TEST-09 property 1 applied to the last
// untrusted decoder that lacked a fuzz target.
//
// The SSE reader sits on a provider response body — bytes AgentKit did not
// produce, arriving from a vendor or whatever gateway is between us and one
// — which is exactly the surface REQ-SEC-11 governs. Properties 2 and 3
// (re-encodable, fixed point) do not apply here: this decoder produces
// framing, not a document, and there is nothing to re-encode.
//
// What it must hold instead is the framing contract the streaming providers
// rely on, and both halves have been real bugs: a truncated final event is
// DISCARDED rather than dispatched (a half-arrived event is not an event),
// and no single field may be buffered without bound.
func FuzzSSEReaderNeverPanics(f *testing.F) {
	for _, s := range []string{
		"data: {}\n\n",
		"event: message_start\ndata: {\"a\":1}\n\n",
		": a comment\ndata: x\n\n",
		"data: a\ndata: b\n\n",
		"data:no-space\n\n",
		"data: [DONE]\n\n",
		"data: {\"a\":1}",        // truncated: no terminating blank line
		"event: x\n",             // truncated mid-event
		"\n\n\n",                 // empty events
		"data: \xff\xfe\n\n",     // invalid UTF-8
		"\r\ndata: crlf\r\n\r\n", // CRLF framing
		"id: 1\nretry: 100\ndata: x\n\n",
		strings.Repeat("data: x\n", 200) + "\n",
	} {
		f.Add([]byte(s), 1<<20)
	}

	f.Fuzz(func(t *testing.T, data []byte, max int) {
		// The cap is the thing under test, so let the fuzzer move it, but
		// keep it sane: a non-positive cap is a caller bug, not a wire input.
		if max <= 0 {
			max = 1 << 20
		}
		if max > 1<<22 {
			max = 1 << 22
		}
		r := provider.NewSSEReader(bytes.NewReader(data), max)
		for i := 0; i < 10000; i++ {
			ev, err := r.Next()
			if err != nil {
				// Every terminal condition must be an ERROR, never a panic
				// and never an infinite loop. io.EOF ends a well-framed
				// stream; anything else is a refusal, which is the correct
				// answer for a stream we cannot trust.
				if errors.Is(err, io.EOF) {
					return
				}
				return
			}
			// A dispatched event never exceeds the cap it was read under:
			// that bound is what stops a peer's unterminated `data:` field
			// from being a free allocation primitive (REQ-SEC-11.2).
			if len(ev.Data) > max {
				t.Fatalf("event data %d bytes exceeds the %d cap it was read under", len(ev.Data), max)
			}
		}
		t.Fatal("the reader yielded 10000 events without ending; a bounded input must terminate")
	})
}
