# Erratum: extending a compaction checkpoint summarizes the delta

**Relates to:** REQ-GO-12 rule 2, REQ-GO-14, REQ-GO-15.
**Status:** implemented 2026-09-09.

## What the PRD says

REQ-GO-12.2: a checkpoint is permanent; "the threshold check decides only
whether to *extend* the checkpoint by summarizing a longer prefix; when
extending, the previous summary is re-sent to the summarizer inside
`<previous-summary>` tags under a distinct prompt."

## What the code did

`NewContextTransform` handed the summarizer `msgs[:cut]` — the whole original
prefix from message 0 — on every extension, with the previous summary in the
system prompt beside it. That is O(total history) tokens per extension, and
it fails structurally: once the original history has outgrown the context
window (which is exactly when a second extension is due), the summarization
request itself exceeds the window, the provider rejects it, compaction
returns the unchanged view, and the next turn fails or re-attempts the same
oversized summarization. The "extend it rather than restating it" prompt was
asking the model to do what the request shape made impossible to do cheaply.

## What changed

- The summarizer receives `msgs[cp.PrefixLen:cut]` — only the messages the
  previous checkpoint did not cover — with `previous` set. "Summarizing a
  longer prefix" is read as the checkpoint covering a longer prefix, which is
  what the resulting `{prefix_len, summary}` records.
- The turn split of REQ-GO-14 is applied inside the delta.
- A cut at or before the existing checkpoint extends nothing and is refused,
  so a heuristic cut can never move a checkpoint backwards.
- A delta that begins on an assistant message (a `CutNotToolResult` boundary)
  is opened with a fixed one-line user turn, because every wire requires the
  first message to be the user's. The note is model-visible and pinned by
  `TestADeltaStartingOnAnAssistantGetsAUserTurnFirst`.
- With no reserve stated, the summary's `max_tokens` is bounded by
  `agentkit.DefaultMaxTokens` rather than the model's output cap.

Pinned by `TestExtendingSummarizesOnlyTheDelta` and
`TestACutBelowTheCheckpointDoesNotShrinkIt`.
