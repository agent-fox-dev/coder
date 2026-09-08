package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/agentfox/agentkit-go/core"
)

// Classification is the issue taxonomy from the af-fix skill. It is a typed
// value rather than a free string because three later decisions read it — the
// branch prefix, the conventional-commit type, and how the implementation
// phase is briefed — and a model that answers "bugfix" or "Bug" would silently
// change all three. The tool schema declares it as an enum, so the value is
// validated before it reaches this program.
type Classification string

const (
	ClassBug         Classification = "bug"
	ClassFeature     Classification = "feature"
	ClassRefactor    Classification = "refactor"
	ClassPerformance Classification = "performance"
)

func (c Classification) Valid() bool {
	switch c {
	case ClassBug, ClassFeature, ClassRefactor, ClassPerformance:
		return true
	}
	return false
}

// BranchPrefix keeps the branch namespace readable at a glance.
func (c Classification) BranchPrefix() string {
	switch c {
	case ClassBug:
		return "fix"
	case ClassPerformance:
		return "perf"
	case ClassRefactor:
		return "refactor"
	default:
		return "feature"
	}
}

// CommitType is the conventional-commits type for this class of change.
func (c Classification) CommitType() string {
	switch c {
	case ClassBug:
		return "fix"
	case ClassPerformance:
		return "perf"
	case ClassRefactor:
		return "refactor"
	default:
		return "feat"
	}
}

// Confidence mirrors the af-issue vocabulary: the analysis says how sure it
// is, and the issue comment prints it. An unqualified diagnosis reads as fact
// to whoever reviews the pull request, and most of them are not.
type Confidence string

const (
	Confirmed Confidence = "confirmed"
	Probable  Confidence = "probable"
	Suspected Confidence = "suspected"
)

// FileChange is one entry of the "files to modify" table.
type FileChange struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

// Clarification is the escape hatch of Step 4.4: two readings of the issue
// that lead to opposite fixes, and nothing in the repository to choose
// between them.
type Clarification struct {
	Question string `json:"question"`
	OptionA  string `json:"option_a"`
	OptionB  string `json:"option_b"`
}

// Analysis is the structured output of the read-only phase. It arrives as
// validated tool arguments rather than as prose this program has to parse
// back out of the final message — which is the whole reason the phase ends in
// a terminating tool instead of in a paragraph.
type Analysis struct {
	Classification Classification `json:"classification"`
	Confidence     Confidence     `json:"confidence"`
	Summary        string         `json:"summary"`
	RootCause      string         `json:"root_cause"`
	Approach       string         `json:"approach"`
	Files          []FileChange   `json:"files"`
	Assumptions    []string       `json:"assumptions"`
	Clarification  *Clarification `json:"clarification,omitempty"`
}

// TestNote is one entry of the "tests" table in the summary comment.
type TestNote struct {
	Path   string `json:"path"`
	Covers string `json:"covers"`
}

// Implementation is the structured output of the write phase.
//
// Note what is NOT in it: any claim about whether the tests passed. The agent
// is asked what it changed, not whether it worked; the verification step re-runs
// the suite itself. A field like `tests_pass: true` would be a self-report
// that this program then prints as if it had checked.
type Implementation struct {
	Summary       string       `json:"summary"`
	CommitSubject string       `json:"commit_subject"`
	Changes       []FileChange `json:"changes"`
	Tests         []TestNote   `json:"tests"`
	Limitations   []string     `json:"limitations"`
}

// RunStats is what one agent phase cost.
type RunStats struct {
	Phase      string             `json:"phase"`
	Turns      int                `json:"turns"`
	StopReason core.RunStopReason `json:"stop_reason"`
	Usage      core.Usage         `json:"-"`
	CostUSD    float64            `json:"cost_usd"`
	Elapsed    time.Duration      `json:"-"`
	ElapsedMS  int64              `json:"elapsed_ms"`
}

func (s RunStats) String() string {
	return fmt.Sprintf("%s: %d turns · stop %s · in %d / out %d tokens · $%.5f · %s",
		s.Phase, s.Turns, s.StopReason, s.Usage.InputTokens, s.Usage.OutputTokens,
		s.CostUSD, s.Elapsed.Round(time.Millisecond))
}

// VerifyResult is one run of the project's own quality command.
type VerifyResult struct {
	Command  string        `json:"command"`
	ExitCode int           `json:"exit_code"`
	OK       bool          `json:"ok"`
	Skipped  bool          `json:"skipped"`
	Output   string        `json:"-"` // tail only; the full log stays on stderr
	Elapsed  time.Duration `json:"-"`
}

func (v VerifyResult) Status() string {
	switch {
	case v.Skipped:
		return "skipped"
	case v.OK:
		return "pass"
	default:
		return fmt.Sprintf("fail (exit %d)", v.ExitCode)
	}
}

// tail keeps the last n lines of command output, which is where a failing
// build says what went wrong.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return "…\n" + strings.Join(lines[len(lines)-n:], "\n")
}
