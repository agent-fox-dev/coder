// Package compaction is REQ-GO-12 through REQ-GO-16: the context transform the
// loop applies before every model call, the four strategies that decide
// whether and where to cut, the summarizers that produce a checkpoint, and the
// anchored token estimate (REQ-GO-15) that drives both the trigger and the
// REQ-CAT-04 clamp.
//
// Compaction is a context transform applied inside the loop, never a
// middleware: a compaction middleware's own summarization call would re-enter
// the chain, be fingerprinted by the dedup cache and be charged against the
// budget gate as though it were a conversational turn. NewContextTransform
// builds the closure installed as core.AgentConfig.TransformContext.
package compaction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/agentfox/agentkit-go/core"
)

// SummaryPrefix wraps a summary when it is rendered into the model
// context. It is MODEL-VISIBLE format contract and is pinned by a golden test:
// changing it changes what every compacted session says to the model.
const SummaryPrefix = "[Earlier conversation, summarized]\n\n"

// SplitSeparator joins the two halves of a SPLIT summary
// (REQ-GO-14): the summary of the completed turns, then the summary of the
// turn the cut landed inside. Model-visible format contract, pinned by the
// same golden as SummaryPrefix.
const SplitSeparator = "\n\n[The turn in progress at the cut, summarized separately]\n\n"

// Strategy decides whether and where to compact.
type Strategy interface {
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
	// permits a cut landing on an ASSISTANT message. Summarization
	// is fine there because it prepends a summary user message, but
	// TurnWindow and TokenWindow have nothing to prepend, so they would
	// produce a view starting on an assistant turn — which Anthropic rejects
	// and which REQ-PROV-11 does not repair, since rule 6 covers only
	// orphaned tool_use blocks.
	CutUserOnly
)

// ---------------------------------------------------------------- strategies

// None never compacts.
type None struct{}

func (None) ShouldCompact(int, int) bool     { return false }
func (None) CutIndex(core.Messages, int) int { return 0 }
func (None) CutPolicy() CutPolicy            { return CutNotToolResult }

// TurnWindow keeps the most recent MaxTurns user turns.
type TurnWindow struct{ MaxTurns int }

func (t TurnWindow) CutPolicy() CutPolicy { return CutUserOnly }

func (t TurnWindow) ShouldCompact(est, window int) bool { return true }

func (t TurnWindow) CutIndex(msgs core.Messages, _ int) int {
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

// TokenWindow keeps the most recent KeepTokens' worth of messages.
type TokenWindow struct{ KeepTokens int }

func (t TokenWindow) CutPolicy() CutPolicy { return CutUserOnly }

func (t TokenWindow) ShouldCompact(est, window int) bool {
	return est > t.KeepTokens
}

func (t TokenWindow) CutIndex(msgs core.Messages, _ int) int {
	return cutByTokens(msgs, t.KeepTokens, CutUserOnly)
}

// Summarization replaces the summarized prefix with a model-written
// summary.
type Summarization struct {
	// ThresholdFraction of the context window at which compaction fires.
	ThresholdFraction float64
	// KeepTokens is the size of the tail kept verbatim.
	KeepTokens int
}

func (s Summarization) CutPolicy() CutPolicy { return CutNotToolResult }

func (s Summarization) ShouldCompact(est, window int) bool {
	f := s.ThresholdFraction
	if f <= 0 {
		f = 0.8
	}
	if window <= 0 {
		return false
	}
	return float64(est) > f*float64(window)
}

func (s Summarization) CutIndex(msgs core.Messages, _ int) int {
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
//
// reserveTokens is REQ-GO-12.3's reserve: the summary's max_tokens is
// min(0.8 × reserve, model.MaxTokens). A caller passing the whole reserve
// gets a summary that cannot consume it entirely, and one passing more than
// the model can emit gets the model's ceiling rather than a 400. Zero means
// "the model's own ceiling".
//
// The request carries its OWN session id (a fresh one per summarizer, never
// the conversation's): the summary is not part of the conversation prefix,
// and routing it on the conversation's key would land it on the node whose
// KV cache is warm for a prefix it does not share.
func ModelSummarizer(p core.ProviderClient, m *core.Model, reserveTokens int) Summarizer {
	return modelSummarizer(p, m, reserveTokens, false)
}

// ModelTurnSummarizer is the summarizer for the SPLIT half of a cut
// (REQ-GO-14): the prefix of the turn the boundary landed inside, summarized
// under a distinct prompt at half the token budget. Install it as
// Deps.TurnSummarizer; when absent, the main summarizer is used for
// both halves and the distinct prompt and the half budget are lost.
func ModelTurnSummarizer(p core.ProviderClient, m *core.Model, reserveTokens int) Summarizer {
	return modelSummarizer(p, m, reserveTokens/2, true)
}

func modelSummarizer(p core.ProviderClient, m *core.Model, reserveTokens int, turnOnly bool) Summarizer {
	sessionID := randomID("summary")
	return func(ctx context.Context, prefix core.Messages, previous string) (string, error) {
		system := "Summarize the conversation so far. Preserve decisions, file paths, " +
			"identifiers, and anything the assistant committed to. Omit pleasantries."
		if turnOnly {
			system = "Summarize the PARTIAL turn below: what was asked, what the assistant " +
				"had done so far, and any tool results it had received. It was interrupted " +
				"at the end; do not invent a conclusion. Preserve file paths and identifiers."
		}
		if previous != "" {
			system += "\n\nA previous summary covers the earliest part; extend it rather " +
				"than restating it:\n<previous-summary>\n" + previous + "\n</previous-summary>"
		}
		maxTokens := summaryMaxTokens(m, reserveTokens)
		// An extension's delta can begin on an assistant message (the previous
		// checkpoint's cut was CutNotToolResult, not CutUserOnly), and every
		// wire requires the first message to be the user's. The summary the
		// delta continues from is already in the system prompt, so a one-line
		// user turn pointing at it is enough to make the request valid.
		msgs := prefix
		if len(msgs) > 0 && msgs[0].Role() != core.RoleUser {
			msgs = append(core.Messages{core.UserMessage{Content: core.Content{
				core.TextBlock{Text: summaryContinuationNote}}}}, msgs...)
		}
		req := core.Request{
			System:   []core.ContentBlock{core.TextBlock{Text: system}},
			Messages: msgs,
			// An empty tool list plus an explicit "none" is what makes a
			// tool-free turn reliably forceable (REQ-TOOL-16).
			ToolChoice: core.ToolChoiceNone,
			MaxTokens:  &maxTokens,
			Options:    core.RequestOptions{SessionID: sessionID},
		}
		msg := core.Complete(ctx, p, m, req, core.ProviderStreamOptions{
			// Caching is disabled for this request: it is not part of the
			// conversation prefix and would pollute it.
			CacheRetention: core.CacheRetentionNone,
		})
		return ValidateSummary(msg)
	}
}

// summaryContinuationNote is the user turn prepended to an extension delta
// that begins mid-turn (see modelSummarizer). Model-visible, fixed.
const summaryContinuationNote = "[The earlier conversation is summarized in the system prompt; the messages that follow continue it.]"

// summaryMaxTokens is REQ-GO-12.3's clamp: min(0.8 × reserve, model.MaxTokens),
// with each unknown side deferring to the other and a floor of 1. With no
// reserve stated the model's ceiling is further bounded by core.DefaultMaxTokens: a
// summary does not need a 128K output budget, and providers size rate-limit
// reservations from max_tokens.
func summaryMaxTokens(m *core.Model, reserve int) int {
	n := 0
	if reserve > 0 {
		n = reserve * 8 / 10
	}
	if m != nil && m.MaxTokens > 0 && (n <= 0 || n > m.MaxTokens) {
		n = m.MaxTokens
	}
	if reserve <= 0 && n > core.DefaultMaxTokens {
		n = core.DefaultMaxTokens
	}
	if n <= 0 {
		n = 1
	}
	return n
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

// Deps are the inputs a compaction closure binds.
//
// REQ-GO-12's ContextTransform signature cannot return an error, cannot see
// the current model (which changes mid-session under REQ-SESS-03) and cannot
// reach the SessionStore to write the REQ-SESS-04 entry. Rather than widen the
// pinned signature, those inputs are supplied by BINDING a closure — the field
// holds a bound closure, not a free function (ruling P-40).
type Deps struct {
	Strategy   Strategy
	Summarizer Summarizer
	// TurnSummarizer summarizes the prefix of a SPLIT turn (REQ-GO-14) under
	// a distinct prompt at half the budget. Nil disables the split and the
	// whole prefix is summarized as one block; ModelTurnSummarizer is the
	// shipped implementation.
	TurnSummarizer Summarizer
	History        *core.ConversationHistory
	Model          *core.Model
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
func NewContextTransform(d Deps) core.ContextTransform {
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

		// Extending means summarizing what the previous checkpoint did NOT
		// cover — msgs[cp.PrefixLen:cut] — with the previous summary handed
		// to the summarizer to build on. Re-summarizing from index 0 re-sent
		// the whole original prefix on every extension: O(total history)
		// tokens each time, and once the history outgrew the context window
		// the summarization request itself failed, every turn, while the
		// view stayed over the threshold.
		//
		// The cut is chosen by a character heuristic and the threshold by a
		// provider-reported anchor, so the two can disagree; a cut that lands
		// at or before the existing checkpoint has nothing to extend and must
		// not shrink the checkpoint (compaction is permanent, REQ-GO-12.2).
		from := 0
		if hasCP && cp.Summary != "" {
			from = cp.PrefixLen
		}
		if cut <= from {
			return view
		}

		summary, err := summarizeWithSplit(ctx, d, msgs, from, cut, cp.Summary)
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

// summarizeWithSplit is REQ-GO-14's turn split.
//
// CutNotToolResult permits a boundary on an ASSISTANT message, which means
// the cut lands INSIDE a turn: the user's question is in the summarized
// prefix and the assistant's reply — possibly tool calls and results — is in
// the kept tail. Summarizing the whole prefix as one block folds the head of
// that turn into "earlier conversation" and the model resumes a reply to a
// question it can no longer see verbatim. So the prefix of the split turn,
// from the nearest preceding user message, is summarized SEPARATELY under a
// distinct prompt at half the budget, and the two summaries are joined with a
// fixed separator. A cut that lands on a user message is not split.
//
// The split needs a summarizer with the DISTINCT prompt the requirement
// names; with no TurnSummarizer configured there is none, and splitting would
// only double the calls under a prompt that tells the model to "extend the
// previous summary" over half a turn. So the split applies when a
// TurnSummarizer is installed (ModelTurnSummarizer is the shipped one), and
// a caller who wires only the main summarizer gets the single-block form.
//
// from is the first message NOT covered by the previous checkpoint (0 when
// there is none): only msgs[from:cut] is summarized, and the previous summary
// stands in for everything before it.
func summarizeWithSplit(ctx context.Context, d Deps, msgs core.Messages, from, cut int, previous string) (string, error) {
	if _, onUser := msgs[cut].(core.UserMessage); onUser || d.TurnSummarizer == nil {
		return d.Summarizer(ctx, msgs[from:cut], previous)
	}
	turnStart := cut - 1
	for turnStart > from {
		if _, ok := msgs[turnStart].(core.UserMessage); ok {
			break
		}
		turnStart--
	}
	if turnStart <= from {
		// The whole delta is one turn; there is nothing to split it from.
		return d.Summarizer(ctx, msgs[from:cut], previous)
	}
	head, err := d.Summarizer(ctx, msgs[from:turnStart], previous)
	if err != nil {
		return "", err
	}
	tail, err := d.TurnSummarizer(ctx, msgs[turnStart:cut], "")
	if err != nil {
		return "", err
	}
	return head + SplitSeparator + tail, nil
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
		Content: core.Content{core.TextBlock{Text: SummaryPrefix + cp.Summary}},
	})
	out = append(out, msgs[cp.PrefixLen:]...)
	return out
}

// randomID is the summarizer's per-instance session id: eight random bytes
// under a prefix. rand.Read from crypto/rand cannot fail on any supported
// platform; since Go 1.24 it panics rather than returning an error.
func randomID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
