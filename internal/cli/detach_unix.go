//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in its own session, so closing the terminal (SIGHUP) or
// Ctrl-C in it never reaches the watcher.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
