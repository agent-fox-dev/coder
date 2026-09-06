package wire

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
)

// FrameReader reads one untrusted message at a time, bounded before it
// allocates (REQ-SEC-11).
//
// It is POISONED by its first malformed message (REQ-SEC-11.4): once a frame
// is rejected, every later call returns the same error and no attempt is made
// to resynchronize. Resynchronizing a framed stream after a parse error means
// guessing where the next frame starts, using framing that has already proved
// untrustworthy — and a peer that can desynchronize the framing can then
// choose what AgentKit reads as a message boundary.
type FrameReader struct {
	br     *bufio.Reader
	lim    Limits
	header bool // Content-Length framing rather than newline-delimited
	err    error
}

// NewNDJSON reads newline-delimited JSON: MCP stdio's framing.
func NewNDJSON(r io.Reader, l Limits) *FrameReader {
	return &FrameReader{br: bufio.NewReaderSize(r, 64<<10), lim: l.withDefaults()}
}

// NewContentLength reads LSP-style `Content-Length: N` framing.
func NewContentLength(r io.Reader, l Limits) *FrameReader {
	return &FrameReader{br: bufio.NewReaderSize(r, 64<<10), lim: l.withDefaults(), header: true}
}

// Next returns the next frame's bytes, or io.EOF at a clean end of stream.
func (f *FrameReader) Next() ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	var (
		b   []byte
		err error
	)
	if f.header {
		b, err = f.nextHeaderFramed()
	} else {
		b, err = f.nextLine()
	}
	if err != nil {
		// io.EOF is an ending, not a malformed message, and must not poison a
		// reader a caller may legitimately drain again.
		if err != io.EOF {
			f.err = err
		}
		return nil, err
	}
	return b, nil
}

// Poisoned reports whether a malformed frame has torn the reader down.
func (f *FrameReader) Poisoned() bool { return f.err != nil }

func (f *FrameReader) nextLine() ([]byte, error) {
	var out []byte
	for {
		chunk, more, err := f.br.ReadLine()
		if err != nil {
			return nil, err
		}
		// Checked BEFORE the append that would grow the buffer, which is the
		// whole of "bounded before it allocates".
		if int64(len(out))+int64(len(chunk)) > f.lim.MaxMessageBytes {
			return nil, failf(RuleMessageBytes, "$", "frame exceeds %d bytes",
				f.lim.MaxMessageBytes)
		}
		out = append(out, chunk...)
		if !more {
			break
		}
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return f.nextLine() // blank keep-alive lines are not frames
	}
	return out, nil
}

func (f *FrameReader) nextHeaderFramed() ([]byte, error) {
	length := int64(-1)
	for {
		line, err := f.readHeaderLine()
		if err != nil {
			return nil, err
		}
		if line == "" {
			break // end of headers
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, failf(RuleSyntax, "$", "malformed frame header %q", line)
		}
		if !strings.EqualFold(strings.TrimSpace(name), "content-length") {
			continue
		}
		n, err := parseDeclaredLength(strings.TrimSpace(value), f.lim.MaxMessageBytes)
		if err != nil {
			return nil, err
		}
		length = n
	}
	if length < 0 {
		return nil, failf(RuleSyntax, "$", "frame has no Content-Length header")
	}

	// REQ-SEC-11.2: the buffer is NOT sized to the declared length. A peer
	// announcing 16 MiB and sending one byte must cost one byte, not 16 MiB —
	// otherwise the declared number is a free allocation primitive. Buffer's
	// ReadFrom grows geometrically as bytes actually arrive.
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, f.br, length); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, failf(RuleSyntax, "$",
				"frame declared %d bytes and the stream ended early", length)
		}
		return nil, err
	}
	return buf.Bytes(), nil
}

func (f *FrameReader) readHeaderLine() (string, error) {
	var out []byte
	for {
		chunk, more, err := f.br.ReadLine()
		if err != nil {
			return "", err
		}
		// A header line is bounded too: a peer sending an endless header never
		// reaches the Content-Length check that would have stopped it.
		if len(out)+len(chunk) > 8<<10 {
			return "", failf(RuleMessageBytes, "$", "frame header line exceeds 8 KiB")
		}
		out = append(out, chunk...)
		if !more {
			return strings.TrimRight(string(out), "\r"), nil
		}
	}
}

// parseDeclaredLength is REQ-SEC-11.1.
//
// The value is parsed as UINT64 and range-checked BEFORE it is narrowed. Parse
// it as int on a 32-bit build and a declared 2^31 wraps negative, sails past a
// `> max` check, and panics on a negative slice bound — a remote crash from a
// header field. Parsing wide and narrowing late is the fix, and it costs
// nothing on the platform where the bug is invisible.
func parseDeclaredLength(s string, max int64) (int64, error) {
	if s == "" {
		return 0, failf(RuleSyntax, "$", "empty Content-Length")
	}
	u, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, failf(RuleSyntax, "$", "malformed Content-Length %q", s)
	}
	if u > uint64(max) {
		return 0, failf(RuleMessageBytes, "$",
			"frame declares %d bytes, limit is %d", u, max)
	}
	return int64(u), nil
}
