# Agent Instructions

Instructions for coding agents (Cursor, Claude Code, Codex, etc.) working on
this repository. Treat this file as mandatory policy for every coding session.

## Understand Before You Code (MANDATORY)

Before making any changes, orient yourself:

1. **Read `README.md`** for project overview and quick-start.
2. **Read `.specs/steering.md`** if it exists — project-level directives that
   apply to all agents and skills. Follow any instructions found there.
3. **Read ADRs and errata** in `docs/` for architectural context.
4. **Explore the codebase:** `<main_package>/` is the main package, `<test_directory>/` has
   unit, property, and integration tests. Their location is language dependent.
5. **Check git state:** `git log --oneline -20`, `git status --short --branch`.

**Important:** Read all documents and code in depth — don't skim.

**Important:** Only read files tracked by git. Skip anything matched by
`.gitignore`. When in doubt, run `git ls-files` to see what's tracked.

Do not implement anything before completing these steps.

## Project Structure

```
<main_package>/         # Main package
<test_directory>/       # Tests directory
docs/                   # Documentation
.specs/                 # Specs to be implemented
.specs/archive/         # Old specs. Ignore for coding tasks, except for reference
```

## Spec-Driven Workflow

This project uses spec-driven development. Specifications live in
`.specs/NN_name/` (numbered by creation order) and contain:

- `prd.md` — product requirements, goals, tech stack, high-level design
- `requirements.json` — EARS-syntax acceptance criteria, execution paths, external API contracts, glossary
- `test_spec.json` — language-agnostic test contracts
- `tasks.json` — implementation plan with subtask states and test commands
- `architecture.md` — (optional) detailed architecture

Cross-reference `external_apis` in `requirements.json` against installed
libraries — API signatures in specs may be unverified assumptions.

## Quality Commands

| Command | What it does |
|---------|-------------|
| `make check` | Run lint + all tests (use before committing) |
| `make test` | Run all tests (`uv run pytest -q`) |

Run the full quality suite before committing:

```
make check
```

**Important:** If `make check` or `make test` are not present, look for language specific test suites.

## Git Workflow

- **Branch from `main`: `feature/<descriptive-name>`.
- **Never commit directly** to `main`.
- **Conventional commits:** `<type>: <description>` (e.g. `feat:`, `fix:`,
  `refactor:`, `docs:`, `test:`, `chore:`).
- **Commit discipline:** only commit files relevant to the current change.
- **Never add `Co-Authored-By` lines.** No AI attribution in commits — ever.
- **Feature branches are local-only** — do not push them to origin. Only `main` is pushed to the remote.

## Scope Discipline

- Focus on one coherent change per session.
- Do not include unrelated "while here" fixes.
- Priority: fix broken behavior before adding new behavior.

## Documentation

- **PRDs** live in `docs/prd/NN-imperative-verb-phrase.md`. To choose NN,
  list existing files, find the max numeric prefix, and use the next number
  zero-padded to two digits for consistency (three digits once past 99).
- **ADRs** live in `docs/adr/NN-imperative-verb-phrase.md`. To choose NN,
  list existing files, find the max numeric prefix, and use the next number
  zero-padded to two digits for consistency (three digits once past 99).
- **Errata** live in `docs/errata/NN_snake_case_topic.md` — for spec
  divergences. NN is the spec number the erratum relates to (e.g.
  `28_github_issue_rest_api.md` for spec 28). For project-wide errata not
  tied to a specific spec, omit the numeric prefix.
- **Other docs** live in `docs/{topic}.md`.
- When you add or change user-facing behavior, public APIs, configuration, or
  architecture, update the relevant documentation in the same session.

## Session Completion

A session is not complete until:

1. `make lint` and `make test` passes (no regressions).
2. Changes are committed with a clear conventional commit message.
3. Changes are merged into `main` locally.
4. `git status` shows a clean working tree.
5. You provide a brief handoff note summarizing what was done and what remains.