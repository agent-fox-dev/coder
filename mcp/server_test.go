package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentfox/agentkit-go/mcp"
	"github.com/agentfox/agentkit-go/wire"
)

// ---- REQ-MCP-SERVER-05: templated resources

// TestATemplatedResourceMatchesAConcreteURI is the requirement's own example.
// REQ-MCP-SERVER-05 names `nightshift://issues/{number}/triage-report`, which
// exact-URI registration cannot express at all: a host would have to register
// every issue it has ever seen.
func TestATemplatedResourceMatchesAConcreteURI(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "nightshift://issues/{number}/triage-report",
		Name:        "triage report",
	}, func(_ context.Context, uri string, vars map[string]string) (mcp.ResourcesReadResult, error) {
		return mcp.ResourcesReadResult{Contents: []mcp.ResourceContents{
			{URI: uri, Text: "issue " + vars["number"]},
		}}, nil
	}))

	conn := connected(t, s)
	res, err := conn.ReadResource(context.Background(), "nightshift://issues/4271/triage-report")
	must(t, err)
	if len(res.Contents) != 1 || res.Contents[0].Text != "issue 4271" {
		t.Fatalf("the template variable must reach the handler decoded; got %+v", res.Contents)
	}
}

// TestATemplateVariableDoesNotSpanASlash guards the matcher's central choice.
//
// If `{number}` swallowed slashes, `nightshift://issues/1/2/triage-report`
// would match with number="1/2" — a handler would then look up an issue id
// that cannot exist and fail somewhere far from the mistake.
func TestATemplateVariableDoesNotSpanASlash(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "x://issues/{number}/report", Name: "r",
	}, func(context.Context, string, map[string]string) (mcp.ResourcesReadResult, error) {
		return mcp.ResourcesReadResult{}, nil
	}))
	// And a greedy one, which may.
	must(t, s.RegisterResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "y://files/{+path}", Name: "f",
	}, func(_ context.Context, _ string, vars map[string]string) (mcp.ResourcesReadResult, error) {
		return mcp.ResourcesReadResult{Contents: []mcp.ResourceContents{{Text: vars["path"]}}}, nil
	}))

	conn := connected(t, s)
	if _, err := conn.ReadResource(context.Background(), "x://issues/1/2/report"); err == nil {
		t.Fatal("a plain {var} must not span a path separator; matching here would " +
			"hand the handler an id like \"1/2\"")
	}
	res, err := conn.ReadResource(context.Background(), "y://files/a/b/c.txt")
	must(t, err)
	if len(res.Contents) != 1 || res.Contents[0].Text != "a/b/c.txt" {
		t.Fatalf("{+var} is RFC 6570 reserved expansion and must span slashes; got %+v", res.Contents)
	}
}

// TestAnEmptyTemplateVariableIsNotAMatch. `x://issues//report` is not an issue
// whose number is the empty string.
func TestAnEmptyTemplateVariableIsNotAMatch(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "x://issues/{number}/report", Name: "r",
	}, func(context.Context, string, map[string]string) (mcp.ResourcesReadResult, error) {
		return mcp.ResourcesReadResult{}, nil
	}))
	conn := connected(t, s)
	if _, err := conn.ReadResource(context.Background(), "x://issues//report"); err == nil {
		t.Fatal("an empty variable must not match")
	}
}

// TestAnExactResourceWinsOverAMatchingTemplate. Otherwise registration order
// decides, and a specific registration becomes silently unreachable.
func TestAnExactResourceWinsOverAMatchingTemplate(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "x://docs/{name}", Name: "any doc",
	}, func(context.Context, string, map[string]string) (mcp.ResourcesReadResult, error) {
		return mcp.ResourcesReadResult{Contents: []mcp.ResourceContents{{Text: "from template"}}}, nil
	}))
	must(t, s.RegisterResource(mcp.Resource{URI: "x://docs/readme", Name: "readme"},
		func(context.Context, string) (mcp.ResourcesReadResult, error) {
			return mcp.ResourcesReadResult{Contents: []mcp.ResourceContents{{Text: "from exact"}}}, nil
		}))

	conn := connected(t, s)
	res, err := conn.ReadResource(context.Background(), "x://docs/readme")
	must(t, err)
	if res.Contents[0].Text != "from exact" {
		t.Fatalf("the exact registration must win, whatever the order; got %q", res.Contents[0].Text)
	}
}

// TestTemplatesAreListedSeparatelyFromResources. A template is not a readable
// URI, so listing it under resources/list would advertise something no
// resources/read can fetch.
func TestTemplatesAreListedSeparatelyFromResources(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterResource(mcp.Resource{URI: "x://config", Name: "config"},
		func(context.Context, string) (mcp.ResourcesReadResult, error) {
			return mcp.ResourcesReadResult{}, nil
		}))
	must(t, s.RegisterResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "x://issues/{n}", Name: "issue",
	}, func(context.Context, string, map[string]string) (mcp.ResourcesReadResult, error) {
		return mcp.ResourcesReadResult{}, nil
	}))

	conn := connected(t, s)
	list, err := conn.ListResources(context.Background())
	must(t, err)
	if len(list) != 1 || list[0].URI != "x://config" {
		t.Fatalf("resources/list must carry only concrete URIs; got %+v", list)
	}
	tmpls, err := conn.ListResourceTemplates(context.Background())
	must(t, err)
	if len(tmpls) != 1 || tmpls[0].URITemplate != "x://issues/{n}" {
		t.Fatalf("resources/templates/list must carry the templates; got %+v", tmpls)
	}
}

// TestAMalformedTemplateIsRejectedAtRegistration. A host sees the mistake at
// startup rather than as a read that never matches.
func TestAMalformedTemplateIsRejectedAtRegistration(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{})
	noop := func(context.Context, string, map[string]string) (mcp.ResourcesReadResult, error) {
		return mcp.ResourcesReadResult{}, nil
	}
	for _, bad := range []string{"x://a/{unterminated", "x://a/{}", "x://a/b"} {
		if err := s.RegisterResourceTemplate(mcp.ResourceTemplate{URITemplate: bad, Name: "n"}, noop); err == nil {
			t.Fatalf("template %q must be refused at registration", bad)
		}
	}
}

// ---- REQ-MCP-SERVER-06: per-request version, server/discover

// TestARequestWithoutProtocolVersionMetaIsRefused.
//
// 2026-07-28 has no handshake in which a version could have been agreed, so a
// request that omits it is not interpretable. Guessing "probably the current
// one" is how a client speaking something else gets served silently wrong
// semantics — the failure the handshake used to prevent, moved to every
// request.
func TestARequestWithoutProtocolVersionMetaIsRefused(t *testing.T) {
	raw := rawPeer(t, echoServer(t))

	raw.write(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	m := raw.read(t)
	if m.Error == nil {
		t.Fatal("a request with no _meta must be refused")
	}
	if m.Error.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Fatalf("code = %d, want %d", m.Error.Code, mcp.CodeUnsupportedProtocolVersion)
	}
	if !strings.Contains(m.Error.Message, mcp.MetaProtocolVersion) {
		t.Fatalf("the refusal must name the missing field; got %q", m.Error.Message)
	}

	// And the same request WITH _meta is answered.
	raw.write(t, req("2", mcp.MethodToolsList))
	if m := raw.read(t); m.Error != nil {
		t.Fatalf("a request carrying _meta must be answered; got %v", m.Error)
	}
}

// TestAnUnsupportedVersionIsRejectedWithTheSupportedList is REQ-MCP-SERVER-06.2.
//
// The `supported` list is the only negotiation the protocol still has: without
// it a client that guessed wrong has nothing to retry with.
func TestAnUnsupportedVersionIsRejectedWithTheSupportedList(t *testing.T) {
	raw := rawPeer(t, echoServer(t))
	raw.write(t, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{%q:"1999-01-01",%q:{}}}}`,
		mcp.MetaProtocolVersion, mcp.MetaClientCapabilities))

	m := raw.read(t)
	if m.Error == nil || m.Error.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Fatalf("want %d, got %+v", mcp.CodeUnsupportedProtocolVersion, m.Error)
	}
	var data mcp.UnsupportedVersionData
	must(t, json.Unmarshal(m.Error.Data, &data))
	if len(data.Supported) == 0 || data.Supported[0] != mcp.ProtocolVersion {
		t.Fatalf("supported = %v, want it to name %s", data.Supported, mcp.ProtocolVersion)
	}
	if data.Requested != "1999-01-01" {
		t.Fatalf("requested = %q, want the version the client actually asked for", data.Requested)
	}
}

// TestServerDiscoverAdvertisesVersionsAndCapabilities is REQ-MCP-SERVER-06.1:
// servers MUST implement it.
func TestServerDiscoverAdvertisesVersionsAndCapabilities(t *testing.T) {
	raw := rawPeer(t, echoServer(t))
	raw.write(t, req("1", mcp.MethodDiscover))

	m := raw.read(t)
	if m.Error != nil {
		t.Fatalf("server/discover: %v", m.Error)
	}
	var res mcp.DiscoverResult
	must(t, json.Unmarshal(m.Result, &res))
	if len(res.SupportedVersions) == 0 || res.SupportedVersions[0] != mcp.ProtocolVersion {
		t.Fatalf("supportedVersions = %v", res.SupportedVersions)
	}
	if res.ResultType != mcp.ResultComplete {
		t.Fatalf("resultType = %q, want %q", res.ResultType, mcp.ResultComplete)
	}
	if res.Capabilities.Tools == nil {
		t.Fatal("a server with tools must advertise the tools capability")
	}
	if res.Meta == nil || res.Meta.ServerInfo == nil {
		t.Fatal("a server SHOULD identify itself in each result's _meta")
	}
}

// TestALegacyInitializeGetsADiagnosticNamingTheSupportedVersions.
//
// A legacy client has no fall-forward mechanism: it cannot retry with a newer
// version because it does not implement one. This error message is the only
// thing its user will ever see, so answering "method not found" would be
// technically correct and useless. Dropping legacy support is our decision
// (PRD 0.4.0), which is exactly why the resulting failure must not be mute.
func TestALegacyInitializeGetsADiagnosticNamingTheSupportedVersions(t *testing.T) {
	raw := rawPeer(t, echoServer(t))
	raw.write(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"old","version":"1"}}}`)

	m := raw.read(t)
	if m.Error == nil {
		t.Fatal("initialize must be refused: this revision removed the handshake")
	}
	if m.Error.Code == mcp.CodeMethodNotFound {
		t.Fatal("a bare method-not-found tells a legacy client's user nothing")
	}
	if !strings.Contains(m.Error.Message, mcp.ProtocolVersion) {
		t.Fatalf("the refusal must name the version we DO speak; got %q", m.Error.Message)
	}
	var data mcp.UnsupportedVersionData
	must(t, json.Unmarshal(m.Error.Data, &data))
	if len(data.Supported) == 0 {
		t.Fatal("the error must carry the supported list")
	}
}

// TestPingIsGone. 2026-07-28 removed it; answering it would be implementing a
// method the revision does not define.
func TestPingIsGone(t *testing.T) {
	raw := rawPeer(t, echoServer(t))
	raw.write(t, req("1", "ping"))
	m := raw.read(t)
	if m.Error == nil || m.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("ping must be method-not-found; got %+v", m.Error)
	}
}

// ---- REQ-MCP-SERVER-03: list_changed is advertised, so it must be sent

// TestRegisteringAToolNotifiesConnectedClients.
//
// The handshake advertises `tools.listChanged: true`. A capability advertised
// but never honoured is worse than one never claimed: a client that trusts it
// caches its tool list forever.
func TestRegisteringAToolNotifiesConnectedClients(t *testing.T) {
	s := echoServer(t)
	conn := connected(t, s)
	subscribe(t, conn)

	before, err := conn.ListTools(context.Background())
	must(t, err)

	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "late", Description: "added after connect"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			return mcp.ToolsCallResult{}, nil
		}))

	// The notification invalidates the cache; the next list re-fetches.
	deadline := time.Now().Add(2 * time.Second)
	for {
		after, err := conn.ListTools(context.Background())
		must(t, err)
		if len(after) == len(before)+1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the client's cached tool list was never invalidated: %d tools, want %d",
				len(after), len(before)+1)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUnregisteringAToolNotifiesConnectedClients is the other half: a client
// that keeps offering a withdrawn tool produces calls that cannot succeed.
func TestUnregisteringAToolNotifiesConnectedClients(t *testing.T) {
	s := echoServer(t)
	conn := connected(t, s)
	subscribe(t, conn)
	before, err := conn.ListTools(context.Background())
	must(t, err)

	s.UnregisterTool("boom")

	deadline := time.Now().Add(2 * time.Second)
	for {
		after, err := conn.ListTools(context.Background())
		must(t, err)
		if len(after) == len(before)-1 {
			for _, tool := range after {
				if tool.Name == "boom" {
					t.Fatal("the withdrawn tool is still listed")
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unregistration was never announced: %d tools, want %d",
				len(after), len(before)-1)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNoNotificationBeforeTheHandshakeCompletes. The spec forbids traffic
// other than ping before the handshake, and a list_changed arriving mid-
// handshake announces a list the client has not been told exists.
func TestNoNotificationBeforeTheHandshakeCompletes(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{})
	raw := rawPeer(t, s)

	// Round-trip a ping FIRST. Serve registers the session before it starts
	// reading, so an answered ping proves the session is registered — without
	// it this test races the goroutine and would pass whether or not the
	// handshake is checked.
	raw.write(t, req("1", mcp.MethodToolsList))
	if m := raw.read(t); m.Error != nil {
		t.Fatalf("ping: %v", m.Error)
	}

	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "a"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			return mcp.ToolsCallResult{}, nil
		}))

	// Nothing may have been written. Prove it by asking a question and
	// checking the FIRST thing back is that question's answer.
	raw.write(t, req("2", mcp.MethodToolsList))
	m := raw.read(t)
	if m.Method != "" {
		t.Fatalf("a notification reached an un-initialized session: %s", m.Method)
	}
	if m.ID.Key() != mcp.NumberID(2).Key() {
		t.Fatalf("expected the second ping response, got a reply for %s", m.ID)
	}
}

// ---- cancellation

// TestACancelledRequestStopsTheHandlerAndGoesUnanswered.
//
// Two invariants in one flow, because they are one guarantee: the handler's
// context ends, and no response is sent. Answering a request the client has
// stopped waiting for makes the reply look like an answer to whatever it asked
// next.
func TestACancelledRequestStopsTheHandlerAndGoesUnanswered(t *testing.T) {
	entered := make(chan struct{})
	stopped := make(chan struct{})
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "slow"},
		func(ctx context.Context, _ map[string]any) (mcp.ToolsCallResult, error) {
			close(entered)
			<-ctx.Done()
			close(stopped)
			return mcp.ToolsCallResult{Content: []mcp.Content{{Type: "text", Text: "too late"}}}, nil
		}))

	raw := rawPeer(t, s)
	raw.handshake(t)
	raw.write(t, req("7", mcp.MethodToolsCall, `"name":"slow"`, `"arguments":{}`))

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler never ran")
	}

	raw.write(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"user stopped"}}`)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("notifications/cancelled must cancel the handler's context")
	}

	// Now ask something else. If a response for id 7 were sent, it would
	// arrive before this one's.
	raw.write(t, req("8", mcp.MethodToolsList))
	m := raw.read(t)
	if m.ID.Key() != mcp.NumberID(8).Key() {
		t.Fatalf("a cancelled request must go unanswered; got a reply for %s first", m.ID)
	}
}

// TestACancellationForAnUnknownIDIsIgnored. A cancellation racing a response
// that already went out is normal, and tearing the connection down over it
// would make a benign race fatal.
func TestACancellationForAnUnknownIDIsIgnored(t *testing.T) {
	raw := rawPeer(t, echoServer(t))
	raw.handshake(t)
	raw.write(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":999}}`)
	raw.write(t, req("3", mcp.MethodToolsList))
	if m := raw.read(t); m.Error != nil {
		t.Fatalf("an unknown cancellation must be ignored, not fatal: %v", m.Error)
	}
}

// TestAStringAndANumericRequestIDDoNotCollideWhenCancelling.
//
// The correlation key encodes the id's TYPE. If it did not, cancelling the
// string "5" would cancel the numeric 5 — a peer's choice of id type would let
// it stop somebody else's call.
func TestAStringAndANumericRequestIDDoNotCollideWhenCancelling(t *testing.T) {
	var mu sync.Mutex
	cancelled := map[string]bool{}
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "slow"},
		func(ctx context.Context, args map[string]any) (mcp.ToolsCallResult, error) {
			who, _ := args["who"].(string)
			select {
			case <-ctx.Done():
				mu.Lock()
				cancelled[who] = true
				mu.Unlock()
			case <-time.After(2 * time.Second):
			}
			return mcp.ToolsCallResult{}, nil
		}))

	raw := rawPeer(t, s)
	raw.handshake(t)
	raw.write(t, req("5", mcp.MethodToolsCall, `"name":"slow"`, `"arguments":{"who":"number"}`))
	raw.write(t, req(`"5"`, mcp.MethodToolsCall, `"name":"slow"`, `"arguments":{"who":"string"}`))
	raw.write(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"5"}}`)

	// The string call is cancelled and goes unanswered; the numeric one runs
	// to completion and replies.
	m := raw.read(t)
	if m.ID.Key() != mcp.NumberID(5).Key() {
		t.Fatalf("cancelling the string id %q must not cancel the numeric 5; got a reply for %s",
			"5", m.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if !cancelled["string"] {
		t.Fatal("the string-id call should have been cancelled")
	}
	if cancelled["number"] {
		t.Fatal("the numeric-id call must not have been cancelled")
	}
}

// TestShutdownCancelsInFlightHandlers. Without it a handler blocked on a slow
// dependency holds the shutdown open for as long as it likes, writing to a
// transport that is already gone.
func TestShutdownCancelsInFlightHandlers(t *testing.T) {
	entered := make(chan struct{})
	stopped := make(chan struct{})
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "park"},
		func(ctx context.Context, _ map[string]any) (mcp.ToolsCallResult, error) {
			close(entered)
			<-ctx.Done()
			close(stopped)
			return mcp.ToolsCallResult{}, nil
		}))

	raw := rawPeer(t, s)
	raw.handshake(t)
	raw.write(t, req("1", mcp.MethodToolsCall, `"name":"park"`, `"arguments":{}`))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler never ran")
	}

	raw.shutdown()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("tearing the transport down must cancel in-flight handlers")
	}
}

// ---- pagination

// TestListingPagesAndResumesAfterTheCursor.
func TestListingPagesAndResumesAfterTheCursor(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{PageSize: 2})
	for i := range 5 {
		must(t, s.RegisterTool(mcp.ToolDefinition{Name: fmt.Sprintf("t%d", i)},
			func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
				return mcp.ToolsCallResult{}, nil
			}))
	}
	conn := connected(t, s)
	tools, err := conn.ListTools(context.Background())
	must(t, err)
	if len(tools) != 5 {
		t.Fatalf("paging must yield every tool exactly once; got %d: %+v", len(tools), tools)
	}
	seen := map[string]int{}
	for _, tool := range tools {
		seen[tool.Name]++
	}
	for i := range 5 {
		if n := seen[fmt.Sprintf("t%d", i)]; n != 1 {
			t.Fatalf("t%d appeared %d times; a cursor that resumes AT rather than "+
				"AFTER its entry duplicates a tool per page", i, n)
		}
	}
}

// TestTheLastPageCarriesNoCursor. A cursor on the final page makes a client
// fetch an empty page it did not need, and a client that trusts a non-empty
// cursor to mean "more" loops.
func TestTheLastPageCarriesNoCursor(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{PageSize: 2})
	for _, n := range []string{"a", "b", "c", "d"} {
		must(t, s.RegisterTool(mcp.ToolDefinition{Name: n},
			func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
				return mcp.ToolsCallResult{}, nil
			}))
	}
	raw := rawPeer(t, s)
	raw.handshake(t)

	raw.write(t, req("1", mcp.MethodToolsList))
	var first mcp.ToolsListResult
	must(t, json.Unmarshal(raw.read(t).Result, &first))
	if first.NextCursor == "" {
		t.Fatal("with 4 tools and a page size of 2 the first page must carry a cursor")
	}

	raw.write(t, req("2", mcp.MethodToolsList, fmt.Sprintf(`"cursor":%q`, first.NextCursor)))
	var second mcp.ToolsListResult
	must(t, json.Unmarshal(raw.read(t).Result, &second))
	if second.NextCursor != "" {
		t.Fatalf("the page that exhausts the list must carry no cursor; got %q", second.NextCursor)
	}
	if len(second.Tools) != 2 {
		t.Fatalf("want the remaining 2 tools, got %d", len(second.Tools))
	}
}

// TestAFabricatedCursorIsRefused. Cursors are opaque by spec; a client that
// guessed at the format gets a clear error instead of a page computed from a
// name it invented.
func TestAFabricatedCursorIsRefused(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{PageSize: 1})
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "a"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			return mcp.ToolsCallResult{}, nil
		}))
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "b"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			return mcp.ToolsCallResult{}, nil
		}))
	raw := rawPeer(t, s)
	raw.handshake(t)
	raw.write(t, req("1", mcp.MethodToolsList, `"cursor":"a"`))
	if m := raw.read(t); m.Error == nil {
		t.Fatal("a cursor this server did not issue must be refused")
	}
	raw.write(t, req("2", mcp.MethodToolsList, `"cursor":"n:gone"`))
	if m := raw.read(t); m.Error == nil {
		t.Fatal("a cursor naming an entry that no longer exists must be refused, " +
			"not silently restarted from the top")
	}
}

// TestPageSizeZeroReturnsEverythingInOnePage. Pagination costs a round trip,
// and a server with nine tools should not make its client pay for one.
func TestPageSizeZeroReturnsEverythingInOnePage(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{})
	for i := range 9 {
		must(t, s.RegisterTool(mcp.ToolDefinition{Name: fmt.Sprintf("t%d", i)},
			func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
				return mcp.ToolsCallResult{}, nil
			}))
	}
	raw := rawPeer(t, s)
	raw.handshake(t)
	raw.write(t, req("1", mcp.MethodToolsList))
	var res mcp.ToolsListResult
	must(t, json.Unmarshal(raw.read(t).Result, &res))
	if len(res.Tools) != 9 || res.NextCursor != "" {
		t.Fatalf("want 9 tools and no cursor; got %d and %q", len(res.Tools), res.NextCursor)
	}
}

// ---- REQ-MCP-SERVER-01/02: the runner

// TestRunIsANoOpWhenServerModeIsDisabled. REQ-MCP-SERVER-01 says off by
// default, so "not enabled" is a successful no-op and not an error every host
// has to special-case.
func TestRunIsANoOpWhenServerModeIsDisabled(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- mcp.NewServer(mcp.ServerOptions{}).Run(context.Background(),
			mcp.ServerModeConfig{Enabled: false, Transport: "http", Port: 1}, nil)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a disabled config must return nil without listening; got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a disabled config must not start a listener")
	}
}

// TestHTTPModeRefusesToStartWithoutAKey is REQ-MCP-SERVER-07 at the runner.
//
// The dangerous case is api_key_env NAMED but unset: the config looks
// authenticated, so nobody re-reads it. Refusing to start is the only outcome
// that cannot be mistaken for a working deployment.
func TestHTTPModeRefusesToStartWithoutAKey(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{})
	// An already-cancelled context, so that a build which WRONGLY got past the
	// guard shuts its listener down and fails the assertion instead of parking
	// the test on an open port. The guard fires before anything listens, so a
	// correct build reaches the same error either way.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	port := freePort(t)
	for _, tc := range []struct {
		name string
		cfg  mcp.ServerModeConfig
		env  func(string) string
		want string
	}{
		{"no api_key_env at all",
			mcp.ServerModeConfig{Enabled: true, Transport: "http", Port: port}, nil, "api_key_env"},
		{"api_key_env names an unset variable",
			mcp.ServerModeConfig{Enabled: true, Transport: "http", Port: port, APIKeyEnv: "MCP_KEY"},
			func(string) string { return "" }, "MCP_KEY"},
	} {
		err := s.Run(ctx, tc.cfg, tc.env)
		if !errors.Is(err, mcp.ErrNoAPIKey) {
			t.Fatalf("%s: want ErrNoAPIKey, got %v", tc.name, err)
		}
		// The refusal must name what to fix. HTTPHandler also refuses an empty
		// key, so without this the test would pass on that guard alone and say
		// nothing about the runner — and an operator would get "requires an
		// API key" with no clue which variable was empty.
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: the refusal must name %q; got %q", tc.name, tc.want, err)
		}
	}
}

// TestRunRejectsAnUnknownTransport. Falling back to stdio would leave a host
// that meant to listen on a port silently speaking to a closed stdin.
func TestRunRejectsAnUnknownTransport(t *testing.T) {
	err := mcp.NewServer(mcp.ServerOptions{}).Run(context.Background(),
		mcp.ServerModeConfig{Enabled: true, Transport: "websocket"}, nil)
	if err == nil || !strings.Contains(err.Error(), "websocket") {
		t.Fatalf("an unknown transport must be refused by name; got %v", err)
	}
}

// TestHTTPModeBindsLoopback. A default that binds every interface turns a
// config file's `port = 8080` into an internet-facing service.
func TestHTTPModeBindsLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := freePort(t)
	s := echoServer(t)
	done := make(chan error, 1)
	go func() {
		done <- s.Run(ctx, mcp.ServerModeConfig{
			Enabled: true, Transport: "http", Port: port, APIKeyEnv: "MCP_KEY",
		}, func(string) string { return "sekrit" })
	}()

	waitForPort(t, fmt.Sprintf("127.0.0.1:%d", port))
	if dialable(fmt.Sprintf("%s:%d", nonLoopbackHost(t), port)) {
		t.Fatal("http mode must bind loopback only")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a shutdown we asked for is not a failure; got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the context must stop the listener")
	}
}

// ---- helpers

// subscribe opens a tool-change subscription and waits for it to be live.
//
// 2026-07-28 made notifications OPT-IN: a client that has not sent
// subscriptions/listen receives nothing, which is the change these two tests
// have to account for. Under the old model, opening a connection was enough.
func subscribe(t *testing.T, conn *mcp.ServerConnection) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = conn.SubscribeToolChanges(ctx) }()

	// Wait for the acknowledgement rather than sleeping: the subscription is
	// live only once the server has recorded the filter, and registering a
	// tool before then is a race the test would lose intermittently.
	deadline := time.Now().Add(2 * time.Second)
	for !conn.Subscribed() {
		if time.Now().After(deadline) {
			t.Fatal("the subscription was never acknowledged")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// connected returns a client for srv.
func connected(t *testing.T, srv *mcp.Server) *mcp.ServerConnection {
	t.Helper()
	conn := pair(t, srv, mcp.ServerConfig{Name: "s"}, mcp.ConnectionOptions{})
	must(t, conn.Discover(context.Background()))
	return conn
}

// rawPeer talks to a server with hand-written frames, so a test can send
// things our own client would never send.
type peer struct {
	w    *io.PipeWriter
	r    *io.PipeReader
	dec  *json.Decoder
	stop func()
}

func rawPeer(t *testing.T, srv *mcp.Server) *peer {
	t.Helper()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	serverSide := mcp.NewPipeTransport(c2sR, s2cW, wire.Limits{})

	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(context.Background(), serverSide) }()

	p := &peer{w: c2sW, r: s2cR, dec: json.NewDecoder(s2cR)}
	p.stop = func() {
		_ = c2sW.Close()
		_ = serverSide.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("the server loop did not stop")
		}
	}
	t.Cleanup(p.stop)
	return p
}

func (p *peer) write(t *testing.T, frame string) {
	t.Helper()
	if _, err := p.w.Write([]byte(frame + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// read returns the next frame, failing rather than hanging.
func (p *peer) read(t *testing.T) mcp.Message {
	t.Helper()
	type result struct {
		m   mcp.Message
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var m mcp.Message
		err := p.dec.Decode(&m)
		ch <- result{m, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read: %v", r.err)
		}
		return r.m
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a frame")
	}
	return mcp.Message{}
}

// handshake is a NO-OP at 2026-07-28 and kept as a named call so a reader of
// these tests sees where a handshake used to be. There is nothing to send: the
// version and capabilities travel in every request's _meta instead.
func (p *peer) handshake(t *testing.T) { t.Helper() }

// meta is the per-request _meta every request must carry. Tests build frames
// by hand, so they carry it by hand.
func meta() string {
	return fmt.Sprintf(`"_meta":{%q:%q,%q:{}}`,
		mcp.MetaProtocolVersion, mcp.ProtocolVersion, mcp.MetaClientCapabilities)
}

// req builds a raw request frame with _meta merged into params.
func req(id, method string, paramFields ...string) string {
	fields := append([]string{meta()}, paramFields...)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":{%s}}`,
		id, method, strings.Join(fields, ","))
}

func (p *peer) shutdown() { p.stop() }

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	must(t, l.Close())
	return port
}

func waitForPort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if dialable(addr) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("nothing is listening on %s", addr)
}

func dialable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// nonLoopbackHost is this machine's first routable address, or a skip. The
// loopback assertion is only meaningful where there is another interface to
// have been wrongly bound.
func nonLoopbackHost(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	must(t, err)
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.To4() == nil {
			continue
		}
		return n.IP.String()
	}
	t.Skip("no non-loopback IPv4 interface; nothing to bind wrongly")
	return ""
}

// ---- MRTR (REQ-MCP-CLIENT-08, amended in PRD 0.4.0)

// TestAnUnboundedInputLoopIsRefused.
//
// The old server-initiated model made this impossible by construction: the
// server asked, the client answered, the call continued. MRTR turns it into
// "the client retries the whole request", so a server that returns
// input_required every time holds the client in a loop forever. The bound is
// what turns a hang into a reported error.
func TestAnUnboundedInputLoopIsRefused(t *testing.T) {
	var rounds atomic.Int32
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "greedy"},
		func(ctx context.Context, _ map[string]any) (mcp.ToolsCallResult, error) {
			rounds.Add(1)
			// Never satisfied: asks again on every retry, answers included.
			return mcp.ToolsCallResult{}, mcp.NeedSampling("s", "k", mcp.SamplingParams{
				Messages:  []mcp.SamplingMessage{{Role: "user", Content: mcp.Content{Type: "text", Text: "again"}}},
				MaxTokens: 8,
			})
		}))

	conn := pair(t, s, mcp.ServerConfig{Name: "s", AllowSampling: true}, mcp.ConnectionOptions{
		Sampling: func(context.Context, mcp.SamplingParams) (mcp.SamplingResult, error) {
			return mcp.SamplingResult{Role: "assistant", Model: "t",
				Content: mcp.Content{Type: "text", Text: "ok"}}, nil
		},
	})

	_, err := conn.Call(context.Background(), "greedy", nil)
	if !errors.Is(err, mcp.ErrTooManyInputRounds) {
		t.Fatalf("want ErrTooManyInputRounds, got %v", err)
	}
	if n := rounds.Load(); int(n) > mcp.MaxInputRounds {
		t.Fatalf("the handler ran %d times; the bound is %d", n, mcp.MaxInputRounds)
	}
}

// TestAnMRTRRetryCarriesTheAnswersAndTheOpaqueState. The server keeps NOTHING
// between the two calls — there is no session — so requestState is the only
// way it can remember where it was, and it must come back unchanged.
func TestAnMRTRRetryCarriesTheAnswersAndTheOpaqueState(t *testing.T) {
	var gotState string
	s := mcp.NewServer(mcp.ServerOptions{})
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "ask"},
		func(ctx context.Context, _ map[string]any) (mcp.ToolsCallResult, error) {
			in, retry := mcp.InputFrom(ctx)
			if !retry {
				return mcp.ToolsCallResult{}, mcp.NeedSampling("state-42", "k",
					mcp.SamplingParams{MaxTokens: 8})
			}
			gotState = in.RequestState
			var res mcp.SamplingResult
			if err := json.Unmarshal(in.Responses["k"], &res); err != nil {
				return mcp.ToolsCallResult{}, err
			}
			return mcp.ToolsCallResult{
				Content: []mcp.Content{{Type: "text", Text: "got:" + res.Content.Text}}}, nil
		}))

	conn := pair(t, s, mcp.ServerConfig{Name: "s", AllowSampling: true}, mcp.ConnectionOptions{
		Sampling: func(context.Context, mcp.SamplingParams) (mcp.SamplingResult, error) {
			return mcp.SamplingResult{Role: "assistant", Model: "t",
				Content: mcp.Content{Type: "text", Text: "answer"}}, nil
		},
	})

	res, err := conn.Call(context.Background(), "ask", nil)
	must(t, err)
	if len(res.Content) != 1 || res.Content[0].Text != "got:answer" {
		t.Fatalf("the retry did not carry the answer: %+v", res.Content)
	}
	if gotState != "state-42" {
		t.Fatalf("requestState = %q, want it returned verbatim", gotState)
	}
}

// TestAnExpiredToolCacheIsRefetched is the ttlMs half of REQ-MCP-SERVER-06.4.
//
// A client that never opened a subscription has no other way to learn its
// cached list went stale, so the freshness hint is the only thing standing
// between it and a permanently wrong tool list.
func TestAnExpiredToolCacheIsRefetched(t *testing.T) {
	s := mcp.NewServer(mcp.ServerOptions{ListTTLMs: 50})
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "one"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			return mcp.ToolsCallResult{}, nil
		}))

	now := time.Now()
	conn := pair(t, s, mcp.ServerConfig{Name: "s"}, mcp.ConnectionOptions{
		Now: func() time.Time { return now },
	})
	ctx := context.Background()

	first, err := conn.ListTools(ctx)
	must(t, err)
	if len(first) != 1 {
		t.Fatalf("want 1 tool, got %d", len(first))
	}

	// A tool appears, and this client is NOT subscribed — so nothing tells it.
	must(t, s.RegisterTool(mcp.ToolDefinition{Name: "two"},
		func(context.Context, map[string]any) (mcp.ToolsCallResult, error) {
			return mcp.ToolsCallResult{}, nil
		}))

	cached, err := conn.ListTools(ctx)
	must(t, err)
	if len(cached) != 1 {
		t.Fatalf("inside the ttl the cached list must be served; got %d", len(cached))
	}

	// Past the hint, the client re-fetches.
	now = now.Add(100 * time.Millisecond)
	fresh, err := conn.ListTools(ctx)
	must(t, err)
	if len(fresh) != 2 {
		t.Fatalf("past ttlMs the list must be re-fetched; got %d tools", len(fresh))
	}
}

// TestCacheScopeDefaultsToPrivate.
//
// A tool list can encode which tools THIS caller is allowed to see — a
// per-tenant inventory, a per-key filter. `cacheScope: public` tells shared
// intermediaries they may serve that response to anybody, so the default has
// to be the one that cannot leak.
func TestCacheScopeDefaultsToPrivate(t *testing.T) {
	raw := rawPeer(t, echoServer(t))
	raw.write(t, req("1", mcp.MethodToolsList))

	m := raw.read(t)
	if m.Error != nil {
		t.Fatal(m.Error)
	}
	var res mcp.ToolsListResult
	must(t, json.Unmarshal(m.Result, &res))
	if res.CacheScope != mcp.CachePrivate {
		t.Fatalf("cacheScope = %q, want %q: a list that may be caller-specific must not "+
			"be advertised as publicly cacheable", res.CacheScope, mcp.CachePrivate)
	}
	if res.ResultType != mcp.ResultComplete {
		t.Fatalf("resultType = %q", res.ResultType)
	}
}
