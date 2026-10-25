//go:build windows

package testrun

import "os/exec"

// setProcessGroup keeps exec's default cancel (kill the process) on Windows.
func setProcessGroup(*exec.Cmd) {}
