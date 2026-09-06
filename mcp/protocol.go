package mcp

import (
	"encoding/json"

	"github.com/agentfox/agentkit-go/wire"
)

// ProtocolVersion is the version this implementation speaks (REQ-MCP-SERVER-06).
const ProtocolVersion = "2025-03-26"

// LegacyProtocolVersion is the HTTP/SSE-era version (REQ-MCP-CLIENT-02). It is
// accepted from a server during negotiation; nothing here downgrades to it
// voluntarily.
const LegacyProtocolVersion = "2024-11-05"

// Method names.
const (
	MethodInitialize    = "initialize"
	MethodInitialized   = "notifications/initialized"
	MethodToolsList     = "tools/list"
	MethodToolsCall     = "tools/call"
	MethodToolsChanged  = "notifications/tools/list_changed"
	MethodResourcesList = "resources/list"
	MethodResourcesRead = "resources/read"
	MethodSampling      = "sampling/createMessage"
	MethodPing          = "ping"
)

// ---------------------------------------------------------------- initialize

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCapabilities is what we tell a server we can do.
//
// Sampling is advertised only when an embedder opted the server in
// (REQ-MCP-CLIENT-08). Advertising a capability we will then refuse to honour
// invites a server to build a flow around it and fail at the worst moment.
type ClientCapabilities struct {
	Sampling *struct{} `json:"sampling,omitzero"`
	Roots    *struct {
		ListChanged bool `json:"listChanged,omitzero"`
	} `json:"roots,omitzero"`
}

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
	Logging *struct{} `json:"logging,omitzero"`
}

type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitzero"`
}

// Validate is REQ-SEC-12.3's hook. A server that answers `initialize` without
// naming a protocol version has told us nothing about what it speaks, and
// proceeding on a guess is how a capability mismatch turns into a confusing
// failure three calls later.
func (r InitializeResult) Validate() error {
	if r.ProtocolVersion == "" {
		return errMissing("protocolVersion")
	}
	return nil
}

// ---------------------------------------------------------------- tools

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitzero"`
	InputSchema json.RawMessage `json:"inputSchema,omitzero"`
	Annotations json.RawMessage `json:"annotations,omitzero"`
	Title       string          `json:"title,omitzero"`
}

func (t ToolDefinition) Validate() error {
	if t.Name == "" {
		return errMissing("name")
	}
	return nil
}

type ToolsListParams struct {
	Cursor string `json:"cursor,omitzero"`
}

type ToolsListResult struct {
	Tools      []ToolDefinition `json:"tools"`
	NextCursor string           `json:"nextCursor,omitzero"`
}

type ToolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitzero"`
}

// Content is one item of a tool result.
//
// Text, image and resource are modelled; anything else is retained as Raw. A
// content type this build does not know is a capability we lack, not a message
// to reject — the same argument REQ-SESS-05.2 makes about unknown entry types.
type Content struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitzero"`
	Data     string          `json:"data,omitzero"`
	MimeType string          `json:"mimeType,omitzero"`
	Resource json.RawMessage `json:"resource,omitzero"`
	URI      string          `json:"uri,omitzero"`
	Raw      json.RawMessage `json:"-"`
}

type ToolsCallResult struct {
	Content []Content `json:"content"`
	// IsError is the PROTOCOL-level tool failure. It is distinct from a
	// JSON-RPC error: a tool that failed is a result the model should see and
	// react to, where a JSON-RPC error is a call that never happened.
	IsError           bool            `json:"isError,omitzero"`
	StructuredContent json.RawMessage `json:"structuredContent,omitzero"`
}

// ---------------------------------------------------------------- resources

type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitzero"`
	Description string `json:"description,omitzero"`
	MimeType    string `json:"mimeType,omitzero"`
}

func (r Resource) Validate() error {
	if r.URI == "" {
		return errMissing("uri")
	}
	return nil
}

type ResourcesListResult struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitzero"`
}

type ResourcesReadParams struct {
	URI string `json:"uri"`
}

type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitzero"`
	Text     string `json:"text,omitzero"`
	Blob     string `json:"blob,omitzero"`
}

type ResourcesReadResult struct {
	Contents []ResourceContents `json:"contents"`
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
}

type SamplingResult struct {
	Role       string  `json:"role"`
	Content    Content `json:"content"`
	Model      string  `json:"model"`
	StopReason string  `json:"stopReason,omitzero"`
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
