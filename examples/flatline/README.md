# flatline — implement one spec pack, the way agent-fox does

Give it a spec pack. It walks the task groups in `tasks.json` in order, runs a
coder session per group with the spec rendered and scoped to that group, runs
the pack's own test commands, commits, marks the group's subtasks done, and
squash-merges the group into the branch it started from — then runs the final
checks and an informational verifier.

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd examples/flatline
go run . --dir ~/src/widgets 3                        # .specs/03_* under --dir
go run . --dir ~/src/widgets .specs/03_widget_counter  # or the directory itself
```

It is the AgentKit implementation of [agent-fox](https://github.com/agent-fox-dev/agent-fox)'s
`af code` in its simplest configuration: **one spec, no cross-spec
dependencies, `--no-parallel`** — the case where agent-fox's task graph is a
flat line, `spec:1 → spec:2 → … → spec:N`, and the scheduler degenerates to
"run the next group when the previous one completed". The spec pack format
and the library that reads and writes it come from
[agent-fox-dev/spec](https://github.com/agent-fox-dev/spec) (`afspec`, Go).

## Why this shape

agent-fox is an orchestrator: a DuckDB plan, worktrees, a knowledge store,
reviewer and verifier archetypes, parallel dispatch, hot-loading. Almost all of
that exists to run *many* specs at once. Strip it to one spec on one branch and
what is left is a short, ordered list of steps, most of which are git and
process control — and, as with [`cleaner`](../cleaner), the split that makes
the program trustworthy is that **the thing deciding whether a group succeeded
is not the thing that did the work.**

```
 Go (deterministic)                                     the model
 ──────────────────                                     ─────────
 preflight: clean tree, LoadSpec, Validate, no deps
 for each task group, in id order:
   skip it if every subtask is done and all_tests passes
   branch feature/{spec}/{N} from the base branch
   assemble the context (scoped render, steering, memory)
                                          ───────►  coder session, or gate session
   run linter · spec_tests · all_tests    ◄───────  submit_group{summary, commit_subject, …}
   linter red? → fresh branch, retry with the error in the prompt (≤ 3 attempts)
   commit · mark subtasks done in tasks.json · commit that
   squash-merge into the base branch (or keep the branch)
 name the finished work after the spec (--land=branch)
 final check: all three commands                        ───────►  verifier session (read-only)
 summary · JSONL journal                               ◄───────  submit_verdicts{PASS/FAIL per requirement}
```

Everything the model produces arrives through a terminating tool with a
schema. agent-fox asks its coder to write `.agent-fox/session-summary.json`
and parses it afterwards; here the same fields are the arguments of
`submit_group`, validated by the loop, and a malformed answer comes back to
the model as an error it can correct.

## How it maps to agent-fox

Read from `agent-fox` v4.9.1, `packages/agentfox/agentfox/`.

| Concern | agent-fox | flatline |
|---|---|---|
| Unit of work | one task group per session (`graph/builder.py`) | same |
| Order | Kahn's sort over intra-spec edges; array order when `dependencies: []` (`graph/resolver.py`) | groups sorted by id |
| Archetype | `coder`, or `gate` for `kind: checkpoint` (`archetypes.py`) | `ArchetypeFor` |
| Preflight | skip a group whose subtasks are all done **and** whose `make test` passes; else launch with a "Preflight State" block (`engine/preflight.py`) | same, with the pack's `all_tests` |
| Workspace | worktree + branch `feature/{spec}/{group}` from the integration branch (`workspace/worktree.py`) | branch of the same name, in place |
| System prompt | archetype profile + `## Context` (`session/prompt.py`) | the same profiles, in [`prompts.go`](prompts.go) |
| Context | `## Requirements` · `## Test Specification` · `## Tasks` scoped by `render_individual_scoped(group, max_tokens=30_000)` · `## Architecture` · `## Steering Directives` · `## Memory Facts`, joined by `---` (`session/context.py`) | `Pack.AssembleContext`, via `afspec.RenderIndividualScoped` |
| Task prompt | "Implement task group N from specification `x`… Do not modify tasks.json… commit… run the relevant test suite and linter" (`session/prompt.py:129`) | same text, two sentences changed (below) |
| Retry note | "**Note:** This is retry attempt N. The previous attempt failed with: …" (`session_lifecycle.py:383`) | verbatim |
| Tools | the CLI's toolset; Bash gated by a program allowlist and a shell-operator ban (`core/security.py`) | AgentKit's file tools + `execute` under `RestrictedPolicy` with agent-fox's allowlist. The coder may use shell operators — the pack's own test commands need `&&` — and every simple command on a line is checked; the gate and the verifier get no operators. `git` is limited to read-only subcommands, and an environment assignment in front of a program does not hide it |
| Bounds | Sonnet, `max_turns=300`, `max_budget_usd=20`, session timeout 45 min, `max_retries=2` | the same defaults |
| Done | the orchestrator marks the subtasks done and commits `chore: mark task group N subtasks done` (`session_lifecycle.py:774`) | same message, through `TransitionSubtask` |
| Landing | `git merge --squash` into `main` with the last non-housekeeping commit's message (`workspace/harvest.py`) | `SquashMergeInto` + `SquashMessage` |
| Blocked | retries exhausted → node blocked, dependents blocked in cascade, branch renamed `stalled/…` | the same; on a chain the cascade is "stop" |
| Final check | `make check` once the graph drains → `COMPLETED` or `COMPLETED_DIRTY` (`engine.py:738`) | the pack's three commands → exit 0 or 4 |
| Verifier | read-only session with a requirement-to-test checklist; PASS/FAIL verdicts, never gating (`profiles/verifier.md`) | same, but off unless `--verifier` is given: a session whose verdicts change nothing is a cost the operator opts into |
| Exit codes | completed 0 · stalled 2 · cost limit 3 · interrupted 130 | 0 · 1 · 3 · 130, plus 2 usage and 4 dirty |

### Where it deliberately differs

Each of these is a choice, not an omission.

1. **Go makes the commit.** agent-fox's coder commits its own work and is
   asked not to push or rebase. Here the coder's shell refuses every mutating
   `git` subcommand and the commit is made from the subject it submits — so
   "committed as `abc123`" in the summary means one thing, and a failed
   attempt can be discarded by resetting a branch nobody else wrote to.
2. **The linter gates every group.** agent-fox runs none of the pack's
   `test_commands` per group; it renders them into the prompt and trusts the
   coder. flatline runs all three after every coder group and fails the
   attempt on the **linter only** — the one command whose expected result is
   the same for every group. The spec tests are *supposed* to fail after a
   `tests` group and may keep failing until the last implementation group, so
   they are recorded per group and judged once, at the end, where agent-fox
   judges them too.
3. **No worktrees, one checkout.** A serial pass never has two groups in
   flight, so the branch is created in place. With `--land=merge` (agent-fox's
   default) each group's branch is squash-merged into the base branch as soon
   as it passes and the next group branches from there; with `--land=branch`
   the branches are kept and each is created from the previous one's tip, so
   the chain still builds on itself. In that mode the finished work is also
   given a branch named after the spec —
   `feature/{spec_id}_{spec_name}`, the spec's own directory name — and the
   run ends checked out on it, so the branch to merge into the default branch
   is not something to work out by counting task groups. It is
   **not** `feature/{spec_name}`: git keeps branches as files under
   `refs/heads`, so that name and `feature/{spec_name}/2` cannot coexist.
   An existing branch of that name is only moved when the move loses nothing;
   otherwise the run says where the work is and leaves it alone.
4. **No knowledge store.** agent-fox's `## Memory Facts` come from DuckDB:
   prior session summaries, reviewer findings, drift reports. flatline's come
   from this run: the `summary`, `gotchas`, `assumptions` and
   `rejected_approaches` each completed group submitted are injected into every
   later group's context.
5. **No pre-flight reviewer, no audit-review, no coverage-regression
   finding.** All three feed and read the knowledge store. Compaction *is*
   set up: every session installs `agentkit.NewContextTransform` with
   `SummarizationCompaction` at 60% of the context window (`installCompaction`
   in [`phases.go`](phases.go)), so a long coder session summarizes its own
   transcript instead of ending on a context-length error.
6. **AGENTS.md / CLAUDE.md and steering are rendered into the coder's
   context only.** agent-fox relies on the Claude CLI picking them up from
   the worktree; an AgentKit agent has no such implicit behaviour, so the
   file is a `## Project Instructions` section instead — for the session that
   writes code. A gate runs commands and a verifier reads; neither needs the
   house style.
7. **The test commands run without credentials.** The pack's `test_commands`
   are repository code; flatline runs them with the model vendors' keys and
   every `*_TOKEN` / `*_SECRET` / `*_API_KEY` variable stripped from the
   environment (`tools.ReducedEnv`). The model's own shell is not reduced.

### Three things the library port made necessary

- The format says `$schema` is required and the Go decoder enforces it, but
  the Python `afspec` — the one agent-fox runs — treats it as optional and
  omits it on write, so a pack straight out of `spec generate` fails to load
  in Go. flatline loads such a pack as if each file declared its canonical
  URI (from a temporary copy; the originals are untouched) and writes
  `tasks.json` back without the key when it was absent.

- The Go `TasksV1Json.Render()` omits the `## Test Commands` block that the
  Python renderer emits, and the profiles refer to it by name. flatline
  renders that block itself ([`TestCommandsBlock`](pack.go)).
- The Go `CompleteSubtaskStates` marks *dropped* subtasks done as well, which
  the Python `complete_subtask_states` does not and the format's state machine
  forbids. flatline walks `TransitionSubtask` per non-dropped subtask instead,
  and writes `tasks.json` alone with `afspec.MarshalJSON` — `Save` would also
  re-render `prd.md` and recompute `test_spec.json`'s coverage, which is more
  than a state update should touch.

## What each file does

| File | Role |
|---|---|
| [`main.go`](main.go) | Flags, spec resolution, model and credential pre-flight, wiring, exit codes. |
| [`pipeline.go`](pipeline.go) | The ordered steps, the retry ladder, landing, the journal. Read this first. |
| [`pack.go`](pack.go) | Loading and validating the pack, group order and completeness, marking subtasks done, context assembly, steering. |
| [`phases.go`](phases.go) | The AgentKit half: per-archetype tool scoping, the authorization guard, the three terminating tools. |
| [`prompts.go`](prompts.go) | The three profiles and the task prompts. |
| [`verify.go`](verify.go) | Running the pack's `test_commands` through `sh -c`; which of them gate what. |
| [`git.go`](git.go) | Every git command the program runs. |
| [`render.go`](render.go) | Commit and squash messages, the run summary. |
| [`flatline_test.go`](flatline_test.go) | The whole thing offline: a scripted model, a temporary repository, the real spec library. |
| [`testdata/specs/01_widget_counter`](testdata/specs/01_widget_counter) | A four-group fixture pack: tests · standard · checkpoint · wiring_verification. |

## Usage

```
flatline [flags] <spec>
```

`<spec>` is a spec directory, or a number / `NN_name` resolved under
`--specs-dir` (default `<dir>/.specs`). The pack must validate, must not be
sealed, superseded or archived, and must declare no cross-spec dependencies
unless `--assume-deps` is given.

| Flag | Default | What it does |
|---|---|---|
| `--dir` | `.` | The repository to work in. The file tools cannot reach outside it. |
| `--specs-dir` | `<dir>/.specs` | Where the `NN_name` directories live. |
| `--land` | `merge` | `merge`: squash each passing group into the current branch. `branch`: keep `feature/{spec}/{N}`, chained, and name the result after the spec. |
| `--final-branch` | `feature/{spec_id}_{spec_name}` | With `--land=branch`, the branch the finished work ends up on and is checked out at. Ignored by `--land=merge`, where the base branch already carries everything. |
| `--push` | off | Push what the run lands: the base branch after each merge, or the final branch when the groups are kept. With retries. Nothing is pushed otherwise. |
| `--model` | `$AGENTKIT_MODEL`, else `anthropic/claude-sonnet-5` | Any model in the catalog. |
| `--max-turns` | `300` | Per-session turn ceiling (agent-fox's coder default). |
| `--budget` | `20.00` | Per-session spend ceiling, in dollars (`max_budget_usd`). |
| `--max-cost` | none | Run spend ceiling; checked between groups, like agent-fox's circuit breaker. |
| `--max-retries` | `2` | Retries per group after the first attempt: three attempts in all. |
| `--session-timeout` | `45m` | Wall-clock ceiling per session. |
| `--check-timeout` | `10m` | Timeout for one test command. |
| `--verifier` | off | Run the informational verifier session after the last group. Its verdicts are printed and journaled, never acted on. (`--no-verifier` is kept for compatibility.) |
| `--assume-deps` | off | Treat the pack's cross-spec `dependencies` as already implemented instead of refusing the run. Each is reported as a warning and rendered into the coder's context as a `## Dependencies` table. |
| `--journal` | — | Append a JSONL record of every step to this file. |
| `--allow` | — | Extra programs the coder's shell may run. The pack's own test commands are always allowed. |
| `--show-text` | off | Print the model's prose as well as its tool calls. |
| `--verbose` | off | Tool calls, timing and cost diagnostics. |

| Exit | Meaning |
|---|---|
| `0` | Completed: every group landed and the final checks pass. |
| `1` | Failed: a step errored, or a group exhausted its retries (agent-fox's *stalled*). The stage is named on stderr; the last attempt is on `stalled/feature/{spec}/{N}` (with a numeric suffix if an earlier run already left one there). |
| `2` | Usage error, or a pack that does not validate. Nothing was branched. |
| `3` | The `--max-cost` ceiling was reached between groups. |
| `4` | Completed *dirty*: every group landed, but the final checks fail. |
| `130` | Interrupted. Whatever the current session wrote stays on its branch. |

### Requirements

- A clean working tree in `--dir`, on the branch the work should land on.
- A credential for the model's vendor: see the table in
  [`examples/README.md`](../README.md).
- **A checkout of `agent-fox-dev/spec` next to this repository**, because
  the spec library's published module path does not match its directory
  layout and `go get` cannot fetch it; see [`go.mod`](go.mod). This is also
  why `flatline` is a nested module: the library brings a YAML parser and a
  JSON Schema validator, and the root module is standard-library-only by test.

```bash
git clone https://github.com/agent-fox-dev/spec ../spec     # from the coder checkout
cd examples/flatline && go test ./...                        # no key needed
```

## How it is tested

The happy path costs money and rewrites a repository, so none of the test
suite goes near a model. The deterministic half is tested directly, and the
agent half runs against `provider/faux`, so the pipeline tests drive the
**real** `Run` — the real agent loop, the real file tools, a real temporary git
repository, the real spec library writing `tasks.json` — with a model that is
seven scripted turns.

- `TestPipelineEndToEnd` — four groups, one of them a checkpoint: four squash
  commits on `main`, every subtask `done`, no branches left, memory facts from
  group 1 in group 2's system prompt, the journal in the claimed order.
- `TestPipelineRetriesAGateFailureWithTheErrorInThePrompt` — the linter fails
  once; the second attempt's prompt carries the failure, the first attempt's
  branch is gone.
- `TestPipelineStallsWhenRetriesAreExhausted` — three failures: the group is
  blocked, the rest are blocked without running, `main` has not moved, the
  last attempt is on `stalled/…`, the subtasks are still `pending`.
- `TestPipelineSkipsACompletedGroup` / `TestPipelineLaunchesADoneGroupWhoseTestsFail`
  — the preflight's two outcomes.
- `TestPipelineKeepsBranchesWhenAsked` — the chain builds on itself, the work
  is named `feature/01_widget_counter` and checked out, and the summary says
  which branch to merge.
- `TestFinalBranchLeavesAnUnrelatedBranchAlone` / `TestFinalBranchRefusesTheBaseBranch`
  — naming the work never relabels an earlier run's branch and never moves the
  base branch; the run completes and says where the work is.
- `TestPipelineStopsAtTheCostCeiling`,
  `TestGateFailureIsRetriedAndVerifierIsInformational`,
  `TestPipelineRefusesADirtyTree`, `TestLoadPackRefusesDependenciesAndSealedSpecs`.
- `TestMarkGroupDoneWalksTheStateMachine` — dropped subtasks stay dropped;
  the file is the library's canonical encoding.
- `TestToolGuard` — `git commit`, `git -C . push`, `git checkout`, `git branch -D`
  are refused; `git log` and `go test` are not; a read-only session cannot
  `write_file`.

## What it does not do

- **Cross-spec dependencies.** agent-fox schedules a group behind the group
  of the other spec it depends on. flatline cannot, so a pack with a
  non-empty `dependencies` array is refused unless `--assume-deps` says the
  other specs are already in place — in which case the assumption is printed,
  journaled and shown to the coder, never silently made.
- **Parallelism, hot-loading, watch mode.** One spec, one branch, one session
  at a time.
- **Merge-conflict resolution.** agent-fox hands a conflicting squash to a
  merge agent. In a serial pass the base branch cannot move under a group, so
  a conflict means something else touched it; flatline stops and says so.
- **Session persistence.** Each session is a fresh agent; a crashed run is
  re-run, and the preflight skips the groups that already landed.
- **It is not a sandbox.** `go`, `make` and `uv` can run arbitrary code from
  the repository, and the coder's shell has operators. The guard checks every
  simple command on a line against the allowlist and the git rules, but it is
  a classifier over shell syntax, not a shell. Anything genuinely untrusted
  belongs in a container.

## Related

- [`cleaner`](../cleaner) — the same split (judgment in the model, everything
  else in Go), applied to one GitHub issue instead of one spec pack.
- [`codingagent`](../codingagent) — the smallest version of the
  agent-with-tools setup used here.
- [`testing`](../testing) — how to test agent code with `provider/faux`.
