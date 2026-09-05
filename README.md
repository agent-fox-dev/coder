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

To talk to a real model, register a wire API on the config. Nothing is
registered by import side effect, so the root package never drags `net/http`
into a consumer that only wants the loop:

```go
reg := agentkit.DefaultProviders()
reg.Register(anthropic.Provider(anthropic.Options{}))

cfg := core.AgentConfig{Model: model, Providers: reg}   // credential per REQ-AUTH-03
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
| `tools` | Built-in tools, path containment, bounded accumulator, process control, glob, and a layered gitignore engine. |
| `provider` | Send-time transcript repair, HTTP transport + retry, credential resolution, header precedence, cost arithmetic, SSE decoding — everything shared by every wire API. |
| `provider/{anthropic,openai,google,ollama,faux}` | One wire API each, encode and decode. |
| `difftest` | Separate module: the NFR-TEST-06/07 differential harness — canonicalizing comparator, key-order side channel, divergence ledger, exit machine. |
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

**A tool added mid-session is declared after the prefix, not prepended to it**
([`provider/toolcache.go`](provider/toolcache.go)). Prepending a newly
discovered tool invalidates the provider-side cache over the entire
transcript — on the turn an MCP server connects, which is when the transcript
is longest. `SplitDeferredTools` is a single forward pass and later usage
cannot un-defer a tool: a tool used on the turn *after* it appeared is the
normal case, and un-deferring there promotes it exactly when promotion costs
most.

**Ignore rules are layered, and a nested repository is its own root**
([`tools/ignore.go`](tools/ignore.go)). A deeper `.gitignore` overrides a
shallower one; a vendored dependency that is itself a git checkout does not
inherit the outer project's rules. Without the boundary, a rule the outer
project wrote about *its* build output silently deletes files from the listing
of a repository that has never heard of it.

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

## Four wires, one conformance suite

Anthropic, OpenAI, Google and Ollama all decode into the same canonical
message, and [`provider/conformance_test.go`](provider/conformance_test.go)
runs one set of fixtures — the same logical turn, four dialects — through all
of them. The requirements it pins are stated once about "a provider", so they
are tested once against every provider rather than four times in four
dialects.

The traps it catches are per-wire and would each pass a single-provider suite:

**Cached tokens are netted out of input exactly once.** The OpenAI family and
Google report a prompt total *inclusive* of cached tokens and must subtract;
Anthropic reports input *exclusive* of them and must not. Both mistakes are
silent and comparable in size — one overstates cost by up to ~90% on a
well-cached loop, the other understates it by the same — so the netting lives
in each decoder and the outcome is pinned centrally.

**The cached count lives in three places.** `prompt_tokens_details.cached_tokens`
(OpenAI, OpenRouter), `prompt_cache_hit_tokens` (DeepSeek), a top-level
`cached_tokens` (Moonshot). Reading only the first is correct on OpenAI and
silently full-prices every cached token on the two vendors whose caching is the
reason to use them.

**Gemini reports reasoning tokens beside output, not inside it.** Copying that
shape through under-reports output by exactly the reasoning volume — the more
the model thinks, the larger the error.

**Ollama and Gemini synthesize tool-call ids.** Neither wire carries one;
results pair positionally. The streaming and whole-response paths must
synthesize the *same* id, or a replayed transcript stops matching its own
results.

**Ollama reports failure inside a 200.** A top-level `error` string on an
otherwise ordinary chunk. The transport layer cannot see it.

## Testing

`go test -race ./...` is the default gate for the root module;
`cd difftest && go test ./...` for the harness, which is its own module. Tests were **mutation-verified**
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
| Emit block-end events from the chunk handler | `TestBlockEndEventsFollowTheWholeStream` |
| Re-encode decoded tool arguments | `TestStreamingAndWholeResponseAgreeByteForByte` |
| Apply OpenAI's cache-token netting to Anthropic | `TestAnthropicInputTokensAreNotNettedAgain` |
| Drop header deletion markers per layer | `TestNilHeaderValueSuppressesAProviderDefault` |
| Clamp an overlong `Retry-After` instead of abandoning | `TestOverlongServerDelayIsAbandonedNotClamped` |
| Close a truncated JSON string instead of dropping it | `TestATruncatedMemberIsDroppedNotClosed` |
| Dispatch a partially accumulated SSE event at EOF | `TestATruncatedFinalEventIsDiscardedNotDispatched` |
| Skip the cache-token subtraction on OpenAI | `TestCachedTokensAreNettedOutOfInputExactlyOnce` |
| Read only the nested `cached_tokens` arm | `TestTheCachedCountIsReadFromAllThreePlaces` |
| Report Gemini thoughts beside output | `TestGeminiThoughtsAreInsideOutputNotBesideIt` |
| Continue one text block across a Gemini function call | `TestTextAfterAToolCallStartsANewBlock` |
| Ignore Ollama's error string inside a 200 | `TestOllamaReportsErrorsInsideA200Body` |
| Synthesize tool-call ids from a global counter | `TestStreamingAndWholeResponsesAgree` |
| Load only the root `.gitignore` | `TestFindFilesHonoursANestedGitignore` |
| Ignore the nested-repository boundary | `TestANestedRepositoryIsItsOwnIgnoreRoot` |
| Consult XDG before `core.excludesFile` | `TestGlobalExcludesResolutionOrder` |
| Evaluate ignore layers deepest-first | `TestADeeperGitignoreOverridesAShallowerOne` |
| Un-defer a tool on later usage | `TestLaterUsageCannotUnDeferATool` |
| Treat a tool addition as a prefix invalidation | `TestAddingAToolDoesNotInvalidateThePrefix` |
| Trust schema pointer identity alone | `TestRebuildingAnIdenticalToolDoesNotInvalidate` |
| Stamp the breakpoint on a deferred tool | `TestADeferredToolIsDeclaredAfterThePrefixAndCarriesNoBreakpoint` |
| Credit a cache hit a session average | `TestALevel2HitCreditsWhatThatResponseActuallyCost` |
| Launder JSON numbers through `float64` | `TestNumberLiteralsAreNotNormalized` |
| Treat an explicit `null` as absent | `TestNullVersusAbsentIsADifference` |
| Sort arrays before comparing | `TestArrayOrderIsNeverNormalized` |
| Let a ledger entry cover any kind at its path | `TestClassificationAndStaleEntries` |
| Treat a stale ledger entry as clean | `TestAStaleLedgerEntryExitsThree` |
| Report a dark run as a pass | `TestADarkRunPrintsNoTally` |
| Remove the project trust gate | `TestProjectSkillsAreNotDiscoveredWithoutExplicitTrust` |
| Fall back to a relative path when `HOME` is unresolvable | `TestAnUnresolvableHomeSkipsTheUserTierInsteadOfResolvingRelatively` |

Two attempts **failed to discriminate**, which is worth stating because a
mutation that does not distinguish the two implementations proves nothing
about the test.

The original abort test cancelled *before* the batch, where correct and broken
implementations behave identically. It now cancels mid-batch, where the broken
one runs 1 of 3 handlers.

The first id-synthesis mutation changed the streaming and whole-response paths
identically, so the test that compares them stayed green. Replacing it with a
global counter — the shape the real bug takes — turned it red.

The dependency gate carries a third test asserting the cgo probe is armed:
with `CGO_ENABLED=0` the toolchain excludes cgo files by build constraint, so
`net` reports 0 cgo files and the check passes while the dependency is present.
A cgo-off gate cannot see the thing it claims to check.

Also included: `FuzzRepairAlwaysSendable` (432k executions clean),
`FuzzSalvageAlwaysProducesValidJSON` (1.6M clean), `FuzzSessionLogLoad`, and a
cross-target build gate for `linux/amd64`,
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
- **`CredentialStore` and OAuth refresh** (REQ-AUTH-05/06). The ordered
  per-vendor environment table, the three-valued credential state and the
  redaction boundary ship; the application-owned store and the
  refresh-under-lock do not.
- **`fetch_url` and the SSRF guard.** Image normalization.
- **Reference bodies for the differential harness.** The harness itself ships
  ([`difftest/`](difftest/), a separate module) and its own suite is
  mutation-verified. It has no scenarios, because NFR-TEST-06.3 forbids
  hand-authoring a reference — a hand-authored expectation encodes the same
  mental model as the code under test — and producing a real one needs a
  vendor SDK or a live key. So `go run ./cmd/difftest` reports **DARK** and
  exits 1, which is NFR-TEST-07.3's required answer rather than a bug. The
  unit suite pins the wire format against *regression*; only a reference pins
  it against *truth*, and the weaker claim is the honest one until then.
- **`docs/PROVIDERS.md`** (NFR-COMPAT-07): the ledger of pinned API versions
  and capture dates. It has nothing to record until the harness has a corpus.
- **Benchmarks.** Four numeric performance budgets have no acceptance
  mechanism. Per the specification's own instruction, a budget with no
  benchmark behind it constrains nothing.

`search_files` (REQ-TOOL-05's grep tool, with `rg --json` acceleration) is not
built. The ignore engine underneath it is, and `find_files` uses it.

## License

MIT.
