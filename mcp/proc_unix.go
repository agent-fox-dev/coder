//go:build !windows

package mcp

import (
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
