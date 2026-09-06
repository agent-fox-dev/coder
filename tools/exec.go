package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Outcome classifies how a command ended. It is a distinct type because the
// classification is a PURE FUNCTION with a pinned precedence, unit-tested on
// its own (REQ-TOOL-17.6): mixing it into the run path is how "the command
// timed out" ends up reported as "exit status 1".
type Outcome string

const (
	OutcomeOK      Outcome = "ok"
	OutcomeExit    Outcome = "exit"
	OutcomeSignal  Outcome = "signal"
	OutcomeTimeout Outcome = "timeout"
	OutcomeAbort   Outcome = "abort"
)

// ClassifyOutcome is REQ-TOOL-17.6's pure function. Precedence is part of the
// contract and is deliberately NOT the order the events happened in:
//
//	abort > timeout > exit status
//
// A timed-out command is also a killed command with a non-zero exit status,
// and an aborted one is both. Reporting the exit status would tell the model
// the build failed when in fact the user pressed Ctrl-C.
func ClassifyOutcome(aborted, timedOut bool, exitCode int, signaled bool) Outcome {
	switch {
	case aborted:
		return OutcomeAbort
	case timedOut:
		return OutcomeTimeout
	case signaled:
		// A signal-killed child has no meaningful exit code. Report what
		// happened rather than inventing one.
		return OutcomeSignal
	case exitCode != 0:
		return OutcomeExit
	}
	return OutcomeOK
}

// ExecResult is the outcome of one command.
type ExecResult struct {
	Output     string
	Outcome    Outcome
	ExitCode   int
	Truncated  bool
	TotalBytes int64
	SpillPath  string
	Duration   time.Duration
}

// ExecOptions configures Run.
type ExecOptions struct {
	Dir string
	// Timeout of zero means no timeout. REQ-TOOL-06 makes timeout_s optional
	// with NO default: a default wall clock silently kills long builds, and
	// safety comes from process control rather than from a clock.
	Timeout time.Duration
	// MaxBytes bounds the window that reaches the model.
	MaxBytes int
	// SpillDir enables the full-output spill file.
	SpillDir string
	// Env, when non-nil, replaces the inherited environment entirely.
	Env []string
}

// Run executes a command through a bash-family shell with full process-group
// lifecycle control.
//
// The pieces that are not obvious, each from REQ-TOOL-17:
//
//   - The command runs in its OWN PROCESS GROUP, and both timeout and context
//     cancellation kill the whole group. Killing only the direct child leaves
//     every grandchild running — a background server, a watch process — long
//     after the agent believes the command is over.
//   - stdout and stderr share ONE PIPE, so they interleave in true write
//     order. Separate captures are prohibited: they produce a transcript in
//     which the error appears before the line that caused it.
//   - WaitDelay backstops a descendant still holding the pipe open after the
//     parent exits, so Wait cannot hang forever.
//   - Output is truncated from the TAIL (REQ-TOOL-09a).
func Run(ctx context.Context, command string, opts ExecOptions) (ExecResult, error) {
	shell, args, err := ResolveShell()
	if err != nil {
		return ExecResult{}, err
	}
	return runArgv(ctx, append(append([]string{shell}, args...), command), opts)
}

// RunArgv is REQ-TOOL-06's structured variant: an argv vector, executed with
// NO SHELL between the caller and the program.
//
// The difference is the entire point. `Run` hands a string to bash, so `;`,
// `$(…)`, backticks, globs and redirection all mean something. Here they do
// not: every element is one argument, verbatim, and a filename containing a
// space or a semicolon reaches the program as itself. A caller that has
// already got its arguments as separate values should never have to quote
// them back into a shell string and hope the quoting is right.
//
// Everything else — process group, timeout, group kill, interleaved output,
// tail truncation, spill — is identical, because those are properties of
// running a subprocess and not of how the command was spelled.
func RunArgv(ctx context.Context, argv []string, opts ExecOptions) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, errors.New("tools: run_command needs at least one argument")
	}
	// Resolved against PATH here rather than left to exec.Command, so a
	// missing program is a clear error instead of a start failure whose
	// message names only the file.
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return ExecResult{}, fmt.Errorf("tools: %q not found on PATH: %w", argv[0], err)
	}
	return runArgv(ctx, append([]string{bin}, argv[1:]...), opts)
}

// runArgv is the shared body. Both entry points reach it with a fully
// resolved argv, so there is exactly one implementation of the process
// lifecycle rather than two that drift.
func runArgv(ctx context.Context, argv []string, opts ExecOptions) (ExecResult, error) {
	runCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = opts.Dir
	cmd.Stdin = nil // REQ-TOOL-06: stdin is DEVNULL, never the agent's own.
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	setProcessGroup(cmd)
	// A descendant holding the output pipe must not wedge Wait forever.
	cmd.WaitDelay = 2 * time.Second

	acc := NewAccumulator(opts.MaxBytes, TruncateTail)
	acc.SpillDir, acc.SpillPrefix = opts.SpillDir, "agentkit-exec"
	defer acc.Close()

	// ONE writer for both streams, so they interleave in true write order.
	cmd.Stdout = acc
	cmd.Stderr = acc

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return ExecResult{}, err
	}

	// Kill the GROUP on cancellation or timeout, not just the child.
	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			killGroup(cmd)
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	close(done)
	elapsed := time.Since(start)

	exitCode := 0
	signaled := false
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		exitCode = ee.ExitCode()
		if exitCode == -1 {
			// No exit code means it was terminated by a signal.
			signaled = true
		}
	}

	aborted := ctx.Err() != nil
	timedOut := !aborted && runCtx.Err() != nil

	return ExecResult{
		Output:     acc.String(),
		Outcome:    ClassifyOutcome(aborted, timedOut, exitCode, signaled),
		ExitCode:   exitCode,
		Truncated:  acc.Truncated(),
		TotalBytes: acc.Total(),
		SpillPath:  acc.SpillPath(),
		Duration:   elapsed,
	}, nil
}

// ResolveShell is REQ-TOOL-06's fixed ladder.
//
// It never consults $SHELL and never falls back to cmd.exe. $SHELL is the
// user's INTERACTIVE shell — fish, nushell, zsh with a custom rc — and a
// command written for bash is not portable to it. A silent fallback to a
// different dialect produces failures that look like the model wrote bad
// shell.
//
// On Windows the ladder ends in a HARD ERROR naming the paths searched, rather
// than falling through to cmd.
func ResolveShell() (string, []string, error) {
	return resolveShell()
}

func lookPathAny(names ...string) (string, bool) {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, true
		}
	}
	return "", false
}

// ReducedEnv strips credentials from the inherited environment while KEEPING
// PATH verbatim (ruling P-47).
//
// Dropping PATH is the obvious reading of "reduced environment" and it breaks
// every command, so it is not what this does. What it removes is provider API
// keys, which a subprocess has no business reading and which would otherwise
// be one `env` away from any command the model writes.
//
// This is a real but LIMITED protection, and the limit is worth stating: a
// command can still read the keys from any file the agent can read. The
// boundary is the interceptor, not this function.
func ReducedEnv(base []string, extraPrefixes ...string) []string {
	prefixes := append([]string{
		"ANTHROPIC_", "OPENAI_", "GOOGLE_", "GEMINI_", "GROQ_", "DEEPSEEK_",
		"OPENROUTER_", "AZURE_OPENAI_", "AWS_", "MISTRAL_", "COHERE_",
		"TOGETHER_", "FIREWORKS_", "XAI_", "AGENTKIT_",
	}, extraPrefixes...)
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		drop := false
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
