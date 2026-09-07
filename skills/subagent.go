package skills

import (
	"context"
	"fmt"
	"strings"
)

// REQ-SKILL-08 + OQ-1: declarative skill subagents.
//
// The requirement's constraint is the design: "skill plugin code may not
// directly invoke internal session or backend packages". So this package does
// not spawn anything and does not import the root agent package — it could not
// without inverting the dependency, and a skills package that could open a
// session would be exactly the coupling REQ-SKILL-08 forbids. What it defines
// is the SEAM: the manifest declares the step, the embedder supplies something
// that can run a prompt and return text, and this file walks the declarations,
// applies the on_failure policy and returns the text to inject.

// on_failure values (OQ-1). The zero value is normalized to OnFailureWarn by
// ParseManifest, which is the PRD's recommendation: pre-analysis is
// enrichment, not a hard dependency, and authored content that fails should
// degrade rather than reject (REQ-SKILL-10's posture).
const (
	// OnFailureAbort stops the session. For a skill whose analysis the rest of
	// the run is meaningless without.
	OnFailureAbort = "abort"
	// OnFailureWarn proceeds and injects a warning in the analysis' place, so
	// the model is told the enrichment is missing instead of silently
	// reasoning as though it had been given. This is the default.
	OnFailureWarn = "warn"
	// OnFailureSkip proceeds and injects nothing.
	OnFailureSkip = "skip"
)

// ModeBeforeSession is the mode with defined timing today (OQ-1): the step
// runs once, before the main session's first turn, and its result is part of
// the assembled system prompt. Other values are carried to the runner
// verbatim; this package never interprets them, because when to run a step is
// the runner's knowledge, not the manifest parser's.
const ModeBeforeSession = "before_session"

// Declared reports whether the manifest actually asks for a subagent step. A
// [skill.subagent] table with nothing in it but on_failure (which always has a
// value after parsing) declares no step.
func (s SubagentSection) Declared() bool {
	return s.Archetype != "" || s.PromptTemplate != "" || s.ResultKey != ""
}

// SubagentRequest is one declared step, resolved.
type SubagentRequest struct {
	// Skill is the declaring skill's name, for the runner's own logging and
	// for the error a failure produces.
	Skill string
	// Archetype is [skill.subagent].archetype: which agent the runner should
	// spawn. Empty means the runner's default.
	Archetype string
	// Mode is [skill.subagent].mode, uninterpreted (see ModeBeforeSession).
	Mode string
	// Prompt is prompt_template with its placeholders substituted.
	Prompt string
}

// SubagentRunner is the seam. The embedder supplies something that can run a
// prompt and return text; this package never learns how.
//
// A runner is expected to honour ctx for the timeout OQ-1 asks about — this
// package deliberately imposes none, because the right budget for a
// pre-analysis is the host's to decide and a hidden default here would be
// wrong for somebody.
type SubagentRunner interface {
	RunSubagent(ctx context.Context, req SubagentRequest) (string, error)
}

// SubagentRunnerFunc adapts a plain function to SubagentRunner.
type SubagentRunnerFunc func(context.Context, SubagentRequest) (string, error)

// RunSubagent implements SubagentRunner.
func (f SubagentRunnerFunc) RunSubagent(ctx context.Context, req SubagentRequest) (string, error) {
	return f(ctx, req)
}

// PreAnalysis is one step's contribution to the main session's system prompt.
type PreAnalysis struct {
	// Skill is the declaring skill.
	Skill string
	// Key is [skill.subagent].result_key, defaulting to the skill name: the
	// label the text is injected under, and what the skill's own prompt.md
	// tells the model to look for.
	Key string
	// Text is the subagent's result, or — when Failed — the warning that
	// takes its place.
	Text string
	// Failed marks the OnFailureWarn arm. The prompt block renders it as
	// unavailable rather than as an analysis, because a model told "the
	// analysis says: it failed" will treat the failure as a finding.
	Failed bool
}

// SubagentError is the OnFailureAbort arm. It names the skill, because
// "subagent failed" is not something an operator can act on and "the
// code-review skill's pre-analysis failed" is.
type SubagentError struct {
	Skill string
	Err   error
}

func (e *SubagentError) Error() string {
	return fmt.Sprintf("skills: skill %q subagent step failed and its on_failure is %q: %v",
		e.Skill, OnFailureAbort, e.Err)
}

func (e *SubagentError) Unwrap() error { return e.Err }

// RunSubagents executes the [skill.subagent] step of every selected skill that
// declares one, in the order the skills were selected, and returns the
// analyses to inject (Input.Analyses).
//
// vars are the placeholder values for prompt_template; by convention "task"
// holds the session's task prompt. An unknown placeholder is left in the text
// verbatim (see RenderTemplate).
//
// A nil runner is treated as a FAILURE of every declared step, not as "no
// steps". The distinction matters: an embedder that forgot to wire the seam
// and a subagent that timed out are the same situation from the main session's
// point of view — the enrichment is not there — and silently dropping the step
// would hide the missing wiring precisely on the skills whose author asked for
// abort.
func RunSubagents(ctx context.Context, runner SubagentRunner, sel []Skill,
	vars map[string]string) ([]PreAnalysis, error) {

	var out []PreAnalysis
	for _, s := range sel {
		if !s.Subagent.Declared() {
			continue
		}

		var (
			text string
			err  error
		)
		req := SubagentRequest{
			Skill:     s.Name,
			Archetype: s.Subagent.Archetype,
			Mode:      s.Subagent.Mode,
			Prompt:    RenderTemplate(s.Subagent.PromptTemplate, vars),
		}
		if runner == nil {
			err = fmt.Errorf("no SubagentRunner was supplied to RunSubagents")
		} else {
			text, err = runner.RunSubagent(ctx, req)
		}
		if err == nil && strings.TrimSpace(text) == "" {
			// An empty result is not an error, and it is not content either.
			// Injecting an empty block would spend prompt on a label with
			// nothing under it and invite the model to invent what belongs
			// there.
			continue
		}

		key := s.Subagent.ResultKey
		if key == "" {
			key = s.Name
		}
		if err == nil {
			out = append(out, PreAnalysis{Skill: s.Name, Key: key, Text: text})
			continue
		}

		switch s.Subagent.OnFailure {
		case OnFailureAbort:
			return nil, &SubagentError{Skill: s.Name, Err: err}
		case OnFailureSkip:
			continue
		default: // OnFailureWarn, and anything ParseManifest normalized to it
			out = append(out, PreAnalysis{Skill: s.Name, Key: key, Failed: true,
				Text: fmt.Sprintf("The %q pre-analysis did not complete (%v). "+
					"Proceed without it and do not assume its findings either way.", s.Name, err)})
		}
	}
	return out, nil
}

// RenderTemplate substitutes {{key}} placeholders in a prompt_template.
//
// Unknown placeholders are LEFT AS WRITTEN rather than emptied. A template
// that says "review {{diff}}" and silently becomes "review " sends the
// subagent off with a plausible-looking instruction and no subject; leaving
// the placeholder visible makes the missing variable obvious in the subagent's
// own transcript, which is where somebody will be reading when they notice.
func RenderTemplate(tmpl string, vars map[string]string) string {
	if tmpl == "" || len(vars) == 0 {
		return tmpl
	}
	pairs := make([]string, 0, 2*len(vars))
	for k, v := range vars {
		pairs = append(pairs, "{{"+k+"}}", v)
	}
	return strings.NewReplacer(pairs...).Replace(tmpl)
}
