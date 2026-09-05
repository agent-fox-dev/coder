// Package agentkit is a dependency-free Go agent SDK.
//
// The loop, the tool system and the provider abstraction are ordinary Go you
// can read and step through. Nothing is hidden inside a subprocess or a graph
// engine.
//
// The canonical vocabulary lives in the core package and is re-exported here
// by type alias, so agentkit.Tool and core.Tool are the same type and no
// conversion exists anywhere.
package agentkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentfox/agentkit-go/core"
	"github.com/agentfox/agentkit-go/session"
)

// Agent holds its own config, tool registry, history and middleware chain. It
// has no global state: creating a child agent for delegation is constructing a
// new Agent value (§5, Agent as Value Object).
//
// Value semantics govern COMPOSITION, not immutability. The model and thinking
// level are mutable mid-session (REQ-SESS-03), messages may be queued into a
// live run (REQ-LOOP-13/14), and a run in flight is guarded by a run slot
// (REQ-LOOP-15) — so "no global state" does not imply "no concurrency rules".
//
// Every exported method is safe to call from any goroutine while a turn is in
// flight (REQ-LIFE-03). Conflicting operations fail with ErrBusy rather than
// queueing, and never block: a prompt queued behind a running turn was written
// against a transcript the caller could see, and by the time it would run the
// transcript has changed underneath it. That decision belongs to the caller.
type Agent struct {
	// producerID is fixed for the lifetime of this Agent value and is what
	// makes a Revision comparable (REQ-LIFE-02). A restored Agent gets a fresh
	// one, so a consumer holding a pre-restore snapshot must compare
	// ProducerID BEFORE Revision.
	producerID string

	// phase is an atomic because REQ-LIFE-01 requires Phase() to be cheap,
	// non-blocking and callable from inside a turn hook or tool interceptor.
	// A mutex here would be held across a model call by any careless read.
	phase atomic.Int32

	// mu guards everything below. It is NEVER held while user code runs — no
	// tool handler, interceptor, hook or event listener (NFR-REL-02.1). It is
	// also never acquired while a batch's finalize mutex is held; that lock
	// order (batch.mu -> stream.mu, a.mu never under batch.mu) is the
	// reconciliation of REQ-LOOP-05 with NFR-REL-02.1 (ruling P-6).
	mu sync.Mutex

	cfg     core.AgentConfig
	tools   []core.Tool
	history *core.ConversationHistory
	// rec is the single write path for anything that must survive a restart
	// (REQ-SESS-03). It is never nil: with no SessionStore configured it is a
	// recorder over a nil store, which still updates history. That uniformity
	// is what stops the persisted and non-persisted paths drifting apart —
	// the bug where a session log is built, tested, and never actually
	// written to during a run.
	rec *session.Recorder

	// running is the run slot (REQ-LOOP-15). It is claimed BEFORE the queues
	// are drained, under this one lock: claiming after draining lets a
	// concurrent Run win the slot and silently discard the drained messages.
	running bool
	// cancelRun is the run's OWN canceller, stored at start. Abort() takes no
	// context precisely so a caller that does not own the Run goroutine — a
	// signal handler, an RPC handler, a UI event loop — can stop the turn
	// (REQ-LIFE-04).
	cancelRun context.CancelFunc
	aborted   bool

	steering []core.Message
	followUp []core.Message

	// holds is REQ-LIFE-06's operation refcount. It exists so an owner with
	// in-flight work of its own can keep the agent from being reclaimed
	// without owning the Run goroutine.
	holds int

	usage core.Usage
}

// NewAgent constructs an Agent. Credentials, catalog lookup and provider
// registration are the caller's; cfg.Model must already be resolved.
//
// A non-empty SessionStore is rejected with ErrSessionNotEmpty: resuming means
// folding the log and passing the recovered configuration to construction
// (REQ-SESS-02), not building an agent and patching a model onto it.
func NewAgent(cfg core.AgentConfig) (*Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agentkit: AgentConfig.Model is nil; resolve it with catalog.ResolveModel first")
	}
	if cfg.SessionStore != nil {
		if len(cfg.SessionStore.Entries()) > 0 {
			return nil, core.ErrSessionNotEmpty
		}
	}
	return newAgent(cfg, core.NewConversationHistory()), nil
}

// NewAgentWithHistory is the resume constructor: the recovered model, thinking
// level and messages are CONSTRUCTION INPUTS (REQ-SESS-02), not post-hoc
// mutations.
func NewAgentWithHistory(cfg core.AgentConfig, h *core.ConversationHistory) (*Agent, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("agentkit: AgentConfig.Model is nil; resolve it with catalog.ResolveModel first")
	}
	if h == nil {
		h = core.NewConversationHistory()
	}
	return newAgent(cfg, h), nil
}

func newAgent(cfg core.AgentConfig, h *core.ConversationHistory) *Agent {
	a := &Agent{producerID: newID("prod"), cfg: cfg, history: h}
	a.rec = session.NewRecorder(cfg.SessionStore, h, cfg.OnPersistError)
	a.tools = append(a.tools, cfg.ToolPolicy.CustomTools...)
	return a
}

func newID(prefix string) string {
	var b [8]byte
	// rand.Read from crypto/rand cannot fail on any supported platform; since
	// Go 1.24 it panics rather than returning an error.
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// RegisterTool adds a tool to the registry. It is not safe to call while a run
// is in flight and returns ErrBusy in that case: the tool list is part of the
// cached prompt prefix, and mutating it mid-turn would silently change what
// the model was shown.
func (a *Agent) RegisterTool(t core.Tool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return core.ErrBusy
	}
	if t.Handler == nil && t.Execute == nil {
		return fmt.Errorf("agentkit: tool %q has neither Handler nor Execute", t.Name)
	}
	if t.Handler != nil && t.Execute != nil {
		return fmt.Errorf("agentkit: tool %q sets both Handler and Execute; exactly one", t.Name)
	}
	for _, ex := range a.tools {
		if ex.Name == t.Name {
			return fmt.Errorf("agentkit: tool %q already registered", t.Name)
		}
	}
	a.tools = append(a.tools, t)
	return nil
}

// Tools returns the resolved tool set for a run, after ToolPolicy resolution
// (REQ-TOOL-10).
func (a *Agent) Tools() []core.Tool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return ResolveToolPolicy(a.tools, a.cfg.ToolPolicy)
}

// ---------------------------------------------------------------- lifecycle

// Phase reports what the agent is doing right now (REQ-LIFE-01). Cheap,
// non-blocking, safe from any goroutine including from inside a hook.
func (a *Agent) Phase() core.Phase { return core.Phase(a.phase.Load()) }

func (a *Agent) setPhase(p core.Phase) { a.phase.Store(int32(p)) }

// Idle reports whether the agent can safely be disposed of or its history
// persisted as complete (REQ-LIFE-06).
func (a *Agent) Idle() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Phase() == core.PhaseIdle && a.holds == 0 && !a.running
}

// Hold raises the operation refcount and returns its release. It lets an owner
// with in-flight work of its own keep the agent from being reclaimed
// underneath it without owning the Run goroutine (REQ-LIFE-06). Release is
// idempotent.
func (a *Agent) Hold() (release func()) {
	a.mu.Lock()
	a.holds++
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			if a.holds > 0 {
				a.holds--
			}
			a.mu.Unlock()
		})
	}
}

// Abort cancels the in-flight turn. It takes no arguments and no context
// (REQ-LIFE-04): the run stores its own canceller at start, so a caller that
// does not own the Run goroutine can stop it. Idempotent, and a no-op when
// idle.
func (a *Agent) Abort() {
	a.mu.Lock()
	cancel := a.cancelRun
	if a.running {
		a.aborted = true
	}
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Snapshot returns the authoritative session state, safe to serialize and safe
// to read concurrently with a running turn (REQ-LIFE-02).
//
// A snapshot taken while Idle() is false is a CONSISTENT view, not a COMPLETE
// one: the in-flight turn is not in it (REQ-LIFE-07). Callers persisting for
// resume either wait for Idle or accept that the interrupted turn replays from
// its last completed turn boundary.
func (a *Agent) Snapshot(ctx context.Context) (core.SessionSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return core.SessionSnapshot{}, err
	}
	a.mu.Lock()
	cfg, usage := a.cfg, a.usage
	names := make([]string, 0, len(a.tools))
	for _, t := range ResolveToolPolicy(a.tools, cfg.ToolPolicy) {
		names = append(names, t.Name)
	}
	idle := a.Phase() == core.PhaseIdle && a.holds == 0 && !a.running
	a.mu.Unlock()

	return core.SessionSnapshot{
		ProducerID: a.producerID,
		Revision:   a.history.Revision(),
		Phase:      a.Phase(),
		Idle:       idle,
		SessionID:  cfg.SessionID,
		Config: core.ConfigView{
			ModelID:       cfg.Model.ID,
			Provider:      cfg.Model.Provider,
			API:           cfg.Model.API,
			MaxTokens:     cfg.MaxTokens,
			ThinkingLevel: cfg.ThinkingLevel,
			ToolNames:     names,
		},
		Messages: a.history.CloneBranch(),
		Usage:    usage,
		TakenAt:  time.Now(),
	}, nil
}

// History exposes the in-memory active branch. It is a view; the durable
// representation is the session log (NFR-REL-04).
func (a *Agent) History() *core.ConversationHistory { return a.history }

// Usage returns cumulative usage for the agent's lifetime.
func (a *Agent) Usage() core.Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage
}

// SetModel changes the model mid-session and appends the log entry that makes
// the change recoverable (REQ-SESS-03). A change not written into the log at
// the moment it happens is not recoverable by the fold.
//
// The entry carries the (provider, api, model) TRIPLE, not just provider and
// model: REQ-PROV-11 rule 1 needs all three to decide same_model, and
// two-of-three makes it false on the first post-resume request, silently
// downgrading every signed thinking block to plain text (ruling P-4).
func (a *Agent) SetModel(m *core.Model) error {
	if m == nil {
		return fmt.Errorf("agentkit: SetModel(nil)")
	}
	a.mu.Lock()
	a.cfg.Model = m
	rec := a.rec
	a.mu.Unlock()

	_, err := rec.RecordModelChange(m.Provider, m.API, m.ID)
	return err
}

// SetThinkingLevel changes the reasoning level mid-session and logs it
// (REQ-SESS-03).
func (a *Agent) SetThinkingLevel(l core.ThinkingLevel) error {
	a.mu.Lock()
	a.cfg.ThinkingLevel = l
	rec := a.rec
	a.mu.Unlock()

	_, err := rec.RecordThinkingLevel(l)
	return err
}

// ------------------------------------------------------------------- queues

// Steer enqueues a message for delivery into the CURRENT run (REQ-LOOP-13).
// The queue is drained at the head of the next iteration, immediately before
// the provider request — never between an assistant response and its tool
// results, which would violate REQ-LOOP-02.
//
// Calling Steer while idle is allowed: REQ-LOOP-16's assistant branch is
// otherwise unreachable and Continue would always error (ruling P-34).
func (a *Agent) Steer(msgs ...core.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steering = append(a.steering, msgs...)
	return nil
}

// SteerText is the common case.
func (a *Agent) SteerText(s string) error {
	return a.Steer(core.UserMessage{Content: core.Content{core.TextBlock{Text: s}}, Timestamp: time.Now()})
}

// FollowUp enqueues a message polled only when the inner loop is exhausted
// (REQ-LOOP-14). A pending follow-up restarts the outer loop within the same
// run: no second AgentStartEvent, no AgentDoneEvent, one RunResult.
func (a *Agent) FollowUp(msgs ...core.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUp = append(a.followUp, msgs...)
	return nil
}

// FollowUpText is the common case.
func (a *Agent) FollowUpText(s string) error {
	return a.FollowUp(core.UserMessage{Content: core.Content{core.TextBlock{Text: s}}, Timestamp: time.Now()})
}

func (a *Agent) ClearSteeringQueue() { a.mu.Lock(); a.steering = nil; a.mu.Unlock() }
func (a *Agent) ClearFollowUpQueue() { a.mu.Lock(); a.followUp = nil; a.mu.Unlock() }

func (a *Agent) ClearAllQueues() {
	a.mu.Lock()
	a.steering, a.followUp = nil, nil
	a.mu.Unlock()
}

// HasQueuedMessages reports whether either queue is non-empty.
func (a *Agent) HasQueuedMessages() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.steering) > 0 || len(a.followUp) > 0
}

// drainSteeringLocked pops per the configured queue mode. Caller holds a.mu.
func (a *Agent) drainSteeringLocked() []core.Message {
	return drainLocked(&a.steering, a.cfg.SteeringQueueMode)
}

func (a *Agent) drainFollowUpLocked() []core.Message {
	return drainLocked(&a.followUp, a.cfg.FollowUpQueueMode)
}

func drainLocked(q *[]core.Message, mode core.QueueMode) []core.Message {
	if len(*q) == 0 {
		return nil
	}
	if mode == core.QueueDrainAll {
		out := *q
		*q = nil
		return out
	}
	out := []core.Message{(*q)[0]}
	*q = append([]core.Message(nil), (*q)[1:]...)
	return out
}

// ----------------------------------------------------------------- run slot

// claimSlot claims the run slot and drains the steering queue UNDER THE SAME
// LOCK (REQ-LOOP-15). Claiming after draining lets a concurrent Run win the
// slot and silently discard the drained messages, which is a real lost-message
// race, not a theoretical one.
func (a *Agent) claimSlot(ctx context.Context) (context.Context, context.CancelFunc, []core.Message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return nil, nil, nil, core.ErrBusy
	}
	a.running = true
	a.aborted = false
	rctx, cancel := context.WithCancel(ctx)
	a.cancelRun = cancel
	pending := a.drainSteeringLocked()
	return rctx, cancel, pending, nil
}

func (a *Agent) releaseSlot() {
	a.mu.Lock()
	a.running = false
	a.cancelRun = nil
	a.mu.Unlock()
	a.setPhase(core.PhaseIdle)
}

func (a *Agent) wasAborted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.aborted
}
