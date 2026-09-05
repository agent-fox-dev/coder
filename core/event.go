package core

type EventType string

const (
	EvAgentStart EventType = "agent_start"
	EvAgentDone  EventType = "agent_done"
	EvTurnStart  EventType = "turn_start"
	EvTurnEnd    EventType = "turn_end"

	EvMessageStart  EventType = "message_start"
	EvMessageUpdate EventType = "message_update"
	EvMessageEnd    EventType = "message_end"

	EvTextStart EventType = "text_start"
	EvTextDelta EventType = "text_delta"
	EvTextEnd   EventType = "text_end"

	EvThinkingStart EventType = "thinking_start"
	EvThinkingDelta EventType = "thinking_delta"
	EvThinkingEnd   EventType = "thinking_end"

	// Model-stream decode events (REQ-OBS-06). Distinct from the execution
	// triple below: REQ-LOOP-05 and REQ-LOOP-11.3 say "ToolCallStartEvent" for
	// tool EXECUTION, which would produce two events of that name per call
	// with different payloads. The loop emits the execution triple.
	EvToolCallStart  EventType = "tool_call_start"
	EvToolInputDelta EventType = "tool_input_delta"
	EvToolCallEnd    EventType = "tool_call_end"

	EvToolExecutionStart  EventType = "tool_execution_start"
	EvToolExecutionUpdate EventType = "tool_execution_update"
	EvToolExecutionEnd    EventType = "tool_execution_end"

	EvToolResult EventType = "tool_result"
	EvError      EventType = "error"
)

// EventClass is REQ-OBS-06a's split, with a third member the requirement needs
// and does not have.
//
// 06a defines authoritative as "complete and final for the item it names,
// exactly one per item, always emitted" and then places everything that is not
// a delta into that class. But MessageUpdateEvent is by construction repeated
// and non-final, and 06b explicitly wants many of them. A consumer following
// 06a literally would treat the first MessageUpdateEvent as final and stop
// rendering.
type EventClass uint8

const (
	// ClassIncremental is an optimization. It may be coalesced, may be absent
	// entirely (a non-streaming provider emits none), and is never truth.
	ClassIncremental EventClass = iota
	// ClassSnapshot is complete as of now, repeatable, never final. A consumer
	// may render from it and must not treat it as closing anything.
	ClassSnapshot
	// ClassAuthoritative is complete and final for the item it names, exactly
	// one per item, always emitted. Receiving one means DISCARD the deltas
	// accumulated for that item and take this payload whole.
	ClassAuthoritative
)

// Event is a sealed union. clone and size are unexported so the union cannot
// be extended from outside and so EventStream.Push can enforce REQ-OBS-06b
// centrally, rather than trusting every producer to remember.
type Event interface {
	EventType() EventType
	Class() EventClass
	clone() Event
	size() int
}

type AgentStartEvent struct {
	SessionID string
	Provider  string
	API       API
	Model     string
}

type AgentDoneEvent struct {
	Result RunResult
	Usage  Usage // session aggregate
}

type TurnStartEvent struct{ TurnIndex int }

type TurnEndEvent struct {
	TurnIndex int
	Message   AssistantMessage
	// ToolResults is always non-nil; [] for a no-tool turn (REQ-OBS-06).
	ToolResults []ToolResultMessage
	Usage       Usage
}

type MessageStartEvent struct{ Message AssistantMessage }
type MessageUpdateEvent struct{ Message AssistantMessage }
type MessageEndEvent struct{ Message AssistantMessage }

type TextStartEvent struct{ BlockIndex int }
type TextDeltaEvent struct {
	BlockIndex int
	Delta      string
}

// TextEndEvent carries the WHOLE text, not just a final delta. REQ-OBS-06's
// payload table says "block_index, delta", but REQ-OBS-06a requires a consumer
// to discard accumulated deltas and take the authoritative payload whole,
// which is impossible if the end event carries only a delta. 06a wins.
type TextEndEvent struct {
	BlockIndex int
	Text       string
}

type ThinkingStartEvent struct{ BlockIndex int }
type ThinkingDeltaEvent struct {
	BlockIndex int
	Delta      string
}
type ThinkingEndEvent struct {
	BlockIndex int
	Thinking   string
	Signature  string
	Redacted   bool
}

type ToolCallStartEvent struct {
	BlockIndex int
	ToolUseID  string
	Name       string
}
type ToolInputDeltaEvent struct {
	BlockIndex int
	ToolUseID  string
	Delta      string // partial argument JSON, as received
}

// ToolCallEndEvent carries the finalized block, Input bytes included, so the
// consumer needs no salvage parser of its own. REQ-OBS-08.4: it AUGMENTS the
// accumulated call and never replaces state later events read.
type ToolCallEndEvent struct {
	BlockIndex int
	Block      ToolUseBlock
}

type ToolExecutionStartEvent struct {
	ToolUseID string
	Name      string
}
type ToolExecutionUpdateEvent struct {
	ToolUseID string
	Name      string
	Chunk     string
}
type ToolExecutionEndEvent struct {
	ToolUseID string
	Name      string
	Result    Content
	IsError   bool
	ElapsedMS int64
}

type ToolResultEvent struct{ Message ToolResultMessage }

type ErrorEvent struct {
	Message string
	// Err is not serialized; the wire form carries Message.
	Err      error
	Terminal bool
}

func (AgentStartEvent) EventType() EventType          { return EvAgentStart }
func (AgentDoneEvent) EventType() EventType           { return EvAgentDone }
func (TurnStartEvent) EventType() EventType           { return EvTurnStart }
func (TurnEndEvent) EventType() EventType             { return EvTurnEnd }
func (MessageStartEvent) EventType() EventType        { return EvMessageStart }
func (MessageUpdateEvent) EventType() EventType       { return EvMessageUpdate }
func (MessageEndEvent) EventType() EventType          { return EvMessageEnd }
func (TextStartEvent) EventType() EventType           { return EvTextStart }
func (TextDeltaEvent) EventType() EventType           { return EvTextDelta }
func (TextEndEvent) EventType() EventType             { return EvTextEnd }
func (ThinkingStartEvent) EventType() EventType       { return EvThinkingStart }
func (ThinkingDeltaEvent) EventType() EventType       { return EvThinkingDelta }
func (ThinkingEndEvent) EventType() EventType         { return EvThinkingEnd }
func (ToolCallStartEvent) EventType() EventType       { return EvToolCallStart }
func (ToolInputDeltaEvent) EventType() EventType      { return EvToolInputDelta }
func (ToolCallEndEvent) EventType() EventType         { return EvToolCallEnd }
func (ToolExecutionStartEvent) EventType() EventType  { return EvToolExecutionStart }
func (ToolExecutionUpdateEvent) EventType() EventType { return EvToolExecutionUpdate }
func (ToolExecutionEndEvent) EventType() EventType    { return EvToolExecutionEnd }
func (ToolResultEvent) EventType() EventType          { return EvToolResult }
func (ErrorEvent) EventType() EventType               { return EvError }

func (TextDeltaEvent) Class() EventClass      { return ClassIncremental }
func (ThinkingDeltaEvent) Class() EventClass  { return ClassIncremental }
func (ToolInputDeltaEvent) Class() EventClass { return ClassIncremental }
func (ToolExecutionUpdateEvent) Class() EventClass { return ClassIncremental }
func (MessageUpdateEvent) Class() EventClass  { return ClassSnapshot }

func (AgentStartEvent) Class() EventClass         { return ClassAuthoritative }
func (AgentDoneEvent) Class() EventClass          { return ClassAuthoritative }
func (TurnStartEvent) Class() EventClass          { return ClassAuthoritative }
func (TurnEndEvent) Class() EventClass            { return ClassAuthoritative }
func (MessageStartEvent) Class() EventClass       { return ClassAuthoritative }
func (MessageEndEvent) Class() EventClass         { return ClassAuthoritative }
func (TextStartEvent) Class() EventClass          { return ClassAuthoritative }
func (TextEndEvent) Class() EventClass            { return ClassAuthoritative }
func (ThinkingStartEvent) Class() EventClass      { return ClassAuthoritative }
func (ThinkingEndEvent) Class() EventClass        { return ClassAuthoritative }
func (ToolCallStartEvent) Class() EventClass      { return ClassAuthoritative }
func (ToolCallEndEvent) Class() EventClass        { return ClassAuthoritative }
func (ToolExecutionStartEvent) Class() EventClass { return ClassAuthoritative }
func (ToolExecutionEndEvent) Class() EventClass   { return ClassAuthoritative }
func (ToolResultEvent) Class() EventClass         { return ClassAuthoritative }
func (ErrorEvent) Class() EventClass              { return ClassAuthoritative }

// clone implements REQ-OBS-06b. Every event carrying a partial message carries
// an independent deep copy taken at push time, never a pointer to one live,
// mutating message shared by all events.
func (e AgentStartEvent) clone() Event { return e }
func (e AgentDoneEvent) clone() Event {
	e.Result.Messages = e.Result.Messages.Clone()
	return e
}
func (e TurnStartEvent) clone() Event { return e }
func (e TurnEndEvent) clone() Event {
	e.Message = e.Message.Clone().(AssistantMessage)
	rs := make([]ToolResultMessage, len(e.ToolResults))
	for i := range e.ToolResults {
		rs[i] = e.ToolResults[i].Clone().(ToolResultMessage)
	}
	e.ToolResults = rs
	return e
}
func (e MessageStartEvent) clone() Event {
	e.Message = e.Message.Clone().(AssistantMessage)
	return e
}
func (e MessageUpdateEvent) clone() Event {
	e.Message = e.Message.Clone().(AssistantMessage)
	return e
}
func (e MessageEndEvent) clone() Event {
	e.Message = e.Message.Clone().(AssistantMessage)
	return e
}
func (e TextStartEvent) clone() Event      { return e }
func (e TextDeltaEvent) clone() Event      { return e }
func (e TextEndEvent) clone() Event        { return e }
func (e ThinkingStartEvent) clone() Event  { return e }
func (e ThinkingDeltaEvent) clone() Event  { return e }
func (e ThinkingEndEvent) clone() Event    { return e }
func (e ToolCallStartEvent) clone() Event  { return e }
func (e ToolInputDeltaEvent) clone() Event { return e }
func (e ToolCallEndEvent) clone() Event {
	e.Block = e.Block.CloneBlock().(ToolUseBlock)
	return e
}
func (e ToolExecutionStartEvent) clone() Event  { return e }
func (e ToolExecutionUpdateEvent) clone() Event { return e }
func (e ToolExecutionEndEvent) clone() Event {
	e.Result = e.Result.Clone()
	return e
}
func (e ToolResultEvent) clone() Event {
	e.Message = e.Message.Clone().(ToolResultMessage)
	return e
}
func (e ErrorEvent) clone() Event { return e }

// size is an approximate payload cost used only by MaxPendingBytes.
func (e AgentStartEvent) size() int             { return len(e.SessionID) + len(e.Model) + 64 }
func (e AgentDoneEvent) size() int              { return 256 }
func (e TurnStartEvent) size() int              { return 32 }
func (e TurnEndEvent) size() int                { return contentSize(e.Message.Content) + 128 }
func (e MessageStartEvent) size() int           { return contentSize(e.Message.Content) + 64 }
func (e MessageUpdateEvent) size() int          { return contentSize(e.Message.Content) + 64 }
func (e MessageEndEvent) size() int             { return contentSize(e.Message.Content) + 64 }
func (e TextStartEvent) size() int              { return 32 }
func (e TextDeltaEvent) size() int              { return len(e.Delta) + 32 }
func (e TextEndEvent) size() int                { return len(e.Text) + 32 }
func (e ThinkingStartEvent) size() int          { return 32 }
func (e ThinkingDeltaEvent) size() int          { return len(e.Delta) + 32 }
func (e ThinkingEndEvent) size() int            { return len(e.Thinking) + len(e.Signature) + 32 }
func (e ToolCallStartEvent) size() int          { return len(e.ToolUseID) + len(e.Name) + 32 }
func (e ToolInputDeltaEvent) size() int         { return len(e.Delta) + 32 }
func (e ToolCallEndEvent) size() int            { return len(e.Block.Input) + 64 }
func (e ToolExecutionStartEvent) size() int     { return len(e.Name) + 32 }
func (e ToolExecutionUpdateEvent) size() int    { return len(e.Chunk) + 32 }
func (e ToolExecutionEndEvent) size() int       { return contentSize(e.Result) + 64 }
func (e ToolResultEvent) size() int             { return contentSize(e.Message.Content) + 64 }
func (e ErrorEvent) size() int                  { return len(e.Message) + 32 }

func contentSize(c Content) int {
	n := 0
	for _, b := range c {
		switch v := b.(type) {
		case TextBlock:
			n += len(v.Text)
		case ThinkingBlock:
			n += len(v.Thinking) + len(v.Signature)
		case ToolUseBlock:
			n += len(v.Input) + len(v.Name)
		case ToolResultBlock:
			n += contentSize(v.Content)
		case ImageBlock:
			n += len(v.Data)
		case RawBlock:
			n += len(v.Raw)
		}
		n += 32
	}
	return n
}
