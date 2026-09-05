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
prints five behaviours that the specification originally got wrong.

## Status

This implements the core of [`agent-kit-prd.md`](agent-kit-prd.md) v0.3.0.
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
| `tools` | Built-in tools, path containment, bounded accumulator, process control, glob + ignore. |
| `provider` | Send-time transcript repair shared by every wire API. |
| `provider/{anthropic,openai,faux}` | One wire API each. |
| `.` (root) | `Agent`, the loop, the batch executor, stop policies, the argument pipeline, compaction. |

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
Half right: it is an Anthropic **wire** rule, and on OpenAI the single-message
form is not representable at all. `TestToolResultShapeAsymmetry` hands one
transcript to both providers and gets **3 Anthropic messages and 6 OpenAI
messages** out.

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

## One correction to the specification

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
- **Skills, project context files, plugins.** `TrustProject` exists on the
  config and gates nothing yet.
- **Three of five wire APIs**: OpenAI Responses, Google, Ollama. Constants are
  reserved.
- **Caching levels 0, 2 and 3.** Anthropic `cache_control` stamping ships
  (it is the dominant cost); the dedup LRU, schema cache and deferred tool
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
