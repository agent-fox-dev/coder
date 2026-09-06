//go:build windows

package mcp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// setProcessGroup starts the child in a new process group. Windows has no
// process groups in the POSIX sense; the tree is killed via taskkill instead
// (REQ-TOOL-17.2).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killGroup terminates the whole tree with taskkill /F /T, resolved by
// ABSOLUTE PATH.
//
// Resolving it through PATH would let a repository being edited by the agent
// drop a taskkill.exe into the working directory and have it run as part of
// killing a timed-out command.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	tk := filepath.Join(root, "System32", "taskkill.exe")
	kill := exec.Command(tk, "/F", "/T", "/PID", fmt.Sprint(cmd.Process.Pid))
	if err := kill.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}
