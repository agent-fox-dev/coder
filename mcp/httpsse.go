package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// SSETransport is REQ-MCP-CLIENT-02's 2024-11-05 transport.
//
// A long-lived GET carries every server->client message. The server's FIRST
// event names a separate URL to POST client messages to, so nothing can be
// sent until that event arrives.
type SSETransport struct {
	*httpCommon

	ready    chan struct{} // closed once postURL is set
	readyOne sync.Once

	postMu  sync.RWMutex
	postURL *url.URL

	endpointTimeout time.Duration
}

// StartHTTPSSE opens the stream and waits for the endpoint event.
//
// The wait is here rather than in Send because a server that never names an
// endpoint is not usable, and finding that out at connection time names the
// server that is broken. Deferring it to the first Send would surface it as a
// failed `initialize` instead.
func StartHTTPSSE(ctx context.Context, opts HTTPTransportOptions) (*SSETransport, error) {
	c, err := newHTTPCommon(ctx, opts)
	if err != nil {
		return nil, err
	}
	timeout := opts.EndpointTimeout
	if timeout <= 0 {
		timeout = DefaultEndpointTimeout
	}
	t := &SSETransport{httpCommon: c, ready: make(chan struct{}), endpointTimeout: timeout}

	req, err := http.NewRequestWithContext(t.ctx, http.MethodGet, t.endpoint.String(), nil)
	if err != nil {
		t.cancel()
		return nil, err
	}
	t.applyHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-store")

	resp, err := t.hc.Do(req)
	if err != nil {
		t.cancel()
		return nil, fmt.Errorf("mcp: opening the event stream: %w", err)
	}
	if resp.StatusCode >= 400 {
		body := drainAndClose(resp.Body, 8<<10)
		t.cancel()
		return nil, &HTTPStatusError{
			Status: resp.StatusCode, Method: http.MethodGet,
			URL: t.endpoint.String(), Body: string(bytes.TrimSpace(body)),
		}
	}
	if !isEventStream(resp.Header.Get("Content-Type")) {
		ct := resp.Header.Get("Content-Type")
		drainAndClose(resp.Body, 4<<10)
		t.cancel()
		return nil, fmt.Errorf("mcp: %s answered with content-type %q; a 2024-11-05 "+
			"server answers a GET with text/event-stream", t.endpoint, ct)
	}

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer t.readyOne.Do(func() { close(t.ready) }) // unblock a waiting Send on a dead stream
		t.readStream(resp.Body, t.setEndpoint)
	}()

	// Wait for the endpoint here so a caller holds a transport that can
	// actually send.
	//
	// The two wake-ups are NOT branches with different outcomes. Rejecting an
	// endpoint cancels t.ctx, so a rejection wakes the timeout arm as readily
	// as the ready arm — and deciding the error from which arm fired reports a
	// 30-second timeout for a refusal that happened in a millisecond. The
	// stored error is the authority; the select only decides when to look.
	wait, cancel := context.WithTimeout(t.ctx, t.endpointTimeout)
	defer cancel()
	select {
	case <-t.ready:
	case <-wait.Done():
	}

	if err := t.storedErr(); err != nil {
		_ = t.httpCommon.Close()
		return nil, err
	}
	if t.PostURL() == "" {
		_ = t.httpCommon.Close()
		if perr := ctx.Err(); perr != nil {
			return nil, perr // the caller gave up, not the server
		}
		return nil, fmt.Errorf("mcp: %s: %w after %s", t.endpoint, ErrNoEndpoint, t.endpointTimeout)
	}
	return t, nil
}

// PostURL is the endpoint the server named, empty until it does.
func (t *SSETransport) PostURL() string {
	t.postMu.RLock()
	defer t.postMu.RUnlock()
	if t.postURL == nil {
		return ""
	}
	return t.postURL.String()
}

// setEndpoint handles the `endpoint` event.
//
// The URL is resolved against the stream's own URL, then CHECKED AGAINST ITS
// ORIGIN. This is the security-critical line in the file: every POST carries
// the configured Headers, which is where a bearer token for the server lives.
// A server that could name an arbitrary origin here — because it is malicious,
// or compromised, or just reverse-proxied by someone who is — would be
// redirecting our credential to a host of its choosing. Same scheme, same
// host, same port, or we do not send.
func (t *SSETransport) setEndpoint(raw string) {
	raw = string(bytes.TrimSpace([]byte(raw)))
	if raw == "" {
		t.fail(fmt.Errorf("mcp: %s: the endpoint event carried no url", t.endpoint))
		t.readyOne.Do(func() { close(t.ready) })
		return
	}
	ref, err := url.Parse(raw)
	if err != nil {
		t.fail(fmt.Errorf("mcp: %s: unparseable endpoint %q: %w", t.endpoint, raw, err))
		t.readyOne.Do(func() { close(t.ready) })
		return
	}
	// Relative is the common case and is exactly what the same-origin rule
	// makes safe: `/messages?sessionId=abc` cannot leave this host.
	resolved := t.endpoint.ResolveReference(ref)
	if !sameOrigin(t.endpoint, resolved) {
		t.fail(fmt.Errorf("%w: stream %s named %s", ErrEndpointOrigin, t.endpoint, resolved))
		t.readyOne.Do(func() { close(t.ready) })
		return
	}

	t.postMu.Lock()
	first := t.postURL == nil
	t.postURL = resolved
	t.postMu.Unlock()

	if first {
		t.readyOne.Do(func() { close(t.ready) })
	}
}

// sameOrigin compares scheme, host and port, defaulting the port by scheme so
// `https://h` and `https://h:443` are one origin rather than two.
func sameOrigin(a, b *url.URL) bool {
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	return strings.EqualFold(originHost(a), originHost(b))
}

func originHost(u *url.URL) string {
	host, port := u.Hostname(), u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return host + ":" + port
}

func (t *SSETransport) Send(frame []byte) error {
	if err := t.ctx.Err(); err != nil {
		if stored := t.storedErr(); stored != nil {
			return stored
		}
		return ErrTransportClosed
	}
	target := t.PostURL()
	if target == "" {
		return ErrNoEndpoint
	}

	req, err := http.NewRequestWithContext(t.ctx, http.MethodPost, target, bytes.NewReader(frame))
	if err != nil {
		return err
	}
	t.applyHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.hc.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: posting to %s: %w", target, err)
	}
	body := drainAndClose(resp.Body, 8<<10)
	if resp.StatusCode >= 400 {
		return &HTTPStatusError{
			Status: resp.StatusCode, Method: http.MethodPost,
			URL: target, Body: string(bytes.TrimSpace(body)),
		}
	}
	// The reply does NOT come back here. In 2024-11-05 a POST is acknowledged
	// and the response arrives on the event stream, correlated by id.
	return nil
}

var _ Transport = (*SSETransport)(nil)
var _ Transport = (*StreamableHTTPTransport)(nil)

// StartHTTP opens whichever remote transport the mode selects.
//
// HTTPModeAuto is the spec's own backwards-compatibility procedure: POST as
// 2025-03-26 and, if the server rejects the POST outright, fall back to the
// 2024-11-05 GET. The fallback is probed with a real `initialize` so the
// decision is made by the server's actual behaviour rather than by a guess;
// see autoTransport.
func StartHTTP(ctx context.Context, opts HTTPTransportOptions) (Transport, error) {
	switch opts.Mode {
	case HTTPModeStreamable:
		return StartStreamableHTTP(ctx, opts)
	case HTTPModeSSE:
		return StartHTTPSSE(ctx, opts)
	case HTTPModeAuto:
		return newAutoTransport(ctx, opts)
	}
	return nil, fmt.Errorf("mcp: unknown http mode %q (want %q, %q, or empty for auto)",
		opts.Mode, HTTPModeStreamable, HTTPModeSSE)
}

// autoTransport is HTTPModeAuto.
//
// It starts as streamable HTTP and switches to 2024-11-05 if the FIRST POST is
// rejected with a status that means "this endpoint does not take POSTs" — 405,
// or the 404 a server serving only a GET stream returns for an unknown path.
// Only the first: after a successful exchange the revision is settled, and a
// later 404 is ErrSessionExpired, which means something else entirely.
type autoTransport struct {
	ctx  context.Context
	opts HTTPTransportOptions

	mu       sync.Mutex
	chosen   Transport
	switched bool
	closed   bool
	// gen increments on the swap. Receive is normally parked inside the
	// transport being replaced, so it wakes with that transport's shutdown
	// error; gen is how it tells "the session ended" from "the transport under
	// me was exchanged", and without it the fallback kills the very connection
	// it was performing to save.
	gen uint64
}

func newAutoTransport(ctx context.Context, opts HTTPTransportOptions) (*autoTransport, error) {
	st, err := StartStreamableHTTP(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &autoTransport{ctx: ctx, opts: opts, chosen: st}, nil
}

func (a *autoTransport) Send(frame []byte) error {
	a.mu.Lock()
	tr, maySwitch := a.chosen, !a.switched
	a.mu.Unlock()

	err := tr.Send(frame)
	if err == nil || !maySwitch || !looksLikeWrongRevision(err) {
		if maySwitch && err == nil {
			a.mu.Lock()
			a.switched = true // settled on streamable
			a.mu.Unlock()
		}
		return err
	}

	// Fall back. The 2024-11-05 handshake has to complete before the frame can
	// go anywhere, so this blocks exactly as long as StartHTTPSSE does.
	sse, serr := StartHTTPSSE(a.ctx, a.opts)
	if serr != nil {
		// Report the ORIGINAL failure as the cause: the server rejected our
		// POST and then had no 2024-11-05 stream either, and the first of
		// those is what a reader needs to see.
		return fmt.Errorf("mcp: %s rejected a streamable-http POST (%v) and has no "+
			"2024-11-05 event stream either: %w", a.opts.URL, err, serr)
	}

	a.mu.Lock()
	old := a.chosen
	a.chosen, a.switched = sse, true
	a.gen++
	closed := a.closed
	a.mu.Unlock()

	_ = old.Close()
	if closed {
		_ = sse.Close()
		return ErrTransportClosed
	}
	return sse.Send(frame)
}

func (a *autoTransport) Receive() ([]byte, error) {
	for {
		a.mu.Lock()
		tr, gen := a.chosen, a.gen
		a.mu.Unlock()

		frame, err := tr.Receive()
		if err == nil {
			return frame, nil
		}
		a.mu.Lock()
		swapped := a.gen != gen
		a.mu.Unlock()
		if swapped {
			// This error is the discarded transport being closed by the
			// fallback, not the session ending. Read the replacement instead.
			continue
		}
		return nil, err
	}
}

func (a *autoTransport) Close() error {
	a.mu.Lock()
	a.closed = true
	tr := a.chosen
	a.mu.Unlock()
	return tr.Close()
}

// looksLikeWrongRevision reports whether a Send failure means "this endpoint
// is not a 2025-03-26 POST endpoint" rather than "this request failed".
func looksLikeWrongRevision(err error) bool {
	var se *HTTPStatusError
	if !errors.As(err, &se) {
		return false
	}
	// 405 is the direct answer. 404 and 400 are what a 2024-11-05 server
	// returns for a POST to its GET-only stream URL; a transport failure or a
	// 5xx is a working server having a bad day and must not silently change
	// which protocol revision we speak.
	return se.Status == http.StatusMethodNotAllowed ||
		se.Status == http.StatusNotFound ||
		se.Status == http.StatusBadRequest
}
