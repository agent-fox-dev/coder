# Requirement audit — PRD 0.4.1

A requirement-by-requirement comparison of `agent-kit-prd.md` against the
code, made on 2026-09-06. Every row is one finding: what the requirement
says, what the code did, and what happened about it. `Fixed` names the test
that now pins the behaviour. `Deferred` is a decision, recorded here so it is
not rediscovered as a surprise; each deferred row says what it would take.

Two rules governed the pass. A requirement the code contradicts is a bug and
was fixed in this cycle unless the fix is a subsystem rather than a change.
A requirement the PRD contradicts itself on was resolved in the PRD, with the
reasoning, per the document's own practice (0.3.1, 0.3.2, 0.3.5, 0.4.0).

## Loop, lifecycle, extension axes

| Req | Finding | Status |
|---|---|---|
| NFR-REL-02 | Panics in middleware, `StopPolicy`, `TransformContext`, `Tool.PrepareArguments` and `Tracer` crashed the process; only hooks, interceptors and handler bodies were wrapped. | Fixed — `TestPanicsInThirdPartyCodeDoNotCrashTheProcess`. Ruling: a panicking `StopPolicy` STOPS the run (a limit that fails open is an unbounded bill); the others are contained and the run continues. The run goroutine carries a backstop that leaves the REQ-LOOP-09 terminal marker. |
| REQ-LOOP-09 / 16 | A cancellation landing during a tool batch was followed by one more provider call on a dead context, recording a content-free aborted turn; `Continue` then refused the transcript as "a completed assistant turn". | Fixed — `TestACancelledBatchEndsTheRunAtTheTurnBoundary`, `TestContinueAfterAnAbortedTurnIsAllowed`. PRD REQ-LOOP-16 amended. |
| REQ-LOOP-16 | `Continue` with only a follow-up queued sent the assistant-terminated transcript first and delivered the follow-up a turn late. | Fixed — `TestContinueWithOnlyAFollowUpQueuedDeliversItFirst`. |
| REQ-GO-09 | Caller-ctx cancellation returned `ErrAborted`, indistinguishable from `Agent.Abort()`. | Fixed — `TestAbortAndCallerCancellationAreDistinguishable`. |
| REQ-LOOP-09 | `Agent.Abort()` mid-stream cleared `error_message` on the aborted message (and wrote through the provider stream's own value). | Fixed — `TestAnAbortedMessageKeepsItsErrorMessage`. |
| REQ-LOOP-11.3 | Aborted calls emitted an execution End with no Start; blocked/invalid/unknown calls emitted neither. | Fixed — `TestEveryCallOpensAndClosesExactlyOnce`. Start now fires in the sequential prepare phase, per REQ-LOOP-05 phase 1. |
| REQ-LOOP-11.2 | The prepare loop never checked `ctx.Err()`, so interceptors still ran for the remaining calls after cancellation. | Fixed (same test). |
| REQ-LIFE-03 / REQ-SESS-03 | `runLoop` and `callModel` read `cfg.Model` without the lock while `SetModel` wrote it under the lock — a real race. | Fixed — `TestSetModelDuringARunDoesNotRace` under `-race`. |
| REQ-OBS-06 | `TurnEndEvent.ToolResults` was nil for the `OnTurnEnd` hook and `StopContext` (only the stream copy was rebuilt non-nil). | Fixed — `TestTurnEndToolResultsAreNonNilForHooksToo`. |
| REQ-OBS-06 | A deferred response emitted `TurnStart` with no `TurnEnd`. | Fixed — `TestDeferredResponseStillEmitsTurnEnd`. |
| REQ-OBS-06c | No discriminated JSON union for events. | Fixed — `core.MarshalEvent` + `agentkit.EventJSON`; `TestEventsSerializeAsADiscriminatedUnion`. Messages inside events are encoded by the session codec, the one lossless encoder, not a second copy. |
| REQ-OBS-03 | `EventHookPlugin.OnSessionStart/End` were never invoked; only the config's own hooks fired. | Fixed — `TestPluginSessionHooksFire`. |
| REQ-GO-14 | No turn split: a cut landing on an assistant message summarized the whole prefix as one block. | Fixed — `summarizeWithSplit`, `CompactionDeps.TurnSummarizer`, `ModelTurnSummarizer` (distinct prompt, half budget), `CompactionSplitSeparator` pinned in the wrappers golden; `TestACutInsideATurnSummarizesTheTurnSeparately`. |
| REQ-GO-12.3 | The summarization request carried no session id of its own and no `min(0.8 × reserve, model.max_tokens)` clamp. | Fixed — `ModelSummarizer` takes the reserve; `summaryMaxTokens`. |
| REQ-MULTI-05 | No registry of agent definitions by name. | Fixed — `AgentDefinition`, `AgentRegistry`, `NewAgentFromDefinition`; `TestNamedSpecialistsAreInvokableByName`. |
| REQ-CAT-07 | No `Agent`-level accessor for the resolved model. | Fixed — `Agent.ResolvedModel()`. |
| REQ-LIFE-02 | `Snapshot` read `Revision` and the messages under two separate locks, so the revision could lag its messages. | Fixed — `ConversationHistory.SnapshotBranch`; `TestSnapshotRevisionMatchesItsMessages`. |
| NFR-REL-02.1 | The documented lock order ("a.mu never under the batch mutex") was false: image normalization and `AfterToolCall`'s panic path reached `a.fireError` inside the finalize section. | Fixed — the batch carries a lock-free reporter built from the config copy. |
| §5 / REQ-SKILL-12.5 | `AgentConfig.TrustProject` was a dead field nothing outside `core` read; the required "untrusted default through the real constructor, absent from the assembled prompt" test existed only inside the skills package. | Fixed — `SkillsConfigFor` derives skills discovery from the agent config; `TestAnUntrustedProjectSkillNeverReachesTheAssembledPrompt` runs both arms through `NewAgent` and asserts on the prompt the provider received. |
| OQ-8 | Unresolved: `execute` registered with no interceptor ran an unrestricted shell by omission. | Resolved as recommended — `ErrUnguardedExecute`, `AllowAllToolCalls`, `RestrictedPolicy`; `TestAShellToolWithNoInterceptorFailsTheRun`, `TestRestrictedPolicy`. PRD OQ-8 marked resolved. |
| — | `TestAbortFromAnotherGoroutine` skipped on every run (the scripted double ignores ctx) and pinned nothing; `TestAbortedBatchStillProducesAResultPerCall` asserted results, not events. | Fixed — a ctx-honouring double (`blocking`) and event-counting assertions. |
| REQ-TOOL-13.2 | The abort path discarded termination votes cast by calls blocked in prepare. | Deferred by ruling: an aborted batch did not finish, so it does not finish the run on a tool's say-so; the run ends as aborted regardless. Documented in `batch.go`. |

## Built-in tools

| Req | Finding | Status |
|---|---|---|
| REQ-SEC-01 / NFR-SEC-02 | `write_file` escaped the workspace through a DANGLING symlink: the parent resolved inside the root, the base was rejoined, and `os.WriteFile` followed the link. | Fixed — containment follows a dangling link's target (bounded at 40 hops), and every write Lstats the final path under the lock immediately before opening; `TestDanglingSymlinkCannotEscapeTheWorkspace`, `TestCheckWriteTargetRefusesALinkPlantedAfterResolve`. |
| REQ-TOOL-09b | Truncation markers named calls that could not work: `limit=` values the tool clamps back, `Use limit=` on `search_files` whose parameter is `max_matches`, a `read_file` byte marker with no offset that was itself sliced off. | Fixed — `TestCapMarkersNameACallThatWorks`, `TestSearchMarkerNamesMaxMatches`, `TestReadFileByteCutIsOnWholeLinesWithARealOffset`. |
| REQ-TOOL-09c | `LongLineMarker` existed and had no callers. | Fixed — `TestReadFileLongLineGetsTheSedMarker`. |
| REQ-TOOL-09 / 15.3 | `search_files` had no byte cap (100 matches × context × 500 chars can be megabytes). | Fixed — `TestSearchAppliesTheByteCap`. |
| REQ-TOOL-17.5 | Post-exit drain is a fixed `WaitDelay`, not a re-arming idle timer. | Deferred — needs the tool to own the pipe and copy on a goroutine that resets an idle timer per read; the current 2 s backstop cuts a detached descendant that keeps writing. |
| NFR-COMPAT-06 | Signal death reported `exit_code = -1`, not `128+signum`. | Fixed — `TestSignalKilledProcessReportsExitCode128PlusSignum`. |
| REQ-TOOL-06 | `execute` did not validate `timeout_s`; a negative value meant "no timeout". | Fixed — `TestExecuteRejectsANonPositiveTimeout`. |
| REQ-LOOP-12 | The "two spellings" test compared paths that `filepath.Join` cleans to the same string and drove the lock table directly; `lockKey` swallowed every resolution error. | Fixed — the test runs two concurrent `edit_file` calls through a symlink and a relative spelling; only not-exist falls back. |
| REQ-TOOL-04 | `find_files` had no `file_type`; `read_file` counted lines off by one, sliced mid-rune, and loaded the whole file before applying its limits; `edit_file` used a non-overlapping count for uniqueness (`"aa"` in `"aaa"` was "unique"). | Fixed — `TestFindFilesFileType`, `TestReadFileCountsLinesLikeAnEditor`, `TestReadFileStreamsInsteadOfLoadingTheFile`, `TestCapLineCutsOnRuneBoundaries`, `TestOverlappingOccurrencesAreNotUnique`. |
| REQ-TOOL-09d | Spill was off unless `SpillDir` was set, and nothing set it. | Fixed — defaults to a per-workspace directory under the OS temp dir; `DisableSpill` opts out; spill files are the embedder's to clean; `TestExecuteSpillsByDefault`. |
| REQ-SEC-08 | The reduced environment stripped fifteen provider prefixes and passed `GITHUB_TOKEN`, `NPM_TOKEN`, `*_SECRET`…; tools built outside `All()` inherited the full environment. | Fixed — generic `_TOKEN/_SECRET/_API_KEY/_PASSWORD/_CREDENTIALS` suffix stripping; nil `Env` defaults everywhere; `TestReducedEnvStripsGenericCredentialSuffixes`, `TestExecuteBuiltOutsideAllReducesTheEnvironment`. |
| NFR-TEST-04 | `find_files`/`search_files` read the developer's real global git excludes and spawned `git config` per call. | Fixed — `IgnoreOptions` threaded through `Options`, `NoGlobalExcludes()`, per-workspace memoization. Found in passing: the ripgrep backend never applied the global excludes layer at all (it ran with an empty env); it now receives `--ignore-file`. |
| REQ-TOOL-14 | `read_file` refused WebP outright because the normalizer cannot decode it. | Fixed — forwarded untouched (dimensions unknown), refused only over the base64 budget; `TestReadFileForwardsWebP`. |
| — | `run_command` resolved a relative program against the process cwd, not the workspace. | Fixed — `TestRunCommandResolvesARelativeProgramAgainstTheWorkspace`. |
| — | `list_files` put its truncation marker inside `entries` as if it were a filename. | Fixed — moved to `note`. |

## Providers

| Req | Finding | Status |
|---|---|---|
| REQ-CAT-04 | `max_tokens` was never clamped: `ClampMaxTokens` had no callers, every adapter forwarded the config value verbatim, and `req.EstContextTokens` was computed and unused. Anthropic could send `max_tokens: 0` and `messages: null`. | Fixed in every adapter — `TestMaxTokensIsClampedDeepIntoATranscriptOnEveryWire`, `TestMaxTokensIsNeverZeroAndMessagesNeverNull`. |
| REQ-PROV-15 | Thinking levels were never clamped; OpenAI passed `xhigh`/`max` through verbatim (a 400); Anthropic emitted `{"type":"enabled"}` with no `budget_tokens` for an effort-style row; Google fell back to a default table instead of clamping. | Fixed — `TestReasoningEffortIsClampedNeverPassedThrough`, `TestThinkIsClampedAgainstTheRowsMap`, `TestAnEffortStyleRowIsPricedIntoABudget`, `TestAnUnpricedLevelIsClampedNotDefaulted`. |
| REQ-LOOP-09 / REQ-PROV-14 | Mid-stream cancellation produced `stop_reason: "error"`, not `"aborted"`, on all five wires; only a transport-level failure recognised `ctx.Err()`. A per-request `TimeoutMs` expiry was reported as an abort (terminal, never retried). | Fixed — `TestMidStreamCancellationIsAnAbortNotAnErrorOnEveryWire`, `TestAPerRequestTimeoutMidStreamIsARetryableErrorOnEveryWire`. |
| REQ-PROV-12 | The catalog's compat rows used keys the adapter never read (`max_tokens_field`, `supports_temperature`, …), so every override was silently ignored; the profile was not inferred from the host, so `store:false` and the `developer` role went to every gateway; six flags from the table were missing. | Fixed — rows and struct share one vocabulary and an unknown key fails loudly; the profile is inferred from `Provider`+`BaseURL` before row overrides; `SupportsTemperature`, `SupportsLongCacheRetention`, `SupportsFinishReason` wired. `TestTheProfileIsInferredFromTheHost`, `TestTheCatalogRowOverridesTheInferredProfileKeyByKey`, `TestAnUnknownCompatKeyFailsLoudly`, `TestEveryCatalogRowIsUnderstood`, `TestARowThatRejectsTemperatureOmitsIt`, `TestAnUntrustedFinishReasonIsInferredFromContent`, `TestCacheControlAndItsTTLFollowTheProfile`. Still absent: `ThinkingFormat` (DeepSeek `reasoning_content` echo), `ThinkingTokenBudgetField`, `AllowsUserAfterToolResult`, `CacheControlFormat` (OpenRouter `anthropic/*` caching) — deferred, each needs a named vendor and a reproducing case per the requirement's own rule. |
| REQ-TOOL-03 | `strict:true` was emitted on the RAW schema; `schema.StrictSubset` had no callers; `prefer`/`require` were never read. | Fixed — the rewrite is probed first, `prefer` falls back, `require` fails the request; `TestStrictRidesOnlyOnTheRewrittenSchema`. |
| REQ-LOOP-02 | An empty Anthropic tool result serialised as `{"type":"text"}` with no `text` key. | Fixed — `TestAnEmptyToolResultOmitsContent`. |
| REQ-PROV-05.6 | The Responses adapter priced with the configured service tier, not the served one it had already decoded. | Fixed — `TestTheServedTierIsBilledNotTheConfiguredOne`. |
| REQ-PROV-13 | The `MaxRetryDelayMs` ceiling was applied before retryability, so a non-retryable 400 carrying `Retry-After: 3600` was discarded as `ErrRetryDelayTooLong`. | Fixed — `TestANonRetryableResponseIsReturnedWhateverItsRetryAfter`. |
| REQ-PROV-11 rule 5 | Tool-call ids were normalised on every transcript, not only across models. | Fixed — `TestSameModelToolCallIDsAreNotRewritten`. |
| REQ-OBS-06 | The Responses adapter's failure path pushed no `ErrorEvent` before `MessageEnd`, unlike the other four and the normative faux sequence. | Fixed — `TestAFailedStreamCarriesATerminalErrorEventBeforeMessageEnd`. |
| — | `anthropic/decode.go` discarded the `json.Unmarshal` error on `content_block_start`, silently dropping a malformed block. | Fixed — `TestAMalformedContentBlockStartFailsTheStream`. |
| NFR-TEST-06.1 | The differential harness's target list omitted `openai-responses`. | Fixed — `difftest/run.go`. |
| REQ-CACHE-06 / 10 | The schema-serialization cache and deferred tool loading are attached to Anthropic only; the other adapters re-marshal every schema each turn, and neither Responses `additional_tools` nor the "withhold and re-declare" arm exists. | Deferred — a per-adapter cache keyed on the wire dialect; the Anthropic implementation is the template. |
| NFR-COMPAT-05 | The Google path is `/v1beta/models/{id}`; a Vertex AI base URL needs `/v1/projects/…/publishers/google/models/{id}`, so "only a config change" does not hold. Ambient credential state is correct. | Deferred — recorded in `docs/PROVIDERS.md`'s `Implemented` column. |
| §6.2a / REQ-PROV-19 | Gemini `CachedContent` and deferred requests are not implemented. | Deferred, as the PRD already allows (NFR-PERF-08 demoted; OQ-11 option b). |
| NFR-TEST-09 | No fuzz target on the SSE reader. | Deferred — belongs in `provider/`, where the reader lives. |

## MCP, wire decoder, plugins

| Req | Finding | Status |
|---|---|---|
| NFR-SEC-03 | An unresolved `${VAR}` in a server's env or headers was a warning; the child started with a blank value. A variable set to the empty string was indistinguishable from unset. | Fixed — `Connect` returns a typed error naming the variables; the lookup reports presence. `TestAnUnresolvedVariableIsAConfigurationError`, `TestAnUnresolvedHeaderVariableIsAConfigurationError`, `TestAnExplicitlyEmptyVariableIsNotUnresolved`. |
| REQ-MCP-CLIENT-07 | The HTTP transport posted with the transport context and the client called `Send` before selecting on the call context, so a server that accepted the POST and stalled hung `Call` forever; `timeout_s` never fired. | Fixed — `SendContext`; `TestThePerCallDeadlineFiresOnAStallingHTTPServer`. |
| — | `cmd.Wait` ran the moment the process exited and, per `os/exec`, closed the pipes it created — a server that wrote its final response and exited immediately could lose that frame. | Fixed — the transport owns its pipes; `TestAResponseWrittenJustBeforeExitIsDelivered`. |
| REQ-SEC-12.1 / REQ-SEC-11.3 | The JSON-RPC envelope was decoded with `encoding/json`, which matches keys case-insensitively and ignores unknown members: `{"id":1,"ID":2}` passed the duplicate-key guard and correlated to 2. | Fixed on client and server — `TestTheClientRejectsANonStrictEnvelope`, `TestTheServerRejectsANonStrictEnvelope`. |
| — | The strict binder rejected spec-standard optional fields the protocol model omitted (`outputSchema`, `_meta`, `annotations`, `icons`, `structuredContent`, …), so one conforming server field made `tools/list` fail and the whole server unusable. | Fixed — every `2026-07-28` optional field modelled, `_meta`/`annotations` carried raw; `TestAToolWithOutputSchemaAndMetaListsAndCallsFine`. |
| NFR-REL-03 / REQ-MCP-CLIENT-02.3 | No reconnect and no re-issue: a broken response stream cancelled every in-flight call, and a dead transport stayed dead. | Fixed — a lost request is re-issued once with a new id; `per_session_reconnect_limit` (default 3) re-spawns or re-dials; exhaustion is an `is_error` result. `TestABrokenResponseStreamReissuesTheRequestWithANewID`, `TestADeadStdioServerIsRespawnedWithinTheReconnectLimit`, `TestAnHTTPTransportIsRedialledAfterItDies`. |
| REQ-MCP-CLIENT-07 | `timeout_s = 30.0` — the PRD's own default form — was dropped because the TOML subset rejected floats. | Fixed — `TestTOMLParsesFloats`, `TestTimeoutSAcceptsAFloatAndTheReconnectLimitParses`. |
| REQ-CACHE-07 | `Pool.Connect` never opened a `subscriptions/listen` stream, so `tools/list_changed` invalidated nothing unless the embedder subscribed by hand. | Fixed for stdio — `TestAStdioServerConnectedByThePoolIsSubscribedToToolChanges`; HTTP remains `ttlMs`-only and says so on the field. |
| REQ-MCP-SERVER-06.5 | `subscriptions/listen` answered `InvalidRequest` in HTTP mode, so AgentKit's own client could not subscribe to AgentKit's own HTTP server. | Fixed — an SSE response with a per-request session; `TestSubscriptionsListenWorksOverHTTP`, `TestNoNotificationWithoutASubscription`. |
| — | HTTP notifications were acknowledged 202 before routing-header validation; the parse-error reply omitted `"id": null`; stdio `Serve` spawned unbounded handler goroutines. | Fixed — `TestANotificationWithMismatchedHeadersIsRejectedNot202`, `TestAParseErrorReplyCarriesANullID`, `TestStdioServeBoundsConcurrentHandlers`. |
| REQ-SEC-11 / NFR-TEST-09.3 | The scanner's string paths passed invalid UTF-8 through, and the new fixed-point fuzz property found it within a second: `{"":"\xff"}` re-encoded differently on the second pass. | Fixed — rejected on both paths as RFC 8259 requires; `TestInvalidUTF8InAStringIsRejected`, seeded into the fuzz corpus. |
| NFR-TEST-09 | The fuzz target asserted no-panic and re-encodable but never the byte-level fixed point; the framed readers had no fuzz target. | Fixed — `FuzzGuardNeverPanics` asserts property 3; `FuzzNDJSONFrameReaderNeverPanics`, `FuzzContentLengthFrameReaderNeverPanics`. |
| REQ-PLUGIN-05/06/07/09 | Discovery, `[plugins]` config, loading order, `disabled` and load-time refusal were unwired library pieces: nothing parsed the config section, nothing composed the three tiers, `Registry.Remove` had no caller, and a lint failure produced a report but never refused registration. | Fixed — `plugins.ParseConfig`, `plugins.Load`; eight tests in `plugins/load_test.go`. The REQ-SEC-07 honesty note stands: this is a lint, not a sandbox. |
| REQ-MCP-CLIENT-06 | The name-collision check runs in `Pool.Tools`, not at `Connect`. | Deferred by note: correct for a host that resolves tools at session init; documented on the method. |

## Session, skills, catalog, policy

| Req | Finding | Status |
|---|---|---|
| REQ-SESS-09 | A partial `write` failure left the next append able to concatenate onto the partial bytes and re-emit the header mid-file. | Fixed — the store truncates the partial tail (the same policy as the load-time repair) and falls back to a pending newline; `TestAPartialWriteDoesNotSwallowTheNextEntry`, `TestAPartialWriteOfTheHeaderKeepsTheHeaderPending`. |
| REQ-SESS-04 | `RecordCompaction` documented that the anchor must be in the log but never checked; an unknown anchor was written and reported as a repair on every later load. | Fixed — `TestRecordCompactionRefusesAnAnchorThatIsNotInTheLog`. |
| — | `OpenSession` ignored `NewID`/`Now` on first create, so a golden through it could not pin the header. | Fixed — `TestOpenSessionHonoursNewIDAndNowOnFirstCreate`. |
| REQ-SKILL-13 / REQ-SKILL-04 | No authoring contract was documented and no built-in skill existed to serve as the worked example; the built-in tier was always empty. | Fixed — `_skills/code-review` (six-part contract, each prohibition with its incident), the contract in `skills/doc.go`, `skills.BuiltinDir()` (empty, never relative, when unresolvable); `TestTheShippedSkillLoadsThroughTheRealLoader`, `TestTheShippedSkillRendersThroughAssemble`. |
| REQ-SKILL-05/07/08/09/11 | `LoadForSession` drops `config` and ignores `taskPrompt` (the gate moved to `Discover`); no REQ-CACHE-10 marking for a skill activating mid-session; the subagent section is parsed only; no import lint for skill plugin code; `AuditSkills` must be called by the embedder. | Deferred — each is documented in `skills/doc.go`; the plugin lint exists and can be reused for skill plugins once a skill can carry one. |
| NFR-TEST-08 | Golden tests carry no per-golden provenance docblock, and `-update` regenerates from AgentKit's own output. | Deferred with the reasoning already in `docs/PROVIDERS.md`: for the prompt, session-log and wrapper goldens AgentKit is the only possible reference; for the request goldens the gap is real and the ledger says so. |
| REQ-SESS-05 | `RepairMissingHeader` said a header is synthesized but never wrote one. | Fixed by doc: the header is synthesized in memory only; rewriting an append-only file is the thing the store must never do. |

## Documentation and process

| Req | Finding | Status |
|---|---|---|
| REQ-GO-01 / NFR-COMPAT-01 | The PRD said Go 1.21; the code uses `iter.Seq` (1.23) and `omitzero` (1.24) and `go.mod` declares 1.24. | PRD amended to 1.24. The code is the copy that binds. |
| NFR-COMPAT-02 / REQ-GO-10 | Both still described the pre-0.4.0 posture (`2025-03-26` with legacy compatibility; `mcp-go` in a nested module). | PRD amended to what 0.3.5/0.4.0 decided. |
| NFR-COMPAT-07.2/.3 | The provider ledger had no `Implemented` column and no rulings section. | Fixed — `docs/PROVIDERS.md`. |
| REQ-SEC-13.1 | The attribution header set was documented nowhere but the PRD. | Fixed — `docs/PROVIDERS.md`, "Attribution headers". |
| NFR-COMPAT-06 | The README claimed a cross-target build gate that did not exist. | Fixed — `internal/policy/crosstarget_test.go` builds and vets every supported `GOOS/GOARCH`. |
| — | README: "implements v0.3.2"; a paragraph about `mcp/httpsse.go`, a transport removed in 0.4.0; `RequestSampling` and "the handshake", neither of which exists; `difftest/README.md` said the ledger "is not yet written"; the dependency gate's escalation message named a `docs/DEPS.md` that did not exist. | Fixed. |
