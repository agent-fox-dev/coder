package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	// DrainIdle is how long the output pipe must stay QUIET after the child
	// has exited before draining stops (REQ-TOOL-17.5). Zero means
	// defaultDrainIdle. It is an idle interval, not a deadline: every read
	// re-arms it, so a descendant that keeps writing keeps being drained.
	DrainIdle time.Duration
	// DrainCeiling bounds the whole post-exit drain, so a descendant writing
	// forever cannot hold the tool open forever. Zero means
	// defaultDrainCeiling.
	DrainCeiling time.Duration
}

// The post-exit drain defaults (REQ-TOOL-17.5). The idle interval matches the
// old fixed WaitDelay, so a command with no surviving descendant is bounded
// exactly as before; the difference is that output ARRIVING inside the window
// now re-arms it instead of racing a constant.
const (
	defaultDrainIdle    = 2 * time.Second
	defaultDrainCeiling = 10 * time.Second
)

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
//   - The pipe is OURS (os.Pipe), not one exec created, and after the child
//     exits output is drained on a RE-ARMING idle timer rather than a fixed
//     post-exit deadline (REQ-TOOL-17.5). exec closes the pipes it made as
//     soon as Wait returns, which truncates a detached descendant's output at
//     a constant — losing precisely the tail of a background job's log.
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
	prog := argv[0]
	if hasPathSeparator(prog) && !filepath.IsAbs(prog) && opts.Dir != "" {
		// `./script.sh` means "in the directory the command runs in", which
		// is cmd.Dir — the workspace — and not the process working
		// directory that exec.LookPath would consult. Resolved here so the
		// lookup and the run agree on what "." means.
		prog = filepath.Join(opts.Dir, prog)
	}
	bin, err := exec.LookPath(prog)
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
	// REQ-TOOL-17.3: a backstop for exec's own bookkeeping. It no longer
	// bounds the drain — the pipe below is not exec's to close — and the real
	// bound is DrainCeiling.
	cmd.WaitDelay = 2 * time.Second

	acc := NewAccumulator(opts.MaxBytes, TruncateTail)
	acc.SpillDir, acc.SpillPrefix = opts.SpillDir, "agentkit-exec"
	defer acc.Close()

	// ONE pipe for both streams, so they interleave in true write order
	// (REQ-TOOL-17.4), and OUR pipe rather than the one exec would create for
	// an io.Writer: exec closes the pipes it created the moment Wait returns,
	// which is the fixed post-exit deadline REQ-TOOL-17.5 rules out. A pipe
	// exec did not create, exec does not close — the same reason the MCP
	// stdio transport owns its pipes.
	pr, pw, err := os.Pipe()
	if err != nil {
		return ExecResult{}, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw

	start := time.Now()
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return ExecResult{}, err
	}
	// The child holds the write end now. Ours must go, or the reader never
	// sees EOF — it would be waiting on a writer that is this very process.
	_ = pw.Close()

	// One copier goroutine feeds the accumulator; the sink serialises its
	// writes against this goroutine's read of the result and can be stopped,
	// so a descendant still writing after the drain gave up cannot race the
	// accumulator or resurrect its spill file.
	sink := &drainSink{acc: acc}
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		_, _ = io.Copy(sink, pr)
	}()

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
	// Duration is measured at the child's exit, above: the drain that follows
	// is the SDK waiting on a descendant, not the command running.
	drainAfterExit(pr, copyDone, sink, opts.DrainIdle, opts.DrainCeiling)

	exitCode := 0
	signaled := false
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		exitCode = ee.ExitCode()
		// NFR-COMPAT-06: a signal-killed child reports 128+signum on unix,
		// the convention every shell uses, rather than Go's -1 placeholder.
		// On Windows the wait status carries no signal and signalExitCode
		// never claims one.
		if code, ok := signalExitCode(ee); ok {
			exitCode, signaled = code, true
		} else if exitCode == -1 {
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

// drainSink is the accumulator's gate during the post-exit drain.
//
// Two goroutines are involved — the copier reading the pipe and the caller
// reading the result — and the copier may still be blocked in a read on a
// descendant's pipe when the drain gives up. The mutex serialises the two, and
// stop() makes every later write a no-op: an Accumulator write after
// Accumulator.Close would otherwise re-create the spill file that Close just
// released, and the caller's read of the retained window would race the write.
type drainSink struct {
	mu      sync.Mutex
	acc     *Accumulator
	stopped bool
	// lastWrite is the UnixNano of the most recent byte. It is what re-arms
	// the idle timer (REQ-TOOL-17.5), so it is written on every read even when
	// the sink has stopped accumulating.
	lastWrite atomic.Int64
}

func (s *drainSink) Write(p []byte) (int, error) {
	s.lastWrite.Store(time.Now().UnixNano())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		// Reported as written rather than as an error: the copier is being
		// wound down deliberately, and an error here would only make it log a
		// failure for output nobody is waiting for any more.
		return len(p), nil
	}
	return s.acc.Write(p)
}

// stop closes the sink. It returns once any in-flight write has finished, so
// the caller may read the accumulator afterwards without further locking.
func (s *drainSink) stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
}

// drainAfterExit is REQ-TOOL-17.5's re-arming idle drain.
//
// The child has exited, but a DETACHED DESCENDANT inherited the write end of
// the pipe and can keep writing for as long as it likes. A fixed post-exit
// deadline cuts that output off at a constant — which is why the requirement
// rules one out — and loses exactly the tail of a background job's log.
//
// So: keep copying while output keeps arriving, re-arming the idle timer at
// every read, and stop only once the pipe has been QUIET for idle. The ceiling
// is the second bound and a different question: idle answers "is anyone still
// writing", ceiling answers "how long will we wait on someone who never
// stops".
func drainAfterExit(pr *os.File, copyDone <-chan struct{}, sink *drainSink, idle, ceiling time.Duration) {
	if idle <= 0 {
		idle = defaultDrainIdle
	}
	if ceiling <= 0 {
		ceiling = defaultDrainCeiling
	}
	idleTimer := time.NewTimer(idle)
	defer idleTimer.Stop()
	ceilingTimer := time.NewTimer(ceiling)
	defer ceilingTimer.Stop()

	for drained := false; !drained; {
		select {
		case <-copyDone:
			// EOF: every writer, descendants included, has let the pipe go.
			drained = true
		case <-idleTimer.C:
			// Re-armed for the REMAINDER of the window rather than a fresh
			// one, so the timer measures quiet time since the last read and
			// not since the last tick.
			if quiet := time.Since(time.Unix(0, sink.lastWrite.Load())); quiet < idle {
				idleTimer.Reset(idle - quiet)
				continue
			}
			drained = true
		case <-ceilingTimer.C:
			drained = true
		}
	}

	// Stop first, then close: after stop the copier cannot touch the
	// accumulator, so whether closing the read end unblocks it (it does
	// wherever a pipe read is pollable) only decides when its goroutine ends,
	// never what the caller sees.
	sink.stop()
	_ = pr.Close()
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

// hasPathSeparator reports whether a program name is a path rather than a
// bare name to look up on PATH. Both separators are checked on every platform:
// a model writes `./x` on Windows too.
func hasPathSeparator(name string) bool {
	return strings.ContainsAny(name, `/\`)
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
// every command, so it is not what this does. What it removes is credentials,
// which a subprocess has no business reading and which would otherwise be one
// `env` away from any command the model writes (REQ-SEC-08). Two rules:
//
//   - provider PREFIXES, for the keys the SDK itself knows about; and
//   - generic SUFFIXES — *_TOKEN, *_SECRET, *_API_KEY, *_PASSWORD,
//     *_CREDENTIALS — because a provider list is only ever the providers
//     someone thought of, and GITHUB_TOKEN or NPM_TOKEN in a developer's shell
//     is at least as valuable to exfiltrate as an Anthropic key.
//
// PATH, HOME, LANG, TMPDIR and TERM are kept unconditionally: no suffix rule
// touches them, and they are what a command needs to run at all.
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
		if !credentialName(name, prefixes) {
			out = append(out, kv)
		}
	}
	return out
}

// credentialSuffixes is the generic half of REQ-SEC-08's rule, matched
// case-insensitively: `github_token` is as much a token as `GITHUB_TOKEN`.
var credentialSuffixes = []string{"_TOKEN", "_SECRET", "_API_KEY", "_PASSWORD", "_CREDENTIALS"}

// keptEnv names variables ReducedEnv never drops, whatever they are called.
var keptEnv = map[string]bool{"PATH": true, "HOME": true, "LANG": true, "TMPDIR": true, "TERM": true}

func credentialName(name string, prefixes []string) bool {
	upper := strings.ToUpper(name)
	if keptEnv[upper] {
		return false
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, suf := range credentialSuffixes {
		if strings.HasSuffix(upper, suf) {
			return true
		}
	}
	return false
}
