//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// boundProcessGroup puts the command in its own process group and makes the
// context timeout kill the whole group, not just the top process, so a runaway
// chain cannot outlive the test.
func boundProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
