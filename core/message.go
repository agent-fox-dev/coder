// Package core holds AgentKit's canonical vocabulary and every interface seam.
// Machinery lives elsewhere; core holds declarations, the small pure functions
// that are part of the contract, and the EventStream.
//
// core imports only jsonx and schema. Nothing in AgentKit below the root
// package may import the root package, which is what keeps SubagentTool
// (needs *Agent) and compaction (needs a model call) out of core.
package core

import (
	"encoding/json"
	"time"

	"github.com/agentfox/agentkit-go/jsonx"
)

type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolResult Role = "tool_result"
)

// Message is a sealed union: UserMessage | AssistantMessage | ToolResultMessage.
//
// An interface, not a Role-discriminated fat struct. REQ-LOOP-02 makes
// ToolResultMessage a first-class role whose ToolUseID, IsError, ToolName and
// Usage exist on no other role. A fat struct puts those fields on every
// message and makes "flatten results into a shared user message" — the mistake
// REQ-LOOP-02 and Appendix A#2 name — a one-line accident instead of a type
// error. Sealed by isMessage() so provider adapters' role switches stay total.
type Message interface {
	Role() Role
	Clone() Message
	isMessage()
}

type Messages []Message

func (ms Messages) Clone() Messages {
	if ms == nil {
		return nil
	}
	out := make(Messages, len(ms))
	for i, m := range ms {
		out[i] = m.Clone()
	}
	return out
}

type UserMessage struct {
	Content   Content
	Timestamp time.Time
	// Unknown holds top-level keys this build does not model, in source order,
	// so a re-encode is lossless (NFR-TEST-03).
	Unknown jsonx.OrderedObject
}

type AssistantMessage struct {
	Content Content

	// StopReason is the canonical enum; RawStopReason is the provider's own
	// finish string, verbatim. Neither may be read by the continuation check
	// (REQ-LOOP-01) — use ExtractToolUse.
	StopReason    StopReason
	RawStopReason string
	ErrorMessage  string
	Usage         Usage
	Timestamp     time.Time

	// Provenance (§5, "Provenance is not optional"). REQ-PROV-11 rule 1
	// computes same_model from exactly (Provider, API, Model). Model is the
	// REQUESTED model, never ResponseModel: a fallback-served turn judged
	// against ResponseModel would have its own valid signatures stripped.
	Provider      string
	API           API
	Model         string
	ResponseModel string
	ResponseID    string
	ThinkingLevel ThinkingLevel

	Unknown jsonx.OrderedObject
}

type ToolResultMessage struct {
	ToolUseID string
	ToolName  string
	Content   Content
	IsError   bool
	Timestamp time.Time
	// AddedToolNames marks tools that became available at this point in the
	// transcript (REQ-CACHE-10). Opaque to the loop.
	AddedToolNames []string
	Usage          *Usage // optional (§5); a pointer, never omitempty
	Unknown        jsonx.OrderedObject
}

func (UserMessage) Role() Role       { return RoleUser }
func (AssistantMessage) Role() Role  { return RoleAssistant }
func (ToolResultMessage) Role() Role { return RoleToolResult }

func (UserMessage) isMessage()       {}
func (AssistantMessage) isMessage()  {}
func (ToolResultMessage) isMessage() {}

func (m UserMessage) Clone() Message {
	m.Content = m.Content.Clone()
	m.Unknown = m.Unknown.Clone()
	return m
}

func (m AssistantMessage) Clone() Message {
	m.Content = m.Content.Clone()
	m.Unknown = m.Unknown.Clone()
	return m
}

func (m ToolResultMessage) Clone() Message {
	m.Content = m.Content.Clone()
	m.Unknown = m.Unknown.Clone()
	m.AddedToolNames = append([]string(nil), m.AddedToolNames...)
	if m.Usage != nil {
		u := *m.Usage
		m.Usage = &u
	}
	return m
}

// ---------------------------------------------------------------- content

type BlockType string

const (
	BlockText       BlockType = "text"
	BlockThinking   BlockType = "thinking"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockImage      BlockType = "image"
)

// ContentBlock is a sealed union. Sealed because every provider adapter
// switches exhaustively over it (REQ-PROV-03) and a third-party variant would
// make that switch silently incomplete. RawBlock covers forward compatibility.
type ContentBlock interface {
	BlockType() BlockType
	CloneBlock() ContentBlock
	isContentBlock()
}

type Content []ContentBlock

func (c Content) Clone() Content {
	if c == nil {
		return nil
	}
	out := make(Content, len(c))
	for i, b := range c {
		out[i] = b.CloneBlock()
	}
	return out
}

// Text concatenates every TextBlock, for RunResult.FinalText.
func (c Content) Text() string {
	var s string
	for _, b := range c {
		if t, ok := b.(TextBlock); ok {
			s += t.Text
		}
	}
	return s
}

// TextBlock holds a Go string, not []byte, deliberately: strings are
// immutable, so the deep copy REQ-OBS-06b demands at every Push is a header
// copy rather than a byte copy. Cloning a partial assistant message is
// therefore O(blocks), not O(streamed characters).
type TextBlock struct{ Text string }

type ThinkingBlock struct {
	Thinking string
	// Signature is provider-issued and opaque; it may be empty on an aborted
	// stream, which REQ-PROV-11 rule 4 keys on. Never inspected.
	Signature string
	Redacted  bool
}

type ToolUseBlock struct {
	ID   string
	Name string
	// Input is the provider's own argument bytes, verbatim and AUTHORITATIVE
	// for every replay, fingerprint and serialization path (§5, REQ-PROV-17).
	// Invariant: always syntactically valid JSON object bytes; NewToolUse
	// normalizes nil/empty to {}.
	Input json.RawMessage
	// InputOrder is the same value in order-preserving form, from the SAME
	// decode pass (REQ-TOOL-12).
	//
	// PRECEDENCE, because REQ-TOOL-12.1 and REQ-PROV-17 name different replay
	// sources and can differ: Input bytes WIN whenever present and unmodified.
	// InputOrder is the regeneration path, used only when the bytes must be
	// rebuilt — after PrepareArguments changed the value, after salvage repair
	// of a truncated stream, or by a wire format that re-encodes arguments as
	// a JSON string. A provider must never re-encode InputOrder when Input is
	// usable.
	InputOrder jsonx.OrderedObject
	// ThoughtSignature is opaque and provider-issued; stripped on cross-model
	// replay by REQ-PROV-11 rule 3. Never inspected.
	ThoughtSignature string
}

// NewToolUse decodes raw once, producing both authoritative forms. A nil,
// empty or whitespace-only raw yields {} and an empty InputOrder.
func NewToolUse(id, name string, raw json.RawMessage) (ToolUseBlock, error) {
	b := ToolUseBlock{ID: id, Name: name}
	trimmed := trimSpace(raw)
	if len(trimmed) == 0 {
		b.Input = json.RawMessage("{}")
		b.InputOrder = jsonx.OrderedObject{}
		return b, nil
	}
	ord, err := jsonx.DecodeOrderedObject(trimmed)
	if err != nil {
		return b, err
	}
	b.Input = append(json.RawMessage(nil), trimmed...)
	b.InputOrder = ord
	return b, nil
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && isSpace(b[i]) {
		i++
	}
	for j > i && isSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// InputMap decodes lazily for handler and interceptor convenience. The result
// is NEVER re-encoded back into a request (§5).
//
// Numbers come back as json.Number, not float64, because NFR-TEST-03(d)
// requires numeric literals to survive verbatim. Handlers doing
// args["limit"].(float64) will fail their type assertion; use ArgInt/ArgFloat.
func (b ToolUseBlock) InputMap() map[string]any { return b.InputOrder.Map() }

type ToolResultBlock struct {
	ToolUseID string
	// Content is a content list, not a string: a tool may return interleaved
	// text and image blocks (§5).
	Content Content
	IsError bool
}

type ImageBlock struct {
	Data     string // base64
	MimeType string // image/jpeg | image/png | image/gif | image/webp
}

// RawBlock retains a block type this build does not model, verbatim, so a log
// written by a newer version survives a read/write cycle by an older one — the
// same argument REQ-SESS-05.2 makes for unknown entry types. Providers drop it.
type RawBlock struct {
	Type string
	Raw  json.RawMessage
}

func (TextBlock) BlockType() BlockType       { return BlockText }
func (ThinkingBlock) BlockType() BlockType   { return BlockThinking }
func (ToolUseBlock) BlockType() BlockType    { return BlockToolUse }
func (ToolResultBlock) BlockType() BlockType { return BlockToolResult }
func (ImageBlock) BlockType() BlockType      { return BlockImage }
func (b RawBlock) BlockType() BlockType      { return BlockType(b.Type) }

func (TextBlock) isContentBlock()       {}
func (ThinkingBlock) isContentBlock()   {}
func (ToolUseBlock) isContentBlock()    {}
func (ToolResultBlock) isContentBlock() {}
func (ImageBlock) isContentBlock()      {}
func (RawBlock) isContentBlock()        {}

func (b TextBlock) CloneBlock() ContentBlock     { return b }
func (b ThinkingBlock) CloneBlock() ContentBlock { return b }
func (b ImageBlock) CloneBlock() ContentBlock    { return b }

func (b ToolUseBlock) CloneBlock() ContentBlock {
	b.Input = append(json.RawMessage(nil), b.Input...)
	b.InputOrder = b.InputOrder.Clone()
	return b
}

func (b ToolResultBlock) CloneBlock() ContentBlock {
	b.Content = b.Content.Clone()
	return b
}

func (b RawBlock) CloneBlock() ContentBlock {
	b.Raw = append(json.RawMessage(nil), b.Raw...)
	return b
}
