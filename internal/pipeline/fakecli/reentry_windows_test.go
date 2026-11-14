//go:build windows

package main

import "os/exec"

// boundProcessGroup is a no-op on Windows: the run's context timeout still
// ends the top process, and the depth guard bounds the rest.
func boundProcessGroup(cmd *exec.Cmd) {}
