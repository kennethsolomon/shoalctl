//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

// detach gives cmd its own process group, so Ctrl-C in this console never
// reaches the watcher. (start needs tmux, which Windows has no native build of.)
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
