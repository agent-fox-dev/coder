package session

import (
	"fmt"

	"github.com/agentfox/agentkit-go/core"
)

// Recorder writes loop events into a session log and keeps a
// core.ConversationHistory in step with it.
//
// It exists because REQ-SESS-03 makes a configuration change durable only if
// it is written into the same ordered log at the moment it happens, and
// because REQ-SESS-08 requires an explicit OnPersistError when the SDK
// subscribes the store to loop events internally: the caller of SetModel does
// not see Append's return value, so something has to carry the error out.
//
// Every method returns its error AND routes it to onErr. Both, deliberately:
// the return is for a caller that can act, the hook is for the loop path that
// cannot.
type Recorder struct {
	store   core.SessionStore
	history *core.ConversationHistory
	onErr   func(error)
}

// SessionRecorder is the name the implementation plan uses for this type.
type SessionRecorder = Recorder

// NewRecorder binds a store to a history. Either may be nil: a nil store makes
// every Record a no-op that still updates history (useful for a session that
// is not persisted), and a nil history means only the log is maintained.
func NewRecorder(store core.SessionStore, history *core.ConversationHistory, onErr func(error)) *Recorder {
	return &Recorder{store: store, history: history, onErr: onErr}
}

// Store returns the underlying store, or nil.
func (r *Recorder) Store() core.SessionStore { return r.store }

// History returns the in-memory view, or nil.
func (r *Recorder) History() *core.ConversationHistory { return r.history }

// Head is the active leaf, or core.NullLeaf when there is no store.
func (r *Recorder) Head() core.EntryID {
	if r.store == nil {
		return core.NullLeaf
	}
	return r.store.Head()
}

func (r *Recorder) append(e core.Entry) (core.EntryID, error) {
	if r.store == nil {
		if e.ID == core.NullLeaf {
			e.ID = randomID()
		}
		return e.ID, nil
	}
	if err := r.store.Append(e); err != nil {
		if r.onErr != nil {
			r.onErr(err)
		}
		return core.NullLeaf, err
	}
	return r.store.Head(), nil
}

// RecordMessage appends a message entry and records it in history under the
// id the store assigned, so a later compaction anchor resolves.
func (r *Recorder) RecordMessage(m core.Message) (core.EntryID, error) {
	id, err := r.append(NewMessageEntry(m))
	if err != nil {
		return core.NullLeaf, err
	}
	if r.history != nil {
		r.history.Record(id, m)
	}
	return id, nil
}

// RecordModelChange appends a model_change entry (REQ-SESS-03). It takes the
// provenance TRIPLE: recording provider and model without api is P-4.
func (r *Recorder) RecordModelChange(provider string, api core.API, modelID string) (core.EntryID, error) {
	id, err := r.append(NewModelChangeEntry(provider, api, modelID))
	if err != nil {
		return core.NullLeaf, err
	}
	if r.history != nil {
		r.history.Record(id)
	}
	return id, nil
}

// RecordThinkingLevel appends a thinking_level_change entry (REQ-SESS-03).
func (r *Recorder) RecordThinkingLevel(l core.ThinkingLevel) (core.EntryID, error) {
	id, err := r.append(NewThinkingLevelEntry(l))
	if err != nil {
		return core.NullLeaf, err
	}
	if r.history != nil {
		r.history.Record(id)
	}
	return id, nil
}

// RecordCompaction appends a compaction entry and installs the matching
// in-memory checkpoint (REQ-SESS-04). The summarized entries stay in the file
// and in history; only the per-request view drops them.
//
// firstKept must name an entry already in the log, and that is CHECKED before
// anything is appended. An unknown anchor used to be written as-is: the
// checkpoint then had PrefixLen 0 (nothing dropped, so the compaction bought
// nothing) and every later load reported RepairUnresolvedAnchor for an entry
// the store itself had produced — the store manufacturing the damage class
// P-37 exists to detect. The log is append-only, so the only place to refuse
// it is here. CreatedAtLen is taken from history at the moment of the call,
// which is exactly what REQ-GO-15's skip rule (c) needs (P-2).
func (r *Recorder) RecordCompaction(summary string, firstKept core.EntryID, previous string) (core.CompactionCheckpoint, error) {
	if !r.hasEntry(firstKept) {
		err := fmt.Errorf("session: compaction anchor: %w: %q", ErrUnknownEntry, firstKept)
		if r.onErr != nil {
			r.onErr(err)
		}
		return core.CompactionCheckpoint{}, err
	}
	id, err := r.append(NewCompactionEntry(summary, firstKept, previous))
	if err != nil {
		return core.CompactionCheckpoint{}, err
	}
	cp := core.CompactionCheckpoint{Summary: summary, EntryID: id}
	if r.history != nil {
		r.history.Record(id)
		if idx, ok := r.history.IndexOfEntry(firstKept); ok {
			cp.PrefixLen = idx
		}
		cp.CreatedAtLen = r.history.Len()
		r.history.SetCheckpoint(cp)
	}
	return cp, nil
}

// hasEntry reports whether id can anchor a compaction: an entry the log
// holds, or — for a recorder with no store — one history has seen. With
// neither there is nothing to check against and the call is a no-op anyway,
// so it is allowed; the empty id is never allowed, because it is the
// NullLeaf sentinel and the loader treats it as unresolved.
func (r *Recorder) hasEntry(id core.EntryID) bool {
	if id == core.NullLeaf {
		return false
	}
	if r.store != nil {
		if s, ok := r.store.(interface{ Has(core.EntryID) bool }); ok {
			return s.Has(id)
		}
		for _, e := range r.store.Entries() {
			if e.ID == id {
				return true
			}
		}
		return false
	}
	if r.history != nil {
		_, ok := r.history.IndexOfEntry(id)
		return ok
	}
	return true
}

// RecordCustom appends a custom_message entry and renders it into history as a
// user message, matching Fold (P-54).
func (r *Recorder) RecordCustom(kind string, c core.Content) (core.EntryID, error) {
	id, err := r.append(NewCustomMessageEntry(kind, c))
	if err != nil {
		return core.NullLeaf, err
	}
	if r.history != nil {
		r.history.Record(id, core.UserMessage{Content: c})
	}
	return id, nil
}

// RecordBranchSummary appends a branch_summary entry and renders it into
// history with the fixed wrapper of REQ-SESS-07.
func (r *Recorder) RecordBranchSummary(summary string, fromLeaf, forkPoint core.EntryID) (core.EntryID, error) {
	id, err := r.append(NewBranchSummaryEntry(summary, fromLeaf, forkPoint))
	if err != nil {
		return core.NullLeaf, err
	}
	if r.history != nil {
		r.history.Record(id, core.UserMessage{
			Content: core.Content{core.TextBlock{Text: RenderBranchSummary(summary)}},
		})
	}
	return id, nil
}

// Sync flushes the store (REQ-SESS-09 / P-36). A caller that wants
// fsync-per-turn calls this at its own turn boundary; the store cannot see a
// turn.
func (r *Recorder) Sync() error {
	if r.store == nil {
		return nil
	}
	err := r.store.Sync()
	if err != nil && r.onErr != nil {
		r.onErr(err)
	}
	return err
}
