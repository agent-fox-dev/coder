package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

// ContextSender is a Transport whose Send honours a per-call deadline.
//
// It exists because Streamable HTTP does real work inside Send: the POST and,
// for a JSON-answered request, the body read. Running those under the
// TRANSPORT's context meant a server that accepted the POST and then stalled
// held the caller forever — REQ-MCP-CLIENT-07's timeout_s never fired, because
// the call was still inside Send when its context expired. The client uses
// this method when a transport offers it and falls back to Send otherwise.
type ContextSender interface {
	SendContext(ctx context.Context, msg []byte) error
}

// sendContext sends through SendContext when the transport has one.
func sendContext(ctx context.Context, tr Transport, msg []byte) error {
	if cs, ok := tr.(ContextSender); ok {
		return cs.SendContext(ctx, msg)
	}
	return tr.Send(msg)
}

// ErrTransportClosed is returned once a transport has been shut down. A write
// that fails because the peer is gone wraps it too, so a caller can tell "the
// link is dead" from "this message was refused" without matching on text.
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
	stdout *os.File
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
	// stdout and stderr are OUR pipes, not cmd.StdoutPipe's. os/exec documents
	// that Wait closes the pipes it created, and the reaper below calls Wait
	// the moment the process exits — so a server that wrote its last response
	// and exited immediately could have that frame discarded by Wait before
	// the reader got to it. A pipe exec did not create, exec does not close:
	// the read side stays open until the reader has drained it to EOF.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	if err := cmd.Start(); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return nil, fmt.Errorf("mcp: starting %q: %w", opts.Command, err)
	}
	// The child holds the write ends now. Ours must go, or the reader never
	// sees EOF — it would be waiting on a writer that is this very process.
	_ = stdoutW.Close()
	_ = stderrW.Close()

	t := &StdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdoutR,
		frames: wire.NewNDJSON(stdoutR, opts.Limits),
		closed: make(chan struct{}),
		waitCh: make(chan error, 1),
	}

	go func() {
		defer stderrR.Close()
		sc := bufio.NewScanner(stderrR)
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
		// A write to the child's stdin fails only when the child is gone (or
		// closed its end, which for an MCP server is the same thing). Naming
		// it a closed transport is what lets the connection's reconnect path
		// (NFR-REL-03) recognise a dead server at the first call after it
		// died, rather than after a timeout.
		return fmt.Errorf("%w: writing to the server's stdin: %v", ErrTransportClosed, err)
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
		// Only now, after the process is reaped: closing the read end earlier
		// is the frame-losing race this transport exists to avoid, and a
		// reader still blocked on it is unblocked by this close.
		_ = t.stdout.Close()
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
	if _, err := t.w.Write(append(msg, '\n')); err != nil {
		// As for stdio: a pipe that refuses a write has lost its peer.
		return fmt.Errorf("%w: %v", ErrTransportClosed, err)
	}
	return nil
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
// The lookup reports PRESENCE as well as value, and only an ABSENT variable
// is returned as missing. A variable deliberately set to the empty string is
// a value the operator chose, not a reference that failed to resolve — and
// NFR-SEC-03 makes the latter a configuration error, so the two must not be
// confused. The missing list is what Connect turns into that error; the
// literal `${VAR}` is never handed to the child either way.
func interpolate(s string, lookup func(string) (string, bool)) (string, []string) {
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
		v, ok := lookup(name)
		if !ok {
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
