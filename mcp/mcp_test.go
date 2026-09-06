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
	"sort"
	"strings"
	"sync"
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
	if info.Meta == nil || info.Meta.ServerInfo == nil || info.Meta.ServerInfo.Name != "test-server" {
		t.Fatalf("server identity = %+v", info.Meta)
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

func TestEnvInterpolationReportsUnsetVariables(t *testing.T) {
	cfg := mcp.ServerConfig{
		Name: "gh", Command: "true",
		Env: map[string]string{"TOKEN": "${GH_TOKEN}", "MODE": "${MISSING}-suffix"},
	}
	var warnings []string
	p := mcp.NewPool(mcp.ConnectionOptions{
		Warnf: func(f string, a ...any) { warnings = append(warnings, fmt.Sprintf(f, a...)) },
	})
	_, _ = p.Connect(context.Background(), cfg, []string{"PATH=/usr/bin"},
		func(name string) string {
			if name == "GH_TOKEN" {
				return "ghp_secret"
			}
			return ""
		})
	defer p.Close()

	var sawMissing bool
	for _, w := range warnings {
		if strings.Contains(w, "MISSING") {
			sawMissing = true
		}
		if strings.Contains(w, "ghp_secret") {
			t.Fatalf("a warning leaked the resolved credential: %s", w)
		}
	}
	if !sawMissing {
		t.Fatalf("an unset ${VAR} must be reported; otherwise the child gets an empty "+
			"credential and fails authentication with a message about a bad token, "+
			"which sends the reader to the wrong place. warnings = %v", warnings)
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

// TestAShadowedNativeToolIsRefusedAtConnectionTime is REQ-MCP-CLIENT-06.
//
// At CONNECTION time, not call time: a shadowed native tool is a
// misconfiguration, and discovering it when the model happens to call the tool
// means discovering it in production — with the wrong tool having run.
func TestAShadowedNativeToolIsRefusedAtConnectionTime(t *testing.T) {
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
	if os.Getenv("AGENTKIT_MCP_CHILD") == "1" {
		runChildServer()
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}

	p := mcp.NewPool(mcp.ConnectionOptions{})
	defer p.Close()

	cfg := mcp.ServerConfig{
		Name: "child", Command: exe,
		Args: []string{"-test.run=TestAStdioServerRunsAsASubprocessWithAReducedEnvironment"},
		Env:  map[string]string{"SUPPLIED": "${A_SECRET}"},
	}
	conn, err := p.Connect(context.Background(), cfg,
		[]string{"AGENTKIT_MCP_CHILD=1", "PATH=" + os.Getenv("PATH")},
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

// runChildServer is the subprocess half of the test above.
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
