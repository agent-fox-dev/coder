package core

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// --- REQ-LOOP-01 / Appendix A correction #1 -------------------------------

func TestExtractToolUseIgnoresStopReason(t *testing.T) {
	call, err := NewToolUse("toolu_1", "search", json.RawMessage(`{"z":1,"a":2}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		reason  StopReason
		content Content
		iterate bool
		short   bool
	}{
		// Rows 1 and 3 FAIL on any loop gated on stop_reason == "tool_use".
		{"gateway: stop WITH tool calls", StopReasonStop, Content{call}, true, false},
		{"tool_use with NO calls", StopReasonToolUse, Content{TextBlock{Text: "hi"}}, false, false},
		{"stop_sequence with calls", StopReasonStopSequence, Content{call}, true, false},
		{"unrecognized reason with calls", StopReason("weird"), Content{call}, true, false},
		{"max_tokens with calls", StopReasonLength, Content{call}, true, false},
		{"error short-circuits", StopReasonError, Content{call}, true, true},
		{"aborted short-circuits", StopReasonAborted, Content{call}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &AssistantMessage{StopReason: c.reason, Content: c.content}
			if got := ShouldIterate(m); got != c.iterate {
				t.Fatalf("ShouldIterate = %v, want %v", got, c.iterate)
			}
			if got := c.reason.ShortCircuits(); got != c.short {
				t.Fatalf("ShortCircuits = %v, want %v", got, c.short)
			}
		})
	}
}

// --- REQ-GO-08 ------------------------------------------------------------

func TestPushNeverBlocksWithNoConsumer(t *testing.T) {
	s := NewEventStream(StreamOptions{})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			s.Push(TextDeltaEvent{Delta: "tok"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Push blocked with no consumer: this is the channel-based bug OQ-7 forbids")
	}
}

func TestResultWithoutReadingAnyEvent(t *testing.T) {
	s := NewEventStream(StreamOptions{})
	want := &AssistantMessage{Content: Content{TextBlock{Text: "final"}}}
	go func() {
		s.Push(TextDeltaEvent{Delta: "fin"})
		s.End(StreamResult{Message: want})
	}()
	if got := s.Result(); got == nil || got.Content.Text() != "final" {
		t.Fatal("Result must be fed by End, not by consumption")
	}
}

func TestNextDrainsQueuedEventsAfterEnd(t *testing.T) {
	s := NewEventStream(StreamOptions{})
	s.Push(TextDeltaEvent{Delta: "a"})
	s.Push(TextDeltaEvent{Delta: "b"})
	s.End(StreamResult{Message: &AssistantMessage{}})
	n := 0
	for range s.Events() {
		n++
	}
	if n != 2 {
		t.Fatalf("drained %d events, want 2: a done-check before the queue-check loses terminal events", n)
	}
}

// The defect a test that reads events immediately can never see.
func TestSnapshotsAreIndependentDeepCopies(t *testing.T) {
	s := NewEventStream(StreamOptions{})
	call, _ := NewToolUse("t1", "edit", json.RawMessage(`{"path":"a.go"}`))
	live := AssistantMessage{Content: Content{TextBlock{Text: "partial"}, call}}

	s.Push(MessageUpdateEvent{Message: live})

	// Mutate the source the way a streaming accumulator does.
	live.Content[0] = TextBlock{Text: "partial and then some more"}
	live.Content[1].(ToolUseBlock).Input[2] = 'X'

	s.End(StreamResult{Message: &live})
	ev, ok := s.Next()
	if !ok {
		t.Fatal("no event")
	}
	got := ev.(MessageUpdateEvent).Message
	if got.Content.Text() != "partial" {
		t.Fatalf("buffered event saw a later mutation: %q — REQ-OBS-06b violated", got.Content.Text())
	}
	if string(got.Content[1].(ToolUseBlock).Input) != `{"path":"a.go"}` {
		t.Fatalf("Input bytes aliased the live message: %s", got.Content[1].(ToolUseBlock).Input)
	}
}

func TestMaxPendingBytesDropsConsumerNotRun(t *testing.T) {
	s := NewEventStream(StreamOptions{MaxPendingBytes: 1}) // raised to the floor
	big := string(make([]byte, MinMaxPendingBytes+1))
	s.Push(TextDeltaEvent{Delta: big})
	want := &AssistantMessage{Content: Content{TextBlock{Text: "complete"}}}
	s.End(StreamResult{Message: want})

	if _, ok := s.Next(); ok {
		t.Fatal("consumer should be dropped")
	}
	if s.Err() != ErrStreamOverrun {
		t.Fatalf("Err = %v, want ErrStreamOverrun", s.Err())
	}
	if s.Result().Content.Text() != "complete" {
		t.Fatal("the RUN must still complete; only the consumer is dropped")
	}
}

func TestEndIsIdempotentFirstWins(t *testing.T) {
	s := NewEventStream(StreamOptions{})
	s.End(StreamResult{Message: &AssistantMessage{ResponseID: "first"}})
	s.End(StreamResult{Message: &AssistantMessage{ResponseID: "second"}})
	if s.Result().ResponseID != "first" {
		t.Fatal("first End must win")
	}
}

func TestErrorStreamIsPreClosedAndPreservesErr(t *testing.T) {
	sentinel := ErrToolRejected
	s := ErrorStream(nil, sentinel)
	ev, ok := s.Next()
	if !ok || ev.EventType() != EvError {
		t.Fatal("want one ErrorEvent")
	}
	if _, ok := s.Next(); ok {
		t.Fatal("stream must be pre-closed")
	}
	// REQ-PROV-04 shape + REQ-PROV-18 "propagate unmodified" both satisfied.
	if s.Err() != sentinel {
		t.Fatalf("Err = %v, want the original sentinel unwrapped", s.Err())
	}
	if s.Result().StopReason != StopReasonError {
		t.Fatal("want stop_reason=error")
	}
}

// --- REQ-PROV-16 / REQ-GO-15 ---------------------------------------------

func TestUsageReportedZeroBeatsUnreported(t *testing.T) {
	var unreported Usage
	zero := int64(0)
	var reported Usage
	UsageWire{InputTokens: &zero}.Into(&reported)

	if unreported.Reported() {
		t.Fatal("nothing reported must not read as reported")
	}
	if !reported.Reported() {
		t.Fatal("an explicit 0 must set the presence bit (REQ-PROV-16.4)")
	}
	// Both are IsZero: that is the REQ-GO-15 anchor-skip question, and it is a
	// DIFFERENT question from Reported.
	if !reported.IsZero() || !unreported.IsZero() {
		t.Fatal("both are all-zero and must be skipped as anchors")
	}
}

func TestUsageContextTokensFallsBackToComponents(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 1}
	if u.ContextTokens() != 18 {
		t.Fatalf("got %d, want 18", u.ContextTokens())
	}
	u.TotalTokens = 100
	if u.ContextTokens() != 100 {
		t.Fatalf("got %d, want TotalTokens to win", u.ContextTokens())
	}
}

func TestUsageAddSumsAndOrsPresence(t *testing.T) {
	a, b := Usage{}, Usage{}
	in, out := int64(10), int64(20)
	UsageWire{InputTokens: &in}.Into(&a)
	UsageWire{OutputTokens: &out}.Into(&b)
	sum := a.Add(b)
	if sum.InputTokens != 10 || sum.OutputTokens != 20 {
		t.Fatal("field-wise sum wrong")
	}
	if !sum.Has(UsageInputTokens) || !sum.Has(UsageOutputTokens) {
		t.Fatal("presence must OR")
	}
}

// --- REQ-TOOL-13 ----------------------------------------------------------

func TestBatchTerminatesIsAnAnd(t *testing.T) {
	for _, c := range []struct {
		votes []bool
		want  bool
	}{
		{nil, false}, // an empty batch never terminates
		{[]bool{true}, true},
		{[]bool{true, false}, false}, // FAILS on OR semantics
		{[]bool{true, true}, true},
	} {
		if got := BatchTerminates(c.votes); got != c.want {
			t.Fatalf("BatchTerminates(%v) = %v, want %v", c.votes, got, c.want)
		}
	}
}

// --- REQ-TOOL-01 ----------------------------------------------------------

// The projection is exported (the PRD's unexported wire() is unimplementable
// once providers live in their own packages). The invariant is preserved by
// TYPE instead: this test fails the moment someone widens the projection, and
// Request.Tools being []ToolWire means no provider can name a Handler at all.
func TestWireProjectionFieldSet(t *testing.T) {
	want := map[string]bool{
		"Name": true, "Description": true,
		"InputSchema": true, "ConstrainedSampling": true,
	}
	ty := reflect.TypeOf(ToolWire{})
	if ty.NumField() != len(want) {
		t.Fatalf("ToolWire has %d fields, want %d", ty.NumField(), len(want))
	}
	for i := 0; i < ty.NumField(); i++ {
		if !want[ty.Field(i).Name] {
			t.Fatalf("ToolWire gained field %q: the request body just widened", ty.Field(i).Name)
		}
	}
}

func TestWireDropsLoopOnlyFields(t *testing.T) {
	tool := Tool{
		Name: "search", Description: "d", Label: "Search",
		PromptGuidelines: []string{"never sent"},
		ExecutionMode:    Sequential,
		Handler: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			return nil, nil
		},
	}
	w := tool.Wire()
	if w.Name != "search" || w.Description != "d" {
		t.Fatal("wire lost the fields it must carry")
	}
	// Label, PromptGuidelines, ExecutionMode and Handler are structurally
	// unreachable from w — that is the point, and it is a compile-time fact.
}
