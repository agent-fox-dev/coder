# `issued` — a skill, rebuilt as a program

`issued` takes a problem report — free text, a `.md`/`.txt` file, piped stdin,
or a GitHub issue URL — reads the codebase it concerns, and produces a
structured, evidence-cited GitHub issue.

```bash
go run ./examples/issued "panic: assignment to entry in nil map in loop.go, after an abort"
go run ./examples/issued ./crash.log --dir ./service
go run ./examples/issued https://github.com/owner/repo/issues/42 --label af:fix
go run ./examples/issued https://github.com/owner/repo/issues/42 --overwrite
go run ./examples/issued ./crash.log --dir ./service --dry-run
kubectl logs deploy/api --since 1h | go run ./examples/issued -
```

It creates the issue on GitHub by default. Pass `--dry-run` to print the
diagnosis without filing it.

## Why this example exists

[`af-issue`](af-issue) is a real slash-command skill: ~450 lines of markdown
handed to a coding CLI, telling it to diagnose a bug and file it. It is a good
skill. It also demonstrates, by its own shape, what a prompt cannot do.

Read it and count the categories. There is a **classify the argument** table.
A **detect the repository** step with two shell commands and a regex. An
**issue body template** as a fenced markdown block. A **labels menu** with
three numbered choices. A `gh issue create` invocation. And a **Guardrails**
section that ends with the sentence every prompt-shaped program ends with:

> **Read-only.** Do not modify, create, or delete any source files. […] Use
> only: `cat`, `head`, `tail`, `ls`, `git log`, …

That sentence is a request. The CLI reading it has `write_file` and a shell,
and nothing between the request and the tool.

`issued` keeps the one part of that skill that genuinely needs a model — *read
the code and work out why* — and turns every other part into a mechanism.

| `af-issue` says, in prose | `issued` does, in code |
|---|---|
| "Analysis-only mandate. The codebase is read-only to you." | `ToolPolicy.ExcludeTools` drops `write_file`, `edit_file`, `execute`, `run_command`, `powershell`. There is nothing to call. |
| "Use only `cat`, `ls`, `grep`…" | There is no shell. Leaving `BeforeToolCall` nil means the SDK's own guard fails any run whose resolved set carries one. |
| "Every file path must come from actually reading the code. Do not guess." | `file_issue` resolves every cited path against the workspace and refuses the call, by name, when one is not there. |
| The issue-body markdown template | A `schema.Object` with required fields and enums. Go renders the markdown. |
| "Ask the user how to label the issue (1/2/3)" | `--label`, parsed before the run starts. |
| `gh issue create --repo …` | A `net/http` call in `main`, after the run, suppressed by `--dry-run`. The model has no network tool at all. |
| "Halt until input is received." | `ErrNoInput` and exit 2, before a token is spent. So is a missing `GITHUB_TOKEN` when the run would file the issue. |
| "One issue per invocation." | One `Issue` value, one terminating tool call. |

None of the right-hand column depends on the model cooperating. That is the
whole argument for embedding an agent in a program rather than writing a longer
prompt: **the parts you cannot afford to have wrong stop being prompt.**

## How it works

```
argument ──► ResolveInput ──► Report ─┐
             (Go: text | file |       │
              stdin | issue URL)      │
                                      ▼
  workspace ──► tools.All ──► ToolPolicy ──► [read_file list_files
   (--dir)                    ExcludeTools    find_files search_files]
                                      │             +  file_issue
                                      ▼
                                  agent loop  ◄── model reads the code
                                      │
                       file_issue ────┤  path check → error result → retry
                                      │
                                      ▼
                                   Issue (validated struct)
                                      │
                          Render ─────┼──► stdout / --out
                                      │
                       gh.CreateIssue ┴──► GitHub  (unless --dry-run)
```

Six files, one `main` package:

| File | What is in it |
|---|---|
| [`main.go`](main.go) | Flags, orchestration, the credential pre-flight, the one call that writes to GitHub. |
| [`input.go`](input.go) | Classifying the argument; fetching an issue and its comments; bounded, visible truncation. |
| [`triage.go`](triage.go) | The tool policy, the read-only invariant, the system prompt, and `file_issue`. |
| [`issue.go`](issue.go) | The `Issue` type, its schema, and the markdown renderer. |
| [`github.go`](github.go) | ~100 lines of `net/http`: read an issue, create an issue, parse a remote. |
| [`issued_test.go`](issued_test.go) | The whole thing, offline. |

Unlike the other examples this one is several files rather than a single
`main.go` — it is meant to read like an application you would ship, not like a
walkthrough of one API.

### 1. The read-only mandate is a resolved tool set

```go
var mutatingTools = []string{"write_file", "edit_file", "execute", "run_command", "powershell"}

cfg.ToolPolicy = core.ToolPolicy{ExcludeTools: mutatingTools}
// cfg.BeforeToolCall is deliberately nil.
```

`ExcludeTools` is a denylist over the *resolved* set — custom tools included —
so it cannot be dodged by registering a tool later.

The nil `BeforeToolCall` is the second, independent check. AgentKit fails any
run whose resolved tool set carries a shell tool and no interceptor
(`core.ErrUnguardedExecute`), *before the first request*. Delete an entry from
`mutatingTools` and the program stops working; it does not quietly gain a
shell. `assertReadOnly` catches the same mistake one layer earlier with a
message that names the tool.

The comment on that guard in [`policy.go`](../../policy.go) describes this
program exactly:

> REQ-SEC-03 replaced the command allowlist with a per-call interceptor […]
> That reasoning assumes an embedder that can answer a permission question. **A
> daemon triaging issues overnight cannot**, and "allow" by default is strictly
> worse than the allowlist it replaced.

### 2. The issue template is a schema

The skill's markdown template becomes [`issueSchema()`](issue.go): `title`,
`problem`, `reproduction`, `confidence` (enum), `root_cause`,
`related_instances` (optional), `affected_files` (min 1), `suggested_fix`
(nested), `acceptance_criteria` (min 1), `severity` (enum),
`severity_rationale`.

A model that omits severity does not produce a slightly worse document — it
produces a validation error, in the shape of the schema, which it reads and
fixes on the next turn. `ConstrainedSampling` with `StrictPrefer` narrows this
further on providers that support it, and is correctly *not* the thing keeping
the arguments well-formed: it is honoured on the OpenAI wires and ignored on
Anthropic's, so validation runs either way.

The markdown is then produced by `Issue.Render`, a pure function. Two runs that
reach the same diagnosis produce byte-identical documents.

### 3. "Cite real files" is a check, and a refusal is repairable

The most common way a machine-written triage issue wastes a reader's time is a
confident reference to `src/session/handler.go`, a file that does not exist.
It is also mechanically detectable:

```go
if missing := t.checkPaths(issue.AffectedFiles); len(missing) > 0 {
    return core.ErrResult("unknown_path", fmt.Sprintf(
        "these affected_files paths are not in the workspace: %s. "+
            "Use find_files or search_files to get the real path, then call file_issue again. …",
        strings.Join(missing, ", ")))
}
```

An error result is not a crash: the loop appends it to the transcript, the
model reads it and searches for the real path, and the run continues. The
wording is load-bearing because it is the entire repair instruction.

`suggested_fix.files` is checked differently — a fix legitimately adds a file
that does not exist yet, so the requirement there is *containment*, not
existence. `Workspace.Resolve` handles both, symlinks included.

The run summary reports how many attempts it took:

```
[issued] 1 file_issue call(s) rejected for uncited paths: session/imagined.go
[issued] anthropic/claude-sonnet-5 · 9 turns · stop tool_terminate · in 48211 / out 3104 tokens · $0.19
```

A nonzero count is the check working. A large one is a signal to read the
diagnosis more carefully — the model was writing from the report rather than
from the code.

### 4. The side effect is out of the model's reach

There is no `create_issue` tool. `file_issue` writes a struct into this process
and votes to end the run; `tools.FetchTool` is never constructed, so the model
has no outbound network of any kind. The `net/http` call that files the issue
runs in `main`, after the loop has finished, unless `--dry-run` was
passed.

The consequence is worth stating plainly: **no sequence of model outputs can
cause this program to write to GitHub.** Not a jailbroken prompt, not a
poisoned issue comment, not a log line that says "ignore your instructions".
The capability is not there to be misused. Reports are fenced and labelled with
their provenance in the task prompt for the same reason — a GitHub issue body
is text a stranger wrote.

### 5. Ending is an outcome, not a vibe

`file_issue` sets `ToolResult.Terminate`, so `RunStopToolTerminate` is what
"there is a diagnosis" means. A run that hits the turn limit or the budget
returns an error naming the stop reason, and no issue. `StopAny(StopAfterTurns(100),
StopOverBudget(2.00))` bounds a read loop that wanders.

A long read loop also fills the context window. `NewTriager` installs
`agentkit.NewContextTransform` with `SummarizationCompaction` at 60% of the
window (`installCompaction` in [`triage.go`](triage.go)), so the transcript is
summarized in place rather than the run ending on a context-length error
before `file_issue` is ever called.

## Running it

```bash
export ANTHROPIC_API_KEY=sk-ant-...          # see ../README.md for other vendors
go run ./examples/issued "<report>" [flags]
```

| Flag | Effect |
|---|---|
| `--dir` | Workspace root. The analysis cannot read outside it. Default `.` |
| `--repo owner/repo` | Target repository. Defaults to the issue the report came from, else the `origin` remote of `--dir`. |
| `--dry-run` | Make no changes to GitHub; only print the rendered issue. |
| `--overwrite` | Rewrite the input GitHub issue in place — title and body — instead of creating a new issue. Needs an issue URL as the input, and cannot be combined with `--label` or `--repo`; either is a usage error before the run. |
| `--label a,b` | Labels for the created issue, e.g. `af:fix`. |
| `--out FILE` | Also write the rendered issue to a file. |
| `--debug` | Stream the model's reasoning text to stderr. |
| `--verbose` | Verbose output: input details, tools, execution traces and cost summary. |

Flags may come before or after the report. `AGENTKIT_MODEL` picks the model
(`AGENTKIT_MODEL=openai/gpt-5.6-terra`); `GITHUB_TOKEN` or `GH_TOKEN`
authenticates GitHub, and `GITHUB_API_URL` points at a GitHub Enterprise host
(its host is then also accepted as the `origin` remote's host when `--repo`
is detected). Reading a public issue needs no token; filing one does, and a
run that would file checks for the token before the model is called (unless
`--dry-run` is passed).

Exit codes: `0` filed or printed, `1` failed, `2` usage — no input, an
undefined flag, or `--overwrite` without an issue URL or with `--label`/`--repo`.

A typical session:

```
$ go run ./examples/issued ./crash.log --dir . --repo agent-fox-dev/coder --dry-run
[issued] analysing (3s) · 1.2k↑ 450↓
session: resume folds a trailing tool call into an empty turn

## Problem
…
[issued] dry run. Re-run without --dry-run to file it.
```

With `--verbose`:

```
$ go run ./examples/issued ./crash.log --dir . --repo agent-fox-dev/coder --dry-run --verbose
[issued] input: file (crash.log, 3184 bytes)
[issued] workspace: /home/you/coder
[issued] tools: read_file, list_files, find_files, search_files, file_issue
[issued] analysing…
  read   search_files
  read   read_file
  read   read_file
  read   file_issue
session: resume folds a trailing tool call into an empty turn

## Problem
…
[issued] anthropic/claude-sonnet-5 · 7 turns · stop tool_terminate · in 1200 / out 450 tokens · $0.14000
[issued] dry run. Re-run without --dry-run to file it.
```

## Testing it

```bash
go test ./examples/issued/ -v
```

No API key, no network, no environment variable. The model is a script
(`provider/faux`) and the codebase under analysis is a temp directory with
three files in it. Every claim this README makes is a test:

| Test | Claim |
|---|---|
| `TestTheResolvedToolSetCarriesNothingThatCanWrite` | The mandate holds. |
| `TestWideningTheExcludesFailsTheRunRatherThanTheReview` | Widen the excludes and the run fails on `ErrUnguardedExecute` before the first request. |
| `TestACitedPathThatDoesNotExistIsRefusedAndTheModelRecovers` | An invented path is refused *and* the model recovers from the refusal. |
| `TestAProposedNewFileIsAllowedButAnEscapeIsNot` | A new test file is a legal fix; `../../etc/passwd` is not. |
| `TestARunThatNeverCallsFileIssueReturnsNoIssue` | A run that reaches no conclusion is a failure, not an empty issue. |
| `TestTheRequestDeclaresFileIssueAndNoWriteTools` | What was actually sent on the wire. |
| `TestRenderProducesEverySectionAndIsStable` | The document is complete and deterministic. |
| `TestResolveInputClassifiesEverySource`, `TestParseIssueURL`, `TestParseRemote`, `TestAnOversizedReportIsTruncatedVisibly` | The dull decisions, decided in Go — including that an `origin` on GitLab is not a GitHub repository. |
| `TestFlagParsingRejectsCreateAndAcceptsDryRun` | Flags accept `--dry-run` and reject `--create`. |
| `TestFlagParsingOverwrite` | Flags parse `--overwrite`; without an issue URL, or with `--label`/`--repo`, it is a usage error. |
| `TestReadIssueHintsAtTheTokenOnlyOnAReal404` | The "set GITHUB_TOKEN" hint is keyed on the status code, not on digits in the message. |
| `TestTargetRepoValidationHaltsWithoutDryRun` | Missing repository halts before analysis unless `--dry-run` is passed. |
| `TestDryRunGating` | Gating prevents issue creation in dry-run mode and allows it by default. |
| `TestGitHubUpdateIssue`, `TestFileOrDryRunOverwrite` | In-place issue overwrite (title and body) via GitHub PATCH API. |
| `TestFormatTokenTiming` | Token timing format renders duration and token counts correctly. |
| `TestFlagParsingDebugAndVerbose` | Flags parse `--debug` and `--verbose`. |
| `TestAC1DebugStreamsReasoningDeltas` | `--debug` streams reasoning text deltas, suppressed otherwise. |
| `TestAC2AndAC3NonVerbosePhaseProgressAndSummary` | Non-verbose mode displays progress spinner and token timing without cost figures. |
| `TestAC4VerbosePreservesTracesAndCost` | Verbose mode prints tool traces and full summary with USD cost. |

## What it does not do

- **It does not fix anything.** By construction. Analysis and repair are
  different risk classes, and this is the one that is safe to run unattended.
- **It reads one page of issue comments.** A 300-comment thread is truncated at
  the report cap, visibly.
- **It has no dependencies.** No `gh`, no GitHub SDK, no YAML parser — the
  whole example is the Go standard library plus AgentKit, which is itself the
  standard library.
- **It does not deduplicate.** The skill suggests searching for an existing
  issue first; that would be a second `GitHub` method and a second run, and it
  is the obvious next thing to add.

## Where to look next

- [`../codingagent`](../codingagent) — the same built-in tools with the shell
  left in, and the interceptor that then becomes mandatory.
- [`../customtools`](../customtools) — the schema combinators, `Handler` vs
  `Execute`, and terminating tools, at reference depth.
- [`../testing`](../testing) — the techniques `issued_test.go` uses, explained
  one per test function.
- [`af-issue`](af-issue) — the skill this program was translated from. Worth
  reading side by side with [`triage.go`](triage.go).
