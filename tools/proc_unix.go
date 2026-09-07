//go:build !windows

package tools

import (
	"fmt"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so the whole tree
// can be killed as a unit (REQ-TOOL-17.1).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills the entire process group, falling back to the direct child
// only if the group is already gone.
//
// The negative pid is the whole point: kill(pid) reaps the shell and orphans
// every grandchild it started, which is how a `npm run dev &` survives the
// command that launched it and keeps writing to a pipe nobody is reading.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// signalExitCode reports a signal-killed child as 128+signum, the convention
// every unix shell uses for $? (NFR-COMPAT-06). Go's ExitCode() returns -1 for
// the same case, which no shell script, CI matcher or human reads as "killed
// by SIGKILL".
func signalExitCode(ee *exec.ExitError) (int, bool) {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return 128 + int(ws.Signal()), true
}

func resolveShell() (string, []string, error) {
	// Fixed ladder. $SHELL is deliberately not consulted: it names the user's
	// INTERACTIVE shell, and a command written for bash is not portable to
	// fish or nushell.
	if p, ok := lookPathAny("/bin/bash"); ok {
		return p, []string{"-c"}, nil
	}
	if p, ok := lookPathAny("bash"); ok {
		return p, []string{"-c"}, nil
	}
	if p, ok := lookPathAny("sh"); ok {
		return p, []string{"-c"}, nil
	}
	return "", nil, fmt.Errorf(
		"tools: no usable shell found (searched /bin/bash, bash on PATH, sh on PATH)")
}
