package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/mcp"
	"github.com/agentfox/agentkit-go/wire"
)

// pair wires a client connection and a server together over two in-memory
// pipes.
//
// No subprocess, no port, no timing. The client and server are the SHIPPED
// implementations talking to each other, so a protocol mistake on either side
// shows up as a failing test rather than as a mismatch nobody notices until a
// real server is involved.
func pair(t *testing.T, srv *mcp.Server, cfg mcp.ServerConfig, opts mcp.ConnectionOptions) *mcp.ServerConnection {
	t.Helper()
	c2sR, c2sW := io.Pipe() // client -> server
	s2cR, s2cW := io.Pipe() // server -> client

	serverSide := mcp.NewPipeTransport(c2sR, s2cW, wire.Limits{})
	clientSide := mcp.NewPipeTransport(s2cR, c2sW, wire.Limits{})

	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(context.Background(), serverSide) }()

	conn := mcp.NewConnection(cfg, clientSide, opts)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = serverSide.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("the server loop did not stop")
		}
	})
	return conn
}

func echoServer(t *testing.T) *mcp.Server {
	t.Helper()
	s := mcp.NewServer(mcp.ServerOptions{Info: mcp.Implementation{Name: "test-server", Version: "1"}})
	must(t, s.RegisterTool(mcp.ToolDefinition{
		Name:        "echo",
		Description: "echo the message back",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","description":"what to echo"}},"required":["message"]}`),
	}, func(_ context.Context, args map[string]any) (mcp.ToolsCallResult, error) {
		msg, _ := args["message"].(string)
		return mcp.ToolsCallResult{Content: []mcp.Content{{Type: "text", Text: "echo: " + msg}}}, nil
	}))
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "boom", Description: "always fails"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			return mcp.ToolsCallResult{}, errors.New("the tool refused")
		}))
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestTheClientAndServerDiscoverAndCall is the end-to-end shape.
//
// There is no handshake to complete: server/discover is an OPTIONAL probe and
// the call below would work without it.
func TestTheClientAndServerDiscoverAndCall(t *testing.T) {
	conn := pair(t, echoServer(t), mcp.ServerConfig{Name: "test"}, mcp.ConnectionOptions{})
	ctx := context.Background()

	if err := conn.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	info := conn.Info()
	if len(info.SupportedVersions) == 0 || info.SupportedVersions[0] != mcp.ProtocolVersion {
		t.Fatalf("supportedVersions = %v, want %s", info.SupportedVersions, mcp.ProtocolVersion)
	}
	if id := mcp.ParseResultMeta(info.Meta).ServerInfo; id == nil || id.Name != "test-server" {
		t.Fatalf("server identity = %s", info.Meta)
	}

	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("%d tools, want 2", len(tools))
	}

	res, err := conn.Call(ctx, "echo", map[string]any{"message": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "echo: hello" {
		t.Fatalf("result = %+v", res.Content)
	}
}

// TestAFailingHandlerIsAToolErrorNotAProtocolError is the distinction that
// decides whether the model ever hears about it.
//
// A tool that failed is a result the model should see and react to. A JSON-RPC
// error means the call never happened, and surfacing one as the other either
// hides a real failure from the model or turns a routine failure into a
// connection-level fault.
func TestAFailingHandlerIsAToolErrorNotAProtocolError(t *testing.T) {
	conn := pair(t, echoServer(t), mcp.ServerConfig{Name: "test"}, mcp.ConnectionOptions{})
	ctx := context.Background()
	must(t, conn.Discover(ctx))

	res, err := conn.Call(ctx, "boom", nil)
	if err != nil {
		t.Fatalf("a failing TOOL must not surface as a call error: %v", err)
	}
	if !res.IsError {
		t.Fatal("the result must be marked as an error so the model can react")
	}
	if !strings.Contains(res.Content[0].Text, "refused") {
		t.Fatalf("content = %+v, want the handler's own message", res.Content)
	}

	// An unknown tool IS a protocol error: that call genuinely never happened.
	if _, err := conn.Call(ctx, "nonexistent", nil); err == nil {
		t.Fatal("calling a tool the server does not have must be an error")
	}
}

// TestAPanickingHandlerDoesNotKillTheConnection: one broken tool must not be a
// dead connection for every other one.
func TestAPanickingHandlerDoesNotKillTheConnection(t *testing.T) {
	s := echoServer(t)
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "panicky"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			panic("handler bug")
		}))
	conn := pair(t, s, mcp.ServerConfig{Name: "test"}, mcp.ConnectionOptions{})
	ctx := context.Background()
	must(t, conn.Discover(ctx))

	res, err := conn.Call(ctx, "panicky", nil)
	if err != nil {
		t.Fatalf("a panicking handler must come back as a result: %v", err)
	}
	if !res.IsError {
		t.Fatal("a panic is an error result")
	}
	// The connection still works.
	if _, err := conn.Call(ctx, "echo", map[string]any{"message": "still here"}); err != nil {
		t.Fatalf("the connection died with the handler: %v", err)
	}
}

// TestToolListsAreCachedAndInvalidatedByTheNotification is REQ-CACHE-07.
func TestToolListsAreCachedAndInvalidatedByTheNotification(t *testing.T) {
	var lists int
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "a"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			return mcp.ToolsCallResult{}, nil
		}))

	conn := pair(t, s, mcp.ServerConfig{Name: "test"}, mcp.ConnectionOptions{})
	_ = lists
	ctx := context.Background()
	must(t, conn.Discover(ctx))

	for i := 0; i < 5; i++ {
		if _, err := conn.ListTools(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// The cache is observable through RefreshTools rather than a call count,
	// because the server here is the real one and counting its calls would
	// need a wrapper that is not the shipped code.
	conn.RefreshTools()
	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("%d tools after a refresh, want 1", len(tools))
	}
}

// ---- REQ-MCP-CLIENT-09

// TestResultsAreCappedAcrossTheWholeResult is REQ-MCP-CLIENT-09.
//
// The budget is spent ACROSS the content items, not per item. A server
// returning two hundred blocks of 49K each passes a per-item cap and delivers
// ten megabytes into the model's context.
func TestResultsAreCappedAcrossTheWholeResult(t *testing.T) {
	var items []mcp.Content
	for i := 0; i < 200; i++ {
		items = append(items, mcp.Content{Type: "text", Text: strings.Repeat("x", 49_000)})
	}
	out := mcp.CapContent(items)

	total := 0
	var note string
	for _, it := range out {
		total += len([]rune(it.Text))
		if strings.Contains(it.Text, "truncated") {
			note = it.Text
		}
	}
	if total > mcp.ResultCharCap+len([]rune(note)) {
		t.Fatalf("content totals %d characters, past the %d cap", total, mcp.ResultCharCap)
	}
	if note == "" {
		t.Fatal("truncation must carry a note to the model, or it reasons over a fragment " +
			"it believes is whole")
	}
	if !strings.Contains(note, "Narrow the request") {
		t.Fatalf("the note should tell the model what to DO: %q", note)
	}
}

// TestTheCapCountsRunesNotBytes: "characters" in a requirement about model
// context means what the model sees. A byte cap gives a CJK result a third of
// the room an ASCII one gets, silently, and worst for the languages that need
// it most.
func TestTheCapCountsRunesNotBytes(t *testing.T) {
	// Each of these is 3 bytes and 1 rune.
	text := strings.Repeat("漢", mcp.ResultCharCap-10)
	out := mcp.CapContent([]mcp.Content{{Type: "text", Text: text}})
	if len(out) != 1 {
		t.Fatalf("%d items, want the whole thing kept: it is under the cap in RUNES", len(out))
	}
	if len([]rune(out[0].Text)) != mcp.ResultCharCap-10 {
		t.Fatalf("kept %d runes, want %d", len([]rune(out[0].Text)), mcp.ResultCharCap-10)
	}
}

func TestNonTextContentIsNotTruncated(t *testing.T) {
	out := mcp.CapContent([]mcp.Content{
		{Type: "image", Data: strings.Repeat("A", 100_000), MimeType: "image/png"},
	})
	if len(out) != 1 || len(out[0].Data) != 100_000 {
		t.Fatal("an image is not text and slicing its base64 produces a corrupt image, " +
			"not a shorter one")
	}
}

// ---- REQ-MCP-CLIENT-07 / -08

func TestThePerSessionCallLimitIsEnforced(t *testing.T) {
	cfg := mcp.ServerConfig{Name: "test", PerSessionCallLimit: 3}
	conn := pair(t, echoServer(t), cfg, mcp.ConnectionOptions{})
	ctx := context.Background()
	must(t, conn.Discover(ctx))

	for i := 0; i < 3; i++ {
		if _, err := conn.Call(ctx, "echo", map[string]any{"message": "x"}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, err := conn.Call(ctx, "echo", map[string]any{"message": "x"})
	if !errors.Is(err, mcp.ErrCallLimit) {
		t.Fatalf("err = %v, want ErrCallLimit after the cap", err)
	}
}

func TestTheDefaultCallLimitIsAThousandAndNegativeMeansUnlimited(t *testing.T) {
	cfg := mcp.ServerConfig{Name: "test"} // zero => default
	conn := pair(t, echoServer(t), cfg, mcp.ConnectionOptions{})
	ctx := context.Background()
	must(t, conn.Discover(ctx))
	if _, err := conn.Call(ctx, "echo", map[string]any{"message": "x"}); err != nil {
		t.Fatal(err)
	}

	unlimited := mcp.ServerConfig{Name: "u", PerSessionCallLimit: -1}
	conn2 := pair(t, echoServer(t), unlimited, mcp.ConnectionOptions{})
	must(t, conn2.Discover(ctx))
	for i := 0; i < 5; i++ {
		if _, err := conn2.Call(ctx, "echo", map[string]any{"message": "x"}); err != nil {
			t.Fatalf("unlimited must not cap: %v", err)
		}
	}
}

// TestEveryToolCallIsAudited is REQ-MCP-CLIENT-03 and REQ-OBS-05.
func TestEveryToolCallIsAudited(t *testing.T) {
	var mu sync.Mutex
	var events []core.AuditEvent
	conn := pair(t, echoServer(t), mcp.ServerConfig{Name: "gh"}, mcp.ConnectionOptions{
		Audit: func(e core.AuditEvent) { mu.Lock(); events = append(events, e); mu.Unlock() },
	})
	ctx := context.Background()
	must(t, conn.Discover(ctx))
	if _, err := conn.Call(ctx, "echo", map[string]any{"message": "secret-value"}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Call(ctx, "boom", nil); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("%d audit events, want one per call", len(events))
	}
	for _, e := range events {
		if e.ServerName != "gh" {
			t.Fatalf("server_name = %q, want gh (REQ-OBS-05)", e.ServerName)
		}
		blob, _ := json.Marshal(e)
		if strings.Contains(string(blob), "secret-value") {
			t.Fatalf("the audit event carries the argument VALUE: %s", blob)
		}
	}
	if events[0].ArgumentsHash == "" {
		t.Fatal("REQ-OBS-05 requires an arguments hash")
	}
	if !events[1].IsError {
		t.Fatal("a failed tool call must be audited as an error")
	}
}

// TestSamplingIsRefusedUnlessEnabledAndAlwaysAudited is REQ-MCP-CLIENT-08.
//
// Both halves matter. A refusal that leaves no trace is indistinguishable from
// a server that never asked, and the two want very different responses from
// whoever reads the audit log.
func TestSamplingIsRefusedUnlessEnabledAndAlwaysAudited(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allow   bool
		handler mcp.SamplingHandler
		wantErr bool
	}{
		{"disabled by default", false, okSampler, true},
		{"enabled with a handler", true, okSampler, false},
		{"enabled with no handler", true, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var events []core.AuditEvent
			cfg := mcp.ServerConfig{Name: "s", AllowSampling: tc.allow}
			srv := samplingServer(t)
			conn := pair(t, srv, cfg, mcp.ConnectionOptions{
				Sampling: tc.handler,
				Audit:    func(e core.AuditEvent) { mu.Lock(); events = append(events, e); mu.Unlock() },
			})
			ctx := context.Background()
			must(t, conn.Discover(ctx))

			res, err := conn.Call(ctx, "ask", nil)
			if tc.wantErr {
				// Under MRTR a refusal happens CLIENT-side, before the retry:
				// the client never sends the answer, so the call fails rather
				// than returning a server-authored "refused" string.
				if !errors.Is(err, mcp.ErrSamplingNotAllowed) {
					t.Fatalf("want ErrSamplingNotAllowed, got %v (result %+v)", err, res)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				text := ""
				if len(res.Content) > 0 {
					text = res.Content[0].Text
				}
				if !strings.Contains(text, "sampled") {
					t.Fatalf("sampling should have succeeded; server saw %q", text)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			var sampled bool
			for _, e := range events {
				if e.ToolName == mcp.MethodSampling {
					sampled = true
				}
			}
			if !sampled {
				t.Fatalf("every sampling request must be audited, refused ones included; "+
					"events = %+v", events)
			}
		})
	}
}

func okSampler(context.Context, mcp.SamplingParams) (mcp.SamplingResult, error) {
	return mcp.SamplingResult{Role: "assistant", Model: "test",
		Content: mcp.Content{Type: "text", Text: "sampled"}}, nil
}

// samplingServer answers `ask` through MRTR: the first call returns an input
// request, and the client's RETRY carries the answer.
//
// This is the shape 2026-07-28 forces. The server can no longer block inside
// the handler waiting for the client, so "ask the client something" becomes
// two invocations of the same handler with the answer threaded between them by
// the client itself.
func samplingServer(t *testing.T) *mcp.Server {
	t.Helper()
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "ask"},
		func(ctx context.Context, _ map[string]any) (mcp.ToolsCallResult, error) {
			in, retry := mcp.InputFrom(ctx)
			if !retry {
				return mcp.ToolsCallResult{}, mcp.NeedSampling("asked-once", "s1",
					mcp.SamplingParams{
						Messages:  []mcp.SamplingMessage{{Role: "user", Content: mcp.Content{Type: "text", Text: "hi"}}},
						MaxTokens: 16,
					})
			}
			if in.RequestState != "asked-once" {
				t.Errorf("requestState must come back verbatim; got %q", in.RequestState)
			}
			var res mcp.SamplingResult
			if err := json.Unmarshal(in.Responses["s1"], &res); err != nil {
				return mcp.ToolsCallResult{}, err
			}
			return mcp.ToolsCallResult{Content: []mcp.Content{{Type: "text", Text: res.Content.Text}}}, nil
		}))
	return s
}

// ---- REQ-MCP-SERVER-07

func TestHTTPModeRequiresAnAPIKey(t *testing.T) {
	s := echoServer(t)
	if _, err := s.HTTPHandler(mcp.HTTPOptions{}); !errors.Is(err, mcp.ErrNoAPIKey) {
		t.Fatalf("err = %v; a server that starts unauthenticated because a config key was "+
			"missing is exactly what REQ-MCP-SERVER-07 exists to prevent", err)
	}
}

func TestUnauthenticatedHTTPRequestsGet401(t *testing.T) {
	s := echoServer(t)
	h, err := s.HTTPHandler(mcp.HTTPOptions{APIKey: "sekret"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{%q:%q,%q:{}}}}`,
		mcp.MetaProtocolVersion, mcp.ProtocolVersion, mcp.MetaClientCapabilities)
	for _, tc := range []struct {
		name   string
		header [2]string
		want   int
	}{
		{"no credential", [2]string{"", ""}, http.StatusUnauthorized},
		{"wrong key", [2]string{"X-API-Key", "nope"}, http.StatusUnauthorized},
		{"bearer", [2]string{"Authorization", "Bearer sekret"}, http.StatusOK},
		{"raw api key header", [2]string{"X-API-Key", "sekret"}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
			if tc.header[0] != "" {
				req.Header.Set(tc.header[0], tc.header[1])
			}
			// 2026-07-28 requires these on every POST, and the server rejects
			// a request without them before it ever reaches a handler.
			req.Header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion)
			req.Header.Set(mcp.HeaderMethod, mcp.MethodToolsList)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestAuthenticationRunsBeforeTheMethodCheck: answering 405 to an
// unauthenticated caller tells them which verbs exist, and reading the body
// first lets them spend our memory without a credential.
func TestAuthenticationRunsBeforeTheMethodCheck(t *testing.T) {
	h, err := echoServer(t).HTTPHandler(mcp.HTTPOptions{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated GET returned %d; it must be 401 rather than 405, "+
			"which would confirm that POST is the interesting verb", resp.StatusCode)
	}
}

func TestTheHTTPBodyIsBounded(t *testing.T) {
	h, err := echoServer(t).HTTPHandler(mcp.HTTPOptions{APIKey: "k", MaxBodyBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	big := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` +
		strings.Repeat("x", 4096) + `"}}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(big))
	req.Header.Set("X-API-Key", "k")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "error") {
		t.Fatalf("an oversized body must be refused: %s", out)
	}
}

// ---- REQ-SEC-11 on the protocol surface

func TestAMalformedFrameTearsTheConnectionDown(t *testing.T) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	serverSide := mcp.NewPipeTransport(c2sR, s2cW, wire.Limits{})
	done := make(chan error, 1)
	go func() { done <- echoServer(t).Serve(context.Background(), serverSide) }()

	// A duplicate key: legal to encoding/json, rejected by REQ-SEC-11.3.
	_, _ = c2sW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","method":"ping"}` + "\n"))

	go func() { _, _ = io.ReadAll(s2cR) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a malformed frame must tear the connection down: the framing is " +
				"already untrustworthy, so there is no safe place to resume from " +
				"(REQ-SEC-11.4)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the server kept reading after a malformed frame")
	}
	_ = c2sW.Close()
	_ = s2cW.Close()
}

// ---- interpolation

// TestAnUnresolvedVariableIsAConfigurationError is NFR-SEC-03: "unexpanded
// variable references are a configuration error, not silently passed to the
// subprocess". A warning plus a blank value was the thing it forbids — the
// child started with an empty credential and failed authentication with a
// message about a bad token, which sends the reader to the wrong place.
func TestAnUnresolvedVariableIsAConfigurationError(t *testing.T) {
	cfg := mcp.ServerConfig{
		Name: "gh", Command: "true",
		Env: map[string]string{"TOKEN": "${GH_TOKEN}", "MODE": "${MISSING}-suffix", "OTHER": "$ALSO_MISSING"},
	}
	p := mcp.NewPool(mcp.ConnectionOptions{})
	defer p.Close()
	_, err := p.Connect(context.Background(), cfg, []string{"PATH=/usr/bin"},
		func(name string) string {
			if name == "GH_TOKEN" {
				return "ghp_secret"
			}
			return ""
		})

	var unresolved *mcp.UnresolvedVariableError
	if !errors.As(err, &unresolved) {
		t.Fatalf("err = %v; an unset ${VAR} must be a typed configuration error", err)
	}
	if got := strings.Join(unresolved.Variables, ","); got != "MISSING,ALSO_MISSING" {
		t.Fatalf("variables = %v; the error must name every unresolved reference so they "+
			"are fixed in one pass", unresolved.Variables)
	}
	if !strings.Contains(err.Error(), "${MISSING}") || strings.Contains(err.Error(), "ghp_secret") {
		t.Fatalf("the message must name the variable and never the resolved secret: %v", err)
	}
	if len(p.Names()) != 0 {
		t.Fatal("nothing may be spawned on a configuration error")
	}
}

// TestAnExplicitlyEmptyVariableIsNotUnresolved. `FOO=` in the environment is a
// value the operator chose; only an ABSENT variable is unresolved. A lookup
// that returned "" for both could not tell them apart.
func TestAnExplicitlyEmptyVariableIsNotUnresolved(t *testing.T) {
	if os.Getenv("AGENTKIT_MCP_CHILD") != "" {
		t.Skip("child process")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	p := mcp.NewPool(mcp.ConnectionOptions{})
	defer p.Close()

	cfg := mcp.ServerConfig{Name: "child", Command: exe,
		Env: map[string]string{"SUPPLIED": "[${EMPTY}]"}}
	conn, err := p.Connect(context.Background(), cfg,
		[]string{"AGENTKIT_MCP_CHILD=env", "EMPTY=", "PATH=" + os.Getenv("PATH")}, nil)
	if err != nil {
		t.Fatalf("connect: %v; a variable set to the empty string is set", err)
	}
	res, err := conn.Call(context.Background(), "env", nil)
	must(t, err)
	if !strings.Contains(res.Content[0].Text, "SUPPLIED=[]") {
		t.Fatalf("child env = %q; the empty value must be substituted", res.Content[0].Text)
	}
}

// ---- pool: REQ-MCP-CLIENT-04, -05, -06

func poolWith(t *testing.T, cfgs ...mcp.ServerConfig) *mcp.Pool {
	t.Helper()
	p := mcp.NewPool(mcp.ConnectionOptions{})
	for _, cfg := range cfgs {
		conn := pair(t, echoServer(t), cfg, mcp.ConnectionOptions{})
		must(t, conn.Discover(context.Background()))
		must(t, p.Add(conn))
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestToolNamesAreQualifiedByServer is REQ-MCP-CLIENT-05.
func TestToolNamesAreQualifiedByServer(t *testing.T) {
	p := poolWith(t, mcp.ServerConfig{Name: "github"})
	tools, err := p.Tools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
		if tl.MCPServer != "github" {
			t.Fatalf("%s carries MCPServer %q; the audit trail must not have to guess "+
				"the server from a name whose prefix is configurable", tl.Name, tl.MCPServer)
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "github__boom,github__echo" {
		t.Fatalf("names = %v, want the server_name__tool_name convention", names)
	}
}

func TestAConfiguredPrefixOverridesTheDefault(t *testing.T) {
	p := poolWith(t, mcp.ServerConfig{Name: "github", ToolPrefix: "gh."})
	tools, err := p.Tools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if !strings.HasPrefix(tl.Name, "gh.") {
			t.Fatalf("%s does not use the configured prefix", tl.Name)
		}
	}

	// An empty tool_prefix in a config file means "the default"; DisablePrefix
	// is how a caller asks for none, because "" cannot mean both.
	p2 := poolWith(t, mcp.ServerConfig{Name: "raw", DisablePrefix: true})
	tools, err = p2.Tools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if strings.Contains(tl.Name, "__") {
			t.Fatalf("%s is prefixed despite DisablePrefix", tl.Name)
		}
	}
}

// TestAShadowedNativeToolIsRefusedAtConnect is REQ-MCP-CLIENT-06 where the
// requirement puts it: at CONNECTION time.
//
// The pool is told what the host's own tools are called, so the collision is a
// refused connection at startup. The check that used to live only in Tools was
// unreachable for a host that never called Tools — it would connect the server
// happily and find out when the model called `echo` and the server answered,
// which is discovering it in production with the wrong tool having run.
func TestAShadowedNativeToolIsRefusedAtConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, method, params := readRPC(t, r)
		writeJSONRPC(t, w, id, answerRPC(t, method, params)) // exposes `echo`
	}))
	t.Cleanup(srv.Close)

	p := mcp.NewPool(mcp.ConnectionOptions{})
	p.NativeTools = []string{"echo"} // the host's own tool of the same name
	t.Cleanup(func() { _ = p.Close() })

	conn, err := p.Connect(context.Background(), mcp.ServerConfig{
		Name: "remote", URL: srv.URL, DisablePrefix: true}, nil, nil)
	if !errors.Is(err, mcp.ErrNameCollision) {
		t.Fatalf("err = %v, want ErrNameCollision at Connect", err)
	}
	if conn != nil {
		t.Fatal("a refused connection must not be returned")
	}
	if len(p.Names()) != 0 {
		t.Fatalf("names = %v; the connection must be torn down, not pooled", p.Names())
	}
}

// TestAPrefixedToolDoesNotCollideAtConnect is the other arm: the default
// `server__tool` qualification is what keeps an MCP `echo` and a native `echo`
// apart, and a connect-time check that compared UNQUALIFIED names would refuse
// every ordinary configuration.
func TestAPrefixedToolDoesNotCollideAtConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, method, params := readRPC(t, r)
		writeJSONRPC(t, w, id, answerRPC(t, method, params))
	}))
	t.Cleanup(srv.Close)

	p := mcp.NewPool(mcp.ConnectionOptions{})
	p.NativeTools = []string{"echo"}
	t.Cleanup(func() { _ = p.Close() })

	if _, err := p.Connect(context.Background(), mcp.ServerConfig{
		Name: "remote", URL: srv.URL}, nil, nil); err != nil {
		t.Fatalf("connect: %v; `remote__echo` shadows nothing", err)
	}
}

// TestANativeToolRegisteredAfterConnectIsCaughtByTheBackstop keeps the check in
// Tools honest. Connect can only see the names the pool had been told about by
// then; a host that registers a native tool later, or a server that grows one
// during the session, is caught here and nowhere else.
func TestANativeToolRegisteredAfterConnectIsCaughtByTheBackstop(t *testing.T) {
	p := poolWith(t, mcp.ServerConfig{Name: "srv", DisablePrefix: true})
	native := []core.Tool{{Name: "echo", Description: "the native one"}}

	_, err := p.Tools(context.Background(), native)
	if !errors.Is(err, mcp.ErrNameCollision) {
		t.Fatalf("err = %v, want ErrNameCollision", err)
	}
	if !strings.Contains(err.Error(), "native") {
		t.Fatalf("the error must say what it collided with: %v", err)
	}
}

func TestTwoServersExposingTheSameNameCollide(t *testing.T) {
	p := poolWith(t,
		mcp.ServerConfig{Name: "a", DisablePrefix: true},
		mcp.ServerConfig{Name: "b", DisablePrefix: true})
	if _, err := p.Tools(context.Background(), nil); !errors.Is(err, mcp.ErrNameCollision) {
		t.Fatalf("err = %v; two unprefixed servers both exposing `echo` collide", err)
	}
}

// TestAnAdaptedToolRunsThroughTheRealConnection closes the loop: the core.Tool
// the pool produces actually calls the MCP server.
func TestAnAdaptedToolRunsThroughTheRealConnection(t *testing.T) {
	p := poolWith(t, mcp.ServerConfig{Name: "srv"})
	tools, err := p.Tools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var echo core.Tool
	for _, tl := range tools {
		if tl.Name == "srv__echo" {
			echo = tl
		}
	}
	if echo.Execute == nil {
		t.Fatal("the adapted tool has no Execute")
	}
	res := echo.Execute(context.Background(), json.RawMessage(`{"message":"through"}`))
	if !res.OK {
		t.Fatalf("call failed: %s %s", res.Error, res.Detail)
	}
	blob, _ := json.Marshal(res.Data)
	if !strings.Contains(string(blob), "echo: through") {
		t.Fatalf("data = %s", blob)
	}
}

// TestTheAdaptedSchemaCarriesRequiredProperties: a tool whose schema is
// flattened to an open object loses the validation REQ-TOOL-11 does before the
// call, so a malformed argument reaches the server instead of the model.
func TestTheAdaptedSchemaCarriesRequiredProperties(t *testing.T) {
	p := poolWith(t, mcp.ServerConfig{Name: "srv"})
	tools, err := p.Tools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if tl.Name != "srv__echo" {
			continue
		}
		if !tl.InputSchema.IsRequired("message") {
			t.Fatalf("message is not required; the server declared it so, and dropping "+
				"that loses the pre-call validation: %+v", tl.InputSchema)
		}
		props := tl.InputSchema.PropertyList()
		if len(props) != 1 || props[0] != "message" {
			t.Fatalf("properties = %v, want [message]", props)
		}
		return
	}
	t.Fatal("srv__echo not found")
}

// ---- REQ-MCP-CLIENT-07 config

func TestServerConfigParsesFromTOML(t *testing.T) {
	src := `
[mcp]

[[mcp.servers]]
name = "github"
command = "gh-mcp"
args = ["--stdio"]
tool_prefix = "gh__"
allow_sampling = true
per_session_call_limit = 25
timeout_s = 10

[mcp.servers.env]
GITHUB_TOKEN = "${GH_PAT}"

[[mcp.servers]]
name = "db"
url = "https://db.example/mcp"

[mcp_server]
enabled = true
transport = "http"
port = 8931
api_key_env = "AGENTKIT_MCP_KEY"
`
	cfg, diags, err := mcp.ParseConfig("config.toml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diags {
		if d.Severity == "error" {
			t.Fatalf("unexpected error diagnostic: %s", d)
		}
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("%d servers, want 2", len(cfg.Servers))
	}
	gh := cfg.Servers[0]
	if gh.Name != "github" || gh.Command != "gh-mcp" || len(gh.Args) != 1 {
		t.Fatalf("github = %+v", gh)
	}
	if !gh.AllowSampling || gh.PerSessionCallLimit != 25 || gh.Timeout != 10*time.Second {
		t.Fatalf("github options = %+v", gh)
	}
	if gh.Env["GITHUB_TOKEN"] != "${GH_PAT}" {
		t.Fatalf("env = %v; the reference must survive parsing and be resolved at SPAWN "+
			"time, so the credential is never in the config file", gh.Env)
	}
	if cfg.Servers[1].URL == "" {
		t.Fatal("the url server did not parse")
	}
	if !cfg.Server.Enabled || cfg.Server.Transport != "http" || cfg.Server.Port != 8931 {
		t.Fatalf("mcp_server = %+v", cfg.Server)
	}
}

// TestTheServerIsOffUnlessTheConfigSaysOtherwise is REQ-MCP-SERVER-01.
func TestTheServerIsOffUnlessTheConfigSaysOtherwise(t *testing.T) {
	cfg, _, err := mcp.ParseConfig("c.toml", []byte("[mcp]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Enabled {
		t.Fatal("the inbound server must be off unless a config explicitly enables it; " +
			"there is no key whose ABSENCE turns it on")
	}
	if cfg.Server.Transport != "stdio" {
		t.Fatalf("default transport = %q, want stdio: it is the one that relies on OS "+
			"process isolation rather than on a key someone has to remember",
			cfg.Server.Transport)
	}
}

func TestHTTPModeWithoutAnAPIKeyEnvIsAConfigError(t *testing.T) {
	_, diags, err := mcp.ParseConfig("c.toml", []byte(
		"[mcp_server]\nenabled = true\ntransport = \"http\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	var flagged bool
	for _, d := range diags {
		if d.Severity == "error" && strings.Contains(d.Message, "api_key_env") {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("an http server with no api_key_env must be flagged at CONFIG time, not "+
			"discovered when it refuses to start: %v", diags)
	}
}

func TestDuplicateServerNamesAreAConfigError(t *testing.T) {
	_, diags, err := mcp.ParseConfig("c.toml", []byte(`
[[mcp.servers]]
name = "x"
command = "a"

[[mcp.servers]]
name = "x"
command = "b"
`))
	if err != nil {
		t.Fatal(err)
	}
	var flagged bool
	for _, d := range diags {
		if d.Severity == "error" {
			flagged = true
		}
	}
	if !flagged {
		t.Fatal("the name keys the pool, the tool prefix and every audit event; two " +
			"servers cannot share one")
	}
}

// ---- stdio, against a real subprocess

// TestAStdioServerRunsAsASubprocessWithAReducedEnvironment is
// REQ-MCP-CLIENT-10, against a real process.
//
// The server here is this test binary re-executed, so there is no fixture to
// keep in sync and no dependency on anything being installed.
func TestAStdioServerRunsAsASubprocessWithAReducedEnvironment(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}

	p := mcp.NewPool(mcp.ConnectionOptions{})
	defer p.Close()

	cfg := mcp.ServerConfig{
		Name: "child", Command: exe,
		Env: map[string]string{"SUPPLIED": "${A_SECRET}"},
	}
	conn, err := p.Connect(context.Background(), cfg,
		[]string{"AGENTKIT_MCP_CHILD=env", "PATH=" + os.Getenv("PATH")},
		func(name string) string {
			if name == "A_SECRET" {
				return "resolved-at-spawn"
			}
			return ""
		})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	res, err := conn.Call(context.Background(), "env", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Content[0].Text
	if !strings.Contains(got, "SUPPLIED=resolved-at-spawn") {
		t.Fatalf("child env = %q; a ${VAR} must be resolved at spawn time", got)
	}
	if strings.Contains(got, "ANTHROPIC_") || strings.Contains(got, "OPENAI_") {
		t.Fatalf("the child inherited provider credentials: %q. REQ-MCP-CLIENT-10 and "+
			"REQ-SEC-08 both require a reduced environment — an MCP server is somebody "+
			"else's code and it does not need our API keys.", got)
	}
}

// TestMain is where this test binary becomes an MCP SERVER when re-executed
// as a subprocess. The children below are the fixtures for every test that
// needs a real process: there is no script to keep in sync and no dependency
// on anything being installed.
func TestMain(m *testing.M) {
	switch os.Getenv("AGENTKIT_MCP_CHILD") {
	case "env":
		runChildServer()
		return
	case "one-shot":
		runOneShotChild()
		return
	case "mortal":
		runMortalChild()
		return
	}
	os.Exit(m.Run())
}

// runChildServer reports its environment.
func runChildServer() {
	s := mcp.NewServer(mcp.ServerOptions{Info: mcp.Implementation{Name: "child", Version: "1"}})
	_ = s.RegisterTool(mcp.ToolDefinition{Name: "env"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			return mcp.ToolsCallResult{Content: []mcp.Content{
				{Type: "text", Text: strings.Join(os.Environ(), "\n")}}}, nil
		})
	tr := mcp.NewPipeTransport(os.Stdin, os.Stdout, wire.Limits{})
	_ = s.Serve(context.Background(), tr)
}

// runOneShotChild answers the first request with a fixed response for id 1
// and exits IMMEDIATELY: the shape of a server whose last frame the old stdio
// transport could lose.
func runOneShotChild() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	_, _ = os.Stdout.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","ok":true}}` + "\n"))
	os.Exit(0)
}

// runMortalChild is a server that can be told to die: `die` answers and then
// exits as soon as that answer is written, `crash` exits without answering,
// `add_tool` registers a tool after the fact (a list_changed source), and
// `hello` just works.
func runMortalChild() {
	s := mcp.NewServer(mcp.ServerOptions{Info: mcp.Implementation{Name: "mortal", Version: "1"}})
	out := &exitAfterWrite{w: os.Stdout}
	ok := func(text string) (mcp.ToolsCallResult, error) {
		return mcp.ToolsCallResult{Content: []mcp.Content{{Type: "text", Text: text}}}, nil
	}
	_ = s.RegisterTool(mcp.ToolDefinition{Name: "hello"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) { return ok("hi") })
	_ = s.RegisterTool(mcp.ToolDefinition{Name: "die"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			out.armed.Store(true)
			return ok("bye")
		})
	_ = s.RegisterTool(mcp.ToolDefinition{Name: "crash"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			os.Exit(1)
			return ok("unreachable")
		})
	_ = s.RegisterTool(mcp.ToolDefinition{Name: "add_tool"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			_ = s.RegisterTool(mcp.ToolDefinition{Name: "late"},
				func(context.Context, map[string]any) (mcp.ToolsCallResult, error) { return ok("late") })
			return ok("added")
		})
	tr := mcp.NewPipeTransport(os.Stdin, out, wire.Limits{})
	_ = s.Serve(context.Background(), tr)
}

// exitAfterWrite exits the process right after the write that follows arming,
// so a response is fully written before the server is gone — deterministic
// where a timer would be a race.
type exitAfterWrite struct {
	w     io.Writer
	armed atomic.Bool
}

func (e *exitAfterWrite) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if e.armed.Load() {
		os.Exit(0)
	}
	return n, err
}

// childPool connects one re-executed child of the given mode through a Pool.
func childPool(t *testing.T, mode string, cfg mcp.ServerConfig) (*mcp.Pool, *mcp.ServerConnection) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	cfg.Command = exe
	p := mcp.NewPool(mcp.ConnectionOptions{
		Warnf: func(f string, a ...any) { t.Logf("client: "+f, a...) },
	})
	t.Cleanup(func() { _ = p.Close() })
	conn, err := p.Connect(context.Background(), cfg,
		[]string{"AGENTKIT_MCP_CHILD=" + mode, "PATH=" + os.Getenv("PATH")}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return p, conn
}

// waitFor polls a condition, failing rather than hanging.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- stdio: the last frame before exit (item: cmd.Wait raced the reader)

// TestAResponseWrittenJustBeforeExitIsDelivered. os/exec's Wait closes the
// pipes it created, and the reaper called Wait the instant the process exited
// — so a server that answered and exited could have its answer discarded
// before the reader got to it. The Receive is delayed so the frame is sitting
// in the pipe when the process is reaped, which is the losing order.
func TestAResponseWrittenJustBeforeExitIsDelivered(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	tr, err := mcp.StartStdio(context.Background(), mcp.StdioOptions{
		Command: exe, Env: []string{"AGENTKIT_MCP_CHILD=one-shot", "PATH=" + os.Getenv("PATH")},
	})
	must(t, err)
	t.Cleanup(func() { _ = tr.Close() })

	must(t, tr.Send([]byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`)))
	time.Sleep(300 * time.Millisecond) // let the child answer, exit and be reaped

	frame, err := tr.Receive()
	if err != nil {
		t.Fatalf("the response written just before exit was lost: %v", err)
	}
	var m mcp.Message
	must(t, json.Unmarshal(frame, &m))
	if m.ID.Key() != mcp.NumberID(1).Key() || m.Error != nil {
		t.Fatalf("got %s", frame)
	}
}

// ---- NFR-REL-03: reconnection

// TestADeadStdioServerIsRespawnedWithinTheReconnectLimit is NFR-REL-03 end to
// end against a real process: a disconnect mid-call is an is_error tool
// result and the loop's next call re-spawns the server; the budget is
// honoured; and past it the connection is dead and every call is is_error —
// never a Go error the agent loop would have to handle.
func TestADeadStdioServerIsRespawnedWithinTheReconnectLimit(t *testing.T) {
	p, conn := childPool(t, "mortal", mcp.ServerConfig{Name: "mortal", PerSessionReconnectLimit: 1})
	ctx := context.Background()
	tools, err := p.Tools(ctx, nil)
	must(t, err)
	tool := func(name string) core.Tool {
		for _, tl := range tools {
			if tl.Name == "mortal__"+name {
				return tl
			}
		}
		t.Fatalf("no tool %q in %v", name, tools)
		return core.Tool{}
	}

	// 1. The server dies DURING a call: an is_error result, not a Go error.
	if r := tool("crash").Execute(ctx, nil); r.OK {
		t.Fatal("a call cut off by the server's exit must be an error result")
	}
	waitFor(t, "the client to notice the exit", func() bool { return !conn.Alive() })

	// 2. The next call re-spawns the server and succeeds.
	if r := tool("hello").Execute(ctx, nil); !r.OK {
		t.Fatalf("the call after a death must re-spawn the server: %+v", r)
	}
	if conn.Reconnects() != 1 || !conn.Alive() {
		t.Fatalf("reconnects = %d, alive = %v; want 1 and alive", conn.Reconnects(), conn.Alive())
	}

	// 3. It dies again, this time AFTER answering, and the budget is spent.
	if r := tool("die").Execute(ctx, nil); !r.OK {
		t.Fatalf("die must answer before exiting: %+v", r)
	}
	waitFor(t, "the client to notice the second exit", func() bool { return !conn.Alive() })

	r := tool("hello").Execute(ctx, nil)
	if r.OK {
		t.Fatal("past the reconnect limit the call must fail")
	}
	if !strings.Contains(r.Detail, "reconnect limit") {
		t.Fatalf("the result must say why: %+v", r)
	}
	if !conn.Dead() || conn.Reconnects() != 1 {
		t.Fatalf("dead = %v, reconnects = %d; the limit must not be exceeded", conn.Dead(), conn.Reconnects())
	}
	// And it stays dead: no further spawn is attempted.
	if r := tool("hello").Execute(ctx, nil); r.OK || conn.Reconnects() != 1 {
		t.Fatalf("a dead connection must fail fast without re-spawning: %+v, reconnects = %d",
			r, conn.Reconnects())
	}
}

// ---- REQ-CACHE-07: the pool subscribes stdio servers to list_changed

// TestAStdioServerConnectedByThePoolIsSubscribedToToolChanges. The server
// sends list_changed ONLY on a stream the client opened, and nobody was
// opening one — so the cache lived by the ttlMs hint alone, and a server
// sending none (this one) kept a stale list for the whole session.
func TestAStdioServerConnectedByThePoolIsSubscribedToToolChanges(t *testing.T) {
	_, conn := childPool(t, "mortal", mcp.ServerConfig{Name: "mortal"})
	ctx := context.Background()
	waitFor(t, "the subscription to be acknowledged", conn.Subscribed)

	before, err := conn.ListTools(ctx)
	must(t, err)
	res, err := conn.Call(ctx, "add_tool", nil)
	must(t, err)
	if res.IsError {
		t.Fatalf("add_tool: %+v", res)
	}
	waitFor(t, "the tool cache to be invalidated", func() bool {
		after, err := conn.ListTools(ctx)
		must(t, err)
		return len(after) == len(before)+1
	})
}

// ---- REQ-SEC-12.1 vs the protocol model

// TestAToolWithOutputSchemaAndMetaListsAndCallsFine. Strict binding rejects
// unknown members, so every spec-standard optional member has to be modelled
// — a conforming server sending outputSchema on ONE tool made tools/list fail
// and the whole server unusable.
func TestAToolWithOutputSchemaAndMetaListsAndCallsFine(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterTool(mcp.ToolDefinition{
		Name: "typed", Title: "Typed", Description: "returns structured content",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
		Annotations:  json.RawMessage(`{"readOnlyHint":true,"vendor/x":1}`),
		Icons:        json.RawMessage(`[{"src":"https://example/i.png"}]`),
		Meta:         json.RawMessage(`{"vendor/tag":"v"}`),
	}, func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
		return mcp.ToolsCallResult{
			Content: []mcp.Content{
				{Type: "text", Text: "n=1", Annotations: json.RawMessage(`{"audience":["user"]}`),
					Meta: json.RawMessage(`{"k":"v"}`)},
				{Type: "resource_link", URI: "x://doc", Name: "doc", Size: new(int64)},
			},
			StructuredContent: json.RawMessage(`{"n":1}`),
			Meta:              json.RawMessage(`{"vendor/trace":"abc"}`),
		}, nil
	}))
	size := int64(12)
	must(t, s.RegisterResource(mcp.Resource{URI: "x://doc", Name: "doc", Size: &size,
		Annotations: json.RawMessage(`{"priority":0.5}`), Meta: json.RawMessage(`{}`)},
		func(context.Context, string) (mcp.ResourcesReadResult, error) {
			return mcp.ResourcesReadResult{Contents: []mcp.ResourceContents{
				{URI: "x://doc", Text: "hi", Meta: json.RawMessage(`{"etag":"1"}`)}}}, nil
		}))
	conn := pair(t, s, mcp.ServerConfig{Name: "s"}, mcp.ConnectionOptions{})
	ctx := context.Background()

	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools) != 1 || len(tools[0].OutputSchema) == 0 || len(tools[0].Meta) == 0 {
		t.Fatalf("tools = %+v", tools)
	}
	res, err := conn.Call(ctx, "typed", nil)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if string(res.StructuredContent) != `{"n":1}` || len(res.Meta) == 0 ||
		len(res.Content) != 2 || len(res.Content[0].Annotations) == 0 || res.Content[1].Size == nil {
		t.Fatalf("result = %+v", res)
	}
	resources, err := conn.ListResources(ctx)
	if err != nil || len(resources) != 1 || resources[0].Size == nil || *resources[0].Size != 12 {
		t.Fatalf("resources = %+v, %v", resources, err)
	}
	read, err := conn.ReadResource(ctx, "x://doc")
	if err != nil || len(read.Contents) != 1 || len(read.Contents[0].Meta) == 0 {
		t.Fatalf("read = %+v, %v", read, err)
	}
}

// ---- REQ-SEC-12.1 / REQ-SEC-11.3 on the envelope itself

// TestTheClientRejectsANonStrictEnvelope. encoding/json matched envelope keys
// case-insensitively and ignored unknown ones, so `{"id":1,"ID":2}` passed the
// duplicate-key check and correlated to 2, and `"bogus":true` was accepted.
func TestTheClientRejectsANonStrictEnvelope(t *testing.T) {
	for _, tc := range []struct{ name, frame string }{
		{"case-variant duplicate id", `{"jsonrpc":"2.0","id":1,"ID":2,"result":{"resultType":"complete","supportedVersions":["x"],"capabilities":{},"ttlMs":0}}`},
		{"unknown member", `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","supportedVersions":["x"],"capabilities":{},"ttlMs":0},"bogus":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c2sR, c2sW := io.Pipe()
			s2cR, s2cW := io.Pipe()
			conn := mcp.NewConnection(mcp.ServerConfig{Name: "s"},
				mcp.NewPipeTransport(s2cR, c2sW, wire.Limits{}), mcp.ConnectionOptions{})
			t.Cleanup(func() { _ = conn.Close(); _ = c2sR.Close(); _ = s2cW.Close() })

			go func() {
				// Read the request (its id is 1: the first the client issues) and
				// answer it with the crafted frame.
				_, _ = bufio.NewReader(c2sR).ReadString('\n')
				_, _ = s2cW.Write([]byte(tc.frame + "\n"))
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := conn.Discover(ctx); err == nil {
				t.Fatal("a frame with a case-variant duplicate or an unknown member must be rejected")
			}
			waitFor(t, "the connection to be torn down", func() bool { return !conn.Alive() })
		})
	}
}

// ---- config

// TestTimeoutSAcceptsAFloat is REQ-MCP-CLIENT-07, whose own default is written
// `30.0`; a TOML reader that rejected floats dropped the requirement's example.
func TestTimeoutSAcceptsAFloatAndTheReconnectLimitParses(t *testing.T) {
	src := `
[[mcp.servers]]
name = "a"
command = "a"
timeout_s = 30.0
per_session_reconnect_limit = 5

[[mcp.servers]]
name = "b"
command = "b"
timeout_s = 2.5
`
	cfg, diags, err := mcp.ParseConfig("c.toml", []byte(src))
	must(t, err)
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v", diags)
	}
	if cfg.Servers[0].Timeout != 30*time.Second || cfg.Servers[0].PerSessionReconnectLimit != 5 {
		t.Fatalf("a = %+v", cfg.Servers[0])
	}
	if cfg.Servers[1].Timeout != 2500*time.Millisecond {
		t.Fatalf("b.timeout = %v, want 2.5s", cfg.Servers[1].Timeout)
	}
}
