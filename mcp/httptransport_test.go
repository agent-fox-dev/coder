package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agentfox/agentkit-go/internal/diag"
	"github.com/agentfox/agentkit-go/mcp"
	"github.com/agentfox/agentkit-go/wire"
)

// ---- 2025-03-26 streamable HTTP

// TestStreamableHTTPCarriesAWholeSession is the end-to-end shape: the SHIPPED
// client, over the SHIPPED transport, against a server that answers the way
// the 2025-03-26 spec says to.
func TestStreamableHTTPCarriesAWholeSession(t *testing.T) {
	srv := httptest.NewServer(streamableHandler(t, streamableOptions{}))
	t.Cleanup(srv.Close) // LIFO: runs after the transport cleanup that unblocks it

	conn := httpConn(t, mcp.HTTPTransportOptions{URL: srv.URL})
	ctx := context.Background()
	must(t, conn.Discover(ctx))

	tools, err := conn.ListTools(ctx)
	must(t, err)
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools/list over streamable http returned %+v", tools)
	}

	res, err := conn.Call(ctx, "echo", map[string]any{"message": "hi"})
	must(t, err)
	if res.IsError || len(res.Content) != 1 || res.Content[0].Text != "hi" {
		t.Fatalf("tools/call returned %+v", res)
	}
}

// TestStreamableHTTPAcceptsAnSSEAnsweredPOST. A 2025-03-26 server may answer
// the same POST with either a JSON body or an event stream, chosen per
// request; a client that handles only one of them works against half the
// servers and fails confusingly against the other half.
func TestStreamableHTTPAcceptsAnSSEAnsweredPOST(t *testing.T) {
	srv := httptest.NewServer(streamableHandler(t, streamableOptions{answerWithSSE: true}))
	t.Cleanup(srv.Close) // LIFO: runs after the transport cleanup that unblocks it

	conn := httpConn(t, mcp.HTTPTransportOptions{URL: srv.URL})
	ctx := context.Background()
	must(t, conn.Discover(ctx))
	res, err := conn.Call(ctx, "echo", map[string]any{"message": "streamed"})
	must(t, err)
	if len(res.Content) != 1 || res.Content[0].Text != "streamed" {
		t.Fatalf("an SSE-answered POST must decode the same as a JSON one; got %+v", res)
	}
}

// TestAnOversizedJSONResponseIsRefused is REQ-SEC-11.2 on this surface: bound
// before allocating, or a server chooses how much memory we spend.
func TestAnOversizedJSONResponseIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"pad":"`))
		_, _ = w.Write([]byte(strings.Repeat("a", 4096)))
		_, _ = w.Write([]byte(`"}}`))
	}))
	t.Cleanup(srv.Close) // LIFO: runs after the transport cleanup that unblocks it

	tr, err := mcp.StartStreamableHTTP(context.Background(), mcp.HTTPTransportOptions{
		URL:    srv.URL,
		Limits: wire.Limits{MaxMessageBytes: 512},
	})
	must(t, err)
	defer tr.Close()

	if err := tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)); err == nil {
		t.Fatal("a response body past the limit must be refused, not buffered")
	}
}

// ---- 2024-11-05 HTTP+SSE

// ---- auto-negotiation

// ---- construction

// TestOnlyHTTPSchemesAreTransports. A file:// or custom-scheme endpoint in a
// config file is either a mistake or an attempt to make the client read
// something local; there is no MCP server behind either.
func TestOnlyHTTPSchemesAreTransports(t *testing.T) {
	for _, bad := range []string{"file:///etc/passwd", "ftp://h/x", "ws://h/x", "", "http://"} {
		if _, err := mcp.StartStreamableHTTP(context.Background(),
			mcp.HTTPTransportOptions{URL: bad}); err == nil {
			t.Fatalf("url %q must be refused", bad)
		}
	}
}

// ---- helpers

func httpConn(t *testing.T, opts mcp.HTTPTransportOptions) *mcp.ServerConnection {
	t.Helper()
	tr, err := mcp.StartStreamableHTTP(context.Background(), opts)
	must(t, err)
	conn := mcp.NewConnection(mcp.ServerConfig{Name: "remote"}, tr, mcp.ConnectionOptions{
		Warnf: func(f string, a ...any) { t.Logf("client: "+f, a...) },
	})
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

type streamableOptions struct {
	answerWithSSE bool
	refuseGET     bool
	onRequest     func(*http.Request)
}

// streamableHandler is a minimal 2025-03-26 server.
func streamableHandler(t *testing.T, opts streamableOptions) http.Handler {
	t.Helper()
	theAnswer := answerRPC
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.onRequest != nil {
			opts.onRequest(r)
		}
		if r.Method == http.MethodGet {
			if opts.refuseGET {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		id, method, params := readRPC(t, r)
		if !id.IsSet() {
			w.WriteHeader(http.StatusAccepted) // a notification
			return
		}
		result := theAnswer(t, method, params)
		if opts.answerWithSSE {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
			must(t, err)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
			w.(http.Flusher).Flush()
			return
		}
		writeJSONRPC(t, w, id, result)
	})
}

// sseServer is a minimal 2024-11-05 server: a GET stream plus a POST endpoint.
type sseServer struct {
	t            *testing.T
	base         string
	endpoint     string // relative or absolute; empty means "<base>/messages"
	refusePOSTAt string // a path that answers POST with 405, like a stream url

	mu      sync.Mutex
	streams []chan string
}

func newSSEServer(t *testing.T) *sseServer { return &sseServer{t: t} }

func (s *sseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && s.refusePOSTAt != "" && r.URL.Path == s.refusePOSTAt {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	switch {
	case r.Method == http.MethodGet:
		ch := make(chan string, 16)
		s.mu.Lock()
		s.streams = append(s.streams, ch)
		s.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		ep := s.endpoint
		if ep == "" {
			ep = s.base + "/messages"
		}
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", ep)
		w.(http.Flusher).Flush()
		for {
			select {
			case msg := <-ch:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}

	case r.Method == http.MethodPost:
		id, method, params := readRPC(s.t, r)
		w.WriteHeader(http.StatusAccepted)
		if !id.IsSet() {
			return
		}
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "result": answerRPC(s.t, method, params)})
		must(s.t, err)
		s.mu.Lock()
		streams := append([]chan string(nil), s.streams...)
		s.mu.Unlock()
		for _, ch := range streams {
			select {
			case ch <- string(body):
			default:
			}
		}

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// answerRPC is the one server behaviour both fixtures share.
func answerRPC(t *testing.T, method string, params json.RawMessage) any {
	t.Helper()
	switch method {
	case mcp.MethodDiscover:
		return map[string]any{
			"resultType":        "complete",
			"supportedVersions": []string{mcp.ProtocolVersion},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"ttlMs":             0,
			"cacheScope":        "private",
			"_meta": map[string]any{
				mcp.MetaServerInfo: map[string]any{"name": "remote", "version": "1"}},
		}
	case mcp.MethodToolsList:
		return map[string]any{"resultType": "complete", "ttlMs": 0, "cacheScope": "private",
			"tools": []any{map[string]any{
				"name": "echo", "description": "echo", "inputSchema": map[string]any{"type": "object"},
			}}}
	case mcp.MethodToolsCall:
		var p struct {
			Arguments map[string]any `json:"arguments"`
		}
		must(t, json.Unmarshal(params, &p))
		msg, _ := p.Arguments["message"].(string)
		return map[string]any{"resultType": "complete",
			"content": []any{map[string]any{"type": "text", "text": msg}}}
	}
	return map[string]any{}
}

func readRPC(t *testing.T, r *http.Request) (mcp.ID, string, json.RawMessage) {
	t.Helper()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	must(t, err)
	var m mcp.Message
	must(t, json.Unmarshal(body, &m))
	return m.ID, m.Method, m.Params
}

func readID(t *testing.T, r *http.Request) mcp.ID {
	t.Helper()
	id, _, _ := readRPC(t, r)
	return id
}

func writeJSONRPC(t *testing.T, w http.ResponseWriter, id mcp.ID, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	must(t, err)
	_, _ = w.Write(body)
}

// ---- remote server configuration

// TestARemoteServersHeadersAreInterpolatedFromSecrets. A bearer token for a
// remote server has the same reason not to sit in a config file as a
// subprocess's credential does (REQ-MCP-CLIENT-10), so headers resolve through
// the same ${VAR} path.
func TestARemoteServersHeadersAreInterpolatedFromSecrets(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(recordingHandler(t, &got))
	t.Cleanup(srv.Close)

	p := mcp.NewPool(mcp.ConnectionOptions{})
	t.Cleanup(func() { _ = p.Close() })
	_, err := p.Connect(context.Background(), mcp.ServerConfig{
		Name: "remote", URL: srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${GH_TOKEN}"},
	}, nil, func(name string) string {
		if name == "GH_TOKEN" {
			return "ghp_secret"
		}
		return ""
	})
	must(t, err)

	if h := got.Get("Authorization"); h != "Bearer ghp_secret" {
		t.Fatalf("Authorization was %q; the ${VAR} must resolve from the secrets store", h)
	}
}

// TestAHeaderThatResolvesToNothingIsDroppedNotSentBlank. An empty
// Authorization header is not a weaker credential, it is a malformed request,
// and the 401 it earns tells an operator far less than the warning does.
func TestAHeaderThatResolvesToNothingIsDroppedNotSentBlank(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(recordingHandler(t, &got))
	t.Cleanup(srv.Close)

	var warnings []string
	p := mcp.NewPool(mcp.ConnectionOptions{
		Warnf: func(f string, a ...any) { warnings = append(warnings, fmt.Sprintf(f, a...)) },
	})
	t.Cleanup(func() { _ = p.Close() })
	_, err := p.Connect(context.Background(), mcp.ServerConfig{
		Name: "remote", URL: srv.URL,
		Headers: map[string]string{"Authorization": "Bearer ${MISSING}"},
	}, nil, func(string) string { return "" })
	must(t, err)

	if _, present := got["Authorization"]; present {
		t.Fatalf("an unresolved header must be dropped, not sent as %q", got.Get("Authorization"))
	}
	if len(warnings) == 0 {
		t.Fatal("dropping a header must warn; silently sending no credential is how a " +
			"401 becomes a mystery")
	}
}

// TestAHeaderCarryingAControlByteIsRefused is where isHeaderSafe is reachable:
// the value can arrive from a config file or from an interpolated secret, and
// neither goes through Go's response-header validation.
func TestAHeaderCarryingAControlByteIsRefused(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(recordingHandler(t, &got))
	t.Cleanup(srv.Close)

	p := mcp.NewPool(mcp.ConnectionOptions{})
	t.Cleanup(func() { _ = p.Close() })
	_, err := p.Connect(context.Background(), mcp.ServerConfig{
		Name: "remote", URL: srv.URL,
		Headers: map[string]string{"X-Token": "abc${INJECT}"},
	}, nil, func(string) string { return "def\r\nX-Smuggled: yes" })
	must(t, err)

	if _, present := got["X-Token"]; present {
		t.Fatal("a header value carrying CRLF must be dropped, not sent")
	}
	if got.Get("X-Smuggled") != "" {
		t.Fatal("a smuggled header reached the server")
	}
}

func hasError(diags []mcp.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == diag.SeverityError {
			return true
		}
	}
	return false
}

// recordingHandler is a minimal streamable-HTTP server that keeps the headers
// of the first POST it sees.
//
// The method guard is not decoration: the transport also opens a standalone
// GET for server-initiated messages, and that request has no body — decoding
// one as JSON-RPC fails for a reason that has nothing to do with the test.
func recordingHandler(t *testing.T, got *http.Header) http.Handler {
	t.Helper()
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		if *got == nil {
			*got = r.Header.Clone()
		}
		mu.Unlock()

		id, method, params := readRPC(t, r)
		if !id.IsSet() {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSONRPC(t, w, id, answerRPC(t, method, params))
	})
}

// ---- the SSE decoder's own bounds and framing

// TestAnOversizedSSEEventIsRefused is REQ-SEC-11.2 on the streaming path. The
// JSON path bounds its body with a LimitReader; an event stream has no
// Content-Length to bound, so a server that never ends a `data:` field is
// asking us to buffer it forever.
//
// The failure surfaces from Receive, not Send: the stream is read on its own
// goroutine so a slow answer does not block the caller's request path.
func TestAnOversizedSSEEventIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Many small data lines rather than one long one, so this exercises
		// the ACCUMULATED bound and not the scanner's per-line buffer.
		for i := 0; i < 40; i++ {
			fmt.Fprintf(w, "data: %s\n", strings.Repeat("a", 64))
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	tr, err := mcp.StartStreamableHTTP(context.Background(), mcp.HTTPTransportOptions{
		URL:    srv.URL,
		Limits: wire.Limits{MaxMessageBytes: 512},
	})
	must(t, err)
	t.Cleanup(func() { _ = tr.Close() })

	must(t, tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)))
	_, err = tr.Receive()
	if !errors.Is(err, mcp.ErrSSEEventTooLarge) {
		t.Fatalf("an event past the limit must be refused; got %v", err)
	}
}

// TestASSEEventJoinsItsDataLines. A JSON-RPC frame split across `data:` lines
// is only a frame once they are joined; a decoder that kept the last line, or
// dispatched each one, delivers garbage.
//
// The stream also carries a keep-alive comment. That is context, not the
// assertion: a comment is ignored by two independent paths in the decoder, so
// no behaviour here can distinguish them.
func TestASSEEventJoinsItsDataLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, ": keep-alive\n\n")
		// Split between a comma and the next key, where SSE's newline join
		// lands on insignificant JSON whitespace. Splitting inside a string
		// literal would make the reassembled frame invalid by construction —
		// which is why servers do not do it either.
		fmt.Fprint(w, "event: message\n"+
			"data: {\"jsonrpc\":\"2.0\",\"id\":1,\n"+
			"data: \"result\":{\"ok\":true}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	tr, err := mcp.StartStreamableHTTP(context.Background(), mcp.HTTPTransportOptions{
		URL: srv.URL})
	must(t, err)
	t.Cleanup(func() { _ = tr.Close() })

	must(t, tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)))
	frame, err := tr.Receive()
	must(t, err)
	// Either half alone is invalid JSON, so this parses only if both lines
	// reached the decoder as one event. (Whether they were joined with a
	// newline or with nothing is not observable in a JSON payload, and no
	// other payload crosses this transport.)
	var m mcp.Message
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("the joined event must be one valid frame; got %q: %v", frame, err)
	}
	if m.ID.Key() != mcp.NumberID(1).Key() {
		t.Fatalf("frame carried id %s, want 1", m.ID)
	}
}

// TestAStreamThatEndsMidEventIsAnError. Dispatching a half-read event would
// hand the layer above a truncated frame, which it would treat as malformed
// and tear the session down for — blaming the framing rather than the truncated
// stream that caused it.
func TestAStreamThatEndsMidEventIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\"")
		w.(http.Flusher).Flush()
		// Return without the blank line that dispatches the event.
	}))
	t.Cleanup(srv.Close)

	tr, err := mcp.StartStreamableHTTP(context.Background(), mcp.HTTPTransportOptions{
		URL: srv.URL})
	must(t, err)
	t.Cleanup(func() { _ = tr.Close() })

	must(t, tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)))
	if _, err := tr.Receive(); err == nil {
		t.Fatal("a stream that ends mid-event must report an error, not deliver a " +
			"truncated frame")
	}
}

// TestAnOversizedSSELineIsRefused is the OTHER half of the streaming bound.
//
// The accumulated bound catches many small data lines; this catches one line
// that never ends, which the accumulator never sees because the scanner is
// still buffering it. Both are needed: a server picks which shape to send.
func TestAnOversizedSSELineIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: ")
		for i := 0; i < 64; i++ {
			fmt.Fprint(w, strings.Repeat("a", 1024))
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	tr, err := mcp.StartStreamableHTTP(context.Background(), mcp.HTTPTransportOptions{
		URL:    srv.URL,
		Limits: wire.Limits{MaxMessageBytes: 4096},
	})
	must(t, err)
	t.Cleanup(func() { _ = tr.Close() })

	must(t, tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)))
	if _, err := tr.Receive(); !errors.Is(err, mcp.ErrSSEEventTooLarge) {
		t.Fatalf("a single unterminated data line must be refused; got %v", err)
	}
}
