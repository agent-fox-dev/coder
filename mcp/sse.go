package mcp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/agentfox/agentkit-go/wire"
)

// This is a second SSE reader in the tree: provider/sse.go has one already.
//
// They are not the same problem. The provider reader is tuned to a completion
// stream — it knows what a truncated one looks like (`message_stop` never
// arrived) and reports ErrSSETruncated so the retry middleware can act. MCP's
// use has no terminal event: a stream is a transport that stays open for the
// life of a session, its `event:` names select behaviour (`endpoint` vs
// `message`), and `id:` matters for resumption. Sharing one reader would mean
// a parameter for every one of those differences. The layering says the same
// thing: `mcp` depends on core and wire, and reaching into `provider` for
// sixty lines would drag catalog in behind it.

// ErrSSEEventTooLarge is a bound, not a parse failure. A server that streams
// an unbounded `data:` field is asking us to buffer it, and REQ-SEC-11.2 says
// a decoder bounds itself before it allocates.
var ErrSSEEventTooLarge = errors.New("mcp: sse event exceeds the configured limit")

// sseEvent is one dispatched event.
//
// There is no id field. `id:` exists so a client can resume a broken stream
// with Last-Event-ID, and nothing here reconnects — so an id would be a value
// we store, never send, and quietly imply we support resumption with. It is
// parsed and discarded, like `retry:`.
type sseEvent struct {
	name string // `event:`, empty for the default
	data []byte // `data:` lines joined with \n
}

// sseDecoder reads the text/event-stream framing.
type sseDecoder struct {
	sc    *bufio.Scanner
	max   int
	first bool
}

func newSSEDecoder(r io.Reader, max int) *sseDecoder {
	if max <= 0 {
		max = int(wire.Defaults().MaxMessageBytes)
	}
	sc := bufio.NewScanner(r)
	// One line may be the whole event, so the line bound is the event bound.
	// The +1 leaves room for the scanner to detect the overrun rather than
	// returning a token that exactly fills the buffer.
	sc.Buffer(make([]byte, 0, 4096), max+1)
	return &sseDecoder{sc: sc, max: max, first: true}
}

// next returns the next dispatched event, or io.EOF at a clean end of stream.
func (d *sseDecoder) next() (sseEvent, error) {
	var (
		ev    sseEvent
		data  bytes.Buffer
		saw   bool // any field seen since the last dispatch
		total int
	)
	for d.sc.Scan() {
		line := d.sc.Bytes()
		if d.first {
			// A UTF-8 BOM is legal at the head of an event stream and is not
			// part of the first field name.
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
			d.first = false
		}
		line = bytes.TrimSuffix(line, []byte("\r"))

		if len(line) == 0 {
			if !saw {
				continue // a blank line with nothing pending dispatches nothing
			}
			ev.data = data.Bytes()
			return ev, nil
		}
		if line[0] == ':' {
			// A comment — how servers keep an idle connection open. The field
			// switch below would ignore it anyway (its name parses as empty),
			// so this is the spec's rule written down rather than the only
			// thing preventing a keep-alive from becoming data.
			continue
		}

		field, value := splitSSEField(line)
		saw = true
		switch string(field) {
		case "event":
			ev.name = string(value)
		case "data":
			total += len(value) + 1
			if total > d.max {
				return sseEvent{}, fmt.Errorf("%w (%d bytes)", ErrSSEEventTooLarge, total)
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(value)
		case "id", "retry":
			// Both belong to reconnection, which this transport does not do.
			// They are matched rather than ignored so they cannot be mistaken
			// for data, and discarded rather than stored so nothing here looks
			// like support for resuming a stream.
		}
	}

	if err := d.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return sseEvent{}, fmt.Errorf("%w: a single line exceeded %d bytes",
				ErrSSEEventTooLarge, d.max)
		}
		return sseEvent{}, err
	}
	if saw {
		// The stream ended mid-event. Dispatching a half-read event would hand
		// the decoder above a truncated JSON-RPC frame, which it would then
		// treat as a malformed one and tear the session down for.
		return sseEvent{}, io.ErrUnexpectedEOF
	}
	return sseEvent{}, io.EOF
}

// splitSSEField splits `name: value`, dropping ONE optional leading space from
// the value, per the spec. A second space is data.
func splitSSEField(line []byte) (field, value []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return line, nil // a bare field name means an empty value
	}
	field, value = line[:i], line[i+1:]
	value = bytes.TrimPrefix(value, []byte(" "))
	return field, value
}

// isHeaderSafe reports whether s is safe to place in an HTTP header value:
// visible ASCII and spaces only. Go's transport rejects control characters
// too, but it does so at write time with an opaque error; refusing here names
// the field that was wrong.
func isHeaderSafe(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
