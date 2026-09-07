package core

import (
	"encoding/json"
	"fmt"
)

// REQ-OBS-06c: event structs serialize to a DISCRIMINATED JSON union — a
// `type` field plus exactly the fields that variant carries. No shared
// optional-everything envelope.
//
// Messages inside an event are encoded by the caller-supplied encoder rather
// than by this package: the canonical message codec lives with the session
// log (NFR-TEST-03 requires it to be lossless, unknown keys included), and
// carrying a second, drifting copy of it here is the failure mode the golden
// tests exist to catch. The root package wires the session codec in.

// MessageEncoder renders one canonical message as JSON.
type MessageEncoder func(Message) (json.RawMessage, error)

// MarshalEvent encodes e as its discriminated JSON form.
func MarshalEvent(e Event, enc MessageEncoder) ([]byte, error) {
	if enc == nil {
		return nil, fmt.Errorf("core: MarshalEvent needs a MessageEncoder")
	}
	msg := func(m Message) (json.RawMessage, error) { return enc(m) }
	results := func(rs []ToolResultMessage) ([]json.RawMessage, error) {
		out := make([]json.RawMessage, 0, len(rs))
		for _, r := range rs {
			b, err := enc(r)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
		return out, nil
	}
	// Every variant names its type FIRST so a consumer can dispatch on it
	// without decoding the rest.
	switch v := e.(type) {
	case AgentStartEvent:
		return json.Marshal(struct {
			Type      EventType `json:"type"`
			SessionID string    `json:"session_id"`
			Provider  string    `json:"provider"`
			API       API       `json:"api"`
			Model     string    `json:"model"`
		}{v.EventType(), v.SessionID, v.Provider, v.API, v.Model})
	case AgentDoneEvent:
		ms, err := messagesJSON(v.Result.Messages, enc)
		if err != nil {
			return nil, err
		}
		errText := ""
		if v.Result.Error != nil {
			errText = v.Result.Error.Error()
		}
		return json.Marshal(struct {
			Type       EventType         `json:"type"`
			StopReason RunStopReason     `json:"stop_reason"`
			LastReason StopReason        `json:"last_reason"`
			TurnCount  int               `json:"turn_count"`
			Error      string            `json:"error"`
			RunUsage   UsageJSON         `json:"run_usage"`
			Messages   []json.RawMessage `json:"messages"`
			Usage      UsageJSON         `json:"usage"`
		}{v.EventType(), v.Result.StopReason, v.Result.LastReason, v.Result.TurnCount, errText,
			usageJSON(v.Result.Usage), ms, usageJSON(v.Usage)})
	case TurnStartEvent:
		return json.Marshal(struct {
			Type      EventType `json:"type"`
			TurnIndex int       `json:"turn_index"`
		}{v.EventType(), v.TurnIndex})
	case TurnEndEvent:
		m, err := msg(v.Message)
		if err != nil {
			return nil, err
		}
		rs, err := results(v.ToolResults)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Type        EventType         `json:"type"`
			TurnIndex   int               `json:"turn_index"`
			Message     json.RawMessage   `json:"message"`
			ToolResults []json.RawMessage `json:"tool_results"`
			Usage       UsageJSON         `json:"usage"`
		}{v.EventType(), v.TurnIndex, m, rs, usageJSON(v.Usage)})
	case MessageStartEvent:
		return messageEvent(v.EventType(), v.Message, msg)
	case MessageUpdateEvent:
		return messageEvent(v.EventType(), v.Message, msg)
	case MessageEndEvent:
		return messageEvent(v.EventType(), v.Message, msg)
	case TextStartEvent:
		return json.Marshal(struct {
			Type       EventType `json:"type"`
			BlockIndex int       `json:"block_index"`
		}{v.EventType(), v.BlockIndex})
	case TextDeltaEvent:
		return json.Marshal(struct {
			Type       EventType `json:"type"`
			BlockIndex int       `json:"block_index"`
			Delta      string    `json:"delta"`
		}{v.EventType(), v.BlockIndex, v.Delta})
	case TextEndEvent:
		return json.Marshal(struct {
			Type       EventType `json:"type"`
			BlockIndex int       `json:"block_index"`
			Text       string    `json:"text"`
		}{v.EventType(), v.BlockIndex, v.Text})
	case ThinkingStartEvent:
		return json.Marshal(struct {
			Type       EventType `json:"type"`
			BlockIndex int       `json:"block_index"`
		}{v.EventType(), v.BlockIndex})
	case ThinkingDeltaEvent:
		return json.Marshal(struct {
			Type       EventType `json:"type"`
			BlockIndex int       `json:"block_index"`
			Delta      string    `json:"delta"`
		}{v.EventType(), v.BlockIndex, v.Delta})
	case ThinkingEndEvent:
		return json.Marshal(struct {
			Type       EventType `json:"type"`
			BlockIndex int       `json:"block_index"`
			Thinking   string    `json:"thinking"`
			Signature  string    `json:"signature"`
			Redacted   bool      `json:"redacted"`
		}{v.EventType(), v.BlockIndex, v.Thinking, v.Signature, v.Redacted})
	case ToolCallStartEvent:
		return json.Marshal(struct {
			Type       EventType `json:"type"`
			BlockIndex int       `json:"block_index"`
			ToolUseID  string    `json:"tool_use_id"`
			Name       string    `json:"name"`
		}{v.EventType(), v.BlockIndex, v.ToolUseID, v.Name})
	case ToolInputDeltaEvent:
		return json.Marshal(struct {
			Type       EventType `json:"type"`
			BlockIndex int       `json:"block_index"`
			ToolUseID  string    `json:"tool_use_id"`
			Delta      string    `json:"delta"`
		}{v.EventType(), v.BlockIndex, v.ToolUseID, v.Delta})
	case ToolCallEndEvent:
		return json.Marshal(struct {
			Type       EventType       `json:"type"`
			BlockIndex int             `json:"block_index"`
			ToolUseID  string          `json:"tool_use_id"`
			Name       string          `json:"name"`
			Input      json.RawMessage `json:"input"`
		}{v.EventType(), v.BlockIndex, v.Block.ID, v.Block.Name, rawOrEmptyObject(v.Block.Input)})
	case ToolExecutionStartEvent:
		return json.Marshal(struct {
			Type      EventType `json:"type"`
			ToolUseID string    `json:"tool_use_id"`
			Name      string    `json:"name"`
		}{v.EventType(), v.ToolUseID, v.Name})
	case ToolExecutionUpdateEvent:
		return json.Marshal(struct {
			Type      EventType `json:"type"`
			ToolUseID string    `json:"tool_use_id"`
			Name      string    `json:"name"`
			Chunk     string    `json:"chunk"`
		}{v.EventType(), v.ToolUseID, v.Name, v.Chunk})
	case ToolExecutionEndEvent:
		return json.Marshal(struct {
			Type      EventType `json:"type"`
			ToolUseID string    `json:"tool_use_id"`
			Name      string    `json:"name"`
			IsError   bool      `json:"is_error"`
			ElapsedMS int64     `json:"elapsed_ms"`
		}{v.EventType(), v.ToolUseID, v.Name, v.IsError, v.ElapsedMS})
	case ToolResultEvent:
		m, err := msg(v.Message)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Type    EventType       `json:"type"`
			Message json.RawMessage `json:"message"`
		}{v.EventType(), m})
	case ErrorEvent:
		return json.Marshal(struct {
			Type     EventType `json:"type"`
			Message  string    `json:"message"`
			Terminal bool      `json:"terminal"`
		}{v.EventType(), v.Message, v.Terminal})
	}
	return nil, fmt.Errorf("core: MarshalEvent: unknown event %T", e)
}

func messageEvent(t EventType, m AssistantMessage, enc func(Message) (json.RawMessage, error)) ([]byte, error) {
	b, err := enc(m)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Type    EventType       `json:"type"`
		Message json.RawMessage `json:"message"`
	}{t, b})
}

func messagesJSON(ms Messages, enc MessageEncoder) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(ms))
	for _, m := range ms {
		b, err := enc(m)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func rawOrEmptyObject(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return b
}

// UsageJSON is Usage's wire shape for events. Every field is present — a
// consumer reads the same keys whether or not the provider reported them —
// and `reported` says which ones the provider actually set (REQ-GO-15).
type UsageJSON struct {
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	ReasoningTokens    int64   `json:"reasoning_tokens"`
	CacheReadTokens    int64   `json:"cache_read_tokens"`
	CacheWriteTokens   int64   `json:"cache_write_tokens"`
	CacheWrite1hTokens int64   `json:"cache_write_1h_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	CostUSD            float64 `json:"cost_usd"`
	BilledModel        string  `json:"billed_model"`
	Reported           bool    `json:"reported"`
}

func usageJSON(u Usage) UsageJSON {
	return UsageJSON{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, ReasoningTokens: u.ReasoningTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
		CacheWrite1hTokens: u.CacheWrite1hTokens, TotalTokens: u.TotalTokens,
		CostUSD: u.CostUSD, BilledModel: u.BilledModel, Reported: u.Reported(),
	}
}
