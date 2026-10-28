//go:build unix

package testrun

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

// ProcessStart returns the start time of process pid as ps reports it
// (lstart, in the C locale). Together with the pid it identifies a process,
// since a pid can be reused once its process exits.
func ProcessStart(pid int) (string, error) {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	start := strings.Join(strings.Fields(string(out)), " ")
	if err != nil || start == "" {
		return "", fmt.Errorf("read start time of pid %d with ps: %v (output %q)", pid, err, out)
	}
	return start, nil
}
