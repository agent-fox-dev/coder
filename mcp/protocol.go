package mcp

import (
	"encoding/json"

	"github.com/agentfox/agentkit-go/wire"
)

// ProtocolVersion is the ONLY version this implementation speaks
// (REQ-MCP-SERVER-06, amended in PRD 0.4.0).
//
// 2026-07-28 is not a version bump over 2025-03-26: it removes the
// initialize/initialized handshake, protocol-level sessions, ping, the HTTP
// GET stream and server-initiated requests. The spec calls an implementation
// that speaks both eras "dual-era"; this one is modern-only by explicit
// product decision, and the cost is stated where a reader will hit it: a
// server that has not migrated cannot be talked to at all.
const ProtocolVersion = "2026-07-28"

// SupportedProtocolVersions is the negotiation table, newest first.
//
// A function rather than a package-level slice: an exported slice is writable
// by anything that imports the package, and a caller that appended a version
// we do not actually speak would make the server agree to it.
func SupportedProtocolVersions() []string { return []string{ProtocolVersion} }

// Supports reports whether a version is one we speak.
func Supports(v string) bool {
	for _, s := range SupportedProtocolVersions() {
		if s == v {
			return true
		}
	}
	return false
}

// Method names.
//
// There is no `initialize`, no `notifications/initialized` and no `ping`:
// 2026-07-28 removed all three. MethodInitialize survives as a constant
// ONLY so the server can recognise a legacy client and answer it with a
// diagnostic instead of "method not found" — a legacy client has no
// fall-forward mechanism, so that message is the only thing its user sees.
const (
	MethodDiscover              = "server/discover"
	MethodToolsList             = "tools/list"
	MethodToolsCall             = "tools/call"
	MethodResourcesList         = "resources/list"
	MethodResourcesRead         = "resources/read"
	MethodResourceTemplatesList = "resources/templates/list"
	MethodSubscriptionsListen   = "subscriptions/listen"

	MethodToolsChanged     = "notifications/tools/list_changed"
	MethodResourcesChanged = "notifications/resources/list_changed"
	MethodResourcesUpdated = "notifications/resources/updated"
	MethodCancelled        = "notifications/cancelled"
	MethodSubscriptionsAck = "notifications/subscriptions/acknowledged"

	// MethodInitialize is recognised, never implemented. See above.
	MethodInitialize = "initialize"

	// MethodSampling is no longer a request a server SENDS. It survives as the
	// `method` inside an InputRequiredResult's inputRequests map, which is
	// where sampling now lives (MRTR).
	MethodSampling = "sampling/createMessage"
)

// The reserved `_meta` keys of 2026-07-28. They are namespaced strings rather
// than plain field names, so they are constants here and the structs below
// carry the tags.
const (
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaLogLevel           = "io.modelcontextprotocol/logLevel"
	MetaServerInfo         = "io.modelcontextprotocol/serverInfo"
	MetaSubscriptionID     = "io.modelcontextprotocol/subscriptionId"

	// MetaKey is the params member the above live under.
	MetaKey = "_meta"
)

// ResultType is 2026-07-28's required discriminator on every result.
type ResultType string

const (
	ResultComplete      ResultType = "complete"
	ResultInputRequired ResultType = "input_required"
)

// RequestMeta is the per-request `_meta` that replaced the handshake.
//
// Protocol version and client capabilities are REQUIRED on every request.
// That is the whole design: there is no session to have agreed them once, so
// a request that omits them is not interpretable and the server rejects it.
type RequestMeta struct {
	ProtocolVersion    string             `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         *Implementation    `json:"io.modelcontextprotocol/clientInfo,omitzero"`
	ClientCapabilities ClientCapabilities `json:"io.modelcontextprotocol/clientCapabilities"`
	LogLevel           string             `json:"io.modelcontextprotocol/logLevel,omitzero"`
	ProgressToken      any                `json:"progressToken,omitzero"`
}

// ResultMeta is the typed VIEW of a result's `_meta`: the reserved keys this
// implementation reads or writes.
//
// Results carry `_meta` as json.RawMessage rather than as this struct. The
// member is an OPEN namespace by spec — a server may put its own keys in it —
// and binding it strictly (REQ-SEC-12.1) would reject a conforming server for
// the crime of adding one. The raw bytes have already passed REQ-SEC-11's
// bounds and duplicate-key check as part of the frame, so reading the known
// keys out of them leniently is safe; ParseResultMeta does that.
type ResultMeta struct {
	ServerInfo     *Implementation `json:"io.modelcontextprotocol/serverInfo,omitzero"`
	SubscriptionID *ID             `json:"io.modelcontextprotocol/subscriptionId,omitzero"`
}

// ParseResultMeta reads the reserved keys out of a result's `_meta`. Keys it
// does not know are ignored, which is what an open namespace requires; an
// undecodable payload yields the zero value rather than an error, because the
// result it came with has already been accepted.
func ParseResultMeta(raw json.RawMessage) ResultMeta {
	var m ResultMeta
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

// JSON renders the view for the wire.
func (m ResultMeta) JSON() json.RawMessage {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return raw
}

// CacheHints are 2026-07-28's ttlMs/cacheScope, required on every list result
// and on resources/read.
//
// They are a freshness hint, not an invalidation mechanism: they complement
// the list_changed notifications rather than replacing them, because a
// notification only reaches a client that opened a subscription stream.
type CacheHints struct {
	TTLMs      int64  `json:"ttlMs"`
	CacheScope string `json:"cacheScope,omitzero"`
}

// Cache scopes.
const (
	CachePublic  = "public"
	CachePrivate = "private"
)

// DefaultListTTLMs is what a server advertises when a host says nothing.
//
// Zero would be spec-legal and means "immediately stale", which is the
// behaviour of a server with no caching at all — the honest default for a
// registry a host can mutate at any moment via RegisterTool.
const DefaultListTTLMs = 0

// ---------------------------------------------------------------- discovery

type Implementation struct {
	Name       string          `json:"name"`
	Version    string          `json:"version"`
	Title      string          `json:"title,omitzero"`
	WebsiteURL string          `json:"websiteUrl,omitzero"`
	Icons      json.RawMessage `json:"icons,omitzero"`
}

// ClientCapabilities is what we tell a server we can do, on EVERY request.
//
// Sampling is advertised only when an embedder opted the server in
// (REQ-MCP-CLIENT-08). Advertising a capability we will then refuse to honour
// invites a server to build a flow around it and fail at the worst moment —
// and under MRTR that failure lands mid-call, after the server has already
// done the work that produced the input request.
type ClientCapabilities struct {
	// Sampling and Roots are DEPRECATED in 2026-07-28 with a twelve-month
	// window. Kept because REQ-MCP-CLIENT-08 names sampling; nothing new is
	// built on either.
	Sampling *struct{} `json:"sampling,omitzero"`
	Roots    *struct {
		ListChanged bool `json:"listChanged,omitzero"`
	} `json:"roots,omitzero"`
	Elicitation  json.RawMessage `json:"elicitation,omitzero"`
	Tasks        json.RawMessage `json:"tasks,omitzero"`
	Extensions   map[string]any  `json:"extensions,omitzero"`
	Experimental map[string]any  `json:"experimental,omitzero"`
}

// ServerCapabilities models every capability key the revision defines, not
// only the ones this client acts on. The struct is bound strictly
// (REQ-SEC-12.1), so a key missing here is not "ignored" but a rejection of
// the whole server/discover result — and a server advertising `logging` would
// have been unusable for advertising something we merely do not use.
type ServerCapabilities struct {
	Tools *struct {
		ListChanged bool `json:"listChanged,omitzero"`
	} `json:"tools,omitzero"`
	Resources *struct {
		Subscribe   bool `json:"subscribe,omitzero"`
		ListChanged bool `json:"listChanged,omitzero"`
	} `json:"resources,omitzero"`
	Prompts *struct {
		ListChanged bool `json:"listChanged,omitzero"`
	} `json:"prompts,omitzero"`
	Completions  json.RawMessage `json:"completions,omitzero"`
	Logging      json.RawMessage `json:"logging,omitzero"`
	Tasks        json.RawMessage `json:"tasks,omitzero"`
	Extensions   map[string]any  `json:"extensions,omitzero"`
	Experimental map[string]any  `json:"experimental,omitzero"`
}

// RequestParams is the `_meta` every request carries, embedded by each params
// type.
//
// It is declared on the structs rather than tolerated by the decoder because
// REQ-SEC-12.1 rejects unknown properties on a protocol payload: a field the
// struct does not name is a field that reached us without being modelled, and
// `_meta` is now on every single request. Its CONTENTS are raw, for the same
// reason as ResultMeta: the namespace is open, and the server reads the one
// key it needs (the protocol version) with its own bounded probe.
type RequestParams struct {
	Meta json.RawMessage `json:"_meta,omitzero"`
}

// DiscoverParams is server/discover's request. It carries only `_meta`.
type DiscoverParams struct {
	RequestParams
}

// DiscoverResult is what replaced InitializeResult.
//
// The difference that matters is `supportedVersions` PLURAL. The handshake
// negotiated one version and both sides remembered it; discovery reports the
// whole set and settles nothing, because each later request names its own
// version and is accepted or rejected on its own.
type DiscoverResult struct {
	ResultType        ResultType         `json:"resultType"`
	SupportedVersions []string           `json:"supportedVersions"`
	Capabilities      ServerCapabilities `json:"capabilities"`
	Instructions      string             `json:"instructions,omitzero"`
	// ServerInfo is the identity a server MAY carry at the top level as well
	// as under _meta; both are accepted so neither form is a rejection.
	ServerInfo *Implementation `json:"serverInfo,omitzero"`
	Meta       json.RawMessage `json:"_meta,omitzero"`
	CacheHints
}

// Validate is REQ-SEC-12.3's hook.
func (r DiscoverResult) Validate() error {
	if len(r.SupportedVersions) == 0 {
		return errMissing("supportedVersions")
	}
	return nil
}

// UnsupportedVersionData is the payload of a -32022 error.
//
// The `supported` list is the whole point: it is what lets a client pick a
// mutually supported version and retry, which is the only negotiation the
// protocol still has.
type UnsupportedVersionData struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

// ---------------------------------------------------------------- MRTR

// InputRequiredResult is 2026-07-28's replacement for a server-initiated
// request (REQ-MCP-CLIENT-08, amended in 0.4.0).
//
// The server cannot ask the client anything mid-call any more. It returns
// this instead, and the client answers by RETRYING the original request with
// the answers attached — which is why requestState exists: the server has to
// reconstruct where it was.
type InputRequiredResult struct {
	ResultType    ResultType                 `json:"resultType"`
	InputRequests map[string]json.RawMessage `json:"inputRequests,omitzero"`
	// RequestState is OPAQUE. A client that parses it is reading a server's
	// private state and will break when the server changes it.
	RequestState string          `json:"requestState,omitzero"`
	Meta         json.RawMessage `json:"_meta,omitzero"`
}

// InputRequest is one entry of inputRequests: a JSON-RPC request object the
// server would formerly have sent.
type InputRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitzero"`
}

// ---------------------------------------------------------------- tools

// The structs below name EVERY optional member 2026-07-28 defines on them,
// including the ones this build never reads (outputSchema, icons, ...). That
// is not completeness for its own sake: the binder is strict (REQ-SEC-12.1),
// so a member absent here is not ignored, it fails the whole message — and a
// conforming server sending outputSchema on one tool would have made every
// tool on that server unusable. Open-namespace members (`_meta`, annotations,
// icons) are carried as json.RawMessage for the reason ResultMeta gives.

type ToolDefinition struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitzero"`
	InputSchema  json.RawMessage `json:"inputSchema,omitzero"`
	OutputSchema json.RawMessage `json:"outputSchema,omitzero"`
	Annotations  json.RawMessage `json:"annotations,omitzero"`
	Title        string          `json:"title,omitzero"`
	Icons        json.RawMessage `json:"icons,omitzero"`
	Meta         json.RawMessage `json:"_meta,omitzero"`
}

func (t ToolDefinition) Validate() error {
	if t.Name == "" {
		return errMissing("name")
	}
	return nil
}

type ToolsListParams struct {
	RequestParams
	Cursor string `json:"cursor,omitzero"`
}

type ToolsListResult struct {
	ResultType ResultType       `json:"resultType"`
	Tools      []ToolDefinition `json:"tools"`
	NextCursor string           `json:"nextCursor,omitzero"`
	Meta       json.RawMessage  `json:"_meta,omitzero"`
	CacheHints
}

type ToolsCallParams struct {
	RequestParams
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitzero"`
	// InputResponses and RequestState carry an MRTR retry (REQ-MCP-CLIENT-08).
	// They are absent on a first attempt and present on a retry answering an
	// InputRequiredResult.
	InputResponses map[string]any `json:"inputResponses,omitzero"`
	RequestState   string         `json:"requestState,omitzero"`
}

// Content is one item of a tool result.
//
// Type is NOT validated: a content type this build does not know (audio, say)
// is a capability we lack, not a message to reject — the same argument
// REQ-SESS-05.2 makes about unknown entry types — and it is carried through
// with whatever modelled members it uses. Only an unknown MEMBER is rejected
// (REQ-SEC-12.1), which is why the union of every content type's members is
// declared here: text, image and audio (data, mimeType), embedded resource,
// and resource_link, whose members are those of Resource.
type Content struct {
	Type        string          `json:"type"`
	Text        string          `json:"text,omitzero"`
	Data        string          `json:"data,omitzero"`
	MimeType    string          `json:"mimeType,omitzero"`
	Resource    json.RawMessage `json:"resource,omitzero"`
	URI         string          `json:"uri,omitzero"`
	Name        string          `json:"name,omitzero"`
	Title       string          `json:"title,omitzero"`
	Description string          `json:"description,omitzero"`
	Size        *int64          `json:"size,omitzero"`
	Icons       json.RawMessage `json:"icons,omitzero"`
	Annotations json.RawMessage `json:"annotations,omitzero"`
	Meta        json.RawMessage `json:"_meta,omitzero"`
}

type ToolsCallResult struct {
	ResultType ResultType `json:"resultType"`
	Content    []Content  `json:"content"`
	// IsError is the PROTOCOL-level tool failure. It is distinct from a
	// JSON-RPC error: a tool that failed is a result the model should see and
	// react to, where a JSON-RPC error is a call that never happened.
	IsError           bool            `json:"isError,omitzero"`
	StructuredContent json.RawMessage `json:"structuredContent,omitzero"`
	Meta              json.RawMessage `json:"_meta,omitzero"`
}

// ---------------------------------------------------------------- resources

type Resource struct {
	URI         string          `json:"uri"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitzero"`
	Description string          `json:"description,omitzero"`
	MimeType    string          `json:"mimeType,omitzero"`
	Size        *int64          `json:"size,omitzero"`
	Icons       json.RawMessage `json:"icons,omitzero"`
	Annotations json.RawMessage `json:"annotations,omitzero"`
	Meta        json.RawMessage `json:"_meta,omitzero"`
}

func (r Resource) Validate() error {
	if r.URI == "" {
		return errMissing("uri")
	}
	return nil
}

// ResourcesListParams carries the pagination cursor.
type ResourcesListParams struct {
	RequestParams
	Cursor string `json:"cursor,omitzero"`
}

type ResourcesListResult struct {
	ResultType ResultType      `json:"resultType"`
	Resources  []Resource      `json:"resources"`
	NextCursor string          `json:"nextCursor,omitzero"`
	Meta       json.RawMessage `json:"_meta,omitzero"`
	CacheHints
}

type ResourcesReadParams struct {
	RequestParams
	URI string `json:"uri"`
}

type ResourceContents struct {
	URI      string          `json:"uri"`
	MimeType string          `json:"mimeType,omitzero"`
	Text     string          `json:"text,omitzero"`
	Blob     string          `json:"blob,omitzero"`
	Meta     json.RawMessage `json:"_meta,omitzero"`
}

type ResourcesReadResult struct {
	ResultType ResultType         `json:"resultType"`
	Contents   []ResourceContents `json:"contents"`
	Meta       json.RawMessage    `json:"_meta,omitzero"`
	CacheHints
}

// ---------------------------------------------------------------- sampling

type SamplingMessage struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

type SamplingParams struct {
	Messages         []SamplingMessage `json:"messages"`
	SystemPrompt     string            `json:"systemPrompt,omitzero"`
	MaxTokens        int               `json:"maxTokens,omitzero"`
	Temperature      *float64          `json:"temperature,omitzero"`
	IncludeContext   string            `json:"includeContext,omitzero"`
	StopSequences    []string          `json:"stopSequences,omitzero"`
	ModelPreferences json.RawMessage   `json:"modelPreferences,omitzero"`
	Metadata         json.RawMessage   `json:"metadata,omitzero"`
	Meta             json.RawMessage   `json:"_meta,omitzero"`
}

type SamplingResult struct {
	Role       string          `json:"role"`
	Content    Content         `json:"content"`
	Model      string          `json:"model"`
	StopReason string          `json:"stopReason,omitzero"`
	Meta       json.RawMessage `json:"_meta,omitzero"`
}

// ---------------------------------------------------------------- subscriptions

// SubscriptionFilter is what a client opts IN to.
//
// A server MUST NOT send a type that was not requested. That is stricter than
// the old model, where opening the GET stream subscribed you to everything the
// server felt like sending, and it is the reason this is a struct of explicit
// bools rather than a "send me everything" flag.
type SubscriptionFilter struct {
	ToolsListChanged      bool     `json:"toolsListChanged,omitzero"`
	PromptsListChanged    bool     `json:"promptsListChanged,omitzero"`
	ResourcesListChanged  bool     `json:"resourcesListChanged,omitzero"`
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitzero"`
}

// Any reports whether the filter asks for anything at all. A listen request
// that opted into nothing would hold a stream open forever to deliver nothing.
func (f SubscriptionFilter) Any() bool {
	return f.ToolsListChanged || f.PromptsListChanged ||
		f.ResourcesListChanged || len(f.ResourceSubscriptions) > 0
}

type SubscriptionsListenParams struct {
	RequestParams
	Notifications SubscriptionFilter `json:"notifications"`
}

// SubscriptionsListenResult is sent only when the SERVER tears the
// subscription down gracefully. An abrupt transport close carries no result,
// which is why a client cannot treat its absence as an error.
type SubscriptionsListenResult struct {
	ResultType ResultType      `json:"resultType"`
	Meta       json.RawMessage `json:"_meta,omitzero"`
}

// ---------------------------------------------------------------- decoding

// decodeParams binds a params payload strictly (REQ-SEC-12).
//
// This is a PROTOCOL payload, so the full strictness applies — including
// unknown-property rejection, which PRD 0.3.4 scoped away from provider
// responses and deliberately left in place here. The difference is what
// happens to the decoded value: an MCP tool result flows into a handler and a
// tool definition flows into the prompt, so a smuggled field reaches something.
func decodeParams(raw []byte, target any, limits wire.Limits) error {
	if len(raw) == 0 {
		return nil
	}
	v, err := wire.Parse(raw, limits)
	if err != nil {
		return err
	}
	return wire.Bind(v, target)
}

func errMissing(field string) error { return &missingField{field} }

type missingField struct{ field string }

func (m *missingField) Error() string { return "missing required field " + m.field }
