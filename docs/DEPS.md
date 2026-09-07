# Dependency ledger

REQ-GO-11 / REQ-GO-13. The root module requires nothing outside the Go standard
library, and that policy is a **test**, not this file:
`internal/policy/deps_test.go` walks the transitive build graph of `./...` and
fails on any module absent from its `allowedModules` map. This file holds the
rulings behind that map. When the two disagree, the test wins and this file is
corrected in the same commit (NFR-COMPAT-07.5's rule, applied to the dependency
surface).

The file holds bindings only. Per-cycle narrative belongs in commit messages; a
ledger that grows a section per cycle stops being read and therefore stops
binding.

## Allowlist

Derived from `allowedModules`. Every row states what the module buys and why
hand-rolling is not credible; a row that cannot say so is not a row.

| Module | Buys | Why not hand-roll | Ruling |
|---|---|---|---|
| `github.com/agentfox/agentkit-go` | the module under test | — | R1 |

That is the whole list. Nested modules (`difftest/`) carry their own `go.mod`
and their own budget; they do not appear here because they do not appear in the
root's build graph.

## Rulings

Decisions about the dependency surface are recorded once and are not
re-litigated. Re-asking a settled question is itself a failure mode
(NFR-COMPAT-07.3).

- **R1 — stdlib only, by test.** The root module's budget is the Go standard
  library. The gate is `TestNoUnapprovedModules`; adding a module is an edit to
  `allowedModules` with a reason string, and that edit is the review gate. A
  build tag or a sub-package does not confine a dependency — it still lands in
  `go.mod`, `go.sum`, `go list -m all` and every downstream SBOM. The only
  mechanism in Go that confines one is a nested module.
- **R2 — cgo is rejected regardless of allowlist status.** `TestNoCgoOutsideStdlib`
  fails on any non-stdlib package shipping cgo files, because cgo breaks the
  cross-target gate of NFR-COMPAT-06 (`TestCrossTargetBuildAndVet`:
  linux/amd64, linux/arm64, darwin/arm64, windows/amd64). The check runs with
  `CGO_ENABLED=1` and that is load-bearing: with cgo off the toolchain excludes
  cgo files by build constraint, so the probe reports zero and passes while the
  dependency is present. `TestCgoProbeIsArmed` guards the guard.
- **R3 — TOML is hand-rolled.** `internal/toml` reads the subset that manifests
  and `config.toml` use (strings, booleans, integers, string arrays, tables,
  arrays of tables) and reports what it does not read as a diagnostic that skips
  the key (REQ-SKILL-10). A full TOML library would buy floats, dates, inline
  tables and multi-line strings, none of which a manifest needs, at the cost of
  the first non-stdlib module. Settled; do not re-open on the strength of a
  manifest wanting a float.
- **R4 — JSON at trust boundaries is hand-rolled.** `wire` parses untrusted
  bytes with bounds applied before allocation, duplicate-key rejection and
  literal preservation (REQ-SEC-11/12). `encoding/json` accepts duplicate keys
  silently and reads a declared length before it bounds it; no third-party
  parser was evaluated because the requirement is the bounds, not the speed.
- **R5 — vendor SDKs stay out of the root.** The provider packages speak each
  API over `net/http` directly. A vendor SDK would pin the root to that
  vendor's release cadence and transitive tree, and NFR-TEST-06's reference
  bodies — the one place a vendor SDK is wanted — belong in the nested
  `difftest/` module for exactly that reason.
- **R6 — a new dependency needs three answers in its reason string.** What it
  buys; whether hand-rolling is credible; where the ruling is recorded (a new
  R-number here). `TestNoUnapprovedModules` prints this checklist in its
  failure message so the answer is written at the moment the question is
  asked.
