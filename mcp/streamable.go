package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// StreamableHTTPTransport is REQ-MCP-CLIENT-02's 2025-03-26 transport.
//
// One endpoint. Every client message is a POST; the server answers with 202
// (nothing to say), a single JSON body, or an SSE stream carrying one or more
// messages. A separate long-lived GET carries messages the server originates
// on its own — sampling requests and notifications — and a server that does
// not support that answers the GET with 405, which is not an error.
type StreamableHTTPTransport struct {
	*httpCommon

	sessMu    sync.RWMutex
	sessionID string

	// listenOnce starts the standalone GET after the first successful POST,
	// because before the handshake there is no session id to open it with.
	listenOnce sync.Once
}

// StartStreamableHTTP opens a 2025-03-26 transport. Nothing is sent until the
// first Send: the protocol has no separate connect step, and dialling eagerly
// would report an unreachable server from a constructor rather than from the
// handshake that needed it.
func StartStreamableHTTP(ctx context.Context, opts HTTPTransportOptions) (*StreamableHTTPTransport, error) {
	c, err := newHTTPCommon(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &StreamableHTTPTransport{httpCommon: c}, nil
}

// SessionID is the server-assigned session, empty until the server assigns
// one. Exposed for diagnostics: a 404 mid-session is only interpretable next
// to the id it was for.
func (t *StreamableHTTPTransport) SessionID() string {
	t.sessMu.RLock()
	defer t.sessMu.RUnlock()
	return t.sessionID
}

func (t *StreamableHTTPTransport) Send(frame []byte) error {
	if err := t.ctx.Err(); err != nil {
		return ErrTransportClosed
	}
	resp, err := t.post(t.ctx, frame)
	if err != nil {
		return err
	}
	if err := t.consume(resp); err != nil {
		return err
	}
	// Only now: the server has answered at least once, so a session id (if it
	// issues them) is in hand and the GET can carry it.
	t.listenOnce.Do(func() {
		t.wg.Add(1)
		go func() { defer t.wg.Done(); t.listen() }()
	})
	return nil
}

func (t *StreamableHTTPTransport) post(ctx context.Context, frame []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint.String(), bytes.NewReader(frame))
	if err != nil {
		return nil, err
	}
	t.applyHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	// Both are advertised because the server chooses per request: a
	// notification gets 202, a request that streams gets an event stream, and
	// a request answered in one shot gets JSON.
	req.Header.Set("Accept", "application/json, text/event-stream")
	if id := t.SessionID(); id != "" {
		req.Header.Set(MCPSessionHeader, id)
	}
	return t.hc.Do(req)
}

// consume handles one POST response.
func (t *StreamableHTTPTransport) consume(resp *http.Response) error {
	if id := resp.Header.Get(MCPSessionHeader); id != "" {
		t.adoptSession(id)
	}

	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent:
		// The frame was a notification or a response; there is nothing to
		// read, and reading anyway would hold the connection open.
		drainAndClose(resp.Body, 4<<10)
		return nil

	case resp.StatusCode == http.StatusNotFound && t.SessionID() != "":
		// A 404 for a session we hold is the spec's "your session is gone".
		// Distinguishable so a caller re-initializes instead of retrying a
		// request that can never succeed again.
		drainAndClose(resp.Body, 4<<10)
		t.clearSession()
		return ErrSessionExpired

	case resp.StatusCode >= 400:
		body := drainAndClose(resp.Body, 8<<10)
		return &HTTPStatusError{
			Status: resp.StatusCode, Method: http.MethodPost,
			URL: t.endpoint.String(), Body: string(bytes.TrimSpace(body)),
		}
	}

	ct := resp.Header.Get("Content-Type")
	switch {
	case isEventStream(ct):
		// The stream stays open until the server finishes answering, which may
		// be after several messages. Reading it on this goroutine would block
		// Send until then, and Send is called from the caller's request path.
		t.wg.Add(1)
		go func() { defer t.wg.Done(); t.readStream(resp.Body, nil) }()
		return nil

	case isJSON(ct):
		defer resp.Body.Close()
		// Bounded before allocating (REQ-SEC-11.2). The extra byte is how an
		// over-limit body is detected rather than silently truncated into a
		// frame that would then fail to parse for the wrong reason.
		max := t.limits.MaxMessageBytes
		body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
		if err != nil {
			return fmt.Errorf("mcp: reading the response body: %w", err)
		}
		if int64(len(body)) > max {
			return fmt.Errorf("mcp: response body exceeds %d bytes", max)
		}
		if len(bytes.TrimSpace(body)) == 0 {
			return nil
		}
		t.deliver(body)
		return nil
	}

	drainAndClose(resp.Body, 4<<10)
	return fmt.Errorf("mcp: server answered a POST with content-type %q; want "+
		"application/json or text/event-stream", ct)
}

// listen holds the standalone GET open for server-initiated messages.
func (t *StreamableHTTPTransport) listen() {
	req, err := http.NewRequestWithContext(t.ctx, http.MethodGet, t.endpoint.String(), nil)
	if err != nil {
		t.warn("cannot open the server-initiated stream: %v", err)
		return
	}
	t.applyHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	if id := t.SessionID(); id != "" {
		req.Header.Set(MCPSessionHeader, id)
	}

	resp, err := t.hc.Do(req)
	if err != nil {
		if t.ctx.Err() == nil {
			// Not fatal. Requests and their replies flow over POSTs; only
			// server-initiated messages need this stream, so a server without
			// one still works for everything except sampling.
			t.warn("server-initiated stream unavailable: %v", err)
		}
		return
	}
	switch {
	case resp.StatusCode == http.StatusMethodNotAllowed:
		// The spec's documented "I have no server-initiated stream".
		drainAndClose(resp.Body, 4<<10)
		return
	case resp.StatusCode >= 400:
		body := drainAndClose(resp.Body, 4<<10)
		t.warn("server-initiated stream refused with %d: %s",
			resp.StatusCode, bytes.TrimSpace(body))
		return
	}
	if !isEventStream(resp.Header.Get("Content-Type")) {
		drainAndClose(resp.Body, 4<<10)
		t.warn("server-initiated stream is %q, not text/event-stream",
			resp.Header.Get("Content-Type"))
		return
	}
	t.readStream(resp.Body, nil)
}

// adoptSession stores a server-assigned id after validating it.
//
// The LENGTH bound is the one doing work. Go's own response reader rejects a
// header value containing any control byte — the whole response fails as a
// malformed MIME header — so CR/LF injection cannot reach here; but nothing
// bounds the value's size below MaxResponseHeaderBytes, which defaults to
// 10 MB. Unbounded, a server picks how many bytes we send on every subsequent
// request. isHeaderSafe stays as the second line of that defence, so the rule
// does not depend on a guarantee made by a package we do not own.
func (t *StreamableHTTPTransport) adoptSession(id string) {
	if len(id) > MaxSessionIDBytes || !isHeaderSafe(id) {
		t.warn("ignoring an unusable %s from the server (%d bytes)", MCPSessionHeader, len(id))
		return
	}
	t.sessMu.Lock()
	t.sessionID = id
	t.sessMu.Unlock()
}

func (t *StreamableHTTPTransport) clearSession() {
	t.sessMu.Lock()
	t.sessionID = ""
	t.sessMu.Unlock()
}

// Close ends the session politely, then tears the transport down.
//
// The DELETE is best-effort and bounded by its own context: a server that has
// gone away must not make Close hang, and the local teardown is the part that
// actually matters.
func (t *StreamableHTTPTransport) Close() error {
	if id := t.SessionID(); id != "" {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.ctx), DefaultTimeout)
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.endpoint.String(), nil); err == nil {
			t.applyHeaders(req)
			req.Header.Set(MCPSessionHeader, id)
			if resp, err := t.hc.Do(req); err == nil {
				drainAndClose(resp.Body, 4<<10)
			}
		}
		cancel()
	}
	return t.httpCommon.Close()
}

// HTTPStatusError is a non-2xx from an MCP endpoint. The body is included
// because an MCP server's refusal usually explains itself there and a bare
// status code sends the reader to the server's logs for no reason.
type HTTPStatusError struct {
	Status int
	Method string
	URL    string
	Body   string
}

func (e *HTTPStatusError) Error() string {
	msg := fmt.Sprintf("mcp: %s %s: http %d", e.Method, e.URL, e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}
