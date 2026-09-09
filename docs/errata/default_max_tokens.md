# Erratum: an unstated `max_tokens` is an SDK default, not the model's cap

**Relates to:** REQ-CAT-04, REQ-PROV-16.
**Status:** implemented 2026-09-09.

## What the code did

`AgentConfig.MaxTokens` is a `*int`; nil meant "no bound", and
`catalog.ClampMaxTokens` resolved that to the model's own output cap. On every
current Anthropic row that is 128,000, sent on every request including a
one-line tool call.

## Why that is wrong

Providers size output-rate-limit reservations from `max_tokens` at request
start, so each request reserved 128K of output-per-minute budget. On most
tiers that is a 429 on the first or second concurrent request — which is
what parallel delegation (`SubagentTool`, `RunParallel`) produces by design.
It also removed the only bound on a runaway turn.

## What changed

- `agentkit.DefaultMaxTokens` (32,768) is sent when the config states no
  bound. It is still clamped by REQ-CAT-04 against the model's cap and the
  remaining window; a caller who wants the cap says so.
- The compaction summarizer's budget is bounded the same way when no reserve
  is stated (REQ-GO-12.3's `min(0.8 × reserve, model.max_tokens)` still holds
  when a reserve is given).
