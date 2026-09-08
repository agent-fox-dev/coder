package main

import (
	"fmt"
	"strings"
)

// The system prompts are short and the task prompts carry the context. That
// split is deliberate: the system prompt is the role, which does not change
// between issues, while everything an individual run needs — the issue, the
// baseline, the plan — belongs in the turn, where it is visible in the
// transcript and in the session log.

const analysisSystemPrompt = `You are a senior engineer diagnosing a GitHub issue in a repository you have read access to.

You cannot modify anything in this phase: you have read, list, find and search tools, and a shell restricted to read-only programs. Attempting to write is blocked.

Work from evidence. Every file, function and line range you cite must come from a file you actually read. Distinguish the symptom from the root cause, and fix the root cause — never a band-aid. Prefer the search and read tools to shell commands.

Finish by calling submit_analysis exactly once.`

const implementSystemPrompt = `You are a senior engineer implementing an agreed fix on a feature branch.

The diagnosis and the plan are already done and are given to you; follow them unless reading the code shows they are wrong, in which case say so in your summary and do the correct thing instead.

Rules that matter here:
- Test first. Add or update a test that fails for the current code, then make it pass.
- Follow the conventions already in the repository: its style, its test framework, its file layout.
- Minimal and correct. No unrelated "while here" cleanups — they make the change harder to review and harder to revert.
- Update the documentation in the same change when you alter user-facing behaviour, a public API, or configuration.
- Run the project's own checks before you finish, and fix what they report.

git commit, git push and gh are not available to you: the surrounding program owns the branch, the commit and everything posted to GitHub.

Finish by calling submit_implementation exactly once.`

// analysisPrompt renders the issue, its comments and any linked pull requests
// into the first user turn.
func analysisPrompt(in AnalysisInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Diagnose issue #%d in %s and plan the fix.\n\n", in.Ref.Number, in.Ref.Slug())
	fmt.Fprintf(&b, "## Issue #%d: %s\n\n", in.Ref.Number, in.Issue.Title)
	fmt.Fprintf(&b, "URL: %s\n", in.Issue.URL)
	fmt.Fprintf(&b, "Author: @%s\n", in.Issue.Author.Login)
	if labels := in.Issue.LabelNames(); len(labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(labels, ", "))
	}
	fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(in.Issue.Body))

	for i, c := range in.Issue.Comments {
		fmt.Fprintf(&b, "\n### Comment %d by @%s\n\n%s\n", i+1, c.Author.Login, strings.TrimSpace(c.Body))
	}

	for _, pr := range in.LinkedPRs {
		fmt.Fprintf(&b, "\n### Linked pull request #%d: %s\n\n%s\n", pr.Number, pr.Title, strings.TrimSpace(pr.Body))
		if len(pr.Files) > 0 {
			paths := make([]string, 0, len(pr.Files))
			for _, f := range pr.Files {
				paths = append(paths, f.Path)
			}
			fmt.Fprintf(&b, "\nFiles it touched: %s\n", strings.Join(paths, ", "))
		}
	}

	b.WriteString("\n## Repository state\n\n")
	b.WriteString(baselineNote(in.VerifyCommand, in.Baseline))

	b.WriteString(`
## What to do

1. Read the project's own instructions first — README.md, and AGENTS.md or CLAUDE.md if either exists. They outrank your habits about style, layout and workflow.
2. Orient yourself: the module layout, where tests live, what the conventions are.
3. Trace the issue through the code until you can name the exact place the behaviour is decided.
4. Check whether the same defect exists elsewhere in the codebase, and say so if it does.
5. Decide the fix, and which files it touches.

If — and only if — the issue has two readings that lead to opposite fixes and nothing in the repository chooses between them, fill in the clarification field. Minor ambiguity is not that: resolve it, and record what you assumed.

Then call submit_analysis.`)

	return b.String()
}

// implementPrompt hands the agreed plan to the write phase.
func implementPrompt(in ImplementInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Implement the agreed fix for issue #%d in %s, on branch `%s`.\n\n",
		in.Ref.Number, in.Ref.Slug(), in.Branch)
	fmt.Fprintf(&b, "## Issue #%d: %s\n\n%s\n", in.Ref.Number, in.Issue.Title, strings.TrimSpace(in.Issue.Body))

	fmt.Fprintf(&b, "\n## Agreed diagnosis (%s, confidence: %s)\n\n%s\n",
		in.Analysis.Classification, in.Analysis.Confidence, strings.TrimSpace(in.Analysis.RootCause))
	fmt.Fprintf(&b, "\n## Agreed approach\n\n%s\n", strings.TrimSpace(in.Analysis.Approach))

	if len(in.Analysis.Files) > 0 {
		b.WriteString("\n## Files the analysis expects to change\n\n")
		for _, f := range in.Analysis.Files {
			fmt.Fprintf(&b, "- `%s` — %s\n", f.Path, f.Change)
		}
		b.WriteString("\nThis list is the plan, not a limit: touch what the fix actually needs.\n")
	}
	if len(in.Analysis.Assumptions) > 0 {
		b.WriteString("\n## Assumptions the analysis made\n\n")
		for _, a := range in.Analysis.Assumptions {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}

	b.WriteString("\n## Repository state\n\n")
	b.WriteString(baselineNote(in.VerifyCommand, in.Baseline))

	if in.VerifyCommand != "" {
		fmt.Fprintf(&b, "\nRun `%s` yourself before you finish. It will be run again after you stop, "+
			"and that second run is what decides whether this change is reported as verified.\n", in.VerifyCommand)
	}

	b.WriteString("\nWhen the change is complete and the checks pass, call submit_implementation.")
	return b.String()
}

// baselineNote states what the suite did BEFORE the change.
//
// It is in both prompts because a red baseline changes what "the tests pass"
// means, and an agent that does not know the suite was already failing will
// either chase someone else's failure or quietly report a pass it did not earn.
func baselineNote(command string, base VerifyResult) string {
	switch {
	case command == "":
		return "No verification command could be detected for this project. Say so in your summary " +
			"if you cannot check your work, and be explicit about what you did not verify.\n"
	case base.Skipped:
		return fmt.Sprintf("Verification command: `%s` (not run before the change).\n", command)
	case base.OK:
		return fmt.Sprintf("Verification command: `%s` — passing before any change. It must still pass after.\n", command)
	default:
		return fmt.Sprintf("Verification command: `%s` — ALREADY FAILING before any change (exit %d). "+
			"You are not responsible for pre-existing failures; do not fix unrelated ones, and do not add new ones. "+
			"The tail of that run:\n\n```\n%s\n```\n", command, base.ExitCode, base.Output)
	}
}
