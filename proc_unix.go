//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup runs the command in its own process group so that stopping
// it also stops anything it started (test runners often fork).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
