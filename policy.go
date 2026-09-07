package agentkit

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/agentfox/agentkit-go/core"
)

// This file is OQ-8's resolution: the `execute` boundary in non-interactive
// deployments.
//
// REQ-SEC-03 replaced the command allowlist with a per-call interceptor on the
// grounds that a static allowlist is both trivially escaped and too narrow to
// run a build. That reasoning assumes an embedder that can answer a permission
// question. A daemon triaging issues overnight cannot, and "allow" by default
// is strictly worse than the allowlist it replaced. So, per OQ-8's
// recommendation (b) plus (a):
//
//   - a run fails LOUDLY when a shell tool is in the resolved set and no
//     interceptor is configured (ErrUnguardedExecute), and
//   - RestrictedPolicy ships as an importable, REPLACEABLE starting point —
//     kept out of the SDK's enforcement path so it can be swapped rather than
//     only narrowed.

// ShellToolNames are the tools the guard treats as a shell. A caller-supplied
// tool of the same name counts: the name is what the model calls, and a
// custom `execute` is no less a shell for being custom.
var ShellToolNames = []string{"execute", "run_command", "powershell"}

// AllowAllToolCalls is the explicit opt-out from the guard: an interceptor
// that never blocks. Passing it is the affirmative act OQ-8 asks for — the
// embedder has said, in code, that this agent runs an unrestricted shell.
func AllowAllToolCalls(context.Context, core.BeforeToolCallContext) core.BeforeToolCallDecision {
	return core.BeforeToolCallDecision{}
}

// checkExecuteGuard is consulted at the head of every run (before the slot is
// claimed, so the failure is an ordinary returned error and not a stream
// nobody reads).
func (a *Agent) checkExecuteGuard() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.BeforeToolCall != nil {
		return nil
	}
	for _, t := range ResolveToolPolicy(a.tools, a.cfg.ToolPolicy) {
		if isShellTool(t.Name) {
			return fmt.Errorf("%w (tool %q)", core.ErrUnguardedExecute, t.Name)
		}
	}
	return nil
}

func isShellTool(name string) bool {
	for _, n := range ShellToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// RestrictedOptions configures RestrictedPolicy.
type RestrictedOptions struct {
	// AllowedPrograms are the program names (the basename of argv[0], or of
	// the first word of an `execute` command) that may run. Empty means every
	// shell call is blocked, which is the safe default for a policy whose
	// only purpose is to say no.
	AllowedPrograms []string
	// AllowShellOperators permits pipes, `;`, `&&`, `||`, redirection,
	// subshells, command substitution and variable expansion in `execute`
	// commands. Off by default. The grammar this filter understands is POSIX
	// sh / bash quoting — single quotes literal, double quotes expanding —
	// and it is declared here because REQ-SEC-04 requires a filter to say
	// which grammar it filters.
	AllowShellOperators bool
	// PowerShellFilter decides `powershell` calls. There is no PowerShell
	// grammar filter in this file, so per REQ-SEC-04 the tool is REFUSED
	// OUTRIGHT unless the embedder supplies one: a control that silently does
	// not hold on one of the supported shells is worse than no shell.
	PowerShellFilter func(command string) (block bool, reason string)
	// BlockedTools are refused by name, shell or not.
	BlockedTools []string
	// TerminateOnBlock casts the REQ-TOOL-13.2 vote so that a refusal ends
	// the run instead of looping the model into retrying.
	TerminateOnBlock bool
}

// RestrictedPolicy is the reference interceptor for headless embedders: an
// allowlist of programs plus shell-operator rejection.
//
// It is a FLOOR, not a sandbox. REQ-SEC-03's argument still holds — an
// allowlist wide enough to run a build is escapable through the allowed
// programs' own configuration and subprocess surfaces — and this policy does
// nothing about that. What it does is make the unattended default "no"
// instead of "yes", which is the difference OQ-8 exists to close. Embedders
// with real context should replace it, not extend it.
func RestrictedPolicy(o RestrictedOptions) core.BeforeToolCall {
	allowed := make(map[string]bool, len(o.AllowedPrograms))
	for _, p := range o.AllowedPrograms {
		allowed[path.Base(strings.TrimSpace(p))] = true
	}
	blocked := make(map[string]bool, len(o.BlockedTools))
	for _, t := range o.BlockedTools {
		blocked[t] = true
	}
	block := func(reason string) core.BeforeToolCallDecision {
		return core.BeforeToolCallDecision{Block: true, Terminate: o.TerminateOnBlock, Reason: reason}
	}

	return func(_ context.Context, in core.BeforeToolCallContext) core.BeforeToolCallDecision {
		if blocked[in.ToolName] {
			return block(fmt.Sprintf("RestrictedPolicy: tool %q is not permitted", in.ToolName))
		}
		switch in.ToolName {
		case "execute":
			cmd, _ := in.Arguments["command"].(string)
			if !o.AllowShellOperators {
				if op, found := firstShellOperator(cmd); found {
					return block(fmt.Sprintf("RestrictedPolicy: shell operator %q is not permitted "+
						"(grammar: POSIX sh); use run_command with an argument list, or one plain command", op))
				}
			}
			prog := firstProgram(cmd)
			if prog == "" || !allowed[prog] {
				return block(fmt.Sprintf("RestrictedPolicy: program %q is not on the allowlist", prog))
			}
		case "run_command":
			argv, _ := in.Arguments["argv"].([]any)
			prog := ""
			if len(argv) > 0 {
				s, _ := argv[0].(string)
				prog = path.Base(strings.TrimSpace(s))
			}
			if prog == "" || !allowed[prog] {
				return block(fmt.Sprintf("RestrictedPolicy: program %q is not on the allowlist", prog))
			}
		case "powershell":
			if o.PowerShellFilter == nil {
				return block("RestrictedPolicy: powershell is refused outright; this policy filters " +
					"POSIX sh only and has no PowerShell grammar (REQ-SEC-04)")
			}
			cmd, _ := in.Arguments["command"].(string)
			if b, reason := o.PowerShellFilter(cmd); b {
				return block("RestrictedPolicy: " + reason)
			}
		}
		return core.BeforeToolCallDecision{}
	}
}

// firstShellOperator scans a POSIX-sh command for the first construct that
// hands control to the shell — a pipe, a list operator, redirection, a
// subshell, command substitution or parameter expansion — honouring the two
// quoting rules that matter: nothing expands inside single quotes, and only
// `$` and backtick expand inside double quotes. Returns the operator found.
func firstShellOperator(cmd string) (string, bool) {
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			switch c {
			case '"':
				inDouble = false
			case '\\':
				i++ // the next byte is literal
			case '$', '`':
				return string(c), true
			}
		default:
			switch c {
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case '\\':
				i++
			case '|', ';', '&', '<', '>', '(', ')', '`', '$', '\n':
				return string(c), true
			}
		}
	}
	return "", false
}

// firstProgram returns the basename of the first word of a command, skipping
// leading VAR=value assignments — `FOO=1 make` runs make.
func firstProgram(cmd string) string {
	for _, w := range strings.Fields(cmd) {
		if i := strings.IndexByte(w, '='); i > 0 && !strings.ContainsAny(w[:i], "/") {
			continue
		}
		return path.Base(strings.Trim(w, `"'`))
	}
	return ""
}
