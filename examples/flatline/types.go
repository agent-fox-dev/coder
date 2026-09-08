package main

import (
	"fmt"
	"strings"
	"time"

	afspec "github.com/agent-fox-dev/spec"
	"github.com/agentfox/agentkit-go/core"
)

// Archetype is the role a session plays. agent-fox has four (coder, reviewer,
// verifier, gate); the linear pass needs the three that do not depend on its
// knowledge store. Which one a task group gets is decided by its `kind`, in
// Go, before any model is involved.
type Archetype string

const (
	Coder    Archetype = "coder"    // tests, standard and wiring_verification groups
	Gate     Archetype = "gate"     // checkpoint groups: run the checks, change nothing
	Verifier Archetype = "verifier" // after the last group: verdicts, informational
)

// ArchetypeFor mirrors agent-fox's builder: a checkpoint group is a gate,
// everything else is a coder.
func ArchetypeFor(kind afspec.TaskGroupKind) Archetype {
	if kind == afspec.TaskGroupKindCheckpoint {
		return Gate
	}
	return Coder
}

// GroupReport is what a coder session hands back through submit_group. It is
// agent-fox's `.agent-fox/session-summary.json` schema, plus the commit
// subject — because here the commit is made by the program, not the agent.
//
// Note what is NOT in it: any claim that the tests pass. The gates re-run
// them.
type GroupReport struct {
	Summary            string       `json:"summary"`
	CommitSubject      string       `json:"commit_subject"`
	Changes            []FileChange `json:"changes"`
	TestsAddedModified []TestNote   `json:"tests_added_or_modified"`
	Gotchas            []string     `json:"gotchas"`
	Assumptions        []string     `json:"assumptions"`
	RejectedApproaches []Rejected   `json:"rejected_approaches"`
	Unaddressed        []string     `json:"unaddressed"`
}

type FileChange struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

type TestNote struct {
	Path        string `json:"path"`
	Description string `json:"description"`
}

type Rejected struct {
	Approach string `json:"approach"`
	Reason   string `json:"reason"`
}

// MemoryFacts renders the parts of a report that later groups benefit from —
// what agent-fox's knowledge provider injects as `[CONTEXT]` facts.
func (r GroupReport) MemoryFacts(groupID int) []string {
	var out []string
	if s := strings.TrimSpace(r.Summary); s != "" {
		out = append(out, fmt.Sprintf("[CONTEXT] task group %d: %s", groupID, s))
	}
	for _, g := range r.Gotchas {
		out = append(out, fmt.Sprintf("[GOTCHA] task group %d: %s", groupID, g))
	}
	for _, a := range r.Assumptions {
		out = append(out, fmt.Sprintf("[ASSUMPTION] task group %d: %s", groupID, a))
	}
	for _, rj := range r.RejectedApproaches {
		out = append(out, fmt.Sprintf("[REJECTED] task group %d: %s — %s", groupID, rj.Approach, rj.Reason))
	}
	return out
}

// GateReport is a checkpoint session's answer: each check it ran and whether
// it passed. A gate that fails is a failed group attempt, like any other.
type GateReport struct {
	Passed  bool          `json:"passed"`
	Results []CheckResult `json:"results"`
	Notes   string        `json:"notes"`
}

type CheckResult struct {
	Check  string `json:"check"`
	Passed bool   `json:"passed"`
	Output string `json:"output"`
}

// Verdicts is the verifier's output, in agent-fox's shape. It is recorded and
// printed; it gates nothing, exactly as in agent-fox.
type Verdicts struct {
	Verdicts       []Verdict `json:"verdicts"`
	OverallVerdict string    `json:"overall_verdict"`
	Summary        string    `json:"summary"`
}

type Verdict struct {
	RequirementID string `json:"requirement_id"`
	Verdict       string `json:"verdict"`
	Evidence      string `json:"evidence"`
}

// RunStats is what one model session cost.
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

// CheckRun is one execution of one of the pack's test_commands.
type CheckRun struct {
	Name     string        `json:"name"` // linter | spec_tests | all_tests
	Command  string        `json:"command"`
	ExitCode int           `json:"exit_code"`
	OK       bool          `json:"ok"`
	Skipped  bool          `json:"skipped"`
	Output   string        `json:"-"` // tail only
	Elapsed  time.Duration `json:"-"`
}

func (c CheckRun) Status() string {
	switch {
	case c.Skipped:
		return "skipped"
	case c.OK:
		return "pass"
	default:
		return fmt.Sprintf("fail (exit %d)", c.ExitCode)
	}
}

// GroupStatus is agent-fox's node status vocabulary, reduced to what a chain
// can produce.
type GroupStatus string

const (
	StatusPending    GroupStatus = "pending"
	StatusInProgress GroupStatus = "in_progress"
	StatusCompleted  GroupStatus = "completed"
	StatusSkipped    GroupStatus = "skipped"      // preflight: already done and green
	StatusBlocked    GroupStatus = "blocked"      // retries exhausted
	StatusCostBlock  GroupStatus = "cost_blocked" // the run ceiling was hit first
)

// GroupOutcome is the record of one task group in the run.
type GroupOutcome struct {
	ID        int         `json:"id"`
	Kind      string      `json:"kind"`
	Title     string      `json:"title"`
	Archetype Archetype   `json:"archetype"`
	Status    GroupStatus `json:"status"`
	Attempts  int         `json:"attempts"`
	Branch    string      `json:"branch,omitempty"`
	Commit    string      `json:"commit,omitempty"`  // the group's own commit
	Landed    string      `json:"landed,omitempty"`  // the squash commit on the base branch
	Changed   []string    `json:"changed,omitempty"` // files, from git
	Checks    []CheckRun  `json:"checks,omitempty"`
	Report    GroupReport `json:"report"`
	Gate      GateReport  `json:"gate"`
	Error     string      `json:"error,omitempty"`
}

// Result is what the run produced, whether or not it finished.
type Result struct {
	SpecDir   string
	SpecName  string
	Base      string
	Groups    []GroupOutcome
	Final     []CheckRun // the post-run check, agent-fox's `make check`
	FinalOK   bool
	Verdicts  *Verdicts
	Stats     []RunStats
	Warnings  []string
	Journal   []JournalEntry
	Stage     string // the step that failed; empty on success
	Stalled   bool   // a group exhausted its retries
	CostLimit bool   // the run ceiling was hit
	Dirty     bool   // every group completed, but the final check fails
}

func (r *Result) CostUSD() float64 {
	var sum float64
	for _, s := range r.Stats {
		sum += s.CostUSD
	}
	return sum
}

// JournalEntry is one line of the run log: what an autonomous run leaves
// behind for the person who reads it afterwards.
type JournalEntry struct {
	Time    time.Time `json:"time"`
	Step    string    `json:"step"`
	Status  string    `json:"status"` // ok | warn | fail | skip
	Detail  string    `json:"detail,omitempty"`
	Elapsed int64     `json:"elapsed_ms"`
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

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
