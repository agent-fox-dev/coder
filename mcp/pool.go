package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
	"github.com/agentfox/agentkit-go/wire"
)

// UnresolvedVariableError is NFR-SEC-03's configuration error: a server's env
// or headers referenced a ${VAR} that resolved to nothing.
//
// It is an ERROR, not a warning with a blank substituted, because the two
// outcomes of substituting a blank are both worse than refusing. The child
// starts with an empty credential and fails authentication with a message
// about a bad token, which sends the reader looking at the token; or the
// header is dropped and the 401 explains nothing at all. Neither names the
// variable. This does, and it names every one at once so the operator fixes
// them in one pass rather than one per restart.
//
// A variable set to the EMPTY STRING is not unresolved: that is a value the
// operator chose. Only an absent one is.
type UnresolvedVariableError struct {
	Server    string
	Variables []string
}

func (e *UnresolvedVariableError) Error() string {
	return fmt.Sprintf("mcp: server %q references unset variable(s) %s; unexpanded "+
		"references are a configuration error (NFR-SEC-03)", e.Server, e.namesList())
}

func (e *UnresolvedVariableError) namesList() string {
	names := make([]string, len(e.Variables))
	for i, v := range e.Variables {
		names[i] = "${" + v + "}"
	}
	return strings.Join(names, ", ")
}

// Pool is REQ-MCP-CLIENT-04: server_name -> connection, built during session
// initialization and torn down when the session ends.
type Pool struct {
	// NativeTools names the host's OWN tools, which no MCP server may shadow
	// (REQ-MCP-CLIENT-06). The embedder populates it before connecting —
	// it is the only party that knows what its native tool set is called.
	//
	// It exists because the check in Tools is not enough on its own: a host
	// that connects its servers and never calls Tools (it drives connections
	// directly, or assembles its tool list some other way) would ship a server
	// whose `read_file` silently stands in front of the SDK's, and find out
	// when the model called it. With this set, the same misconfiguration is a
	// refused connection at startup.
	NativeTools []string

	mu    sync.Mutex
	conns map[string]*ServerConnection
	order []string
	opts  ConnectionOptions
}

func NewPool(opts ConnectionOptions) *Pool {
	return &Pool{conns: map[string]*ServerConnection{}, opts: opts}
}

// Add registers an already-connected server.
func (p *Pool) Add(c *ServerConnection) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.conns[c.Name()]; dup {
		return fmt.Errorf("mcp: two servers are named %q", c.Name())
	}
	p.conns[c.Name()] = c
	p.order = append(p.order, c.Name())
	return nil
}

// Connect opens a server and initializes it (REQ-MCP-CLIENT-02).
//
// A `command` server is spawned as a subprocess over stdio; a `url` server is
// opened over Streamable HTTP. env is the reduced environment for a child
// (REQ-MCP-CLIENT-10), and secrets resolves ${VAR} references at CONNECT time
// — so a credential lives in the child's environment or in a request header,
// and never in the config file, the process table, or a log of the command
// line. A reference that resolves to nothing is an *UnresolvedVariableError
// and nothing is spawned (NFR-SEC-03).
//
// The connection it returns reconnects on its own (NFR-REL-03): a stdio server
// that exits is re-spawned and an HTTP transport that died is re-opened, at
// the next call and at most PerSessionReconnectLimit times. A stdio server is
// also subscribed to tools/list_changed for the life of the connection
// (REQ-CACHE-07); an HTTP server is not — see ServerConfig.URL.
func (p *Pool) Connect(ctx context.Context, cfg ServerConfig, env []string, secrets func(string) string) (*ServerConnection, error) {
	switch {
	case cfg.Command != "":
	case cfg.URL != "":
		return p.connectHTTP(ctx, cfg, env, secrets)
	default:
		return nil, fmt.Errorf("mcp: server %q has neither a command nor a url", cfg.Name)
	}
	childEnv, missing := resolveEnv(cfg.Env, env, secrets)
	if len(missing) > 0 {
		return nil, &UnresolvedVariableError{Server: cfg.Name, Variables: missing}
	}

	stdioOpts := StdioOptions{
		Command: cfg.Command, Args: cfg.Args, Dir: cfg.Dir, Env: childEnv,
		Limits: p.opts.Limits,
		Stderr: func(line string) {
			if p.opts.Warnf != nil {
				p.opts.Warnf("server %q: %s", cfg.Name, line)
			}
		},
	}
	tr, err := StartStdio(ctx, stdioOpts)
	if err != nil {
		return nil, err
	}

	c := NewConnection(cfg, tr, p.opts)
	if err := c.Discover(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := p.refuseShadowedNames(ctx, c); err != nil {
		_ = c.Close()
		return nil, err
	}
	// Reconnection is armed only AFTER discovery succeeded. A server that
	// dies while being discovered is misconfigured, and re-spawning it three
	// more times would report the same failure three times later.
	c.setDial(func(ctx context.Context) (Transport, error) { return StartStdio(ctx, stdioOpts) })
	c.keepToolsSubscribed()
	if err := p.Add(c); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// connectHTTP opens a remote server.
//
// The headers are resolved through the SAME ${VAR} path as a subprocess's
// environment, so `Authorization = "Bearer ${GH_TOKEN}"` in a config file
// carries a reference and not a token.
func (p *Pool) connectHTTP(ctx context.Context, cfg ServerConfig, env []string, secrets func(string) string) (*ServerConnection, error) {
	headers, missing, dropped := resolveHeaders(cfg.Headers, env, secrets)
	if len(missing) > 0 {
		return nil, &UnresolvedVariableError{Server: cfg.Name, Variables: missing}
	}
	for _, name := range dropped {
		if p.opts.Warnf != nil {
			p.opts.Warnf("server %q: header %q resolved to a blank or unsafe value and was "+
				"not sent", cfg.Name, name)
		}
	}

	httpOpts := HTTPTransportOptions{
		URL: cfg.URL, Headers: headers,
		Limits: p.opts.Limits, Warnf: func(format string, args ...any) {
			if p.opts.Warnf != nil {
				p.opts.Warnf("server %q: "+format, append([]any{cfg.Name}, args...)...)
			}
		},
	}
	tr, err := StartStreamableHTTP(ctx, httpOpts)
	if err != nil {
		return nil, err
	}

	c := NewConnection(cfg, tr, p.opts)
	if err := c.Discover(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := p.refuseShadowedNames(ctx, c); err != nil {
		_ = c.Close()
		return nil, err
	}
	c.setDial(func(ctx context.Context) (Transport, error) { return StartStreamableHTTP(ctx, httpOpts) })
	if err := p.Add(c); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// Get returns a connection by server name.
func (p *Pool) Get(name string) (*ServerConnection, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.conns[name]
	return c, ok
}

// Names returns the server names in connection order.
func (p *Pool) Names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.order...)
}

// Close tears down every connection (REQ-MCP-CLIENT-04).
//
// It closes ALL of them even when one fails, and returns the failures joined.
// Stopping at the first error would leave the remaining subprocesses running
// for the life of the host — a leaked MCP server is a leaked process tree.
func (p *Pool) Close() error {
	p.mu.Lock()
	conns := make([]*ServerConnection, 0, len(p.conns))
	for _, n := range p.order {
		conns = append(conns, p.conns[n])
	}
	p.conns, p.order = map[string]*ServerConnection{}, nil
	p.mu.Unlock()

	var errs []string
	for _, c := range conns {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", c.Name(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("mcp: closing servers: %s", strings.Join(errs, "; "))
	}
	return nil
}

// refuseShadowedNames is REQ-MCP-CLIENT-06 raised at CONNECTION time.
//
// A shadowed native tool is a misconfiguration, and the cost of noticing it
// late is not a confusing error — it is the WRONG TOOL having run, because the
// model called `read_file` and a server answered. So the server's tool list is
// pulled once, here, while the connection can still be torn down, rather than
// at the first call.
//
// A server is listed only when there is something to shadow: with no native
// names declared this costs nothing, and a host that has not told the pool
// what its tools are called keeps the behaviour it had before.
func (p *Pool) refuseShadowedNames(ctx context.Context, c *ServerConnection) error {
	native := p.nativeNames()
	if len(native) == 0 {
		return nil
	}
	defs, err := c.ListTools(ctx)
	if err != nil {
		// Not verifiable is not the same as fine: a server whose list failed
		// may expose anything, and the connection has not been added yet.
		return fmt.Errorf("mcp: %s: listing tools to check for name collisions: %w",
			c.Name(), err)
	}
	sort.SliceStable(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	for _, d := range defs {
		if qualified := QualifiedName(c.cfg, d.Name); native[qualified] {
			return fmt.Errorf("%w: server %q exposes %q as %q, which is already a native tool",
				ErrNameCollision, c.Name(), d.Name, qualified)
		}
	}
	return nil
}

// nativeNames snapshots the declared native tool names as a set.
func (p *Pool) nativeNames() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.NativeTools) == 0 {
		return nil
	}
	out := make(map[string]bool, len(p.NativeTools))
	for _, n := range p.NativeTools {
		out[n] = true
	}
	return out
}

// Tools adapts every connected server's tools into core.Tools with qualified
// names (REQ-MCP-CLIENT-05).
//
// existing is the native tool set. A qualified name that collides with one —
// or with a name in NativeTools, or with another server's — is
// ErrNameCollision (REQ-MCP-CLIENT-06). This is the BACKSTOP: the collision is
// raised at connect (refuseShadowedNames) for everything the pool knew about
// then, and again here for the native tools a host registers afterwards and
// for the tools a server grows during the session, neither of which a
// connect-time check can see.
func (p *Pool) Tools(ctx context.Context, existing []core.Tool) ([]core.Tool, error) {
	taken := make(map[string]string, len(existing))
	for name := range p.nativeNames() {
		taken[name] = "a native tool"
	}
	for _, t := range existing {
		taken[t.Name] = "a native tool"
	}

	var out []core.Tool
	for _, name := range p.Names() {
		c, ok := p.Get(name)
		if !ok {
			continue
		}
		defs, err := c.ListTools(ctx)
		if err != nil {
			return nil, err
		}
		sort.SliceStable(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })

		for _, d := range defs {
			qualified := QualifiedName(c.cfg, d.Name)
			if owner, dup := taken[qualified]; dup {
				return nil, fmt.Errorf("%w: server %q exposes %q as %q, which is already %s",
					ErrNameCollision, name, d.Name, qualified, owner)
			}
			taken[qualified] = fmt.Sprintf("server %q", name)
			out = append(out, p.adapt(c, d, qualified))
		}
	}
	return out, nil
}

// adapt turns one MCP tool definition into a core.Tool.
func (p *Pool) adapt(c *ServerConnection, d ToolDefinition, qualified string) core.Tool {
	unqualified := d.Name
	return core.Tool{
		Name:        qualified,
		Description: d.Description,
		// MCPServer is set so REQ-OBS-05's audit does not have to guess the
		// server from a name whose prefix is configurable.
		MCPServer:   c.Name(),
		InputSchema: schemaFrom(d.InputSchema),
		Execute: func(ctx context.Context, in json.RawMessage) core.ToolResult {
			args := map[string]any{}
			if len(in) > 0 {
				if err := json.Unmarshal(in, &args); err != nil {
					return core.ErrResult("invalid_arguments", err.Error())
				}
			}
			res, err := c.Call(ctx, unqualified, args)
			if err != nil {
				return core.ErrResult("mcp_call_failed", err.Error())
			}
			data := map[string]any{"content": contentToAny(res.Content)}
			if len(res.StructuredContent) > 0 {
				data["structured"] = json.RawMessage(res.StructuredContent)
			}
			if res.IsError {
				// A tool that FAILED is a result the model should see and
				// react to; only a call that never happened is an SDK error.
				return core.ToolResult{OK: false, Data: data, Error: "tool_error",
					Detail: contentText(res.Content)}
			}
			return core.OKResult(data)
		},
	}
}

func contentToAny(items []Content) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		m := map[string]any{"type": it.Type}
		if it.Text != "" {
			m["text"] = it.Text
		}
		if it.MimeType != "" {
			m["mimeType"] = it.MimeType
		}
		if it.Data != "" {
			m["data"] = it.Data
		}
		if it.URI != "" {
			m["uri"] = it.URI
		}
		out = append(out, m)
	}
	return out
}

func contentText(items []Content) string {
	var b strings.Builder
	for _, it := range items {
		if it.Type == "text" {
			b.WriteString(it.Text)
		}
	}
	return b.String()
}

// schemaFrom converts an MCP inputSchema into the typed combinator form.
//
// A schema this converter does not model becomes an OPEN object rather than a
// rejection. The alternative is refusing to expose a tool because its schema
// uses a keyword we have not implemented, which trades a tool the model could
// have used for a validation guarantee the server is applying anyway.
func schemaFrom(raw json.RawMessage) *schema.Schema {
	if len(raw) == 0 {
		return schema.Object()
	}
	v, err := wire.Parse(raw, wire.Limits{})
	if err != nil {
		return schema.Object()
	}
	return convertSchema(v, 0)
}

func convertSchema(v wire.Value, depth int) *schema.Schema {
	if depth > 16 || v.Kind != wire.KindObject {
		return schema.String()
	}
	desc := ""
	if d, ok := v.Get("description"); ok && d.Kind == wire.KindString {
		desc = d.String
	}

	typ := ""
	if t, ok := v.Get("type"); ok && t.Kind == wire.KindString {
		typ = t.String
	}

	switch typ {
	case "object", "":
		props, _ := v.Get("properties")
		required := map[string]bool{}
		if r, ok := v.Get("required"); ok && r.Kind == wire.KindArray {
			for _, e := range r.Array {
				if e.Kind == wire.KindString {
					required[e.String] = true
				}
			}
		}
		var fields []schema.Field
		for _, key := range props.Keys {
			sub := convertSchema(props.Object[key], depth+1)
			if required[key] {
				fields = append(fields, schema.Prop(key, sub))
				continue
			}
			fields = append(fields, schema.Opt(key, sub))
		}
		return schema.Object(fields...).Describe(desc)
	case "array":
		items, ok := v.Get("items")
		if !ok {
			return schema.Array(schema.String(), desc)
		}
		return schema.Array(convertSchema(items, depth+1), desc)
	case "integer":
		return schema.Int(desc)
	case "number":
		return schema.Number(desc)
	case "boolean":
		return schema.Bool(desc)
	}
	if e, ok := v.Get("enum"); ok && e.Kind == wire.KindArray {
		var vals []string
		for _, x := range e.Array {
			if x.Kind == wire.KindString {
				vals = append(vals, x.String)
			}
		}
		if len(vals) > 0 {
			return schema.Enum(desc, vals...)
		}
	}
	return schema.String(desc)
}

// resolveEnv builds the child's environment (REQ-MCP-CLIENT-10).
func resolveEnv(declared map[string]string, base []string, secrets func(string) string) ([]string, []string) {
	lookup := envLookup(base, secrets)

	out := append([]string(nil), base...)
	var missing []string
	keys := make([]string, 0, len(declared))
	for k := range declared {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic, so a child's environment is reproducible
	for _, k := range keys {
		v, miss := interpolate(declared[k], lookup)
		missing = append(missing, miss...)
		out = append(out, k+"="+v)
	}
	return out, missing
}

// envLookup is the one resolution order for ${VAR}: the secrets store first,
// then the reduced environment. Sharing it is what keeps a header and a child
// environment from resolving the same reference differently.
//
// It reports presence separately from value, because NFR-SEC-03 makes an
// unresolved reference an error and a variable set to "" is not unresolved.
// The secrets function cannot express absence, so a store answering "" falls
// through to the environment, where an explicitly empty variable counts as
// set.
func envLookup(base []string, secrets func(string) string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if secrets != nil {
			if v := secrets(name); v != "" {
				return v, true
			}
		}
		for _, kv := range base {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				return v, true
			}
		}
		return "", false
	}
}

// resolveHeaders expands ${VAR} in header values.
//
// missing names the references that resolved to nothing; the caller makes
// that a configuration error (NFR-SEC-03). dropped names the headers whose
// RESOLVED value could not be sent: blank, or carrying a control byte. A
// control byte in a header is a request-splitting attempt, and it can arrive
// through an interpolated secret rather than through the config file — so
// the header is withheld and reported rather than handed to net/http to
// reject with an opaque error.
func resolveHeaders(declared map[string]string, base []string, secrets func(string) string) (headers map[string]string, missing, dropped []string) {
	if len(declared) == 0 {
		return nil, nil, nil
	}
	lookup := envLookup(base, secrets)
	out := make(map[string]string, len(declared))
	seen := map[string]bool{}
	keys := make([]string, 0, len(declared))
	for k := range declared {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		expanded, names := interpolate(declared[k], lookup)
		for _, n := range names {
			if !seen[n] {
				seen[n], missing = true, append(missing, n)
			}
		}
		if len(names) > 0 {
			continue
		}
		if strings.TrimSpace(expanded) == "" || !isHeaderSafe(k) || !isHeaderSafe(expanded) {
			dropped = append(dropped, k)
			continue
		}
		out[k] = expanded
	}
	if len(out) == 0 {
		return nil, missing, dropped
	}
	return out, missing, dropped
}
