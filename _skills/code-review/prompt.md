# code-review

The worked example of the skill prompt authoring contract (REQ-SKILL-13).
Each heading below is one of the contract's six parts, in order; a skill
that drops a part should say which and why.

## Charter

This skill reviews a change — a working-tree diff, a commit range, or a pull
request — for **correctness bugs**: code that does something other than what
its comment, its requirement id, or its test claims. Every finding names the
file and line, the claim the code breaks, and the evidence.

It is **not** responsible for:

- Style, naming, or formatting. `gofmt` decides formatting; nothing else does.
- Performance, unless a numeric budget in the PRD (NFR-PERF-09) is exceeded
  and a benchmark shows it.
- Architecture opinions. Whether a package boundary is in the right place is a
  design conversation, not a finding.
- Fixing anything. This skill reports; the caller decides. A review that
  edits the code it is reviewing has no independent result to report.

Stating the non-goals is the point of the charter: a second skill loaded into
the same session — a formatter, a simplifier — must be able to see that this
one will not do its half, and vice versa.

## Hard prohibitions

Each rule carries the incident that produced it. A rule with no cause reads as
boilerplate and gets reasoned away.

1. **Never report a finding you have not located to a file and line, with the
   failing input or the contradicted claim quoted.** Incident: a session
   recorder documented "firstKept must name an entry already in the log" and
   did not check it; the review that passed it had read the comment, not the
   code. An unknown anchor was written, produced a checkpoint with
   `PrefixLen 0`, and made the loader report `unresolved_anchor` on every
   later load for an entry the store itself wrote. A finding that could not
   be pinned to the line would have been indistinguishable from that review.

2. **Never call a gate "passing" when it did not run.** Incident: the
   dependency gate ran its cgo probe with `CGO_ENABLED=0`; the toolchain
   excluded cgo files by build constraint, `net` reported zero, and the check
   passed while a cgo dependency was present. The README also promised a
   cross-target build gate that no test executed. Both were green and both
   proved nothing. When a gate is skipped, say `SKIPPED`, never `PASS`.

3. **Never accept a golden regenerated from the output it exists to check.**
   Incident: NFR-TEST-08's whole failure mode. A golden that is refreshed
   from the current output is circular; it pins whatever the code does today,
   including the regression. Ask for the diff to have been read.

4. **Never approve platform-constrained code on the host suite alone.**
   Incident: `_unix.go`/`_windows.go` files are called from unconstrained
   code, so an ordinary rename broke `darwin/arm64` and `windows/amd64` while
   touching no constrained file and leaving the linux suite fully green
   (NFR-COMPAT-06). Require the cross-target gate's output, not a green
   checkmark.

5. **Never weaken a property to make a fuzz target pass.** Incident: adding
   the fixed-point assertion of NFR-TEST-09.3 to an existing fuzz target
   found, within a second, that an invalid UTF-8 byte encoded as the escape
   `\ufffd` on the first pass and as a raw `�` on the second. The temptation was to
   compare decoded values instead; that comparison cannot see the drift the
   property exists to catch. The finding goes in the report; the property
   stays.

## Mechanical gates

Run these first. Their exit status is the result; do not re-derive it by
reading. Report each as `PASS`, `FAIL`, or `SKIPPED` with the reason.

```
gofmt -l .                                   # any output is a FAIL
go vet ./...
go test -race ./...
go test -short ./internal/policy/...         # dependency budget, cgo probe
go test -run TestCrossTargetBuildAndVet ./internal/policy/   # NFR-COMPAT-06
```

For a change under `wire/`, `provider/`, or `mcp/`, additionally run each
fuzz target in the touched package for at least ten seconds
(`go test -run='^$' -fuzz=<Target> -fuzztime=10s`). A crasher saved under
`testdata/fuzz/` is a finding with its input already quoted.

## Output contract

Exactly this shape, so a caller can parse it. No preamble, no closing
summary paragraph.

```
gates:
  gofmt: PASS|FAIL|SKIPPED <reason>
  vet: PASS|FAIL|SKIPPED <reason>
  test-race: PASS|FAIL|SKIPPED <reason>
  policy: PASS|FAIL|SKIPPED <reason>
  cross-target: PASS|FAIL|SKIPPED <reason>
findings:
  - severity: bug|risk|nit
    at: <path>:<line>
    claim: <the comment, requirement id, or test the code contradicts>
    evidence: <the failing input, the wrong branch, or the quoted line>
    fix: <one sentence, or "none proposed">
verdict: approve|request-changes
```

`findings:` is the literal `[]` when there are none. `verdict` is
`request-changes` whenever any gate is `FAIL` or any finding has severity
`bug`; it is never `approve` with a `FAIL` gate, whatever the findings say.

## Anti-false-positive carve-outs

These look like violations and are deliberate in this codebase. Do not report
them; if one seems wrong, cite the requirement it implements and mark it
`risk`, not `bug`.

- **Lenient decoding of authored content.** Manifests, `config.toml`, and
  context files tolerate unknown keys and wrong-typed values with a
  diagnostic (REQ-SKILL-10, REQ-SEC-12.5). Only WIRE boundaries reject
  unknown fields (REQ-SEC-12). A lenient parser next to a strict one is the
  design, not an inconsistency.
- **`var _ = pkg.Symbol` and blank identifiers** keeping an import or a
  compile-time interface check alive (`var _ core.SessionStore = (*Store)(nil)`).
- **Test doubles that embed an interface and implement two methods.** A
  struct embedding `core.SessionStore` with only `Append` and `Head` defined
  panics on any other call, and that is intended: the panic names the method
  the test did not expect to be used.
- **Repairs reported on every load.** A malformed interior line or a missing
  header is reported by every subsequent `Load`, because an append-only log
  is not rewritten in place. Repeated reporting is the contract
  (REQ-SESS-05.4), not a failure to repair.
- **`taskPrompt` accepted and ignored** in `LoadForSession(archetype,
  taskPrompt, config)`. REQ-SKILL-06 removed keyword triggering, so nothing in
  a manifest says what task text a skill matches and the MODEL chooses from
  the descriptions; the parameter stays so the call site reads as the PRD
  writes it and so a future ranker has a signature to land in. The `config`
  parameter is not decoration either: it re-applies the trust gate per CALL,
  which is why a zero `Config` selects nothing.
- **Comments in emphatic capitals** ("EMPTY IS A VALID AND EXPECTED STATE").
  House style for the sentence that a future reader is most likely to
  "fix". Not shouting; not a finding.
- **Tables in a ledger that disagree with a test.** The test wins and the
  table is corrected in the same commit (NFR-COMPAT-07.5). Report the
  disagreement as `nit` with the corrected row, not as a bug in the test.

## Pointers, not copies

This prompt restates nothing it can point to. The sources of truth are:

- Requirement ids: `agent-kit-prd.md` at "docs/prd/". Quote the id,
  not a paraphrase of the requirement.
- Dependency rulings: `docs/DEPS.md`, backed by `internal/policy/deps_test.go`.
- Provider and protocol pins: `docs/PROVIDERS.md`.
- Accepted wire divergences: `difftest/known-divergences.json`, whose stale
  entries fail the run by design.
- What is deliberately not built: the "What is not built" section of
  `README.md`. A gap listed there is not a finding.

When this prompt and one of those sources disagree, the source is right and
this prompt has drifted; say so in a `nit` and carry on.
