package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/mcp"
	"github.com/agentfox/agentkit-go/wire"
)

// ---- 1.2: a peer-sent -32001 is an error, never a re-issue

// TestAPeerSentStreamBrokenCodeDoesNotReissueTheCall. CodeResponseStreamBroken
// is minted by the transport when a response stream ends with no response on
// it, and the client re-issues that one request. The code travels in a frame,
// and a peer can write the same frame: if the code alone triggered the
// re-issue, a server could make any tool run twice by answering -32001 the
// first time. The re-issue rides on the transport's own out-of-band record.
func TestAPeerSentStreamBrokenCodeDoesNotReissueTheCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, method, params := readRPC(t, r)
		if method != mcp.MethodToolsCall {
			writeJSONRPC(t, w, id, answerRPC(t, method, params))
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": mcp.CodeResponseStreamBroken, "message": "not mine to say"}})
		must(t, err)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	conn := httpConn(t, mcp.HTTPTransportOptions{URL: srv.URL})
	_, err := conn.Call(context.Background(), "echo", map[string]any{"message": "once"})
	var rpcErr *mcp.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeResponseStreamBroken {
		t.Fatalf("err = %v; want the peer's -32001 surfaced as the call's error", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("tools/call was issued %d time(s) on a peer-sent -32001; want exactly 1 — "+
			"a re-issue here is a duplicate tool execution the peer chose", n)
	}
}

// ---- 1.3: Call returns at timeout_s even when the cancellation cannot be sent

// stallingTransport accepts the first Send (the request) and stalls every
// later one until it is closed. It is the shape of a stdio pipe the peer has
// stopped draining.
type stallingTransport struct {
	sends  atomic.Int32
	closed chan struct{}
	once   sync.Once
}

func newStallingTransport() *stallingTransport {
	return &stallingTransport{closed: make(chan struct{})}
}

func (s *stallingTransport) Send([]byte) error {
	if s.sends.Add(1) == 1 {
		return nil
	}
	<-s.closed
	return mcp.ErrTransportClosed
}

func (s *stallingTransport) Receive() ([]byte, error) {
	<-s.closed
	return nil, mcp.ErrTransportClosed
}

func (s *stallingTransport) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// TestCallReturnsAtTheTimeoutWhenTheCancellationCannotBeSent is
// REQ-MCP-CLIENT-07's timeout_s on the path where it used to be a lie: a
// timed-out call sends notifications/cancelled as a courtesy, and it sent it
// on the caller's goroutine with no deadline. The transport that failed to
// answer in time is the one that write goes to, so a wedged peer held Call
// for as long as it liked — after the timeout had "fired".
func TestCallReturnsAtTheTimeoutWhenTheCancellationCannotBeSent(t *testing.T) {
	tr := newStallingTransport()
	conn := mcp.NewConnection(mcp.ServerConfig{Name: "stall", Timeout: 100 * time.Millisecond}, tr,
		mcp.ConnectionOptions{})
	t.Cleanup(func() { _ = conn.Close() })

	done := make(chan error, 1)
	go func() {
		_, err := conn.Call(context.Background(), "slow", map[string]any{})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v; want the call's own deadline", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Call hung past timeout_s inside the cancellation notification; the " +
			"wedged transport held the caller")
	}
	// The courtesy is still paid — on its own goroutine, under its own
	// deadline — so a peer that IS reading learns the client gave up.
	waitFor(t, "the cancellation notification to be attempted", func() bool {
		return tr.sends.Load() >= 2
	})
}

// ---- 2.1: the default HTTP client does not follow redirects

// TestTheDefaultHTTPClientDoesNotFollowRedirects. The configured headers carry
// the server's bearer token, and net/http forwards custom headers (and, on a
// 307, the body) to wherever a redirect points. An endpoint that answers 307
// — misconfigured, or compromised — would receive our credential at an
// address the operator never configured. The response is used as-is instead,
// and the call fails where the operator can see it.
func TestTheDefaultHTTPClientDoesNotFollowRedirects(t *testing.T) {
	var forwarded atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		id, method, params := readRPC(t, r)
		writeJSONRPC(t, w, id, answerRPC(t, method, params))
	}))
	t.Cleanup(elsewhere.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	conn := httpConn(t, mcp.HTTPTransportOptions{URL: origin.URL,
		Headers: map[string]string{"X-Api-Key": "the-secret"}})
	if err := conn.Discover(context.Background()); err == nil {
		t.Fatal("a redirected discover must fail, not succeed against a server we never configured")
	}
	if n := forwarded.Load(); n != 0 {
		t.Fatalf("the redirect target received %d request(s) carrying our headers; the "+
			"client must not follow a redirect", n)
	}
}

// ---- 2.2 / 2.7: the tool cache and the ttlMs hint

// listCounter is a 2026-07-28 server whose tools/list answer is chosen per
// test and counted.
func listCounter(t *testing.T, list func() map[string]any) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, method, params := readRPC(t, r)
		if method != mcp.MethodToolsList {
			writeJSONRPC(t, w, id, answerRPC(t, method, params))
			return
		}
		lists.Add(1)
		writeJSONRPC(t, w, id, list())
	}))
	t.Cleanup(srv.Close)
	return srv, &lists
}

func oneTool(fields ...any) map[string]any {
	out := map[string]any{"resultType": "complete", "cacheScope": "private",
		"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
	for i := 0; i+1 < len(fields); i += 2 {
		out[fields[i].(string)] = fields[i+1]
	}
	return out
}

// TestAnExplicitZeroTTLMeansDoNotCache. ttlMs: 0 is the server saying
// "immediately stale" — the honest hint for a registry a host can mutate at
// any moment, and what this package's own server sends by default. The client
// read 0 and ABSENT the same way, as "cache until told otherwise", which for
// the server that sent 0 is the opposite of what it asked: a url server has no
// subscription stream, so nothing would ever have told it.
func TestAnExplicitZeroTTLMeansDoNotCache(t *testing.T) {
	srv, lists := listCounter(t, func() map[string]any { return oneTool("ttlMs", 0) })
	conn := httpConn(t, mcp.HTTPTransportOptions{URL: srv.URL})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tools, err := conn.ListTools(ctx)
		must(t, err)
		if len(tools) != 1 {
			t.Fatalf("list %d returned %d tools", i, len(tools))
		}
	}
	if n := lists.Load(); n != 3 {
		t.Fatalf("tools/list was issued %d time(s) under ttlMs: 0; want 3 — an explicit zero "+
			"is \"do not cache\", not \"no hint\"", n)
	}
}

// TestAnAbsentTTLStillCachesUntilInvalidated is the other half: a server that
// sends no hint keeps the behaviour it had, so distinguishing 0 from absent
// does not turn every non-conforming server into a re-list per turn.
func TestAnAbsentTTLStillCachesUntilInvalidated(t *testing.T) {
	srv, lists := listCounter(t, func() map[string]any { return oneTool() })
	conn := httpConn(t, mcp.HTTPTransportOptions{URL: srv.URL})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := conn.ListTools(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := lists.Load(); n != 1 {
		t.Fatalf("tools/list was issued %d time(s) with no hint; want 1", n)
	}
}

// TestAListChangedDuringAnInFlightListIsNotLost. tools/list_changed clears
// the cache; tools/list fills it. When the notification lands while a list is
// in flight, the list that completes second was answered by a server that may
// already have changed — and storing it re-validates a cache that nothing
// will ever invalidate again. The list is stored only if no invalidation
// happened while it was out.
func TestAListChangedDuringAnInFlightListIsNotLost(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv, lists := listCounter(t, func() map[string]any {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return oneTool() // no hint: cacheable, so the bug would be observable
	})
	conn := httpConn(t, mcp.HTTPTransportOptions{URL: srv.URL})
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		_, err := conn.ListTools(ctx)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the list never reached the server")
	}
	// The invalidation arrives while the list is in flight.
	conn.RefreshTools()
	close(release)
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("the in-flight list never returned")
	}

	if _, err := conn.ListTools(ctx); err != nil {
		t.Fatal(err)
	}
	if n := lists.Load(); n != 2 {
		t.Fatalf("tools/list was issued %d time(s); want 2 — the list that raced the "+
			"invalidation must not re-validate the cache", n)
	}
}

// ---- 2.3: timeout_s applies to every request-shaped operation

// TestTimeoutSAppliesToDiscoveryListingAndReads. Call had the deadline;
// nothing else did, so a server that stalled on tools/list held a session's
// prompt assembly forever — and Discover, on the pool's connect path, held
// startup. Subscribe is the deliberate exception: a stream held open on
// purpose.
func TestTimeoutSAppliesToDiscoveryListingAndReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept every POST, promise JSON, deliver nothing.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ops := map[string]func(context.Context, *mcp.ServerConnection) error{
		"Discover": func(ctx context.Context, c *mcp.ServerConnection) error { return c.Discover(ctx) },
		"ListTools": func(ctx context.Context, c *mcp.ServerConnection) error {
			_, err := c.ListTools(ctx)
			return err
		},
		"ListResources": func(ctx context.Context, c *mcp.ServerConnection) error {
			_, err := c.ListResources(ctx)
			return err
		},
		"ReadResource": func(ctx context.Context, c *mcp.ServerConnection) error {
			_, err := c.ReadResource(ctx, "x://y")
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			conn := httpConnFor(t, mcp.ServerConfig{Name: "remote", Timeout: 200 * time.Millisecond},
				mcp.HTTPTransportOptions{URL: srv.URL})
			done := make(chan error, 1)
			go func() { done <- op(context.Background(), conn) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v; want the operation's own deadline", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s hung on a stalling server; timeout_s never fired", name)
			}
		})
	}
}

// ---- 2.4: a wedged subscriber does not block registration

// TestAWedgedSubscriberDoesNotBlockRegisterTool. Registration notifies every
// subscribed session, and the notification used to be a transport write on
// the registrar's goroutine: one client that subscribed and stopped reading
// its pipe blocked every RegisterTool on the server, for every other client.
// Notifications are queued per session now, and a session that lets its queue
// overflow is closed rather than obeyed.
func TestAWedgedSubscriberDoesNotBlockRegisterTool(t *testing.T) {
	var warned atomic.Int32
	s := mcp.NewServer(mcp.ServerOptions{Warnf: func(f string, a ...any) {
		if strings.Contains(fmt.Sprintf(f, a...), "not reading") {
			warned.Add(1)
		}
	}})
	raw := rawPeer(t, s)
	raw.write(t, req("1", mcp.MethodSubscriptionsListen, `"notifications":{"toolsListChanged":true}`))
	if ack := raw.read(t); ack.Method != mcp.MethodSubscriptionsAck {
		t.Fatalf("first frame = %+v, want the subscription acknowledgement", ack)
	}
	// From here the peer reads nothing. Its pipe has no buffer at all: the
	// first notification write blocks until somebody reads it, and nobody
	// will.

	const registrations = 200 // comfortably past any sane queue depth
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < registrations; i++ {
			_ = s.RegisterTool(mcp.ToolDefinition{Name: fmt.Sprintf("t%d", i)},
				func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
					return mcp.ToolsCallResult{}, nil
				})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RegisterTool blocked behind a subscriber that is not reading")
	}
	waitFor(t, "the wedged session to be reported and closed", func() bool { return warned.Load() > 0 })
}

// ---- 2.5: a stderr line the scanner cannot buffer does not wedge the child

// TestAnOversizedStderrLineDoesNotWedgeTheServer. The stderr reader delivered
// lines through a bufio.Scanner and stopped at the first line it could not
// buffer — which closed the read end while the child still held the write
// end. A child that logged a 2 MiB line then either blocked on its next
// stderr write (pipe full, nobody reading) or died of SIGPIPE, and its stdout
// frames stopped with it. The rest of stderr is drained now, and the caller
// is told once why its diagnostics stopped.
func TestAnOversizedStderrLineDoesNotWedgeTheServer(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	var mu sync.Mutex
	var warnings []string
	p := mcp.NewPool(mcp.ConnectionOptions{
		Warnf: func(f string, a ...any) {
			mu.Lock()
			warnings = append(warnings, fmt.Sprintf(f, a...))
			mu.Unlock()
		},
	})
	t.Cleanup(func() { _ = p.Close() })
	conn, err := p.Connect(context.Background(), mcp.ServerConfig{Name: "flood", Command: exe},
		[]string{"AGENTKIT_MCP_CHILD=stderr-flood"}, nil)
	if err != nil {
		t.Fatalf("connect: %v — the child wrote a 2 MiB stderr line before serving, and did "+
			"not survive it", err)
	}
	res, err := conn.Call(context.Background(), "hello", nil)
	must(t, err)
	if len(res.Content) != 1 || res.Content[0].Text != "hi" {
		t.Fatalf("result = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, w := range warnings {
		if strings.Contains(w, "stderr line exceeded") {
			return
		}
	}
	t.Fatalf("the caller was never told its diagnostics stopped: %q", warnings)
}

// ---- 2.6: the model's argument bytes pass through verbatim

// TestToolArgumentsPassThroughWithoutFloat64Laundering. The pool decoded the
// model's arguments into a map[string]any to hand them to Call, and Go's
// default for a JSON number is float64: 9007199254740993 came out the other
// side as 9007199254740992 and 1.10 as 1.1. A server checking an id against
// its own records finds nothing, with no error anywhere to explain why. The
// bytes go through as the model wrote them.
func TestToolArgumentsPassThroughWithoutFloat64Laundering(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "inspect"},
		func(_ context.Context, args map[string]any) (mcp.ToolsCallResult, error) {
			mu.Lock()
			got = args
			mu.Unlock()
			return mcp.ToolsCallResult{}, nil
		}))
	conn := pair(t, s, mcp.ServerConfig{Name: "s"}, mcp.ConnectionOptions{})
	p := mcp.NewPool(mcp.ConnectionOptions{})
	must(t, p.Add(conn))

	tools, err := p.Tools(context.Background(), nil)
	must(t, err)
	if len(tools) != 1 {
		t.Fatalf("%d tools", len(tools))
	}
	res := tools[0].Execute(context.Background(),
		json.RawMessage(`{"id":9007199254740993,"ratio":1.10,"exp":1e3}`))
	if !res.OK {
		t.Fatalf("execute failed: %+v", res)
	}

	mu.Lock()
	defer mu.Unlock()
	want := map[string]json.Number{"id": "9007199254740993", "ratio": "1.10", "exp": "1e3"}
	for k, w := range want {
		n, ok := got[k].(json.Number)
		if !ok || n != w {
			t.Fatalf("server received %s = %v (%T); want the literal %s — the arguments were "+
				"laundered through a float64 on the way", k, got[k], got[k], w)
		}
	}
}

// ---- 3.4: non-text content is charged against the cap

// TestNonTextContentIsChargedAgainstTheCap. The cap counted text only, so a
// server could deliver ten megabytes past it as one image block. A non-text
// block costs its Data length; it is never sliced — half a base64 image is a
// corrupt image — but one that does not fit is dropped whole, with the note.
func TestNonTextContentIsChargedAgainstTheCap(t *testing.T) {
	out := mcp.CapContent([]mcp.Content{
		{Type: "text", Text: strings.Repeat("t", 20_000)},
		{Type: "image", Data: strings.Repeat("A", 40_000), MimeType: "image/png"},
		{Type: "text", Text: "after"},
	})
	total := 0
	for _, it := range out {
		if it.Type == "image" {
			t.Fatal("a 40K image after 20K of text does not fit in a 50K cap; it must be dropped, " +
				"and it must not be sliced")
		}
		total += len(it.Text)
	}
	if !strings.Contains(out[len(out)-1].Text, "truncated") {
		t.Fatalf("the drop must carry the note: %+v", out)
	}

	// An image that fits is kept whole and still spends the budget.
	out = mcp.CapContent([]mcp.Content{
		{Type: "image", Data: strings.Repeat("A", 40_000), MimeType: "image/png"},
		{Type: "text", Text: strings.Repeat("t", 20_000)},
	})
	if out[0].Type != "image" || len(out[0].Data) != 40_000 {
		t.Fatalf("an image within the cap must be kept intact: %+v", out[0])
	}
	if len([]rune(out[1].Text)) != 10_000 {
		t.Fatalf("the text after a 40K image gets the remaining 10K, got %d", len([]rune(out[1].Text)))
	}
}

// ---- 3.7: the HTTP body cap honours the smaller wire limit

// TestTheHTTPBodyCapHonoursTheSmallerWireLimit. HTTPOptions.MaxBodyBytes
// defaulted to the WIRE default, not to the server's configured Limits: a host
// with a 256-byte message bound was reading 16 MiB bodies into memory before
// its decoder refused them for their size. The cap is the smaller of the two,
// and an oversized body is refused at the reader (-32600), never parsed.
func TestTheHTTPBodyCapHonoursTheSmallerWireLimit(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{Limits: wire.Limits{MaxMessageBytes: 256}})
	h, err := s.HTTPHandler(mcp.HTTPOptions{APIKey: "k"})
	must(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	big := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` +
		strings.Repeat("x", 4096) + `"}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(big))
	req.Header.Set("X-API-Key", "k")
	resp, err := srv.Client().Do(req)
	must(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var m mcp.Message
	must(t, json.Unmarshal(body, &m))
	if m.Error == nil || m.Error.Code != mcp.CodeInvalidRequest {
		t.Fatalf("response = %s; want the body refused by the reader (%d) under the configured "+
			"%d-byte limit, not read whole and rejected by the parser", body,
			mcp.CodeInvalidRequest, 256)
	}
}
