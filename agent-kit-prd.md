# AgentKit: A Dependency-Free Agent SDK for Go

**Author:** [Platform Engineering]
**Date:** 2026-09-05
**Status:** Draft
**Version:** 0.3.0

> **Revision note (0.3.0).** This revision incorporates a review of a shipped, zero-dependency
> Go agent SDK and coding agent of comparable scope and identical constraints
> ([`sky-valley/pi`](https://github.com/sky-valley/pi), a pure-Go port of Mario Zechner's `pi`
> — ~105k lines of Go, five-provider surface, two third-party modules). It is the closest
> real-world artifact to what this document specifies, and several of its shipped designs
> **contradict** rather than extend the 0.2.0 draft. Those corrections are marked in place;
> see [Appendix A](#appendix-a-prior-art-review) for the provenance and the full list.

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Problem Statement](#2-problem-statement)
3. [Goals and Non-Goals](#3-goals-and-non-goals)
4. [User Personas](#4-user-personas)
5. [Core Concepts and Architecture](#5-core-concepts-and-architecture)
6. [Feature Requirements](#6-feature-requirements)
7. [API Design Sketches](#7-api-design-sketches)
8. [Non-Functional Requirements](#8-non-functional-requirements)
9. [Open Questions](#9-open-questions)
- [Appendix A: Prior-Art Review](#appendix-a-prior-art-review)

---

## 1. Executive Summary

### Overview

AgentKit is a lightweight, dependency-free Go library that gives developers direct, transparent control over the full agentic stack. It provides a production-grade agent loop, a unified tool system, a skills packaging format, a plugin extension model, and bidirectional MCP (Model Context Protocol) support — all without wrapping external frameworks such as LangChain, Google ADK, or the Claude Code CLI.

AgentKit is a library, not a product. It has no binary, no daemon, no CLI of its own. It is `import`ed by tools that need agentic capabilities — tools like nightshift (an issue-triage CLI) or hub (an API gateway) — and those tools own the user-facing interface. Every meaningful behavior in AgentKit is expressed in ordinary Go code that a developer can read, step through with a debugger, and override.

### Positioning

AgentKit sits **below** end-user tools in the stack, not beside them:

```
Claude Code / nightshift / hub / your-tool
         ↓  (imports)
       AgentKit
         ↓  (calls)
  Anthropic / OpenAI / Gemini / OpenRouter / Ollama
```

AgentKit does not compete with Claude Code, the Anthropic CLI, or any end-user agent product. It is the foundation those products could be built on — or that teams embed when building their own. A nightshift binary that uses AgentKit is still nightshift; AgentKit is invisible to the end user.

### Motivation

Existing agent frameworks each impose non-trivial abstraction taxes. The Claude Agent SDK shells out to a closed-source Bun-compiled binary rather than calling the Anthropic Messages API directly. Google ADK is a large, opinionated framework with deep coupling to Google Cloud services and no Go implementation. LangChain/LangGraph carries a layered package ecosystem and a state-machine DSL that must be learned before simple tool-calling works.

AgentKit eliminates that overhead by calling model provider APIs directly, owning the loop explicitly, and exposing every extension point through small, composable interfaces. The tool system is a struct with a handler function. The extension system is three orthogonal axes: middleware, turn hooks, and tool interceptors. Nothing is hidden inside a subprocess or a graph engine.

The core insight behind AgentKit is that the agentic loop is **readable**, not that it is trivial. Earlier drafts of this document claimed the loop was "six lines of logic" and "a while loop with a switch statement"; that framing is retired, because it is the reason the 0.2.0 loop specification was wrong in three independent places — it gated iteration on `stop_reason` (REQ-LOOP-01), it hardcoded one provider's tool-result packing as a universal invariant (REQ-LOOP-02), and it checked its limits at a point that leaves an unresumable transcript (REQ-LOOP-04a). The loop is perhaps sixty lines, and every additional line is a provider disagreement, a cancellation boundary, or a repair that some real API demanded. Value comes from those lines being *ordinary Go a developer can read and step through* — not from there being few of them.

What the existing frameworks actually provide is tooling around that loop: retries, compaction, streaming, observability, multi-agent coordination, and secure tool execution. AgentKit provides all of that tooling as transparent, composable components rather than as opaque framework internals.

---

## 2. Problem Statement

### Why Not the Claude Agent SDK?

The `claude-agent-sdk` Python package is a thin subprocess wrapper around the Claude Code CLI binary. It does not call the Anthropic Messages API directly. It resolves the `claude` binary using `shutil.which()` or a bundled copy at `package_root/_bundled/claude` (version 2.1.44 as of the current release), then spawns it using `anyio.open_process()`. The binary is a Mach-O arm64 executable compiled with Bun (Oven's TypeScript bundler), embedding a complete Node.js runtime.

The agent loop runs entirely inside that closed binary. Every turn is mediated through a bidirectional NDJSON control protocol over stdin/stdout: the SDK sends `control_request` messages (initialize, interrupt, set_model, set_permission_mode), the CLI sends `control_response` plus `can_use_tool` permission gates and `hook_callback` events. Extending the loop — custom retry logic, custom compaction, cost tracking at turn granularity — requires intercepting this control protocol rather than composing ordinary code.

For production systems that need auditability, multi-provider routing, a Go implementation, or the ability to step through the agent loop in a debugger, the Claude Agent SDK is not viable.

### Why Not Google ADK?

Google ADK is open-source (Apache 2.0) and genuinely model-agnostic, but it is a large, opinionated framework. ADK 2.0 (GA May 2026) introduced a graph-based execution engine where agents and tool nodes are graph nodes, edges are typed transitions, and the runner is a stateless graph traversal orchestrator. Understanding an ADK agent requires understanding `LlmAgent`, `Runner`, `Session`, `Event`, `NodeInfo`, `isolationScope`, `RetryConfig`, and the `NodeInterruptedError` control-flow exception used internally for human-in-the-loop pauses (catching `BaseException` in user code will silently break HITL pause/resume).

Non-Gemini providers are supported only via LiteLLM, adding another dependency layer. The Google Cloud toolsets (VertexAI, BigQuery, AlloyDB) are first-class citizens; Anthropic models are second-class. There is no production-ready Go implementation for ADK despite the `adk-go` repository existing. For teams building on Anthropic models with Go services, ADK is not suitable.

### Why Not LangChain / LangGraph?

LangChain is a mature ecosystem but carries significant complexity across its layered package tree: `langchain-core`, `langchain`, `langchain-community` (now officially sunset), and a growing set of provider packages. The LCEL pipe-operator abstraction (`chain = prompt | llm | parser`) is elegant for linear chains but becomes opaque when composed with `RunnableParallel`, `RunnableWithMessageHistory`, and `RunnableLambda`. The `AgentExecutor` is deprecated; the replacement is LangGraph's `StateGraph`, which requires learning a typed state-machine DSL.

Tool registration alone has multiple partially-overlapping patterns: the `@tool` decorator, `StructuredTool.from_function`, `BaseTool` subclass, `bind_tools()` on the model, and `ToolNode` in LangGraph. MCP is supported via a separate `langchain-mcp-adapters` package maintained by the LangChain team. There is no Go implementation. For teams that want a minimal, auditable agent loop without framework lock-in, LangChain is over-specified.

### The Core Gap

| Requirement | Claude Agent SDK | Google ADK | LangChain / LangGraph | AgentKit |
|---|---|---|---|---|
| Direct provider API calls, no subprocess | No (shells to CLI) | Yes | Yes | Yes |
| Explicit, readable agent loop | No (inside binary) | Partial (graph engine) | Partial (LangGraph) | Yes |
| Go first-class | No | No (alpha) | No | Yes |
| Anthropic provider | Yes (via CLI) | Via LiteLLM | Via langchain-anthropic | Yes (native) |
| OpenAI provider | Via CLI | Via LiteLLM | Yes (native) | Yes (native) |
| Google Gemini provider | No | Yes (native) | Via langchain-google | Yes (native) |
| OpenRouter provider | No | Via LiteLLM | Via langchain-openai compat | Yes (native) |
| Ollama provider | No | Via LiteLLM | Via langchain-ollama | Yes (native) |
| Provider-side prompt caching | Partial (Anthropic via CLI) | Gemini only | No | Yes (all 3 that support it) |
| Request-level dedup cache | No | No | No | Yes |
| Built-in coding-agent tool library | Via CLI binary | Partial (code_execution) | Via community | Yes |
| Skills packaging system | Via CLI skills | No | No | Yes |
| Plugin extension model | No | Via Toolsets+Callbacks | Via Toolkits | Yes |
| MCP client (consume servers) | Via CLI | Via McpToolset | Via langchain-mcp-adapters | Yes |
| MCP server (expose capabilities) | No | Via FastMCP | No | Yes |
| Zero mandatory framework dependencies | N/A | No | No | Yes |
| Embedded model catalog (context window, pricing, capabilities) | Via CLI | Partial | Partial | Yes |
| Per-vendor OpenAI-compat quirk profiles | No | Via LiteLLM | Partial | Yes |
| Durable append-only session log with damage-tolerant resume | Via CLI | Session service | Via checkpointer | Yes |
| Mid-run steering / follow-up into a live turn | Via control protocol | Via HITL interrupt | Via interrupt | Yes |
| Wire-level differential testing against an independent reference | N/A | No | No | Yes |
| Default-off trust gate on repository-authored prompt material | Yes (trust prompt) | No | No | Yes |

No existing SDK simultaneously satisfies every requirement above. AgentKit is built to fill that gap.

---

## 3. Goals and Non-Goals

### Goals

**G1 — Embeddable agentic loop.** Implement the full loop (call model, dispatch tools, loop until stop) in Go as an importable library, with no binary entrypoint of its own and no dependency on external agent frameworks. The canonical consumers are tools like nightshift and hub that `import` AgentKit and expose their own CLI/API surface to users.

**G2 — Provider abstraction.** Abstract the Anthropic Messages API, OpenAI Chat Completions, OpenAI Responses, the Google Gemini API and Ollama's native chat API behind a thin `ProviderClient` interface keyed by **wire protocol, not vendor** (REQ-PROV-02). Provider-specific types must not appear in agent logic. Vendors that speak an existing wire protocol — OpenRouter, Groq, DeepSeek, Together, vLLM, gateways — are catalog rows with a compatibility profile, not new implementations.

**G2a — Smart caching.** Implement a multi-level caching subsystem: sticky request routing keyed on a caller-supplied session id (without which provider-side caching is a coin flip on any multi-node endpoint), provider-side prompt caching with a **rolling** breakpoint that extends the cached prefix every turn, request-level deduplication fingerprinted over the exact wire bytes, and tool-schema caching that survives mid-session tool discovery rather than being invalidated by it. The caching layer is transparent — callers see correct results; `Usage` cache-token fields surface the savings.

**G2b — Model catalog.** Embed a catalog supplying the per-model metadata no API returns and no pass-through can synthesize: wire API, base URL, context window, pricing, reasoning support, modality support and compatibility profile. The catalog is not an allowlist — an unrecognized model ID under a known vendor inherits a sibling row and works the day it ships (REQ-CAT-03).

**G3 — Built-in local tool library.** Provide a **deliberately minimal** tool set: file read/write/edit, list, find, grep (with ripgrep acceleration and a real gitignore engine behind it), shell command execution with process-group lifecycle control, and SSRF-protected HTTP fetch. A tool earns a slot only when it has semantics `execute` cannot express; deletion, renaming, appending and stat are one shell word each and ship no tool.

**G4 — Skills packaging format.** Define a TOML manifest + Markdown prompt file + optional tool plugin format, with a discovery/loading pipeline across SDK-built-in, user-global, and project-local directories.

**G5 — Plugin extension model.** Define four plugin categories (backend, tool provider, storage, event hook) via Go interfaces, discoverable via plugin.toml manifests in configured directories.

**G6 — MCP client support.** Consume MCP servers as tool providers using the mcp-go library (`github.com/mark3labs/mcp-go`), exposing them through the same unified tool registry as native tools, with stdio and streamable-HTTP transports.

**G7 — MCP server support.** Optionally expose AgentKit capabilities as an MCP server consumable by Claude Desktop, Claude Code, and other MCP hosts.

**G8 — Two-level streaming.** Provide token-level text deltas and tool-call lifecycle events via a non-blocking `EventStream` (REQ-GO-08). Streaming is the provider primitive; the non-streaming call is derived from it once in the SDK, so the two paths cannot disagree.

**G9 — Three orthogonal extension axes.** Support middleware (wraps `complete()` calls), turn hooks (lifecycle callbacks), and tool interceptors (wraps individual tool executions).

**G10 — Security boundary enforcement.** Enforce path containment for built-in file tools, output size limits, a per-call **tool interception boundary** owned by the embedder (which replaces the command allowlist and shell-operator regex — see REQ-SEC-03/04), an SSRF guard, bounded and strict decoding of every untrusted wire payload, a **default-off project trust gate** on repository-authored prompt material, skill import restrictions, and a plugin private-API blocklist.

**G11 — Durable, resumable sessions.** Persist a session as an append-only event log that survives a process kill mid-write, folds back into a fully configured agent (model, reasoning level, compaction checkpoint, branch), and tolerates structural damage on load rather than refusing it (§6.12).

**G12 — Executable invariants.** Ship the properties this document asserts as tests rather than prose: the dependency budget as a build-graph test (REQ-GO-13), provider wire fidelity as a differential harness against an independent reference (NFR-TEST-06/07), assembled artifacts as byte-for-byte goldens (NFR-TEST-08), and untrusted decoders as fuzz targets (NFR-TEST-09).

### Non-Goals

**NG1** — AgentKit is not a hosted platform or cloud service. It is a library.

**NG2** — AgentKit does not implement LLM inference. It calls external provider APIs.

**NG3** — AgentKit does not provide a web UI, dashboard, REPL, or daemon. It does define the **seams** such a thing would be built against — a phase predicate, a concurrency-safe snapshot with producer identity, an out-of-band abort, and mid-run message queues (§6.13) — because those require changing the loop, which means forking. Transport, framing and RPC are ordinary code any consumer can write on top and are out of scope.

**NG4** — AgentKit does not wrap or depend on LangChain, LangGraph, Google ADK, Claude Code CLI, or the `claude-agent-sdk` package.

**NG5** — AgentKit does not implement a vector database or embedding store; it may define a protocol for a pluggable knowledge store but does not ship one.

**NG6** — MCP Resources and MCP Prompts are not in scope for the initial client release. Only MCP Tools are consumed. Resources and Prompts may be added in a future release.

**NG7** — AgentKit does not provide a graphical agent builder or visual workflow editor.

**NG8** — AgentKit is not a replacement for, nor a competitor to, Claude Code, the Anthropic CLI, nightshift, hub, or any end-user agent tool. It is the layer those tools embed. AgentKit has no user-facing CLI of its own; it exposes Go package APIs only.

**NG9** — AgentKit is not a distribution mechanism for AI capabilities. It does not ship models, manage API subscriptions, or provide a marketplace. Teams that need to expose AgentKit-powered agents to end users must build their own CLI, service, or integration on top of it.

---

## 4. User Personas

### Persona A: Platform Engineer (Go)

Builds internal automation tooling for an engineering organization. Maintains Go services and wants to add agentic capabilities to an existing Go service without adopting a heavyweight framework or introducing a runtime dependency on an interpreter. Needs direct control over retry logic, cost tracking, and context compaction to stay within budget. Will register custom tools against internal APIs. Will write skills to encode company-specific coding standards. Evaluates the SDK by reading the agent loop source code directly — if the loop is opaque, the SDK is disqualified.

**Key needs:** Composable middleware for retries and budget gates; Go-native `context.Context` integration throughout the agent loop; clean separation between session configuration and system prompt; observable cost per turn.

### Persona B: Backend Engineer (Go)

Maintains a Go service that processes engineering issues at scale. Needs to embed agent capabilities in the same process as the Go service without introducing a runtime dependency or spawning subprocess agents. Requires a Go-native interface with goroutine-safe concurrent tool execution and `context.Context` cancellation propagation. Will use the Anthropic provider. Will not use the skills system initially — primarily needs the core loop and tool registration.

**Key needs:** A root module that requires nothing outside the standard library, with `mcp-go` confined to a nested module they can decline to import (REQ-GO-11); tool registration with no code-generation build step (REQ-TOOL-02); typed sentinel errors for programmatic error handling; streaming that cannot stall the loop if their consumer is slow.

### Persona C: AI Application Developer

Building a multi-agent product where one orchestrator delegates to multiple specialist agents. Needs subagent delegation via the tool-as-agent pattern, parallel execution of child agents, and budget propagation across delegation boundaries. Needs streaming events to drive a UI showing tool calls and partial text in real time — including the ability for a user to type *while* the agent is working, and to interrupt it from the UI event loop rather than from the goroutine that called `Run`. Will leverage MCP to integrate third-party tools (GitHub, filesystem, database) without writing custom tool implementations.

**Key needs:** `SubagentTool` delegation wrapper; `errgroup` parallel subagent execution (distinct from the intra-batch tool concurrency of REQ-GO-04); budget propagation as an explicit child config field rather than a `context.Context` value (REQ-MULTI-03); a streaming taxonomy with bracketed blocks and a clear incremental/authoritative split (REQ-OBS-06a); out-of-band `Steer`, `Abort` and `Snapshot` (§6.13); MCP server pool with qualified tool names.

### Persona D: SDK Integrator

Maintains a plugin that adds a new capability domain (Kubernetes tooling, Slack integration) to AgentKit deployments. Publishes the plugin as a Go module with a registered plugin factory. The plugin registers a `ToolProviderPlugin` returning tool handlers and an `EventHookPlugin` logging all tool calls to an external observability platform. Needs the plugin interface to be stable and the private-API blocklist to be clearly documented so they do not accidentally depend on internal packages that may change.

**Key needs:** Stable Go interfaces for all four plugin categories; plugin.toml manifest-based discovery in configured directories; `nightshift --validate-plugins` command to check conformance without starting the daemon.

---

## 5. Core Concepts and Architecture

### The Agentic Loop

The loop repeats per turn until a stop condition is met:

```
PRE:  Append user message to history.
LOOP:
  1. PrepareNextTurn (compaction, context rewrite) — immediately before the request
  2. Drain steering queue into history
  3. Call provider.Stream(history, tools, config) → assistant response
  4. Append assistant response to history
  5. If stop_reason is Error or Aborted: emit TurnEnd, exit run
  6. toolCalls := every tool_use block in response.content
  7. If len(toolCalls) == 0: emit TurnEnd, exit inner loop
  8. Execute tools (parallel if configured); append ONE ToolResultMessage per call
  9. Emit TurnEnd; consult ShouldStopAfterTurn; if true, exit run → goto LOOP
```

**The continuation predicate is `len(toolCalls) > 0`, not `stop_reason == "tool_use"`.** Providers disagree about finish reasons: Gemini returns a STOP-family reason alongside `functionCall` parts, and several OpenAI-compatible gateways emit `finish_reason: "stop"` with a populated `tool_calls` array. A loop gated on the stop reason silently drops those calls and returns an empty answer — a failure that is provider- and gateway-specific and passes every Anthropic-only test. `stop_reason` is normalized to a canonical enum for reporting, telemetry and truncation handling; it never decides whether the loop iterates. `Error` and `Aborted` are the only reasons that short-circuit the loop, and they do so before tool extraction.

Stop policy is a **single post-turn predicate**, `ShouldStopAfterTurn`, consulted after every `TurnEndEvent` — that is, after the turn's tools have executed and their results are in history. `max_turns`, `max_budget_usd`, wall-clock deadlines and sentinel-tool detection are default implementations of that predicate, not loop primitives. A limit checked between "extract tool_use" and "execute tools" ends the transcript with dangling `tool_use` blocks that no provider will accept on resume.

`stop_reason == "max_tokens"` is **not** a terminal condition when the response carries tool calls; it is a recoverable per-call failure that keeps the loop running (REQ-LOOP-10).

**Critical correctness invariant:** every `tool_use` block emitted by a model turn must have a matching tool result in history before the next model call. The canonical transcript carries **one `ToolResultMessage` per tool call**, each with its own `tool_use_id`, tool name, `is_error` flag and timestamp. How those results are packed onto the wire is a **provider concern, not a loop invariant**: the Anthropic adapter coalesces consecutive `ToolResultMessage`s into a single `user` message — splitting them across messages silently degrades parallel tool call quality — while the OpenAI adapters emit one `{"role": "tool", "tool_call_id": ...}` message per result and Gemini one `functionResponse` part per result. Encoding the Anthropic shape into the provider-independent layer makes the transcript unrepresentable for three of the five first-class providers and destroys per-result metadata. When a tool handler raises an exception the loop appends a tool result with `is_error=true` rather than aborting — the model can observe the error and self-correct.
### Type System

AgentKit defines a canonical content block type system that is provider-independent. No provider-specific type leaks into agent logic.

| Type | Fields |
|---|---|
| `AgentConfig` | `model`, `provider`, `max_tokens`, `temperature`, `system_prompt`, `stop_policy` (REQ-LOOP-04), `parallel_tools`, `thinking_level`, `tool_choice`, `cache_retention`, `session_id`, `trust_project`, `tool_policy` (REQ-TOOL-10), `request_options` (REQ-PROV-18) |
| `Message` | Union of `UserMessage`, `AssistantMessage`, `ToolResultMessage` |
| `UserMessage` | `content []ContentBlock`, `timestamp` |
| `AssistantMessage` | `content []ContentBlock`, `stop_reason` (canonical enum), `raw_stop_reason` (provider string, verbatim), `error_message`, `usage`, `timestamp`, plus the provenance set: `provider`, `api`, `model`, `response_model`, `response_id`, `thinking_level` |
| `ToolResultMessage` | `tool_use_id`, `tool_name`, `content []ContentBlock`, `is_error`, `timestamp`, `added_tool_names []string`, `usage` (optional) |
| `ContentBlock` | Union of `TextBlock`, `ImageBlock`, `ToolUseBlock`, `ToolResultBlock`, `ThinkingBlock` |
| `ImageBlock` | `data` (base64), `mime_type` (`image/jpeg`, `image/png`, `image/gif`, `image/webp`) |
| `ToolUseBlock` | `id`, `name`, `input json.RawMessage` (the provider's own argument bytes, verbatim), `input_order OrderedObject` (REQ-TOOL-12), `thought_signature` (opaque, optional) |
| `ThinkingBlock` | `thinking`, `signature` (opaque, provider-issued; may be empty on an aborted stream), `redacted` |
| `Tool` | `name`, `description`, `input_schema` (`*Schema`), `handler`, `constrained_sampling`, `execution_mode` (`Parallel` \| `Sequential`), `prepare_arguments`, `prompt_guidelines`, `label` |
| `RunResult` | `messages`, `stop_reason`, `usage`, `turn_count` |
| `Usage` | `input_tokens`, `output_tokens`, `reasoning_tokens` (subset of output), `cache_read_tokens`, `cache_write_tokens`, `cache_write_1h_tokens` (subset of cache_write), `total_tokens`, `cost_usd`, `billed_model` |

**Stop reasons are a lossy mapping, so keep both.** `stop_reason` is a canonical enum — `Stop`, `Length`, `ToolUse`, `StopSequence`, `Refusal`, `Error`, `Aborted` — mapped by the provider layer. Because providers are inconsistent, the provider's own finish string is preserved verbatim in `raw_stop_reason` for debugging and telemetry. Neither field drives control flow (REQ-LOOP-01).

**`ToolResultMessage` is a first-class message role**, not a content block inside a `UserMessage`. It is the only place per-result `is_error`, tool name, timestamp and usage can live, and it is the only shape representable across all five providers (REQ-LOOP-02).

**Tool-call argument bytes are authoritative; a decoded map is not.** Tool-call argument key order is model-visible: the bytes the model reads on turn N+1 include the arguments it authored on turn N. A `map[string]any` does not preserve key order, and Go map iteration is randomized, so any canonical block that stores tool input as a map silently reorders every tool call it replays. `ToolUseBlock.Input` is therefore the raw bytes as the provider emitted them, authoritative for every replay, fingerprint and serialization path; `InputMap()` decodes lazily for handler and interceptor convenience, and a decoded map is never re-encoded back into a request. Two concrete failures this prevents: provider-side prompt caching keys on an exact prefix, so a reordered replay is a silent cache miss on every turn, visible only in the bill; and any request fingerprint computed over a map is nondeterministic across runs unless it canonicalizes, and once it canonicalizes it no longer identifies the bytes actually sent.

**Provenance is not optional.** Because REQ-PROV-08 permits different models per agent and REQ-SESS-03 permits changing the model mid-session, a single transcript routinely contains turns produced by several providers. Replaying another provider's opaque reasoning signature, or its tool-call ID format, is a hard HTTP 400. The only way to know which is which is to record, per assistant message, the model that produced it. `signature`, `thought_signature` and `raw_stop_reason` are opaque: the SDK stores and replays them unmodified and never inspects them. See REQ-PROV-11 for the replay rules that consume these fields.

**Presence is not emptiness.** Any field whose provider treats *absent* differently from that field's zero value is a pointer or carries an explicit presence flag — never a bare value with `omitempty`. `{}`, `{"thinking": null}` and an absent `thinking` key are three different requests (REQ-PROV-16).

`ToolResultBlock.content` is a **content list**, not a string: a tool may return interleaved text and image blocks. Providers that cannot accept images in tool results degrade to the text blocks with a note; the canonical type does not vary by provider.

Providers translate between canonical types and provider wire formats on every call. The Anthropic provider maps `ToolUseBlock` to `{"type": "tool_use", "id": ..., "name": ..., "input": {...}}` by writing the `Input` bytes into the `input` position unchanged, and maps inbound `{"type": "tool_use"}` blocks back by capturing the `input` span as bytes. The OpenAI provider maps to and from `tool_calls` in Chat Completions format under the same rule.
### Agent as Value Object

An Agent holds its own config, tool registry, history, and middleware chain. It has no global state. Creating a child agent for delegation is constructing a new `Agent` value — not registering with a global executor or inheriting a global session. Value semantics govern *composition*: two agents never share mutable state, and parallel delegation is safe by construction (REQ-MULTI-04).

Value semantics do **not** mean the config is frozen at construction. Model and reasoning level are mutable mid-session (`SetModel`, `SetThinkingLevel`), and each mutation is appended to the session log at the moment it happens (REQ-SESS-03), because a mutation that is not logged is not recoverable on resume. Messages may be queued into a live run (REQ-LOOP-13, REQ-LOOP-14). A run in flight is guarded by a run slot, so "no global state" does not imply "no concurrency rules" (REQ-LOOP-15).

History is owned by the agent and injectable from outside at construction to enable session resumability and testing. It is a **tree**, not a flat list: an append-only durable log cannot delete, so rewind, edit-and-retry and branch navigation are expressed by re-parenting (REQ-SESS-07), and the active conversation is the root→leaf path. `RunResult.messages` is the flattened active branch — a view, not the durable representation, and not what a caller should persist.

The system prompt is stored on `AgentConfig`, not in history, so it is excluded from token count estimates and can be updated without mutating conversation state.

**Context convention.** `context.Context` carries cancellation and deadlines and nothing else. Budget, telemetry parents, session identity, credentials, scoped environment overrides and request options are explicit struct fields, never `context.WithValue` payloads. A value smuggled through `ctx` is invisible to the type system, untestable in isolation, and silently absent whenever a caller passes a bare `context.Background()`.
### Extension Axes

Three orthogonal extension axes are defined, each with a distinct scope:

**Axis 1 — Middleware** wraps the entire model call. Last registered is outermost, first to execute. Built-in middlewares include: `RetryMiddleware` (backoff over the two retry layers of REQ-PROV-13/14), `RateLimitMiddleware` (token bucket), `BudgetMiddleware` (cost gate before each turn), `TracingMiddleware` (spans over the REQ-OBS-01 interface), `CachingMiddleware` (identical request deduplication). Middleware operates on **canonical types**: it can change *what* is asked for, not *how* the provider encoded it. Post-serialization interception is a separate seam (REQ-PROV-18). History compaction is **not** a middleware — it is a context transform applied inside the loop (REQ-GO-12), because a compaction middleware's own summarization call would re-enter the middleware chain, be fingerprinted by the dedup cache, and be charged against the budget gate.

**Axis 2 — Turn hooks** fire at lifecycle points (`OnTurnStart`, `OnTurnEnd`, `OnAgentDone`, `OnError`) without modifying request/response. Hooks are for observation, not for interception.

**Axis 3 — Tool interceptors** wrap individual tool executions. The `BeforeToolCall` interceptor is the SDK's single tool-authorization boundary (REQ-SEC-03); the caching interceptor is innermost (skip the work on a cache hit).

All three axes execute third-party code inside the loop, and all three are invoked through a `recover()`ing wrapper while holding no agent lock (NFR-REL-02).
### Skills and Plugins Relationship

| Concept | What it extends | Scope | Who writes it |
|---|---|---|---|
| Archetype | Session execution parameters | Agent runtime | SDK authors / operators |
| Skill | Agent behavior — prompt additions and tools | Session | Domain experts |
| Plugin | SDK runtime infrastructure | Framework-wide | Third-party integrators |

The `steering.md` convention used in the existing nightshift codebase is a degenerate always-on skill with no manifest. Skills target specific archetypes; plugins are framework-wide. Skill plugin code is sandboxed: it may not import internal agentkit packages, any LLM client library, or model API packages.

---

## 6. Feature Requirements

### 6.1 Core Agent Loop

- **REQ-LOOP-01:** The loop must continue on the **presence of `tool_use` content blocks** in the assistant response, never on `stop_reason`. The continuation predicate is `len(ExtractToolUse(response)) > 0`, evaluated by scanning `response.Content` for `ToolUseBlock`. `Stop`, `StopSequence`, `Length` and any unrecognized canonical reason are treated identically for control-flow purposes: if the content carries tool calls they are executed and the loop iterates; if it does not, the inner loop exits. Only `Error` and `Aborted` short-circuit before tool extraction. The provider's finish reason is normalized into `AssistantMessage.stop_reason` and preserved verbatim in `raw_stop_reason` for reporting, telemetry and truncation handling — neither field may be read by the continuation check.
- **REQ-LOOP-02:** The canonical transcript records **one `ToolResultMessage` per tool call**, in the order the calls appeared in the assistant message, each carrying its own `tool_use_id`, `tool_name`, `is_error`, `timestamp` and optional `usage`. Coalescing is a provider responsibility:

  | Provider | Required wire shape for a batch of N tool results |
  |---|---|
  | Anthropic | Scan forward over consecutive `ToolResultMessage`s and emit them as ONE `{"role": "user"}` message whose content holds all N `tool_result` blocks. Splitting them across messages silently degrades parallel tool call quality. Non-`tool_result` content on those results must be displaced into sibling blocks positioned **after** every `tool_result` block — Anthropic rejects the interleaved mix. |
  | OpenAI (Chat Completions / Responses) | Emit N separate `{"role": "tool", "tool_call_id": ...}` messages, one per result. Grouping is not representable. |
  | Google Gemini | Emit one `functionResponse` part per result. |
  | OpenRouter / Ollama | Follow the OpenAI-compatible shape. |

  The provider-independent layer must never assume a grouping. A loop or storage layer that flattens results into content blocks of a shared user message discards `is_error`, tool name, timestamp and usage, and is unimplementable for three of the five first-class providers.
- **REQ-LOOP-03:** Tool handler exceptions must be caught and converted to `ToolResultMessage{is_error: true}`. The loop continues rather than aborting.
- **REQ-LOOP-04:** Stop policy is a **single caller-supplied predicate**, not a priority-ordered list of loop conditions:

  ```go
  type StopContext struct {
      Message     *AssistantMessage   // the turn that just completed
      ToolResults []ToolResultMessage // its results, already in history
      History     *ConversationHistory
      NewMessages []Message           // everything appended during this run
      TurnCount   int
      Usage       Usage               // cumulative for the run
  }

  type StopPolicy func(StopContext) bool
  ```

  The predicate is consulted once after every `TurnEndEvent`. Returning `true` emits `AgentDoneEvent` and returns **before** the steering queue is polled and before `PrepareNextTurn` runs. Policies compose via `StopAny(policies...)`.
- **REQ-LOOP-04a:** The stop check must run **after** the completed turn's tools have executed and their results are appended to history. It may never run between tool_use extraction and tool execution: a transcript whose last assistant message carries `tool_use` blocks with no matching results is rejected by every provider and cannot be resumed (NFR-REL-04).
- **REQ-LOOP-04b:** `PrepareNextTurn` (compaction and any other context rewrite) runs at the **head of the next iteration, immediately before the provider request it prepares** — not directly after `TurnEndEvent`. It therefore never fires for a turn that will not happen, and it does cover the request issued after a tool batch.
- **REQ-LOOP-05:** Parallel tool execution is opt-in via `AgentConfig.parallel_tools=true` and runs in three phases with distinct concurrency rules:
  1. **Prepare — strictly sequential, on the loop goroutine.** For each call in order: emit `ToolCallStartEvent`, run argument preparation and validation (REQ-TOOL-11), run the `BeforeToolCall` interceptor, produce a per-call thunk. A call blocked by an interceptor is finalized inline here.
  2. **Execute — parallel.** Thunks run in one goroutine each, joined by `sync.WaitGroup`. **Only the tool handler body runs outside the lock.**
  3. **Finalize — serialized under one mutex.** `AfterToolCall` interceptors, any mutation of shared agent context, and `ToolCallEndEvent` emission run inside a func-scoped critical section with a deferred unlock, so a panicking interceptor or event listener cannot leak the mutex and deadlock the remaining tool goroutines at the join.

  Results are written into a pre-sized slice **by slot index** and the `ToolResultMessage`s are appended only after the join, in slot order. Tool ordering in the transcript is therefore independent of completion order.
- **REQ-LOOP-05a:** `Tool.ExecutionMode` may be `Parallel` (default) or `Sequential`. **A single `Sequential` tool anywhere in a batch demotes the entire batch to sequential execution.** Tools with process-wide or workspace-wide side effects — `execute`, `run_command` — ship as `Sequential`.
- **REQ-LOOP-05b:** A failing tool must never cancel its peers. Every call in a batch must produce a result — a handler error, a panic, an interceptor block, a validation failure and an abort all become a `ToolResultMessage` with `is_error=true`. Any batch that returns fewer results than calls leaves dangling `tool_use` blocks and makes the next request invalid.
- **REQ-LOOP-06:** In Go, a single `Agent` type suffices. There is no sync/async distinction.
- **REQ-LOOP-07:** `MaxTurns` is a built-in `StopPolicy` implementation, not a loop primitive: `StopAfterTurns(n)` returns `true` when `TurnCount >= n`. The run ends with `RunResult.StopReason = "max_turns"`; `ErrMaxTurns` is returned only when `AgentConfig.ErrorOnLimit` is set.
- **REQ-LOOP-08:** `MaxBudgetUSD` is a built-in `StopPolicy` implementation: `StopOverBudget(usd)` returns `true` when cumulative `Usage.CostUSD` exceeds the limit. Because the check runs post-turn, a run may overshoot the budget by at most one turn plus its tool batch; the pre-turn gate that prevents overshoot is `BudgetMiddleware` (Axis 1), which is a separate mechanism.
- **REQ-LOOP-09:** The loop supports cancellation via `context.Context`. On cancellation the in-flight stream resolves to an `AssistantMessage` carrying the partial content accumulated so far, `stop_reason = "aborted"` and `error_message` set, and **that message is appended to history verbatim**. Provider failure and a recovered panic terminate the turn the same way, with `stop_reason = "error"`. "Without corrupting history state" means *writing this terminal marker*, not leaving history untouched: a turn that started always has a terminal message, and two other subsystems depend on it — REQ-PROV-11 drops it from the outbound request, and REQ-GO-15 skips it when choosing a usage anchor. History after an abort is therefore **recoverable, not clean**: the aborted turn can contain half-streamed tool calls with no results and thinking blocks with no signature, and making it sendable again is the job of the repair pass in REQ-PROV-11. Implementations must not keep history sendable by suppressing or rewriting the aborted message at abort time — the partial content is what the UI transcript and the session log display.
- **REQ-LOOP-09a:** An abort is terminal for the run and is never retried (REQ-PROV-14). An abort landing during a retry backoff sleep normalizes to an aborted `AssistantMessage` with the error message cleared. `ctx` is one of two cancellation channels: the run also stores its own canceller so `Agent.Abort()` (REQ-LIFE-04) can stop a turn without access to the `ctx` the caller passed to `Run`. Cancelling a `ctx` passed to any *other* `Agent` method must never terminate a running turn.
- **REQ-LOOP-10:** When an assistant message has `stop_reason == "max_tokens"` **and** carries tool calls, the loop must execute **none** of them. It must instead synthesize an error tool result for **every** call in the batch, emit the normal event sequence for each so the transcript and the UI stay well-formed, and **continue the loop** so the model can re-issue. The result text is fixed:

  ```
  Tool call "<name>" was not executed: the response hit the output token limit,
  so its arguments may be truncated. Re-issue the tool call with complete arguments.
  ```

  The rationale is normative for implementers: streamed tool-call arguments are finalized by a best-effort JSON salvage parser so the SDK can render arguments live and so a stream cut mid-object still produces a value. That parser turns a truncated `{"path":"/et` into a syntactically valid object that passes JSON Schema validation. A truncated `edit_file` whose `new_string` was cut off therefore applies cleanly and silently corrupts the file. Argument validation cannot detect this; only the stop reason can.
- **REQ-LOOP-10a:** `stop_reason == "max_tokens"` with **no** tool calls is not special-cased: the inner loop exits normally and the reason is reported on `RunResult`.
- **REQ-LOOP-11 (abort granularity):** Cancellation of a tool batch is **all-or-nothing and decided once**, on the loop goroutine, after the sequential prepare phase and **before any tool handler starts**. The decision must not be re-checked inside each tool goroutine.
  1. If the context is cancelled at the decision point, **no** handler in the batch runs. Every non-blocked call short-circuits to a `ToolResultMessage` with `is_error=true` and content `"Operation aborted"`; `AfterToolCall` runs for none of them.
  2. If the context is cancelled during the prepare phase, the prepare loop stops enqueuing further calls, but calls already prepared are still covered by the single decision.
  3. An aborted call still emits its start and end events and a tool result, so the transcript stays well-formed and resumable.
  4. A listener error or panic raised while emitting an aborted call's end event must be captured and re-raised only after all in-flight tool goroutines have been joined.

  Per-goroutine `ctx.Err()` checks — the obvious Go idiom, and what REQ-GO-05 alone implies — let the scheduler split the batch: an abort landing just after the batch starts skips whichever calls had not yet been scheduled and runs the rest. That is nondeterministic, unreproducible in tests, and shows up in production as phantom side effects after the user pressed Ctrl-C.
- **REQ-LOOP-12 (concurrent tool safety):** With `parallel_tools=true`, correctness comes from per-resource serialization, not only from marking tools sequential. Every mutating built-in file tool (`write_file`, `edit_file`) runs inside a **file mutation queue**: a refcounted per-path mutex keyed by the **symlink-resolved absolute path**, so two spellings of one file share a lock while operations on different files stay concurrent. A path that does not yet exist falls back to its absolute form; any other resolution error propagates rather than being swallowed. Map entries are released at refcount zero so the lock table cannot grow unbounded. A test must assert that two concurrent `edit_file` calls against the same file — including via a symlink and a relative spelling — serialize, and that edits to distinct files do not. Models routinely emit two edits to the same file in one batch; without this they interleave read-modify-write and one silently loses.
- **REQ-LOOP-13 (steering queue):** `Agent.Steer(msg) error` enqueues a message for delivery into the **current** run. The queue is polled after every `TurnEndEvent` and drained at the head of the next iteration, immediately before the provider request — never between the assistant response and its tool results, which would violate REQ-LOOP-02. Pending steering messages keep the inner loop alive even when the assistant produced no tool calls: the loop condition is `hasMoreToolCalls || len(pending) > 0`. Because `PrepareNextTurn` may be long-running, the queue is polled a **second** time after preparation returns, guarded by `if len(pending) == 0` so that one-at-a-time mode does not deliver two messages into a single turn.
- **REQ-LOOP-14 (follow-up queue):** `Agent.FollowUp(msg) error` enqueues a message polled **only when the inner loop is exhausted**. A pending follow-up restarts the outer loop within the same run: no second `AgentStartEvent`, no `AgentDoneEvent`, one `RunResult`.
- **REQ-LOOP-15 (queue semantics and the run slot):** Both queues default to `QueueOneAtATime` — a single poll yields at most one message — configurable to drain-all per queue. `ClearSteeringQueue()`, `ClearFollowUpQueue()`, `ClearAllQueues()` and `HasQueuedMessages()` are provided. `Run`/`Stream` invoked while a run is active must fail fast with `ErrBusy` rather than blocking or interleaving: a prompt queued behind a running turn was written against a transcript the caller could see, and by the time it would run the transcript has changed underneath it — the decision to retry, queue or reject belongs to the caller. Any entry point that resumes a run must **claim the run slot and drain the queues under a single lock**; claiming after draining lets a concurrent `Run` win the slot and silently discard the drained messages.
- **REQ-LOOP-16 (Continue):** `Agent.Continue(ctx) (RunResult, error)` resumes a transcript without supplying a new user message. It is a distinct operation from `Run(ctx, prompt)` and is required for resuming a session loaded from disk. Shape precondition, by the role of the last message:
  - `user` or `tool_result` — resume the loop with no new message. The model owes a reply; `Run` cannot express this because it would append a spurious user turn.
  - `assistant` — drain the steering and follow-up queues and run with those. If both are empty, return an error: a completed assistant turn is not continuable.

  Resuming a transcript that ends in a `tool_result` is the normal outcome of REQ-LOOP-09 cancellation, so `Continue` is not an optional convenience.
### 6.2 Model Provider Abstraction

- **REQ-PROV-01:** **Streaming is the primitive.** A `ProviderClient` implements a single required method, `Stream(ctx, model, req, opts) *EventStream`. `Complete()` is a derived convenience implemented once in the SDK as `Stream(...).Result()` — never per provider. A provider therefore has exactly one place that parses its wire format, and the streaming and non-streaming paths cannot disagree.
- **REQ-PROV-02:** Provider implementations are keyed by **wire API**, not by vendor. Vendor identity selects credentials, base URL and catalog row; it never selects an implementation.

  | `Api` | Wire protocol | Vendors served (examples) |
  |---|---|---|
  | `anthropic-messages` | Anthropic Messages | Anthropic; OpenRouter `anthropic/*` |
  | `openai-completions` | OpenAI Chat Completions | OpenRouter, Groq, Cerebras, DeepSeek, Together, xAI, Moonshot, Fireworks, Nvidia, vLLM, llama.cpp, Cloudflare AI Gateway |
  | `openai-responses` | OpenAI Responses | OpenAI, Azure OpenAI |
  | `google-generative-ai` | Gemini `generateContent` | Google AI Studio, Vertex AI |
  | `ollama-chat` | Ollama native `/api/chat` | Ollama |

  `openai-completions` and `openai-responses` are **separate implementations, not one implementation with a flag**. They differ in the message model (messages vs. items), the tool-call identity model (`call_id` vs. composite `callId|itemId`), the reasoning-replay model, the caching parameters, and the billing model (Responses applies a service-tier multiplier post-hoc). A model's catalog row selects which API it uses; there is no runtime toggle. Adding a vendor is a catalog row (REQ-CAT-01), not a new provider implementation — "OpenRouter" and "Ollama" are vendor ids, not provider implementations. One vendor MAY route different models to different APIs: the OpenRouter vendor dispatches `anthropic/*` to `anthropic-messages` and everything else to `openai-completions`, and the design must permit this without a special case.
- **REQ-PROV-03:** Each provider translates canonical `Message`/`ContentBlock` types to and from the provider wire format. No provider-specific types in agent logic.
- **REQ-PROV-04:** Provider failures are **encoded in the returned stream, not returned as a Go error**. Once `Stream` is invoked, request, model, transport and runtime failures must terminate the stream with an error event and produce an `AssistantMessage` carrying `stop_reason: "error"` and `error_message`. This includes failures raised before any bytes are sent — an unregistered `Api`, an unusable option — which return a pre-closed error stream rather than an error value. A streaming provider can emit half an assistant message and then fail (SSE truncation, a mid-stream `error` event, a refusal fallback); the partial content plus the failure must be representable in one value, because the retry classifier (REQ-PROV-14) and session persistence both consume it.
- **REQ-PROV-05:** Each provider populates `Usage` on every response and computes `cost_usd` itself, against the model that actually **served** the request. Every trap below produces a wrong number rather than an error and must be handled explicitly:
  1. **Cached tokens are inside the prompt total.** OpenAI-family APIs report `prompt_tokens` inclusive of cached tokens. Providers must compute `input_tokens = prompt_tokens - cache_read_tokens - cache_write_tokens` before costing. Setting `input_tokens = prompt_tokens` double-counts the cached portion and overstates cost by up to ~90% on a well-cached agent loop, which then silently trips the REQ-LOOP-08 budget gate.
  2. **The cached count lives in three places.** Check `prompt_tokens_details.cached_tokens` (OpenAI, OpenRouter), `prompt_cache_hit_tokens` (DeepSeek), and a top-level `cached_tokens` (Moonshot). All three arms are required.
  3. **1h cache writes bill at 2x base input.** `cache_write_1h_tokens` is a subset of `cache_write_tokens`; cost is `rate_cache_write * (cache_write - cache_write_1h) + rate_input * 2 * cache_write_1h`.
  4. **Pricing tiers are request-wide.** Tier selection uses `input + cache_read + cache_write` and picks the highest tier whose threshold is **strictly** exceeded; that tier's rates then apply to the whole request, output included.
  5. **Bill the served model.** When a provider serves a different model than requested (a server-side refusal fallback), rebuild the usage model from the served model's catalog row and record it in `billed_model`. If a later event names the requested model again, the response must be repriced **back**.
  6. **Service tiers multiply post-hoc.** OpenAI Responses applies a per-tier multiplier to the computed cost (flex 0.5x, priority 2x).

  The session layer only sums per-message `Usage`; it never recomputes cost. Providers that do not expose a field set it to zero — and a zero-usage response must never be treated as an authoritative measurement (REQ-GO-15).
- **REQ-PROV-06:** Retry is specified at **two independent layers** with independent budgets — the transport layer retries HTTP requests (REQ-PROV-13), the semantic layer retries completed-but-failed assistant messages (REQ-PROV-14). Neither subsumes the other, and a single `RetryMiddleware` keyed on 429/5xx is insufficient.
- **REQ-PROV-07:** The Anthropic provider supports the server-side compaction mechanism (`compact-2026-01-12` beta) as an opt-in optimization: compaction blocks from the response are passed back unchanged in subsequent turns. It is not the default — see OQ-4.
- **REQ-PROV-08:** Per-agent model selection is supported. Different agents in the same delegation tree can use different models or providers. Mid-session model changes are supported and are made safe by REQ-PROV-11.
- **REQ-PROV-09:** Registration is `RegisterAPIProvider(APIProvider{Api, Stream})`, keyed by the `Api` string and held on `AgentKitConfig`, not in a package-level global (REQ-PLUGIN-11). Registration by vendor name is prohibited: a vendor-keyed registry forces either near-duplicate implementations per vendor or an undocumented internal re-dispatch. `AgentConfig.Provider` names a **vendor** and is used only for credential resolution (REQ-AUTH-03) and catalog lookup (REQ-CAT-02). Third-party wire protocols register via `BackendPlugin` supplying a new `Api` value.
- **REQ-PROV-10:** Every request resolves to a `Model` descriptor before dispatch, carrying at minimum `ID`, `Name`, `Api`, `Provider`, `BaseURL`, `Headers`, `Compat`, `ContextWindow`, `MaxTokens`, `Cost`, `Input` (modalities), `Reasoning` and `ThinkingLevelMap`. Dispatch is `GetAPIProvider(model.Api)`.
- **REQ-PROV-11 (send-time transcript repair):** Before serializing any request, every provider applies a shared, unconditional transform over canonical history. It is part of the `ProviderClient` contract, not the loop — the loop is not running when a transcript is loaded from disk, and no caller may be able to skip it. The transform produces a **view**; it never mutates history and is never persisted. Rules, in order:

  | # | Rule | Failure it prevents |
  |---|---|---|
  | 1 | Compute `same_model = (msg.provider, msg.api, msg.model) == target` per assistant message | Everything below depends on it |
  | 2 | Drop assistant messages whose `stop_reason` is `Error` or `Aborted` | Unsigned thinking blocks and partial tool calls with no results |
  | 3 | When not `same_model`: downgrade signed `ThinkingBlock`s to `TextBlock`s, drop `redacted` thinking blocks, strip `thought_signature` | Replaying another vendor's opaque signature is a hard 400 |
  | 4 | Demote `ThinkingBlock`s with a missing or empty `signature` to plain text; drop if empty | Anthropic rejects unsigned thinking replayed from an aborted stream |
  | 5 | When not `same_model`: rewrite tool-call IDs to the target's format (Anthropic: non-`[A-Za-z0-9_-]` → `_`, truncate 64) and apply the same mapping to every matching `tool_result` | Cross-provider replay rejected on ID format |
  | 6 | At every assistant→user boundary and at the end, insert a synthetic `ToolResultMessage{content: "No result provided", is_error: true}` for any `tool_use` with no matching result | Every provider 400s on an unanswered `tool_use` |
  | 7 | Replace `ImageBlock`s when `model.Input` lacks `image` (REQ-CAT-05) | 400 on an unsupported modality |

  An orphaned `tool_use` is what every cancellation, crashed tool and killed process leaves behind. Repairing the view at serialization time — rather than on load, or by refusing to persist — keeps the durable log complete while guaranteeing a valid request. Rule 2 is lossy by design: the partial text of an aborted turn stays visible in the UI transcript and the session log but is invisible to the model on the next turn. Token accounting applies the same exclusion (REQ-GO-15).
- **REQ-PROV-12 (compatibility profiles):** "OpenAI-compatible" is not a base-URL swap. Each model resolves a **compatibility profile** — a struct of named quirk flags — inferred from `model.Provider` + `model.BaseURL` and overridable key-by-key from the catalog row's `Compat` field. The `openai-completions` profile must cover at minimum:

  | Flag | Effect | Vendors requiring the non-default |
  |---|---|---|
  | `UseMaxTokens` | emit `max_tokens` instead of `max_completion_tokens` | DeepSeek, Moonshot, Together, Nvidia, Cloudflare AI Gateway |
  | `SupportsStore` | emit `store: false` | false on every non-`api.openai.com` host |
  | `SupportsDeveloperRole` | `developer` vs `system` role | false except OpenRouter `anthropic/*` and `openai/*` |
  | `SupportsReasoningEffort` | emit `reasoning_effort` | false on xAI, Moonshot, Together, Nvidia, CF Gateway |
  | `ThinkingFormat` | wire shape for reasoning replay | one of `openai`, `deepseek` (`reasoning_content` echoed back), `together`, `openrouter`, `chat-template` |
  | `ThinkingTokenBudgetField` | field name for the reasoning budget | `thinking_token_budget` (vLLM) / `thinking_budget` (Qwen) / `thinking_budget_tokens` (llama.cpp) |
  | `SupportsStrictTools` | emit `strict: true` on tool schemas | false on Moonshot, Together, Nvidia, CF Gateway |
  | `SupportsLongCacheRetention` | emit 1h cache TTL | false on Together, Cloudflare, Nvidia |
  | `SupportsFinishReason` | trust `finish_reason` to terminate | false on servers that never emit it — infer from content instead of erroring |
  | `AllowsNullAssistantContent` | send `content: null` vs `""` | some gateways reject `null` |
  | `AllowsUserAfterToolResult` | send a user message directly after a tool result | where false, inject a synthetic assistant turn between them |
  | `RequiresToolResultName` | emit `name` on tool-result messages | required by some gateways |
  | `CacheControlFormat` | emit Anthropic `cache_control` over the Chat Completions wire | OpenRouter `anthropic/*` |

  Each flag corresponds to a request that 400s, hangs, or silently produces no answer when the default is used. A profile flag is added only with a named vendor and a reproducing case. `openai-responses`, `anthropic-messages` and `google-generative-ai` carry their own profile structs with disjoint keys; profiles are not shared across APIs.
- **REQ-PROV-13 (transport retry layer):** Wraps the HTTP request inside the provider.
  - Retryable statuses: `408`, `409`, `429`, and all `>= 500`. A response header `x-should-retry: true|false` overrides the status logic entirely, in both directions.
  - Backoff: `min(500ms * 2^attempt, 8s)` with up to 25% **downward** jitter. Jitter never lengthens a delay.
  - A server-dictated delay wins over computed backoff, read in order: `retry-after-ms`, then `Retry-After` as seconds, then `Retry-After` as an HTTP date.
  - If the server-dictated delay exceeds `MaxRetryDelayMs` (default 60 s), the request is **abandoned with a typed error**, not clamped and not slept. A `Retry-After: 3600` is not a transient throttle; sleeping an hour inside a request is worse than failing.
  - Default `MaxRetries` is **0** — a single attempt. Retry policy is owned above the transport (see OQ-9).
- **REQ-PROV-14 (semantic retry layer):** Operates on a completed `AssistantMessage` with `stop_reason == "error"`, classifying `error_message` text.
  - A **non-retryable denylist is checked first** and wins: `insufficient_quota`, `quota exceeded`, `billing`, `out of budget`, `available balance`, monthly/usage-limit wording. Retrying an out-of-credit 429 burns the backoff budget and returns the same error.
  - A retryable allowlist is checked second: `overloaded`, `rate limit`, `too many requests`, bare `429`/`500`/`502`/`503`/`524`, `socket hang up`, `getaddrinfo`, `EAI_AGAIN`, `ECONNRESET`, `stream ended before message_stop`, `ResourceExhausted`, `you can retry your request`. A large fraction of real provider failures are not HTTP statuses at all — truncated SSE streams, DNS failures, gateway text bodies — and only text classification catches them.
  - `stop_reason == "aborted"` is terminal and is never retried. A `context.Context` cancellation landing **during** the backoff sleep is normalized into an aborted `AssistantMessage`, not an error.
- **REQ-PROV-15 (reasoning effort):** Reasoning effort is a first-class request parameter, not a content-block type. `AgentConfig.ThinkingLevel` is one of `minimal | low | medium | high | xhigh | max`; a model's catalog row additionally supports `off`.
  - Each `Model` carries a `ThinkingLevelMap` mapping each level to that model's wire value. An entry that is **present and null** means "explicitly unsupported" and is distinct from an **absent** entry. `xhigh` and `max` are opt-in: absent means unsupported.
  - `ClampThinkingLevel` searches **upward** from the requested level for the first supported level, and only if none is found searches downward. Requesting `xhigh` on a model whose map is `{xhigh: null, max: "max"}` clamps **up** to `max`; requesting `max` on a plain reasoning model clamps down to `high`. A user asking for maximum thinking gets the most the model offers, never silently less.
  - Passing an unclamped level through is prohibited: `reasoning_effort: "xhigh"` to a model that does not know it is a 400.
  - Per-API resolution differs and belongs in the adapter: Anthropic uses a tri-state where undefined omits the key and `false` sends `{"type": "disabled"}`; Google requires a per-family disable config (some Gemini families cannot disable thinking and take a floor level instead of a zero budget) and per-family token-budget tables; `openai-completions` endpoints that share `max_tokens` between reasoning and the answer require an explicit budget under the field name their compat profile specifies — without one, a reasoning-heavy turn consumes the whole response and emits no answer.
- **REQ-PROV-16 (presence semantics):** Any canonical field whose provider treats *absent* differently from that field's zero value must be a pointer or carry an explicit presence flag. This covers at least `temperature`, `max_tokens`, `top_p`, `strict`, every thinking/reasoning toggle, and every `Usage` cache-token field.
  1. A field the caller never set is omitted from the wire payload. A field explicitly set to the zero value is emitted as that zero value.
  2. `omitempty` is **prohibited** on any struct field reachable by a provider wire encoder. Presence is decided by the pointer or flag, never by emptiness.
  3. Where a provider's own default for a boolean is `true`, that default is seeded explicitly. Go's `false` zero value must never stand in for an unset boolean whose provider default is `true`.
  4. The same rule governs decoding: an explicit `0` returned by a provider must beat any SDK fallback, so numeric usage fields decode into pointers before being normalized onto `Usage`.
  5. `map[string]any` must not be used to carry an authored ordering — Go marshals it with sorted keys unconditionally.
- **REQ-PROV-17 (byte-faithful tool-call replay):** Providers must carry tool-call argument bytes through unchanged in both directions. On decode, the provider stores the argument JSON exactly as received into `ToolUseBlock.Input`; on replay, it writes those bytes back into the request without a decode-and-re-encode round trip. A streaming provider that reassembles partial argument fragments stores the concatenation of those fragments, and must produce byte-identical `Input` to what its non-streaming path produces for the same tool call — including when the fragments required repair. Byte-identity of streaming vs. non-streaming replay is a per-provider conformance test.
- **REQ-PROV-18 (request escape hatches and transport injection):** Middleware (Axis 1) operates on canonical types and cannot reach the encoded provider request. `AgentConfig.RequestOptions` is applied to every provider call:

  | Field | Type | Purpose |
  |---|---|---|
  | `Headers` | `map[string]*string` | Extra HTTP headers merged into every request. A **nil value suppresses** a provider default header of that name. |
  | `TimeoutMs` | `int` | Per-request timeout, independent of the `context.Context` deadline. |
  | `MaxRetries`, `MaxRetryDelayMs` | `int` | Transport retry bounds (REQ-PROV-13). |
  | `SessionID` | `string` | Provider-side cache key and routing affinity (§6.2a). |
  | `CacheRetention` | enum | Per-request override of §6.2a Level 1; must be settable to "none". |
  | `Env` | `map[string]string` | Scoped credential/environment overrides, consulted before `os.Getenv` (REQ-AUTH-03). |
  | `Transport` | `http.RoundTripper` | Injected transport, for tests and corporate proxies. |
  | `StreamFn` | `func(...)` | Injected stream function — the seam NFR-TEST-01 and NFR-TEST-05 require. |
  | `OnPayload` | `func(payload any, model *Model) (any, error)` | Invoked **after** canonical→wire translation and before the first byte is written. Returning `(nil, nil)` leaves the payload unchanged; a non-nil payload replaces it; a non-nil error aborts the call before any network I/O and must propagate to the caller unmodified. |
  | `OnResponse` | `func(resp ProviderResponse, model *Model) error` | Invoked after the response is received, before canonical decoding. |

  `OnPayload`/`OnResponse` are public, supported API, not test scaffolding. Every real integration eventually needs a beta header combination, an undocumented field, or a provider-specific routing key the canonical types cannot express; these are the only supported way to reach provider-specific JSON, and nothing in the canonical layer may be widened to accommodate one provider's wire quirk. They are also what makes NFR-TEST-06 possible with no API key and no network. An integration test must assert that `Temperature`, `MaxTokens`, a custom header and an `OnPayload` rewrite all reach the wire through a full `Agent` → provider path.
- **REQ-PROV-19 (deferred requests):** Background/deferred submission is an **optional** provider capability, capability-probed rather than assumed. A provider that supports it accepts `DeferredRequest{Window}` and returns an assistant message with `stop_reason = "deferred"` carrying a durable `DeferredHandle{provider, model_id, api, id, expires_at, poll_after_ms, data}` **instead of content**, redeemable later via `FetchDeferred(ctx, model, handle, opts)`. A loop that treats a deferred message as a normal completion returns an empty answer. Handles are serialized into the session log like any other entry so a deferred submission survives process restart.
### 6.2a Smart Caching

AgentKit implements a four-level caching strategy. Levels compose — a request can be a hit at any level independently.

**Level 0 — Sticky request routing (provider-side, makes Level 1 hit at all)**

`AgentConfig.SessionID` is a caller-supplied stable identifier for a conversation. It does two things no other field can:

- **Cache key.** OpenAI-family APIs take `prompt_cache_key` (clamped to 64 runes — the API rejects longer). Without it, cache hits are best-effort prefix matching; with it they are addressed.
- **Session affinity.** Gateways that front a fleet route on a session header so the request lands on the node whose KV cache is already warm. A perfectly constructed cacheable prefix still misses if the request reaches a different node.

Level 1 without Level 0 is a coin flip on any multi-node endpoint. `SessionID` is per-conversation and must never carry a user identity, a workspace path, or request content.

**Level 1 — Provider-side prompt caching (server-side, reduces billable input tokens)**

- **Anthropic:** Cache retention is a tri-state — `none` | `short` | `long` — defaulting to `short` (caching ON) when the caller says nothing; `long` emits `"ttl": "1h"` only when the model's compat profile reports the endpoint supports it. When retention is not `none`, exactly one `cache_control: {"type": "ephemeral"}` marker shape is resolved and stamped in three places per request:
  1. every system text block;
  2. the **last** tool in the tools array only — the marker covers all preceding tools, so one breakpoint suffices and the four-breakpoint budget is not spent on the tool list;
  3. after message conversion, the last content block of the **last** message, if that message is a user message and the block is `text`, `image` or `tool_result`.

  Placement (3) is a **rolling** breakpoint: it moves forward every turn, extending the cached prefix to include the previous turn's tool results. Breakpoints are recomputed on **every** request; there is no structural-hash comparison and no "recompute only when the system prompt or tool list changes" optimization — that optimization is what produces a static prefix-only breakpoint, which re-pays full input price on the entire growing transcript, the dominant cost in exactly the multi-turn agent workload AgentKit exists for. Marking every tool block is equally prohibited: it spends the breakpoint budget for nothing.
- **Google Gemini:** For sessions with a large, stable system prompt (>32k tokens), AgentKit optionally creates an explicit `CachedContent` resource and references it in subsequent calls. For smaller prompts, implicit caching applies automatically. `context_cache_ttl` defaults to 60 minutes.
- **OpenAI:** Caching applies automatically for prompts over 1024 tokens on supported models and is addressed by `prompt_cache_key` (Level 0). AgentKit additionally structures messages to maximize reuse: system message first, stable content before dynamic content.
- **OpenRouter:** Cache behaviour is delegated to the underlying model; `cache_control` is emitted over the Chat Completions wire where the compat profile declares support (REQ-PROV-12).
- **Ollama:** No provider-level caching. Level 2 applies.

**Level 2 — Request-level deduplication (in-process, zero-latency hit)**

- **REQ-CACHE-01:** `CachingMiddleware` maintains a bounded LRU cache of request fingerprints → response. The fingerprint is a SHA-256 over the **serialized request bytes** — the exact bytes the provider would put on the wire, including preserved tool-argument key order (REQ-TOOL-12) — never over a `map[string]any`. Go map iteration order is randomized, so a map-derived fingerprint is nondeterministic across runs; canonicalizing the map to fix that produces a stable fingerprint that no longer identifies the bytes actually sent, which is worse.
- **REQ-CACHE-02:** Cache is scoped per `Session` by default. An optional shared `CacheStore` allows cross-session sharing for workloads that reuse a large common prefix.
- **REQ-CACHE-03:** Maximum cache size defaults to 128 entries per store, configurable via `CacheOptions{MaxSize: N}`. Eviction policy: LRU.
- **REQ-CACHE-04:** Cached responses are not returned when `temperature > 0` unless `CachingMiddleware` is configured with `IgnoreTemperature: true`.
- **REQ-CACHE-05:** Every cache hit emits a `CacheHitEvent`; every miss a `CacheMissEvent`. Both include the fingerprint and tier.

**Level 3 — Tool schema caching (in-process, eliminates repeated serialization)**

- **REQ-CACHE-06:** The canonical `Tool` list is serialized to provider wire format once per session and reused on subsequent turns. Adding tools mid-session must **not** invalidate the serialized prefix or the provider-side prompt cache (REQ-CACHE-10). Removing a tool, or changing an existing tool's schema, does invalidate it.
- **REQ-CACHE-07:** MCP tool lists are cached per `MCPServerConnection` and refreshed only on `notifications/tools/list_changed` or an explicit `RefreshTools()`.
- **REQ-CACHE-10 (deferred tool loading):** When tools are added mid-session — an MCP server connecting, a skill activating — their definitions must be declared at the transcript position where they appeared, not prepended to the cached prompt prefix. `SplitDeferredTools(tools, history)` partitions the tool set into `Immediate` and `Deferred`:
  - A tool is deferred only if a tool result marked it as newly added (`ToolResultMessage.added_tool_names`) **and** no assistant turn used it before that marker. This is a single forward pass; later usage cannot un-defer a tool.
  - Deferred tools are emitted per API: Anthropic `defer_loading: true` with **no** `cache_control`; OpenAI Responses `additional_tools`; vendors supporting neither withhold them from the top-level `tools` array and re-declare them in a system message placed after the tool-result run.
  - Safety valve: if **every** current tool would be deferred there is no prefix to anchor references against — promote them all back to immediate and accept the cache wipe.

  Invalidating the prefix is the expensive answer to mid-session tool discovery; this is the cheap one.

**Observability**

- **REQ-CACHE-08:** `Session.CacheStats()` returns `{hits, misses, provider_cache_read_tokens, provider_cache_write_tokens, estimated_savings_usd}` aggregated across all levels for the session lifetime. Savings are computed against the anchored token accounting of REQ-GO-15, not against a re-estimate.
- **REQ-CACHE-09:** `TracingMiddleware` adds `cache.hit`, `cache.tier`, `cache.fingerprint` as span attributes on every model call span when a cache hit occurs.
- **REQ-CACHE-11:** `CacheStats()` additionally reports deferred-tool promotions and prefix invalidations, so an operator can see when a session lost its prompt cache and why.

### 6.2b Model Catalog

AgentKit embeds a model catalog. The catalog supplies the metadata no API returns and pass-through cannot synthesize: context window, pricing, reasoning support, modality support, wire API, base URL and compat profile. It is **not** an allowlist — unknown model IDs still work (REQ-CAT-03).

- **REQ-CAT-01:** The catalog is a JSON document embedded at build time via `//go:embed`, keyed by vendor id then model id, versioned separately from the SDK and overridable by the caller. Each row populates the `Model` descriptor of REQ-PROV-10. A corrupt or unparseable catalog panics at init rather than yielding a silently empty catalog.
- **REQ-CAT-02:** `ResolveModel(spec) (*Model, error)` is the single entry point; `AgentConfig.Model` and `AgentConfig.Provider` are resolved through it before any request is built. Resolution rules:
  1. A `provider/` prefix is honoured only when the segment before the **first** slash is a known provider; otherwise the whole string is matched as a model ID (OpenRouter IDs contain slashes).
  2. Matching proceeds exact-canonical → provider+ID → bare ID, each accepted only when unambiguous. A bare ID matching two providers resolves to **nothing** — the SDK errors rather than guessing.
- **REQ-CAT-03:** An unknown model id under a **known** vendor resolves by cloning that vendor's default catalog row and swapping only `ID` and `Name`, with a warning. A newly released `claude-*` or `gpt-*` id therefore works the day it ships, inheriting a sibling's `Api`, `BaseURL`, `ContextWindow`, `Cost`, `Compat` and `ThinkingLevelMap`. The clone's capability profile is a documented guess, not a fact; callers may override any inherited field. An unknown **vendor** is a configuration error. Nothing in the resolution path may reject a model ID solely because it is absent from the catalog.
- **REQ-CAT-04:** Every request clamps `max_tokens` against the context window: `available = model.ContextWindow - estimateContextTokens(messages) - safetyMargin`, then `max_tokens = min(requested, max(1, available))`, with `safetyMargin` defaulting to 4096 tokens. Models with an unknown context window receive the floor only. Providers that count input and output against one window reject a request whose `max_tokens` does not fit in what remains — a failure that first appears deep into a long session, exactly when losing the conversation is most expensive. `max_tokens` on `AgentConfig` is therefore an upper bound, not the value sent.
- **REQ-CAT-05:** When `model.Input` does not include `image`, image content blocks are replaced at send time with the placeholder text `(image omitted: model does not support images)` rather than being sent and 400ing.
- **REQ-CAT-06:** Catalog maintenance is tracked work with an explicit regeneration checklist. Two fields are request-body-affecting and must be diffed per model on every regeneration: `ThinkingLevelMap` (REQ-PROV-15) and `Cost` including the provider field on fallback entries (REQ-PROV-05.5) — dropping the latter silently bills fallback-served responses at the wrong rate.
- **REQ-CAT-07:** `Session.ResolvedModel()` exposes the resolved descriptor so callers can inspect which catalog row (or clone) was used.

### 6.2c Credentials and Authentication

API keys are the simple case. A long-running agent process outlives an OAuth access token, and gateways own the auth header rather than supplying a key.

- **REQ-AUTH-01:** Resolved auth is `ModelAuth{APIKey, Headers, BaseURL}`. Any of the three may carry the credential: a key, a set of headers, or a distinct base URL. The API-key-string-only model of the §7 sketch is insufficient and is replaced by a resolver.
- **REQ-AUTH-02:** `ProviderHeaders` is `map[string]*string` with **three** states: absent (send the provider's default), present-non-nil (send this value, empty string included), and present-nil (**deletion marker** — suppress the provider's own default header of that name). The third state is what lets a gateway turn off the upstream `Authorization` / `x-api-key` while its own key rides in a gateway-specific header; no string value can express it. Header values are shared pointers: replace the pointer to change a header, never write through it.
- **REQ-AUTH-03:** Environment resolution is a per-vendor **ordered** table, not a single `<VENDOR>_API_KEY` convention, and it consults `RequestOptions.Env` (a per-request scoped override map) before `os.Getenv`. An empty override falls through rather than masking. Anthropic resolves `ANTHROPIC_AUTH_TOKEN` (sent as `Authorization: Bearer`, **not** `x-api-key`) > `ANTHROPIC_OAUTH_TOKEN` > `ANTHROPIC_API_KEY`. Discovery and retrieval are distinct operations: a variable may participate in "is this vendor configured?" while being unusable as a plain API key.
- **REQ-AUTH-04:** Credential state has **three** values, not two: a resolved key, no credential, and *ambient* — a credential chain the SDK cannot read but the transport can (cloud instance metadata, ADC, a workload identity). Ambient must be distinguishable from "no key", or every deployment using an instance role fails a pre-flight check that a plain key would pass.
- **REQ-AUTH-05:** `CredentialStore` is an application-owned interface. `Modify(ctx, vendorID, fn)` is the **only** write path, serialized per vendor id by a reference-counted keyed lock.
- **REQ-AUTH-06:** OAuth refresh runs **inside** `Modify` with double-checked expiry against a validity floor (default 5 minutes), so a concurrent turn arriving after another has refreshed observes the new token instead of refreshing again. Without this, N concurrent turns each POST the same refresh token, the provider rotates it N times, and N−1 turns hold an invalidated token. The refresh call carries its own timeout (default 15 s) because it holds the per-vendor lock.
- **REQ-AUTH-07:** NFR-SEC-01 redaction is enforced at the `CredentialStore` boundary. No resolved credential is stored on a provider struct field where it can reach a log line or an error string unredacted.
### 6.3 Tool System — Built-in and Custom

- **REQ-TOOL-01:** A tool is **two types, not one**. The registry/loop type carries everything the loop and the prompt builder need; a projection carries the four fields the provider is allowed to see.

  ```go
  type Tool struct {                          // registry + loop
      Name, Description  string
      InputSchema        *Schema
      Handler            ToolHandler
      Label              string               // display name, never sent
      ExecutionMode      ExecutionMode        // Parallel | Sequential
      PrepareArguments   func(map[string]any) map[string]any
      PromptGuidelines   []string             // folded into the system prompt
      ConstrainedSampling *ConstrainedSampling
  }

  func (t Tool) wire() wireTool               // Name, Description, InputSchema, ConstrainedSampling
  ```

  `wire()` is the **only** path from a tool to a provider request and to the REQ-CACHE-06 schema cache. Fields added for the UI, the prompt, or the loop must not silently widen the request body — a flat single type makes every new field a wire-format change.
- **REQ-TOOL-02:** Tool schemas are a **structured, transformable value type** — `*Schema`, built with typed combinators — not `json.RawMessage`, not runtime reflection, and not code generation. `Schema` carries `Type`, `Properties`, **`PropertyOrder`**, `Required`, `Items`, `Enum`, `Const`+`HasConst` (to distinguish `const: null` from absent), `Nullable`, `AnyOf`/`OneOf`/`AllOf`, numeric and string constraints, and `Extra map[string]any` for passthrough keywords.

  ```go
  InputSchema: Object(
      Prop("pattern", String("Regex to search for")),
      Opt("path", String("Directory to search")),
  )
  ```

  The structured form is load-bearing, not ergonomic sugar: schemas are **rewritten** before they reach the wire (strict-mode conversion per REQ-TOOL-03, per-provider dialect translation, Gemini's `FunctionDeclaration` shape), used to **coerce and validate** arguments (REQ-TOOL-11), and rendered into error text. `json.RawMessage` makes every one of those a parse-then-reserialize round trip that loses property order; reflection cannot express `const: null` or a passthrough keyword; code generation puts a build step between a developer and a one-line tool. This resolves OQ-2 with an option the original question did not list.
- **REQ-TOOL-03 (constrained sampling):** `Tool.ConstrainedSampling` is a struct, not a bool: `{type: "json_schema" | "grammar", strict: "prefer" | "require"}`. Unset marshals to bare `false` on the wire.
  - For `json_schema`, the provider **probes the conversion before sending**: the schema is rewritten into the target API's strict subset — objects closed with `additionalProperties: false`, every property listed in `required`, and formerly-optional non-nullable properties widened to `{"anyOf": [<prop>, {"type": "null"}]}` so the model can still omit them.
  - The rewrite rejects schemas containing `$ref`, `$defs`, `allOf`, `oneOf`, `not`, `patternProperties`, `prefixItems`, `if`/`then`/`else` and `dependentSchemas`.
  - If the rewrite fails: `"prefer"` (the default) silently falls back to unconstrained sampling; `"require"` fails the request with the specific rejection reason.
  - Emission is additionally gated on the compat profile's `SupportsStrictTools` (REQ-PROV-12).

  A bare `strict: true` sends a schema the API rejects the moment a tool uses `$ref` or an optional field, and the failure is a 400 on the whole request — every turn carrying that tool dies, not just that tool.
- **REQ-TOOL-04 — File system tools:** The built-in file tool set is deliberately minimal. A tool earns a slot only when it has semantics `execute` cannot express — truncation, unique-match editing, mutation serialization, deterministic gitignore handling. Deletion, renaming, appending and stat are one shell word each; they ship no tool and are performed through `execute`.

  | Tool | Inputs | Output |
  |---|---|---|
  | `read_file` | `path string, offset int (default 0), limit int (optional)` | `{content: string, encoding: string}` |
  | `write_file` | `path string, content string` | `{written: true, bytes: int}` |
  | `edit_file` | `path string, edits []{old_string, new_string}` | `{edits_applied: int}` |
  | `find_files` | `pattern string, path string (default "."), file_type string (default "file"), limit int (default 1000)` | `{files []string, truncated: bool}` |
  | `list_files` | `path string, limit int (default 500)` | `{entries []string, truncated: bool}` |

- **REQ-TOOL-04a:** `edit_file` takes an **array** of edits applied in one call. Every `edits[].old_string` is matched against the **original** file content, never against the result of an earlier edit in the same call. The tool description must state this verbatim.
- **REQ-TOOL-04b:** Rejections are evaluated in this fixed order; each returns `is_error=true` and writes nothing:
  1. empty `old_string`;
  2. `old_string` not found;
  3. `old_string` occurs more than once — `Found N occurrences of the string to replace. The text must be unique. Please provide more context to make it unique.`;
  4. two matched edits overlap (sort matches by offset; reject when `prev.index + prev.length > cur.index`);
  5. the result is byte-identical to the input.
- **REQ-TOOL-04c:** A non-unique `old_string` is **never** a replace-all. Silent multi-site replacement is prohibited, and `{replaced: N}` as a success signal is prohibited: multiplicity is a rejection, not a feature. The uniqueness contract is what makes an edit reviewable — the model must supply enough context to name one site.
- **REQ-TOOL-04d:** Line endings and BOM are preserved. A leading BOM and CRLF are normalized away before matching and restored on write. A whitespace-tolerant fallback pass runs **only after exact matching fails for all edits**: Unicode NFKC, per-line trailing-whitespace trim, and folding of smart quotes, dash variants and exotic space characters to ASCII. It must overlay only the matched line blocks back onto the original so untouched lines keep their exact bytes. This pass is what makes the tool usable against files the model saw through a lossy render; note that NFKC is not in the Go standard library and its cost against REQ-GO-11 must be paid deliberately (a nested module, or a hand-rolled fold restricted to the confusable set).
- **REQ-TOOL-04e:** `list_files`, `find_files` and `search_files` are **opt-in**. When they are absent from the active tool set and `execute` is present, the assembled system prompt must carry the guideline `Use execute for file operations like ls, rg, find`.
- **REQ-TOOL-05 — Search tool:** `search_files(pattern, path, context_lines, file_glob, case_sensitive, max_matches)` returns `{matches: [{file, line, text, before, after}], truncated, files_searched}`. It is accelerated by `rg --json` where available; the `rg` call is internal to the tool and does not pass through any command policy.

  The non-`rg` path is **not** "fall back to `regexp`". Matching `rg`'s observable behaviour requires a real ignore engine, and its absence is the difference between a tool that returns the project's files and one that returns `node_modules`:
  1. **Glob dialect:** brace expansion `{a,b}` with nesting, `[!x]` classes, `**` crossing `/`, and smart-case.
  2. **Ignore sources and precedence:** the global excludes file (`git config --path --get core.excludesFile`, else `$XDG_CONFIG_HOME/git/ignore`, else `~/.config/git/ignore`), `.git/info/exclude`, and every `.gitignore` from the repository root down to the scanned directory, with deeper files overriding shallower and negation (`!`) honoured within a file.
  3. **Nested-repository boundaries:** a nested repository's ignore rules apply within it and do not leak outward.
  4. **Binary-file skipping**, and a documented answer to whether the tool requires a git repository at all.

  The two accelerated backends themselves disagree on some of these; the requirement is that the fallback matches AgentKit's *declared* semantics, and that those semantics are pinned by a parity test against whichever backend is present.
- **REQ-TOOL-06 — Shell tool:** `execute(command string, timeout_s int (optional))` runs `command` through a bash-family shell. `timeout_s` is **optional with no default**; when supplied it must be positive. `stdin` is `DEVNULL`. `cwd` defaults to the workspace root and is settable by the embedder, never by the model. A `run_command(argv []string)` variant is available for structured invocation without shell interpolation.

  Shell resolution is a fixed ladder and never consults `$SHELL`: on Unix `/bin/bash` → `bash` on `PATH` → `sh`; on Windows, Git Bash (`%ProgramFiles%\Git\bin\bash.exe`, then the x86 path, then `bash.exe` on `PATH`) → **a hard error naming the paths searched**, never a silent fall back to `cmd`. A second shell dialect ships as a **separately named tool** (`powershell`), registered on every platform with the platform check deferred to execution so the tool list — and therefore the cached prompt prefix — is platform-stable.
- **REQ-TOOL-07 — HTTP tool:** `fetch_url(url, method, headers, body, timeout_s)` with `SSRFGuardTransport` (blocks private/loopback/link-local IPs at DNS and TCP time), HTTPS-only by default, 512 KB response cap, 5-hop redirect limit with per-hop SSRF re-validation, optional HTML-to-text extraction. It is **not** in the default tool set; it is reachable only by naming it in `ToolPolicy.ToolNames` (REQ-TOOL-10).
- **REQ-TOOL-08 — Output envelope:** All built-in tools return a `ToolResult`:

  ```go
  type ToolResult struct {
      OK        bool            `json:"ok"`
      Data      map[string]any  `json:"data"`
      Error     string          `json:"error,omitempty"`
      Detail    string          `json:"detail,omitempty"`
      Terminate bool            `json:"-"`
      Metadata  *ToolMetadata   `json:"metadata,omitempty"`
  }
  ```

  `ToLLMMap()` strips metadata. Providers receive only the payload.
- **REQ-TOOL-09 — Size limits and truncation:** Two limits compose on every result — a line/entry limit and a byte limit. The result records which one fired via `metadata.truncated_by = "lines" | "bytes"`.

  | Tool | Line / entry limit | Byte limit | Direction |
  |---|---|---|---|
  | `read_file` | 2000 lines | 50 KB | head |
  | `execute` (combined stdout+stderr) | — | 50 KB | **tail** |
  | `search_files` | 100 matches, 500 chars per line | 50 KB | head |
  | `find_files` | 1000 results | 50 KB | head |
  | `list_files` | 500 entries | 50 KB | head |
  | MCP tool result | — | 50 KB | head |

- **REQ-TOOL-09a:** `execute` truncates from the **tail**. A failing build puts its error at the end of the log; head-truncation preserves the banner and discards the failure.
- **REQ-TOOL-09b:** `metadata.truncated=true` alone is insufficient. Every truncated result must carry a marker line naming the exact next call that retrieves the remainder — e.g. `[Showing lines 1-2000 of 8000. Use offset=2001 to continue.]`, `[1000 results limit reached. Use limit=2000 for more, or refine pattern]`. A truncation the model cannot act on costs a turn.
- **REQ-TOOL-09c:** A single line exceeding the byte limit gets its own marker naming a shell workaround: `[Line 3 is 1.2MB, exceeds 50.0KB limit. Use execute: sed -n '3p' <path> | head -c 51200]`.
- **REQ-TOOL-09d:** `execute` output exceeding the cap is additionally streamed to a temp file whose absolute path appears in the marker.
- **REQ-TOOL-10 (tool exposure policy):** Tool availability for a run is a resolution policy over the registry, not just the additive `RegisterTool` list. `AgentConfig.ToolPolicy` carries five fields, resolved in order:

  | Field | Type | Effect |
  |---|---|---|
  | `Tools` | `[]Tool` | When non-nil, used verbatim; bypasses all name-based selection below. |
  | `ToolNames` | `[]string` | Allowlist of tool names. Nil means the default built-in set. |
  | `ExcludeTools` | `[]string` | Denylist, applied **after** the allowlist. |
  | `NoTools` | `"" \| "builtin" \| "all"` | `"builtin"` suppresses the built-in set; `"all"` sets an empty allowlist. |
  | `CustomTools` | `[]Tool` | Caller-supplied tools, appended; a custom tool overrides a built-in of the same name. |

  The policy applies **uniformly to built-in and caller-supplied tools**. The non-obvious consequences are normative and each requires a test: `NoTools = "all"` disables custom tools too; `NoTools = "builtin"` leaves the allowlist unset so custom tools survive; a `ToolNames` allowlist constrains custom tools; `ExcludeTools` applies to custom tools. This is what REQ-MULTI-05's per-agent "tool allowlist" resolves to, and it is what lets a subagent be given read-and-search-only access per delegation without rebuilding the tool set by hand.
- **REQ-TOOL-11 (argument preparation and normalization):** Tool dispatch runs a fixed pipeline before the handler, entirely inside the panic-recover boundary of REQ-LOOP-03:
  1. `Tool.PrepareArguments` — optional, per-tool, runs **first**, returning a copy rather than mutating. It repairs shapes the schema would reject. `edit_file` ships one handling the three observed model failures: `edits` delivered as a JSON string, a bare `{old_string, new_string}` object instead of a one-element array, and legacy top-level `old_string`/`new_string` keys.
  2. Deep-copy the arguments, then **delete explicit `null`s for optional properties**. Constrained sampling forces the model to emit every declared property, so optional fields arrive as explicit nulls; treating them as present is a validation failure on well-formed output.
  3. Coerce primitive types (string→number, string→bool, number→string) against the declared schema.
  4. Validate against the schema. A validation failure produces a `tool_result` with `is_error=true` whose text re-serializes **the model's own arguments in the order it wrote them**, so the error is self-correcting.
  5. Invoke the `BeforeToolCall` interceptor (REQ-SEC-03), then the handler.
- **REQ-TOOL-12 (argument byte fidelity):** A tool call's argument **key order must be preserved verbatim** from the model's stream, through session storage, back into every subsequent request. `ToolUseBlock` therefore carries both `Input json.RawMessage` (bytes as received) and `InputOrder OrderedObject` — a `[]{Key, Value}` slice whose `MarshalJSON` writes pairs in slice order and recurses through nested objects and objects inside arrays. Both are produced from a single decode pass so the ordered form is checkable against the decoded map.
  1. Providers replay tool calls from history using the ordered form, never a re-serialized `map[string]any`. Go's `encoding/json` sorts map keys unconditionally, so the default behaviour is wrong on every provider.
  2. This is load-bearing on OpenAI Chat Completions and Responses, which carry `arguments` as a JSON **string**: replaying a prior call in a different key order conditions the model on literally different text and shifts the prompt-cache prefix, silently invalidating provider-side caching for the rest of the session.
  3. Validation error text returned to the model must echo the model's own key order.
- **REQ-TOOL-13 (batch termination):** A tool signals that the run should end after the current batch by setting `ToolResult.Terminate` — the mechanism behind `finish`, `submit_answer`, `exit_plan_mode` and permission-denial tools.
  1. The batch terminates **only when every finalized result in it sets `Terminate`** — an AND, not an OR. An empty batch never terminates.
  2. A call **blocked** by a `BeforeToolCall` interceptor casts the same vote: `BlockResult.Terminate` is honoured only when `Block` is set. This is what lets a permission denial end the run instead of looping the model into retrying.
  3. An `AfterToolCall` interceptor may override the vote in either direction; its `Terminate` field is therefore `*bool`, where `nil` means "no opinion".
  4. Termination takes effect **after** all tool results are appended to history and `TurnEndEvent` is emitted. The run ends without a further provider call.

  OR semantics are the obvious-but-wrong default: the model emits N parallel calls at once, and if one `finish` unilaterally ended the batch the other N−1 results would be computed, written to history, and never shown to the model.
- **REQ-TOOL-14 (image normalization at the history boundary):** Every `ImageBlock` entering conversation history from a tool result is re-processed, regardless of which tool produced it.
  1. Normalization runs at the point results are appended to history — **not** inside individual tools — so MCP-bridged, custom and screenshot tools are all covered.
  2. It runs **after** any user-supplied post-tool hook, so hook-injected images are normalized too.
  3. Images are downscaled to at most 2000×2000 and at most 4.5 MB of base64, stepping encoder quality down a fixed ladder. The cap is sized against the provider's inline limit with explicit headroom; an image at exactly the provider limit is rejected once and then poisons every later request.
  4. Base64 decoding of tool-supplied data must be **lenient** — whitespace-wrapped and base64url payloads accepted.
  5. A normalization failure keeps the original block rather than deleting the tool's output.
  6. `read_file` detects image content by magic bytes and returns a text note plus an `ImageBlock`. Formats providers reject — CMYK JPEG, animated PNG, non-`IHDR` PNG — are refused at the tool with a clear error rather than forwarded.
- **REQ-TOOL-15 (bounded output accumulation):** The caps in REQ-TOOL-09 and REQ-SEC-02 are enforced by a rolling accumulator, not by buffering the full stream and truncating at the end. Buffering everything is quadratic in output size and out-of-memories on precisely the runaway command the cap exists to contain.
  1. Retain a bounded head and tail, each at most the configured cap, discarding the middle as bytes arrive. Peak memory is bounded by roughly `2 × cap` regardless of how much the subprocess writes.
  2. Lazily open a spill file on first overflow and stream the complete output to it, so the full output stays available to the caller and the audit trail while only the bounded window reaches the model.
  3. Applies to `execute`, `run_command`, `search_files`, and MCP tool results.
- **REQ-TOOL-16 (tool choice):** `AgentConfig.ToolChoice` is a provider-neutral tri-state: `""` (absent) | `"auto"` | `"none"`. Absent is not `auto`: a provider **must not invent a selection** when the field is empty. An explicit choice is forwarded even when the request carries no tools — the compaction request shape is "summarize this conversation" with `tool_choice: "none"` and an empty tool list, and without it a tool-free turn cannot be reliably forced. Wire mapping: Anthropic `{"type": "none"}`; OpenAI `"none"`; Google `functionCallingConfig.mode: "NONE"`.
- **REQ-TOOL-17 (process control for subprocess tools):** Safety for `execute` comes from process control, not from a wall-clock default:
  1. Every command runs in its own process group (`SysProcAttr.Setpgid` on Unix).
  2. Timeout **and** `context.Context` cancellation both terminate the entire process group (`kill(-pid, SIGKILL)`), falling back to the direct child only if the group is gone. On Windows, `taskkill /F /T /PID` resolved by **absolute path** (`%SystemRoot%\System32\taskkill.exe`) so tree termination does not depend on `PATH`.
  3. `cmd.WaitDelay` is set as a backstop for a descendant still holding the output pipe.
  4. stdout and stderr are captured on a **single shared pipe** so they interleave in true write order. Separate per-stream captures are prohibited.
  5. After the child exits, output is drained on a **re-arming** idle timer, not a fixed post-exit deadline — a detached descendant can keep writing past parent exit.
  6. Outcome classification is a pure, separately unit-tested function whose precedence is part of the contract: **abort > timeout > exit status**.
  7. A regression test must background a grandchild that writes a marker file after the parent is killed, cancel the parent early, and fail if the marker appears.
### 6.4 Multi-Agent and Subagent Support

- **REQ-MULTI-01:** Delegation is implemented as tool use. `SubagentTool` wraps an `Agent` instance; its handler calls `child_agent.Run(ctx, prompt)` and returns the text result as a `tool_result` string.
- **REQ-MULTI-02:** Child agents always start with fresh, empty history. Sharing parent history with a child is prohibited — it creates prompt injection risk and inflates context.
- **REQ-MULTI-03:** Budget propagation: before delegating, the parent passes a budget slice to the child as an **explicit config field** (`child.StopPolicy = StopOverBudget(parent.Remaining() * fraction)`), never as a `context.WithValue` payload — see the context convention in §5. The child enforces its own budget independently. Persona C's stated need for "`BudgetTracker` propagation via `context.Context` values" is met by explicit propagation instead; a budget smuggled through `ctx` is silently absent whenever a caller passes a bare `context.Background()`.
- **REQ-MULTI-04:** Parallel delegation is safe by construction (each child is an independent value with its own transcript). `errgroup` is the right tool **here** — a failed child should cancel its siblings — and is precisely the wrong tool for intra-batch tool execution, where every call must produce a result (REQ-GO-04). The two must not share a mechanism.
- **REQ-MULTI-05:** Agent definitions (name, description, system prompt, tool policy, model) are registerable by name so the parent model can invoke a named specialist by name as a tool call. The "tool allowlist" resolves to the `ToolPolicy` of REQ-TOOL-10, which applies uniformly to built-in and caller-supplied tools — a specialist can therefore be scoped to read-and-search-only per delegation without rebuilding the tool set by hand.

### 6.5 Skills System

- **REQ-SKILL-01:** A skill is a packaged, reusable agent behavior unit combining system prompt additions, tool set overrides, trigger conditions, and metadata.
- **REQ-SKILL-02:** Skill directory layout: `skill.toml` (manifest), `prompt.md` (system prompt additions), optional Go plugin module, `README.md` (optional).
- **REQ-SKILL-03:** Manifest fields: `name`, `version`, `description` (required), `author`, `sdk_min_version`, `archetypes`, `disable_model_invocation` (bool), `[skill.tools]` module+factory, `[skill.security]` allowlist_extend, `[skill.session]` max_turns_add, `[skill.subagent]` archetype+mode+prompt_template+result_key+on_failure. The `injection`, `keywords`, `prompt_file` and `prompt_position` fields are **removed** — see REQ-SKILL-06.
- **REQ-SKILL-04:** Discovery searches three locations: SDK built-in (`agentkit/_skills/`), user global (`~/.nightshift/skills/`), and project-local (`<cwd>/.nightshift/skills/`). SDK built-in and user-global skills are trusted sources. **Project-local skills are scanned only when the embedder has explicitly established project trust** (REQ-SKILL-12); when trust is absent they are not loaded, not listed and not named in the system prompt. On name collision among loaded skills, precedence is user-global > project-local > SDK built-in; project-local never overrides a user's own skill.
- **REQ-SKILL-05:** `SkillRegistry.LoadForSession(archetype, taskPrompt, config)` returns the ordered list of skills to inject, applying the archetype filter and the trust gate.
- **REQ-SKILL-06 (progressive disclosure):** Skills are injected as **metadata only**. The assembled system prompt carries, per skill, exactly `name`, `description`, and the **absolute path** to its prompt file, inside an `<available_skills>` block prefixed with an instruction to load the file when the task matches the description. A skill's body is never in the system prompt; the model pays its tokens only when it decides the skill applies.
  1. Cost is N × ~3 lines regardless of how large the skills are. `prompt_position` (`pre`/`post`/`replace_section`) is removed: `replace_section` would let a skill delete part of the host's system prompt, which no discovered content may do.
  2. The block names whichever file-reading tool is **actually active** — `read_file` when present, otherwise `execute` — and is **omitted entirely** when neither is. The prompt must never instruct the model to use a tool it does not have.
  3. The block must carry the resolution rule for a skill's own references: `When a skill file references a relative path, resolve it against the skill directory (the parent of the skill's prompt file) and use that absolute path in tool commands.`
  4. A skill with `disable_model_invocation = true` is filtered out of the block entirely.
  5. Every interpolated field (`name`, `description`, path) is XML-escaped before it enters the prompt.
- **REQ-SKILL-07:** Tool registrations from skills are merged with the session's base tool list. Name collisions raise `SkillConflictError` unless one skill declares `overrides`. A skill activating mid-session marks its tools per REQ-CACHE-10 rather than invalidating the cached prefix.
- **REQ-SKILL-08:** Skill subagent spawning is declarative only. The session runner spawns the subagent session, waits for the result, and injects the result text into the main session's system prompt. Skill plugin code may not directly invoke internal session or backend packages.
- **REQ-SKILL-09:** Skill plugin code is restricted at load time: plugins may not import internal agentkit packages (`agentkit/internal/...`), any LLM client library, or model API packages. Violations reject the skill at load time.
- **REQ-SKILL-10:** Skill manifests are parsed **leniently**: unknown keys produce a warning diagnostic and the skill still loads. Only a missing `description` rejects a skill, since without it the skill cannot be offered to the model at all. A manifest is authored content whose consumer is a language model; rejecting a whole skill over a typo'd key is a worse failure than ignoring the key. Strict unknown-key rejection applies to **wire boundaries only** (REQ-SEC-12).
- **REQ-SKILL-11:** Every loaded skill name is recorded in the session's audit event.
- **REQ-SKILL-12 (project trust gate):** `AgentConfig.TrustProject bool` defaults to the zero value, so **every embedder is untrusted by construction** and a host that has established trust must say so.
  1. The threat is concrete: a skill's `name` and `description` are authored into the system prompt together with an instruction to read the file when the task matches. A hostile repository puts attacker-authored text in front of the model by being the cwd — `git clone`, `cd`, run the agent.
  2. A headless embedder with no way to ask a human must resolve trust to `false`. Silence is not consent.
  3. **Fail closed on ambiguous roots:** if the user's home directory cannot be resolved — ordinary in containers, CI, systemd units and cron — the user-global skill and config directories are **skipped entirely**. They must never fall back to a relative path: a relative `.nightshift/...` resolves against the process working directory, letting a hostile repository impersonate the user's own global config. Every caller must treat "no global directory" as a valid state.
  4. **Standing rule:** any change that adds a new project-local read — anything under `<cwd>/.nightshift/**`, an ancestor context file, or any future discovery source — ships with the trust gate in the same change. A new reader shipping ungated is a defect, not a scope question.
  5. Tests must pin both arms, including the untrusted default through the real constructor and the assertion that an untrusted project skill is absent from the assembled system prompt.
- **REQ-SKILL-13 (skill prompt authoring contract):** §6.5 specifies the skill container; the SDK must additionally document what belongs in `prompt.md`, and the built-in skills ship as the worked examples of it. A conforming skill prompt contains:
  1. **A charter** — what the skill is responsible for and, explicitly, what it is *not*. Without a stated non-goal, two skills loaded into one session both do the easy half of a job and neither does the hard half.
  2. **Hard prohibitions**, each recorded together with the incident that produced it. A rule with no stated cause reads as boilerplate and is reasoned away by the next model that encounters it.
  3. **Mechanical gates** — commands whose exit status is a binary result independent of model judgement — wherever the domain admits any.
  4. **A fixed output contract** — the exact shape the skill's output takes, so a caller can parse it.
  5. **Anti-false-positive carve-outs** — the patterns that look like violations and are deliberate, named explicitly.
  6. **Pointers, not copies.** A skill references the authoritative source rather than restating it; a duplicated table drifts and then contradicts its original silently.

### 6.5a Project Context Files

A context file is repository-authored standing instruction text (house style, build commands, review rules) injected into the system prompt. It is distinct from a skill: it has no manifest, no tools, and is always on.

- **REQ-CTX-01:** Candidate filenames per directory, in order: `AGENTS.override.md`, `AGENTS.md`, `CLAUDE.md`. The **first match in a directory wins** — an override file *replaces* that directory's other candidates rather than adding to them. A candidate path that is a directory falls through to the next name.
- **REQ-CTX-02:** Loading order is user-global (`~/.nightshift/AGENTS.md`) first, then every ancestor of the working directory ordered **root → cwd**, so the most specific file is last in the prompt and therefore most recent. A leading BOM is stripped from every file.
- **REQ-CTX-03:** Context files are subject to the **same trust gate as skills** (REQ-SKILL-12). A context file is strictly more powerful than a skill's metadata — its entire body is repository-authored prose that competes with the user's own instructions for the session — so it may not be less gated.
- **REQ-CTX-04:** Both the path attribute and the file body are escaped before interpolation. A directory named `x"><foo>` or a file containing the block's own closing tag must not break out of its container.
- **REQ-CTX-05:** When a linked git worktree nested inside its main repository has its own context file, the main repository's copy of the same tracked file is skipped. Both occupy one logical repository scope, and loading both applies the same instructions twice, which measurably degrades instruction-following.
### 6.6 Plugin System

- **REQ-PLUGIN-01:** Four plugin categories: `BackendPlugin`, `ToolProviderPlugin`, `StoragePlugin`, `EventHookPlugin`.
- **REQ-PLUGIN-02:** Each category defines a Go interface. `EventHookPlugin.OnToolUse(toolName string, toolInput json.RawMessage, ctx context.Context) string` returns `"allow"`, `"block"`, or `""` (no opinion).
- **REQ-PLUGIN-03:** All `EventHookPlugin` methods have default no-op implementations via an embedded base struct.
- **REQ-PLUGIN-04:** Event hooks execute in registration order. The first hook returning `"block"` from `OnToolUse` wins. **Amended by REQ-SEC-03:** there is no longer a static command allowlist to run ahead of hooks. The embedder-supplied `BeforeToolCall` interceptor is the authorization boundary and may both widen and narrow; event hooks compose with it and may only narrow further. Nothing in the SDK may run before the interceptor in a way it cannot override.
- **REQ-PLUGIN-05:** Discovery via plugin.toml manifests in explicitly configured directories (`[plugins] paths` in config.toml).
- **REQ-PLUGIN-06:** Loading order: built-in SDK components, manifest-declared plugins (alphabetical by name), local plugins last. Name collision: later registration wins with a warning.
- **REQ-PLUGIN-07:** `[plugins] disabled` list allows opt-out by name.
- **REQ-PLUGIN-08:** Go plugin dependencies are resolved at build time via the standard Go module system. Plugin manifests declare their module path in `plugin.toml`. No runtime dependency resolution is performed; incompatible or missing plugin binaries skip the plugin with a warning.
- **REQ-PLUGIN-09:** Plugin code may not import internal agentkit packages (`agentkit/internal/...`). Violations reject the plugin at load time.
- **REQ-PLUGIN-10:** `nightshift --validate-plugins` loads all configured plugins, runs interface conformance checks, and reports violations without starting the daemon.
- **REQ-PLUGIN-11:** `PluginRegistry` is held on `AgentFoxConfig`, not a package-level global, so tests can inject mock plugins without patching global state.

### 6.7 MCP Client Support

- **REQ-MCP-CLIENT-01:** Consume MCP servers as tool providers using `github.com/mark3labs/mcp-go`.
- **REQ-MCP-CLIENT-02:** Three transports: stdio (subprocess + NDJSON), HTTP/SSE (2024-11-05 spec), streamable HTTP (2025-03-26 spec).
- **REQ-MCP-CLIENT-03:** `MCPServerConnection` wraps an mcp-go client session and provides: initialization (protocol handshake + capability negotiation), tool list caching with refresh on `notifications/tools/list_changed`, `Call(toolName string, arguments map[string]any) (map[string]any, error)`, and audit logging of every call via `afaudit`.
- **REQ-MCP-CLIENT-04:** `MCPServerPool` holds `server_name -> MCPServerConnection`, instantiated during session initialization, torn down after the session ends.
- **REQ-MCP-CLIENT-05:** MCP tools exposed through the unified tool registry with qualified names using `server_name__tool_name` convention (e.g., `github__create_issue`). Configurable per-server prefix.
- **REQ-MCP-CLIENT-06:** MCP tool names may not shadow native tool names. Collision raises `MCPNameCollisionError` at connection time.
- **REQ-MCP-CLIENT-07:** Servers configured in `config.toml` as `[[mcp.servers]]` with fields: `name`, `command`, `url`, `env` (with `${VAR}` interpolation), `tool_prefix`, `allow_sampling`, `per_session_call_limit` (default 1000), `timeout_s` (default 30.0).
- **REQ-MCP-CLIENT-08:** Sampling requests require explicit `allow_sampling = true` per server. All sampling requests are logged in the audit trail.
- **REQ-MCP-CLIENT-09:** MCP tool result content is capped at 50K characters per result with truncation and a note to the LLM.
- **REQ-MCP-CLIENT-10:** Stdio server processes spawned with a reduced environment. Credentials resolved from the secrets store at spawn time.
- **REQ-MCP-CLIENT-11:** `AllowlistPolicy` and `PermissionCallback` include MCP qualified tool names. `OnToolUse` event hooks fire for MCP tool calls with the qualified name.

### 6.8 MCP Server Support

- **REQ-MCP-SERVER-01:** Optional, off by default. Enabled via `[mcp_server] enabled = true` in `config.toml`.
- **REQ-MCP-SERVER-02:** Two serving modes: stdio (`nightshift --mcp-server`) and HTTP (`mcp_server.transport = 'http'`, `mcp_server.port`).
- **REQ-MCP-SERVER-03:** Server implementation uses mcp-go server primitives with explicit tool registration.
- **REQ-MCP-SERVER-04:** Exposed tools: `process_issue(issue_number, mode)`, `get_session_status(session_id)`, `list_active_sessions()`, `cancel_session(session_id)`.
- **REQ-MCP-SERVER-05:** Exposed resources: `nightshift://issues/{number}/triage-report`, `nightshift://sessions/{id}/audit-log`, `nightshift://config`.
- **REQ-MCP-SERVER-06:** Implements MCP 2025-03-26 protocol version.
- **REQ-MCP-SERVER-07:** HTTP mode requires API key authentication on every request. Unauthenticated requests return HTTP 401. Stdio mode relies on OS-level process isolation.

### 6.9 Go Implementation

- **REQ-GO-01:** Go 1.21+ target.
- **REQ-GO-02:** Single `Agent` type. No sync/async distinction.
- **REQ-GO-03:** Tool handler signature: `func(ctx context.Context, input json.RawMessage) (json.RawMessage, error)`.
- **REQ-GO-04:** Parallel tool execution uses one goroutine per tool handler invocation, joined by `sync.WaitGroup`, with results written by slot index into a pre-sized slice. **`errgroup` must not be used for tool batches.** `errgroup.Group.Wait` returns only the first error and `errgroup.WithContext` cancels the remaining siblings; in an agent loop a failing tool must not cancel its peers, because every call in the batch needs a result or the next request carries dangling `tool_use` blocks. `errgroup` additionally provides neither the hook serialization nor the emit ordering required by REQ-LOOP-05. Handler errors, panics (`recover()`), interceptor blocks, validation failures and aborts are all converted to a `ToolResultMessage` with `is_error=true`; no tool outcome is ever propagated to the caller as a Go `error`. (`errgroup` remains appropriate for parallel subagent delegation under REQ-MULTI-04, where each child is an independent run with its own transcript.)
- **REQ-GO-05:** `context.Context` cancellation propagates through all levels. `ctx` carries cancellation and deadlines **only**; no agent state, budget, telemetry parent or credential travels on it (see §5, Context convention).
- **REQ-GO-06:** JSON-RPC id generation and response correlation for MCP use a goroutine-safe map protected by `sync.Mutex` or `sync.Map`.
- **REQ-GO-07:** Tool schemas are built with the typed combinators of REQ-TOOL-02. No `json.RawMessage` literals, no runtime reflection, no code-generation build step. This resolves OQ-2.
- **REQ-GO-08:** `EventStream` is an **unbounded mutex + condition-variable queue, not a Go channel**. It exposes `Push(Event)` and `End()` on the producer side and `Next() (Event, bool)`, an `Events()` iterator, and `Result() *AssistantMessage` on the consumer side.
  - **`Push` never blocks.** The producer is a paid model call holding an HTTP connection open; blocking it on a slow consumer stalls the SSE body read, trips the provider's stream idle timeout, and kills the request. The failure is a lost turn, not a slow one. A slow or stalled consumer costs memory and nothing else.
  - **Events are never dropped and never reordered.** Deltas are not independent samples: dropping one corrupts text reconstruction with no way for the consumer to detect the gap.
  - **The terminal event both ends the stream and captures the result.** A caller that never reads a single event may still call `Result()` and receive the complete `AssistantMessage`; the final result is decoupled from event consumption, which is what makes abandoning a stream safe.
  - Pushes after the stream is done are dropped silently. Cancellation is `context.Context`, propagated to the in-flight HTTP request — not a `Close()` on the stream. There is no `StreamOptions.BufferSize`.
  - The memory risk is bounded in practice by `max_tokens`: the worst case is one model response's worth of deltas held in a slice. Callers that must bound it further may set `StreamOptions.MaxPendingBytes`, which drops the **consumer** (closing its view with `ErrStreamOverrun`) rather than dropping events, and lets the run complete normally with `RunResult` still available.
- **REQ-GO-09:** Typed sentinel errors, all comparable with `errors.Is`: `ErrMaxTurns`, `ErrBudgetExceeded`, `ErrToolRejected`, `ErrRefusal`, `ErrBusy` (a conflicting operation was attempted while a turn was in flight — REQ-LOOP-15; the caller may retry, it is never queued), `ErrAborted` (the turn was stopped by `Agent.Abort()`, distinguishable from `context.Canceled`, which signals the caller's own `ctx`), and `ErrStreamOverrun`.
- **REQ-GO-10:** The Go MCP client uses `github.com/mark3labs/mcp-go` (MIT licensed), in the nested module of REQ-GO-11.
- **REQ-GO-11:** The root module (`github.com/agentfox/agentkit-go`) requires **nothing outside the Go standard library**. Build tags and sub-packages do **not** satisfy this: a build-tagged import still appears in `go.mod`, `go.sum`, `go list -m all`, and every downstream SBOM and vulnerability scan, and a sub-package's imports are in the root module graph unconditionally. The only mechanism in Go that confines a dependency to opt-in consumers is a **nested module**. Therefore:
  1. Any provider, plugin, or transport that needs a third-party module lives in a nested module (`providers/<name>/`, `mcp/`, `plugins/<name>/`) with its own `go.mod` and its own tag series. `go list ./...` in the root does not descend into it, so the root stays clean by construction.
  2. A nested module `require`s a **tagged** release of the root module. A `replace` directive is prohibited in any published nested module — `replace` is ignored for downstream consumers, so a `replace`-based submodule is unimportable. `replace` is permitted only in modules nobody imports (an internal test harness).
  3. A nested module attaches only through an already-public seam (`ProviderRegistry`, `PluginRegistry`). Admitting one must require no change to root-module code.
  4. `mcp-go` is third-party and therefore lives in `mcp/`, not the root. Consumers who do not use MCP never resolve it. Persona B's stated budget is met by requiring exactly one nested module.
  5. Each nested module carries its own release ritual: built, vetted and tested in its own right by the release gate, and tagged after the root. A nested module is invisible to `go build ./...` at the root and will otherwise rot silently across a root interface change.
- **REQ-GO-12:** Four compaction strategies: `NoCompaction`, `TurnWindowCompaction` (max turns), `TokenWindowCompaction` (max tokens), `SummarizationCompaction` (model, threshold tokens). Compaction is a **context transform applied inside the loop** — `AgentConfig.TransformContext func(ctx, []Message) []Message`, invoked immediately before canonical→wire conversion on every model call — **not** a `complete()` middleware. Four rules are normative:
  1. **View, not mutation.** Compaction produces the message list *sent on this request*. It never rewrites the stored `ConversationHistory`. The append-only transcript stays complete, so the UI can scroll back, the session log is lossless, and a later run against a larger context window can be given the full history. Compaction state is `{prefix_len, summary}` where `prefix_len` indexes the original, only-ever-appended list.
  2. **Permanent once applied.** A summary checkpoint, once created, is **always** re-applied on every subsequent request. The threshold check decides only whether to *extend* the checkpoint by summarizing a longer prefix; when extending, the previous summary is re-sent to the summarizer inside `<previous-summary>` tags under a distinct prompt. Re-evaluating the threshold from scratch each turn **oscillates**: the compacted request reports small usage → the next check passes → full history returns → the check fails again. Reverting to full history also invalidates every provider-side cache prefix and re-sends content already paid to summarize.
  3. **The summarization call is off the middleware path.** It issues out-of-band with its own system prompt, provider caching disabled, its own session id, and `max_tokens = min(0.8 × reserve_tokens, model.max_tokens)`. It must not re-enter `BudgetMiddleware`, the retry layers' turn accounting, or the Level 2 dedup cache as though it were a conversational turn.
  4. **Cut points are role-constrained** — see REQ-GO-14.
- **REQ-GO-13 (the dependency policy is executable):** The dependency budget of REQ-GO-11 ships as a test in the module (`internal/policy/deps_test.go`), not as prose. The package carries no non-test source and nothing imports it. The test must:
  1. Enumerate the whole transitive build graph via `go list -deps -f '{{.ImportPath}}|{{with .Module}}{{.Path}}{{end}}|{{len .CgoFiles}}' ./...` and fail on any module outside the allowlist.
  2. Hold the allowlist as `map[modulePath]reason` — every entry states in prose why that module is allowed. Adding a dependency is an edit to this map, and that edit is the review gate.
  3. Fail on any non-stdlib package with `CgoFiles > 0`, running `go list` with **`CGO_ENABLED=1`** forced into the child environment. This is load-bearing, not incidental: with cgo disabled the toolchain excludes cgo files by build constraint, so a cgo-requiring dependency reports zero `CgoFiles` and the check passes while the dependency is present. Some drivers additionally ship a build-tagged stub, so even `CGO_ENABLED=0 go build ./...` succeeds and produces a binary that fails only at run time. A cgo-off gate cannot see the thing it claims to check. Cgo-freedom, not module count, is the property that determines whether the SDK cross-compiles.
  4. Name the escalation procedure in the failure message — which module, what it buys, whether hand-rolling is credible, and where to record the ruling.

  Each nested module carries its own copy with its own allowlist. No CI configuration is required for this policy to bind.
- **REQ-GO-14 (compaction cut point is role-constrained):** An index is a valid cut point iff its message role is not `tool_result`. A kept run may never begin on a `tool_result` whose originating `tool_use` was summarized away — the provider rejects it. The algorithm walks backwards from the newest message accumulating estimated tokens until `keep_recent_tokens` is reached, then **snaps forward** to the first valid cut point at or after the crossing index, so a boundary tool result falls into the summarized portion. Snapping backwards is a defect. If the cut does not land on a `user` message the turn is split: the prefix of that turn, from the nearest preceding `user` message, is summarized separately under a distinct prompt at half the token budget, and the two summaries are joined with a fixed separator. `TurnWindowCompaction` and `TokenWindowCompaction` are both subject to this rule; naive token- or turn-count truncation violates it by construction.
- **REQ-GO-15 (token accounting is anchored, not estimated):** There is no tokenizer. The context-size estimate that drives compaction and the `max_tokens` clamp is **anchored on provider-reported usage**, not recomputed by walking the transcript. Scan backwards for the newest assistant message that (a) has `stop_reason` other than `aborted` or `error`, (b) reports non-zero context tokens, and (c) has not been invalidated by a later-inserted prefix message such as a compaction summary; take its reported usage as the base and add a `chars/4`-class estimate only for messages after it. Context tokens are `usage.total_tokens` when non-zero, else `input + output + cache_read + cache_write`. All three skip rules are mandatory: an aborted turn, an all-zero-usage response, and a stale anchor each look like valid anchors and each silently resets the estimate to near zero. When no valid anchor exists, fall back to the pure heuristic over the full transcript. Inline images are estimated at a flat per-image character cost rather than by encoded length.
- **REQ-GO-16 (summarization result validation):** A summarization response is a **failure** when its `stop_reason` is `error` or `max_tokens`, or when it contains any `tool_use` block, regardless of the text alongside it. `max_tokens` is a failure because the summary is truncated mid-thought, and compaction is permanent — a truncated summary looks like a success and then poisons every subsequent turn for the life of the session. An **aborted** summarization is not a failure: the text produced so far is kept. The tool-call guard is applied to the response, not enforced by setting `tool_choice: "none"` on the request. On failure, compaction returns the current view unchanged: it never checkpoints the bad summary and never aborts the session.
### 6.10 Observability and Hooks

- **REQ-OBS-01:** Every model call is wrapped in a tracing span (when `TracingMiddleware` is active) carrying attributes: `model`, `provider`, `turn_count`, `input_tokens`, `output_tokens`, `cost_usd`, `stop_reason`. The span contract is defined by **two AgentKit interfaces** — `telemetry.Context{StartSpan}` and `telemetry.Span{AddEvent, SetAttributes, SetStatus, End}` — plus a shared, fieldless no-op default used whenever no consumer is wired in, so an untraced run neither inspects nor retains what it is handed. `StartSpan` is callback-scoped (`StartSpan(opts, func(span Span) error) error`) and `Span` embeds `Context`, so parenting travels through the `Span` value and **not** on `context.Context`: cancellation belongs to the work the callback closes over, not to the tracing of it. The OpenTelemetry binding is written by the host application or shipped as a nested module (REQ-GO-11) — naming `go.opentelemetry.io/otel` as a root-module dependency would violate REQ-GO-11, and the two requirements as previously written could not both hold. Any in-memory span recorder AgentKit ships is append-only and unbounded by design: it is for tests, and must be documented as unsuitable for a long-lived process.
- **REQ-OBS-02:** Every tool call emits start and end spans with `tool_name`, `tool_use_id`, `is_error`, `elapsed_ms`.
- **REQ-OBS-03:** Session start and end fire `EventHookPlugin.OnSessionStart` and `OnSessionEnd` for all registered hooks.
- **REQ-OBS-04:** Every loaded skill name is recorded in the session audit event.
- **REQ-OBS-05:** Every MCP tool call is logged in the audit trail with `server_name`, `tool_name`, arguments hash, and `is_error` status.
- **REQ-OBS-06:** Streaming event taxonomy. Text and thinking are **bracketed**, not bare deltas, so a consumer can close a block without inferring the boundary from the next event's type:

  | Event | Payload |
  |---|---|
  | `AgentStartEvent` / `AgentDoneEvent` | run boundaries; `AgentDoneEvent` carries `RunResult` and the session-aggregate `Usage` |
  | `TurnStartEvent` / `TurnEndEvent` | `TurnEndEvent` carries the completed `AssistantMessage`, its `[]ToolResultMessage` (always non-nil; `[]` for a no-tool turn), and the turn's final `Usage` including cache tokens |
  | `MessageStartEvent` / `MessageUpdateEvent` / `MessageEndEvent` | a whole-message snapshot of the assistant message as of this event |
  | `TextStartEvent` / `TextDeltaEvent` / `TextEndEvent` | `block_index`, `delta` |
  | `ThinkingStartEvent` / `ThinkingDeltaEvent` / `ThinkingEndEvent` | `block_index`, `delta`, `signature` on end |
  | `ToolCallStartEvent` / `ToolInputDeltaEvent` / `ToolCallEndEvent` | `tool_use_id`, `name`, partial arguments |
  | `ToolExecutionStartEvent` / `ToolExecutionUpdateEvent` / `ToolExecutionEndEvent` | `tool_use_id`, `name`, `result`, `is_error`, `elapsed_ms` |
  | `ToolResultEvent` | the finalized `ToolResultMessage` |
  | `ErrorEvent` | terminal provider/transport error |

- **REQ-OBS-06a (two classes, and which one is truth):** The taxonomy splits into **incremental** events (`TextDeltaEvent`, `ThinkingDeltaEvent`, `ToolInputDeltaEvent`) and **authoritative** events (everything else). Incremental events are an optimization: they may be coalesced and may be absent entirely — a non-streaming provider emits none — and they are never the source of truth. Authoritative events are complete and final for the item they name, exactly one per item, always emitted. A consumer that receives an item's authoritative event **discards the deltas it accumulated for that item** and takes the authoritative payload whole; applying both double-counts.
- **REQ-OBS-06b (snapshot ownership):** Every event carrying a partial message must carry an **independent deep copy** taken at push time, never a pointer to one live, mutating message shared by all events. A shared live partial means any consumer that buffers events — a test, a session recorder, a bridge, a slow UI — reads the *final* message out of every historical event and cannot reconstruct the timeline. This is what makes the unbounded queue of REQ-GO-08 useful rather than merely safe. `MessageUpdateEvent` lets a UI re-render from a snapshot instead of maintaining its own delta accumulator; both forms are provided because a token renderer wants the delta and a diff-based renderer wants the snapshot.
- **REQ-OBS-06c:** Event structs serialize to a discriminated JSON union — a `type` field plus exactly the fields that variant carries. No shared optional-everything envelope.
- **REQ-OBS-07:** Turn hooks (`OnTurnStart`, `OnTurnEnd`, `OnAgentDone`, `OnError`) are callback registration points separate from middleware.
- **REQ-OBS-08 (ordering and finalization):** The taxonomy is ordered, not merely enumerated.
  1. For any single item — a text block, a thinking block, a tool call, a turn — every incremental event for that item is delivered before that item's authoritative event, on the same channel, in emission order. Events for different items may interleave; an item's deltas are never reordered against its own authoritative event. Implementations must not dispatch the two classes on different goroutines without re-establishing this order: a delta that overtakes an authoritative event that already contains it is applied twice, and the double-application is invisible to any test that emits only one class.
  2. When a single streaming chunk carries more than one block type, deltas are emitted in the order the blocks appear in the response, not in a fixed per-handler order. Processing thinking deltas ahead of text deltas flips block order on every chunk that carries both.
  3. Block-end and `TurnEndEvent` are emitted **after** the stream has fully ended, once each, in block order — never from inside the per-chunk handler. Emitting from the chunk handler produces duplicate end events and a `TurnEndEvent` that lacks usage, because usage arrives with the terminal chunk.
  4. Metadata accumulated for a tool call must survive `ToolCallEndEvent`: the end event augments the accumulated call and must not replace state that later events read.
- **REQ-OBS-09 (resync, not replay):** AgentKit does not replay missed events. A consumer that detects a gap — `ErrStreamOverrun`, or a stream it reattached to — must discard all accumulated state and rebuild from `Agent.Snapshot()` (REQ-LIFE-02). There is no partial recovery.
### 6.11 Security and Sandboxing

- **REQ-SEC-01 (path containment):** All built-in file system tools call `checkPath()` before any I/O. Paths resolving outside the workspace root (via `..`, symlinks) are rejected with `error='path_not_allowed'`. Path **normalization is itself an attack surface**: `~` expansion, `file://` unwrapping and leading-sigil stripping all happen *before* containment and must therefore be applied before, not after, canonicalization. This requirement constrains the built-in file tools only; it does **not** constrain `execute`, and the PRD must state that plainly rather than leave readers to infer a boundary that is not there.
- **REQ-SEC-02 (output size limits):** Per REQ-TOOL-09, enforced by the bounded accumulator of REQ-TOOL-15.
- **REQ-SEC-03 (tool call interception — replaces the command allowlist):** The security boundary for `execute` is a per-call interceptor supplied by the embedder, not a static command allowlist. A static allowlist is not a boundary: permitting `git`, `go` or `npm` is escapable through their own configuration and subprocess surfaces, and an allowlist narrow enough to be safe is too narrow to run a build. `AgentConfig.BeforeToolCall func(ctx, BeforeToolCallContext) BeforeToolCallDecision` is the single enforcement point for permission gates, path protection and command policy.
  1. It fires for **every** tool call — built-in, custom, and MCP-qualified — with the tool name, the validated and coerced arguments, and the assistant message that produced the call.
  2. It runs **after** `PrepareArguments` and after schema validation (REQ-TOOL-11), so a policy reading `args["path"]` cannot be fooled by a type the schema would have rejected.
  3. `Block` produces a `tool_result` with `is_error=true` carrying the policy's reason; the loop continues. `Terminate` additionally ends the run per REQ-TOOL-13.
  4. In a parallel batch, all calls are prepared, validated and passed through the interceptor **sequentially** before any handler runs, so the policy observes a deterministic order (REQ-LOOP-05).
  5. The interceptor may both widen and narrow. Nothing in the SDK may run before it in a way it cannot override; REQ-PLUGIN-04's "allowlist runs before hooks" ordering is amended accordingly.

  See OQ-8 for the unresolved question of what a non-interactive embedder supplies here.
- **REQ-SEC-04 (shell operators are permitted):** `execute` passes the command string to the shell unchanged. Pipes, `;`, `&&`, `$()`, backticks and variable expansion are supported by design — a coding agent cannot function without them, and a model blocked from them routes around the check by writing a shell script with `write_file` and executing that. The former `checkShellOperators` regex is removed. Where a filter is nonetheless deployed, it must declare which shell grammar it filters and refuse outright on a platform whose grammar it has no filter for: a security control that silently does not hold on a supported platform is worse than an unsupported platform.
- **REQ-SEC-05 (SSRF guard):** `fetch_url` uses `SSRFGuardTransport` validating against private/loopback/link-local/reserved IP ranges at DNS resolution time and TCP connection time.
- **REQ-SEC-06 (skill sandbox):** Skill plugin code is validated at load time for prohibited import paths. Symlink directories and symlink prompt files are rejected. Skill allowlist extensions are additive only.
- **REQ-SEC-07 (plugin sandbox):** Plugin code is validated at load time for imports of disallowed `agentkit/internal/...` paths. Missing or incompatible plugin binaries cause a graceful skip. Event hook veto is additive only. Note the honest limit: with build-time Go module linkage (REQ-PLUGIN-08) this is an import-path lint, not a sandbox, and must be documented as such.
- **REQ-SEC-08 (MCP threat mitigations):** Tool name prefixing prevents shadowing. Sampling requires explicit opt-in. Resource URI auto-fetch is disabled. Stdio server credentials are resolved from the secrets store at spawn time and the subprocess receives a **reduced environment**; the same reduced-environment rule applies to `execute` and `run_command`, which otherwise inherit the entire parent environment including every provider API key. Per-server call count limits are enforced per session.
- **REQ-SEC-09 (HTTPS enforcement):** `fetch_url` allows only `https://` by default. HTTP is opt-in via `tools.allow_http = true`.
- **REQ-SEC-10 (project-local prompt material is trust-gated):** Discovery of project-local skills and context files is gated on `AgentConfig.TrustProject`, which **defaults to false** — see REQ-SKILL-12 and REQ-CTX-03. A skill's name and description reach the system prompt, so discovering them from an untrusted working directory lets a cloned repository author part of the prompt. Because AgentKit is a headless library with no UI to prompt with, the safe default is off; establishing trust is the embedding application's responsibility and must be an affirmative act, not an inferred one.
- **REQ-SEC-11 (untrusted decode bounds):** Every decoder that reads bytes AgentKit did not produce is bounded *before* it allocates. This covers MCP stdio NDJSON from spawned subprocesses, MCP HTTP/SSE and streamable-HTTP response bodies, and inbound requests to the AgentKit MCP server. REQ-SEC-02 caps tool *output*; this caps untrusted *input*, a different and otherwise unbounded surface.

  | Bound | Default | On breach |
  |---|---|---|
  | Max message length | 16 MiB | Reject the message |
  | Max container length (array elements, object members) | 1,000,000 | Reject |
  | Max nesting depth | 64 | Reject |
  | Duplicate object keys | rejected | Reject the message |

  1. A peer-declared length is range-checked in `uint64` before it is narrowed to `int`. On a 32-bit build a declared length ≥ 2^31 would otherwise go negative, slip past the limit check, and panic on a negative slice bound.
  2. No buffer is pre-allocated to a peer-declared size. A peer announcing a 16 MiB message and sending one byte must not cost 16 MiB.
  3. Duplicate keys are rejected rather than resolved last-wins: last-wins lets an untrusted peer pick which of two values AgentKit sees.
  4. A decoder is **poisoned by its first malformed message**: the connection is torn down and surfaced per NFR-REL-03. Resynchronizing a framed stream after a parse error is not attempted, because the framing is already untrustworthy.
  5. `encoding/json` satisfies none of rules 1–4 and silently accepts duplicate keys. A bare `json.Decoder` on an untrusted stream is a conformance failure.
- **REQ-SEC-12 (strict decoding at wire boundaries):** One strict tree decoder is shared by every untrusted wire surface — MCP client responses, MCP server requests, and provider responses. It operates over a decoded `any` tree so it survives generic JSON-RPC envelopes.
  1. An unknown property is a **rejection**, not something to ignore. A peer that can smuggle extra fields past the parser reaches code paths the schema was meant to gate.
  2. Cross-language number semantics: an integral float satisfies an integer field, but only within the IEEE-754 safe-integer range; an integer satisfies a float field. A peer with a single number type cannot distinguish `1` from `1.0`, and a decoder that insists on wire-integers rejects legal messages.
  3. A `Validator` hook runs as soon as each struct is filled, for constraints the Go type shape cannot express (`minLength`, `minimum`, literal unions).
  4. Explicit `null` for an untyped field must be handled without reflection panics. A reflective setter that panics on null means any peer can crash the process by sending `null` for a tool input.
  5. This applies to **protocol payloads only**. Locally authored manifests (skills, plugins) are decoded leniently — REQ-SKILL-10.
- **REQ-SEC-13 (attribution disclosure and opt-out):** Any header AgentKit sends to a third-party provider that identifies AgentKit, the consuming application, or the session is *attribution* and is governed by this requirement.
  1. The complete per-provider set is enumerated in a dedicated documentation section. Changing that set is a documented, released change even when no other code changes.
  2. A single kill switch — `AGENTKIT_TELEMETRY=0`, or `AgentConfig.Attribution = false` — disables every attribution header. The default being on is precisely why it must be disclosed.
  3. No attribution header may carry a session identifier, workspace path, user identity, prompt text, or any other request content.
  4. Header precedence, lowest to highest: attribution defaults < provider/auth headers < `model.headers` < caller-supplied `RequestOptions.Headers`. A caller may suppress any default entirely with the REQ-AUTH-02 deletion marker.

  This supersedes the unqualified clause in REQ-PROV-02 as originally drafted ("Passes `HTTP-Referer` and `X-Title` headers per OpenRouter convention"), which specified no opt-out, no precedence and no disclosure.

### 6.12 Session Persistence and Resume

Resumability is not "serialize `[]Message`". A conversation's replayable state includes which model produced which turn, what reasoning level was active, where the compaction checkpoint sits, and which branch is live — none of which a message array carries.

- **REQ-SESS-01 (log, not snapshot):** The durable unit of a session is an **append-only JSONL event log**. Line 1 is a session header `{"type":"session","version":int,"id":string,"timestamp":string,"cwd":string}`. Every subsequent line is exactly one entry: `{"id":string,"parent_id":string,"type":string,"timestamp":string, ...}`. Entry types in v1: `message`, `model_change`, `thinking_level_change`, `compaction`, `custom_message`, `branch_summary`. One line holds exactly one JSON value and nothing else.
- **REQ-SESS-02 (resume is a fold, not a parse):** Resuming replays the active branch and folds session configuration out of it, in order: (1) provider and model ID from the last `model_change` entry, or absent one from the provenance of the last assistant message; (2) reasoning level from the last `thinking_level_change`; (3) the message list. The `Agent` is constructed **after** the fold, with the recovered model and reasoning level as construction inputs — not patched onto an already-built agent.
- **REQ-SESS-03 (config mutations are entries):** `SetModel` appends a `model_change` entry; `SetThinkingLevel` appends a `thinking_level_change` entry. A configuration change not written into the same ordered log at the moment it happens is not recoverable by REQ-SESS-02.
- **REQ-SESS-04 (compaction is an entry, not a rewrite):** A compaction appends `{type: "compaction", summary, first_kept_entry_id}`. The summarized entries stay in the file. This is what makes REQ-GO-12's "view, not mutation" durable rather than merely in-memory.
- **REQ-SESS-05 (repair on load, not refusal):** A session log is written by a process that can be killed at any instant, so the loader must tolerate damage rather than reject the file:
  1. A **truncated final line** (partial JSON) is discarded and the rest of the session loads. This is the ordinary outcome of a crash mid-write and must not be an error.
  2. An **unknown entry type** is retained verbatim and re-emitted on write. A loader that drops what it does not model silently destroys data written by a newer version.
  3. A **dangling `parent_id`** — an entry whose parent is missing — is re-parented to the last valid entry, with a diagnostic.
  4. Repairs are **reported**, never silent: the load result carries the list of repairs performed so a caller can surface or refuse them.
- **REQ-SESS-06 (transcript repair is a separate concern):** Structural repair of the *log* (REQ-SESS-05) is distinct from semantic repair of the *transcript* for a provider (REQ-PROV-11). A loaded session commonly ends mid-turn with an unanswered `tool_use`; that is a valid log and an invalid request, and it is the provider's send-time transform — not the loader — that reconciles it. Repairing at load time would corrupt the durable record; refusing to load would make every interrupted session unrecoverable.
- **REQ-SESS-07 (history is a tree):** An append-only log cannot delete, so rewind, edit-and-retry and branch navigation are expressed by **re-parenting**, not truncation. The store exposes `Branch(leafID) []Entry` (root→leaf path, the active conversation), `Leaves() []EntryID` (divergent tips), `ForkFrom(entryID)`, and an explicit null-leaf state for "before the first entry". Both branches remain in the same file; nothing is rewritten. When the caller navigates back out of a branch, a `branch_summary` entry is appended and rendered into the model context as a user message with a fixed wrapper string. Wrapper strings for `branch_summary` and `compaction` summaries are **model-visible format contract** and must be pinned byte-for-byte by golden fixtures.
- **REQ-SESS-08 (persistence errors are surfaced):** `SessionStore.Append(entry) error` returns its error. Marshal and write failures must not be discarded. If the SDK subscribes the store to loop events internally, it must expose an explicit `OnPersistError(func(error))` callback — silent failure is prohibited for an embeddable library.
- **REQ-SESS-09 (create semantics and durability):** The first flush of a new session opens with `O_CREATE|O_EXCL` and never clobbers an existing file at the session path; a collision is reported through REQ-SESS-08, not swallowed. The store must document its durability level explicitly: buffered, `fsync` per entry, or `fsync` per turn. Writes must not be withheld until the first assistant message — a session that crashes during turn 1 must still leave a header and the user entry on disk.

### 6.13 Session Lifecycle and Out-of-Band Control

AgentKit ships no daemon, no service and no REPL (NG1/NG3). It does define the seams a daemon, a detachable TUI, or a multi-attach service would be built against, because those seams are the part a consumer cannot add from outside: transport, framing and RPC are ordinary code any team can write, but a `Phase()` predicate and a mid-run control channel require changing the loop, which means forking.

- **REQ-LIFE-01:** `Agent.Phase() Phase` reports what the agent is doing right now — `PhaseIdle`, `PhaseCallingModel`, `PhaseExecutingTools`, `PhaseCompacting`. It must be cheap, non-blocking, and callable from any goroutine at any time, including from inside a turn hook or a tool interceptor. It must never acquire a lock held across a model call or a tool execution.
- **REQ-LIFE-02:** `Agent.Snapshot(ctx) (SessionSnapshot, error)` returns the authoritative session state — history, config, cumulative usage, phase — as a value safe to serialize and safe to read concurrently with a running turn. Every snapshot carries `Revision uint64`, monotonically increasing for the lifetime of one `Agent` value and incremented on every history mutation, and `ProducerID string`, unique per `Agent` value. **Consumers must compare `ProducerID` before `Revision`:** revisions from two producers are unordered, and a snapshot restored into a new `Agent` restarts the counter, so a strict revision-monotonicity guard is correct for a live stream and wrong at the exact moment the stream restarts. This is the resync target of REQ-OBS-09.
- **REQ-LIFE-03:** Every exported `Agent` method is safe to call from any goroutine while a turn is in flight. Conflicting operations **fail rather than queue**, and never block: `Run` or `Stream` called while a turn is active returns `ErrBusy` immediately. AgentKit never serializes caller operations on the caller's behalf — a prompt queued behind a running turn was written against a transcript the caller could see, and the decision to retry, queue or reject belongs to the caller.
- **REQ-LIFE-04:** `Agent.Abort()` cancels the in-flight turn. It takes no arguments and no `context.Context`: the run stores its own canceller at start, so a caller that does not own the `Run` goroutine — a signal handler, an RPC handler, a UI event loop — can stop the turn. `Abort` is idempotent and a no-op when idle. An aborted run leaves the terminal marker of REQ-LOOP-09.
- **REQ-LIFE-05:** Queue accessors (`Steer`, `FollowUp`, `ClearSteeringQueue`, `ClearFollowUpQueue`, `ClearAllQueues`, `HasQueuedMessages`) are non-blocking and safe to call mid-turn. Drain points are specified in REQ-LOOP-13 and REQ-LOOP-14.
- **REQ-LIFE-06:** `Agent.Idle() bool` returns `Phase() == PhaseIdle && holds == 0`, where `holds` is an operation refcount raised by `Agent.Hold() (release func())`. `Idle()` is the only safe point at which an external owner may dispose of the agent or persist its history as complete. `Hold` exists so an owner with in-flight work of its own — an attached client, an outstanding RPC — can keep the agent from being reclaimed underneath it without owning the `Run` goroutine.
- **REQ-LIFE-07 (snapshot completeness):** A snapshot taken while `Idle()` is false is a *consistent* view, not a *complete* one — the in-flight turn is not in it. A caller persisting for resume either takes the snapshot when `Idle()` is true, or accepts that the interrupted turn replays from its last completed turn boundary. This is the honest statement of what NFR-REL-04 buys.
## 7. API Design Sketches

### Go: Core Agent API

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"

    agentkit "github.com/agentfox/agentkit-go"
)

func main() {
    // Resolve the model through the catalog (REQ-CAT-02). This supplies the wire
    // API, base URL, context window, pricing and compat profile — none of which
    // the model-ID string carries.
    model, err := agentkit.ResolveModel("anthropic/claude-opus-4-5")
    if err != nil {
        log.Fatal(err)
    }

    cfg := agentkit.AgentConfig{
        Model:        model,
        MaxTokens:    8192,                              // an upper bound; clamped per REQ-CAT-04
        SystemPrompt: "You are a coding assistant.",
        StopPolicy:   agentkit.StopAny(                  // REQ-LOOP-04
            agentkit.StopAfterTurns(50),
            agentkit.StopOverBudget(2.00),
        ),
        SessionID:    "issue-4417",                      // cache key + routing affinity (§6.2a L0)
        TrustProject: false,                             // REQ-SKILL-12: untrusted by construction
    }

    agent := agentkit.NewAgent(cfg)                      // credentials resolved per REQ-AUTH-03

    agent.RegisterTool(agentkit.Tool{
        Name:        "search",
        Description: "Search files for a regex pattern",
        // Structured schema, not json.RawMessage — it must survive strict-mode
        // rewriting, per-provider dialect translation and argument coercion.
        InputSchema: agentkit.Object(
            agentkit.Prop("pattern", agentkit.String("Regex to search for")),
            agentkit.Opt("path", agentkit.String("Directory to search")),
        ),
        ExecutionMode:    agentkit.Parallel,
        PromptGuidelines: []string{"Prefer search over reading whole files."},
        Handler: func(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
            var params struct {
                Pattern string `json:"pattern"`
                Path    string `json:"path"`
            }
            if err := json.Unmarshal(input, &params); err != nil {
                return nil, err
            }
            matches, err := grepFiles(ctx, params.Pattern, params.Path)
            if err != nil {
                return nil, err // becomes tool_result(is_error=true); the loop continues
            }
            return json.Marshal(matches)
        },
    })

    ctx := context.Background()
    result, err := agent.Run(ctx, "Find all TODOs in the codebase and summarize them")
    if err != nil {
        log.Fatal(err) // ErrBusy, ErrAborted, … — never a tool failure
    }
    fmt.Printf("Result: %s\n", result.FinalText())
    fmt.Printf("Turns: %d, Cost: $%.4f\n", result.TurnCount, result.Usage.CostUSD)
}
```

### Go: Streaming API

`Stream` returns immediately; the producer never blocks on this consumer (REQ-GO-08). Text and thinking are bracketed, and every incremental event is superseded by the authoritative event for the same item (REQ-OBS-06a).

```go
stream := agent.Stream(ctx, "Refactor the auth module to use the new token interface")

for {
    event, ok := stream.Next()
    if !ok {
        break
    }
    switch e := event.(type) {
    case agentkit.TextDeltaEvent:
        fmt.Print(e.Delta)                       // incremental: an optimization, never truth
    case agentkit.TextEndEvent:
        // authoritative for this block — discard accumulated deltas for it
    case agentkit.ToolCallStartEvent:
        fmt.Printf("\n[calling %s (id: %s)]\n", e.Name, e.ToolUseID)
    case agentkit.ToolResultEvent:
        if e.Message.IsError {
            fmt.Printf("[tool error: %v]\n", e.Message.Content)
        }
    case agentkit.TurnEndEvent:
        fmt.Printf("\n[turn complete: %s, %d tokens, %d cached]\n",
            e.Message.StopReason, e.Usage.OutputTokens, e.Usage.CacheReadTokens)
    case agentkit.AgentDoneEvent:
        fmt.Printf("\nDone in %d turns, cost $%.4f\n",
            e.Result.TurnCount, e.Result.Usage.CostUSD)
    }
}

// Safe even if no event above was ever read: the terminal event carries the result.
msg := stream.Result()
_ = msg

// Cancellation is ctx, not a method on the stream. Out-of-band, from any goroutine:
//   agent.Abort()                      // REQ-LIFE-04
//   agent.Steer("actually, skip the tests for now")   // delivered next turn, REQ-LOOP-13
//   snap, _ := agent.Snapshot(ctx)     // resync target after ErrStreamOverrun, REQ-LIFE-02
```

### Go: Subagent Delegation

```go
// Define a specialist agent. A child may use a different model or provider
// entirely (REQ-PROV-08); REQ-PROV-11 makes replaying a mixed transcript safe.
reviewerModel, _ := agentkit.ResolveModel("anthropic/claude-opus-4-5")

reviewerCfg := agentkit.AgentConfig{
    Model:        reviewerModel,
    MaxTokens:    4096,
    SystemPrompt: "You are a strict security code reviewer.",
    StopPolicy:   agentkit.StopAfterTurns(20),

    // Tool exposure is a resolution policy, not an additive list (REQ-TOOL-10).
    // This is how a subagent is given read-and-search-only access per delegation
    // without rebuilding the tool set by hand.
    ToolPolicy: agentkit.ToolPolicy{
        ToolNames: []string{"read_file", "search_files"},
    },
}
reviewer := agentkit.NewAgent(reviewerCfg)

// Wrap it as a tool callable by the orchestrator.
orchestrator.RegisterTool(agentkit.SubagentTool(
    "security_review",
    "Run a security-focused code review on a file or diff",
    reviewer,
))

// The orchestrator model can now call security_review as a tool.
// The child always starts with empty history (REQ-MULTI-02).
// Budget is propagated as an explicit config field, never through ctx values
// (REQ-MULTI-03; see the context convention in §5):
//     child.StopPolicy = agentkit.StopOverBudget(parent.RemainingBudget() * 0.3)
```

### MCP Client: Protocol Flow

The protocol flow for each MCP tool call:

```
1. Session init:
   pool.Connect("github") ->
     spawn npx subprocess ->
     send: {"jsonrpc":"2.0","id":1,"method":"initialize","params":{...}}
     recv: {"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{}},...}}
     send: {"jsonrpc":"2.0","method":"notifications/initialized"}
     send: {"jsonrpc":"2.0","id":2,"method":"tools/list"}
     recv: {"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"create_issue",...}]}}
     cache tool list as MCPToolDescriptors with prefix "gh__"

2. Agent loop — model emits tool_use for gh__create_issue:
   pool.Call("github", "create_issue", {"title":"Bug","body":"..."}) ->
     send: {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_issue","arguments":{...}}}
     recv: {"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"Issue #42 created"}],"isError":false}}
     return {"content": [...], "is_error": false}
   inject as tool_result in next user message

3. Session end:
   pool.Close("github") -> terminate subprocess
```

---

## 8. Non-Functional Requirements

### Performance

- **NFR-PERF-01:** The agentic loop overhead (excluding model API latency and tool execution time) must be less than 1 ms per turn.
- **NFR-PERF-02:** MCP server connection setup (stdio subprocess spawn + initialize handshake) must complete within 2 seconds for typical local MCP servers. Connection setup happens once per session, not per tool call.
- **NFR-PERF-03:** Tool schema serialization must be computed once per session and cached (REQ-CACHE-06), not recomputed on every model call.
- **NFR-PERF-04:** Parallel tool execution must use true concurrency: one goroutine per tool handler invocation, joined by `sync.WaitGroup` (**not** `errgroup` — REQ-GO-04). Only the handler body runs concurrently; interceptor invocation, event emission and result finalization are serialized under a single mutex (REQ-LOOP-05) and are excluded from the concurrency target. Sequential execution is used when `parallel_tools=false`, when any tool in the batch declares `ExecutionMode: Sequential`, or when the batch holds a single call.
- **NFR-PERF-05:** The streaming path must not buffer complete model responses before yielding the first token. `TextDeltaEvent` must be emitted as soon as the first streaming delta arrives.
- **NFR-PERF-06:** A Level 2 cache hit must add less than 0.5 ms overhead versus a direct response return. The LRU eviction path must not block the agent loop.
- **NFR-PERF-07:** Anthropic `cache_control` breakpoint injection must be a pure in-memory operation performed on **every** request; it must not make any additional API calls. There is no breakpoint cache and no structural-hash recomputation path to budget (§6.2a Level 1). Stamping the three markers must add less than 1 ms for tool sets up to 128 tools and transcripts up to 1000 messages.
- **NFR-PERF-08:** Google `CachedContent` creation is a network call and must run on a background goroutine before the first model call of the session when `context_cache_ttl` is set. The agent loop must not block waiting for it — it falls back to uncached operation if creation has not completed.
- **NFR-PERF-09 (performance budgets need an acceptance mechanism):** Every numeric budget in this section is either attached to a named `go test -bench` target with a CI threshold, or it is demoted to design guidance and labelled as such. A number with no benchmark behind it cannot fail, and therefore does not constrain anything. Prioritization note: in the closest shipped comparable — a zero-dependency Go agent SDK of comparable scope — the entire test budget went to differential, golden, parity and race testing and **no Go benchmarks were written at all**, which is evidence that correctness-under-concurrency and wire fidelity are where the defects actually live. If the benchmarks are not going to be written, the honest move is to delete the numbers rather than ship unenforceable ones.

### Reliability

- **NFR-REL-01:** The agentic loop must tolerate transient provider failures through the **two** retry layers of REQ-PROV-13 and REQ-PROV-14. Transport defaults: retry on 408/409/429/5xx honoring `x-should-retry`, `min(500ms × 2^attempt, 8s)` backoff with up to 25% **downward** jitter, server-dictated `Retry-After` honored up to a 60 s ceiling above which the request fails immediately. Semantic defaults: skipped entirely for account/quota/billing errors and for aborted turns. A retry policy that keys only on HTTP status does not satisfy this NFR. The default retry *count* is unresolved — see OQ-9.
- **NFR-REL-02:** Panics in any code the loop calls must never crash the agent process. Coverage is **not limited to tool handlers**: turn hooks, event hook plugins, tool interceptors, middleware, and provider `OnPayload`/`OnResponse` hooks are all third-party code executing inside the loop, and each must be invoked through a `recover()`ing wrapper.
  1. No listener, hook, or interceptor may be invoked while an agent lock is held, and every lock taken around an emit must be released by a function-scoped `defer`. A panicking listener inside a manual `Lock … emit … Unlock` leaks the mutex and hangs every in-flight tool goroutine at `Wait` — a **deadlock, not a crash**, so it produces no stack trace and no error.
  2. A tool handler panic becomes `tool_result(is_error=true)` and the loop continues. A hook, interceptor or middleware panic is converted to an error, surfaced through `OnError`, and must not abort the run.
  3. An error raised by a hook while a tool is in flight is buffered and re-raised **after** that tool execution settles — never in place, which would abandon the running goroutine.
  4. Any state accessor exposed to concurrent readers returns immutable snapshots; reference-typed fields are copy-on-write, not shared.
  5. Each of these paths carries a regression test that was verified to hang or crash before the fix landed. A deadlock test that has never been observed to fail proves nothing.
- **NFR-REL-03:** MCP server disconnects during a session surface as `ToolResultEvent{IsError: true}` for the affected tool call. The agent loop continues. Reconnection is attempted up to `per_session_reconnect_limit` (default 3).
- **NFR-REL-04:** Session state must be durable as the **append-only event log** of §6.12, not as a JSON snapshot of `ConversationHistory`. An interrupted session resumes by folding that log (REQ-SESS-02), which restores the model, the reasoning level, compaction checkpoints and the branch structure in addition to the messages. Serializing only `[]Message` is **not** a conforming implementation: it loses which model produced which turn, which is required for correct replay (REQ-PROV-11). Persistence failures are reported, never swallowed (REQ-SESS-08). Snapshot completeness is bounded by REQ-LIFE-07.
- **NFR-REL-05:** History compaction must be applied before **every** model call — including the calls issued after a tool batch within the same user turn, not once per user turn. The hook that performs it runs at the head of each loop iteration, immediately before the request it prepares (REQ-LOOP-04b), so a turn ended by the stop predicate never triggers a preparation for a request that will not happen, and a long tool-using turn cannot grow past the context window between compactions. Compaction failures fall back to `TurnWindowCompaction` rather than aborting the session.

### Security

- **NFR-SEC-01:** No credentials may appear in log output, audit events, or error messages. Credentials are redacted to the first 4 and last 4 characters with `***` in the middle, enforced at the `CredentialStore` boundary (REQ-AUTH-07).
- **NFR-SEC-02:** Path containment is enforced by resolving all paths to their absolute canonical form before comparison, after normalization (REQ-SEC-01). No string manipulation that could be fooled by non-canonical paths. Containment must be validated on a case-insensitive filesystem as well as a case-sensitive one — a check validated only on the latter is unvalidated (NFR-COMPAT-06).
- **NFR-SEC-03:** MCP server configs referencing `${VAR}` environment variables must be resolved at spawn time. Unexpanded variable references are a configuration error, not silently passed to the subprocess.
- **NFR-SEC-04:** The AgentKit MCP server in HTTP mode must require API key authentication on every request. Unauthenticated requests return HTTP 401.
- **NFR-SEC-05:** Plugin and skill code is verified at load time for prohibited import paths. No plugin may register `init()` functions that modify global state accessible to the agent runtime. This constrains AgentKit's own packages equally: the provider and plugin registries are held on config (REQ-PROV-09, REQ-PLUGIN-11), not populated by import side effect. An `init()`-populated package-level registry is convenient — it makes `NewSession` work with no wiring — and it is exactly what this requirement forbids, so the ergonomic cost must be paid in an explicit `RegisterDefaults(cfg)` call rather than recovered by exempting first-party code.

### Compatibility

- **NFR-COMPAT-01:** Go 1.21 and later minor versions supported.
- **NFR-COMPAT-02:** MCP client supports MCP protocol version 2025-03-26 and maintains backward compatibility with 2024-11-05 servers.
- **NFR-COMPAT-03:** New model IDs must work without an SDK release. This is satisfied by **catalog sibling-cloning** (REQ-CAT-03), not by pure pass-through. Providers do not pass the model string through untouched: the resolved `Model` descriptor supplies the wire API, base URL, context window, pricing, reasoning support and compat profile that the request builder, the budget gate and the `max_tokens` clamp all depend on. An unknown id under a known vendor inherits a sibling row with a warning; an unknown vendor is a configuration error. The catalog is data, versioned separately from the SDK and overridable by the caller — nothing in the resolution path may reject a model ID solely because it is absent from it.
- **NFR-COMPAT-04:** OpenAI-compatible endpoints — vLLM, llama.cpp, Ollama's `/v1`, Groq, Cerebras, DeepSeek, Together, OpenRouter, Cloudflare AI Gateway — are served by the `openai-completions` implementation with a per-model **compatibility profile** (REQ-PROV-12). A `base_url` override alone is insufficient and must not be presented as the integration path for these endpoints. Ollama's native `/api/chat` remains a separate `Api` because it is a different wire protocol, not a compat variant; it avoids Ollama's compatibility layer, which does not fully implement streaming tool calls or cache-token usage fields.
- **NFR-COMPAT-05:** The Google provider supports both the API-key path and the Vertex AI endpoint (service account / ADC). Switching requires only a config change, not a provider swap, and the ADC case must resolve to the *ambient* credential state of REQ-AUTH-04 rather than to "no key".
- **NFR-COMPAT-06 (platform matrix and cross-target gate):** The supported matrix is `linux/amd64`, `linux/arm64`, `darwin/arm64`, `windows/amd64`. Every release gate runs `GOOS=<target> go build ./...` and `go vet ./...` for every supported `GOOS`, **unconditionally** — not only when a build-constrained file changes. Platform-constrained files are called from unconstrained code, so an ordinary rename breaks a target while touching no constrained file and leaving the host test suite fully green. A host-only gate cannot see this. The tool layer concentrates the divergences: shell resolution (REQ-TOOL-06), process-tree termination (REQ-TOOL-17), exit-code semantics (a signal-killed process reports `128+signum` on unix; a Windows wait status carries no signal, and tool output must not claim a signal it cannot observe), and path containment against case-insensitive filesystems, 8.3 short names, UNC and device paths.
- **NFR-COMPAT-07 (provider surface pinning and drift):** Each moving external surface AgentKit tracks — the five provider APIs and the MCP specification — has a row in a single `docs/PROVIDERS.md` ledger recording the pinned API version or dated beta header, the vendor SDK version or traffic capture the NFR-TEST-06 goldens were produced against, and the date of that capture.
  1. Wire goldens are re-captured against the pinned reference on a fixed cadence and on every pin bump. A drift check that compares only a version string sees a clean slate and is wrong.
  2. Each row distinguishes **reviewed** (the changelog was read and its effect judged) from **implemented** (the change landed). A row may be reviewed-not-implemented; the next drift computation is the version delta **plus** every such row. Conflating the two is how a change is lost the moment more than one person maintains providers.
  3. Decisions about a surface are recorded once in the ledger's rulings section and are not re-litigated. Re-asking a settled question is itself a failure mode.
  4. The ledger holds bindings only. Per-cycle narrative belongs in commit messages; a ledger that grows a section per cycle stops being read and therefore stops binding.
  5. Scope is decided by an executable test wherever one is possible. Any path or field list in the ledger is a derived convenience: when a list and a test disagree, the test wins and the list is corrected in the same commit.

### Testability

- **NFR-TEST-01:** The `ProviderClient` interface allows mock implementations with zero external network calls, enabling full loop testing offline.
- **NFR-TEST-02:** The `PluginRegistry` accepts injected plugins without touching manifests or the file system.
- **NFR-TEST-03:** `SessionStore` and `ConversationHistory` round-trip **losslessly**, asserted on raw bytes rather than on reconstructed values. Coverage must include (a) all `ContentBlock` subtypes; (b) per-message provenance and opaque signature fields — `thinking.signature`, `thinking.redacted`, `tool_use.thought_signature`, `provider`, `api`, `model`, `response_model`, `raw_stop_reason`; (c) entry types the reader does not model, including a `compaction` entry whose `first_kept_entry_id` names an unknown type (the kept tail must survive); and (d) numeric values preserved verbatim rather than round-tripped through `float64`.

  For `ToolUseBlock` the assertion must be **byte equality on `Input`**, not `reflect.DeepEqual` on a decoded value: `DeepEqual` passes on a map that lost its key order, so a value-level property test cannot see the one field that is model-visible, and cannot see it in the one direction where it is lost. At least one golden fixture must have no key in sorted position at three nesting depths — top level, inside a nested object, and inside an object in an array — so that sorting anywhere shows up as different bytes. A loader that silently drops unknown records passes a naive round-trip test and still loses data.
- **NFR-TEST-04:** Every built-in tool must be independently testable against a temporary directory fixture, with no hidden dependencies on global state.
- **NFR-TEST-05:** The agentic loop must be testable with a scripted mock provider returning a predetermined sequence of responses (text, tool_use, tool_use+text) to verify multi-turn behavior without live API calls. **This mock ships as exported, supported API in the same package as the real providers** — not as test-only code inside the module. Consumers use it to test their own tool handlers, middleware and interceptors; an SDK whose double is internal forces every consumer to write one against their own assumptions about the loop. Because the mock is also the executable specification of the streaming protocol, its event sequence is normative: a real provider that disagrees with it is the one that is wrong. It is a functional double for the loop only, and does not substitute for NFR-TEST-06.
- **NFR-TEST-06 (wire-level differential testing):** Every provider must be covered by a differential harness comparing the exact request body the provider constructs against an **independently produced reference body** for the same scenario. A mock provider tests the loop against the abstraction; nothing in NFR-TEST-01 or NFR-TEST-05 tests the abstraction against the provider.
  1. Scenario files are shared across providers; each declares messages, tools and config. A provider with no scenarios is untested regardless of its unit-test coverage.
  2. Both sides capture through `OnPayload` (REQ-PROV-18), which stores the payload and returns an error, aborting before the first byte. The harness must require no API key and make no network call.
  3. The reference body is produced by an independent implementation — the vendor's own SDK at a pinned version, or recorded live-API traffic — **never hand-authored**, because a hand-authored expectation encodes the same mental model as the code under test.
  4. Comparison is structural over canonicalized JSON. **Normalized** (hidden on purpose): object key order, string escaping, whitespace. **Not normalized** (a difference here fails, on purpose): number literal text — decode with `UseNumber` and diff the literal so `1024` vs `1024.0` vs `1e3` stays visible instead of being laundered through a float; array order, ever; key sets; and **`null` versus absent**, which is what makes REQ-PROV-16 enforceable.
  5. Key order is moved to a side channel, never discarded: each side emits one line per object as `<path>\t<keys in original order>`, walking objects by sorted key so both sides traverse identical paths. A scenario may declare `order_sensitive_paths` where insertion order is observable to the model or the provider (chat-template arguments, model-authored tool-call arguments, reasoning blocks replayed verbatim).
  6. Scenario option decoding uses `DisallowUnknownFields`. An unmapped option is a hard error — otherwise both sides ignore it and agree for the wrong reason.
  7. The harness is a **separate Go module** so that consumers of AgentKit never resolve the reference implementation's dependency tree (REQ-GO-11).
- **NFR-TEST-07 (divergence classification and the accepted-divergence ledger):** The NFR-TEST-06 harness assigns every scenario exactly one of four states and exits accordingly.

  | State | Meaning |
  |---|---|
  | `PASS` | Reference and provider are identical. |
  | `KNOWN` | Differs, and **every** difference matches an entry in `known-divergences.json` on scenario, JSON path and kind. Accepted, tracked debt. Not clean. |
  | `FAIL` | Differs in any way the ledger does not cover. A provider bug until proven otherwise. |
  | `FIXED` | A ledger entry that no longer fires. |

  | Exit | Meaning |
  |---|---|
  | `0` | Every scenario is `PASS` or `KNOWN`, and every ledger entry still fires. |
  | `1` | At least one `FAIL`. |
  | `3` | No `FAIL`, but at least one `FIXED` (stale ledger entry). |

  1. A stale ledger entry **fails the run**. It is a live, unattended permission slip: the day someone reintroduces exactly that regression the harness reports `KNOWN` and exits `0`, and the defect ships. `FIXED` takes its own exit code so "got worse" is distinguishable from "got better, paperwork behind" — both are non-zero because both need a human.
  2. The summary line always prints the `KNOWN` count and never renders as clean.
  3. **A run that never reached the scenarios is dark, not passing.** Both capture arms run once before the scenario list; either failing aborts with exit `1` and prints no tally. Zero compared scenarios is not a result.
  4. Each ledger entry states what diverges and why it is accepted. An entry buys time; it does not close the defect. The ledger is reviewed on every pin bump (NFR-COMPAT-07).
- **NFR-TEST-08 (golden tests for assembled artifacts):** Byte-for-byte golden tests are required for the artifacts AgentKit assembles from many parts, where no single unit is wrong but the composed whole can drift: (a) the fully assembled default system prompt, built through the **real tool resolver** rather than a fixture, so that a change to any tool's description or guidelines surfaces as a prompt diff in review, with a second golden pinning the custom-system-prompt branch (assembly order and the assertion that built-in blocks are absent); (b) the per-provider request body (NFR-TEST-06); (c) the serialized session log (NFR-REL-04); and (d) the model-visible wrapper strings of REQ-SESS-07.
  1. Every golden file's test carries a provenance docblock naming the reference implementation, its version or commit, and the exact command that produced the file.
  2. A golden is regenerated **from its reference**, never from AgentKit's own output. Editing an assertion to match new output is a design change requiring justification, not a test fix. A golden regenerated from the output it exists to check is circular and pins whatever last shipped.
  3. Every golden must exercise the same resolution path the shipped code takes. Passing an explicit fixture path where the shipped code runs a resolver proves nothing about the shipped code.
  4. Protocol goldens include **reject vectors** — inputs the reference refuses — not only accepted ones. A decoder that accepts more than the reference is an interop defect an accept-only corpus cannot see.
- **NFR-TEST-08a:** Per-tool usage guidance lives on the tool as `Tool.PromptGuidelines`, not in a separate prompt file that drifts from the tool. The prompt builder collects guidelines across the resolved tool set, deduplicating while **preserving first-seen order**, and appends the universal guidelines last.
- **NFR-TEST-09 (fuzzing at untrusted boundaries):** The MCP message decoder and any other decoder covered by REQ-SEC-11 ship a Go fuzz target asserting three properties over arbitrary bytes:
  1. **Nothing panics**, whatever the input — the executable form of REQ-SEC-11.
  2. **Anything accepted can be re-encoded.** A message AgentKit can read but not write is one it silently drops when relaying it.
  3. **Re-encoding is a fixed point:** `encode(decode(b)) == encode(decode(encode(decode(b))))`, compared as bytes.

  Property 3 catches key-order and numeric-formatting drift; a `reflect.DeepEqual` test over decoded values sees neither. These are standing requirements, not a one-off audit.
## 9. Open Questions

### OQ-1: Conversation history ownership in multi-turn skill subagent spawning

When a skill declares `[skill.subagent]` with `mode='before_session'`, the session runner spawns a separate session and injects the result into the main session's system prompt. If the pre-analysis session fails or times out, should the main session: (a) abort with an error surfaced to the caller, (b) proceed without the pre-analysis with a warning injected into the system prompt, or (c) make the behavior configurable per skill via an `on_failure` manifest field?

Recommendation: default to option (b) — proceed with warning — since pre-analysis is enrichment, not a hard dependency. Make `on_failure = "abort" | "warn" | "skip"` a configurable manifest field. Note that this is consistent with REQ-SKILL-10's lenient-manifest posture: authored content failing should degrade, not reject.

### OQ-2: Schema representation in Go — RESOLVED

**Resolved: neither code-gen nor reflection. Schemas are a structured value type built with typed combinators (REQ-TOOL-02, REQ-GO-07).**

The original question posed a binary — `agentkit-schemagen` code generation versus runtime `reflect` — and both options share a hidden assumption: that a schema's only job is to be *produced* once and then serialized. It is not. Schemas are rewritten before they reach the wire (strict-subset conversion, per-provider dialect translation), used to coerce and validate incoming arguments, and rendered into model-facing error text. Under that load, `json.RawMessage` makes every operation a parse-then-reserialize round trip that loses property order (which is model-visible, REQ-TOOL-12), reflection cannot express `const: null` versus an absent const or carry a passthrough keyword, and code generation puts a build step between a developer and a one-line tool.

The shipped comparable uses a 30-field `Schema` struct with explicit `PropertyOrder` and `Extra`, built with `Object(Prop("city", String("City name")), Opt("limit", Integer()))`, and contains no `reflect` usage in its schema package and no code generator. Ergonomics are comparable to a struct-tag approach; the transformability is not available any other way.

Residual question, now narrower: whether to *additionally* ship an optional reflection-based convenience constructor for simple flat structs, accepting that it cannot express the full `Schema` surface. Recommendation: not in v1.

### OQ-3: MCP Resources and Prompts in client path

The initial release scopes MCP client support to Tools only. The implementation complexity for `resources/list` and `resources/read` is low — two additional JSON-RPC methods on the same connection. The question is whether exposing resources introduces a new attack surface (resource URI auto-fetch as an SSRF vector) that requires additional design work. REQ-SEC-08 already disables auto-fetch and REQ-SEC-11/12 now bound and strictly decode every MCP payload, which removes much of the original hesitation; what remains is whether a resource URI reaching the model constitutes a capability the embedder must opt into. Decision needed before `MCPServerConnection` is finalized.

### OQ-4: SummarizationCompaction — in-process API call vs. server-side compaction

`SummarizationCompaction` can either: (a) call the model API directly from within the SDK to generate a summary (adding latency and token cost, but provider-agnostic and uniform across all five providers), or (b) delegate to Anthropic's server-side `compact-2026-01-12` beta mechanism (lower latency, but Anthropic-specific, and requires passing compaction blocks unchanged in subsequent turns — stripping them to plain text silently breaks compaction state).

**Recommendation, revised: option (a) uniformly, for all providers, with option (b) as an opt-in optimization for the Anthropic provider only.** The earlier recommendation had this the other way round. The closest shipped comparable — a zero-dependency Go agent SDK with the same multi-provider surface — implements in-process summarization for every provider and uses no server-side compaction mechanism at all. Option (a) is also what makes REQ-GO-16's failure taxonomy expressible (`max_tokens` is a failure, `aborted` is not, a `tool_use` in the response is), and that taxonomy is what keeps a truncated summary from becoming a permanent checkpoint under REQ-GO-12.2.

Two things must be settled before (b) ships as a default anywhere:

1. **A failure taxonomy for server-side compaction.** REQ-GO-16 defines what a bad summary looks like when the SDK generates it. There is no defined equivalent for a provider-produced compaction block: what does the SDK do when the block cannot be produced, is truncated, or arrives alongside a `max_tokens` stop reason?
2. **A dual-path burden.** Option (b) as a default means two compaction implementations with different failure modes, and a session whose model changes mid-conversation (REQ-SESS-03) can cross between them. Provider-side compaction blocks are opaque and same-model-only under REQ-PROV-11, so a mid-session provider change strands them.

Stability of the beta header is a necessary but not sufficient condition.

### OQ-5: AgentKit MCP server authentication in stdio mode

When AgentKit runs as an MCP server in stdio mode, the process is spawned by the host application and authentication relies on OS-level process isolation. Is that sufficient for typical developer deployments, or should an API key be required even for stdio mode?

Recommendation unchanged: match the prevailing practice — no additional credential check for stdio mode in the initial release, document the threat, and provide the environment-variable opt-in for teams with stricter requirements. Note that REQ-SEC-11's decode bounds now apply to the stdio server path regardless of the authentication decision, so an unauthenticated local peer still cannot exhaust memory or crash the process with a malformed frame.

### OQ-6: Plugin local directory auto-discovery scope

Recommendation unchanged: require explicit declaration for local plugins via `plugin.toml` manifests in explicitly configured directories. This makes the plugin set predictable and auditable and avoids loading unexpected plugins. Note the added consideration from REQ-SKILL-12: any discovery source rooted in the working directory is untrusted input, and a plugin is strictly more powerful than a skill — auto-discovery of project-local plugins would need a trust gate at minimum, which is a further argument for explicit declaration.

### OQ-7: Streaming backpressure in Go — RESOLVED

**Resolved: none of the four listed options. `EventStream` is not a channel (REQ-GO-08).**

The question assumed a channel-based stream and asked only how to size its buffer. The answer is that the producer must not block at all: it is a paid model call holding an HTTP connection open, and its liveness must not be a property of the slowest attached consumer.

| Option | Failure |
|---|---|
| (a) unbuffered channel | Serializes the agent loop to the consumer's frame rate |
| (b) fixed 64-event buffer | A single slow renderer stalls the provider SSE body read and can trip the provider's stream idle timeout, failing the request. An event count is also not a meaningful bound: one `ToolResultEvent` may be 50 KB and one `TextDeltaEvent` 8 bytes, so "64" is anywhere between a few hundred bytes and several megabytes. |
| (c) drop-oldest | Corrupts text reconstruction — deltas are not independent samples, and a consumer applying incremental deltas cannot detect the gap |
| (d) blocking producer | (b) with a delay; correct only for pipeline consumers, which are not the driving use case (Persona C drives a UI) |

The adopted design is an unbounded mutex + `sync.Cond` queue whose `Push` never blocks, never drops and never reorders. A slow consumer costs memory and nothing else, bounded in practice by `max_tokens` — the worst case is one model response's worth of deltas in a slice. Cancellation is `context.Context`, not `stream.Close()`. Abandonment is safe because `Result()` is fed by the terminal event rather than by consumption. `StreamOptions.BufferSize` is removed; callers needing a hard bound set `MaxPendingBytes`, which drops the **consumer** with `ErrStreamOverrun` and lets the run finish, and which has a floor equal to the largest single event AgentKit can emit so one legal event is never undeliverable.

This resolution depends on REQ-OBS-06b: an unbounded queue is only useful if buffered events carry independent snapshots rather than pointers to one live, mutating message.

### OQ-8: The `execute` boundary in non-interactive deployments

REQ-SEC-03 and REQ-SEC-04 replace the command allowlist and shell-operator regex with a per-call interceptor, on the grounds that a static allowlist is both trivially escaped and too narrow to run a build. That reasoning assumes an embedder that can answer a permission question — interactively, or from a policy with real context.

AgentKit's canonical consumers may not be able to. A nightshift daemon triaging issues overnight and a hub gateway serving API requests both have to decide `Block` or `Allow` with no human present, and a policy that answers "allow" unconditionally is strictly worse than the allowlist it replaced. Options: (a) ship a reference `RestrictedPolicy` interceptor (allowlist plus operator rejection) as an opt-in default for non-interactive embedders, kept out of the SDK's enforcement path so it can be replaced rather than only narrowed; (b) require every embedder to supply an interceptor, failing construction if `execute` is registered without one; (c) run untrusted `execute` in an OS-level sandbox (container, namespace, seatbelt) and drop in-process command filtering entirely.

Recommendation: **(b) plus (a)** — construction fails loudly if `execute` is registered with no interceptor, and the SDK ships `RestrictedPolicy` as an importable, replaceable starting point. Note that under (c) the file tools' `checkPath()` also becomes redundant, which would simplify REQ-SEC-01; whether AgentKit takes on a sandbox dependency is a separate decision that conflicts with G1.

### OQ-9: `RetryMiddleware` default — 3 retries, or off?

REQ-PROV-13 sets the transport default to 0 retries, on the reasoning that retry policy belongs above the transport. NFR-REL-01 does not yet state the agent-level default, and the two halves must agree.

The inherited default of 3 comes from ordinary HTTP client practice rather than from an agent loop: a hidden retry multiplies cost and tail latency invisibly, and in a loop that may run to `max_turns` it multiplies them once per turn — against the same `max_budget_usd` the SDK separately promises to enforce (REQ-LOOP-08). A caller who has not asked for retries is spending money they did not authorize. The alternative is a default of 0: retries are the caller's policy, `RetryMiddleware` is composed in explicitly, and an explicit `0` is never silently coerced upward.

Recommendation: default to 0 with `RetryMiddleware` documented as a one-line opt-in. The policy matrix itself is now specified normatively in REQ-PROV-13/14; only the default count is open.

### OQ-10: An op vocabulary for non-append streaming views

REQ-OBS-06's incremental events are append-only: `TextDeltaEvent` appends to an assistant message, `ToolInputDeltaEvent` appends to a tool call's argument JSON. That is correct for both. It is wrong for any view that is bounded or rewritten rather than grown — a long-running tool streaming into a fixed scrollback window, a progress line that is replaced rather than extended, a diff preview that is re-rendered. Today the only way to express those is a whole-value event: a 200 KB bounded buffer that gains 8 bytes at the tail and drops 8 from the head ships 200 KB per token.

The alternative is a second, structured event class carrying a small op vocabulary — set, delete, string-append, string-front-truncate, array-splice — under which the rolling-window case is `truncate(8) + append(8)`, 16 bytes. Two consequences if we take it: (a) the two ops that make it worth doing are the two RFC 6902 does not have, so this is a bespoke vocabulary and therefore a compatibility commitment; (b) an op stream needs a defined recovery point — a periodic whole-value event and a producer-side bound on the distance to the last one — or a consumer that misses one op has no route back to a consistent state except a full teardown.

Recommendation: not in v1. REQ-OBS-09's snapshot-resync rule is sufficient for the append-only taxonomy we ship. Revisit before adding any event whose payload is a whole re-rendered view — that is the shape that makes whole-value sends expensive, and adding one is the point of no return.

### OQ-11: Deferred/background request support

REQ-PROV-19 admits deferred submission as an optional, capability-probed provider method. Open: whether it ships in v1 at all, and if so how far the durability guarantee extends. A `DeferredHandle` outlives the process — a 24-hour window means the redeeming process may be a different binary version — which makes the handle a **serialization compatibility surface** in a way no other canonical type is. Options: (a) omit entirely from v1; (b) ship the type and the stop reason but no built-in poller, so the embedder owns redemption; (c) ship a poller with a documented handle-format version.

Recommendation: (b). The cost of reserving the stop reason and the handle type now is near zero, and a loop that has never heard of `deferred` mis-handles it as an empty completion — a failure worth foreclosing even if AgentKit itself never submits one.

### OQ-12: Registry ergonomics versus NFR-SEC-05

REQ-PROV-09 and REQ-PLUGIN-11 require registries held on config rather than in package-level globals, and NFR-SEC-05 forbids `init()`-time global mutation. The shipped comparable does the opposite — a package-level registry populated by import side effect, with a blank import making everything work with no wiring, plus `Unregister`/`Clear` escape hatches for tests — and the ergonomic difference is real: `ResolveModel` + `NewSession` + `Run` with no registration call is a materially better first five minutes.

The tension is not resolvable by exempting first-party code: an `init()`-populated global is exactly as untestable and exactly as order-dependent whoever wrote it, and a registry that first-party code may mutate globally is one a plugin can reach too. Options: (a) hold the line, and pay for it with an explicit `agentkit.RegisterDefaults(cfg)` in every quickstart; (b) provide a package-level *default config* value that built-ins register into at init, with every API taking an explicit config that defaults to it — globals for convenience, injection for tests, and no ambient mutation after construction; (c) generate the default registration in the constructor so there is no init-time work at all.

Recommendation: (b), with the default config frozen after first use so late registration is an error rather than a race. Decision needed before the provider and plugin registries are finalized, because it determines whether `NewAgent` can have a one-argument form.
---

## Appendix A: Prior-Art Review

### A.1 What was reviewed and why

Version 0.3.0 incorporates a structured review of [`sky-valley/pi`](https://github.com/sky-valley/pi) — a pure-Go port of Mario Zechner's `pi` agent harness, comprising a unified multi-provider LLM API, an agent runtime, and a coding-agent CLI. It was selected because it is the closest existing artifact to what this document specifies and it has actually shipped:

| Property | `sky-valley/pi` | AgentKit (this PRD) |
|---|---|---|
| Language | Go | Go |
| Scope | Provider layer + agent loop + coding agent | Provider layer + agent loop + coding tools |
| Dependency posture | Two `golang.org/x` modules, CI-enforced | Standard library only, CI-enforced (REQ-GO-13) |
| Providers | Anthropic, OpenAI Completions, OpenAI Responses, Google | Those four plus Ollama |
| Size | ~105k lines Go / ~57k lines test | — |
| Status | Shipped, race-clean, differentially tested | Draft specification |

The review covered six lenses — the agent loop and event protocol; the provider layer, catalog, caching and retries; the coding tools, prompt assembly and trust boundary; session persistence, compaction and the SDK facade; the client/server/protocol layer; and the project's engineering process — plus a completeness sweep over what those six missed.

Two caveats on how to read what follows. First, `pi` is a *port*: its design decisions are constrained by fidelity to an upstream TypeScript implementation, so a few reflect that constraint rather than an independent judgement. Second, it makes different choices than this PRD in places where it is plainly *worse* for AgentKit's stated goals — notably a package-level provider registry populated by `init()` (see OQ-12) and the absence of any path containment (REQ-SEC-01 is retained, with its scope stated honestly). Not everything shipped is a lesson.

### A.2 Corrections — where the 0.2.0 draft was wrong

These are not additions. Each is a place where the previous draft specified something that does not work, and the evidence is a shipped implementation that had to do otherwise.

| # | 0.2.0 said | Correction | Where |
|---|---|---|---|
| 1 | Loop breaks when `stop_reason != "tool_use"` | Iterate on the **presence of `tool_use` blocks**. Gemini and several gateways return a STOP-family reason *with* tool calls; a stop-reason gate silently drops them and returns an empty answer — and passes every Anthropic-only test | REQ-LOOP-01 |
| 2 | All tool results go in **one user message** — "the most common implementation mistake" | True for Anthropic, **unrepresentable** for OpenAI (one `role:"tool"` message per result) and Gemini. It is a wire-format rule, not a loop invariant; canonical history is one `ToolResultMessage` per call | REQ-LOOP-02 |
| 3 | Five stop conditions in priority order | One post-turn `StopPolicy` predicate. More importantly the *position* is load-bearing: checking a limit between tool extraction and tool execution ends the transcript with dangling `tool_use` blocks that no provider accepts on resume | REQ-LOOP-04/04a |
| 4 | `max_tokens` is a terminal stop condition | With tool calls it is a **recoverable per-call failure**. Streamed arguments are salvage-repaired into valid JSON, so a truncated `edit_file` passes schema validation and silently corrupts the file. Only the stop reason can catch this | REQ-LOOP-10 |
| 5 | Parallel tools via `errgroup` | `errgroup` cancels siblings on first error and returns only the first error — but every call in a batch needs a result. `sync.WaitGroup` with slot-indexed results, plus a serialized finalize phase | REQ-GO-04, REQ-LOOP-05 |
| 6 | Cancellation "cleanly terminates without corrupting history" | History after an abort is **recoverable, not clean**. The partial turn is appended verbatim and made sendable by an unconditional per-request repair pass — which someone has to build, and which `context.Context` does not give you | REQ-LOOP-09, REQ-PROV-11 |
| 7 | Channel-based `EventStream`, buffer size an open question | Not a channel. An unbounded non-blocking queue: the producer is a paid model call holding an HTTP connection, and blocking it on a slow UI trips the provider's idle timeout and kills the request | REQ-GO-08, OQ-7 |
| 8 | `ProviderClient.complete()` with a streaming variant | **Streaming is the primitive**; `complete()` is derived from it once in the SDK. Two hand-written paths per provider disagree | REQ-PROV-01 |
| 9 | Five providers, one per vendor | Providers are keyed by **wire API**. OpenAI Completions and Responses are separate implementations; OpenRouter and Ollama-compat are catalog rows | REQ-PROV-02 |
| 10 | OpenAI-compatible endpoints are a `base_url` swap | Thirteen-plus named quirk flags, each corresponding to a request that 400s, hangs, or silently answers nothing | REQ-PROV-12, NFR-COMPAT-04 |
| 11 | Model strings pass through untouched | They cannot: `max_tokens` clamping, cost, thinking clamping and compat all need per-model metadata. A catalog with **sibling-cloning** preserves "new IDs work immediately" without pretending the SDK knows nothing | REQ-CAT-03, NFR-COMPAT-03 |
| 12 | Anthropic cache breakpoints recomputed only on structural change | A **rolling** breakpoint recomputed every request. The "optimization" produces a static prefix-only breakpoint that re-pays full input price on the growing transcript — the dominant cost in exactly this workload | §6.2a, NFR-PERF-07 |
| 13 | `RetryMiddleware` on 429/5xx | Two layers. Most real failures are not statuses at all — truncated SSE, DNS failures, gateway text bodies — and quota/billing errors must be denylisted *first* or the backoff budget burns for nothing | REQ-PROV-13/14 |
| 14 | Command allowlist + shell-operator regex | Neither is a boundary: an allowlist wide enough to run a build is escapable, and a model blocked from pipes writes a script and executes that. The boundary is a per-call embedder interceptor | REQ-SEC-03/04, OQ-8 |
| 15 | `strict bool` on `Tool` | A struct with `prefer`/`require`. A naive `strict: true` sends a schema the API rejects the moment a tool uses `$ref` or an optional field — and the 400 kills the whole request, every turn | REQ-TOOL-03 |
| 16 | `edit_file → {replaced: int}` | Multiplicity is a **rejection**, not a count. Silent multi-site replacement is how an agent corrupts a file it was asked to touch once | REQ-TOOL-04c |
| 17 | `NFR-REL-04`: history "serializable to JSON and restorable" | A `[]Message` snapshot cannot record which model produced which turn, which is required for correct replay. The durable unit is an append-only log that folds back into a configured agent | §6.12, NFR-REL-04 |
| 18 | OpenTelemetry spans (REQ-OBS-01) alongside "zero dependencies" (REQ-GO-11) | The two could not both hold. Two small AgentKit interfaces plus a no-op default; the OTel binding is the host's or a nested module's | REQ-OBS-01 |
| 19 | Skills: strict TOML, unknown keys rejected, body injected at `pre`/`post`/`replace_section` | Lenient manifests (authored content should degrade, not reject) and **progressive disclosure** — name, description and path only, so cost is ~3 lines per skill regardless of size. `replace_section` would let discovered content delete the host's own prompt | REQ-SKILL-06/10 |
| 20 | Sub-packages and build tags satisfy the zero-dependency rule | They do not: a build-tagged import still appears in `go.mod`, `go.sum` and every downstream SBOM. Only a **nested module** confines a dependency | REQ-GO-11 |

### A.3 Additions — concepts absent from the 0.2.0 draft

**Trust.** Project-local prompt material (`.pi/skills`, `AGENTS.md`, `CLAUDE.md`) is discovered only when the embedder affirmatively establishes trust, and a headless host resolves to *untrusted*. A hostile repository otherwise authors part of the system prompt by being the cwd. The fail-closed rule extends to an unresolvable `HOME` — ordinary in containers, CI and cron — which must yield *no* global tier rather than a relative path a repository can impersonate. (REQ-SKILL-12, REQ-CTX-03, REQ-SEC-10.)

**Session repair.** A session log is written by a process that can be killed mid-write. Truncated final lines, unknown entry types and dangling parents are all normal and must be repaired-and-reported on load, never rejected. Distinct from transcript repair, which is a send-time provider concern. (REQ-SESS-05/06.)

**History as a tree.** An append-only log cannot delete, so rewind and edit-and-retry are re-parenting, not truncation. (REQ-SESS-07.)

**Compaction is permanent and anchored.** Once a summary checkpoint exists it is always re-applied; the threshold decides only whether to *extend* it. The naive "compact when over threshold" reading **oscillates**: the compacted request reports small usage, the next check passes, full history returns, the check fails again. Cut points may never land on a tool result. And there is no tokenizer — the trigger anchors on the last valid provider-reported usage, skipping aborted and zero-usage turns, both of which look like valid anchors and silently reset the estimate to near zero. (REQ-GO-12/14/15/16.)

**Mid-run input.** Steering and follow-up queues with specified drain points, plus a run slot claimed before the queues are drained. A UI needs a way to type while the agent works; without it, typing during a turn is an error. (REQ-LOOP-13/14/15.)

**Argument byte fidelity.** Tool-call argument key order is model-visible and Go sorts map keys unconditionally, so the default behaviour is wrong on every provider — a reordered replay is a silent prompt-cache miss on every subsequent turn, visible only in the bill. (REQ-TOOL-12.)

**Concurrent tool safety.** A refcounted per-path mutex keyed on the symlink-resolved path. Models routinely emit two edits to one file in a single batch. (REQ-LOOP-12.)

**Batch termination.** A tool can end the run, and the vote is an **AND** across the batch — OR semantics compute N−1 results the model never sees. (REQ-TOOL-13.)

**Process control.** Process groups, tree kill, a shared stdout/stderr pipe for true interleaving, and a re-arming drain timer for descendants that outlive the parent. Nothing in 0.2.0 addressed orphaned children surviving a timeout. (REQ-TOOL-17.)

**Images.** The `ContentBlock` union had no image type at all, and tool results were assumed to be text. (REQ-TOOL-14.)

**Auth beyond an API key.** Ordered multi-name env resolution with distinct auth *schemes*, a per-request scoped override map, a three-state header map whose nil value **deletes** a provider default, an *ambient* credential state distinct from "no key", and serialized OAuth refresh — without which N concurrent turns each rotate the same refresh token and N−1 hold an invalidated one. (§6.2c.)

**Bounded, strict decoding of untrusted input.** REQ-SEC-02 capped tool *output*; nothing capped what an MCP peer could send. (REQ-SEC-11/12.)

**Deferred requests.** A third request mode beyond complete/stream, with a stop reason that persists into the transcript. A loop that has never heard of it mis-handles a deferred response as an empty completion. (REQ-PROV-19, OQ-11.)

### A.4 Process lessons

Four practices from the reviewed project are now requirements, because each converts an assertion in this document into something that can fail:

1. **The dependency policy is a test** (REQ-GO-13) — a build-graph walk with a `map[module]reason` allowlist, where adding a dependency is an edit to that map and the edit is the review gate. Critically, the cgo check must run with `CGO_ENABLED=1`: with cgo off the toolchain excludes cgo files by build constraint, so the check passes while the dependency is present. Cgo-freedom, not module count, is what determines whether the SDK cross-compiles.
2. **Wire-level differential testing** (NFR-TEST-06/07) — compare the exact request body against an independently produced reference, with an accepted-divergence ledger in which a *stale* entry fails the run. A stale entry is an unattended permission slip: the day someone reintroduces that exact regression, the harness reports `KNOWN` and exits clean.
3. **Golden tests for assembled artifacts** (NFR-TEST-08) — the system prompt, the request body, the session log. No single unit is wrong; the composed whole drifts. A golden regenerated from the output it exists to check is circular.
4. **A pinned upstream ledger** (NFR-COMPAT-07) — the reviewed project tracks a moving upstream through a per-file port ledger pinned to a commit. AgentKit tracks five moving provider APIs and the MCP spec, which is the same problem. The rule that transfers: distinguish *reviewed* from *implemented*, or a change is lost the moment more than one person maintains providers.

One anti-lesson is worth recording. That project ships 57k lines of test against 48k of source and contains **zero Go benchmarks** — the entire budget went to differential, golden, parity and race testing. This document carries four numeric performance budgets with no acceptance mechanism behind any of them (NFR-PERF-09). Either attach each to a benchmark and a CI threshold, or delete the numbers: an unenforceable budget reads as rigour and provides none.

### A.5 The daemon question

The reviewed project also contains an entire architectural layer this PRD's NG3 rules out: a client/server agent daemon over a unix socket, with a hand-written CBOR wire codec (golden- and fuzz-tested), session snapshots, a request queue serializing multiple attached clients against one agent loop, and a delta-synchronization layer for streaming incremental state to those clients.

That is not a reason to build a daemon. It *is* evidence for what NG3 must not foreclose. Detach/reattach, a long-running agent surviving its client, and multiple UIs on one session are ordinary requirements for an interactive coding agent, and every one of them needs something from *inside* the loop: a cheap non-blocking phase predicate, a snapshot safe to take mid-turn and carrying producer identity, an abort reachable without owning the `Run` goroutine, and mid-run message queues with defined drain points. Transport, framing and RPC are ordinary code a consumer can write; the loop-internal seams are not, and a consumer who needs them and cannot get them forks the library.

§6.13 therefore specifies those seams and nothing more. AgentKit still ships no daemon.
