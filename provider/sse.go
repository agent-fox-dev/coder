package provider

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// ErrSSETruncated is the mid-stream truncation of REQ-PROV-04.
//
// Its text is deliberately the phrase REQ-PROV-14's retryable allowlist
// matches ("stream ended before message_stop"). That is a real coupling, not a
// coincidence: a truncated SSE body is a 200 with a complete-looking prefix,
// so the transport layer cannot see it and only the semantic layer can. The
// two layers meet through this string, and changing it silently disables the
// retry for the single commonest streaming failure there is.
var ErrSSETruncated = errors.New("agentkit: stream ended before message_stop")

// MaxSSEEventBytes bounds one accumulated event. A provider response is bytes
// AgentKit did not produce, and REQ-SEC-11's argument — bound before you
// allocate — applies to a compromised or malfunctioning gateway just as it
// does to an MCP peer.
const MaxSSEEventBytes = 16 << 20

// ErrSSEEventTooLarge is the bound breach.
var ErrSSEEventTooLarge = errors.New("agentkit: SSE event exceeds the size bound")

// SSEEvent is one dispatched server-sent event.
type SSEEvent struct {
	Type string
	Data []byte
}

// SSEReader decodes text/event-stream.
//
// It implements the subset the model APIs actually use: `event:` and `data:`
// fields, multiple data lines joined with "\n", blank line dispatches, `:`
// comment lines ignored. It deliberately does NOT implement `id:`, `retry:`,
// or last-event-id reconnection, because REQ-OBS-09 forbids replay: a consumer
// that lost events must rebuild from a snapshot, so a reconnecting reader
// would produce exactly the duplicate-application the requirement rules out.
type SSEReader struct {
	br   *bufio.Reader
	max  int
	data bytes.Buffer
	typ  string
}

func NewSSEReader(r io.Reader, max int) *SSEReader {
	if max <= 0 {
		max = MaxSSEEventBytes
	}
	return &SSEReader{br: bufio.NewReaderSize(r, 64<<10), max: max}
}

// Next returns the next dispatched event, or io.EOF at a clean end of stream.
//
// A partially accumulated event at EOF is DISCARDED rather than dispatched.
// Dispatching it would hand the decoder a half-line of JSON to parse, and the
// resulting parse error names the wrong cause; the caller detects truncation
// by not having seen its terminal event, which is what ErrSSETruncated is for.
func (r *SSEReader) Next() (SSEEvent, error) {
	for {
		line, err := r.readLine()
		if err != nil {
			return SSEEvent{}, err
		}

		// A blank line dispatches whatever has accumulated.
		if len(line) == 0 {
			if r.data.Len() == 0 && r.typ == "" {
				continue
			}
			ev := SSEEvent{Type: r.typ, Data: append([]byte(nil), r.data.Bytes()...)}
			r.data.Reset()
			r.typ = ""
			return ev, nil
		}

		if line[0] == ':' { // comment / keep-alive
			continue
		}

		field, value := splitField(line)
		switch string(field) {
		case "event":
			r.typ = string(value)
		case "data":
			if r.data.Len()+len(value)+1 > r.max {
				return SSEEvent{}, fmt.Errorf("%w: over %d bytes", ErrSSEEventTooLarge, r.max)
			}
			if r.data.Len() > 0 {
				r.data.WriteByte('\n')
			}
			r.data.Write(value)
		}
		// Every other field, `id` and `retry` included, is ignored.
	}
}

// readLine reads one line without its terminator, bounded by max.
func (r *SSEReader) readLine() ([]byte, error) {
	var out []byte
	for {
		chunk, more, err := r.br.ReadLine()
		if err != nil {
			return nil, err
		}
		if out == nil && !more {
			return chunk, nil
		}
		if len(out)+len(chunk) > r.max {
			return nil, fmt.Errorf("%w: line over %d bytes", ErrSSEEventTooLarge, r.max)
		}
		out = append(out, chunk...)
		if !more {
			return out, nil
		}
	}
}

// splitField splits "name: value" per the SSE grammar: the field name runs to
// the first colon, and exactly one leading space of the value is stripped. A
// line with no colon is a field name with an empty value.
func splitField(line []byte) (name, value []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return line, nil
	}
	name, value = line[:i], line[i+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return name, value
}
