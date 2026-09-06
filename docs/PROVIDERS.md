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

| Surface | Pinned version | Endpoint | Goldens captured against | Capture date | Reviewed |
|---|---|---|---|---|---|
| `anthropic-messages` | `anthropic-version: 2023-06-01` | `POST https://api.anthropic.com/v1/messages` | none — hand-authored fixtures | — | 2026-09-06 |
| `openai-completions` | unversioned (`/v1`) | `POST https://api.openai.com/v1/chat/completions` | none — hand-authored fixtures | — | 2026-09-06 |
| `openai-responses` | unversioned (`/v1`) | `POST https://api.openai.com/v1/responses` | none — hand-authored fixtures | — | 2026-09-06 |
| `google-generative-ai` | `v1beta` | `POST https://generativelanguage.googleapis.com/v1beta/models/{id}:streamGenerateContent?alt=sse` | none — hand-authored fixtures | — | 2026-09-06 |
| `ollama-chat` | unversioned | `POST http://localhost:11434/api/chat` | none — hand-authored fixtures | — | 2026-09-06 |

### Dated beta headers

| Header | Value | Requirement | Sent when |
|---|---|---|---|
| `anthropic-beta` | `compact-2026-01-12` | REQ-PROV-07 | only when `Options.Betas` names it |

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
