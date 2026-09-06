package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/wire"
)

// §6.8's requirements name nightshift's tools and resources —
// process_issue, get_session_status, nightshift://config. Those belong to the
// DAEMON, not to a library: AgentKit has no issues and no sessions to list.
//
// So REQ-MCP-SERVER-03's "explicit tool registration" is what ships here, and
// the host registers its own. The same split as REQ-PLUGIN-10's
// --validate-plugins: the SDK owns the mechanism, the product owns the
// inventory.

// ToolHandler answers a tools/call.
type ToolHandler func(ctx context.Context, args map[string]any) (ToolsCallResult, error)

// ResourceHandler answers a resources/read.
type ResourceHandler func(ctx context.Context, uri string) (ResourcesReadResult, error)

// ServerOptions configures a Server.
type ServerOptions struct {
	Info Implementation
	// Audit receives an event per inbound tool call.
	Audit  func(core.AuditEvent)
	Warnf  func(format string, args ...any)
	Limits wire.Limits
	// Instructions is the optional free-text hint returned by initialize.
	Instructions string
	// PageSize bounds a tools/list or resources/list page. Zero means one
	// page: pagination costs a round trip, and a server with nine tools
	// should not make its client pay for one.
	PageSize int
}

// Server is the AgentKit MCP server (REQ-MCP-SERVER-01..07).
//
// It is OFF unless a host constructs one: there is no init(), no ambient
// listener and no default port. REQ-MCP-SERVER-01 says optional and off by
// default, and the only way a library can promise that is by having nothing
// that starts on its own.
type Server struct {
	opts ServerOptions

	mu        sync.RWMutex
	tools     map[string]registeredTool
	toolOrder []string
	resources map[string]registeredResource
	resOrder  []string
	// templates is a SLICE, not a map: matching is order-sensitive (first
	// registration wins) and two templates can legitimately overlap, so there
	// is no key that would not lose one of them.
	templates []registeredTemplate
	// sessions is every live connection, so a registration change can reach
	// the clients that were told listChanged is supported.
	sessions map[*serverSession]struct{}
}

type registeredTool struct {
	def     ToolDefinition
	handler ToolHandler
}

type registeredResource struct {
	res     Resource
	handler ResourceHandler
}

type registeredTemplate struct {
	tmpl    ResourceTemplate
	pattern *uriPattern
	handler TemplateHandler
}

// TemplateHandler answers a resources/read whose URI matched a template. The
// extracted variables are passed decoded so a handler is not re-parsing the
// URI its own template already described.
type TemplateHandler func(ctx context.Context, uri string, vars map[string]string) (ResourcesReadResult, error)

func NewServer(opts ServerOptions) *Server {
	if opts.Info.Name == "" {
		opts.Info = Implementation{Name: "agentkit-go", Version: "0.1.0"}
	}
	return &Server{
		opts:      opts,
		tools:     map[string]registeredTool{},
		resources: map[string]registeredResource{},
		sessions:  map[*serverSession]struct{}{},
	}
}

// RegisterTool is REQ-MCP-SERVER-03's explicit registration.
//
// Re-registering a name REPLACES it and is not an error: a host rebuilding its
// tool set on a config reload would otherwise have to track what it had
// already registered, and getting that wrong is worse than the override.
//
// A change after a client has connected emits notifications/tools/list_changed
// to every live session. The initialize handshake advertises
// `tools.listChanged: true`, and a capability we advertise but never honour is
// worse than one we never claimed: a client that trusts it caches a tool list
// forever.
func (s *Server) RegisterTool(def ToolDefinition, h ToolHandler) error {
	if def.Name == "" {
		return errors.New("mcp: a registered tool needs a name")
	}
	if h == nil {
		return fmt.Errorf("mcp: tool %q has no handler", def.Name)
	}
	s.mu.Lock()
	if _, exists := s.tools[def.Name]; !exists {
		s.toolOrder = append(s.toolOrder, def.Name)
	}
	s.tools[def.Name] = registeredTool{def: def, handler: h}
	sessions := s.liveSessionsLocked()
	s.mu.Unlock()

	notifyAll(sessions, MethodToolsChanged)
	return nil
}

// UnregisterTool removes a tool. Removing an absent one is a no-op rather than
// an error, so a host tearing down a plugin does not have to know whether the
// registration succeeded.
func (s *Server) UnregisterTool(name string) {
	s.mu.Lock()
	_, existed := s.tools[name]
	if existed {
		delete(s.tools, name)
		s.toolOrder = removeString(s.toolOrder, name)
	}
	sessions := s.liveSessionsLocked()
	s.mu.Unlock()

	if existed {
		notifyAll(sessions, MethodToolsChanged)
	}
}

// RegisterResource is REQ-MCP-SERVER-05's mechanism for a concrete URI.
func (s *Server) RegisterResource(r Resource, h ResourceHandler) error {
	if r.URI == "" {
		return errors.New("mcp: a registered resource needs a uri")
	}
	if h == nil {
		return fmt.Errorf("mcp: resource %q has no handler", r.URI)
	}
	s.mu.Lock()
	if _, exists := s.resources[r.URI]; !exists {
		s.resOrder = append(s.resOrder, r.URI)
	}
	s.resources[r.URI] = registeredResource{res: r, handler: h}
	sessions := s.liveSessionsLocked()
	s.mu.Unlock()

	notifyAll(sessions, MethodResourcesChanged)
	return nil
}

// UnregisterResource removes a concrete resource.
func (s *Server) UnregisterResource(uri string) {
	s.mu.Lock()
	_, existed := s.resources[uri]
	if existed {
		delete(s.resources, uri)
		s.resOrder = removeString(s.resOrder, uri)
	}
	sessions := s.liveSessionsLocked()
	s.mu.Unlock()

	if existed {
		notifyAll(sessions, MethodResourcesChanged)
	}
}

// RegisterResourceTemplate registers a parameterised resource
// (REQ-MCP-SERVER-05: `nightshift://issues/{number}/triage-report`).
//
// The template is compiled at REGISTRATION time, so a malformed one is a
// startup error the host sees immediately rather than a read that never
// matches anything.
func (s *Server) RegisterResourceTemplate(t ResourceTemplate, h TemplateHandler) error {
	if err := t.Validate(); err != nil {
		return fmt.Errorf("mcp: resource template: %w", err)
	}
	if h == nil {
		return fmt.Errorf("mcp: resource template %q has no handler", t.URITemplate)
	}
	pat, err := compileTemplate(t.URITemplate)
	if err != nil {
		return err
	}
	s.mu.Lock()
	replaced := false
	for i := range s.templates {
		if s.templates[i].tmpl.URITemplate == t.URITemplate {
			s.templates[i] = registeredTemplate{tmpl: t, pattern: pat, handler: h}
			replaced = true
			break
		}
	}
	if !replaced {
		s.templates = append(s.templates, registeredTemplate{tmpl: t, pattern: pat, handler: h})
	}
	sessions := s.liveSessionsLocked()
	s.mu.Unlock()

	notifyAll(sessions, MethodResourcesChanged)
	return nil
}

func removeString(list []string, want string) []string {
	for i, v := range list {
		if v == want {
			return append(list[:i:i], list[i+1:]...)
		}
	}
	return list
}

// liveSessionsLocked snapshots the sessions under s.mu so the caller can send
// AFTER releasing it. Sending under the registry lock would let one wedged
// client's transport block every registration on the server.
func (s *Server) liveSessionsLocked() []*serverSession {
	if len(s.sessions) == 0 {
		return nil
	}
	out := make([]*serverSession, 0, len(s.sessions))
	for sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

func notifyAll(sessions []*serverSession, method string) {
	for _, sess := range sessions {
		// Only a session that finished initialize: the spec forbids traffic
		// other than ping and logging before the handshake completes, and a
		// list_changed arriving mid-handshake is a notification about a list
		// the client has not yet been told exists.
		if !sess.ready() {
			continue
		}
		sess.send(Message{JSONRPC: Version, Method: method})
	}
}

// serverSession is one connection's state: the transport, and the correlator
// for requests the SERVER issues to the client (sampling).
type serverSession struct {
	tr     Transport
	corr   *correlator
	sendMu sync.Mutex

	// initialized gates every method other than initialize and ping. The
	// handshake is where the protocol version and the capability sets are
	// agreed; answering tools/call before it means acting on a request whose
	// wire contract was never settled.
	initialized atomic.Bool

	// inflight maps a request id to its cancellation. Cancellation is a
	// notification, so it arrives on the SAME read loop that is not blocked on
	// the handler — which is only true because dispatch runs on its own
	// goroutine.
	inflightMu sync.Mutex
	inflight   map[string]*inflightRequest
}

type inflightRequest struct {
	cancel context.CancelFunc
	// cancelled is set by the notification, not by ctx expiry. The spec says a
	// cancelled request SHOULD NOT be answered, and only the notification
	// path carries that instruction — a handler that merely timed out still
	// owes the client a reply.
	cancelled atomic.Bool
}

func (sess *serverSession) ready() bool { return sess.initialized.Load() }

func (sess *serverSession) track(key string, cancel context.CancelFunc) *inflightRequest {
	req := &inflightRequest{cancel: cancel}
	sess.inflightMu.Lock()
	sess.inflight[key] = req
	sess.inflightMu.Unlock()
	return req
}

func (sess *serverSession) untrack(key string) {
	sess.inflightMu.Lock()
	delete(sess.inflight, key)
	sess.inflightMu.Unlock()
}

func (sess *serverSession) cancelRequest(key string) bool {
	sess.inflightMu.Lock()
	req, ok := sess.inflight[key]
	sess.inflightMu.Unlock()
	if !ok {
		return false
	}
	req.cancelled.Store(true)
	req.cancel()
	return true
}

type sessionKey struct{}

// ErrNoBackChannel is returned by RequestSampling when the handler is running
// on a transport that has none.
//
// HTTP mode is request/response: there is no channel back to the client
// between a request and its reply, so a sampling request cannot be expressed
// at all. Failing with this rather than hanging is the difference between a
// handler that reports the limitation and one that times out.
var ErrNoBackChannel = errors.New("mcp: this transport has no back-channel for " +
	"server-initiated requests (sampling needs stdio or a streaming transport)")

// RequestSampling asks the CLIENT to sample, from inside a tool handler
// (REQ-MCP-CLIENT-08's server half).
func RequestSampling(ctx context.Context, p SamplingParams) (SamplingResult, error) {
	sess, ok := ctx.Value(sessionKey{}).(*serverSession)
	if !ok || sess == nil {
		return SamplingResult{}, ErrNoBackChannel
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return SamplingResult{}, err
	}
	id, ch, err := sess.corr.next()
	if err != nil {
		return SamplingResult{}, err
	}
	msg, err := json.Marshal(Message{JSONRPC: Version, ID: id, Method: MethodSampling, Params: raw})
	if err != nil {
		sess.corr.forget(id)
		return SamplingResult{}, err
	}
	sess.sendMu.Lock()
	err = sess.tr.Send(msg)
	sess.sendMu.Unlock()
	if err != nil {
		sess.corr.forget(id)
		return SamplingResult{}, err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return SamplingResult{}, resp.Error
		}
		var out SamplingResult
		if err := decodeParams(resp.Result, &out, wire.Limits{}); err != nil {
			return SamplingResult{}, err
		}
		return out, nil
	case <-ctx.Done():
		sess.corr.forget(id)
		return SamplingResult{}, ctx.Err()
	}
}

// Serve runs the protocol over one transport until it ends (REQ-MCP-SERVER-02,
// stdio mode).
//
// Each request is dispatched on its OWN goroutine. That is not an
// optimization: a handler calling RequestSampling waits for a response that
// arrives on this same transport, so a synchronous dispatch loop would be
// blocked inside the handler and could never read it. Serving one request at a
// time and supporting sampling are mutually exclusive.
//
// Responses may therefore be written out of order. JSON-RPC correlates by id
// and permits it.
func (s *Server) Serve(ctx context.Context, tr Transport) error {
	sess := &serverSession{tr: tr, corr: newCorrelator(), inflight: map[string]*inflightRequest{}}
	ctx = context.WithValue(ctx, sessionKey{}, sess)

	s.mu.Lock()
	s.sessions[sess] = struct{}{}
	s.mu.Unlock()

	var wg sync.WaitGroup
	// Every in-flight handler is joined before returning: a handler still
	// writing to a transport its caller believes is finished is how a shutdown
	// turns into a write to a closed pipe.
	defer func() {
		s.mu.Lock()
		delete(s.sessions, sess)
		s.mu.Unlock()

		sess.corr.fail(ErrTransportClosed)
		// Cancel what is still running before waiting for it. Without this a
		// handler blocked on a slow dependency holds the shutdown open for as
		// long as it likes, and the transport it would write to is gone.
		sess.inflightMu.Lock()
		for _, req := range sess.inflight {
			req.cancel()
		}
		sess.inflightMu.Unlock()
		wg.Wait()
	}()

	for {
		frame, err := tr.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		var m Message
		if derr := decodeFrame(frame, &m, s.opts.Limits); derr != nil {
			// REQ-SEC-11.4: poisoned by the first malformed message. A parse
			// error means the framing is already untrustworthy, so there is no
			// safe place to resume — and an MCP server that resynchronizes
			// lets a peer choose where the next message starts.
			s.warnf("undecodable frame; tearing the connection down: %v", derr)
			sess.send(Message{JSONRPC: Version,
				Error: Errorf(CodeParseError, "malformed message: %v", derr)})
			return derr
		}

		switch {
		case m.IsNotification():
			s.handleNotification(sess, &m)
			continue
		case m.IsResponse():
			if !sess.corr.deliver(&m) {
				s.warnf("client sent a response for unknown id %s", m.ID)
			}
			continue
		}

		reqCtx, cancel := context.WithCancel(ctx)
		// initialize is exempt: the spec says it MUST NOT be cancelled, and
		// tracking it would let a client cancel the handshake it is in the
		// middle of and leave the session permanently un-initialized.
		var tracked *inflightRequest
		if m.Method != MethodInitialize {
			tracked = sess.track(m.ID.Key(), cancel)
		}

		wg.Add(1)
		go func(m Message) {
			defer wg.Done()
			defer cancel()
			if tracked != nil {
				defer sess.untrack(m.ID.Key())
			}

			result, rpcErr := s.dispatch(reqCtx, &m)

			// A cancelled request is answered with silence. Sending a result
			// the client has already stopped waiting for makes it look like a
			// response to whatever it asked next.
			if tracked != nil && tracked.cancelled.Load() {
				return
			}

			out := Message{JSONRPC: Version, ID: m.ID, Error: rpcErr}
			if rpcErr == nil {
				raw, merr := json.Marshal(result)
				if merr != nil {
					out.Error = Errorf(CodeInternalError, "%v", merr)
				} else {
					out.Result = raw
				}
			}
			sess.send(out)
		}(m)
	}
}

// handleNotification processes the two notifications a client sends us.
func (s *Server) handleNotification(sess *serverSession, m *Message) {
	switch m.Method {
	case MethodInitialized:
		sess.initialized.Store(true)
	case MethodCancelled:
		var p CancelledParams
		if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
			s.warnf("undecodable cancellation: %v", err)
			return
		}
		var id ID
		if err := json.Unmarshal(p.RequestID, &id); err != nil {
			s.warnf("cancellation names an unusable request id: %v", err)
			return
		}
		if !sess.cancelRequest(id.Key()) {
			// Not an error worth answering: a cancellation racing a response
			// that already went out is normal, and the spec says to ignore it.
			s.warnf("cancellation for %s: no such in-flight request", id)
		}
	}
}

func (sess *serverSession) send(m Message) {
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	_ = sess.tr.Send(raw)
}

// dispatch handles one request and returns the result or a JSON-RPC error. It
// is shared by the stdio loop and the HTTP mode.
func (s *Server) dispatch(ctx context.Context, m *Message) (any, *Error) {
	sess, _ := ctx.Value(sessionKey{}).(*serverSession)

	// Initialize-first, on connection-oriented transports only.
	//
	// HTTP mode is stateless per request: there is no connection for a
	// handshake to be a property of, so there is nothing to enforce against
	// and pretending otherwise would mean rejecting every HTTP call or
	// inventing a session id the client never asked for. The API key is that
	// mode's gate (REQ-MCP-SERVER-07).
	if sess != nil && !sess.ready() && !allowedBeforeInit(m.Method) {
		return nil, Errorf(CodeInvalidRequest,
			"%q was called before the initialize handshake completed; send "+
				"initialize and then notifications/initialized first", m.Method)
	}

	switch m.Method {
	case MethodInitialize:
		var p InitializeParams
		if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		if err := p.Validate(); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		caps := ServerCapabilities{}
		caps.Tools = &struct {
			ListChanged bool `json:"listChanged,omitzero"`
		}{ListChanged: true}
		caps.Resources = &struct {
			Subscribe   bool `json:"subscribe,omitzero"`
			ListChanged bool `json:"listChanged,omitzero"`
		}{ListChanged: true}
		return InitializeResult{
			// REQ-MCP-SERVER-06: echo the client's version when we speak it,
			// otherwise name ours. Always answering with our own turns a
			// version a client explicitly asked for into one it has to detect
			// we ignored.
			ProtocolVersion: NegotiateProtocol(p.ProtocolVersion),
			Capabilities:    caps,
			ServerInfo:      s.opts.Info,
			Instructions:    s.opts.Instructions,
		}, nil

	case MethodPing:
		return struct{}{}, nil

	case MethodToolsList:
		var p ToolsListParams
		if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		s.mu.RLock()
		names, next, perr := s.page(s.toolOrder, p.Cursor)
		out := ToolsListResult{Tools: make([]ToolDefinition, 0, len(names)), NextCursor: next}
		for _, n := range names {
			out.Tools = append(out.Tools, s.tools[n].def)
		}
		s.mu.RUnlock()
		if perr != nil {
			return nil, perr
		}
		return out, nil

	case MethodToolsCall:
		var p ToolsCallParams
		if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		s.mu.RLock()
		t, ok := s.tools[p.Name]
		s.mu.RUnlock()
		if !ok {
			return nil, Errorf(CodeMethodNotFound, "no tool named %q", p.Name)
		}
		res, err := s.invoke(ctx, t, p)
		if err != nil {
			// A handler failure is a TOOL error, not a protocol error: the
			// caller's model should see it and react, where a JSON-RPC error
			// says the call never happened.
			return ToolsCallResult{IsError: true,
				Content: []Content{{Type: "text", Text: err.Error()}}}, nil
		}
		res.Content = CapContent(res.Content)
		return res, nil

	case MethodResourcesList:
		var p ResourcesListParams
		if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		s.mu.RLock()
		uris, next, perr := s.page(s.resOrder, p.Cursor)
		out := ResourcesListResult{Resources: make([]Resource, 0, len(uris)), NextCursor: next}
		for _, u := range uris {
			out.Resources = append(out.Resources, s.resources[u].res)
		}
		s.mu.RUnlock()
		if perr != nil {
			return nil, perr
		}
		return out, nil

	case MethodResourceTemplatesList:
		s.mu.RLock()
		out := ResourceTemplatesListResult{
			ResourceTemplates: make([]ResourceTemplate, 0, len(s.templates)),
		}
		for _, t := range s.templates {
			out.ResourceTemplates = append(out.ResourceTemplates, t.tmpl)
		}
		s.mu.RUnlock()
		return out, nil

	case MethodResourcesRead:
		var p ResourcesReadParams
		if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		return s.readResource(ctx, p.URI)
	}
	return nil, Errorf(CodeMethodNotFound, "method %q is not implemented", m.Method)
}

// allowedBeforeInit is the pre-handshake allowlist. ping is here because a
// client may legitimately probe liveness before it commits to a handshake.
func allowedBeforeInit(method string) bool {
	return method == MethodInitialize || method == MethodPing
}

// readResource resolves a URI against the exact registrations first, then the
// templates.
//
// Exact wins. A template like `x://docs/{name}` also matches a concretely
// registered `x://docs/readme`, and letting registration order decide which
// one answers makes a specific registration silently unreachable.
func (s *Server) readResource(ctx context.Context, uri string) (any, *Error) {
	s.mu.RLock()
	r, exact := s.resources[uri]
	templates := s.templates
	s.mu.RUnlock()

	if exact {
		res, err := r.handler(ctx, uri)
		if err != nil {
			return nil, Errorf(CodeInternalError, "%v", err)
		}
		return res, nil
	}

	for _, t := range templates {
		vars, ok := t.pattern.match(uri)
		if !ok {
			continue
		}
		res, err := t.handler(ctx, uri, vars)
		if err != nil {
			return nil, Errorf(CodeInternalError, "%v", err)
		}
		return res, nil
	}
	return nil, Errorf(CodeInvalidParams, "no resource at %q", uri)
}

// cursorPrefix marks a cursor as one we minted.
//
// Cursors are opaque to clients by spec, and rejecting anything without the
// prefix means a client that guessed at the format gets a clear error instead
// of a page computed from a name it invented.
const cursorPrefix = "n:"

// page slices an ordered name list, resuming AFTER the cursor's entry.
//
// The cursor is a name rather than an index because indices shift: unregister
// a tool between two pages and an index-based cursor silently skips the item
// that moved into its place. A name that is no longer there is detectable, and
// this reports it.
//
// Callers hold s.mu.
func (s *Server) page(order []string, cursor string) ([]string, string, *Error) {
	start := 0
	if cursor != "" {
		after, ok := strings.CutPrefix(cursor, cursorPrefix)
		if !ok {
			return nil, "", Errorf(CodeInvalidParams, "cursor %q was not issued by this server", cursor)
		}
		idx := -1
		for i, n := range order {
			if n == after {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, "", Errorf(CodeInvalidParams,
				"cursor %q points at an entry that no longer exists; restart the listing", cursor)
		}
		start = idx + 1
	}

	size := s.opts.PageSize
	if size <= 0 || start+size >= len(order) {
		return order[start:], "", nil
	}
	end := start + size
	return order[start:end], cursorPrefix + order[end-1], nil
}

// invoke runs a handler with a panic guard and an audit record.
func (s *Server) invoke(ctx context.Context, t registeredTool, p ToolsCallParams) (res ToolsCallResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			// A panicking handler is the host's bug, and taking the server
			// down for it would turn one broken tool into a dead connection
			// for every other one.
			err = fmt.Errorf("mcp: handler for %q panicked: %v", p.Name, r)
		}
		s.audit(p, err != nil || res.IsError)
	}()
	return t.handler(ctx, p.Arguments)
}

func (s *Server) audit(p ToolsCallParams, isErr bool) {
	if s.opts.Audit == nil {
		return
	}
	raw, _ := json.Marshal(p.Arguments)
	s.opts.Audit(core.AuditEvent{
		Kind: core.AuditToolCall, ToolName: p.Name,
		ArgumentsHash: core.HashArguments(raw), IsError: isErr,
	})
}

func (s *Server) warnf(format string, args ...any) {
	if s.opts.Warnf != nil {
		s.opts.Warnf(format, args...)
	}
}

// ToolNames returns the registered tool names, in registration order.
func (s *Server) ToolNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.toolOrder...)
}

// ---------------------------------------------------------------- HTTP mode

// HTTPOptions configures the REQ-MCP-SERVER-02 HTTP transport.
type HTTPOptions struct {
	// APIKey is REQUIRED (REQ-MCP-SERVER-07). An empty one is refused at
	// construction rather than defaulting to open: stdio mode relies on OS
	// process isolation, and an HTTP listener has none — a server that starts
	// unauthenticated because a config key was missing is the failure mode
	// this requirement exists to prevent.
	APIKey string
	// Header names where the key is accepted. Empty means Authorization
	// (Bearer) and X-API-Key.
	Headers []string
	// MaxBodyBytes bounds a request body. Zero uses the wire default.
	MaxBodyBytes int64
}

// ErrNoAPIKey is REQ-MCP-SERVER-07's refusal.
var ErrNoAPIKey = errors.New("mcp: HTTP mode requires an API key; stdio mode is the " +
	"unauthenticated option and it relies on OS process isolation")

// HTTPHandler serves the protocol over HTTP with API-key authentication.
func (s *Server) HTTPHandler(opts HTTPOptions) (http.Handler, error) {
	if opts.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	headers := opts.Headers
	if len(headers) == 0 {
		headers = []string{"Authorization", "X-API-Key"}
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = wire.Defaults().MaxMessageBytes
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authentication FIRST, before the method check and before the body is
		// read. Checking the method first tells an unauthenticated caller
		// which verbs exist, and reading the body first lets one spend our
		// memory without a credential.
		if !s.authorized(r, opts.APIKey, headers) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			writeRPC(w, Message{JSONRPC: Version,
				Error: Errorf(CodeInvalidRequest, "request body: %v", err)})
			return
		}

		var m Message
		if derr := decodeFrame(body, &m, s.opts.Limits); derr != nil {
			writeRPC(w, Message{JSONRPC: Version,
				Error: Errorf(CodeParseError, "malformed message: %v", derr)})
			return
		}
		if m.IsNotification() {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		result, rpcErr := s.dispatch(r.Context(), &m)
		out := Message{JSONRPC: Version, ID: m.ID, Error: rpcErr}
		if rpcErr == nil {
			raw, merr := json.Marshal(result)
			if merr != nil {
				out.Error = Errorf(CodeInternalError, "%v", merr)
			} else {
				out.Result = raw
			}
		}
		writeRPC(w, out)
	}), nil
}

// authorized compares in CONSTANT TIME.
//
// A byte-by-byte comparison that returns early leaks the key one character at
// a time to anyone who can measure the response, and an API key is exactly the
// kind of secret somebody will try that against.
func (s *Server) authorized(r *http.Request, key string, headers []string) bool {
	want := []byte(key)
	for _, h := range headers {
		v := strings.TrimSpace(r.Header.Get(h))
		if v == "" {
			continue
		}
		v = strings.TrimPrefix(v, "Bearer ")
		v = strings.TrimSpace(v)
		if subtle.ConstantTimeCompare([]byte(v), want) == 1 {
			return true
		}
	}
	return false
}

func writeRPC(w http.ResponseWriter, m Message) {
	w.Header().Set("Content-Type", "application/json")
	raw, err := json.Marshal(m)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(raw)
}

// SortedToolNames is a helper for a host presenting its inventory.
func SortedToolNames(s *Server) []string {
	out := s.ToolNames()
	sort.Strings(out)
	return out
}
