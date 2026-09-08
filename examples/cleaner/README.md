# cleaner — an autonomous GitHub issue fixer

Give it an issue URL. It diagnoses the problem, writes the fix on a branch,
verifies it with the project's own checks, opens a pull request, and reports
what it did on the issue.

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run ./examples/cleaner --dir ~/src/widgets https://github.com/acme/widgets/issues/42
```

It is the AgentKit implementation of a coding-CLI skill — the `af-fix` prompt
sitting next to this file, which a human would invoke as `/af-fix <issue-url>`
inside Claude Code or a similar agentic CLI. Same workflow, same outputs;
the difference is where the workflow *lives*.

In the skill, all ten steps are prose the model is asked to follow. Here the
model does the two steps that need judgment — **diagnose** and **implement** —
and Go does the other eight: parsing the URL, fetching the issue, checking the
working tree, naming the branch, running the test suite, committing, pushing,
opening the pull request, and writing every comment. The model cannot skip a
step it finds boring, cannot report a test run it did not do, and cannot decide
mid-run that the branch should be pushed to `main`.

## Why this shape

An autonomous fixer has one hard problem: **the thing that decides whether the
work succeeded must not be the thing that did the work.** A prompt-only agent
grades its own homework — it runs the tests, and it also writes the sentence
"All existing tests pass: ✅". Nothing checks that those two are related.

So the program is split along that line:

```
 deterministic Go                        the model
 ─────────────────                       ─────────
 validate the URL
 fetch the issue + linked PRs
 run the baseline suite
                            ───────►     analyse (read-only tools)
 create the branch          ◄───────     Analysis{classification, root cause,
 post the analysis comment                        approach, files, assumptions}
                            ───────►     implement (read/write tools + shell)
 re-run the suite           ◄───────     Implementation{summary, changes, tests}
 check the diff is non-empty
 commit · push · open the PR
 post the summary comment
```

The two arrows are the only places a model is involved, and both hand back a
**validated struct** rather than prose: each phase ends by calling a terminating
tool (`submit_analysis`, `submit_implementation`) whose JSON schema the loop
enforces. A malformed answer comes back to the model as a validation error it
can correct, instead of arriving here as text that has to be parsed by hope.

Everything the program then writes — the issue comments, the commit message,
the pull-request body — is rendered by a pure function in [`render.go`](render.go)
from data it already has. That is why the verification section of a summary
comment cannot say ✅ for a suite that was never run: the function takes a
`VerifyResult`, not a boolean.

## What each file does

| File | Role |
|---|---|
| [`main.go`](main.go) | Flags, model resolution, credential pre-flight, wiring, exit codes. |
| [`pipeline.go`](pipeline.go) | The ordered steps, the failure handling, the run journal. Read this first. |
| [`phases.go`](phases.go) | The AgentKit half: per-phase tool scoping, the authorization guard, the two terminating tools. |
| [`prompts.go`](prompts.go) | The system and task prompts, and how issue context is rendered into them. |
| [`git.go`](git.go) | Every git command the program runs, plus branch naming. |
| [`hub.go`](hub.go) | GitHub through the `gh` CLI, behind an interface — with dry-run and offline implementations. |
| [`verify.go`](verify.go) | Detecting and running the project's own quality command. |
| [`render.go`](render.go) | Comments, commit messages and PR bodies, as pure functions. |
| [`cleaner_test.go`](cleaner_test.go) | The whole thing, offline: no key, no network, no `gh`. |

## The AgentKit parts

Six SDK features carry this program, and each is doing a job you would
otherwise have to do with prompt text and hope:

**Per-phase tool scoping.** The analysis phase is registered with
`read_file`, `list_files`, `find_files`, `search_files` and a shell; the
implementation phase also gets `write_file`, `edit_file` and `run_command`. The
read-only phase is read-only because the tools to write are *not registered*,
not because the prompt asks nicely.

**A workspace root.** `tools.NewWorkspace(dir)` resolves every path a file tool
is handed against one directory, symlinks included, so `../../.ssh/id_rsa` is
refused by the tool rather than by a paragraph in the system prompt.

**A layered authorization boundary.** `agentkit.RestrictedPolicy` supplies the
floor — an allowlist of program names, no shell operators. On top of it,
`toolGuard` in [`phases.go`](phases.go) adds the two rules that are specific to
*this* application: the agent may not run mutating `git` subcommands (the
pipeline owns the branch, the commit and the push), and may not run `gh` at all
(every write to the issue goes through the audited comment path). A refusal
comes back to the model as a blocked tool result, so it adapts instead of dying.

**Terminating tools with schemas.** `submit_analysis` and
`submit_implementation` set `ToolResult.Terminate`, which ends the phase the
moment the structured answer arrives — and the stop policy names them too, so
`StopWhenToolCalled` is the intended ending rather than the turn ceiling.

**Bounds that are not the model's choice.** Every phase runs under
`StopAny(StopAfterTurns(n), StopOverBudget(usd), StopWhenToolCalled(...))`. A
phase that never reaches its terminating tool stops anyway, and the program
reports that as a failed run rather than as a fix.

**Streaming.** Tool calls, results and costs are printed as they happen, so a
ten-minute autonomous run is legible while it is happening — and `RunResult`
still carries the usage and the stop reason afterwards.

## Where this corrects the `af-fix` skill

Writing the skill out as a program surfaced five ordering and logic problems.
Each is a place where the prompt, read literally, does something incoherent.

**1. It opens a pull request *and* squash-merges the branch itself.**
Step 9.3 creates a PR whose body says `Closes #N`; step 9.4 then checks out
`main`, squash-merges the branch locally, and step 9.5 pushes `main`. The
squash commit has a different hash from anything on the branch, so GitHub
cannot recognise it as the pull request's merge: you are left with an open PR
whose changes are already on the default branch, and a review that can no
longer block anything. These are alternatives, not steps. `cleaner` makes them
a flag: `--land=pr` (default), `branch`, `merge` or `none`.

**2. It posts to the issue before checking whether it can work.**
The analysis comment goes up in step 5; the dirty-working-tree check is step
6.1, and the branch-already-exists check is step 6.3. So the most common
failure — "you have uncommitted changes" — leaves a public comment describing
work that never began. Here every check that can refuse the run happens first,
before the issue is even fetched.

**3. It promises not to stop, then stops.**
The header declares a "single-pass mandate: complete all steps without halting
for confirmation". Step 6.3 then halts and asks the operator whether to
force-push over an existing branch — inside a workflow that is, by design,
running unattended. Re-running on the same issue is *normal* (the first
attempt was rejected in review), so `cleaner` picks the next free name:
`fix/issue-42-slug`, `fix/issue-42-slug-2`, and so on. Force-pushing over
someone else's branch is not something to do on a guess.

**4. It says the tests must pass, having already recorded that they don't.**
Step 3.3 runs the suite and says to "note the failures" if it is red; step 7.5
then requires that "all existing tests still pass" before continuing. On a
repository with a pre-existing failure those two instructions cannot both be
satisfied, and the model has to pick one silently. `cleaner` measures the
baseline, tells the implementation phase about it, and compares: green→green
is a pass, red→green is reported as *"passes now; it was already failing"*, and
red→red stops the run and leaves the branch for a human.

**5. Its final summary is written before the facts are in.**
Step 8 posts "Fix Implemented — All existing tests pass ✅, no regressions ✅"
*before* step 9 commits, pushes or opens anything, so the summary cannot
contain the PR link and the tick-list is a template rather than a measurement.
Step 10's banner then prints "✅ Issue #N fixed and merged to main" even on the
paths where the push failed and the PR was skipped. Here the summary is posted
last, quotes the actual exit status of the actual verification run, and the
final banner says which step failed when one did.

Two smaller ones: the skill hardcodes `main` as the base branch in three
separate commands (`cleaner` asks the remote for its default branch), and it
delegates step 7 to `_templates/prompts/coding.md`, a file that does not exist
in this repository — a dangling reference in a prompt is a silent no-op.

## Usage

```
cleaner [flags] https://github.com/{owner}/{repo}/issues/{number}
```

| Flag | Default | What it does |
|---|---|---|
| `--dir` | `.` | The repository to work in. The file tools cannot reach outside it. |
| `--land` | `pr` | `pr` · `branch` (push, no PR) · `merge` (squash locally) · `none` (commit only). |
| `--dry-run` | off | Make no *remote* changes: nothing is pushed, no pull request is opened, and comments are printed instead of posted. The branch and the commit are still made locally — the implementation phase edits real files, so containing them on a branch you can delete is safer than leaving them loose. |
| `--model` | `$AGENTKIT_MODEL`, else `anthropic/claude-sonnet-5` | Any model in the catalog: `openai/gpt-5.6-terra`, `ollama/qwen3-coder`, … |
| `--max-turns` | `40` | Per-phase turn ceiling. |
| `--budget` | `5.00` | Per-phase spend ceiling, in dollars. |
| `--verify` | detected | The command that decides success. Detection order: `make check`, `make test`, `go test ./...`, `npm test`, `pytest`, `cargo test`. |
| `--no-verify` | off | Run nothing. The result is then reported as **unverified** — not as a pass. |
| `--verify-timeout` | `10m` | Timeout for one verification run. |
| `--issue-file` | — | Read the issue from a JSON file instead of GitHub. Offline, and implies `--dry-run`. |
| `--journal` | — | Append a JSONL record of every step to this file. |
| `--allow` | — | Extra programs the implementation phase's shell may run. |
| `--show-text` | off | Print the model's prose as well as its tool calls. |
| `--push-attempts` | `4` | Push retries, with exponential backoff. |

Exit codes, because this is meant to be run by something other than a human:

| Code | Meaning |
|---|---|
| `0` | Fixed. Verified, committed, landed as `--land` asked. |
| `1` | Failed. The stage is named on stderr and on the issue. |
| `2` | Usage error. Nothing was fetched, nothing was posted. |
| `3` | Stopped on purpose: the issue is ambiguous, and a question was posted to it. |
| `4` | Code was written but the checks do not pass. The branch is left for a human. |

### Requirements

- The [`gh` CLI](https://cli.github.com), authenticated (`gh auth status`) —
  unless you use `--issue-file`.
- A clean working tree in `--dir`.
- A credential for the model's vendor: see the table in
  [`examples/README.md`](../README.md).

### Try it without touching anything

```bash
# Read a canned issue from disk, run both phases for real, write nothing:
go run ./examples/cleaner \
  --issue-file ./examples/cleaner/testdata/issue-1.json \
  --dir /path/to/a/clean/checkout --dry-run \
  https://github.com/acme/widgets/issues/1
```

And with no API key at all:

```bash
go test ./examples/cleaner/
```

## How it is tested

The happy path costs money and mutates a GitHub repository, so almost none of
the test suite goes near either. The deterministic half is tested directly, and
the agent half runs against `provider/faux`, AgentKit's scripted provider — so
`TestPipelineEndToEnd` drives the **real** pipeline (the real agent loop, the
real file tools, a real temporary git repository, both issue comments) with a
model that is three scripted turns.

That makes the interesting failures testable, which is the point:

- `TestPipelineRefusesToLandAFailingChange` — the suite fails after the change;
  nothing is committed and the failure is posted to the issue.
- `TestPipelineRefusesAnEmptyChange` — the model reports a fix and changed no
  files; the run fails instead of committing an empty tree.
- `TestPipelineRefusesADirtyTree` — pre-flight stops before anything is fetched
  or posted.
- `TestPipelineStopsOnAmbiguity` — the analysis asks a question; no branch, no
  commit, one comment.
- `TestPipelineDegradesWhenThePullRequestFails` — the branch is pushed and the
  change is verified, so a PR that could not be opened is a warning, not a
  failed run.
- `TestToolGuard` — `git commit`, `git -C . commit`, `git push` and `gh` are
  refused; `git log` and `go test` are not.
- `TestVerificationLinesCannotOverclaim` — the reporting functions cannot print
  a green tick for a run that did not happen.

## What it does not do

Stated rather than left to be discovered:

- **No review loop.** One analysis, one implementation. If the checks fail, the
  run stops and leaves the branch; it does not iterate against the failure.
  (The seam is there: `Brain` is an interface, and an implementation that loops
  on `VerifyResult` would be a drop-in.)
- **No session persistence.** Each phase is a fresh agent. A crashed run is
  re-run from the start, not resumed — `agentkit.OpenSession` is what you would
  add here, and [`examples/session`](../session) shows how.
- **It does not look at CI.** Verification is the local command only.
- **The shell allowlist is a floor, not a sandbox.** `go` and `make` alone can
  run arbitrary code from the repository. Anything genuinely untrusted belongs
  in a container, and this program does not make one.
- **It is not a substitute for review.** Every comment it posts says so.

## Related

- [`examples/issued`](../issued) — the `af-issue` skill prompt: the same
  workflow one step earlier, filing the issue this program consumes.
- [`examples/codingagent`](../codingagent) — the smallest version of the
  agent-with-tools setup used here.
- [`examples/customtools`](../customtools) — the schema combinators and the
  terminating-tool pattern, on their own.
- [`examples/testing`](../testing) — how to test agent code with
  `provider/faux`.
