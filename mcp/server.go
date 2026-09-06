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
	// ListTTLMs is the ttlMs hint on every cacheable result (2026-07-28).
	// Zero means "immediately stale", which is the honest answer for a
	// registry a host can mutate at any moment through RegisterTool.
	ListTTLMs int64
	// CacheScope is "public" or "private". Empty means private: a tool list
	// can encode which tools THIS caller may see, and a shared intermediary
	// caching it publicly would serve one tenant's inventory to another.
	CacheScope string
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

// notifyAll delivers a change notification to every session that OPTED IN to
// that type on a subscriptions/listen stream.
//
// 2026-07-28 inverted this. There is no longer a stream you open to receive
// whatever the server feels like sending: a client names the types it wants,
// and the server MUST NOT send one it did not name. A session with no listen
// stream gets nothing at all, which is why a client that wants live updates
// has to ask.
func notifyAll(sessions []*serverSession, method string) {
	for _, sess := range sessions {
		sess.notify(method, nil)
	}
}

// serverSession is one connection's state: the transport, and the correlator
// for requests the SERVER issues to the client (sampling).
type serverSession struct {
	tr     Transport
	corr   *correlator
	sendMu sync.Mutex

	// sub is the filter this session opted in to, nil until it sends a
	// subscriptions/listen. subID is that request's id, which tags every
	// notification on the stream.
	subMu sync.Mutex
	sub   *SubscriptionFilter
	subID ID

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

// notify sends one change notification if this session subscribed to its type.
func (sess *serverSession) notify(method string, params any) {
	sess.subMu.Lock()
	sub := sess.sub
	id := sess.subID
	sess.subMu.Unlock()
	if sub == nil || !subscribedTo(*sub, method) {
		return
	}
	raw, err := json.Marshal(withSubscriptionID(params, id))
	if err != nil {
		return
	}
	sess.send(Message{JSONRPC: Version, Method: method, Params: raw})
}

// subscribedTo maps a notification method onto the filter field that opts in
// to it. An unknown method is NOT sent: the rule is an allowlist, so a
// notification type added later cannot leak to a client that never asked.
func subscribedTo(f SubscriptionFilter, method string) bool {
	switch method {
	case MethodToolsChanged:
		return f.ToolsListChanged
	case MethodResourcesChanged:
		return f.ResourcesListChanged
	case MethodResourcesUpdated:
		return len(f.ResourceSubscriptions) > 0
	}
	return false
}

// withSubscriptionID tags a notification with the stream it belongs to, so a
// client running several can tell them apart.
func withSubscriptionID(params any, id ID) map[string]any {
	out := map[string]any{}
	if params != nil {
		raw, err := json.Marshal(params)
		if err == nil {
			_ = json.Unmarshal(raw, &out)
		}
	}
	out["_meta"] = map[string]any{MetaSubscriptionID: id}
	return out
}

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

// ErrNoBackChannel is retained as the error a handler gets if it asks for
// input on a transport that cannot carry the retry.
//
// It is nearly unreachable now: MRTR needs no back-channel at all, because the
// server answers and the CLIENT comes back. That is the point of the redesign
// — the old model needed a stream the server could push a request onto, and
// HTTP had none between a request and its reply.
var ErrNoBackChannel = errors.New("mcp: this transport cannot carry a multi-round-trip retry")

// inputResponsesKey carries an MRTR retry's answers to the handler.
type inputResponsesKey struct{}

// MRTRContext is what a handler sees on a retry.
type MRTRContext struct {
	// Responses are the client's answers, keyed as the server keyed its
	// requests.
	Responses map[string]json.RawMessage
	// RequestState is whatever the handler put in it last time. The client is
	// required to treat it as opaque and hand it back unchanged.
	RequestState string
}

// InputFrom returns the MRTR answers on a retry, and false on a first attempt.
//
// A handler that needs client-side work reads this FIRST: if the answers are
// there, it is being re-invoked and should finish; if they are not, it returns
// NeedInput and will be called again.
func InputFrom(ctx context.Context) (MRTRContext, bool) {
	v, ok := ctx.Value(inputResponsesKey{}).(MRTRContext)
	return v, ok
}

// InputRequiredError is how a handler asks the client for something
// (REQ-MCP-CLIENT-08, amended in 0.4.0).
//
// It is an ERROR value because a handler's signature returns (result, error)
// and this is neither an ordinary result nor a failure. invoke recognises it
// and turns it into the wire's InputRequiredResult.
type InputRequiredError struct {
	Requests     map[string]InputRequest
	RequestState string
}

func (e *InputRequiredError) Error() string {
	return fmt.Sprintf("mcp: handler needs %d client input(s) before it can complete",
		len(e.Requests))
}

// NeedInput builds the error a handler returns to request input.
//
// state is handed back verbatim on the retry. It exists because the server
// keeps NOTHING between the two calls — there is no session — so anything the
// handler wants to remember has to travel through the client.
func NeedInput(state string, requests map[string]InputRequest) error {
	return &InputRequiredError{Requests: requests, RequestState: state}
}

// NeedSampling is the common case: ask the client to sample.
func NeedSampling(state, key string, p SamplingParams) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return NeedInput(state, map[string]InputRequest{
		key: {Method: MethodSampling, Params: raw},
	})
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

// handleNotification processes the one notification a client still sends.
//
// notifications/initialized is gone with the handshake. On Streamable HTTP
// notifications/cancelled is gone too — closing the response stream IS the
// cancellation signal — so this path is stdio's alone.
func (s *Server) handleNotification(sess *serverSession, m *Message) {
	switch m.Method {
	case MethodCancelled:
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
			Reason    string          `json:"reason,omitzero"`
		}
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
	// 2026-07-28 has no handshake to enforce ordering against. What replaces
	// it is a PER-REQUEST check: the version and capabilities arrive in
	// `_meta` on every request, and a request that names a version we do not
	// speak is rejected on its own rather than failing a connection nobody
	// opened.
	_ = sess
	if rpcErr := s.checkVersion(m); rpcErr != nil {
		return nil, rpcErr
	}

	switch m.Method {
	case MethodInitialize:
		// Recognised, never implemented. A legacy client has no fall-forward
		// mechanism, so this error message is the only diagnostic its user
		// will ever see — naming the versions we speak is the difference
		// between "method not found" and an actionable report. The spec makes
		// this a SHOULD; dropping legacy support is our decision, so the
		// resulting failure should not be mute.
		return nil, &Error{
			Code: CodeUnsupportedProtocolVersion,
			Message: "this server implements MCP " + ProtocolVersion + ", which removed the " +
				"initialize handshake; send requests directly with _meta instead",
			Data: mustJSON(UnsupportedVersionData{
				Supported: SupportedProtocolVersions(), Requested: "initialize (pre-2026-07-28)",
			}),
		}

	case MethodDiscover:
		// REQ-MCP-SERVER-06.1: servers MUST implement this.
		s.mu.RLock()
		caps := s.capabilities()
		s.mu.RUnlock()
		return DiscoverResult{
			ResultType:        ResultComplete,
			SupportedVersions: SupportedProtocolVersions(),
			Capabilities:      caps,
			Instructions:      s.opts.Instructions,
			Meta:              &ResultMeta{ServerInfo: &s.opts.Info},
			CacheHints:        s.cacheHints(),
		}, nil

	case MethodSubscriptionsListen:
		return s.listen(ctx, m)

	case MethodToolsList:
		var p ToolsListParams
		if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		s.mu.RLock()
		names, next, perr := s.page(s.toolOrder, p.Cursor)
		out := ToolsListResult{ResultType: ResultComplete, Meta: &ResultMeta{ServerInfo: &s.opts.Info},
			CacheHints: s.cacheHints(),
			Tools:      make([]ToolDefinition, 0, len(names)), NextCursor: next}
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
		// An MRTR retry carries the client's answers; a first attempt does not.
		// Handing the handler the same context either way would make it
		// impossible to tell which call this is.
		if len(p.InputResponses) > 0 || p.RequestState != "" {
			raw := make(map[string]json.RawMessage, len(p.InputResponses))
			for k, v := range p.InputResponses {
				b, merr := json.Marshal(v)
				if merr != nil {
					return nil, Errorf(CodeInvalidParams, "inputResponses[%q]: %v", k, merr)
				}
				raw[k] = b
			}
			ctx = context.WithValue(ctx, inputResponsesKey{},
				MRTRContext{Responses: raw, RequestState: p.RequestState})
		}

		res, err := s.invoke(ctx, t, p)
		if err != nil {
			// A handler asking for input is neither a result nor a failure.
			var need *InputRequiredError
			if errors.As(err, &need) {
				out := InputRequiredResult{
					ResultType:   ResultInputRequired,
					RequestState: need.RequestState,
					Meta:         &ResultMeta{ServerInfo: &s.opts.Info},
				}
				if len(need.Requests) > 0 {
					out.InputRequests = make(map[string]json.RawMessage, len(need.Requests))
					for k, v := range need.Requests {
						out.InputRequests[k] = mustJSON(v)
					}
				}
				return out, nil
			}
			// A handler failure is a TOOL error, not a protocol error: the
			// caller's model should see it and react, where a JSON-RPC error
			// says the call never happened.
			return ToolsCallResult{ResultType: ResultComplete, IsError: true,
				Content: []Content{{Type: "text", Text: err.Error()}}}, nil
		}
		res.Content = CapContent(res.Content)
		res.ResultType = ResultComplete
		return res, nil

	case MethodResourcesList:
		var p ResourcesListParams
		if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		s.mu.RLock()
		uris, next, perr := s.page(s.resOrder, p.Cursor)
		out := ResourcesListResult{ResultType: ResultComplete, Meta: &ResultMeta{ServerInfo: &s.opts.Info},
			CacheHints: s.cacheHints(),
			Resources:  make([]Resource, 0, len(uris)), NextCursor: next}
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
			ResultType: ResultComplete, Meta: &ResultMeta{ServerInfo: &s.opts.Info},
			CacheHints:        s.cacheHints(),
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

// checkVersion is REQ-MCP-SERVER-06.2.
//
// The version lives in the request's `_meta`, and a request that omits it is
// not interpretable: without a handshake there is no earlier moment at which
// it could have been agreed. Rejecting is the only honest answer — guessing
// "probably the current one" is how a client speaking something else gets
// served silently wrong semantics.
func (s *Server) checkVersion(m *Message) *Error {
	if m.Method == MethodInitialize {
		return nil // answered with its own diagnostic below
	}
	// Read `_meta` WITHOUT binding the whole params object. Binding it here
	// would decode every request twice — once against a probe struct that
	// cannot know the method's own fields, and again in the handler — and the
	// probe would reject the fields it does not model.
	version, err := metaProtocolVersion(m.Params, s.opts.Limits)
	if err != nil {
		return Errorf(CodeInvalidParams, "%v", err)
	}
	if version == "" {
		return &Error{
			Code: CodeUnsupportedProtocolVersion,
			Message: "every request must carry " + MetaProtocolVersion + " in _meta; " +
				"this revision has no handshake in which to have agreed one",
			Data: mustJSON(UnsupportedVersionData{Supported: SupportedProtocolVersions()}),
		}
	}
	if !Supports(version) {
		return &Error{
			Code:    CodeUnsupportedProtocolVersion,
			Message: "unsupported protocol version",
			Data: mustJSON(UnsupportedVersionData{
				Supported: SupportedProtocolVersions(), Requested: version,
			}),
		}
	}
	return nil
}

// metaProtocolVersion pulls one field out of a params object, bounds intact,
// without binding the rest.
func metaProtocolVersion(params []byte, limits wire.Limits) (string, error) {
	if len(params) == 0 {
		return "", nil
	}
	v, err := wire.Parse(params, limits)
	if err != nil {
		return "", err
	}
	meta, ok := v.Object[MetaKey]
	if !ok {
		return "", nil
	}
	ver, ok := meta.Object[MetaProtocolVersion]
	if !ok {
		return "", nil
	}
	return ver.String, nil
}

// capabilities is what this server advertises. Callers hold s.mu.
func (s *Server) capabilities() ServerCapabilities {
	caps := ServerCapabilities{}
	caps.Tools = &struct {
		ListChanged bool `json:"listChanged,omitzero"`
	}{ListChanged: true}
	caps.Resources = &struct {
		Subscribe   bool `json:"subscribe,omitzero"`
		ListChanged bool `json:"listChanged,omitzero"`
	}{ListChanged: true}
	return caps
}

// cacheHints is the freshness hint attached to every cacheable result.
// stampRead fills in the fields every result owes the 2026-07-28 wire, so a
// handler written by a host does not have to know about them.
func (s *Server) stampRead(res ResourcesReadResult) ResourcesReadResult {
	res.ResultType = ResultComplete
	if res.Meta == nil {
		res.Meta = &ResultMeta{ServerInfo: &s.opts.Info}
	}
	res.CacheHints = s.cacheHints()
	return res
}

func (s *Server) cacheHints() CacheHints {
	scope := s.opts.CacheScope
	if scope == "" {
		// Private by default. A tool list can encode which tools THIS caller is
		// allowed to see, and a shared intermediary caching it publicly would
		// serve one tenant's inventory to another.
		scope = CachePrivate
	}
	return CacheHints{TTLMs: s.opts.ListTTLMs, CacheScope: scope}
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

// listen implements subscriptions/listen (REQ-MCP-SERVER-06.5).
//
// The request does not return until the subscription ends. That is the whole
// design: its RESPONSE STREAM is the channel, so the notifications flow on the
// still-open response of this request, and the result that finally arrives
// means the server tore the subscription down.
//
// It therefore blocks a dispatch goroutine for the life of the stream, which
// is only safe because Serve dispatches each request on its own goroutine.
func (s *Server) listen(ctx context.Context, m *Message) (any, *Error) {
	sess, _ := ctx.Value(sessionKey{}).(*serverSession)
	if sess == nil {
		// HTTP mode answers one request per connection and returns; there is
		// no session object to hang a long-lived stream on. Saying so beats
		// holding the request open forever and returning nothing.
		return nil, Errorf(CodeInvalidRequest,
			"subscriptions/listen needs a connection-oriented transport")
	}
	var p SubscriptionsListenParams
	if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
		return nil, Errorf(CodeInvalidParams, "%v", err)
	}
	if !p.Notifications.Any() {
		// A stream that opted into nothing would stay open forever delivering
		// nothing, which is indistinguishable from a hung server.
		return nil, Errorf(CodeInvalidParams,
			"subscriptions/listen must opt in to at least one notification type")
	}

	sess.subMu.Lock()
	if sess.sub != nil {
		sess.subMu.Unlock()
		return nil, Errorf(CodeInvalidRequest, "this session already has a subscription")
	}
	filter := p.Notifications
	sess.sub, sess.subID = &filter, m.ID
	sess.subMu.Unlock()

	defer func() {
		sess.subMu.Lock()
		sess.sub = nil
		sess.subMu.Unlock()
	}()

	// Acknowledge, then block. The ack is what tells the client the stream is
	// live; without it a client cannot distinguish "subscribed and quiet" from
	// "the request never arrived".
	sess.send(Message{JSONRPC: Version, Method: MethodSubscriptionsAck,
		Params: mustJSON(withSubscriptionID(nil, m.ID))})

	<-ctx.Done()
	id := m.ID
	return SubscriptionsListenResult{
		ResultType: ResultComplete,
		Meta:       &ResultMeta{ServerInfo: &s.opts.Info, SubscriptionID: &id},
	}, nil
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
		return s.stampRead(res), nil
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
		return s.stampRead(res), nil
	}
	// -32602, not -32002: 2026-07-28 moved resource-not-found onto the
	// JSON-RPC Invalid Params code, because a URI the server does not host is
	// a bad parameter and not a protocol-level condition.
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
		// An input request is not a failed call: the handler has not finished
		// and will be called again. Auditing it as an error would double-count
		// every MRTR flow as a failure plus a success.
		var need *InputRequiredError
		if !errors.As(err, &need) {
			s.audit(p, err != nil || res.IsError)
		}
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
	// AllowedOrigins is the Origin allowlist. Empty REFUSES every request that
	// carries an Origin header at all, which is the safe default for a server
	// whose whole audience is local processes: a browser always sends one, a
	// local MCP client never does.
	AllowedOrigins []string
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
		// 2026-07-28 removed the GET endpoint: POST is the only verb.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Origin validation, against DNS rebinding. A browser page on any
		// origin can POST to a localhost server; without this check that page
		// reaches every tool the server exposes.
		if !s.originAllowed(r, opts.AllowedOrigins) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
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
		// Header/body agreement (-32020). The headers exist so a gateway can
		// route without parsing the body; if the two disagree, the gateway and
		// the server are acting on different requests, which is the exact
		// split this check exists to prevent.
		if mismatch := validateRoutingHeaders(r.Header, &m); mismatch != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeRPC(w, Message{JSONRPC: Version, ID: m.ID, Error: mismatch})
			return
		}

		result, rpcErr := s.dispatch(r.Context(), &m)
		if rpcErr != nil && rpcErr.Code == CodeUnsupportedProtocolVersion {
			// The spec pins the status for this one: 400, so an intermediary
			// that never reads the body still sees a client error.
			w.WriteHeader(http.StatusBadRequest)
			writeRPC(w, Message{JSONRPC: Version, ID: m.ID, Error: rpcErr})
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

// originAllowed implements the DNS-rebinding defence.
func (s *Server) originAllowed(r *http.Request, allowed []string) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin: not a browser request. A native MCP client does not send
		// one, and the spec only requires rejecting a PRESENT and invalid one.
		return true
	}
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, origin) {
			return true
		}
	}
	return false
}

// validateRoutingHeaders is 2026-07-28's server-side header check.
//
// A missing required header is as much a mismatch as a wrong one: the point is
// that an intermediary and the server agree, and an absent header means the
// intermediary routed on nothing.
func validateRoutingHeaders(h http.Header, m *Message) *Error {
	if got := h.Get(HeaderProtocolVersion); got == "" {
		return Errorf(CodeHeaderMismatch, "missing required header %s", HeaderProtocolVersion)
	} else if want := bodyProtocolVersion(m); want != "" && got != want {
		return Errorf(CodeHeaderMismatch,
			"%s header %q does not match the body's %s %q",
			HeaderProtocolVersion, got, MetaProtocolVersion, want)
	}
	if got := h.Get(HeaderMethod); got == "" {
		return Errorf(CodeHeaderMismatch, "missing required header %s", HeaderMethod)
	} else if got != m.Method {
		return Errorf(CodeHeaderMismatch,
			"%s header %q does not match the body method %q", HeaderMethod, got, m.Method)
	}

	want := bodyName(m)
	if want == "" {
		return nil // this method has no Mcp-Name source field
	}
	got := h.Get(HeaderName)
	if got == "" {
		return Errorf(CodeHeaderMismatch, "missing required header %s for %s",
			HeaderName, m.Method)
	}
	if DecodeHeaderValue(got) != want {
		return Errorf(CodeHeaderMismatch,
			"%s header %q does not match the body value %q", HeaderName, got, want)
	}
	return nil
}

func bodyProtocolVersion(m *Message) string {
	var p struct {
		Meta struct {
			ProtocolVersion string `json:"io.modelcontextprotocol/protocolVersion"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(m.Params, &p)
	return p.Meta.ProtocolVersion
}

// bodyName is the Mcp-Name source field: params.name or params.uri, depending
// on the method.
func bodyName(m *Message) string {
	var p struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	_ = json.Unmarshal(m.Params, &p)
	switch m.Method {
	case MethodToolsCall:
		return p.Name
	case MethodResourcesRead:
		return p.URI
	}
	return ""
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
