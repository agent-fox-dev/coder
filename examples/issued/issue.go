package main

import (
	"fmt"
	"strings"

	"github.com/agentfox/agentkit-go/schema"
)

// Issue is the triage result: the af-issue issue template as a Go type.
//
// The skill states that template as a fenced markdown block and asks the model
// to fill it in. That is the one thing a prompt cannot check. Here the shape is
// a schema (issueSchema, below), so a missing severity or an empty acceptance
// criterion is a validation error the model is handed back and corrects, and
// the markdown is rendered by Render() from fields that already validated —
// the model never writes the document, only its contents.
type Issue struct {
	Title              string    `json:"title"`
	Problem            string    `json:"problem"`
	Reproduction       string    `json:"reproduction"`
	Confidence         string    `json:"confidence"`
	RootCause          string    `json:"root_cause"`
	RelatedInstances   []string  `json:"related_instances"`
	AffectedFiles      []FileRef `json:"affected_files"`
	Fix                Fix       `json:"suggested_fix"`
	AcceptanceCriteria []string  `json:"acceptance_criteria"`
	Severity           string    `json:"severity"`
	SeverityRationale  string    `json:"severity_rationale"`
}

// FileRef is a path plus what that path has to do with the issue. The pair is
// the unit the skill asks for ("`path/to/file.py` — {role in the issue}"), and
// keeping it a struct rather than one preformatted string is what lets the
// tool handler verify Path against the workspace without parsing prose.
type FileRef struct {
	Path string `json:"path"`
	Role string `json:"role"`
}

type Fix struct {
	Approach string    `json:"approach"`
	Files    []FileRef `json:"files"`
	Risks    string    `json:"risks"`
}

// Confidence and severity vocabularies, in the order the skill defines them.
// They are declared once and used twice — in the schema as an enum, and in the
// prompt as the criteria table — so the two cannot drift apart.
var (
	confidenceLevels = []string{"Confirmed", "Probable", "Suspected"}
	severityLevels   = []string{"Critical", "High", "Medium", "Low"}
)

// issueSchema is the contract. Note what is Prop (required) and what is Opt:
// related_instances is genuinely optional, but severity is not — an issue
// without one is not triaged, and leaving it optional would let the model skip
// the judgement call the whole run exists to make.
//
// The descriptions are not documentation. They are the only instruction the
// model gets about each field at the moment it fills it in, which is later and
// closer to the decision than anything in the system prompt.
func issueSchema() *schema.Schema {
	fileRef := schema.Object(
		schema.Prop("path", schema.String("Repository-relative path, exactly as it exists on disk")),
		schema.Prop("role", schema.String("One line: this file's part in the issue")),
	)
	return schema.Object(
		schema.Prop("title", schema.String(
			"Under 80 characters, '{component}: {defect}'. Name the defect, not the symptom.")),
		schema.Prop("problem", schema.String(
			"1-3 sentences: the observable symptom and the conditions it occurs under")),
		schema.Prop("reproduction", schema.String(
			"Steps or conditions to reproduce. If none can be established from the input, say "+
				"'Observed from error output; manual reproduction steps not established.'")),
		schema.Prop("confidence", schema.Enum(
			"Confirmed: you traced the exact path. Probable: mechanism identified, trigger unverified. "+
				"Suspected: hypothesis consistent with the evidence.", confidenceLevels...)),
		schema.Prop("root_cause", schema.String(
			"Why it happens, citing files, functions and line ranges. Trace trigger to fault. "+
				"Explain the mechanism, not the symptom.")),
		schema.Opt("related_instances", schema.Array(schema.String(),
			"Other places in the codebase with the same bug class. Omit if none.")),
		schema.Prop("affected_files", schema.Array(fileRef,
			"Every file involved in the root cause. Each path is checked against the workspace.").MinItemsN(1)),
		schema.Prop("suggested_fix", schema.Object(
			schema.Prop("approach", schema.String("What to change, where, and why it addresses the root cause")),
			schema.Prop("files", schema.Array(fileRef, "Files to modify; role is what changes in each").MinItemsN(1)),
			schema.Prop("risks", schema.String("Risks and trade-offs, or 'None identified'")),
		)),
		schema.Prop("acceptance_criteria", schema.Array(
			schema.String("Given {precondition}, when {action}, then {outcome}"),
			"Testable conditions the fix must satisfy").MinItemsN(1)),
		schema.Prop("severity", schema.Enum(
			"Critical: data loss, security, crash in production, blocks everyone. "+
				"High: core functionality broken, no workaround. "+
				"Medium: impaired, workaround exists. "+
				"Low: cosmetic or an unlikely edge case.", severityLevels...)),
		schema.Prop("severity_rationale", schema.String("One line justifying the severity")),
	)
}

// Render produces the issue body. It is a pure function of the validated
// struct, which is why two runs that reach the same diagnosis produce
// byte-identical documents — a property no amount of "follow this template"
// buys you, and the reason the golden test in issued_test.go can exist.
func (i Issue) Render(src SourceKind, origin string) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	p("## Problem\n\n%s\n\n", block(i.Problem))
	p("## Reproduction\n\n%s\n\n", block(i.Reproduction))

	p("## Root Cause Analysis\n\n**Confidence:** %s\n\n%s\n\n", i.Confidence, block(i.RootCause))
	p("### Related Instances\n\n")
	if len(i.RelatedInstances) == 0 {
		p("No related instances found.\n\n")
	} else {
		for _, r := range i.RelatedInstances {
			p("- %s\n", strings.TrimSpace(r))
		}
		p("\n")
	}

	p("## Affected Files\n\n%s\n", fileList(i.AffectedFiles))
	p("\n## Suggested Fix\n\n**Approach:**\n\n%s\n\n", block(i.Fix.Approach))
	p("**Files to modify:**\n\n%s\n", fileList(i.Fix.Files))
	p("\n**Risks:**\n\n%s\n\n", block(i.Fix.Risks))

	p("## Acceptance Criteria\n\n")
	for n, ac := range i.AcceptanceCriteria {
		p("- **AC-%d:** %s\n", n+1, strings.TrimSpace(ac))
	}
	p("\n## Severity\n\n**%s** — %s\n\n", i.Severity, strings.TrimSpace(i.SeverityRationale))

	p("---\n*Triaged by `issued` from %s: %s.*\n", src, origin)
	return b.String()
}

func fileList(refs []FileRef) string {
	lines := make([]string, 0, len(refs))
	for _, f := range refs {
		lines = append(lines, fmt.Sprintf("- `%s` — %s", f.Path, strings.TrimSpace(f.Role)))
	}
	return strings.Join(lines, "\n")
}

// block normalizes model prose to a single trailing newline's worth of
// whitespace, so the rendered document does not inherit whichever spacing the
// model happened to emit.
func block(s string) string { return strings.TrimSpace(s) }
