package core

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/agentfox/agentkit-go/jsonx"
)

type EntryID string

// NullLeaf is the explicit "before the first entry" state (REQ-SESS-07).
const NullLeaf EntryID = ""

type EntryType string

const (
	EntryMessage             EntryType = "message"
	EntryModelChange         EntryType = "model_change"
	EntryThinkingLevelChange EntryType = "thinking_level_change"
	EntryCompaction          EntryType = "compaction"
	EntryCustomMessage       EntryType = "custom_message"
	EntryBranchSummary       EntryType = "branch_summary"
)

const SessionLogVersion = 1

// SessionHeader is line 1 of the log (REQ-SESS-01).
type SessionHeader struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	CWD       string    `json:"cwd"`
}

// Entry is one line of the log. It is IMMUTABLE once constructed: Raw is
// authoritative for every write path, so mutating a payload after decode is
// silently ignored. To change something, append a new entry.
type Entry struct {
	ID        EntryID   `json:"id"`
	ParentID  EntryID   `json:"parent_id"`
	Type      EntryType `json:"type"`
	Timestamp time.Time `json:"timestamp"`

	Message        *MessageEntry             `json:"message,omitzero"`
	ModelChange    *ModelChangeEntry         `json:"model_change,omitzero"`
	ThinkingChange *ThinkingLevelChangeEntry `json:"thinking_level_change,omitzero"`
	Compaction     *CompactionEntry          `json:"compaction,omitzero"`
	Custom         *CustomMessageEntry       `json:"custom_message,omitzero"`
	BranchSummary  *BranchSummaryEntry       `json:"branch_summary,omitzero"`

	// Raw is the entry's bytes exactly as they appeared in the log, or exactly
	// as they were marshalled on Append.
	//
	// Raw passthrough is what makes NFR-TEST-03 achievable at all. It is not
	// only for unknown entry types: decode-then-re-encode of a MODELLED entry
	// also reorders keys and rewrites numeric literals, so REQ-SESS-05.2 and
	// NFR-TEST-03(d) together force raw retention for every entry.
	Raw json.RawMessage `json:"-"`
}

// Known reports whether this build models e.Type.
func (e Entry) Known() bool {
	switch e.Type {
	case EntryMessage, EntryModelChange, EntryThinkingLevelChange,
		EntryCompaction, EntryCustomMessage, EntryBranchSummary:
		return true
	}
	return false
}

// MessageEntry nests the message under "message" rather than flattening it:
// Message carries its own timestamp, which would collide with the envelope's.
type MessageEntry struct {
	Message Message `json:"message"`
}

// ModelChangeEntry carries the full provenance TRIPLE, not just provider and
// model. REQ-PROV-11 rule 1 computes same_model over (provider, api, model); a
// fold that recovers two of three makes same_model false on the first
// post-resume request and silently downgrades every replayed thinking block to
// text — a regression visible only after a resume.
type ModelChangeEntry struct {
	Provider string `json:"provider"`
	API      API    `json:"api"`
	ModelID  string `json:"model_id"`
}

type ThinkingLevelChangeEntry struct {
	Level ThinkingLevel `json:"thinking_level"`
}

// CompactionEntry is the durable form of a checkpoint (REQ-SESS-04). The
// anchor is an entry id, not an index: indices do not survive re-parenting.
type CompactionEntry struct {
	Summary          string  `json:"summary"`
	FirstKeptEntryID EntryID `json:"first_kept_entry_id"`
	// PreviousSummary is the summary this one extended, "" for the first.
	// Recorded for audit; the fold reads only the last entry.
	PreviousSummary string `json:"previous_summary,omitzero"`
}

// CustomMessageEntry is a host-authored transcript entry that is not a model
// turn. REQ-SESS-01 names the type and defines nothing about it; this is the
// minimal shape, rendered into model context as a UserMessage.
type CustomMessageEntry struct {
	Kind    string  `json:"kind"`
	Content Content `json:"content"`
}

type BranchSummaryEntry struct {
	Summary     string  `json:"summary"`
	FromLeafID  EntryID `json:"from_leaf_id"`
	ForkPointID EntryID `json:"fork_point_id"`
}

// SessionStore is the durable representation of a session (NFR-REL-04). It
// knows nothing about tool_use/tool_result pairing, compaction views, or
// provider repair: structural log repair is not transcript repair
// (REQ-SESS-06).
type SessionStore interface {
	Header() SessionHeader
	// Append durably records one entry parented at Head and advances Head.
	// It returns its error; callers must not discard it (REQ-SESS-08).
	Append(e Entry) error
	Entries() []Entry
	// Branch returns the root->leaf path. Branch(NullLeaf) returns nil, nil.
	Branch(leafID EntryID) ([]Entry, error)
	Leaves() []EntryID
	Head() EntryID
	// ForkFrom repoints Head so the next Append creates a second child of id.
	// Nothing is rewritten; both branches stay in one file.
	ForkFrom(id EntryID) error
	// Sync is the escape hatch for a caller wanting fsync-per-turn: the store
	// cannot observe a turn boundary without importing loop state.
	Sync() error
	Close() error
}

// CompactionCheckpoint is the in-memory form of REQ-SESS-04's entry.
type CompactionCheckpoint struct {
	// PrefixLen indexes the ORIGINAL, only-ever-appended message list
	// (REQ-GO-12.1) — not the view.
	PrefixLen int
	Summary   string
	EntryID   EntryID
	// CreatedAtLen is len(original) when this checkpoint was created. It is
	// what makes REQ-GO-15's skip rule (c) computable: an assistant message at
	// an original index below it was produced under a different prefix.
	// Nothing on AssistantMessage records which prefix it was sent under, so
	// the rule is not evaluable from []Message alone.
	CreatedAtLen int
}

// MinAnchorIndex maps the checkpoint into the compacted view's index space.
// View index v>=1 maps to original index PrefixLen+v-1, so an anchor is stale
// iff PrefixLen+v-1 < CreatedAtLen.
func (cp CompactionCheckpoint) MinAnchorIndex() int {
	if cp.PrefixLen == 0 {
		return 0
	}
	n := cp.CreatedAtLen - cp.PrefixLen + 1
	if n < 0 {
		return 0
	}
	return n
}

// ConversationHistory is the in-memory flattened active branch. It is a VIEW
// derived from a SessionStore branch; the log remains the durable
// representation (NFR-REL-04).
//
// It deliberately has no MarshalJSON: serializing []Message is not a
// conforming way to persist a session (Appendix A correction 17).
type ConversationHistory struct {
	mu       sync.RWMutex
	items    []historyItem
	marks    map[EntryID]int
	revision uint64
	cp       *CompactionCheckpoint
}

type historyItem struct {
	EntryID EntryID
	Msg     Message
}

func NewConversationHistory() *ConversationHistory {
	return &ConversationHistory{marks: map[EntryID]int{}}
}

// Record notes that entry id contributed msgs (possibly none) to the flattened
// view. Non-message entries record zero messages so their position stays
// resolvable by IndexOfEntry — required when a compaction entry's
// first_kept_entry_id names an entry type this build does not model
// (NFR-TEST-03c).
func (h *ConversationHistory) Record(id EntryID, msgs ...Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.marks == nil {
		h.marks = map[EntryID]int{}
	}
	if _, seen := h.marks[id]; !seen && id != NullLeaf {
		h.marks[id] = len(h.items)
	}
	for _, m := range msgs {
		h.items = append(h.items, historyItem{EntryID: id, Msg: m})
	}
	h.revision++
}

func (h *ConversationHistory) Messages() Messages {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(Messages, len(h.items))
	for i, it := range h.items {
		out[i] = it.Msg
	}
	return out
}

// CloneBranch returns a deep copy, for snapshots that must not alias history.
func (h *ConversationHistory) CloneBranch() Messages {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(Messages, len(h.items))
	for i, it := range h.items {
		out[i] = it.Msg.Clone()
	}
	return out
}

func (h *ConversationHistory) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.items)
}

func (h *ConversationHistory) LastRole() (Role, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.items) == 0 {
		return "", false
	}
	return h.items[len(h.items)-1].Msg.Role(), true
}

func (h *ConversationHistory) IndexOfEntry(id EntryID) (int, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	i, ok := h.marks[id]
	return i, ok
}

func (h *ConversationHistory) EntryIDAt(i int) EntryID {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if i < 0 || i >= len(h.items) {
		return NullLeaf
	}
	return h.items[i].EntryID
}

// Revision increments on every mutation, for SessionSnapshot (REQ-LIFE-02).
func (h *ConversationHistory) Revision() uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.revision
}

func (h *ConversationHistory) Checkpoint() (CompactionCheckpoint, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.cp == nil {
		return CompactionCheckpoint{}, false
	}
	return *h.cp, true
}

func (h *ConversationHistory) SetCheckpoint(cp CompactionCheckpoint) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cp = &cp
	h.revision++
}

// Unknown-key retention helper shared by the message codecs.
type unknownKeys = jsonx.OrderedObject
