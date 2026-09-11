# ADR 01: Keep the root package to the Agent

Status: accepted, implemented.

## Context

The root package `agentkit` had grown to 80 exported top-level identifiers
across 4,800 lines: the loop, the tool batch executor, five middlewares, the
cache meter, four compaction strategies and two summarizers, six stop
policies, the prompt assembler, the argument pipeline, the execute guard,
delegation, the session front door and an event encoder. The examples used a
small slice of it, dominated by `NewAgent`, `RegisterDefaults` and three stop
policies.

Nothing inside the module imported the root except the examples and the
dependency-policy test; every sub-package already sat below it in the import
graph. The proposal and the consumer survey behind it are in
[`docs/prd/02-keep-the-root-package-to-the-agent.md`](../prd/02-keep-the-root-package-to-the-agent.md).

## Decision

The root package holds the `Agent` and what constructs it: `Agent` and its
methods, `NewAgent`, `NewAgentWithHistory`, `NewAgentFromSession`,
`DefaultProviders`, `RegisterDefaults`, and `ImageNormalizationError`. Seven
top-level identifiers.

Everything else lives in a package of its own, named for what it is rather
than suffixed with it: `stop`, `middleware`, `compaction`, `prompt`,
`guard`, `subagent`. Four small pieces went to the packages that already
owned their types: `core` (`ToolPolicy.Resolve`, `PrepareArguments`,
`MCPServerOf`, `DefaultMaxTokens`), `session` (`OpenOrCreate`,
`EventJSON`) and `skills` (`ConfigFor`). The mapping from the PRD's names is
[`docs/errata/package_layout.md`](../errata/package_layout.md).

**The import-direction rule.** Every package other than `subagent` sits
below the root and imports only `core` and its siblings; none imports the
root. `subagent` is the one package above the root: it needs a parent
`*agentkit.Agent` to read from and a child to run, and nothing in the root
references it, so the direction is safe. A package that needs an `Agent`
goes above the root; a package the root needs goes below it; nothing does
both.

**No compatibility shims.** The module is untagged and every consumer is in
this repository, so the old names were removed rather than aliased. Aliases
would have kept the 80 identifiers in the root, which was the problem.

**Two additions to the Agent.** `Config()` returns a copy of the
configuration; `SetStopPolicy()` replaces the stop policy and refuses with
`ErrBusy` mid-run. They replace the three private-field reaches delegation
had, and are the only widening of the public surface.

## Consequences

- A test that constructs an `Agent` lives in the root as a wiring test,
  whichever package the component it exercises moved to. A test that touches
  only moved code moved with it. `internal/testkit` exports the Agent-free
  doubles (`Scripted`, `TestModel`, `ToolUse`, …) so a test above the root
  can script a provider; the root's own tests keep their private copies,
  because an internal test package cannot import a helper that imports the
  package under test.
- `core` gained four small algorithms over its own types, beside
  `HashArguments`, `ShouldIterate` and `BatchTerminates`, which it already
  held. It still imports only `jsonx` and `schema`.
- `Agent.Meter` and `Agent.CacheStats` name `middleware` types; the root
  imports `middleware`, `compaction`, `prompt`, `guard`, `session`, `skills`
  and `imagex` and no longer imports `provider`, `schema`, `jsonx` or `tools`
  directly.
- The PRD keeps its original unqualified names; the erratum records the
  mapping rather than the specification being rewritten for a layout change.
