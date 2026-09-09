# Provider surface ledger

NFR-COMPAT-07. One row per moving external surface AgentKit tracks: the pinned
API version or dated beta header, the reference the NFR-TEST-06 goldens were
produced against, and the date of that capture.

The ledger exists because a provider API is a moving target and a library that
tracks five of them plus a protocol spec will otherwise discover a breaking
change from a user's bug report. Reviewing it is part of every pin bump.

**Reviewed is not implemented.** A row says what this build *targets*, not that
someone has diffed it against the vendor's current documentation. `Reviewed`
is the last date a human compared the implementation against the upstream
surface; `Implemented` is what the code does today. When they diverge, the
implementation is what ships and the gap is the work.

## Wire APIs

`Reviewed` is the last date a human compared the implementation against the
upstream surface. `Implemented` is what the code does today against that
surface, as a delta: `full` means every reviewed change landed; anything else
names what is reviewed-not-implemented, and the next drift computation is the
version delta **plus** every such row (NFR-COMPAT-07.2).

| Surface | Pinned version | Endpoint | Goldens captured against | Capture date | Reviewed | Implemented |
|---|---|---|---|---|---|---|
| `anthropic-messages` | `anthropic-version: 2023-06-01` | `POST https://api.anthropic.com/v1/messages` | none — hand-authored fixtures | — | 2026-09-09 | full. Thinking is sent in the generation the catalog row speaks: an effort token becomes `thinking: {type: adaptive}` + `output_config.effort`, an integer becomes `budget_tokens` (never below 1024, never at or above `max_tokens`); `off` sends `disabled` only where the row maps it. Rows with `compat.supports_sampling: false` drop `temperature`/`top_p`. Server-side compaction (REQ-PROV-07) is opt-in per OQ-4 and sends both the beta header and `context_management`. Not implemented: the tool-search server tool (`defer_loading` is therefore never sent — ruling L-10), `top_k`, forced `tool_choice` |
| `openai-completions` | unversioned (`/v1`) | `POST https://api.openai.com/v1/chat/completions` | none — hand-authored fixtures | — | 2026-09-06 | full. Every REQ-PROV-12 flag is inferred from the host and consumed: `cache_control` + 1h TTL (OpenRouter `anthropic/*`), `reasoning_content` replay gated on `ThinkingFormat == deepseek`, the reasoning budget under `ThinkingTokenBudgetField` (vLLM/SGLang/llama.cpp/Qwen), and a synthetic assistant turn where `AllowsUserAfterToolResult` is false (OpenRouter `anthropic/*`, Mistral) |
| `openai-responses` | unversioned (`/v1`) | `POST https://api.openai.com/v1/responses` | none — hand-authored fixtures | — | 2026-09-06 | full, `additional_tools` for deferred tool loading included (REQ-CACHE-10). The field is implemented as the PRD states and has not been checked against live vendor docs |
| `google-generative-ai` | `v1beta` (AI Studio) / `v1` (Vertex) | `POST https://generativelanguage.googleapis.com/v1beta/models/{id}:streamGenerateContent?alt=sse` — on Vertex, `POST https://{location-}aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/publishers/google/models/{id}:streamGenerateContent?alt=sse` | none — hand-authored fixtures | — | 2026-09-06 | full. Both deployments ship and switching is a config change (NFR-COMPAT-05); `CachedContent` ships opt-in, created off the request path (§6.2a, NFR-PERF-08) |
| `ollama-chat` | unversioned | `POST http://localhost:11434/api/chat` | none — hand-authored fixtures | — | 2026-09-06 | full. `is_error` has nowhere to go on this wire and is rendered as an `Error: ` text prefix — model-visible and unstructured, a wire limit rather than a gap |

## Attribution headers (REQ-SEC-13)

Every header AgentKit sends that identifies AgentKit, the consuming
application, or the session is attribution and is listed here; changing this
set is a documented, released change even when no other code changes
(REQ-SEC-13.1).

| Header | Value | Sent to |
|---|---|---|
| `x-agentkit-version` | the SDK version constant | every wire API |
| `user-agent` | `agentkit-go/<version>` | every wire API |

Neither carries a session identifier, workspace path, user identity, prompt
text, or any other request content (REQ-SEC-13.3). The single kill switch is
`AgentConfig.Attribution = false` or the environment variable
`AGENTKIT_TELEMETRY=0`; either disables both. Precedence, lowest to highest:
attribution defaults < provider/auth headers < `model.headers` < caller
`RequestOptions.Headers`, and a caller may suppress any default with the
REQ-AUTH-02 deletion marker (a present-nil value).

## Rulings

Decisions about a surface are recorded here once and are not re-litigated
(NFR-COMPAT-07.3). A ruling states the decision and the reason; the narrative
that produced it belongs in the commit that made it.

| # | Surface | Ruling |
|---|---|---|
| L-1 | MCP | **Modern-only.** AgentKit speaks `2026-07-28` alone — no handshake era, no sessions, no `ping`, no GET stream. Cost: a server that has not migrated is unreachable, and at the time of the ruling most have not. Reversal means implementing the legacy era, not a flag. (PRD 0.4.0.) |
| L-2 | MCP | **Standard library, not `mcp-go`.** A third-party JSON-RPC library owns the wire on the three surfaces REQ-SEC-11 names and none rejects duplicate keys; the two requirements cannot both hold with the dependency. (PRD 0.3.5.) |
| L-3 | all wire APIs | **Goldens are regression pins, not truth.** `testdata/golden/request_*.json` were produced by AgentKit; only a vendor capture or the `difftest` harness pins the wire against the vendor, and until one exists the ledger's capture column stays empty rather than pretending. |
| L-4 | `anthropic-messages` | **Server-side compaction is opt-in.** The `compact-2026-01-12` beta is sent only when named in `Options.Betas`; in-process summarization is the uniform default for every provider (OQ-4). |
| L-5 | `openai-completions` / `openai-responses` | **Two implementations, no flag.** They differ in the message model, tool-call identity, reasoning replay, caching parameters and billing; a flag would branch on itself in every one of those places (REQ-PROV-02). |
| L-6 | `google-generative-ai` | **`v1beta` until the streaming tool-call shape is on `v1`.** The path is a constant in the provider package and this row is its copy. Vertex is `v1` because that is the version Vertex publishes; the two deployments do not share a version string. |
| L-7 | `google-generative-ai` | **Ambient GCP environment does not select Vertex.** `GOOGLE_CLOUD_PROJECT` and friends are set on every GCE and Cloud Run box and are what REQ-AUTH-04's ambient credential state reads; treating them as the deployment switch would silently move every API-key deployment that happens to run on GCP. The switch is an explicit project option or a Vertex base URL. |
| L-8 | `openai-completions` | **A deferred tool is declared in prose on this wire.** It supports neither `defer_loading` nor `additional_tools`, so REQ-CACHE-10's third arm re-declares withheld tools in a system message after the tool-result run. Prose is weaker than a field: the model may call a tool absent from `tools`, and some servers reject that. It is what the requirement specifies. |
| L-10 | `anthropic-messages` | **A deferred tool is positioned, not hidden.** REQ-CACHE-10's Anthropic arm is order alone: a late-added tool is appended after the established ones with no breakpoint, so the cached prefix stays byte-identical. It is NOT marked `defer_loading: true` — on this wire that flag hides the tool from the model until a `tool_search_tool_regex_20251119` server tool finds it, and the adapter declares no such tool. Sending the flag without the search tool made every late-added tool invisible. Revisiting means declaring the search tool first (an `Options` flag) and only then deferring. |
| L-11 | `anthropic-messages` | **Effort goes on the wire as the catalog row speaks it.** A row whose ladder values are effort tokens (`low` … `max`) gets `thinking: {type: adaptive}` + `output_config: {effort}`; a row whose values are integers gets `budget_tokens`. The adapter never translates one into the other: `budget_tokens` is a 400 on every model since Opus 4.7, and an effort is an error on Sonnet 4.5 / Haiku 4.5, so a translation table is a 400 in one direction or the other. `off` consults the row too — `disabled` where it maps to a value, omitted where it is present-and-null (Fable cannot stop thinking) or absent. Ruling P-27 stands: a request for some thinking is never clamped to none. |
| L-9 | `google-generative-ai` | **A deferred tool is not callable on this wire.** Gemini gates calling on `functionDeclarations`, so a withheld tool cannot be invoked at all, and `SplitDeferredTools` never un-defers on later use. Anthropic and Responses do not have this because their arms still declare the tool. Revisiting means graduating deferred tools once the prefix rotates, in `provider/toolcache.go`. |

### Dated beta headers

| Header | Value | Requirement | Sent when |
|---|---|---|---|
| `anthropic-beta` | `compact-2026-01-12` | REQ-PROV-07 | only when `Options.Betas` names it; the body then also carries `context_management: {edits: [{type: compact_20260112}]}`, without which the header is a no-op |

A dated beta is the most perishable thing in this file: it is a header the
vendor retires on a schedule, and the failure when it does is a 400 naming the
header rather than the feature. It is opt-in for that reason.

## Protocol specs

| Surface | Pinned version | Also accepted | Notes |
|---|---|---|---|
| Model Context Protocol | `2026-07-28` | none | REQ-MCP-SERVER-06, amended in PRD 0.4.0. **Modern-only**: no handshake, no sessions, no `ping`. Every request carries its version in `_meta`; a version we do not speak is rejected with `UnsupportedProtocolVersion` (`-32022`) listing what we do. |
| MCP transports | Streamable HTTP (`2026-07-28`), stdio | — | REQ-MCP-CLIENT-02, amended in 0.4.0. The GET stream, `Mcp-Session-Id` and `Last-Event-ID` resumability are gone; HTTP+SSE (2024-11-05) is Deprecated upstream and removed here. |

### The cost of modern-only

The spec defines a *dual-era* mode for implementations that support both the
handshake and the stateless core. AgentKit does not: it speaks `2026-07-28`
alone, by the product decision recorded in PRD 0.4.0.

That has a price worth stating plainly, because it is not visible from the
code: **an AgentKit client cannot talk to a server that has not migrated**, and
at the time of writing most deployed servers have not. The failure is clean
rather than silent — a modern request against a legacy server gets an
implementation-defined error, and our server answers a legacy `initialize` with
a message naming the versions it speaks — but it is still a failure. Revisiting
this means implementing the legacy era, not flipping a flag.

## Goldens: what they are and are not

The **Capture date** column is empty on every row, and that is the honest
state rather than an oversight.

- **`testdata/golden/request_*.json` pin the request body** this build
  produces. They are regression goldens: they catch AgentKit changing what it
  sends. They say nothing about whether what it sends is what the vendor
  currently accepts, because they were produced by AgentKit, not captured from
  a vendor.
- **NFR-TEST-06's differential harness** (`difftest/`) is what would make the
  stronger claim, and it reports **DARK**. NFR-TEST-06.3 forbids hand-authoring
  a reference — a hand-authored expectation encodes the same mental model as
  the code under test — and producing a real one needs a vendor SDK or a live
  key. `known-divergences.json` is empty because there is nothing yet to
  diverge from.

So: the request goldens pin the wire format against *regression*; only a
capture pins it against *truth*. The weaker claim is the one this file makes
until a capture exists.

## Regeneration checklist

On any pin bump — a new `anthropic-version`, a Google path moving off
`v1beta`, an MCP revision, a retired beta header:

1. Update the constant in the provider package. The version strings above are
   copies of constants, and the constant is authoritative.
2. Update this table's row, including **Reviewed**.
3. `go test -run TestGolden -update ./...` and **read the diff**. A golden
   regenerated without reading it is circular — that is the whole failure mode
   NFR-TEST-08 exists to prevent.
4. Re-check REQ-CAT-06's two request-body-affecting catalog fields,
   `ThinkingLevelMap` and `Cost` including the provider field on fallback
   entries. Dropping the latter silently bills fallback-served responses at the
   wrong rate.
5. Review `difftest/known-divergences.json`. An entry buys time; it does not
   close the defect.
