package session

import (
	"fmt"

	"github.com/agentfox/agentkit-go/core"
)

// BranchSummaryPrefix and BranchSummarySuffix wrap a branch_summary entry when
// it is rendered into model context as a user message (REQ-SESS-07).
//
// These are MODEL-VISIBLE FORMAT CONTRACT, not presentation: changing them
// changes what every future turn of a branched session reads. They are pinned
// byte-for-byte by TestBranchSummaryWrapperIsPinned, which is the golden
// fixture REQ-SESS-07 asks for.
const (
	BranchSummaryPrefix = "[The conversation was rewound. A summary of the abandoned branch follows.]\n\n"
	BranchSummarySuffix = ""
)

// RenderBranchSummary wraps a branch summary for the model.
func RenderBranchSummary(summary string) string {
	return BranchSummaryPrefix + summary + BranchSummarySuffix
}

// Resume is what a fold produces: the construction inputs for an agent.
//
// REQ-SESS-02 requires the agent to be built AFTER the fold, with the
// recovered model and reasoning level as construction inputs — "not patched
// onto an already-built agent". That is why Fold returns a value rather than
// taking an *Agent: there is no method here that could patch one, and the only
// way to spend a Resume is to pass it to construction. core.ErrSessionNotEmpty
// closes the other half of the hole, at the constructor.
type Resume struct {
	Path   string
	Header core.SessionHeader

	// Provider, API and ModelID are the provenance TRIPLE (P-4). REQ-SESS-02
	// folds "provider and model ID"; REQ-PROV-11 rule 1 computes same_model
	// over (provider, api, model). Recovering two of three makes same_model
	// false on the first post-resume request, which silently downgrades every
	// signed ThinkingBlock to text and strips every thought_signature — a
	// quality regression that appears only after a resume and that no
	// single-process test can see.
	Provider string
	API      core.API
	ModelID  string

	ThinkingLevel core.ThinkingLevel

	// Messages is the flattened active branch, IN FULL. A compaction is an
	// entry, not a rewrite (REQ-SESS-04): the summarized messages stay here
	// and Checkpoint says where the cut is. Applying the cut is the root
	// package's per-request view (REQ-GO-12).
	Messages core.Messages

	// History is the same content as a core.ConversationHistory, with every
	// entry id marked so a checkpoint anchor stays resolvable.
	History *core.ConversationHistory

	Checkpoint    core.CompactionCheckpoint
	HasCheckpoint bool

	// LeafID is the branch this fold followed; Entries is that branch.
	LeafID  core.EntryID
	Entries []core.Entry

	// LoadRepairs are the structural repairs the loader performed;
	// FoldRepairs are the ones the fold itself found. Both are reported,
	// never silent (REQ-SESS-05.4).
	LoadRepairs []Repair
	FoldRepairs []Repair
}

// Repairs returns load and fold repairs together.
func (r Resume) Repairs() []Repair {
	out := make([]Repair, 0, len(r.LoadRepairs)+len(r.FoldRepairs))
	out = append(out, r.LoadRepairs...)
	return append(out, r.FoldRepairs...)
}

// Fold replays one branch and folds session configuration out of it
// (REQ-SESS-02), in the order the requirement gives:
//
//  1. provider, api and model id from the last model_change entry, or absent
//     one from the provenance of the last assistant message;
//  2. reasoning level from the last thinking_level_change;
//  3. the message list.
//
// Note what it does NOT do. A loaded session commonly ends with an unanswered
// tool_use; that is a valid log and an invalid request, and reconciling it is
// the provider's send-time transform, not the loader's (REQ-SESS-06). No
// synthetic tool result is ever inserted here.
func Fold(h core.SessionHeader, branch []core.Entry) Resume {
	r := Resume{
		Header:  h,
		History: core.NewConversationHistory(),
		Entries: branch,
	}
	if n := len(branch); n > 0 {
		r.LeafID = branch[n-1].ID
	}

	// (3) is built first because (1) and (2) are folded from it, but the
	// PRECEDENCE below is REQ-SESS-02's: an explicit change entry always beats
	// message provenance, however late the message is.
	var lastAssistant *core.AssistantMessage
	seen := make(map[core.EntryID]struct{}, len(branch))
	var pendingCheckpoint *core.CompactionCheckpoint

	for _, e := range branch {
		seen[e.ID] = struct{}{}
		switch e.Type {
		case core.EntryMessage:
			if e.Message == nil || e.Message.Message == nil {
				r.History.Record(e.ID)
				continue
			}
			m := e.Message.Message
			r.Messages = append(r.Messages, m)
			r.History.Record(e.ID, m)
			if am, ok := m.(core.AssistantMessage); ok {
				cp := am
				lastAssistant = &cp
			}

		case core.EntryCustomMessage:
			// P-54: REQ-SESS-01 names custom_message as a v1 entry type and
			// defines nothing about it. Minimal shape: host-authored content
			// rendered as a user message. This is invention, not
			// implementation of a stated requirement.
			if e.Custom == nil {
				r.History.Record(e.ID)
				continue
			}
			m := core.UserMessage{Content: e.Custom.Content, Timestamp: e.Timestamp}
			r.Messages = append(r.Messages, m)
			r.History.Record(e.ID, m)

		case core.EntryBranchSummary:
			if e.BranchSummary == nil {
				r.History.Record(e.ID)
				continue
			}
			m := core.UserMessage{
				Content:   core.Content{core.TextBlock{Text: RenderBranchSummary(e.BranchSummary.Summary)}},
				Timestamp: e.Timestamp,
			}
			r.Messages = append(r.Messages, m)
			r.History.Record(e.ID, m)

		case core.EntryModelChange:
			r.History.Record(e.ID)
			if e.ModelChange != nil {
				r.Provider = e.ModelChange.Provider
				r.API = e.ModelChange.API
				r.ModelID = e.ModelChange.ModelID
			}

		case core.EntryThinkingLevelChange:
			r.History.Record(e.ID)
			if e.ThinkingChange != nil {
				r.ThinkingLevel = e.ThinkingChange.Level
			}

		case core.EntryCompaction:
			r.History.Record(e.ID)
			if e.Compaction == nil {
				continue
			}
			anchor := e.Compaction.FirstKeptEntryID
			idx, ok := r.History.IndexOfEntry(anchor)
			if _, inBranch := seen[anchor]; !ok || !inBranch || anchor == core.NullLeaf {
				// P-37. Keeping a checkpoint whose anchor resolves to nothing
				// would drop every message before an entry that does not
				// exist — the whole transcript — from every later request.
				// The entry stays in the log; only its EFFECT is refused.
				r.FoldRepairs = append(r.FoldRepairs, Repair{
					Kind:    RepairUnresolvedAnchor,
					EntryID: e.ID,
					Detail: fmt.Sprintf("first_kept_entry_id %q is not on this branch; checkpoint not applied",
						anchor),
				})
				continue
			}
			cp := core.CompactionCheckpoint{
				PrefixLen: idx,
				Summary:   e.Compaction.Summary,
				EntryID:   e.ID,
				// CreatedAtLen is the message count at the moment the
				// checkpoint was written. It is what makes REQ-GO-15's skip
				// rule (c) computable at all (P-2): nothing on an
				// AssistantMessage records which prefix it was sent under.
				CreatedAtLen: len(r.Messages),
			}
			pendingCheckpoint = &cp

		default:
			// An entry type this build does not model still occupies a
			// position, so a compaction anchor naming it stays resolvable
			// (NFR-TEST-03 c).
			r.History.Record(e.ID)
		}
	}

	if pendingCheckpoint != nil {
		r.Checkpoint = *pendingCheckpoint
		r.HasCheckpoint = true
		r.History.SetCheckpoint(*pendingCheckpoint)
	}

	// (1) Provenance fallback. REQ-SESS-02 says "or absent one from the
	// provenance of the last assistant message" — and provenance is all THREE
	// fields, not provider and model (P-4).
	if r.Provider == "" && r.ModelID == "" && r.API == "" && lastAssistant != nil {
		r.Provider = lastAssistant.Provider
		r.API = lastAssistant.API
		r.ModelID = lastAssistant.Model
	}

	// (2) Reasoning level. The same fallback is applied for the same reason;
	// REQ-SESS-02 states it only for the model, but a resumed session that
	// silently drops back to the default reasoning level is the same class of
	// invisible regression.
	if r.ThinkingLevel == core.ThinkingUnset && lastAssistant != nil {
		r.ThinkingLevel = lastAssistant.ThinkingLevel
	}

	return r
}
