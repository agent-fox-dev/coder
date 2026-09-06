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
| `mcp` | Model Context Protocol, client and server, on the standard library: JSON-RPC, stdio transport, tool pool, HTTP serving with API-key auth. |
| `plugins` | Four plugin categories, registry, manifest discovery, import lint, conformance report. |
| `wire` | Bounded, strict decoder for bytes AgentKit did not produce: hand-rolled scanner, reflective binder, framed reader. |
| `jsonx` | Order-preserving JSON. Decodes once, marshals in slice order at every depth. |
| `schema` | Structured JSON Schema value + typed combinators. No reflection, no codegen. |
| `core` | Canonical vocabulary and every interface seam: messages, content blocks, events, `EventStream`, `Tool`, `ProviderClient`. |
| `catalog` | Embedded model catalog, resolution, sibling-cloning, `max_tokens` and thinking-level clamping. |
| `session` | Append-only JSONL log, damage-tolerant loader, branch tree, resume fold. |
| `skills` | Skill manifests (hand-rolled TOML subset), progressive disclosure, project context files, and the default-off trust gate. |
| `tools` | Built-in tools, path containment, bounded accumulator, process control, glob, a layered gitignore engine, and `fetch_url` behind an SSRF guard. |
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

**MCP is implemented on the standard library, not on `mcp-go`**
([`mcp/`](mcp/)). REQ-MCP-CLIENT-01 names that library and REQ-SEC-11 names
three MCP surfaces where a decoder must reject duplicate keys and bound itself
before allocating. A third-party library owns the wire on all three, and no
general-purpose JSON-RPC implementation rejects duplicate keys — because
JSON-RPC does not ask it to. The two requirements cannot both hold; the PRD is
amended in 0.3.5 with the argument.

Two consequences worth knowing. **Server handlers run concurrently**, because a
handler calling `RequestSampling` waits for a response arriving on the same
transport — serving one request at a time and supporting sampling are mutually
exclusive. And **the 50K result cap is spent across the whole result, not per
item**: a server returning two hundred blocks of 49K each passes a per-item cap
and delivers ten megabytes into the model's context. It counts runes, not
bytes, or a CJK result gets a third of the room an ASCII one does.

**The server advertises `listChanged`, so it sends it**
([`mcp/server.go`](mcp/server.go)). Registering or withdrawing a tool after a
client has connected emits `notifications/tools/list_changed` to every
initialized session. A capability advertised in the handshake and then never
honoured is worse than one never claimed: a client that trusts it caches its
tool list forever. The notification is sent *after* the registry lock is
released, so one wedged client's transport cannot block every registration on
the server.

**Resource URIs are templated** ([`mcp/resources.go`](mcp/resources.go)).
REQ-MCP-SERVER-05's own examples are parameterised —
`nightshift://issues/{number}/triage-report` — which exact-URI registration
cannot express at all; a host would have to register every issue it has ever
seen. A plain `{var}` matches one path segment and `{+var}` is RFC 6570's
reserved expansion and may span `/`. An exact registration always beats a
matching template, or registration order would decide which answers and a
specific registration would become silently unreachable.

**A cancelled request goes unanswered.** `notifications/cancelled` cancels the
handler's context and suppresses its reply, because a response the client has
stopped waiting for looks like an answer to whatever it asked next. The
correlation key encodes the id's *type*, so cancelling the string `"5"` does
not cancel the numeric `5`. Tearing a transport down cancels everything still
in flight before waiting for it — otherwise one slow handler holds the
shutdown open and then writes to a pipe that is already gone.

**A remote server's `endpoint` event may not leave its origin**
([`mcp/httpsse.go`](mcp/httpsse.go)). In the 2024-11-05 transport the server
names the URL its client should POST to. Every POST carries the configured
headers, which is where a bearer token for that server lives — so a server that
could name any origin would be choosing where our credential gets sent, as
traffic that looks exactly like the protocol working. The named URL is resolved
against the stream's own URL and then checked: same scheme, same host, same
port, or we do not send. Relative endpoints (`/messages?sessionId=…`) are the
common case and are precisely what the rule makes safe.

**The remote transports auto-negotiate, but only on the server's own answer.**
`transport` unset POSTs as 2025-03-26 and falls back to 2024-11-05 when the
server rejects the POST with 405/404/400 — the spec's own backwards-
compatibility procedure, so an operator does not have to know which revision a
third-party server implements. A 5xx or a transport failure never triggers the
fallback: that is a working server having a bad day, and silently changing
which revision we speak because of one turns a transient failure into a
permanent misconfiguration. The fallback swaps the transport under a read loop
that is parked inside the old one, so `Receive` carries a generation counter to
tell "the transport under me was exchanged" from "the session ended".

**Pagination cursors name an entry, not an index.** Unregister a tool between
two pages and an index-based cursor silently skips whatever moved into its
place. A cursor for an entry that no longer exists is refused rather than
restarted from the top, and one this server did not issue is refused by its
prefix — cursors are opaque by spec, and a client that guessed the format
should learn that rather than get a page computed from a name it invented.

**A plugin hook can only narrow, never widen**
([`plugins/`](plugins/)). REQ-PLUGIN-04's original ordering put a static
command allowlist ahead of hooks; REQ-SEC-03 removed the allowlist and made the
embedder's `BeforeToolCall` the authorization boundary. So hooks run *after*
it, a call it already refused never reaches a plugin at all, and a hook's
`"allow"` means "no objection" rather than "overrule the host". The first
`"block"` wins and **stops the scan** — a hook that ran after the decision was
made is one whose author will eventually assume it can change it.

**The import restriction is a lint, not a sandbox**, and REQ-SEC-07 says so.
With build-time module linkage plugin code runs in this process with these
privileges. `LintImports` catches a plugin reaching for `agentkit/internal`; it
does not stop one reading your filesystem. It matches the *prefix*, so a
plugin's own `example.com/mine/internal/x` is its own business — rejecting
every path containing "internal" would refuse a plugin for having ordinary Go
structure. And it parses imports only, so a plugin that does not build against
this SDK version is still linted, which is the case where the lint matters
most.

**Duplicate object keys are a rejection, not a resolution**
([`wire/`](wire/)). `encoding/json` silently takes the last one, which hands an
untrusted peer the choice of which of two values AgentKit sees — and the one it
does not see is the one a human reviewing the message read. That is why this
package has its own scanner rather than driving `json.Decoder`: duplicates are
invisible through `Token()`, which reports both.

Two more from the same file. **A `Content-Length` is parsed as `uint64` and
range-checked before it is narrowed** — parse it as `int` on a 32-bit build and
a declared 2³¹ wraps negative, sails past a `> max` check, and panics on a
negative slice bound, which is a remote crash from a header field. And **no
buffer is sized to a declared length**: a peer announcing 16 MiB and sending
one byte must cost one byte, or the number in the header is a free allocation
primitive.

**Field matching is case-sensitive**, unlike `encoding/json`. `id` and `Id` are
two distinct keys, so duplicate-key rejection does not catch them; case-folded
matching then binds both to one field with the last one winning, reintroducing
exactly the last-wins the layer below rejects.

**The audit trail records an arguments HASH, never the arguments**
([`audit.go`](audit.go)). REQ-OBS-05 says hash, and the word is the design: an
audit trail is precisely the artifact that gets shipped to a log aggregator and
retained for years, while tool arguments routinely carry file contents,
credentials and personal data. A hash correlates the same call across sessions
without making the audit log the largest copy of the data it describes.
`server_name` is derived from the REQ-SEC-08 tool-name prefix, so it is right
for any tool following the convention and *empty* — rather than wrong — for one
that is not.

**Session end fires on every exit**, clean, errored or aborted. A hook that
fires only on the happy path is worse than none: an auditor cannot then
distinguish a session that ended badly from one still running, which is the
case they most need to see.

**OAuth refresh is double-checked inside the lock**
([`provider/credentials.go`](provider/credentials.go)). Without the second
check, N concurrent turns arriving on an expired token each POST the same
refresh token, the provider rotates it N times, and N−1 turns are left holding
a credential the provider has already invalidated. The session does not fail
cleanly — it fails N−1 times out of N, intermittently, and reads as a flaky
provider. The first check, outside the lock, exists so the common case (a valid
token) does not serialize turns that have nothing to coordinate.

Two nearby details: the refresh carries **its own timeout** because it holds
the per-vendor lock, and a zero `ExpiresAt` means *never expires*, not *expired
at the epoch* — read the other way it refreshes a plain API key, which has no
refresh flow, on every turn.

**The SSRF guard validates at connect time, not only at resolution**
([`tools/ssrf.go`](tools/ssrf.go)). Checking only the DNS answer leaves a
TOCTOU window a rebind walks straight through: the name resolves to a public
address for the check and to `169.254.169.254` for the connect, and the SDK
fetches the cloud instance credentials on the attacker's behalf. The
`Dialer.Control` check runs on the concrete address the kernel is about to
connect to, and it is the one that actually holds.

Two more that look like details and are not. **Every resolved address must
pass, not merely the one we would have picked** — a name answering with one
public and one private address *is* the attack, and "connect to the first
permitted one" hands over a retry loop. And **an address is unmapped before
classification**: `netip`'s own predicates unwrap `::ffff:10.0.0.1`, but
`netip.Prefix.Contains` never matches across address families, so every range
in the reserved table is reachable through its IPv4-mapped spelling unless you
unmap first.

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

## Performance budgets

Four numeric budgets now have a benchmark **and a threshold test that fails the
build** — a `Benchmark` function alone prints a number nobody reads, and
NFR-PERF-09 is explicit that a budget which cannot fail does not constrain
anything.

| Budget | Measured | Threshold |
|---|---|---|
| NFR-PERF-01 loop overhead per turn | ~48 µs | < 1 ms |
| NFR-PERF-06 cache hit over a direct return | ~0.8 µs | < 0.5 ms |
| NFR-PERF-07 `cache_control` stamping, 128 tools / 1000 messages | ~47 ns | < 1 ms |
| NFR-PERF-03 schemas serialized on a steady-state request | 0 | 0 |

NFR-PERF-03 is asserted as a **count**, not a duration. "Computed once per
session" has an exact answer, and a count does not flake on a busy runner.
NFR-PERF-04 (true concurrency) and NFR-PERF-05 (first token before the body
ends) are structural, and are pinned by tests that **deadlock** rather than by
stopwatches — the broken implementation cannot reach the assertion at all.

The budget file is `//go:build !race`: the detector inflates every measurement
by roughly an order of magnitude, so a threshold loose enough to survive it is
too loose to catch anything.

**Writing these found two things.**

REQ-CACHE-06's tool-schema cache was implemented, unit-tested, and attached to
no provider — so every request re-serialized every schema, ~0.9 ms of a ~1.4 ms
build at 128 tools. Same shape as the session log that was built, tested and
never written to. It is wired now, and `TestASteadyStateRequestSerializesNoSchemas`
is the assertion that was missing.

NFR-PERF-01's "per turn" is under-specified. The REQ-GO-15 estimate and the
REQ-PROV-11 repair pass are both O(history), so a turn 500 messages deep costs
~1.7x a first turn. Both are well inside the budget; the depth term is reported
by `BenchmarkLoopTurnAtDepth` and deliberately given **no** threshold, because
inventing one the requirement does not state is the unenforceable rigour
NFR-PERF-09 objects to.

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
| Classify an address without unmapping 4-in-6 | `TestIPv4MappedIPv6IsUnmappedBeforeClassification` |
| Validate only the first resolved address | `TestTheGuardRefusesEveryResolvedAddressNotJustTheFirst` |
| Skip per-hop scheme re-validation | `TestARedirectToHTTPIsBlockedWhenHTTPIsNotAllowed` |
| Read the whole body, then truncate | `TestTheResponseIsCappedAt512KB` |
| Keep caller headers across a cross-host redirect | `TestCallerHeadersAreDroppedOnACrossHostRedirect` |
| Unwire the schema cache from the request path | `TestTheProviderOwnsAPrefixByDefault` |
| Buffer the response body before decoding | `TestFirstTokenIsEmittedBeforeTheStreamEnds` |
| Run tool handlers sequentially | `TestParallelToolsUseTrueConcurrency` |
| Drop the double check inside the refresh lock | `TestConcurrentTurnsRefreshExactlyOnce` |
| Use a bare expiry check with no validity floor | `TestTheValidityFloorRefreshesBeforeExpiry` |
| Take one global lock instead of a keyed one | `TestPerVendorLocksDoNotSerializeDifferentVendors` |
| Never release a refcounted lock entry | `TestVendorLocksAreReleased` |
| Rewrite a stored bearer into an API-key header | `TestACredentialStoreReachesEveryWire` |
| Resolve duplicate keys last-wins | `TestDuplicateKeysAreRejected` |
| Parse a `Content-Length` as `int` | `TestAContentLengthIsRangeCheckedInUint64` |
| Pre-allocate to a declared frame length | `TestNoBufferIsPreAllocatedToADeclaredSize` |
| Resynchronize after a malformed frame | `TestAReaderIsPoisonedByItsFirstMalformedFrame` |
| Ignore an unknown property | `TestAnUnknownPropertyIsARejection` |
| Match struct fields case-insensitively | `TestFieldMatchingIsCaseSensitive` |
| Accept an integer past the safe-integer range | `TestIntegersOutsideTheSafeRangeAreRejected` |
| Let an explicit `null` reach the reflective setter | `TestExplicitNullNeverPanics` |
| Validate only the root struct | `TestTheValidatorHookRunsPerStructAtItsOwnPath` |
| Keep the first registration on a name collision | `TestALaterRegistrationWins` |
| Continue the hook scan past a block | `TestTheFirstBlockWinsAndStopsTheScan` |
| Run the plugin gate ahead of the interceptor | `TestAPluginHookCannotOverturnTheInterceptor` |
| Flag any import path containing "internal" | `TestAPluginsOwnInternalPackageIsAllowed` |
| Require plugin source to compile before linting it | `TestTheLintReadsFilesThatDoNotCompile` |
| Skip the "could not lint" report | `TestAManifestWithNoSourceIsReportedRatherThanPassed` |
| Let an MCP subprocess inherit the parent environment | `TestAStdioServerRunsAsASubprocessWithAReducedEnvironment` |
| Cap MCP results per item rather than per result | `TestResultsAreCappedAcrossTheWholeResult` |
| Count the result cap in bytes | `TestTheCapCountsRunesNotBytes` |
| Skip the MCP tool-name collision check | `TestAShadowedNativeToolIsRefusedAtConnectionTime` |
| Answer sampling without the per-server gate | `TestSamplingIsRefusedUnlessEnabledAndAlwaysAudited` |
| Serve MCP over HTTP with no API key | `TestHTTPModeRequiresAnAPIKey` |
| Check the HTTP method before authenticating | `TestAuthenticationRunsBeforeTheMethodCheck` |
| Resynchronize after a malformed MCP frame | `TestAMalformedFrameTearsTheConnectionDown` |
| Answer a method before the MCP handshake completes | `TestAMethodBeforeInitializeIsRefused` |
| Always answer `initialize` with our newest version | `TestTheServerEchoesAProtocolVersionItSpeaks` |
| Advertise `listChanged` without ever sending it | `TestRegisteringAToolNotifiesConnectedClients` |
| Notify a session that has not finished initializing | `TestNoNotificationBeforeTheHandshakeCompletes` |
| Answer a request the client cancelled | `TestACancelledRequestStopsTheHandlerAndGoesUnanswered` |
| Key a cancellation on the id's value, not its type | `TestAStringAndANumericRequestIDDoNotCollideWhenCancelling` |
| Leave in-flight handlers running through a shutdown | `TestShutdownCancelsInFlightHandlers` |
| Let a `{var}` swallow a path separator | `TestATemplateVariableDoesNotSpanASlash` |
| Let a template shadow an exact resource registration | `TestAnExactResourceWinsOverAMatchingTemplate` |
| Resume a listing AT the cursor's entry | `TestListingPagesAndResumesAfterTheCursor` |
| Accept a cursor this server never issued | `TestAFabricatedCursorIsRefused` |
| Bind MCP HTTP mode to every interface | `TestHTTPModeBindsLoopback` |
| POST where a server's `endpoint` event points, off-origin | `TestAnEndpointOnAnotherOriginIsRefused` |
| Compare endpoint origins without the port | `TestAnEndpointOnAnotherOriginIsRefused` |
| Fall back to the older revision on a 5xx | `TestAutoDoesNotSwitchRevisionOnAServerError` |
| Kill the session when the auto transport swaps | `TestAutoFallsBackToTheOlderRevision` |
| Echo an unbounded server-chosen session id | `TestAnOversizedSessionIDIsRefused` |
| Treat a 404 for a live session as any other error | `TestA404ForAKnownSessionIsDistinguishable` |
| Treat 405 on the standalone GET as fatal | `TestAServerWithoutAStandaloneStreamStillWorks` |
| Read an SSE-answered POST as JSON | `TestStreamableHTTPAcceptsAnSSEAnsweredPOST` |
| Buffer an unbounded `data:` field | `TestAnOversizedSSELineIsRefused` |
| Dispatch an event the stream ended in the middle of | `TestAStreamThatEndsMidEventIsAnError` |
| Send `Bearer ` when the token variable is unset | `TestAHeaderThatResolvesToNothingIsDroppedNotSentBlank` |
| Let an interpolated secret smuggle a header | `TestAHeaderCarryingAControlByteIsRefused` |
| Accept a non-http scheme as a transport | `TestOnlyHTTPSchemesAreTransports` |
| Start HTTP mode when `api_key_env` is unset | `TestHTTPModeRefusesToStartWithoutAKey` |
| Fall back to stdio on an unknown transport | `TestRunRejectsAnUnknownTransport` |
| Walk past an array of tables resolving `[a.b]` | `TestASubTableUnderAnArrayElementBelongsToThatElement` |
| Expose MCP tools unqualified | `TestMCPToolsAreGatedByQualifiedNameEverywhere` |
| Adapt an MCP tool without its server name | `TestAnMCPToolCallIsAuditedWithItsServerName` |
| Record tool arguments instead of their hash | `TestTheAuditTrailHashesArgumentsRatherThanRecordingThem` |
| Fire session-end only on a clean run | `TestSessionStartAndEndFireOnEveryExit` |
| Infer an MCP server from an unprefixed tool name | `TestMCPServerOf` |
| Let a panicking observer unwind the run | `TestAPanickingAuditHookDoesNotTakeTheRunWithIt` |
| Remove the project trust gate | `TestProjectSkillsAreNotDiscoveredWithoutExplicitTrust` |
| Fall back to a relative path when `HOME` is unresolvable | `TestAnUnresolvableHomeSkipsTheUserTierInsteadOfResolvingRelatively` |

Twelve attempts **failed to discriminate**, which is worth stating because a
mutation that does not distinguish the two implementations proves nothing
about the test. Four came from one sitting on the SSRF guard, and each was a
test that passed for a reason other than the one it claimed:

| It looked like it tested | It actually passed because |
|---|---|
| 4-in-6 unmapping | the addresses chosen were ones `netip` already unwraps |
| an https→http redirect refusal | the https URL pointed at a plain-HTTP server, so hop 1 died in the handshake |
| the 512 KB read cap | slicing after an unbounded read produces an identical body |
| header stripping across hosts | both hops dialled the same server, so the second never happened |

All four were rewritten to fail against their mutation. Two more came from the
audit work: a `MCPServerOf` table with no local tool name containing the
separator, and a panic mutation that still called `recover()` and so still
swallowed the panic it was meant to release. The ninth measured `HeapAlloc`
after a `runtime.GC()` to catch a 16 MiB pre-allocation that was already
garbage by the time it looked — `TotalAlloc` is the instrument that survives.
The last two were the harness's fault rather than the tests': one replaced the
phrase `parser.ImportsOnly` where it first appeared, which was inside a doc
comment, and one added a second call to the plugin gate without moving the
first. Neither implemented the bug it was named after.

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
`FuzzSalvageAlwaysProducesValidJSON` (1.6M clean),
`FuzzGuardNeverPanics` (2.3M) and `FuzzBindNeverPanics` (2.9M),
`FuzzSessionLogLoad`, and a cross-target build gate for `linux/amd64`,
`linux/arm64`, `darwin/arm64` and `windows/amd64`.

## What is not built

Stated plainly so nobody reports it as done.

- **OpenAI Responses.** It is a separate wire API, not this one with a flag —
  different message model, tool-call identity, reasoning replay and billing.
- **Image normalization** (REQ-TOOL-14).
- **Reference bodies for the differential harness.** The harness itself ships
  ([`difftest/`](difftest/), a separate module) and its own suite is
  mutation-verified. It has no scenarios, because NFR-TEST-06.3 forbids
  hand-authoring a reference — a hand-authored expectation encodes the same
  mental model as the code under test — and producing a real one needs a
  vendor SDK or a live key. So `go run ./cmd/difftest` reports **DARK** and
  exits 1, which is NFR-TEST-07.3's required answer rather than a bug. The
  unit suite pins the wire format against *regression*; only a reference pins
  it against *truth*, and the weaker claim is the honest one until then.
- **SSE stream resumption.** All three of REQ-MCP-CLIENT-02's transports ship
  — stdio against a real subprocess, and both HTTP revisions against real
  `httptest` servers. What is *not* built is reconnecting a dropped event
  stream with `Last-Event-ID`: an optional part of the 2025-03-26 spec that
  needs a retry policy and duplicate suppression to be worth anything. The
  decoder therefore parses `id:` and discards it, rather than storing an id it
  would never send and implying support that is not there.
- **Plugin implementations.** The four categories, the registry, discovery, the
  lint and `validate-plugins` ship; no first-party plugin does. That is the
  intended shape — a plugin is the embedder's code — but it means the
  categories have no in-tree user yet.
- **`docs/PROVIDERS.md`** (NFR-COMPAT-07): the ledger of pinned API versions
  and capture dates. It has nothing to record until the harness has a corpus.

`search_files` (REQ-TOOL-05's grep tool, with `rg --json` acceleration) is not
built. The ignore engine underneath it is, and `find_files` uses it.

## License

MIT.
