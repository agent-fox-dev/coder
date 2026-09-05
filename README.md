# AgentKit

A dependency-free Go agent SDK. The loop, the tool system and the provider
abstraction are ordinary Go you can read and step through — nothing is hidden
inside a subprocess or a graph engine.

**The root module requires nothing outside the Go standard library**, and that
is enforced by a test rather than asserted in prose ([`internal/policy`](internal/policy/deps_test.go)).

```bash
go test ./...          # everything, offline, no API key
go run ./examples/agentdemo
```

The demo drives the real loop against a scripted provider with no network, and
prints seven behaviours — five the specification originally got wrong, plus a
kill-and-resume across two "processes" and three concurrent delegations.

## Status

This implements the core of [`agent-kit-prd.md`](agent-kit-prd.md) v0.3.2.
It is a working library with a thorough test suite; it is not a finished
product. [What is not built](#what-is-not-built) is stated below rather than
left to be discovered.

| Package | What it owns |
|---|---|
| `jsonx` | Order-preserving JSON. Decodes once, marshals in slice order at every depth. |
| `schema` | Structured JSON Schema value + typed combinators. No reflection, no codegen. |
| `core` | Canonical vocabulary and every interface seam: messages, content blocks, events, `EventStream`, `Tool`, `ProviderClient`. |
| `catalog` | Embedded model catalog, resolution, sibling-cloning, `max_tokens` and thinking-level clamping. |
| `session` | Append-only JSONL log, damage-tolerant loader, branch tree, resume fold. |
| `skills` | Skill manifests (hand-rolled TOML subset), progressive disclosure, project context files, and the default-off trust gate. |
| `tools` | Built-in tools, path containment, bounded accumulator, process control, glob + ignore. |
| `provider` | Send-time transcript repair shared by every wire API. |
| `provider/{anthropic,openai,google,ollama,faux}` | One wire API each. |
| `.` (root) | `Agent`, the loop, the batch executor, stop policies, the argument pipeline, compaction, Axis 1 middleware, `SubagentTool`, session resume. |

## The parts worth reading

Most of this SDK is unremarkable. These parts are not, because the obvious
implementation is wrong and the failure is silent.

**The loop iterates on the presence of `tool_use` blocks, never on
`stop_reason`** ([`loop.go`](loop.go)). Gemini and several OpenAI-compatible
gateways return a STOP-family finish reason *alongside* tool calls. A loop
gated on the stop reason drops them silently, returns an empty answer, and
passes every Anthropic-only test.

**One `ToolResultMessage` per call, and coalescing is the provider's job.**
The specification called "all tool results in a single user message" a *loop*
invariant and named splitting them the most common implementation mistake.
It is an Anthropic **wire** rule. One canonical transcript with three parallel
tool calls produces three genuinely different bodies:

| Provider | Shape |
|---|---|
| Anthropic | 1 user message, 3 `tool_result` **blocks** |
| OpenAI | 3 `role:"tool"` **messages**, keyed by `tool_call_id` |
| Gemini | 1 user content, 3 `functionResponse` **parts** |

With two providers you can still believe one shape is canonical and the other
an exception. The third settles it. `TestThreeWireShapesFromOneTranscript`
pins all three.

Worse on two of those wires: **Ollama's native API and Gemini's
`generateContent` carry no id at all**, so results pair *positionally*. Order
is load-bearing, a partial batch is inexpressible (which is what makes the
repair pass's synthetic results load-bearing rather than defensive), and
`is_error` has nowhere to go. The canonical layer keyed on `tool_use_id` is
still right — it is the only representation that survives these wires — but
identity there is reconstructed, not transmitted.

**`max_tokens` with tool calls executes none of them.** Streamed arguments are
salvage-repaired into valid JSON, so a truncated `edit_file` whose
`new_string` was cut off passes schema validation and applies cleanly, quietly
corrupting the file. Only the stop reason can catch it.

**The abort decision is made once, before any handler starts**
([`batch.go`](batch.go)). Per-goroutine `ctx.Err()` checks — the obvious Go
idiom — let the scheduler split a batch, which surfaces in production as
phantom side effects after the user pressed Ctrl-C.

**`errgroup` is not used for tool batches.** It returns the first error and
cancels the siblings; in an agent loop every call needs a result, or the next
request carries dangling `tool_use` blocks.

**The finalize mutex is batch-scoped, not the agent lock.** Reusing the agent
mutex *is* the deadlock the reliability requirement exists to prevent — a
panicking listener leaks it and hangs every peer at the join, producing no
stack trace and no error.

**Compaction applies its checkpoint before estimating.** The naive reading
oscillates: the compacted request reports small usage → the threshold passes →
full history returns → it fails again. Each swing invalidates the provider's
cache prefix and re-sends content already paid to summarize.

**A non-unique `old_string` is a rejection, not a replace-all**
([`tools/edit.go`](tools/edit.go)). There is deliberately no `{replaced: N}`
success shape: silent multi-site replacement is how an agent corrupts a file it
was asked to touch once.

**Tool-call argument bytes reach the wire unchanged.** Go sorts map keys
unconditionally, so a decode-and-re-encode round trip reorders every replayed
call. On OpenAI, where arguments ride as a JSON string, that changes the text
the model is conditioned on and shifts the prompt-cache prefix — a silent cache
miss on every later turn, visible only in the bill.

## Two corrections to the specification

Implementation found a hole in `REQ-PROV-11`, the send-time repair pass, and
the PRD has been amended.

Rule 2 drops an assistant message whose stop reason is `Error` or `Aborted` —
including its `tool_use` blocks. But the `ToolResultMessage`s that answered
those blocks are separate canonical messages and survive rule 2 untouched, and
every provider rejects a tool result whose `tool_use` is absent. The seven
rules as written therefore produce an **invalid request on the commonest
damaged transcript there is**: Ctrl-C during a tool batch, then resume.

**Rule 2b** drops results orphaned by rule 2. Removing it turns
`TestRepairRule2bDropsResultOrphanedByRule2` red. The bug is reachable only on
the resume path, which no single-process test exercises.

The second is REQ-LOOP-02's wire table, which said "OpenRouter / Ollama:
follow the OpenAI-compatible shape". Only three-quarters true: the
message-per-result shape carries over, the `tool_call_id` keying does not,
because the native Ollama tool message has no id field. Amended in 0.3.2 with
the positional-pairing consequences spelled out.

## Testing

`go test -race ./...` is the default gate. Tests were **mutation-verified**
rather than merely written green — the wrong implementation was introduced and
confirmed to turn the corresponding test red:

| Mutation | Test that caught it |
|---|---|
| Gate iteration on `stop_reason` | `TestIterationOnToolUsePresenceNotStopReason` |
| Per-goroutine abort checks | `TestAbortDuringBatchDoesNotSplitIt` |
| OR-semantics batch termination | `TestBatchTerminationIsAnAndNotAnOr` |
| Remove repair rule 2b | `TestRepairRule2bDropsResultOrphanedByRule2` |
| Estimate on full history | `TestCompactionDoesNotOscillate` |
| Add a cgo dependency | `TestNoCgoOutsideStdlib` |
| Leave the session log unwired | `TestARunIsPersistedAsItHappens` |
| Remove the project trust gate | `TestProjectSkillsAreNotDiscoveredWithoutExplicitTrust` |
| Fall back to a relative path when `HOME` is unresolvable | `TestAnUnresolvableHomeSkipsTheUserTierInsteadOfResolvingRelatively` |

One test **failed to discriminate** and was replaced: the original abort test
cancelled *before* the batch, which correct and broken implementations handle
identically. It now cancels mid-batch, where the broken one runs 1 of 3
handlers.

The dependency gate carries a third test asserting the cgo probe is armed:
with `CGO_ENABLED=0` the toolchain excludes cgo files by build constraint, so
`net` reports 0 cgo files and the check passes while the dependency is present.
A cgo-off gate cannot see the thing it claims to check.

Also included: `FuzzRepairAlwaysSendable` (432k executions clean),
`FuzzSessionLogLoad`, and a cross-target build gate for `linux/amd64`,
`linux/arm64`, `darwin/arm64` and `windows/amd64`.

## What is not built

Stated plainly so nobody reports it as done.

- **MCP client and server.** Requires `mcp-go`, which under the dependency
  policy must live in a nested module, which requires a tagged root release
  first. An ordering constraint, not a defect.
- **Plugins** (§6.6). Skills and project context files now ship, with the
  trust gate; the four plugin categories do not.
- **OpenAI Responses.** It is a separate wire API, not this one with a flag —
  different message model, tool-call identity, reasoning replay and billing.
- **Provider response decoding.** All five providers implement the
  request-building half: `BuildRequest` is exported so the exact bytes can be
  captured offline, but there is no HTTP transport, no SSE/NDJSON decoding, no
  usage/cost computation and no transport retry layer. Consequently
  REQ-PROV-17's streaming-vs-whole byte-identity conformance test cannot be
  written yet — only the encode direction is pinned.
- **Caching levels 2 and 3.** Level 0 (`prompt_cache_key`) and Level 1
  (Anthropic `cache_control` stamping) ship, and the dedup LRU of Level 2
  ships as `CachingMiddleware`; the tool-schema cache and deferred tool
  loading do not.
- **Auth beyond environment keys.** No credential store, no OAuth refresh.
- **`fetch_url` and the SSRF guard.** Image normalization.
- **Wire-level differential testing.** The harness the specification requires
  compares against an independently produced reference, and there is no network
  and no key here to produce one. What ships pins the wire format against
  *regression*, not against *truth* — that distinction is real and the weaker
  claim is the honest one.
- **Benchmarks.** Four numeric performance budgets have no acceptance
  mechanism. Per the specification's own instruction, a budget with no
  benchmark behind it constrains nothing.

The ignore engine is also partial: no global excludes file, no
`.git/info/exclude`, no nested-repository boundaries. `find_files` is
therefore not bit-identical to `fd` or `rg`.

## License

MIT.
