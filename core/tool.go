package core

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agentfox/agentkit-go/schema"
)

type ExecutionMode uint8

const (
	// Parallel is the zero value, so every custom tool defaults correctly with
	// no field set (REQ-LOOP-05a).
	Parallel ExecutionMode = iota
	Sequential
)

// ToolHandler is REQ-GO-03's pinned signature.
type ToolHandler func(ctx context.Context, input json.RawMessage) (json.RawMessage, error)

// ResultHandler is the richer form. It exists because REQ-GO-03's signature
// cannot express REQ-TOOL-08: ToolResult.Terminate is `json:"-"` and Metadata
// is stripped by ToLLMMap, so there is no channel through json.RawMessage for
// either. Without it, REQ-TOOL-13's own stated use cases (finish,
// submit_answer, exit_plan_mode) are unimplementable by a consumer, and every
// built-in tool that truncates cannot report metadata.
type ResultHandler func(ctx context.Context, input json.RawMessage) ToolResult

// Tool is the registry + loop type (REQ-TOOL-01). Exactly one of Handler and
// Execute must be set.
type Tool struct {
	Name        string
	Description string
	InputSchema *schema.Schema

	Handler ToolHandler
	Execute ResultHandler

	Label string // display name, never sent
	// Builtin marks a first-party tool. It exists so ToolPolicy.NoTools can
	// mean "builtin" without the resolver keeping a hardcoded name list that
	// silently stops matching when a tool is renamed (REQ-TOOL-10).
	Builtin             bool
	ExecutionMode       ExecutionMode
	PrepareArguments    func(map[string]any) map[string]any
	PromptGuidelines    []string
	ConstrainedSampling *ConstrainedSampling
}

// ToolWire is the projection a provider is allowed to see — the four fields of
// REQ-TOOL-01 and no others.
//
// The PRD sketches `func (t Tool) wire() wireTool`, unexported. That is
// unimplementable once providers live in their own packages, which REQ-PROV-02
// requires. Exported, the invariant is STRONGER than unexported-ness:
// Request.Tools is []ToolWire, so no provider can name a Handler,
// PromptGuidelines or PrepareArguments at all, and a new Tool field cannot
// widen a request body without a compile error at the boundary.
//
// ToolWire carries no json tags on purpose: each provider encodes it into its
// own dialect (Anthropic input_schema, OpenAI parameters).
type ToolWire struct {
	Name                string
	Description         string
	InputSchema         *schema.Schema
	ConstrainedSampling *ConstrainedSampling
}

func (t Tool) Wire() ToolWire {
	return ToolWire{
		Name:                t.Name,
		Description:         t.Description,
		InputSchema:         t.InputSchema,
		ConstrainedSampling: t.ConstrainedSampling,
	}
}

func ToolWires(ts []Tool) []ToolWire {
	out := make([]ToolWire, len(ts))
	for i, t := range ts {
		out[i] = t.Wire()
	}
	return out
}

type ConstrainedSamplingType string

const (
	ConstrainJSONSchema ConstrainedSamplingType = "json_schema"
	ConstrainGrammar    ConstrainedSamplingType = "grammar"
)

type StrictMode string

const (
	StrictPrefer  StrictMode = "prefer"
	StrictRequire StrictMode = "require"
)

// ConstrainedSampling is a struct, not a bool (REQ-TOOL-03, A.2#15).
type ConstrainedSampling struct {
	Type   ConstrainedSamplingType
	Strict StrictMode
}

// ToolResult is REQ-TOOL-08's output envelope.
type ToolResult struct {
	OK     bool           `json:"ok"`
	Data   map[string]any `json:"data"`
	Error  string         `json:"error,omitzero"`
	Detail string         `json:"detail,omitzero"`
	// Terminate never reaches the model; it is the REQ-TOOL-13 batch vote.
	Terminate bool          `json:"-"`
	Metadata  *ToolMetadata `json:"metadata,omitzero"`
}

func OKResult(data map[string]any) ToolResult { return ToolResult{OK: true, Data: data} }

func ErrResult(code, detail string) ToolResult {
	return ToolResult{OK: false, Error: code, Detail: detail}
}

// ToLLMMap strips Metadata and Terminate. Providers receive only the payload.
func (r ToolResult) ToLLMMap() map[string]any {
	m := map[string]any{"ok": r.OK}
	if r.Data != nil {
		m["data"] = r.Data
	}
	if r.Error != "" {
		m["error"] = r.Error
	}
	if r.Detail != "" {
		m["detail"] = r.Detail
	}
	return m
}

type ToolMetadata struct {
	Truncated   bool   `json:"truncated,omitzero"`
	TruncatedBy string `json:"truncated_by,omitzero"` // "lines" | "bytes"
	TotalBytes  int64  `json:"total_bytes,omitzero"`
	TotalLines  int64  `json:"total_lines,omitzero"`
	SpillPath   string `json:"spill_path,omitzero"`
	DurationMS  int64  `json:"duration_ms,omitzero"`
	ExitCode    *int   `json:"exit_code,omitzero"`
	Outcome     string `json:"outcome,omitzero"` // ok|exit|signal|timeout|abort
	LineEnding  string `json:"line_ending,omitzero"`
}

// ---------------------------------------------------------- exposure policy

type NoToolsMode string

const (
	NoToolsNone    NoToolsMode = ""
	NoToolsBuiltin NoToolsMode = "builtin"
	NoToolsAll     NoToolsMode = "all"
)

// ToolPolicy is REQ-TOOL-10's five-field resolution, applied uniformly to
// built-in and caller-supplied tools.
type ToolPolicy struct {
	// Tools, when non-nil (INCLUDING empty), is used verbatim and bypasses
	// everything below. Non-nil-but-empty is distinct from nil.
	Tools        []Tool
	ToolNames    []string // nil means the default built-in set
	ExcludeTools []string
	NoTools      NoToolsMode
	CustomTools  []Tool
}

// ------------------------------------------------------------ interception

// BeforeToolCallContext is handed to the single tool-authorization boundary
// (REQ-SEC-03). Arguments are already validated and coerced.
type BeforeToolCallContext struct {
	ToolName  string
	ToolUseID string
	Tool      Tool
	Arguments map[string]any
	// RawInput is the model's own bytes, key order intact (REQ-TOOL-12).
	RawInput  json.RawMessage
	Assistant *AssistantMessage
	Batch     []ToolUseBlock
	Index     int
	TurnCount int
}

type BeforeToolCallDecision struct {
	Block bool
	// Terminate is honoured ONLY when Block is set (REQ-TOOL-13.2).
	Terminate bool
	Reason    string
	// Arguments, when non-nil, replaces Arguments. The interceptor may widen
	// as well as narrow (REQ-SEC-03.5).
	Arguments map[string]any
}

type AfterToolCallContext struct {
	ToolName  string
	ToolUseID string
	Arguments map[string]any
	Result    *ToolResultMessage // mutable in place
	Elapsed   time.Duration
}

type AfterToolCallDecision struct {
	// Terminate is *bool because nil means "no opinion" (REQ-TOOL-13.3).
	Terminate *bool
}

type BeforeToolCall func(ctx context.Context, in BeforeToolCallContext) BeforeToolCallDecision
type AfterToolCall func(ctx context.Context, in AfterToolCallContext) AfterToolCallDecision

const BlockErrorCode = "blocked_by_policy"

// BatchTerminates is REQ-TOOL-13.1's vote: an AND over finalized results. An
// empty batch never terminates. OR semantics are the obvious-but-wrong
// default: the model emits N parallel calls, and a unilateral finish would
// compute N-1 results it never sees.
func BatchTerminates(votes []bool) bool {
	if len(votes) == 0 {
		return false
	}
	for _, v := range votes {
		if !v {
			return false
		}
	}
	return true
}
