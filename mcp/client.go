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

// DefaultCallLimit and DefaultTimeout are REQ-MCP-CLIENT-07's defaults;
// DefaultReconnectLimit is NFR-REL-03's.
const (
	DefaultCallLimit      = 1000
	DefaultTimeout        = 30 * time.Second
	DefaultReconnectLimit = 3
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
	// ErrTooManyInputRounds is REQ-MCP-CLIENT-08.2's bound.
	ErrTooManyInputRounds = errors.New("mcp: too many multi-round-trip input requests")
	// ErrReconnectLimit is NFR-REL-03's bound: the transport died more times
	// than per_session_reconnect_limit allows, and the connection is dead for
	// the rest of the session. Every later call fails with it immediately.
	ErrReconnectLimit = errors.New("mcp: per-session reconnect limit reached; the connection is dead")
)

// ServerConfig is one `[[mcp.servers]]` entry (REQ-MCP-CLIENT-07).
type ServerConfig struct {
	Name    string
	Command string
	Args    []string
	// URL selects Streamable HTTP. A url server's tool cache is kept honest by
	// the server's ttlMs hint ONLY: Pool.Connect does not hold a
	// subscriptions/listen stream open over HTTP, because that is a request
	// held open for the life of the session against a remote — a connection
	// slot and a keep-alive burden the stdio case does not have. An embedder
	// that wants live invalidation for a remote server calls
	// SubscribeToolChanges itself. A command server IS subscribed by Connect
	// (REQ-CACHE-07).
	URL string
	Dir string
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
	// PerSessionReconnectLimit is NFR-REL-03's bound on re-spawning (stdio) or
	// re-opening (HTTP) a transport that died, per session. 0 means
	// DefaultReconnectLimit; a NEGATIVE value means never reconnect, by the
	// same rule as PerSessionCallLimit.
	PerSessionReconnectLimit int
	Timeout                  time.Duration
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

func (c ServerConfig) reconnectLimit() int {
	switch {
	case c.PerSessionReconnectLimit < 0:
		return 0 // explicitly never
	case c.PerSessionReconnectLimit == 0:
		return DefaultReconnectLimit
	}
	return c.PerSessionReconnectLimit
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
	// Dial re-establishes the transport after it dies (NFR-REL-03). Nil means
	// a dead transport stays dead. Pool.Connect sets it to re-spawn the
	// subprocess or re-open the endpoint exactly as the first connection did.
	Dial func(ctx context.Context) (Transport, error)
}

// link is one incarnation of the transport: the transport, the correlator
// that owns its in-flight ids, and the read loop draining it.
//
// A reconnect (NFR-REL-03) replaces the whole link rather than swapping the
// transport underneath the old correlator. An id issued on a dead transport
// must never be answered by a live one — the peer that answers it is a
// different process, and a match would be a coincidence of counters.
type link struct {
	tr   Transport
	corr *correlator
	done chan struct{} // closed when the read loop exits
}

// ServerConnection is REQ-MCP-CLIENT-03.
type ServerConnection struct {
	cfg  ServerConfig
	opts ConnectionOptions

	mu  sync.Mutex
	cur *link
	// reconnects counts successful AND failed re-dials against
	// PerSessionReconnectLimit. dead is set when the budget is spent.
	reconnects int
	dead       bool
	deadErr    error
	// reconnected is closed and replaced on every successful reconnect, so
	// a goroutine waiting for a fresh link (the subscription keeper) can be
	// woken without polling.
	reconnected chan struct{}
	// reconnectMu serialises reconnect attempts WITHOUT holding mu across the
	// close and the dial: the old link's read loop takes mu to invalidate
	// the tool cache, and waiting for it to exit under mu would deadlock.
	reconnectMu sync.Mutex
	// subCancel ends the tools/list_changed subscription Connect started.
	subCancel context.CancelFunc

	info       DiscoverResult
	discovered bool
	tools      []ToolDefinition
	toolsValid bool
	// toolsExpire is the ttlMs deadline from the list result. 2026-07-28
	// requires servers to send a freshness hint; honouring it is what keeps a
	// cached list from outliving a server that has no subscription stream open
	// to tell us it changed.
	toolsExpire time.Time
	calls       int
	// subscribed is set when the server acknowledges a subscriptions/listen.
	subscribed bool
	closed     bool
}

// NewConnection wraps an established transport.
func NewConnection(cfg ServerConfig, tr Transport, opts ConnectionOptions) *ServerConnection {
	if opts.ClientInfo.Name == "" {
		opts.ClientInfo = Implementation{Name: "agentkit-go", Version: "0.1.0"}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &ServerConnection{cfg: cfg, opts: opts, reconnected: make(chan struct{})}
	c.cur = c.newLink(tr)
	return c
}

func (c *ServerConnection) newLink(tr Transport) *link {
	l := &link{tr: tr, corr: newCorrelator(), done: make(chan struct{})}
	go c.readLoop(l)
	return l
}

// setDial arms reconnection after construction, for a pool that wants
// discovery to have succeeded once before it will ever re-spawn a server.
func (c *ServerConnection) setDial(dial func(ctx context.Context) (Transport, error)) {
	c.mu.Lock()
	c.opts.Dial = dial
	c.mu.Unlock()
}

// Name is the configured server name.
func (c *ServerConnection) Name() string { return c.cfg.Name }

// Info returns the server/discover result: the zero value until Discover has
// run, because nothing else populates it.
func (c *ServerConnection) Info() DiscoverResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Alive reports whether the current transport is still delivering frames. It
// turns false when the read loop has exited — the server exited, the stream
// broke, or Close ran — and true again after a successful reconnect.
func (c *ServerConnection) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.dead {
		return false
	}
	select {
	case <-c.cur.done:
		return false
	default:
		return true
	}
}

// Reconnects is how many reconnect attempts this session has spent.
func (c *ServerConnection) Reconnects() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reconnects
}

// Dead reports whether the reconnect budget is exhausted (NFR-REL-03).
func (c *ServerConnection) Dead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

// Discover is 2026-07-28's OPTIONAL up-front probe (REQ-MCP-CLIENT-03,
// amended in 0.4.0).
//
// It is not a handshake and nothing requires calling it: every request carries
// its own version and capabilities, so a client may invoke any RPC inline and
// handle UnsupportedProtocolVersion if it comes back. It is called here
// because a pool that knows a server's capabilities before the first tool call
// can report an unusable server at connect time rather than mid-turn.
func (c *ServerConnection) Discover(ctx context.Context) error {
	var res DiscoverResult
	if err := c.call(ctx, MethodDiscover, DiscoverParams{}, &res); err != nil {
		var rpcErr *Error
		if errors.As(err, &rpcErr) && rpcErr.Code == CodeUnsupportedProtocolVersion {
			return fmt.Errorf("mcp: %s: %w", c.cfg.Name, unsupportedVersionError(rpcErr))
		}
		return fmt.Errorf("mcp: %s: server/discover: %w", c.cfg.Name, err)
	}
	if err := res.Validate(); err != nil {
		return fmt.Errorf("mcp: %s: server/discover: %w", c.cfg.Name, err)
	}
	if !containsVersion(res.SupportedVersions, ProtocolVersion) {
		// Not fatal on its own — the server answered our request, so it
		// evidently tolerates the version we sent — but a server whose
		// advertised set excludes ours will reject something later, and saying
		// so now beats a confusing failure three calls in.
		c.warnf("server %q advertises %v and does not list %s; later requests may be "+
			"rejected with UnsupportedProtocolVersion",
			c.cfg.Name, res.SupportedVersions, ProtocolVersion)
	}

	c.mu.Lock()
	c.info, c.discovered = res, true
	c.mu.Unlock()
	return nil
}

// unsupportedVersionError renders a -32022 into something a human can act on.
func unsupportedVersionError(e *Error) error {
	var data UnsupportedVersionData
	if len(e.Data) > 0 {
		_ = json.Unmarshal(e.Data, &data)
	}
	if len(data.Supported) == 0 {
		return fmt.Errorf("the server does not support protocol version %s and named no "+
			"alternative", ProtocolVersion)
	}
	return fmt.Errorf("the server supports %v, not %s; this build is modern-only and "+
		"speaks no earlier revision", data.Supported, ProtocolVersion)
}

func containsVersion(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func (c *ServerConnection) ListTools(ctx context.Context) ([]ToolDefinition, error) {
	c.mu.Lock()
	// The cache is valid until list_changed invalidates it OR its ttlMs
	// expires. A zero deadline means the server sent no hint, which under
	// 2026-07-28 it is required to do — treat that as "no expiry" rather than
	// "expired", so a non-conforming server degrades to the old behaviour
	// instead of re-listing on every turn.
	if c.toolsValid && (c.toolsExpire.IsZero() || c.opts.Now().Before(c.toolsExpire)) {
		out := append([]ToolDefinition(nil), c.tools...)
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	var all []ToolDefinition
	var ttl int64
	cursor := ""
	for {
		var res ToolsListResult
		if err := c.call(ctx, MethodToolsList, ToolsListParams{Cursor: cursor}, &res); err != nil {
			return nil, fmt.Errorf("mcp: %s: tools/list: %w", c.cfg.Name, err)
		}
		// The SHORTEST hint across pages wins: one page going stale makes the
		// assembled list stale, and caching to the longest would keep serving
		// a list the server already said had expired in part.
		if res.TTLMs > 0 && (ttl == 0 || res.TTLMs < ttl) {
			ttl = res.TTLMs
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
	c.toolsExpire = time.Time{}
	if ttl > 0 {
		c.toolsExpire = c.opts.Now().Add(time.Duration(ttl) * time.Millisecond)
	}
	c.mu.Unlock()
	return append([]ToolDefinition(nil), all...), nil
}

// RefreshTools drops the cache (REQ-CACHE-07's explicit refresh).
func (c *ServerConnection) RefreshTools() {
	c.mu.Lock()
	c.toolsValid, c.tools = false, nil
	c.toolsExpire = time.Time{}
	c.mu.Unlock()
}

// Call invokes a tool by its UNQUALIFIED name (REQ-MCP-CLIENT-03).
func (c *ServerConnection) Call(ctx context.Context, toolName string, args map[string]any) (ToolsCallResult, error) {
	start := c.opts.Now()

	c.mu.Lock()
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

	res, err := c.callWithMRTR(ctx, toolName, args)
	elapsed := c.opts.Now().Sub(start).Milliseconds()
	if err != nil {
		c.auditCall(toolName, args, true, elapsed, start)
		return ToolsCallResult{}, fmt.Errorf("mcp: %s: tools/call %q: %w", c.cfg.Name, toolName, err)
	}

	res.Content = CapContent(res.Content)
	c.auditCall(toolName, args, res.IsError, elapsed, start)
	return res, nil
}

// resultTypeOf reads the discriminator without binding the body.
func resultTypeOf(raw []byte, limits wire.Limits) (ResultType, error) {
	if len(raw) == 0 {
		return ResultComplete, nil
	}
	v, err := wire.Parse(raw, limits)
	if err != nil {
		return "", err
	}
	rt, ok := v.Object["resultType"]
	if !ok {
		return ResultComplete, nil
	}
	return ResultType(rt.String), nil
}

// MaxInputRounds bounds the MRTR retry loop (REQ-MCP-CLIENT-08.2).
//
// The old server-initiated model made an unbounded exchange impossible by
// construction: the server asked, the client answered, the call continued.
// MRTR turns that into "the client retries the whole request", and a server
// that returns input_required every time would hold the client in a loop
// forever. Three rounds is enough for the flows the spec illustrates and
// small enough that a runaway is a reported error rather than a hang.
const MaxInputRounds = 3

// callWithMRTR runs tools/call, answering any input requests and retrying.
func (c *ServerConnection) callWithMRTR(ctx context.Context, toolName string, args map[string]any) (ToolsCallResult, error) {
	params := ToolsCallParams{Name: toolName, Arguments: args}

	for round := 0; ; round++ {
		var raw json.RawMessage
		if err := c.call(ctx, MethodToolsCall, params, &raw); err != nil {
			return ToolsCallResult{}, err
		}

		// resultType decides how to read the body. It is read WITHOUT binding
		// the rest: a probe struct declaring only resultType would reject
		// every other field under REQ-SEC-12.1's unknown-property rule, and
		// binding the full result before knowing which shape it is would
		// pick the wrong struct half the time.
		//
		// An earlier-revision server omits the field, and the spec says to
		// treat that as "complete".
		kind, err := resultTypeOf(raw, c.opts.Limits)
		if err != nil {
			return ToolsCallResult{}, err
		}
		if kind != ResultInputRequired {
			var res ToolsCallResult
			if err := decodeParams(raw, &res, c.opts.Limits); err != nil {
				return ToolsCallResult{}, err
			}
			return res, nil
		}

		if round+1 >= MaxInputRounds {
			return ToolsCallResult{}, fmt.Errorf(
				"%w: %s asked for input %d times without completing",
				ErrTooManyInputRounds, c.cfg.Name, round+1)
		}
		var need InputRequiredResult
		if err := decodeParams(raw, &need, c.opts.Limits); err != nil {
			return ToolsCallResult{}, err
		}
		answers, err := c.resolveInputRequests(ctx, need.InputRequests)
		if err != nil {
			return ToolsCallResult{}, err
		}
		// The retry carries the answers AND the server's opaque state, which
		// is how it reconstructs where it was — there is no session holding
		// that for it any more.
		params.InputResponses = answers
		params.RequestState = need.RequestState
	}
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
	l := c.cur
	stop := c.subCancel
	c.mu.Unlock()

	if stop != nil {
		stop()
	}
	return c.closeLink(l)
}

// closeLink shuts one link down and waits, bounded, for its read loop.
//
// Closing the transport is what unblocks the read loop, and a transport whose
// Close does not do that would otherwise make Close itself the thing that
// hangs — a shutdown path that can deadlock is worse than one that reports a
// leak. Callers must NOT hold c.mu: the read loop takes it.
func (c *ServerConnection) closeLink(l *link) error {
	err := l.tr.Close()
	// Wake every in-flight call rather than leaving each to its own timeout.
	l.corr.fail(ErrTransportClosed)

	select {
	case <-l.done:
	case <-time.After(2 * time.Second):
		c.warnf("server %q: the read loop did not stop after the transport was closed",
			c.cfg.Name)
	}
	return err
}

// ---------------------------------------------------------------- plumbing

// linkDeadError marks a failure that happened BEFORE the request reached the
// peer, on a transport that is gone. Only those are safe to retry on a fresh
// link: a request that was sent may have executed, and a tool call executed
// twice is the failure NFR-REL-03's "surface as is_error" exists to prevent.
type linkDeadError struct {
	cause error
}

func (e *linkDeadError) Error() string { return e.cause.Error() }
func (e *linkDeadError) Unwrap() error { return e.cause }

// call issues one request, reconnecting once per spent reconnect budget if
// the transport turns out to be dead before the request went out.
func (c *ServerConnection) call(ctx context.Context, method string, params, out any) error {
	return c.callOpts(ctx, method, params, out, true)
}

// callNoReconnect is call for the paths that must not spend the reconnect
// budget on their own initiative: the background subscription keeper, which
// would otherwise re-spawn a server while the session is idle.
func (c *ServerConnection) callNoReconnect(ctx context.Context, method string, params, out any) error {
	return c.callOpts(ctx, method, params, out, false)
}

func (c *ServerConnection) callOpts(ctx context.Context, method string, params, out any, reconnect bool) error {
	raw, err := c.withMeta(params)
	if err != nil {
		return err
	}
	l, err := c.currentLink()
	if err != nil {
		return err
	}
	// Bounded by the reconnect budget: every iteration either returns or
	// spends one reconnect, and reconnect fails once the budget is gone.
	for {
		err := c.callOn(ctx, l, method, raw, out)
		var dead *linkDeadError
		if !errors.As(err, &dead) || !reconnect {
			return err
		}
		if l, err = c.reconnect(ctx, l, dead.cause); err != nil {
			return err
		}
	}
}

func (c *ServerConnection) currentLink() (*link, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrTransportClosed
	}
	if c.dead {
		return nil, c.deadErr
	}
	return c.cur, nil
}

// callOn sends one request on a specific link and waits for its response.
//
// A response stream that ends before the response arrived (Streamable HTTP,
// REQ-MCP-CLIENT-02.3) is re-issued ONCE, with a NEW id: 2026-07-28 has no
// resumability, so a fresh request is the only way to get the answer, and
// once is the bound because a server that drops every stream is not going
// to answer the third time either.
func (c *ServerConnection) callOn(ctx context.Context, l *link, method string, raw json.RawMessage, out any) error {
	for reissued := false; ; {
		id, ch, err := l.corr.next()
		if err != nil {
			// The correlator closes when the read loop exits: the transport
			// is gone and nothing was sent.
			return &linkDeadError{cause: err}
		}
		msg, err := json.Marshal(Message{JSONRPC: Version, ID: id, Method: method, Params: raw})
		if err != nil {
			l.corr.forget(id)
			return err
		}
		if err := sendContext(ctx, l.tr, msg); err != nil {
			l.corr.forget(id)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrTransportClosed) {
				return &linkDeadError{cause: err}
			}
			return err
		}

		select {
		case resp := <-ch:
			if resp.Error != nil {
				if resp.Error.Code == CodeResponseStreamBroken && !reissued {
					c.warnf("server %q: the response stream for %s ended without a response; "+
						"re-issuing the request with a new id", c.cfg.Name, method)
					reissued = true
					continue
				}
				return resp.Error
			}
			if out == nil {
				return nil
			}
			return decodeParams(resp.Result, out, c.opts.Limits)
		case <-ctx.Done():
			// Forget the waiter so a timed-out call does not leak an entry for
			// the life of the connection.
			l.corr.forget(id)
			// Tell the server to stop. Without this a cancelled call leaves
			// the handler running to completion on the other side — for a
			// tool that spends money or holds a lock, "the client gave up"
			// and "the work stopped" have to be the same event.
			c.cancelRemote(l, id, ctx.Err())
			return ctx.Err()
		}
	}
}

// reconnect replaces a dead link (NFR-REL-03), or reports why it cannot.
//
// The budget counts ATTEMPTS, failed dials included: a server that cannot be
// re-spawned would otherwise be re-spawned on every call for the rest of the
// session. Once it is spent the connection is dead and stays dead — the
// session's remaining calls fail fast with ErrReconnectLimit, which the pool
// turns into an is_error tool result the loop carries on from.
func (c *ServerConnection) reconnect(ctx context.Context, stale *link, cause error) (*link, error) {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		return nil, ErrTransportClosed
	case c.cur != stale:
		// Another call already replaced it.
		l := c.cur
		c.mu.Unlock()
		return l, nil
	case c.dead:
		err := c.deadErr
		c.mu.Unlock()
		return nil, err
	case c.opts.Dial == nil:
		c.mu.Unlock()
		return nil, cause
	}
	limit := c.cfg.reconnectLimit()
	if c.reconnects >= limit {
		c.dead = true
		c.deadErr = fmt.Errorf("%w: %s died again after %d reconnect(s): %v",
			ErrReconnectLimit, c.cfg.Name, c.reconnects, cause)
		c.mu.Unlock()
		c.warnf("server %q: transport died (%v) and the reconnect limit (%d) is spent; "+
			"the connection is dead for the rest of the session", c.cfg.Name, cause, limit)
		return nil, c.deadErr
	}
	c.reconnects++
	attempt := c.reconnects
	dial := c.opts.Dial
	c.mu.Unlock()

	c.warnf("server %q: transport died (%v); reconnecting (%d/%d)", c.cfg.Name, cause, attempt, limit)
	_ = c.closeLink(stale)
	// WithoutCancel: the new transport must outlive the call that happened to
	// find the old one dead.
	tr, err := dial(context.WithoutCancel(ctx))
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: reconnect %d/%d: %w", c.cfg.Name, attempt, limit, err)
	}
	l := c.newLink(tr)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = c.closeLink(l)
		return nil, ErrTransportClosed
	}
	c.cur = l
	close(c.reconnected)
	c.reconnected = make(chan struct{})
	c.mu.Unlock()
	return l, nil
}

// withMeta injects the per-request `_meta` that replaced the handshake.
//
// Every request carries the protocol version and the client's capabilities:
// there is no session in which they could have been agreed once, so omitting
// them makes the request uninterpretable and a conforming server rejects it.
//
// The injection goes through a map rather than a struct field on every params
// type. A field per type would be seven places to forget it, and the one that
// got forgotten would fail against a strict server only.
func (c *ServerConnection) withMeta(params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	obj := map[string]json.RawMessage{}
	if len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
	}
	meta, err := json.Marshal(c.requestMeta())
	if err != nil {
		return nil, err
	}
	obj["_meta"] = meta
	return json.Marshal(obj)
}

// requestMeta is what this client says about itself on every request.
func (c *ServerConnection) requestMeta() RequestMeta {
	caps := ClientCapabilities{}
	if c.cfg.AllowSampling && c.opts.Sampling != nil {
		// Advertised only when we will actually honour it
		// (REQ-MCP-CLIENT-08). Under MRTR the cost of over-advertising is
		// higher than it was: a server builds an input request, we refuse it,
		// and the work it did to get there is wasted.
		caps.Sampling = &struct{}{}
	}
	info := c.opts.ClientInfo
	return RequestMeta{
		ProtocolVersion:    ProtocolVersion,
		ClientInfo:         &info,
		ClientCapabilities: caps,
	}
}

// cancelRemote sends notifications/cancelled for an abandoned request. A send
// failure is deliberately ignored: the call has already failed, and the
// cancellation is a courtesy to a peer that may itself be the reason the send
// cannot complete.
func (c *ServerConnection) cancelRemote(l *link, id ID, cause error) {
	rawID, err := id.MarshalJSON()
	if err != nil {
		return
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	_ = c.notify(l, MethodCancelled, map[string]any{"requestId": json.RawMessage(rawID), "reason": reason})
}

func (c *ServerConnection) notify(l *link, method string, params any) error {
	raw, err := c.withMeta(params)
	if err != nil {
		return err
	}
	msg, err := json.Marshal(Message{JSONRPC: Version, Method: method, Params: raw})
	if err != nil {
		return err
	}
	return l.tr.Send(msg)
}

func (c *ServerConnection) readLoop(l *link) {
	defer close(l.done)
	for {
		frame, err := l.tr.Receive()
		if err != nil {
			l.corr.fail(err)
			return
		}

		var m Message
		if derr := decodeFrame(frame, &m, c.opts.Limits); derr != nil {
			// REQ-SEC-11.4: a decoder is poisoned by its first malformed
			// message. The framing is already untrustworthy, so there is no
			// safe place to resume from.
			c.warnf("server %q sent an undecodable frame; tearing down: %v", c.cfg.Name, derr)
			l.corr.fail(derr)
			_ = l.tr.Close()
			return
		}

		switch {
		case m.IsResponse():
			if !l.corr.deliver(&m) {
				c.warnf("server %q sent a response for unknown id %s", c.cfg.Name, m.ID)
			}
		case m.IsNotification():
			c.handleNotification(&m)
		case m.IsRequest():
			// 2026-07-28 removed server-initiated requests entirely: what a
			// server used to ask for mid-call now comes back as an
			// InputRequiredResult and is answered by retrying (MRTR). A server
			// still sending one is speaking an earlier revision, and answering
			// it would be pretending to be dual-era.
			c.warnf("server %q sent a %q REQUEST; this revision has no server-initiated "+
				"requests and the message was ignored", c.cfg.Name, m.Method)
		}
	}
}

func (c *ServerConnection) handleNotification(m *Message) {
	switch m.Method {
	case MethodSubscriptionsAck:
		c.mu.Lock()
		c.subscribed = true
		c.mu.Unlock()
	case MethodToolsChanged:
		c.RefreshTools()
	}
}

// keepToolsSubscribed holds a tools/list_changed subscription open for the
// life of the connection (REQ-CACHE-07).
//
// The tool cache is the one piece of client state that goes stale silently,
// and a server that advertises listChanged sends the notification ONLY on a
// stream the client opened. Nobody else was opening one: an embedder had to
// know to call SubscribeToolChanges, and one that did not had a cache bounded
// by nothing but the server's ttlMs hint.
//
// The loop is bounded by the reconnect budget: a subscription that ends is
// re-opened only after a successful reconnect has produced a fresh link, and
// never on the loop's own initiative — a server refusing to be subscribed
// would otherwise be asked again forever.
func (c *ServerConnection) keepToolsSubscribed() {
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return
	}
	c.subCancel = cancel
	c.mu.Unlock()

	go func() {
		for {
			c.mu.Lock()
			fresh := c.reconnected
			c.mu.Unlock()

			err := c.SubscribeToolChanges(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				c.warnf("server %q: tools/list_changed subscription ended: %v; the tool "+
					"cache is bounded by ttlMs until the next reconnect", c.cfg.Name, err)
			}
			select {
			case <-fresh:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// resolveInputRequests answers an InputRequiredResult (REQ-MCP-CLIENT-08,
// amended in 0.4.0).
//
// Every entry is audited, refusals included: a refusal that leaves no trace is
// indistinguishable from a server that never asked, and the two want very
// different responses.
func (c *ServerConnection) resolveInputRequests(ctx context.Context, reqs map[string]json.RawMessage) (map[string]any, error) {
	out := make(map[string]any, len(reqs))
	for key, raw := range reqs {
		var ir InputRequest
		if err := decodeParams(raw, &ir, c.opts.Limits); err != nil {
			return nil, fmt.Errorf("mcp: %s: undecodable input request %q: %w",
				c.cfg.Name, key, err)
		}
		switch ir.Method {
		case MethodSampling:
			var p SamplingParams
			if err := decodeParams(ir.Params, &p, c.opts.Limits); err != nil {
				c.auditSampling(false, "malformed params")
				return nil, fmt.Errorf("mcp: %s: sampling params: %w", c.cfg.Name, err)
			}
			if !c.cfg.AllowSampling || c.opts.Sampling == nil {
				c.auditSampling(false, "sampling is not enabled for this server")
				return nil, fmt.Errorf("%w: %s", ErrSamplingNotAllowed, c.cfg.Name)
			}
			c.auditSampling(true, "")
			res, err := c.opts.Sampling(ctx, p)
			if err != nil {
				return nil, fmt.Errorf("mcp: %s: sampling: %w", c.cfg.Name, err)
			}
			out[key] = res
		default:
			// Roots and elicitation are not implemented here. Refusing by name
			// beats answering with something shaped wrong, which the server
			// would then act on.
			return nil, fmt.Errorf("mcp: %s: server asked for %q, which this client does "+
				"not implement", c.cfg.Name, ir.Method)
		}
	}
	return out, nil
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

// decodeFrame decodes one JSON-RPC frame with the REQ-SEC-11 bounds and the
// REQ-SEC-12.1 strictness: one bounded parse, then the closed-set envelope
// binder. See bindEnvelope for why encoding/json is not used here.
func decodeFrame(frame []byte, m *Message, limits wire.Limits) error {
	v, err := wire.Parse(frame, limits)
	if err != nil {
		return err
	}
	if err := bindEnvelope(v, m); err != nil {
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
