package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/wire"
)

// ResultCharCap is REQ-MCP-CLIENT-09: 50K characters per tool result.
//
// It is counted in RUNES, not bytes. "Characters" in a requirement about model
// context means what the model sees, and a byte cap truncates a CJK result to
// a third of the length an ASCII one gets — silently, and worst for the
// languages that need the room most.
const ResultCharCap = 50_000

// DefaultCallLimit and DefaultTimeout are REQ-MCP-CLIENT-07's defaults.
const (
	DefaultCallLimit = 1000
	DefaultTimeout   = 30 * time.Second
)

// Errors a connection can raise.
var (
	// ErrNameCollision is REQ-MCP-CLIENT-06's MCPNameCollisionError, raised at
	// CONNECTION time rather than call time: a shadowed native tool is a
	// misconfiguration, and discovering it when the model happens to call the
	// tool means discovering it in production.
	ErrNameCollision = errors.New("mcp: tool name collides with an existing tool")
	// ErrCallLimit is REQ-MCP-CLIENT-07's per-session cap (REQ-SEC-08).
	ErrCallLimit = errors.New("mcp: per-session call limit reached")
	// ErrSamplingNotAllowed is REQ-MCP-CLIENT-08's gate.
	ErrSamplingNotAllowed = errors.New("mcp: sampling is not enabled for this server")
	ErrNotInitialized     = errors.New("mcp: connection is not initialized")
)

// ServerConfig is one `[[mcp.servers]]` entry (REQ-MCP-CLIENT-07).
type ServerConfig struct {
	Name    string
	Command string
	Args    []string
	URL     string
	Dir     string
	// Transport selects the remote revision for a URL server
	// (REQ-MCP-CLIENT-02). Empty auto-negotiates: 2025-03-26 first, falling
	// back to 2024-11-05 when the server rejects the POST. A field is needed
	// because REQ-MCP-CLIENT-07's `url` alone does not say which of the two
	// specs is behind it.
	Transport HTTPMode
	// Headers are sent on every request to a URL server. Values may carry
	// ${VAR} references, resolved from the secrets store exactly like Env — a
	// remote server's bearer token has the same reason not to sit in a config
	// file as a subprocess's does.
	Headers map[string]string
	// Env values may carry ${VAR} references, resolved at spawn time against
	// the secrets store (REQ-MCP-CLIENT-10).
	Env map[string]string
	// ToolPrefix overrides the default `<name>__`. An empty string in the
	// config file means the default; DisablePrefix is how a caller asks for
	// none, because "" cannot mean both.
	ToolPrefix    string
	DisablePrefix bool
	AllowSampling bool
	// PerSessionCallLimit is 0 for the default. A NEGATIVE value disables the
	// limit, so "unlimited" is something a caller has to write down.
	PerSessionCallLimit int
	Timeout             time.Duration
}

func (c ServerConfig) prefix() string {
	if c.DisablePrefix {
		return ""
	}
	if c.ToolPrefix != "" {
		return c.ToolPrefix
	}
	return c.Name + "__"
}

func (c ServerConfig) callLimit() int {
	switch {
	case c.PerSessionCallLimit < 0:
		return -1 // explicitly unlimited
	case c.PerSessionCallLimit == 0:
		return DefaultCallLimit
	}
	return c.PerSessionCallLimit
}

func (c ServerConfig) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// SamplingHandler answers a server's sampling/createMessage request. It is
// consulted only for a server with AllowSampling set.
type SamplingHandler func(ctx context.Context, p SamplingParams) (SamplingResult, error)

// ConnectionOptions are the runtime hooks a connection needs.
type ConnectionOptions struct {
	// Audit receives an event for every tool call and every sampling request
	// (REQ-MCP-CLIENT-03, REQ-MCP-CLIENT-08).
	Audit func(core.AuditEvent)
	// Sampling answers server-initiated sampling. Nil refuses.
	Sampling SamplingHandler
	// Warnf reports non-fatal protocol trouble. Nil discards.
	Warnf  func(format string, args ...any)
	Limits wire.Limits
	// ClientInfo identifies us in the handshake.
	ClientInfo Implementation
	Now        func() time.Time
}

// ServerConnection is REQ-MCP-CLIENT-03.
type ServerConnection struct {
	cfg  ServerConfig
	opts ConnectionOptions
	tr   Transport
	corr *correlator

	mu          sync.Mutex
	initialized bool
	info        InitializeResult
	tools       []ToolDefinition
	toolsValid  bool
	calls       int
	closed      bool

	readerDone chan struct{}
	readErr    error
}

// NewConnection wraps an established transport.
func NewConnection(cfg ServerConfig, tr Transport, opts ConnectionOptions) *ServerConnection {
	if opts.ClientInfo.Name == "" {
		opts.ClientInfo = Implementation{Name: "agentkit-go", Version: "0.1.0"}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &ServerConnection{
		cfg: cfg, opts: opts, tr: tr, corr: newCorrelator(),
		readerDone: make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Name is the configured server name.
func (c *ServerConnection) Name() string { return c.cfg.Name }

// Info returns the handshake result.
func (c *ServerConnection) Info() InitializeResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Initialize performs the handshake and capability negotiation.
func (c *ServerConnection) Initialize(ctx context.Context) error {
	var caps ClientCapabilities
	if c.cfg.AllowSampling && c.opts.Sampling != nil {
		// Advertised only when we will actually honour it (REQ-MCP-CLIENT-08).
		// Advertising a capability we then refuse invites a server to build a
		// flow around it and fail at the worst moment.
		caps.Sampling = &struct{}{}
	}

	var res InitializeResult
	err := c.call(ctx, MethodInitialize, InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    caps,
		ClientInfo:      c.opts.ClientInfo,
	}, &res)
	if err != nil {
		return fmt.Errorf("mcp: %s: initialize: %w", c.cfg.Name, err)
	}
	if res.ProtocolVersion != ProtocolVersion && res.ProtocolVersion != LegacyProtocolVersion {
		// Not fatal. A version we do not recognise may still speak the subset
		// we use, and refusing outright would break against every future
		// server; the warning is what makes the mismatch findable when
		// something later does not work.
		c.warnf("server %q negotiated protocol %q, which this build does not model "+
			"(known: %s, %s)", c.cfg.Name, res.ProtocolVersion, ProtocolVersion, LegacyProtocolVersion)
	}

	c.mu.Lock()
	c.info, c.initialized = res, true
	c.mu.Unlock()

	// The initialized notification is fire-and-forget by protocol design.
	if err := c.notify(MethodInitialized, struct{}{}); err != nil {
		return fmt.Errorf("mcp: %s: initialized notification: %w", c.cfg.Name, err)
	}
	return nil
}

// ListTools returns the server's tools, cached (REQ-CACHE-07).
//
// The cache is invalidated ONLY by notifications/tools/list_changed or by
// RefreshTools. Re-listing per turn would put a round trip on the critical
// path of every request for a list that changes approximately never.
func (c *ServerConnection) ListTools(ctx context.Context) ([]ToolDefinition, error) {
	c.mu.Lock()
	if c.toolsValid {
		out := append([]ToolDefinition(nil), c.tools...)
		c.mu.Unlock()
		return out, nil
	}
	initialized := c.initialized
	c.mu.Unlock()

	if !initialized {
		return nil, ErrNotInitialized
	}

	var all []ToolDefinition
	cursor := ""
	for {
		var res ToolsListResult
		if err := c.call(ctx, MethodToolsList, ToolsListParams{Cursor: cursor}, &res); err != nil {
			return nil, fmt.Errorf("mcp: %s: tools/list: %w", c.cfg.Name, err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" || res.NextCursor == cursor {
			// A server repeating its cursor would otherwise page forever.
			break
		}
		cursor = res.NextCursor
		if len(all) > 10_000 {
			c.warnf("server %q listed more than 10000 tools; stopping", c.cfg.Name)
			break
		}
	}

	c.mu.Lock()
	c.tools, c.toolsValid = all, true
	c.mu.Unlock()
	return append([]ToolDefinition(nil), all...), nil
}

// RefreshTools drops the cache (REQ-CACHE-07's explicit refresh).
func (c *ServerConnection) RefreshTools() {
	c.mu.Lock()
	c.toolsValid, c.tools = false, nil
	c.mu.Unlock()
}

// Call invokes a tool by its UNQUALIFIED name (REQ-MCP-CLIENT-03).
func (c *ServerConnection) Call(ctx context.Context, toolName string, args map[string]any) (ToolsCallResult, error) {
	start := c.opts.Now()

	c.mu.Lock()
	if !c.initialized {
		c.mu.Unlock()
		return ToolsCallResult{}, ErrNotInitialized
	}
	limit := c.cfg.callLimit()
	if limit >= 0 && c.calls >= limit {
		c.mu.Unlock()
		c.auditCall(toolName, args, true, 0, start)
		return ToolsCallResult{}, fmt.Errorf("%w: %s allows %d calls per session",
			ErrCallLimit, c.cfg.Name, limit)
	}
	c.calls++
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, c.cfg.timeout())
	defer cancel()

	var res ToolsCallResult
	err := c.call(ctx, MethodToolsCall, ToolsCallParams{Name: toolName, Arguments: args}, &res)
	elapsed := c.opts.Now().Sub(start).Milliseconds()
	if err != nil {
		c.auditCall(toolName, args, true, elapsed, start)
		return ToolsCallResult{}, fmt.Errorf("mcp: %s: tools/call %q: %w", c.cfg.Name, toolName, err)
	}

	res.Content = CapContent(res.Content)
	c.auditCall(toolName, args, res.IsError, elapsed, start)
	return res, nil
}

// CapContent applies REQ-MCP-CLIENT-09's 50K cap across the whole result.
//
// The budget is spent ACROSS the content items, not per item: a server
// returning two hundred blocks of 49K each would otherwise pass a per-item cap
// and deliver ten megabytes. The note is addressed to the model, because the
// model is what has to decide whether to ask for less next time.
func CapContent(items []Content) []Content {
	remaining := ResultCharCap
	out := make([]Content, 0, len(items))
	for i, it := range items {
		if it.Type != "text" {
			out = append(out, it)
			continue
		}
		runes := []rune(it.Text)
		if len(runes) <= remaining {
			remaining -= len(runes)
			out = append(out, it)
			continue
		}
		if remaining > 0 {
			it.Text = string(runes[:remaining])
			out = append(out, it)
		}
		dropped := len(items) - i - 1
		note := fmt.Sprintf(
			"\n\n[truncated: this result exceeded the %d-character cap. %d further content "+
				"block(s) were dropped. Narrow the request — a filter, a smaller range, a "+
				"more specific query — rather than retrying it unchanged.]",
			ResultCharCap, dropped)
		out = append(out, Content{Type: "text", Text: note})
		return out
	}
	return out
}

// Close tears the connection down.
func (c *ServerConnection) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	err := c.tr.Close()
	// Wake every in-flight call rather than leaving each to its own timeout.
	c.corr.fail(ErrTransportClosed)

	// Bounded. Closing the transport is what unblocks the read loop, and a
	// transport whose Close does not do that would otherwise make Close itself
	// the thing that hangs — a shutdown path that can deadlock is worse than
	// one that reports a leak.
	select {
	case <-c.readerDone:
	case <-time.After(2 * time.Second):
		c.warnf("server %q: the read loop did not stop after the transport was closed",
			c.cfg.Name)
	}
	return err
}

// ---------------------------------------------------------------- plumbing

func (c *ServerConnection) call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id, ch, err := c.corr.next()
	if err != nil {
		return err
	}
	msg, err := json.Marshal(Message{JSONRPC: Version, ID: id, Method: method, Params: raw})
	if err != nil {
		c.corr.forget(id)
		return err
	}
	if err := c.tr.Send(msg); err != nil {
		c.corr.forget(id)
		return err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if out == nil {
			return nil
		}
		return decodeParams(resp.Result, out, c.opts.Limits)
	case <-ctx.Done():
		// Forget the waiter so a timed-out call does not leak an entry for the
		// life of the connection.
		c.corr.forget(id)
		// Tell the server to stop. Without this a cancelled call leaves the
		// handler running to completion on the other side — for a tool that
		// spends money or holds a lock, "the client gave up" and "the work
		// stopped" have to be the same event.
		c.cancelRemote(id, ctx.Err())
		return ctx.Err()
	}
}

// cancelRemote sends notifications/cancelled for an abandoned request. A send
// failure is deliberately ignored: the call has already failed, and the
// cancellation is a courtesy to a peer that may itself be the reason the send
// cannot complete.
func (c *ServerConnection) cancelRemote(id ID, cause error) {
	rawID, err := id.MarshalJSON()
	if err != nil {
		return
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	_ = c.notify(MethodCancelled, CancelledParams{RequestID: rawID, Reason: reason})
}

func (c *ServerConnection) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	msg, err := json.Marshal(Message{JSONRPC: Version, Method: method, Params: raw})
	if err != nil {
		return err
	}
	return c.tr.Send(msg)
}

func (c *ServerConnection) readLoop() {
	defer close(c.readerDone)
	for {
		frame, err := c.tr.Receive()
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			c.corr.fail(err)
			return
		}

		var m Message
		if derr := decodeFrame(frame, &m, c.opts.Limits); derr != nil {
			// REQ-SEC-11.4: a decoder is poisoned by its first malformed
			// message. The framing is already untrustworthy, so there is no
			// safe place to resume from.
			c.warnf("server %q sent an undecodable frame; tearing down: %v", c.cfg.Name, derr)
			c.mu.Lock()
			c.readErr = derr
			c.mu.Unlock()
			c.corr.fail(derr)
			_ = c.tr.Close()
			return
		}

		switch {
		case m.IsResponse():
			if !c.corr.deliver(&m) {
				c.warnf("server %q sent a response for unknown id %s", c.cfg.Name, m.ID)
			}
		case m.IsNotification():
			c.handleNotification(&m)
		case m.IsRequest():
			go c.handleRequest(&m)
		}
	}
}

func (c *ServerConnection) handleNotification(m *Message) {
	if m.Method == MethodToolsChanged {
		c.RefreshTools()
	}
}

func (c *ServerConnection) handleRequest(m *Message) {
	switch m.Method {
	case MethodPing:
		c.respond(m.ID, struct{}{}, nil)

	case MethodSampling:
		var p SamplingParams
		if err := decodeParams(m.Params, &p, c.opts.Limits); err != nil {
			c.auditSampling(false, "malformed params")
			c.respond(m.ID, nil, Errorf(CodeInvalidParams, "%v", err))
			return
		}
		// REQ-MCP-CLIENT-08: EVERY sampling request is audited, refused ones
		// included. A refusal that leaves no trace is indistinguishable from a
		// server that never asked, and the two want very different responses.
		if !c.cfg.AllowSampling || c.opts.Sampling == nil {
			c.auditSampling(false, "sampling is not enabled for this server")
			c.respond(m.ID, nil, Errorf(CodeMethodNotFound, "%v", ErrSamplingNotAllowed))
			return
		}
		c.auditSampling(true, "")
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.timeout())
		defer cancel()
		res, err := c.opts.Sampling(ctx, p)
		if err != nil {
			c.respond(m.ID, nil, Errorf(CodeInternalError, "%v", err))
			return
		}
		c.respond(m.ID, res, nil)

	default:
		c.respond(m.ID, nil, Errorf(CodeMethodNotFound, "method %q is not implemented", m.Method))
	}
}

func (c *ServerConnection) respond(id ID, result any, rpcErr *Error) {
	out := Message{JSONRPC: Version, ID: id, Error: rpcErr}
	if rpcErr == nil {
		raw, err := json.Marshal(result)
		if err != nil {
			out.Error = Errorf(CodeInternalError, "%v", err)
		} else {
			out.Result = raw
		}
	}
	msg, err := json.Marshal(out)
	if err != nil {
		return
	}
	if err := c.tr.Send(msg); err != nil {
		c.warnf("server %q: sending response: %v", c.cfg.Name, err)
	}
}

func (c *ServerConnection) warnf(format string, args ...any) {
	if c.opts.Warnf != nil {
		c.opts.Warnf(format, args...)
	}
}

func (c *ServerConnection) auditCall(tool string, args map[string]any, isErr bool, elapsed int64, at time.Time) {
	if c.opts.Audit == nil {
		return
	}
	raw, _ := json.Marshal(args)
	c.opts.Audit(core.AuditEvent{
		Kind: core.AuditToolCall, Timestamp: at,
		ServerName: c.cfg.Name, ToolName: tool,
		ArgumentsHash: core.HashArguments(raw),
		IsError:       isErr, ElapsedMS: elapsed,
	})
}

func (c *ServerConnection) auditSampling(allowed bool, why string) {
	if c.opts.Audit == nil {
		return
	}
	e := core.AuditEvent{
		Kind: core.AuditToolCall, Timestamp: c.opts.Now(),
		ServerName: c.cfg.Name, ToolName: MethodSampling,
		IsError: !allowed,
	}
	if why != "" {
		e.Error = why
	}
	c.opts.Audit(e)
}

// decodeFrame decodes one JSON-RPC frame with the REQ-SEC-11 bounds.
func decodeFrame(frame []byte, m *Message, limits wire.Limits) error {
	if err := wire.Guard(frame, limits); err != nil {
		return err
	}
	if err := json.Unmarshal(frame, m); err != nil {
		return err
	}
	if m.JSONRPC != Version {
		return fmt.Errorf("mcp: frame declares jsonrpc %q, want %q", m.JSONRPC, Version)
	}
	return nil
}

// QualifiedName is REQ-MCP-CLIENT-05's `server_name__tool_name`.
func QualifiedName(cfg ServerConfig, tool string) string { return cfg.prefix() + tool }

// SplitQualified is the inverse, given the prefix.
func SplitQualified(cfg ServerConfig, qualified string) (string, bool) {
	p := cfg.prefix()
	if p == "" {
		return qualified, true
	}
	return strings.CutPrefix(qualified, p)
}
