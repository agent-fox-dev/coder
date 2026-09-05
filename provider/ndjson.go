package provider

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// NDJSONReader reads newline-delimited JSON: Ollama's native streaming shape,
// and the same framing MCP stdio uses.
//
// It is bounded for the reason REQ-SEC-11 gives: these are bytes AgentKit did
// not produce, the length is peer-declared by where the peer puts a newline,
// and bufio.Scanner's own default 64 KiB limit fails a legitimately large
// message by returning a bare "token too long" with no indication that a
// message was dropped.
type NDJSONReader struct {
	br  *bufio.Reader
	max int
}

func NewNDJSONReader(r io.Reader, max int) *NDJSONReader {
	if max <= 0 {
		max = MaxSSEEventBytes
	}
	return &NDJSONReader{br: bufio.NewReaderSize(r, 64<<10), max: max}
}

// Next returns the next non-empty line, or io.EOF at the end of the stream.
//
// A trailing partial line at EOF is DISCARDED, for the same reason the SSE
// reader discards a partial event: handing half a line to a JSON decoder
// produces a parse error that names the wrong cause.
func (r *NDJSONReader) Next() ([]byte, error) {
	for {
		var out []byte
		for {
			chunk, more, err := r.br.ReadLine()
			if err != nil {
				return nil, err
			}
			if len(out)+len(chunk) > r.max {
				return nil, fmt.Errorf("%w: line over %d bytes", ErrSSEEventTooLarge, r.max)
			}
			out = append(out, chunk...)
			if !more {
				break
			}
		}
		if line := bytes.TrimSpace(out); len(line) > 0 {
			return line, nil
		}
	}
}
