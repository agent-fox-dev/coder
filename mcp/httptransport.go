package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/wire"
)

// REQ-MCP-CLIENT-02's two remote transports.
//
// HTTPModeStreamable is the 2025-03-26 spec: one endpoint, POST a frame and
// the reply comes back either as a single JSON body or as an SSE stream on
// that same response. HTTPModeSSE is the 2024-11-05 spec: a long-lived GET
// carries every server->client message, and the server names a SEPARATE URL to
// POST to in an `endpoint` event.
type HTTPMode string

const (
	// HTTPModeAuto POSTs as 2025-03-26 and falls back to 2024-11-05 when the
	// server rejects the POST. This is the spec's own backwards-compatibility
	// procedure, and it is the default because the alternative is making an
	// operator know which revision a third-party server implements before they
	// can configure it.
	HTTPModeAuto       HTTPMode = ""
	HTTPModeStreamable HTTPMode = "streamable-http"
	HTTPModeSSE        HTTPMode = "sse"
)

// MCPSessionHeader is the 2025-03-26 session identifier.
const MCPSessionHeader = "Mcp-Session-Id"

// MaxSessionIDBytes bounds the session id we will echo back. The value comes
// from the server and goes straight into a request header on every subsequent
// call; unbounded, a server could make us send megabytes per request.
const MaxSessionIDBytes = 512

var (
	// ErrSessionExpired is the 2025-03-26 spec's 404-on-a-known-session. A
	// caller that sees it must start a new session rather than retry, which is
	// why it is distinguishable from any other HTTP failure.
	ErrSessionExpired = errors.New("mcp: the server no longer knows this session")

	// ErrEndpointOrigin is the HTTP/SSE transport's refusal to POST somewhere
	// the stream told it to.
	ErrEndpointOrigin = errors.New("mcp: the server's endpoint event names a different origin")

	// ErrNoEndpoint means the 2024-11-05 stream opened but never named a POST
	// endpoint.
	ErrNoEndpoint = errors.New("mcp: the sse stream ended without an endpoint event")
)

// HTTPTransportOptions configures a remote transport.
type HTTPTransportOptions struct {
	// URL is the server endpoint. http and https only.
	URL  string
	Mode HTTPMode
	// Client is the HTTP client. Nil builds one with no overall timeout,
	// because a request whose response is a long-lived SSE stream must not be
	// cut off by a client deadline — the per-call deadline lives on the
	// context instead.
	Client *http.Client
	// Headers are sent on every request. This is where a bearer token for a
	// remote server goes.
	Headers map[string]string
	Limits  wire.Limits
	Warnf   func(format string, args ...any)
	// EndpointTimeout bounds the wait for the 2024-11-05 `endpoint` event
	// before the first Send can proceed. Zero uses DefaultEndpointTimeout.
	EndpointTimeout time.Duration
}

// DefaultEndpointTimeout bounds the 2024-11-05 handshake.
const DefaultEndpointTimeout = 30 * time.Second

// inboundBuffer is how many frames may sit undelivered before a pump blocks.
// Blocking is the correct back-pressure: dropping a frame loses a response the
// caller is still waiting for.
const inboundBuffer = 32

// httpCommon is the machinery both transports share.
type httpCommon struct {
	endpoint *url.URL
	hc       *http.Client
	headers  map[string]string
	limits   wire.Limits
	warnf    func(string, ...any)

	ctx    context.Context
	cancel context.CancelFunc
	in     chan []byte
	wg     sync.WaitGroup

	errMu sync.Mutex
	err   error

	closeOnce sync.Once
}

func newHTTPCommon(ctx context.Context, opts HTTPTransportOptions) (*httpCommon, error) {
	u, err := parseEndpoint(opts.URL)
	if err != nil {
		return nil, err
	}
	hc := opts.Client
	if hc == nil {
		hc = &http.Client{}
	}
	cctx, cancel := context.WithCancel(ctx)
	return &httpCommon{
		endpoint: u,
		hc:       hc,
		headers:  opts.Headers,
		limits:   opts.Limits.WithDefaults(),
		warnf:    opts.Warnf,
		ctx:      cctx,
		cancel:   cancel,
		in:       make(chan []byte, inboundBuffer),
	}, nil
}

// parseEndpoint rejects everything but http and https.
//
// A `file://` or custom-scheme endpoint in a config file is either a mistake
// or an attempt to make the client read something local; there is no MCP
// server behind either.
func parseEndpoint(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("mcp: an http transport needs a url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("mcp: server url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("mcp: server url %q has scheme %q; only http and https "+
			"are transports", raw, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("mcp: server url %q has no host", raw)
	}
	return u, nil
}

// Receive is the Transport half that hands frames to the client's read loop.
func (h *httpCommon) Receive() ([]byte, error) {
	select {
	case frame := <-h.in:
		return frame, nil
	case <-h.ctx.Done():
		// Drain first. A frame that already arrived is a response somebody is
		// still waiting for, and reporting the close before delivering it
		// turns a clean shutdown into a lost reply.
		select {
		case frame := <-h.in:
			return frame, nil
		default:
		}
		if err := h.storedErr(); err != nil {
			return nil, err
		}
		return nil, ErrTransportClosed
	}
}

func (h *httpCommon) Close() error {
	h.closeOnce.Do(func() {
		h.cancel()
		h.hc.CloseIdleConnections()
	})
	h.wg.Wait()
	return nil
}

// deliver hands a frame to Receive, or gives up if the transport is closing.
func (h *httpCommon) deliver(frame []byte) {
	select {
	case h.in <- frame:
	case <-h.ctx.Done():
	}
}

// fail records the FIRST error and shuts the transport down. Later errors are
// consequences of the first, and reporting the last one names the symptom
// instead of the cause.
func (h *httpCommon) fail(err error) {
	if err == nil {
		return
	}
	h.errMu.Lock()
	if h.err == nil {
		h.err = err
	}
	h.errMu.Unlock()
	h.cancel()
}

func (h *httpCommon) storedErr() error {
	h.errMu.Lock()
	defer h.errMu.Unlock()
	return h.err
}

func (h *httpCommon) warn(format string, args ...any) {
	if h.warnf != nil {
		h.warnf(format, args...)
	}
}

func (h *httpCommon) applyHeaders(r *http.Request) {
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
}

// readStream pumps an SSE body until it ends, delivering `message` events.
func (h *httpCommon) readStream(body io.ReadCloser, onEndpoint func(string)) {
	defer body.Close()
	dec := newSSEDecoder(body, int(h.limits.MaxMessageBytes))
	for {
		ev, err := dec.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			if h.ctx.Err() != nil {
				return // our own shutdown, not the server's failure
			}
			h.fail(fmt.Errorf("mcp: reading the event stream: %w", err))
			return
		}
		switch ev.name {
		case "endpoint":
			if onEndpoint != nil {
				onEndpoint(string(ev.data))
			}
		case "", "message":
			if len(bytes.TrimSpace(ev.data)) == 0 {
				continue // a keep-alive with no payload
			}
			h.deliver(append([]byte(nil), ev.data...))
		default:
			// `ping` and vendor events are not ours to interpret, and treating
			// an unknown event as a frame would feed the decoder garbage.
			h.warn("ignoring sse event %q", ev.name)
		}
	}
}

// drainAndClose reads a bounded amount of a body we are discarding, so the
// connection can be reused, and never more than that.
func drainAndClose(body io.ReadCloser, limit int64) []byte {
	defer body.Close()
	b, _ := io.ReadAll(io.LimitReader(body, limit))
	return b
}

// isEventStream reports whether a Content-Type is text/event-stream, ignoring
// parameters such as charset.
func isEventStream(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "text/event-stream"
}

func isJSON(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}
