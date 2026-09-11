// Package testkit holds the Agent-free test doubles the root package's tests
// use, exported so a test in a package ABOVE the root (subagent) can script a
// provider the same way. It imports core and schema only; a helper that
// constructed an Agent would import the root, and the root's own internal
// tests could not then import it.
package testkit

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/schema"
)

// TestAPI is the wire API every scripted provider registers under.
const TestAPI core.API = "test-api"

// TestModel is a resolved model on TestAPI.
func TestModel() *core.Model {
	return &core.Model{ID: "test-model", Name: "Test", API: TestAPI, Provider: "test", ContextWindow: 100000, MaxTokens: 4096}
}

// Scripted is a provider that replays a predetermined sequence of assistant
// messages, one per turn, and answers "done" once the script is exhausted.
// It is the executable double the loop is tested against; provider/faux is
// the shipped, supported form of the same idea (NFR-TEST-05).
type Scripted struct {
	mu    sync.Mutex
	Turns []core.AssistantMessage
	calls int
	// Seen records the message list each turn was asked to complete, so a
	// test can assert what the loop actually sent and when.
	Seen []core.Messages
	// Systems records the assembled system prompt per turn.
	Systems [][]core.ContentBlock
}

// Provider registers the double under TestAPI.
func (s *Scripted) Provider() core.APIProvider {
	return core.APIProvider{API: TestAPI, Stream: s.stream}
}

func (s *Scripted) stream(ctx context.Context, m *core.Model, req core.Request, _ core.ProviderStreamOptions) *core.EventStream {
	st := core.NewEventStream(core.StreamOptions{})
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.Seen = append(s.Seen, req.Messages)
	s.Systems = append(s.Systems, req.System)
	var msg core.AssistantMessage
	if i < len(s.Turns) {
		msg = s.Turns[i]
	} else {
		msg = core.AssistantMessage{
			Content:    core.Content{core.TextBlock{Text: "done"}},
			StopReason: core.StopReasonStop,
		}
	}
	s.mu.Unlock()

	msg.Provider, msg.API, msg.Model = m.Provider, m.API, m.ID
	go func() {
		st.Push(core.MessageStartEvent{Message: msg})
		st.Push(core.MessageEndEvent{Message: msg})
		st.End(core.StreamResult{Message: &msg})
	}()
	return st
}

// SentAt returns the messages the provider was asked to complete on turn.
func (s *Scripted) SentAt(turn int) core.Messages {
	s.mu.Lock()
	defer s.mu.Unlock()
	if turn >= len(s.Seen) {
		return nil
	}
	return s.Seen[turn]
}

// TurnsRun reports how many requests the double has answered.
func (s *Scripted) TurnsRun() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

// ToolUse builds a tool_use block from raw argument bytes.
func ToolUse(t *testing.T, id, name, args string) core.ToolUseBlock {
	t.Helper()
	b, err := core.NewToolUse(id, name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("NewToolUse: %v", err)
	}
	return b
}

// AssistantWithTools is an assistant message carrying the given blocks.
func AssistantWithTools(reason core.StopReason, blocks ...core.ContentBlock) core.AssistantMessage {
	return core.AssistantMessage{Content: core.Content(blocks), StopReason: reason}
}

// EchoTool is a tool that answers {"echoed":true} and counts its calls.
func EchoTool(name string, calls *atomic.Int32) core.Tool {
	return core.Tool{
		Name:        name,
		Description: "echo",
		InputSchema: schema.Object(schema.Opt("v", schema.String())),
		Handler: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			if calls != nil {
				calls.Add(1)
			}
			return json.RawMessage(`{"echoed":true}`), nil
		},
	}
}
