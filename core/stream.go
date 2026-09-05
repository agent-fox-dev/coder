package core

import (
	"iter"
	"sync"
)

// MinMaxPendingBytes is the floor for StreamOptions.MaxPendingBytes.
//
// OQ-7 says the floor is "the largest single event AgentKit can emit", but
// that is not computable: the largest event is a ToolResultEvent bounded only
// by REQ-TOOL-09's output cap, which is configured elsewhere and can be raised
// after the stream is created. A documented constant is a promise something
// can keep.
const MinMaxPendingBytes int64 = 1 << 20

type StreamOptions struct {
	// MaxPendingBytes bounds queued-but-unread payload. Zero is unbounded (the
	// default). A non-zero value below MinMaxPendingBytes is raised to it.
	// Exceeding it drops the CONSUMER (Next returns false, Err returns
	// ErrStreamOverrun) and lets the run finish with the result still
	// available — it never drops events.
	MaxPendingBytes int64
}

// StreamResult is what End records.
type StreamResult struct {
	// Message is non-nil once the stream has ended, always — including on a
	// pre-flight failure (REQ-PROV-04): partial content plus the failure must
	// be one value.
	Message *AssistantMessage
	// Result is set on agent-level streams and nil on provider-level ones.
	Result *RunResult
	Err    error
}

// EventStream is REQ-GO-08's unbounded mutex + condition-variable queue. It is
// not a channel: the producer is a paid model call holding an HTTP connection
// open, and blocking it on a slow consumer stalls the SSE body read, trips the
// provider's stream idle timeout and kills the request. A slow consumer costs
// memory and nothing else.
//
// There is no Close, no channel and no BufferSize.
type EventStream struct {
	mu   sync.Mutex
	cond *sync.Cond

	q       []Event
	head    int
	pending int64
	maxPend int64

	done    bool
	res     StreamResult
	overrun bool
}

func NewEventStream(opts StreamOptions) *EventStream {
	s := &EventStream{maxPend: opts.MaxPendingBytes}
	if s.maxPend > 0 && s.maxPend < MinMaxPendingBytes {
		s.maxPend = MinMaxPendingBytes
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Push never blocks. It deep-copies e before enqueueing (REQ-OBS-06b) —
// centralizing the copy here is what makes the guarantee hold for producers
// that forget. Pushes after End, and after an overrun, are dropped silently.
func (s *EventStream) Push(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || s.overrun {
		return
	}
	n := int64(e.size())
	if s.maxPend > 0 && s.pending+n > s.maxPend {
		// Drop the CONSUMER, not the events, and free the queue so the
		// producer runs to completion at bounded cost.
		s.overrun = true
		s.q, s.head, s.pending = nil, 0, 0
		s.cond.Broadcast()
		return
	}
	s.q = append(s.q, e.clone())
	s.pending += n
	s.cond.Broadcast()
}

// End records the terminal result and wakes everyone. It is idempotent; the
// first call wins. The producer pushes the terminal EVENT immediately before
// calling End, so a consumer sees that event and then ok=false.
//
// Every producer MUST `defer stream.End(...)`: Result blocks until it lands.
func (s *EventStream) End(res StreamResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done, s.res = true, res
	s.cond.Broadcast()
}

// Next blocks until an event is available or the stream is done. Events queued
// before End are drained before it reports done, so End never loses events.
//
// Single-consumer: two goroutines calling Next split the stream. Fan-out is
// the caller's concern.
func (s *EventStream) Next() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.head < len(s.q) {
			e := s.q[s.head]
			s.q[s.head] = nil
			s.head++
			s.pending -= int64(e.size())
			if s.head == len(s.q) {
				s.q, s.head = s.q[:0], 0
			}
			return e, true
		}
		if s.overrun || s.done {
			return nil, false
		}
		s.cond.Wait()
	}
}

// Events is the range-over-func iterator of REQ-GO-08. Breaking out early is
// safe: the producer is never blocked on this consumer.
func (s *EventStream) Events() iter.Seq[Event] {
	return func(yield func(Event) bool) {
		for {
			e, ok := s.Next()
			if !ok || !yield(e) {
				return
			}
		}
	}
}

// Wait blocks until End and returns the terminal result.
func (s *EventStream) Wait() StreamResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.done {
		s.cond.Wait()
	}
	return s.res
}

// Result blocks until End and returns the assistant message. It works even if
// no event was ever read — the result is fed by End, not by consumption, and
// that is what makes abandoning a stream safe (REQ-GO-08).
func (s *EventStream) Result() *AssistantMessage { return s.Wait().Message }

// RunResult blocks until End and returns the agent-level result. It is the
// run-facing accessor; on a provider-level stream Result is zero and the error
// explains why. §7's sketch conflates the two — Result() there is written as
// though it yields both an AssistantMessage and a RunResult.
func (s *EventStream) RunResult() (RunResult, error) {
	r := s.Wait()
	if r.Result == nil {
		return RunResult{}, r.Err
	}
	return *r.Result, r.Err
}

// Err does not block. It reports ErrStreamOverrun for a dropped consumer even
// when the run itself succeeded.
func (s *EventStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overrun {
		return ErrStreamOverrun
	}
	return s.res.Err
}

// ErrorStream returns an already-ended stream carrying one ErrorEvent. This is
// REQ-PROV-04's pre-closed error stream, for failures raised before any bytes
// are sent — an unregistered Api, an unusable option, an OnPayload abort.
//
// The original error is retrievable unwrapped through Err, so errors.Is and
// errors.As still work on the caller's own sentinel. That is how REQ-PROV-04
// ("failures are encoded in the stream") and REQ-PROV-18 ("an OnPayload error
// must propagate to the caller unmodified") are both satisfied by a function
// whose only return is *EventStream.
func ErrorStream(msg *AssistantMessage, err error) *EventStream {
	s := NewEventStream(StreamOptions{})
	s.Push(ErrorEvent{Message: err.Error(), Err: err, Terminal: true})
	if msg == nil {
		msg = &AssistantMessage{StopReason: StopReasonError, ErrorMessage: err.Error()}
	}
	s.End(StreamResult{Message: msg, Err: err})
	return s
}
