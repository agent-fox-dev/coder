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
}

type registeredTool struct {
	def     ToolDefinition
	handler ToolHandler
}

type registeredResource struct {
	res     Resource
	handler ResourceHandler
}

func NewServer(opts ServerOptions) *Server {
	if opts.Info.Name == "" {
		opts.Info = Implementation{Name: "agentkit-go", Version: "0.1.0"}
	}
	return &Server{
		opts:      opts,
		tools:     map[string]registeredTool{},
		resources: map[string]registeredResource{},
	}
}

// RegisterTool is REQ-MCP-SERVER-03's explicit registration.
//
// Re-registering a name REPLACES it and is not an error: a host rebuilding its
// tool set on a config reload would otherwise have to track what it had
// already registered, and getting that wrong is worse than the override.
func (s *Server) RegisterTool(def ToolDefinition, h ToolHandler) error {
	if def.Name == "" {
		return errors.New("mcp: a registered tool needs a name")
	}
	if h == nil {
		return fmt.Errorf("mcp: tool %q has no handler", def.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tools[def.Name]; !exists {
		s.toolOrder = append(s.toolOrder, def.Name)
	}
	s.tools[def.Name] = registeredTool{def: def, handler: h}
	return nil
}

// RegisterResource is REQ-MCP-SERVER-05's mechanism.
func (s *Server) RegisterResource(r Resource, h ResourceHandler) error {
	if r.URI == "" {
		return errors.New("mcp: a registered resource needs a uri")
	}
	if h == nil {
		return fmt.Errorf("mcp: resource %q has no handler", r.URI)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.resources[r.URI]; !exists {
		s.resOrder = append(s.resOrder, r.URI)
	}
	s.resources[r.URI] = registeredResource{res: r, handler: h}
	return nil
}

// serverSession is one connection's state: the transport, and the correlator
// for requests the SERVER issues to the client (sampling).
type serverSession struct {
	tr     Transport
	corr   *correlator
	sendMu sync.Mutex
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
	sess := &serverSession{tr: tr, corr: newCorrelator()}
	ctx = context.WithValue(ctx, sessionKey{}, sess)

	var wg sync.WaitGroup
	// Every in-flight handler is joined before returning: a handler still
	// writing to a transport its caller believes is finished is how a shutdown
	// turns into a write to a closed pipe.
	defer func() {
		sess.corr.fail(ErrTransportClosed)
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
			continue // initialized, cancelled: nothing to answer
		case m.IsResponse():
			if !sess.corr.deliver(&m) {
				s.warnf("client sent a response for unknown id %s", m.ID)
			}
			continue
		}

		wg.Add(1)
		go func(m Message) {
			defer wg.Done()
			result, rpcErr := s.dispatch(ctx, &m)
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

func (sess *serverSession) send(m Message) {
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	_ = sess.tr.Send(raw)
}

// Dispatch handles one request and returns the result or a JSON-RPC error. It
// is exported so the HTTP mode can share it with the stdio loop.
func (s *Server) dispatch(ctx context.Context, m *Message) (any, *Error) {
	switch m.Method {
	case MethodInitialize:
		var p InitializeParams
		if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		caps := ServerCapabilities{}
		caps.Tools = &struct {
			ListChanged bool `json:"listChanged,omitzero"`
		}{ListChanged: true}
		caps.Resources = &struct {
			Subscribe   bool `json:"subscribe,omitzero"`
			ListChanged bool `json:"listChanged,omitzero"`
		}{}
		return InitializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    caps,
			ServerInfo:      s.opts.Info,
		}, nil

	case MethodPing:
		return struct{}{}, nil

	case MethodToolsList:
		s.mu.RLock()
		defer s.mu.RUnlock()
		out := ToolsListResult{Tools: make([]ToolDefinition, 0, len(s.toolOrder))}
		for _, n := range s.toolOrder {
			out.Tools = append(out.Tools, s.tools[n].def)
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
		s.mu.RLock()
		defer s.mu.RUnlock()
		out := ResourcesListResult{Resources: make([]Resource, 0, len(s.resOrder))}
		for _, u := range s.resOrder {
			out.Resources = append(out.Resources, s.resources[u].res)
		}
		return out, nil

	case MethodResourcesRead:
		var p ResourcesReadParams
		if err := decodeParams(m.Params, &p, s.opts.Limits); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		s.mu.RLock()
		r, ok := s.resources[p.URI]
		s.mu.RUnlock()
		if !ok {
			return nil, Errorf(CodeInvalidParams, "no resource at %q", p.URI)
		}
		res, err := r.handler(ctx, p.URI)
		if err != nil {
			return nil, Errorf(CodeInternalError, "%v", err)
		}
		return res, nil
	}
	return nil, Errorf(CodeMethodNotFound, "method %q is not implemented", m.Method)
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
