//go:build windows

package tools

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

func resolveShell() (string, []string, error) {
	// Git Bash, or a hard error naming what was searched. Never cmd.exe: the
	// grammar differs so completely that a command written for bash does not
	// fail loudly there, it does something else.
	var candidates []string
	for _, pf := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if pf != "" {
			candidates = append(candidates, filepath.Join(pf, "Git", "bin", "bash.exe"))
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, []string{"-c"}, nil
		}
	}
	if p, ok := lookPathAny("bash.exe", "bash"); ok {
		return p, []string{"-c"}, nil
	}
	return "", nil, fmt.Errorf(
		"tools: no bash-family shell found on Windows. Searched: %v, and bash.exe on PATH. "+
			"Install Git for Windows. cmd.exe is deliberately not used: its grammar differs "+
			"enough that a bash command does not fail loudly there, it does something else.",
		candidates)
}
