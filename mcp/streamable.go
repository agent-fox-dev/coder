package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// StreamableHTTPTransport is REQ-MCP-CLIENT-02's remote transport at
// revision 2026-07-28.
//
// One endpoint, POST only. Every client message is its own request; the server
// answers with a single JSON object or an SSE stream scoped to THAT request.
//
// What is deliberately absent, because the revision removed it: the standalone
// GET stream, protocol-level sessions and the `Mcp-Session-Id` header, and
// `Last-Event-ID` resumability. A broken response stream loses the in-flight
// request and the caller must re-issue it with a new id — there is no replay.
type StreamableHTTPTransport struct {
	*httpCommon
}

// StartStreamableHTTP opens a transport. Nothing is sent until the first Send:
// there is no connect step in this revision, and dialling eagerly would report
// an unreachable server from a constructor rather than from the call that
// needed it.
func StartStreamableHTTP(ctx context.Context, opts HTTPTransportOptions) (*StreamableHTTPTransport, error) {
	c, err := newHTTPCommon(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &StreamableHTTPTransport{httpCommon: c}, nil
}

var _ Transport = (*StreamableHTTPTransport)(nil)

func (t *StreamableHTTPTransport) Send(frame []byte) error {
	if err := t.ctx.Err(); err != nil {
		return ErrTransportClosed
	}
	resp, err := t.post(t.ctx, frame)
	if err != nil {
		return err
	}
	return t.consume(resp)
}

// routingHeaders derives the required headers FROM THE BODY.
//
// They are derived, never accepted from a caller, because the server MUST
// reject a request whose headers disagree with its body (-32020). Two sources
// of truth is exactly the split the requirement exists to prevent: a gateway
// routes on the header while the server executes the body.
func routingHeaders(frame []byte) (map[string]string, error) {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
			Meta struct {
				ProtocolVersion string `json:"io.modelcontextprotocol/protocolVersion"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &m); err != nil {
		return nil, fmt.Errorf("mcp: cannot derive routing headers: %w", err)
	}

	version := m.Params.Meta.ProtocolVersion
	if version == "" {
		version = ProtocolVersion
	}
	h := map[string]string{
		HeaderProtocolVersion: version,
		HeaderMethod:          m.Method,
	}
	// Mcp-Name mirrors params.name or params.uri, and is required only for the
	// methods that have one.
	switch m.Method {
	case MethodToolsCall:
		if m.Params.Name != "" {
			h[HeaderName] = EncodeHeaderValue(m.Params.Name)
		}
	case MethodResourcesRead:
		if m.Params.URI != "" {
			h[HeaderName] = EncodeHeaderValue(m.Params.URI)
		}
	}
	return h, nil
}

// EncodeHeaderValue applies the spec's Base64 sentinel to anything that is not
// safe as a plain ASCII header value.
//
// A tool name is only SHOULD-constrained to header-safe characters, so a
// server may legitimately offer one this cannot be sent verbatim. Sending it
// raw would either be rejected by net/http or, worse, split the header.
func EncodeHeaderValue(v string) string {
	if headerSafeValue(v) {
		return v
	}
	return Base64SentinelPrefix + base64.StdEncoding.EncodeToString([]byte(v)) + Base64SentinelSuffix
}

// DecodeHeaderValue reverses EncodeHeaderValue. A value that is not wrapped in
// the sentinel is returned unchanged, which is what the spec requires of a
// server comparing the header against the body.
func DecodeHeaderValue(v string) string {
	if !strings.HasPrefix(v, Base64SentinelPrefix) || !strings.HasSuffix(v, Base64SentinelSuffix) {
		return v
	}
	inner := v[len(Base64SentinelPrefix) : len(v)-len(Base64SentinelSuffix)]
	raw, err := base64.StdEncoding.DecodeString(inner)
	if err != nil {
		// Not decodable: return it verbatim so the server's comparison fails
		// and reports a mismatch, rather than silently comparing garbage.
		return v
	}
	return string(raw)
}

// headerSafeValue reports whether v may travel as a plain header value:
// visible ASCII and spaces, with no leading or trailing whitespace.
func headerSafeValue(v string) bool {
	if v == "" || v != strings.TrimSpace(v) {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	// A value that would itself look like the sentinel must be encoded, or a
	// server cannot tell an encoded value from a literal one.
	return !strings.HasPrefix(v, Base64SentinelPrefix)
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

	routing, err := routingHeaders(frame)
	if err != nil {
		return nil, err
	}
	for k, v := range routing {
		req.Header.Set(k, v)
	}
	return t.hc.Do(req)
}

// consume handles one POST response.
func (t *StreamableHTTPTransport) consume(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent:
		// A notification. There is nothing to read, and reading anyway would
		// hold the connection open.
		drainAndClose(resp.Body, 4<<10)
		return nil

	case resp.StatusCode >= 400:
		body := drainAndClose(resp.Body, 8<<10)
		// A 400 may carry a JSON-RPC error the caller needs to act on —
		// -32022 names the versions the server speaks, -32020 says the
		// headers and body disagreed. Delivering it as a frame lets the
		// correlator route it to the waiting call, where a transport error
		// would surface as an unexplained failure.
		if delivered := t.deliverRPCError(body); delivered {
			return nil
		}
		return &HTTPStatusError{
			Status: resp.StatusCode, Method: http.MethodPost,
			URL: t.endpoint.String(), Body: string(bytes.TrimSpace(body)),
		}
	}

	ct := resp.Header.Get("Content-Type")
	switch {
	case isEventStream(ct):
		// The stream stays open until the server finishes answering, which may
		// be after several notifications. Reading it on this goroutine would
		// block Send until then, and Send is on the caller's request path.
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

// deliverRPCError routes a JSON-RPC error body carried by a 4xx to the waiting
// call. It reports whether it did.
func (t *StreamableHTTPTransport) deliverRPCError(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var probe struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return false
	}
	if probe.JSONRPC != Version || probe.Error == nil || len(probe.ID) == 0 || string(probe.ID) == "null" {
		// An error with no id cannot be correlated with anything; the caller
		// gets the HTTP error instead, which at least names the status.
		return false
	}
	t.deliver(append([]byte(nil), trimmed...))
	return true
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
