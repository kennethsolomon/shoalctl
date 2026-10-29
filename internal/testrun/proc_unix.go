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
	"time"
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

// parseStart parses a ProcessStart value: ps lstart in the C locale. Any
// other form, such as the Windows nanosecond count, does not parse here.
func parseStart(s string) (time.Time, bool) {
	t, err := time.Parse(time.ANSIC, s)
	return t, err == nil
}

// gatePrefix makes the suite shell wait for a line on fd 3 before running
// the command, and exit without running it if the pipe closes first.
const gatePrefix = "IFS= read -r _ <&3 || exit 125\nexec 3<&-\n"

// gate holds the suite at its first line until its pid is recorded, so a
// shoalctl killed in between leaves no suite running that nothing knows of.
type gate struct{ r, w *os.File }

// hold arms the gate on cmd, an unstarted sh -c command with no ExtraFiles.
func hold(cmd *exec.Cmd) (*gate, error) {
	if len(cmd.ExtraFiles) != 0 {
		return nil, fmt.Errorf("hold suite: cmd already has %d extra files; the gate needs fd 3", len(cmd.ExtraFiles))
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("hold suite: create gate pipe: %w", err)
	}
	cmd.ExtraFiles = []*os.File{r}
	cmd.Args[len(cmd.Args)-1] = gatePrefix + cmd.Args[len(cmd.Args)-1]
	return &gate{r: r, w: w}, nil
}

// started drops shoalctl's copy of the read end once the suite holds its own.
func (g *gate) started() { g.r.Close() }

// open lets the suite run.
func (g *gate) open() error {
	_, err := g.w.Write([]byte("\n"))
	if cerr := g.w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("release the held suite: %w", err)
	}
	return nil
}

// shut makes a held suite exit without running its command.
func (g *gate) shut() {
	g.r.Close()
	g.w.Close()
}
