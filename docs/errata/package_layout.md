# Package layout: where the PRD's root-package names now live

The PRD names most of the SDK's building blocks unqualified, as though they
were declared in the root package, and until this erratum they were. The
root package now holds the `Agent` and nothing else (ADR
[01](../adr/01-keep-the-root-package-to-the-agent.md)); everything the PRD
names below lives in a package of its own. The PRD text is left as written.
This table is the mapping.

| PRD name | Where it is now |
|---|---|
| `StopAfterTurns`, `StopOverBudget`, `StopAfterDuration`, `StopWhenToolCalled`, `StopAny`, `StopNever` | `stop.AfterTurns`, `stop.OverBudget`, `stop.AfterDuration`, `stop.WhenToolCalled`, `stop.Any`, `stop.Never` |
| `RetryMiddleware`, `RateLimitMiddleware`, `BudgetMiddleware`, `TracingMiddleware`, `CachingMiddleware` (§5 Axis 1, REQ-PROV-14, REQ-OBS-01, REQ-CACHE-01..05) | `middleware.Retry`, `middleware.RateLimit`, `middleware.Budget`, `middleware.Tracing`, `middleware.Caching` |
| `Session.CacheStats()` (REQ-CACHE-08) | `Agent.CacheStats()` returning `middleware.CacheStats`; the meter is `middleware.CacheMeter` |
| `NoCompaction`, `TurnWindowCompaction`, `TokenWindowCompaction`, `SummarizationCompaction` (REQ-GO-12) | `compaction.None`, `compaction.TurnWindow`, `compaction.TokenWindow`, `compaction.Summarization` |
| the context transform, the summarizers, REQ-GO-16's taxonomy, REQ-GO-15's estimate | `compaction.NewContextTransform`, `compaction.Deps`, `compaction.ModelSummarizer`, `compaction.ModelTurnSummarizer`, `compaction.ValidateSummary`, `compaction.EstimateContextTokens` |
| the assembled system prompt (NFR-TEST-08a, REQ-TOOL-04e, REQ-SKILL-06) | `prompt.Build`, `prompt.Input`, `prompt.SkillBlocks` |
| the OQ-8 interceptor (`RestrictedPolicy`, `AllowAllToolCalls`) | `guard.Restricted`, `guard.Options`, `guard.AllowAll`, `guard.ShellToolNames` |
| `SubagentTool`, agent definitions and their registry (REQ-MULTI) | `subagent.Tool`, `subagent.Options`, `subagent.Factory`, `subagent.Definition`, `subagent.Registry`, `subagent.FromDefinition`, `subagent.RunParallel` |
| REQ-TOOL-10's resolution | `core.ToolPolicy.Resolve` |
| REQ-TOOL-11's argument pipeline | `core.PrepareArguments`, `core.PreparedArguments` |
| REQ-OBS-05's `server_name` fallback | `core.MCPServerOf`, `core.MCPPrefix` |
| the default `max_tokens` (REQ-CAT-04) | `core.DefaultMaxTokens` |
| the session front door and REQ-OBS-06c's event encoder | `session.OpenOrCreate`, `session.EventJSON` |
| the skills discovery config derived from `AgentConfig.TrustProject` | `skills.ConfigFor` |

Two methods were added to `Agent` so that delegation could leave the root
without reaching into private fields: `Config()`, a copy of the
configuration, and `SetStopPolicy()`, which refuses with `ErrBusy` while a
run is in flight.

Nothing about behaviour changed. The one visible difference outside Go
identifiers is that the reason string on a call `guard.Restricted` blocks
now begins `guard.Restricted:` rather than `RestrictedPolicy:`.
