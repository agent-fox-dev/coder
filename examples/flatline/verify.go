package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	afspec "github.com/agent-fox-dev/spec"
)

// The three commands a pack declares. Their names are the JSON keys in
// tasks.json, so a journal line and the file say the same thing.
const (
	checkLinter    = "linter"
	checkSpecTests = "spec_tests"
	checkAllTests  = "all_tests"
)

// Checks is the pack's test_commands as an ordered list.
func Checks(tc afspec.TestCommands) []Check {
	return []Check{
		{Name: checkLinter, Command: tc.Linter},
		{Name: checkSpecTests, Command: tc.SpecTests},
		{Name: checkAllTests, Command: tc.AllTests},
	}
}

type Check struct {
	Name    string
	Command string
}

// RunCheck executes one test command through `sh -c`.
//
// cleaner splits its verify command on whitespace and runs it directly, so
// that "what exactly ran" is unambiguous. That is not available here: the
// spec format defines test_commands as shell lines, and real packs use `&&`
// (agent-fox's own: `pytest -q && go test ./... -count=1`). The shell is the
// contract, so the shell is used — and the exact line is recorded.
func RunCheck(ctx context.Context, run Runner, dir string, c Check, timeout time.Duration) CheckRun {
	if strings.TrimSpace(c.Command) == "" {
		return CheckRun{Name: c.Name, Skipped: true}
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, code, err := run(ctx, dir, []string{"sh", "-c", c.Command})
	res := CheckRun{
		Name:     c.Name,
		Command:  c.Command,
		ExitCode: code,
		OK:       err == nil && code == 0,
		Output:   tail(out, 40),
		Elapsed:  time.Since(start),
	}
	if err != nil {
		res.ExitCode = -1
		res.Output = strings.TrimSpace(err.Error() + "\n" + res.Output)
	}
	return res
}

// RunChecks runs every check and returns them in order, skipped ones included.
func RunChecks(ctx context.Context, run Runner, dir string, checks []Check, timeout time.Duration) []CheckRun {
	out := make([]CheckRun, 0, len(checks))
	for _, c := range checks {
		out = append(out, RunCheck(ctx, run, dir, c, timeout))
	}
	return out
}

// GateFor says which of a group's check runs decide whether the group passed.
//
// agent-fox runs none of them per group: the coder is told to run "the
// relevant test suite and linter" and one `make check` after the whole graph
// drains decides COMPLETED versus COMPLETED_DIRTY. flatline keeps that final
// check and adds exactly one per-group gate — the linter — because it is the
// only command whose expected result is the same for every group. The spec
// tests are SUPPOSED to fail after a `tests` group and are allowed to fail
// after every implementation group but the last, so making them a gate
// would either fail the pass on day one or require the program to know which
// group is the last one that matters. Their results are recorded, not judged.
func GateFor(kind afspec.TaskGroupKind, runs []CheckRun) (failed []CheckRun) {
	for _, r := range runs {
		if r.Name == checkLinter && !r.OK && !r.Skipped {
			failed = append(failed, r)
		}
	}
	return failed
}

// FinalGate is agent-fox's post-merge check: everything must pass now.
func FinalGate(runs []CheckRun) (failed []CheckRun) {
	for _, r := range runs {
		if !r.OK && !r.Skipped {
			failed = append(failed, r)
		}
	}
	return failed
}

// checkFailureText renders failed checks for a retry prompt and for the
// journal.
func checkFailureText(failed []CheckRun) string {
	var b strings.Builder
	for i, f := range failed {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "`%s` (%s) exited %d:\n%s", f.Command, f.Name, f.ExitCode, f.Output)
	}
	return b.String()
}

// checkPrograms is the first word of each simple command in each test
// command — the programs the coder's shell allowlist must include, or the
// agent cannot run the suite it is judged by. `sh` itself is never added.
func checkPrograms(tc afspec.TestCommands) []string {
	var out []string
	for _, c := range []string{tc.Linter, tc.SpecTests, tc.AllTests} {
		for _, seg := range shellSegments(c) {
			if argv := commandWords(strings.Fields(seg)); len(argv) > 0 {
				out = append(out, baseName(argv[0]))
			}
		}
	}
	return out
}
