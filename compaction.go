package agentkit

import (
	"context"
	"fmt"
	"strings"

	"github.com/agentfox/agentkit-go/core"
)

// CompactionSummaryPrefix wraps a summary when it is rendered into the model
// context. It is MODEL-VISIBLE format contract and is pinned by a golden test:
// changing it changes what every compacted session says to the model.
const CompactionSummaryPrefix = "[Earlier conversation, summarized]\n\n"

// CompactionStrategy decides whether and where to compact.
type CompactionStrategy interface {
	// ShouldCompact reports whether the view needs compacting, given the
	// anchored estimate and the model's context window.
	ShouldCompact(estTokens, contextWindow int) bool
	// CutIndex chooses the boundary. It returns the index of the first KEPT
	// message, and must return a valid cut point per CutPolicy.
	CutIndex(msgs core.Messages, contextWindow int) int
	// CutPolicy constrains which indices are valid boundaries.
	CutPolicy() CutPolicy
}

// CutPolicy constrains where a compaction boundary may fall.
type CutPolicy int

const (
	// CutNotToolResult: the boundary may not be a tool result, because that
	// orphans the tool_use above it (REQ-GO-14).
	CutNotToolResult CutPolicy = iota
	// CutUserOnly is strictly stronger and is required for the WINDOW
	// strategies (ruling P-8).
	//
	// REQ-GO-14's "not a tool_result" is necessary but not sufficient: it
	// permits a cut landing on an ASSISTANT message. SummarizationCompaction
	// is fine there because it prepends a summary user message, but
	// TurnWindow and TokenWindow have nothing to prepend, so they would
	// produce a view starting on an assistant turn — which Anthropic rejects
	// and which REQ-PROV-11 does not repair, since rule 6 covers only
	// orphaned tool_use blocks.
	CutUserOnly
)

// ---------------------------------------------------------------- strategies

// NoCompaction never compacts.
type NoCompaction struct{}

func (NoCompaction) ShouldCompact(int, int) bool     { return false }
func (NoCompaction) CutIndex(core.Messages, int) int { return 0 }
func (NoCompaction) CutPolicy() CutPolicy            { return CutNotToolResult }

// TurnWindowCompaction keeps the most recent MaxTurns user turns.
type TurnWindowCompaction struct{ MaxTurns int }

func (t TurnWindowCompaction) CutPolicy() CutPolicy { return CutUserOnly }

func (t TurnWindowCompaction) ShouldCompact(est, window int) bool { return true }

func (t TurnWindowCompaction) CutIndex(msgs core.Messages, _ int) int {
	seen := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if _, ok := msgs[i].(core.UserMessage); ok {
			seen++
			if seen > t.MaxTurns {
				return snapForward(msgs, i+1, CutUserOnly)
			}
		}
	}
	return 0
}

// TokenWindowCompaction keeps the most recent KeepTokens' worth of messages.
type TokenWindowCompaction struct{ KeepTokens int }

func (t TokenWindowCompaction) CutPolicy() CutPolicy { return CutUserOnly }

func (t TokenWindowCompaction) ShouldCompact(est, window int) bool {
	return est > t.KeepTokens
}

func (t TokenWindowCompaction) CutIndex(msgs core.Messages, _ int) int {
	return cutByTokens(msgs, t.KeepTokens, CutUserOnly)
}

// SummarizationCompaction replaces the summarized prefix with a model-written
// summary.
type SummarizationCompaction struct {
	// ThresholdFraction of the context window at which compaction fires.
	ThresholdFraction float64
	// KeepTokens is the size of the tail kept verbatim.
	KeepTokens int
}

func (s SummarizationCompaction) CutPolicy() CutPolicy { return CutNotToolResult }

func (s SummarizationCompaction) ShouldCompact(est, window int) bool {
	f := s.ThresholdFraction
	if f <= 0 {
		f = 0.8
	}
	if window <= 0 {
		return false
	}
	return float64(est) > f*float64(window)
}

func (s SummarizationCompaction) CutIndex(msgs core.Messages, _ int) int {
	keep := s.KeepTokens
	if keep <= 0 {
		keep = 8000
	}
	return cutByTokens(msgs, keep, CutNotToolResult)
}

// cutByTokens walks BACKWARDS accumulating estimated tokens until keepTokens
// is reached, then SNAPS FORWARD to the first valid cut point.
func cutByTokens(msgs core.Messages, keepTokens int, policy CutPolicy) int {
	chars := 0
	i := len(msgs)
	for i > 0 {
		i--
		chars += estimateMessage(msgs[i])
		if chars/charsPerToken >= keepTokens {
			break
		}
	}
	return snapForward(msgs, i, policy)
}

// snapForward moves a candidate boundary FORWARD to the first valid cut point.
//
// Forward, never backward, and the direction is the requirement (REQ-GO-14).
// Snapping backward leaves a tool result in the kept tail whose originating
// tool_use has been summarized away — an orphan the provider rejects. Snapping
// forward puts the boundary result into the summarized portion, where it is
// summarized together with its call.
func snapForward(msgs core.Messages, from int, policy CutPolicy) int {
	if from <= 0 {
		return 0
	}
	for i := from; i < len(msgs); i++ {
		if validCut(msgs[i], policy) {
			return i
		}
	}
	// No valid cut point exists ahead; keep everything rather than produce an
	// invalid view.
	return 0
}

func validCut(m core.Message, policy CutPolicy) bool {
	switch policy {
	case CutUserOnly:
		_, ok := m.(core.UserMessage)
		return ok
	default:
		_, isResult := m.(core.ToolResultMessage)
		return !isResult
	}
}

// ---------------------------------------------------------------- summarizer

// Summarizer produces the summary text for a prefix.
type Summarizer func(ctx context.Context, prefix core.Messages, previous string) (string, error)

// ErrBadSummary is returned when a summarization response fails REQ-GO-16's
// taxonomy.
type ErrBadSummary struct{ Reason string }

func (e *ErrBadSummary) Error() string { return "agentkit: unusable summary: " + e.Reason }

// ModelSummarizer summarizes by calling a provider directly.
//
// It calls core.ProviderClient DIRECTLY and holds NO middleware chain. That is
// structural, not a rule anyone has to remember: REQ-GO-12.3 requires the
// summarization call to stay off the middleware path so it cannot re-enter
// BudgetMiddleware, the retry layers' turn accounting, or the dedup cache as
// though it were a conversational turn. A summarizer that went back through
// the loop would satisfy the requirement only by convention.
func ModelSummarizer(p core.ProviderClient, m *core.Model, maxTokens int) Summarizer {
	return func(ctx context.Context, prefix core.Messages, previous string) (string, error) {
		system := "Summarize the conversation so far. Preserve decisions, file paths, " +
			"identifiers, and anything the assistant committed to. Omit pleasantries."
		if previous != "" {
			system += "\n\nA previous summary covers the earliest part; extend it rather " +
				"than restating it:\n<previous-summary>\n" + previous + "\n</previous-summary>"
		}
		req := core.Request{
			System:   []core.ContentBlock{core.TextBlock{Text: system}},
			Messages: prefix,
			// An empty tool list plus an explicit "none" is what makes a
			// tool-free turn reliably forceable (REQ-TOOL-16).
			ToolChoice: core.ToolChoiceNone,
			MaxTokens:  &maxTokens,
		}
		msg := core.Complete(ctx, p, m, req, core.ProviderStreamOptions{
			// Caching is disabled for this request: it is not part of the
			// conversation prefix and would pollute it.
			CacheRetention: core.CacheRetentionNone,
		})
		return ValidateSummary(msg)
	}
}

// ValidateSummary is REQ-GO-16's failure taxonomy, as a pure function.
//
// A summary is a FAILURE when its stop reason is error or max_tokens, or when
// it contains any tool_use block — regardless of the text alongside it.
//
// max_tokens is a failure because the summary is truncated mid-thought, and
// compaction is PERMANENT: a truncated summary looks like a success and then
// poisons every subsequent turn for the life of the session.
//
// An ABORTED summarization is NOT a failure: the text produced so far is kept.
// That asymmetry is deliberate — an abort is the user's choice, and discarding
// work they interrupted helps nobody.
func ValidateSummary(msg *core.AssistantMessage) (string, error) {
	if msg == nil {
		return "", &ErrBadSummary{Reason: "provider returned no message"}
	}
	for _, b := range msg.Content {
		if _, ok := b.(core.ToolUseBlock); ok {
			// Checked on the RESPONSE, not prevented by tool_choice on the
			// request: a model can emit a tool call anyway, and a summary
			// containing one is not a summary.
			return "", &ErrBadSummary{Reason: "response contained a tool call"}
		}
	}
	switch msg.StopReason {
	case core.StopReasonError:
		return "", &ErrBadSummary{Reason: "provider error: " + msg.ErrorMessage}
	case core.StopReasonLength:
		return "", &ErrBadSummary{Reason: "summary was truncated at the output limit"}
	case core.StopReasonAborted:
		// Keep what we have.
	}
	text := strings.TrimSpace(msg.Content.Text())
	if text == "" {
		return "", &ErrBadSummary{Reason: "summary was empty"}
	}
	return text, nil
}

// ---------------------------------------------------------------- transform

// CompactionDeps are the inputs a compaction closure binds.
//
// REQ-GO-12's ContextTransform signature cannot return an error, cannot see
// the current model (which changes mid-session under REQ-SESS-03) and cannot
// reach the SessionStore to write the REQ-SESS-04 entry. Rather than widen the
// pinned signature, those inputs are supplied by BINDING a closure — the field
// holds a bound closure, not a free function (ruling P-40).
type CompactionDeps struct {
	Strategy   CompactionStrategy
	Summarizer Summarizer
	History    *core.ConversationHistory
	Model      *core.Model
	// OnCheckpoint persists the REQ-SESS-04 entry. Optional.
	OnCheckpoint func(core.CompactionCheckpoint) error
	// OnError surfaces a failed summarization. Compaction never aborts the
	// session (NFR-REL-05), so without this the failure is invisible.
	OnError func(error)
}

// NewContextTransform builds the bound closure installed as
// AgentConfig.TransformContext.
//
// The ORDER inside it is the whole design, and getting it wrong is the bug the
// PRD calls out by name:
//
//  1. Apply the existing checkpoint FIRST, unconditionally.
//  2. Estimate on the COMPACTED view.
//  3. Only then decide whether to EXTEND the checkpoint.
//
// Compaction is PERMANENT (REQ-GO-12.2). Once a summary checkpoint exists it
// is always re-applied; the threshold decides only whether to summarize a
// LONGER prefix. Re-evaluating from scratch each turn OSCILLATES: the
// compacted request reports small usage, the next check passes, full history
// returns, the check fails again — and every swing invalidates the provider's
// cache prefix and re-sends content already paid to summarize.
func NewContextTransform(d CompactionDeps) core.ContextTransform {
	return func(ctx context.Context, msgs core.Messages) core.Messages {
		if d.Strategy == nil {
			return msgs
		}

		// ---- 1. Apply the existing checkpoint, unconditionally.
		cp, hasCP := d.History.Checkpoint()
		view := msgs
		if hasCP && cp.Summary != "" {
			view = ApplyCheckpoint(msgs, cp)
		}

		// ---- 2. Estimate on the COMPACTED view, not on full history.
		window := 0
		if d.Model != nil {
			window = d.Model.ContextWindow
		}
		est := EstimateContextTokens(view, checkpointPtr(cp, hasCP))

		// ---- 3. Only now decide whether to extend.
		if !d.Strategy.ShouldCompact(est, window) {
			return view
		}

		cut := d.Strategy.CutIndex(msgs, window)
		if cut <= 0 || cut >= len(msgs) {
			return view
		}

		if d.Summarizer == nil {
			// A window strategy with no summarizer simply drops the prefix.
			// The cut policy guarantees the tail starts on a user message.
			return msgs[cut:]
		}

		summary, err := d.Summarizer(ctx, msgs[:cut], cp.Summary)
		if err != nil {
			// REQ-GO-16 governs the CHECKPOINT: never persist a bad summary.
			// NFR-REL-05 governs the VIEW: never abort the session. Both hold
			// — we keep the current view and checkpoint nothing (ruling P-39).
			if d.OnError != nil {
				d.OnError(fmt.Errorf("compaction: %w", err))
			}
			return view
		}

		next := core.CompactionCheckpoint{
			PrefixLen:    cut,
			Summary:      summary,
			CreatedAtLen: len(msgs),
		}
		d.History.SetCheckpoint(next)
		if d.OnCheckpoint != nil {
			if err := d.OnCheckpoint(next); err != nil && d.OnError != nil {
				d.OnError(fmt.Errorf("compaction: persisting checkpoint: %w", err))
			}
		}
		return ApplyCheckpoint(msgs, next)
	}
}

func checkpointPtr(cp core.CompactionCheckpoint, ok bool) *core.CompactionCheckpoint {
	if !ok {
		return nil
	}
	return &cp
}

// ApplyCheckpoint renders the compacted view: the summary as a user message,
// followed by the kept tail.
//
// It never mutates msgs. The append-only transcript stays complete, so the UI
// can scroll back, the session log is lossless, and a later run against a
// larger context window can be given the full history (REQ-GO-12.1).
func ApplyCheckpoint(msgs core.Messages, cp core.CompactionCheckpoint) core.Messages {
	if cp.Summary == "" || cp.PrefixLen <= 0 || cp.PrefixLen >= len(msgs) {
		return msgs
	}
	out := make(core.Messages, 0, len(msgs)-cp.PrefixLen+1)
	out = append(out, core.UserMessage{
		Content: core.Content{core.TextBlock{Text: CompactionSummaryPrefix + cp.Summary}},
	})
	out = append(out, msgs[cp.PrefixLen:]...)
	return out
}
