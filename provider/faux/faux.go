// Package faux is a deterministic provider that replays a scripted sequence of
// assistant turns.
//
// It ships as exported, supported API in the same place as the real providers,
// not as test-only code inside the module (NFR-TEST-05). That placement is the
// requirement, not a convenience: consumers use it to test their own tool
// handlers, middleware and interceptors, and an SDK whose double is internal
// forces every consumer to write one against their own guesses about the loop.
//
// It is also the EXECUTABLE SPECIFICATION of the streaming protocol. The event
// order it emits is normative — a real provider that disagrees with it is the
// one that is wrong — so its golden test is the protocol's conformance test.
//
// It is a functional double for the loop only. It does not substitute for
// wire-level differential testing (NFR-TEST-06).
package faux

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// API is the wire API id faux registers under.
const API core.API = "faux"

// Turn is one scripted assistant response.
type Turn struct {
	Blocks     []core.ContentBlock
	StopReason core.StopReason
	// RawStopReason is the provider's own finish string. Scripting it lets a
	// test reproduce the case REQ-LOOP-01 exists for: a STOP-family canonical
	// reason arriving alongside tool calls.
	RawStopReason string
	ErrorMessage  string
	Usage         core.Usage
	// Err, when set, ends the stream with an error after emitting whatever
	// blocks were produced — the half-a-message-then-fail case REQ-PROV-04
	// requires to be representable in one value.
	Err error
	// Delay is inserted before the turn is produced, for tests that need a
	// window in which to Steer, Abort or observe a Phase.
	Delay time.Duration
}

// FauxText scripts a text block.
func FauxText(s string) core.ContentBlock { return core.TextBlock{Text: s} }

// FauxThinking scripts a thinking block. A signature is required for the block
// to survive REQ-PROV-11 rule 4 on replay; passing "" is the deliberate way to
// script the residue of an aborted stream.
func FauxThinking(thinking, signature string) core.ContentBlock {
	return core.ThinkingBlock{Thinking: thinking, Signature: signature}
}

// FauxToolCall scripts a tool call. args must be a JSON object; it panics
// otherwise, because a malformed script is a test bug and surfacing it at the
// call site is more useful than a confusing failure three layers down.
func FauxToolCall(id, name, args string) core.ContentBlock {
	b, err := core.NewToolUse(id, name, json.RawMessage(args))
	if err != nil {
		panic(fmt.Sprintf("faux.FauxToolCall(%q, %q): %v", id, name, err))
	}
	return b
}

// FauxAssistantMessage builds a whole turn in one call.
func FauxAssistantMessage(reason core.StopReason, blocks ...core.ContentBlock) Turn {
	return Turn{Blocks: blocks, StopReason: reason}
}

// Provider is a scripted provider. The zero value is usable and answers every
// request with an empty "stop" turn.
type Provider struct {
	mu    sync.Mutex
	turns []Turn
	calls int
	seen  []core.Request

	// ChunkSize splits text and tool arguments into deltas of this many bytes.
	// Zero means one delta per block. Setting it small is how a test exercises
	// a consumer's delta accumulator.
	ChunkSize int
}

// New returns a provider that replays turns in order. Once the script is
// exhausted every further request answers with an empty "stop" turn, so a test
// that under-scripts fails on its assertion rather than by deadlocking.
func New(turns ...Turn) *Provider { return &Provider{turns: turns} }

// APIProvider is the registry entry.
func (p *Provider) APIProvider() core.APIProvider {
	return core.APIProvider{API: API, Stream: p.Stream}
}

// Model returns a model descriptor pointing at this provider.
func Model() *core.Model {
	return &core.Model{
		ID: "faux-1", Name: "Faux", API: API, Provider: "faux",
		ContextWindow: 200000, MaxTokens: 8192,
		Input: []string{"text"},
	}
}

// Requests returns the requests the provider was asked to complete, in order.
// Tests use it to assert what the loop actually sent, and when.
func (p *Provider) Requests() []core.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]core.Request(nil), p.seen...)
}

// Calls reports how many requests were made.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// Stream implements core.StreamFunc.
func (p *Provider) Stream(ctx context.Context, m *core.Model, req core.Request, _ core.ProviderStreamOptions) *core.EventStream {
	p.mu.Lock()
	i := p.calls
	p.calls++
	p.seen = append(p.seen, req)
	var turn Turn
	if i < len(p.turns) {
		turn = p.turns[i]
	} else {
		turn = Turn{Blocks: []core.ContentBlock{core.TextBlock{Text: ""}}, StopReason: core.StopReasonStop}
	}
	chunk := p.ChunkSize
	p.mu.Unlock()

	s := core.NewEventStream(core.StreamOptions{})
	go p.produce(ctx, s, m, turn, chunk)
	return s
}

// produce emits the normative event sequence. Read it as the specification:
//
//	MessageStart
//	  per block, in order: <Kind>Start, <Kind>Delta*        (incremental)
//	  ... all blocks streamed ...
//	  per block, in order: <Kind>End                        (authoritative)
//	MessageEnd
//	End(result)
//
// Block-end events are emitted AFTER the stream has fully ended, once each, in
// block order — never from inside the per-chunk handler (REQ-OBS-08.2).
// Emitting them from the chunk handler produces duplicate end events and, in a
// real provider, a message end that lacks usage, because usage arrives with
// the terminal chunk.
//
// Every incremental event for an item precedes that item's authoritative event
// (REQ-OBS-08.1), so a consumer that discards accumulated deltas on the
// authoritative event never double-applies.
func (p *Provider) produce(ctx context.Context, s *core.EventStream, m *core.Model, turn Turn, chunk int) {
	partial := core.AssistantMessage{
		Provider: m.Provider, API: m.API, Model: m.ID,
		Timestamp: time.Now(),
	}

	if turn.Delay > 0 {
		select {
		case <-time.After(turn.Delay):
		case <-ctx.Done():
			p.finishAborted(s, partial)
			return
		}
	}
	if err := ctx.Err(); err != nil {
		p.finishAborted(s, partial)
		return
	}

	s.Push(core.MessageStartEvent{Message: partial})

	// ---- Incremental phase.
	for i, b := range turn.Blocks {
		if ctx.Err() != nil {
			p.finishAborted(s, partial)
			return
		}
		switch v := b.(type) {
		case core.TextBlock:
			s.Push(core.TextStartEvent{BlockIndex: i})
			for _, part := range split(v.Text, chunk) {
				s.Push(core.TextDeltaEvent{BlockIndex: i, Delta: part})
			}
		case core.ThinkingBlock:
			s.Push(core.ThinkingStartEvent{BlockIndex: i})
			for _, part := range split(v.Thinking, chunk) {
				s.Push(core.ThinkingDeltaEvent{BlockIndex: i, Delta: part})
			}
		case core.ToolUseBlock:
			s.Push(core.ToolCallStartEvent{BlockIndex: i, ToolUseID: v.ID, Name: v.Name})
			for _, part := range split(string(v.Input), chunk) {
				s.Push(core.ToolInputDeltaEvent{BlockIndex: i, ToolUseID: v.ID, Delta: part})
			}
		}
		partial.Content = append(partial.Content, b)
		s.Push(core.MessageUpdateEvent{Message: partial})
	}

	// ---- Authoritative phase, in block order, after the stream has ended.
	for i, b := range turn.Blocks {
		switch v := b.(type) {
		case core.TextBlock:
			// The end event carries the WHOLE text, not a final delta, so a
			// consumer can discard its accumulator and take the authoritative
			// payload (REQ-OBS-06a).
			s.Push(core.TextEndEvent{BlockIndex: i, Text: v.Text})
		case core.ThinkingBlock:
			s.Push(core.ThinkingEndEvent{BlockIndex: i, Thinking: v.Thinking,
				Signature: v.Signature, Redacted: v.Redacted})
		case core.ToolUseBlock:
			s.Push(core.ToolCallEndEvent{BlockIndex: i, Block: v})
		}
	}

	final := partial
	final.StopReason = turn.StopReason
	if final.StopReason == "" {
		final.StopReason = core.StopReasonStop
	}
	final.RawStopReason = turn.RawStopReason
	if final.RawStopReason == "" {
		final.RawStopReason = string(final.StopReason)
	}
	final.ErrorMessage = turn.ErrorMessage
	final.Usage = turn.Usage

	if turn.Err != nil {
		final.StopReason = core.StopReasonError
		final.ErrorMessage = turn.Err.Error()
		s.Push(core.ErrorEvent{Message: turn.Err.Error(), Err: turn.Err, Terminal: true})
		s.Push(core.MessageEndEvent{Message: final})
		s.End(core.StreamResult{Message: &final, Err: turn.Err})
		return
	}

	s.Push(core.MessageEndEvent{Message: final})
	s.End(core.StreamResult{Message: &final})
}

// finishAborted produces the terminal marker of REQ-LOOP-09: the partial
// content accumulated so far, with an aborted stop reason. History after an
// abort is recoverable, not clean, and this is what makes it so.
func (p *Provider) finishAborted(s *core.EventStream, partial core.AssistantMessage) {
	partial.StopReason = core.StopReasonAborted
	partial.ErrorMessage = "Request was aborted"
	s.Push(core.MessageEndEvent{Message: partial})
	s.End(core.StreamResult{Message: &partial, Err: context.Canceled})
}

func split(s string, n int) []string {
	if s == "" {
		return nil
	}
	if n <= 0 || n >= len(s) {
		return []string{s}
	}
	var out []string
	for i := 0; i < len(s); i += n {
		j := i + n
		if j > len(s) {
			j = len(s)
		}
		out = append(out, s[i:j])
	}
	return out
}

// EventNames renders an event sequence as a comma-separated list of type
// names. It exists so a golden test of the protocol reads as a sentence rather
// than as a struct dump.
func EventNames(events []core.Event) string {
	var out []string
	for _, e := range events {
		out = append(out, string(e.EventType()))
	}
	return strings.Join(out, ",")
}
