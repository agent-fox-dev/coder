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

// Pool is REQ-MCP-CLIENT-04: server_name -> connection, built during session
// initialization and torn down when the session ends.
type Pool struct {
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
// opened over one of the two HTTP transports. env is the reduced environment
// for a child (REQ-MCP-CLIENT-10), and secrets resolves ${VAR} references at
// CONNECT time — so a credential lives in the child's environment or in a
// request header, and never in the config file, the process table, or a log of
// the command line.
func (p *Pool) Connect(ctx context.Context, cfg ServerConfig, env []string, secrets func(string) string) (*ServerConnection, error) {
	switch {
	case cfg.Command != "":
	case cfg.URL != "":
		return p.connectHTTP(ctx, cfg, env, secrets)
	default:
		return nil, fmt.Errorf("mcp: server %q has neither a command nor a url", cfg.Name)
	}
	childEnv, missing := resolveEnv(cfg.Env, env, secrets)
	for _, name := range missing {
		if p.opts.Warnf != nil {
			p.opts.Warnf("server %q: ${%s} is unset and expanded to empty; the child will "+
				"start with a blank value rather than the literal reference", cfg.Name, name)
		}
	}

	tr, err := StartStdio(ctx, StdioOptions{
		Command: cfg.Command, Args: cfg.Args, Dir: cfg.Dir, Env: childEnv,
		Limits: p.opts.Limits,
		Stderr: func(line string) {
			if p.opts.Warnf != nil {
				p.opts.Warnf("server %q: %s", cfg.Name, line)
			}
		},
	})
	if err != nil {
		return nil, err
	}

	c := NewConnection(cfg, tr, p.opts)
	if err := c.Initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
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
	headers, missing := resolveHeaders(cfg.Headers, env, secrets)
	for _, name := range missing {
		if p.opts.Warnf != nil {
			p.opts.Warnf("server %q: ${%s} is unset and expanded to empty; the request "+
				"header will carry a blank value rather than the literal reference",
				cfg.Name, name)
		}
	}

	tr, err := StartHTTP(ctx, HTTPTransportOptions{
		URL: cfg.URL, Mode: cfg.Transport, Headers: headers,
		Limits: p.opts.Limits, Warnf: func(format string, args ...any) {
			if p.opts.Warnf != nil {
				p.opts.Warnf("server %q: "+format, append([]any{cfg.Name}, args...)...)
			}
		},
	})
	if err != nil {
		return nil, err
	}

	c := NewConnection(cfg, tr, p.opts)
	if err := c.Initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
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

// Tools adapts every connected server's tools into core.Tools with qualified
// names (REQ-MCP-CLIENT-05).
//
// existing is the native tool set. A qualified name that collides with one is
// ErrNameCollision (REQ-MCP-CLIENT-06), raised HERE — at connection time —
// rather than when the model happens to call it, because a shadowed native
// tool is a misconfiguration and discovering it at call time means
// discovering it in production.
func (p *Pool) Tools(ctx context.Context, existing []core.Tool) ([]core.Tool, error) {
	taken := make(map[string]string, len(existing))
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
func envLookup(base []string, secrets func(string) string) func(string) string {
	return func(name string) string {
		if secrets != nil {
			if v := secrets(name); v != "" {
				return v
			}
		}
		for _, kv := range base {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				return v
			}
		}
		return ""
	}
}

// resolveHeaders expands ${VAR} in header values, reporting the names that
// resolved to nothing.
//
// A header referencing a variable that did not resolve is DROPPED ENTIRELY,
// not sent with the gap filled in. `Bearer ${TOKEN}` with no TOKEN is
// `Bearer ` — literal scaffolding around a missing secret, which is not a
// weaker credential but a malformed request, and the 401 it earns tells an
// operator far less than the warning this returns. Checking the resolved
// string for emptiness would miss exactly this case, so the test is whether
// any REFERENCE went unresolved.
func resolveHeaders(declared map[string]string, base []string, secrets func(string) string) (map[string]string, []string) {
	if len(declared) == 0 {
		return nil, nil
	}
	lookup := envLookup(base, secrets)
	out := make(map[string]string, len(declared))
	var missing []string
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
		if len(names) > 0 || strings.TrimSpace(expanded) == "" {
			continue
		}
		if !isHeaderSafe(k) || !isHeaderSafe(expanded) {
			// A control byte in a header is a request-splitting attempt, and
			// it can arrive through an interpolated secret rather than through
			// the config file.
			if !seen[k] {
				seen[k], missing = true, append(missing, k)
			}
			continue
		}
		out[k] = expanded
	}
	if len(out) == 0 {
		return nil, missing
	}
	return out, missing
}
