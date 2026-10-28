//go:build unix

package testrun

import (
	"errors"
	"os/exec"
	"syscall"
)

// procGroup is the suite's process group: the suite shell leads it, so every
// process the suite starts can be signalled at once.
type procGroup struct{ pgid int }

// prepare puts the command in its own process group. Ctrl-C at the terminal
// reaches shoalctl's group, not the suite's, so shoalctl forwards it.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attach(cmd *exec.Cmd) (*procGroup, error) {
	return &procGroup{pgid: cmd.Process.Pid}, nil
}

func (g *procGroup) terminate() error { return ignoreGone(syscall.Kill(-g.pgid, syscall.SIGTERM)) }
func (g *procGroup) kill() error      { return ignoreGone(syscall.Kill(-g.pgid, syscall.SIGKILL)) }
func (g *procGroup) close()           {}

func ignoreGone(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// GroupAlive reports whether a suite's process group, led by pid, still has
// a member.
func GroupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
