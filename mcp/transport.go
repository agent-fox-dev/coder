package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/wire"
)

// Transport carries framed JSON-RPC messages in both directions.
//
// Receive returns ONE message. It does not take a context: a transport read is
// unblocked by closing the transport, not by cancelling a read, and a
// context-aware Receive would have to either leak the goroutine still blocked
// on the socket or lie about having stopped. Close is the cancellation.
type Transport interface {
	Send(msg []byte) error
	Receive() ([]byte, error)
	Close() error
}

// ErrTransportClosed is returned once a transport has been shut down.
var ErrTransportClosed = errors.New("mcp: transport is closed")

// ---------------------------------------------------------------- stdio

// StdioOptions configures a subprocess transport.
type StdioOptions struct {
	Command string
	Args    []string
	Dir     string
	// Env is the COMPLETE environment for the child. REQ-MCP-CLIENT-10 and
	// REQ-SEC-08 both require a reduced one: a stdio MCP server inheriting the
	// parent environment receives every provider API key in it, which is the
	// same class of mistake as passing a credential on a command line.
	//
	// Nil means an EMPTY environment, not the parent's. Defaulting to
	// inheritance would make the safe case the one you have to remember.
	Env []string
	// Stderr receives the server's diagnostics line by line. Nil discards
	// them — but discarding is the caller's explicit choice, because a stdio
	// server that fails to start says why on stderr and nowhere else.
	Stderr func(line string)
	Limits wire.Limits
}

// StdioTransport runs an MCP server as a subprocess and speaks NDJSON to it.
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	frames *wire.FrameReader

	sendMu sync.Mutex
	once   sync.Once
	closed chan struct{}
	waitCh chan error
}

// StartStdio spawns the server.
func StartStdio(ctx context.Context, opts StdioOptions) (*StdioTransport, error) {
	if opts.Command == "" {
		return nil, errors.New("mcp: stdio transport needs a command")
	}
	cmd := exec.Command(opts.Command, opts.Args...)
	cmd.Dir = opts.Dir
	// An explicit empty slice, never nil: exec treats a nil Env as "inherit
	// the parent's", which is the one behaviour REQ-MCP-CLIENT-10 forbids.
	cmd.Env = opts.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	setProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: starting %q: %w", opts.Command, err)
	}

	t := &StdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		frames: wire.NewNDJSON(stdout, opts.Limits),
		closed: make(chan struct{}),
		waitCh: make(chan error, 1),
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
		for sc.Scan() {
			if opts.Stderr != nil {
				opts.Stderr(sc.Text())
			}
		}
	}()
	go func() { t.waitCh <- cmd.Wait() }()

	return t, nil
}

func (t *StdioTransport) Send(msg []byte) error {
	select {
	case <-t.closed:
		return ErrTransportClosed
	default:
	}
	// Serialized: two goroutines writing concurrently would interleave two
	// JSON objects into one line, and NDJSON has no way to tell the peer that
	// what it just read was two messages.
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	if _, err := t.stdin.Write(append(msg, '\n')); err != nil {
		return err
	}
	return nil
}

func (t *StdioTransport) Receive() ([]byte, error) { return t.frames.Next() }

// Close terminates the server and reaps it.
//
// It kills the process GROUP, not the process: an MCP server that spawned
// helpers of its own leaves them holding the pipe otherwise, and the read side
// never sees EOF.
func (t *StdioTransport) Close() error {
	var err error
	t.once.Do(func() {
		close(t.closed)
		_ = t.stdin.Close()

		select {
		case werr := <-t.waitCh:
			err = werr
		case <-time.After(2 * time.Second):
			// A server that will not exit on a closed stdin gets killed. The
			// wait that follows is what stops a zombie, and the timeout on it
			// is what stops Close from being the thing that hangs.
			killGroup(t.cmd)
			select {
			case werr := <-t.waitCh:
				err = werr
			case <-time.After(2 * time.Second):
				err = errors.New("mcp: server did not exit after being killed")
			}
		}
	})
	if err != nil && isExpectedExit(err) {
		return nil
	}
	return err
}

// isExpectedExit reports the shutdown outcomes that are not failures: a server
// we killed, and one that exited because we closed its stdin.
func isExpectedExit(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe)
}

// ---------------------------------------------------------------- pipe

// PipeTransport speaks NDJSON over an arbitrary reader/writer pair.
//
// It is the transport a SERVER uses in stdio mode, and it is what makes the
// client and server testable against each other in-process, with no
// subprocess, no port and no timing.
type PipeTransport struct {
	w io.Writer
	// Both ends are closed by Close. Closing only the writer leaves the PEER's
	// reader blocked forever — and a Close that does not unblock the read side
	// is a Close that deadlocks anything waiting for the read loop to finish.
	wCloser io.Closer
	rCloser io.Closer
	frames  *wire.FrameReader
	sendMu  sync.Mutex
	once    sync.Once
	closed  chan struct{}
}

func NewPipeTransport(r io.Reader, w io.Writer, limits wire.Limits) *PipeTransport {
	t := &PipeTransport{w: w, frames: wire.NewNDJSON(r, limits), closed: make(chan struct{})}
	if c, ok := w.(io.Closer); ok {
		t.wCloser = c
	}
	if c, ok := r.(io.Closer); ok {
		t.rCloser = c
	}
	return t
}

func (t *PipeTransport) Send(msg []byte) error {
	select {
	case <-t.closed:
		return ErrTransportClosed
	default:
	}
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	_, err := t.w.Write(append(msg, '\n'))
	return err
}

func (t *PipeTransport) Receive() ([]byte, error) { return t.frames.Next() }

func (t *PipeTransport) Close() error {
	var err error
	t.once.Do(func() {
		close(t.closed)
		if t.rCloser != nil {
			// The reader first: this is what unblocks a read loop waiting on
			// it, and the write close below can only report an error.
			_ = t.rCloser.Close()
		}
		if t.wCloser != nil {
			err = t.wCloser.Close()
		}
	})
	return err
}

// ---------------------------------------------------------------- helpers

// interpolate expands ${VAR} and $VAR against a lookup (REQ-MCP-CLIENT-07).
//
// An UNSET variable expands to empty and is REPORTED, rather than being left
// as the literal `${VAR}`. Leaving the literal means the child receives the
// eight characters "${TOKEN}" as its credential and fails authentication with
// a message about a bad token — which sends the reader looking at the token
// rather than at the variable that was never set.
func interpolate(s string, lookup func(string) string) (string, []string) {
	var missing []string
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			break
		}
		if s[i+1] == '$' {
			b.WriteByte('$') // $$ is a literal dollar
			i += 2
			continue
		}
		name, next := "", 0
		if s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteString(s[i:])
				break
			}
			name, next = s[i+2:i+2+end], i+3+end
		} else {
			j := i + 1
			for j < len(s) && (isWordByte(s[j])) {
				j++
			}
			if j == i+1 {
				b.WriteByte('$')
				i++
				continue
			}
			name, next = s[i+1:j], j
		}
		v := lookup(name)
		if v == "" {
			missing = append(missing, name)
		}
		b.WriteString(v)
		i = next
	}
	return b.String(), missing
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
