package main

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// conventionalSubject is the shape a commit subject must have before this
// program uses the model's version of it.
var conventionalSubject = regexp.MustCompile(`^(fix|feat|refactor|perf|docs|test|chore|build|ci|style)(\([a-zA-Z0-9._/-]+\))?!?: .{3,}$`)

// GroupCommitMessage builds the group's commit. The subject the coder proposed
// is used when it is well formed and replaced from the group when it is not —
// a malformed subject would otherwise reach the history forever. The body is
// the summary the coder reported.
func GroupCommitMessage(specName string, groupID int, groupTitle string, r GroupReport) string {
	subject := strings.TrimSpace(r.CommitSubject)
	if !conventionalSubject.MatchString(subject) {
		title := strings.TrimSuffix(strings.TrimSpace(groupTitle), ".")
		kind := "feat"
		if groupID == 1 || strings.Contains(strings.ToLower(title), "test") {
			kind = "test"
		}
		subject = fmt.Sprintf("%s: %s (%s task group %d)", kind, lowerFirst(title), specName, groupID)
	}
	if len(subject) > 72 {
		subject = strings.TrimSpace(subject[:72])
	}
	var b strings.Builder
	b.WriteString(subject)
	if s := strings.TrimSpace(r.Summary); s != "" {
		b.WriteString("\n\n")
		b.WriteString(s)
	}
	if len(r.Unaddressed) > 0 {
		b.WriteString("\n\nNot addressed:\n")
		for _, u := range r.Unaddressed {
			fmt.Fprintf(&b, "- %s\n", u)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// TasksDoneMessage is agent-fox's housekeeping commit, verbatim.
func TasksDoneMessage(groupID int) string {
	return fmt.Sprintf("chore: mark task group %d subtasks done", groupID)
}

// housekeepingPrefixes are the commit subjects agent-fox's harvest skips when
// it picks the message for the squash commit.
var housekeepingPrefixes = []string{"chore: mark task group ", "fix: auto-commit uncommitted changes"}

func isHousekeeping(subject string) bool {
	for _, p := range housekeepingPrefixes {
		if strings.HasPrefix(subject, p) {
			return true
		}
	}
	return false
}

// SquashMessage is agent-fox's _build_squash_message: the full message of the
// last non-housekeeping commit, plus — when the branch carried more than one
// commit — the other subjects as bullets.
func SquashMessage(lastMessage string, subjects []string) string {
	msg := strings.TrimSpace(lastMessage)
	if msg == "" {
		msg = "chore: land task group"
	}
	var others []string
	last := strings.SplitN(msg, "\n", 2)[0]
	for _, s := range subjects {
		if s == last || isHousekeeping(s) {
			continue
		}
		others = append(others, s)
	}
	if len(others) == 0 {
		return msg + "\n"
	}
	var b strings.Builder
	b.WriteString(msg)
	b.WriteString("\n\nAlso on this branch:\n")
	for _, s := range others {
		fmt.Fprintf(&b, "- %s\n", s)
	}
	return b.String()
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if len(r) > 1 && r[1] >= 'a' && r[1] <= 'z' && r[0] >= 'A' && r[0] <= 'Z' {
		r[0] = r[0] - 'A' + 'a'
	}
	return string(r)
}

// ------------------------------------------------------------- summary --

// Summary is the last thing printed, and it reports what happened rather than
// what was attempted.
func Summary(w io.Writer, res *Result, err error, verbose bool) {
	fmt.Fprintln(w)
	switch {
	case res.CostLimit:
		fmt.Fprintf(w, "[flatline] $ %s stopped: the run cost ceiling was reached.\n", res.SpecName)
	case res.Stalled:
		fmt.Fprintf(w, "[flatline] ✗ %s stalled: a task group exhausted its retries.\n", res.SpecName)
	case err != nil:
		fmt.Fprintf(w, "[flatline] ✗ %s NOT completed (failed during %s).\n", res.SpecName, res.Stage)
	case res.Dirty:
		fmt.Fprintf(w, "[flatline] ~ %s completed DIRTY: every task group landed, but the final checks fail.\n", res.SpecName)
	default:
		fmt.Fprintf(w, "[flatline] ✓ %s completed.\n", res.SpecName)
	}

	if len(res.Groups) > 0 {
		fmt.Fprintf(w, "  %-4s %-8s %-12s %-8s %s\n", "grp", "role", "status", "tries", "landed / branch")
		for _, g := range res.Groups {
			where := g.Landed
			if where == "" {
				where = g.Branch
			}
			tries := "-"
			if g.Attempts > 0 {
				tries = fmt.Sprint(g.Attempts)
			}
			fmt.Fprintf(w, "  %-4d %-8s %-12s %-8s %s\n", g.ID, g.Archetype, g.Status, tries, where)
		}
	}
	if res.FinalBranch != "" {
		fmt.Fprintf(w, "  merge:    %s\n", res.FinalBranch)
	}
	for _, c := range res.Final {
		if !c.Skipped {
			fmt.Fprintf(w, "  final %-10s %s\n", c.Name+":", c.Status())
		}
	}
	if v := res.Verdicts; v != nil {
		fmt.Fprintf(w, "  verifier: %s — %s\n", v.OverallVerdict, firstLine(v.Summary, 100))
		for _, d := range v.Verdicts {
			if d.Verdict != "PASS" || verbose {
				fmt.Fprintf(w, "    %-14s %s  %s\n", d.RequirementID, d.Verdict, firstLine(d.Evidence, 80))
			}
		}
	}
	for _, s := range res.Stats {
		if verbose {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
	if verbose {
		if c := res.CostUSD(); c > 0 {
			fmt.Fprintf(w, "  cost:     $%.4f\n", c)
		}
	}
	for _, m := range res.Warnings {
		fmt.Fprintf(w, "  ! %s\n", m)
	}
	if err != nil {
		fmt.Fprintf(w, "\nerror: %v\n", err)
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}
