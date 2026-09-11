# Keep the root package to the Agent

Status: **proposal, awaiting review**. Nothing in this document has been
implemented. It records what the root package holds today, what it should
hold, the dependency facts that decide which code can move where, and the
order of the moves so that every step leaves `make check` green.

## 1. The problem

The root package `agentkit` is 20 source files and 4,800 non-test lines with
**80 top-level exported identifiers** (40 functions, 25 types, 11 constants,
4 variables) plus 35 methods on `Agent`. It is at once the loop, the tool
batch executor, five middlewares, the cache meter, four compaction strategies
and two summarizers, six stop policies, the prompt assembler, the argument
pipeline, the execute guard and reference interceptor, the delegation layer,
the session front door, and an event encoder.

What consumers actually reach for is much smaller. Across the fifteen
examples the root identifiers used are:

| Identifier | Call sites | Identifier | Call sites |
|---|---|---|---|
| `StopAfterTurns` | 19 | `RetryMiddleware` / `RetryOptions` | 3 / 3 |
| `NewAgent` | 16 | `OpenSession` | 3 |
| `StopOverBudget` | 14 | `NewContextTransform` / `CompactionDeps` | 3 / 3 |
| `StopAny` | 14 | `SummarizationCompaction` | 3 |
| `RegisterDefaults` | 13 | `ModelSummarizer` / `ModelTurnSummarizer` | 3 / 3 |
| `Agent` | 11 | `NewAgentWithHistory` | 3 |
| `RestrictedPolicy` / `RestrictedOptions` | 5 / 5 | everything else | 0 to 2 |

Nothing inside the module imports the root except the examples and the
dependency-policy test. Every sub-package (`core`, `session`, `provider`,
`skills`, `tools`, `mcp`, `plugins`, …) sits *below* the root in the import
graph. That is the fact that makes this refactor mechanical: code moving out
of the root only has to avoid importing the root, and almost none of it
needs to.

## 2. What the root should keep

The root package is the thing you construct and run. It keeps exactly:

| Stays | Why |
|---|---|
| `Agent` and every method on it (`agent.go`, `loop.go`, `batch.go`, `deferred.go`, the audit and hook methods) | The loop is the product. Every method touches the private run slot, queues, recorder or meter. |
| `NewAgent`, `NewAgentWithHistory`, `NewAgentFromSession` | Constructors. `NewAgentFromSession` needs `NewAgentWithHistory`, so it cannot live in `session` (the root imports `session`). |
| `DefaultProviders`, `RegisterDefaults` | The first thing the README shows. Two functions, pure. |
| `ImageNormalizationError` and the unexported `normalizeImages` | The history-boundary normalization is loop logic; the error type is what an `OnError` hook matches with `errors.As`. |
| Two **new** methods, `Config()` and `SetStopPolicy()` | The only API additions. They are what lets delegation leave the root (§3.6). |

That is 7 top-level identifiers instead of 80, and about 2,300 non-test
lines instead of 4,800.

Dropped outright, no replacement in the root:

- `Span`, `Tracer`, `NoopTracer` — aliases of `core.Span`, `core.Tracer`,
  `core.NoopTracer`. Callers use `core` directly.
- `ArgumentsHash` — a wrapper around `core.HashArguments` whose own comment
  says it was kept for history.
- `DefaultMaxTokens` — moves to `core` because both the loop and the
  compaction package need it.

## 3. Where everything else goes

Six new packages, plus four small moves into packages that already exist.
The import direction of every new package is `→ core` (and for two of them
`→ provider` or `→ skills, tools`); none imports the root, except
`subagent`, which is allowed to because nothing in the root references it.

### 3.1 `stop` — stop policies (from `stoppolicy.go`)

| Today | After |
|---|---|
| `agentkit.StopAfterTurns(n)` | `stop.AfterTurns(n)` |
| `agentkit.StopOverBudget(usd)` | `stop.OverBudget(usd)` |
| `agentkit.StopAfterDuration(d)` | `stop.AfterDuration(d)` |
| `agentkit.StopWhenToolCalled(name)` | `stop.WhenToolCalled(name)` |
| `agentkit.StopAny(p...)` | `stop.Any(p...)` |
| `agentkit.StopNever()` | `stop.Never()` |

Imports `core` only. The types (`core.StopPolicy`, `core.StopContext`) are
already in `core`, so this is a pure move. It is the most-used cluster in
the examples, so it is the move that reduces the most call sites.

### 3.2 `middleware` — Axis 1 plus the cache meter (from `middleware.go`, `cachestats.go`)

| Today | After |
|---|---|
| `RetryMiddleware(RetryOptions)` | `middleware.Retry(middleware.RetryOptions)` |
| `Retryable(msg)` | `middleware.Retryable(msg)` |
| `BudgetMiddleware(max, usage)`, `ErrBudgetGate` | `middleware.Budget(max, usage)`, `middleware.ErrBudgetGate` |
| `CachingMiddleware(CacheOptions)`, `Fingerprint` | `middleware.Caching(middleware.CacheOptions)`, `middleware.Fingerprint` |
| `TracingMiddleware(t)` | `middleware.Tracing(t core.Tracer)` |
| `RateLimitMiddleware(rate, burst)` | `middleware.RateLimit(rate, burst)` |
| `CacheMeter`, `NewCacheMeter`, `CacheStats` | `middleware.CacheMeter`, `middleware.NewCacheMeter`, `middleware.CacheStats` |

The meter goes with the middleware rather than into its own package because
`CachingMiddleware` calls the meter's unexported `hit`, `miss` and `price`,
and the caching and tracing middlewares share the unexported context note.
Separating them would mean exporting those. Imports `core` and `provider`
(for `ComputeCost`, `SyncReport`, `DeferredSplit`). The root imports
`middleware` for the meter it holds: `Agent.Meter()` returns
`*middleware.CacheMeter` and `Agent.CacheStats()` returns
`middleware.CacheStats`.

### 3.3 `compaction` — strategies, summarizers, the transform, the estimate (from `compaction.go`, `estimate.go`)

| Today | After |
|---|---|
| `CompactionStrategy` | `compaction.Strategy` |
| `CutPolicy`, `CutNotToolResult`, `CutUserOnly` | `compaction.CutPolicy`, `compaction.CutNotToolResult`, `compaction.CutUserOnly` |
| `NoCompaction`, `TurnWindowCompaction`, `TokenWindowCompaction`, `SummarizationCompaction` | `compaction.None`, `compaction.TurnWindow`, `compaction.TokenWindow`, `compaction.Summarization` |
| `Summarizer`, `ModelSummarizer`, `ModelTurnSummarizer`, `ValidateSummary`, `ErrBadSummary` | same names under `compaction.` |
| `CompactionDeps`, `NewContextTransform`, `ApplyCheckpoint` | `compaction.Deps`, `compaction.NewContextTransform`, `compaction.ApplyCheckpoint` |
| `CompactionSummaryPrefix`, `CompactionSplitSeparator` | `compaction.SummaryPrefix`, `compaction.SplitSeparator` |
| `EstimateContextTokens` | `compaction.EstimateContextTokens` |

The estimate goes here rather than staying in the root because compaction
is its primary consumer and the two are specified together (REQ-GO-12/15).
The loop's one call, in `callModel`, becomes
`compaction.EstimateContextTokens(view, checkpointOf(a.history))`. Two
root-internal dependencies are cut: `DefaultMaxTokens` moves to `core`, and
the summarizer's session-id generator becomes a private eight-byte random id
inside the package (the root's `newID` is three lines).

### 3.4 `prompt` — the assembled system prompt (from `prompt.go`)

| Today | After |
|---|---|
| `BuildSystemPrompt(PromptInput)` | `prompt.Build(prompt.Input)` |
| `PromptSection`, `SectionBase` … | `prompt.Section`, `prompt.SectionBase` … |
| `BaseInstructions`, `UniversalGuidelines` | `prompt.BaseInstructions`, `prompt.UniversalGuidelines` |
| `SkillBlocks(sk, files, active)` | `prompt.SkillBlocks(sk, files, active)` |

Imports `core`, `skills`, `tools`. The root imports `prompt` from
`callModel`. After this move the root no longer imports `tools` at all.

### 3.5 `guard` — the execute boundary (from `policy.go`)

| Today | After |
|---|---|
| `ShellToolNames` | `guard.ShellToolNames`, plus `guard.IsShellTool(name)` |
| `AllowAllToolCalls` | `guard.AllowAll` |
| `RestrictedPolicy(RestrictedOptions)` | `guard.Restricted(guard.Options)` |

Imports `core` only. The root keeps `checkExecuteGuard` as a private method
(it reads the agent's resolved tool set under the lock) and calls
`guard.IsShellTool`. The package doc carries the OQ-8 argument that is at
the top of `policy.go` today.

### 3.6 `subagent` — delegation (from `subagent.go`)

| Today | After |
|---|---|
| `AgentFactory` | `subagent.Factory` |
| `SubagentOptions`, `SubagentTool(parent, f, opts)` | `subagent.Options`, `subagent.Tool(parent, f, opts)` |
| `AgentDefinition` | `subagent.Definition` |
| `AgentRegistry`, `NewAgentRegistry` | `subagent.Registry`, `subagent.NewRegistry` |
| `NewAgentFromDefinition(parent, def)` | `subagent.FromDefinition(parent, def)` |
| `RunParallel` | `subagent.RunParallel` |

This is the one package that **imports the root**, which is legal because
nothing in the root references `subagent.go` (verified by a cross-reference
of every root identifier). It also imports `stop` for `stop.Any` and
`stop.OverBudget`.

Today `subagent.go` reaches into three private fields of a child agent:
`child.mu`, `child.cfg.StopPolicy` (to graft a budget onto a
factory-supplied child) and `child.history.Len()`, and into `parent.cfg` to
copy the infrastructure fields. Two exported seams on `Agent` replace that:

- `func (a *Agent) Config() core.AgentConfig` — a copy of the config, read
  under the lock. `Snapshot()` already exposes a `ConfigView`; this is the
  full struct for callers building a derived agent.
- `func (a *Agent) SetStopPolicy(p core.StopPolicy) error` — `ErrBusy`
  while a run is in flight, exactly like `SetPromptBlocks`.

`child.history.Len()` becomes `child.History().Len()`, which already
exists. These two methods are the only additions to the public API in the
whole plan. The alternative is to leave delegation in the root, which keeps
eight more identifiers there; I recommend the move.

### 3.7 Moves into existing packages

| Today | After | Why there |
|---|---|---|
| `ResolveToolPolicy(registered, p)` | `func (p core.ToolPolicy) Resolve(registered []core.Tool) []core.Tool` | `core` owns `ToolPolicy`; the resolution is a method on it. Used by four root files and one example. |
| `PrepareArguments`, `Prepared` | `core.PrepareArguments`, `core.PreparedArguments` | `core` already imports `jsonx` and `schema` and already holds `HashArguments`, `ShouldIterate` and `BatchTerminates`, the same kind of small algorithm over its own types. `schema` cannot host it (it would import `core`, a cycle). |
| `MCPPrefix`, `MCPServerOf` | `core.MCPPrefix`, `core.MCPServerOf` | The authoritative field is `core.Tool.MCPServer`; the fallback belongs beside it. Not `mcp`: that package brings `wire`, `toml` and the HTTP server into the loop's graph. |
| `DefaultMaxTokens` | `core.DefaultMaxTokens` | Needed by both the loop and `compaction`. |
| `EventJSON` | `session.EventJSON` | It is `core.MarshalEvent` bound to the session codec. |
| `OpenSession(path, opts)` | `session.OpenOrCreate(path, opts)` | It calls only `session.Open`, `session.Create`, `session.Fold`. The `sess_` id default moves with it. |
| `SkillsConfigFor(cfg, workDir, builtinDir)` | `skills.ConfigFor(cfg core.AgentConfig, workDir, builtinDir)` | `skills` already imports `core`. `Agent.LoadSkills` stays a method; it is a two-line wrapper over `skills.Record`. |

`core` grows by four small items. If keeping `core` lean matters more than
avoiding another package, `PrepareArguments` can instead become its own
`toolargs` package; I would not, because it is 190 lines and has no other
natural home.

### 3.8 The import graph after the move

```
agentkit (root) ──► core, session, skills, imagex,
                    middleware, compaction, prompt, guard
subagent        ──► agentkit, core, schema, stop
stop            ──► core
guard           ──► core
compaction      ──► core
middleware      ──► core, provider
prompt          ──► core, skills, tools
```

No cycles: the six new packages sit below the root exactly as `core`,
`session` and `provider` do today, with `subagent` above it. The root stops
importing `provider`, `schema`, `jsonx` and `tools` directly.

## 4. Tests

The root's 22 test files (6,600 lines) are all `package agentkit`, so they
see private identifiers, and 14 of them share helpers declared in
`loop_test.go` (`scripted`, `newTestAgent`, `testModel`, `testAPI`,
`echoTool`, `toolUse`, `assistantWithTools`). Two rules keep the triage
mechanical:

1. **A test that constructs an `Agent` stays in the root.** It is a wiring
   test of the loop with that component, whichever package the component
   moved to. This covers `loop_test.go`, `loop_fixes_test.go`,
   `resume_test.go`, `deferred_test.go`, `perf_*_test.go`,
   `plugin_wiring_test.go`, `mcp_wiring_test.go`, `images_test.go`, the
   request and session-log goldens, and the Agent-driven halves of
   `middleware_test.go`, `cachestats_test.go`, `audit_test.go` and
   `skillsconfig_test.go`.
2. **A test that touches only the moved code moves with it**, as an
   internal test of the new package so it keeps access to private
   identifiers (`backoff`, `newLRU`, `snapForward`, `summaryMaxTokens`).
   This covers all of `compaction_test.go` and `compaction_delta_test.go`,
   `middleware_stream_test.go`, `resume_options_test.go` (to `session`),
   `events_test.go` (to `session`), the three system-prompt goldens in
   `golden_test.go` (to `prompt`, with their three fixture files under
   `prompt/testdata/golden/`), and the pure halves of the four split files
   above.

The Agent-free helpers (`scripted`, `testModel`, `testAPI`, `echoTool`,
`toolUse`, `assistantWithTools`) move to a new `internal/testkit` package
that imports only `core`, so a moved test can still script a provider.
`newTestAgent` stays in the root's tests: a helper package that constructed
an `Agent` would import the root, and the root's own internal tests could
not then import it (Go rejects that cycle in test builds). A moved test
that genuinely needs an `Agent` is written as an external test package
(`package middleware_test`), which may import the root even though the root
imports `middleware`.

The mutation table in the README names tests by function name, not file, so
it survives the moves as long as no test is renamed.

## 5. Documentation to update in the same change

- `README.md`: the package table gains six rows and the `.` row shrinks to
  "`Agent`, the loop, the batch executor, provider registration"; the prose
  links to `policy.go`, `compaction.go` and `audit.go` are repointed; the
  three code snippets that name root identifiers are updated.
- `examples/README.md`, `examples/{cleaner,issued,flatline}/README.md`:
  `agentkit.RestrictedPolicy`, `agentkit.AllowAllToolCalls`,
  `agentkit.NewContextTransform`, `agentkit.NewAgentWithHistory`, and the
  `policy.go` and `loop.go` links.
- `docs/GAPS.md`: three file references (`batch.go` stays; `compaction.go`
  moves).
- `docs/errata/compaction_extension.md`, `docs/errata/default_max_tokens.md`:
  `agentkit.NewContextTransform`, `agentkit.DefaultMaxTokens`.
- `docs/prd/agent-kit-prd.md` names `RetryMiddleware`, `NoCompaction`,
  `SubagentTool` and friends unqualified. The PRD is the specification and
  should not be rewritten for a layout change; a new
  `docs/errata/package_layout.md` records the mapping in §3 as the
  divergence, per the errata convention.
- A new `docs/adr/01-keep-the-root-package-to-the-agent.md` records the
  decision and the import-direction rule (nothing below the root imports
  it; `subagent` is the one package above it). `docs/adr/` does not exist
  yet; `AGENTS.md` already names it as the ADR location.
- The package doc on `agent.go` says the canonical vocabulary is
  "re-exported here by type alias, so `agentkit.Tool` and `core.Tool` are
  the same type". That has not been true for some time (only `Span`,
  `Tracer` and `NoopTracer` are aliased, and this plan removes those). It
  is rewritten to say that `core` is the vocabulary and the root is the
  agent.

## 6. Order of work

Each step compiles, passes `make check`, and is one conventional commit.
Steps 2 through 7 are independent of each other once step 1 has landed;
step 8 depends on step 2.

| # | Step | Commit type |
|---|---|---|
| 0 | `internal/testkit` with the Agent-free helpers; root tests import it. Fix the `agent.go` package doc. No behaviour change. | `test:` / `docs:` |
| 1 | `core`: add `DefaultMaxTokens`, `ToolPolicy.Resolve`, `PrepareArguments` and `PreparedArguments`, `MCPPrefix` and `MCPServerOf`. Delete the root copies and repoint the root, the tests and `examples/issued`. | `refactor(core):` |
| 2 | `stop` package. Repoint every example. | `refactor(stop):` |
| 3 | `compaction` package, with `estimate.go` and both compaction test files. `loop.go` imports it for the estimate. | `refactor(compaction):` |
| 4 | `middleware` package, with `cachestats.go`. `Agent.meter` changes type; `Meter()` and `CacheStats()` signatures follow. Split `middleware_test.go` and `cachestats_test.go` per §4. | `refactor(middleware):` |
| 5 | `prompt` package and `skills.ConfigFor`. Move the three prompt goldens and their fixtures. | `refactor(prompt):` |
| 6 | `guard` package. `checkExecuteGuard` stays in the root. | `refactor(guard):` |
| 7 | `session.OpenOrCreate` and `session.EventJSON`. Delete `resume.go`'s front door and `events.go`. | `refactor(session):` |
| 8 | `Agent.Config()` and `Agent.SetStopPolicy()`, then the `subagent` package. | `feat(agent):` + `refactor(subagent):` |
| 9 | Documentation from §5, the erratum, the ADR. Final check: `go doc .` lists seven top-level identifiers. | `docs:` |

Rough size: about 4,800 source lines move (mechanically, with `gofmt` and
the compiler as the check), and about 6,600 test lines are triaged by the
two rules in §4. The test triage is where the time goes; the source moves
are an afternoon.

## 7. Compatibility

The module is untagged, and every consumer of the root package is inside
this repository (fifteen examples, one of which is the nested `flatline`
module that already `replace`s the root to `../..`). I recommend a **clean
break**: no deprecated forwarders left in the root. Forwarders would keep
all 80 identifiers in the root, which is the opposite of the goal, and
there is no released version to stay compatible with.

If an external consumer exists that this repository does not know about,
the fallback is type aliases only (`type RetryOptions = middleware.RetryOptions`
and so on, marked `Deprecated:`), removed at the first tag. Function
forwarders are not worth it either way.

## 8. Decisions for the reviewer

1. **Clean break or aliases** (§7). Recommendation: clean break.
2. **Package names.** `stop`, `middleware`, `compaction`, `prompt`,
   `guard`, `subagent`. The two I am least sure of: `guard` (alternatives
   `authz`, `execpolicy`) and `subagent` (alternative `delegate`).
3. **`PrepareArguments` in `core`** or in a new `toolargs` package (§3.7).
   Recommendation: `core`.
4. **`EstimateContextTokens` in `compaction`** or in `core` (§3.3).
   Recommendation: `compaction`.
5. **Delegation leaves the root at the cost of `Config()` and
   `SetStopPolicy()`** (§3.6), or stays and the root keeps eight more
   identifiers. Recommendation: it leaves.
6. **The PRD is left as written and an erratum records the mapping**, or
   the PRD's identifier names are rewritten in place (§5). Recommendation:
   erratum.

## 9. Noticed, not part of this change

- `CacheMeter.ObserveSync` and `CacheMeter.ObserveDeferredSplit` (the
  REQ-CACHE-11 prefix-invalidation counters) have no caller outside their
  own test. `CacheStats.PrefixInvalidations`, `DeferredToolPromotions` and
  `LastInvalidation` are therefore always zero and empty on a real agent.
  The move to `middleware` preserves that; wiring them is a separate fix.
- The README says the root "never drags `net/http` into a consumer that
  only wants the loop". `core/provider.go` already imports `net/http` for
  `RequestOptions.Transport` and `OnResponse`, so every consumer of `core`
  links it today. Not made worse by this plan, and not fixed by it.
