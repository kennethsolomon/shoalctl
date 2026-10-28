//go:build windows

package testrun

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procGroup is a job object holding the suite and everything it starts. The
// job kills its processes when its last handle closes, so the suite also dies
// when shoalctl itself is killed.
type procGroup struct{ job windows.Handle }

// prepare keeps the suite out of shoalctl's console process group, so
// shoalctl decides how the suite stops on Ctrl-C.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// attach puts the started suite into a new kill-on-close job object. A child
// the suite spawns before this returns is not in the job; the window is the
// few microseconds after process creation.
func attach(cmd *exec.Cmd) (*procGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("configure job object: %w", err)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("open suite process %d: %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("assign suite process %d to its job object: %w", cmd.Process.Pid, err)
	}
	return &procGroup{job: job}, nil
}

// terminate has no gentle form for a job: it ends every process in it.
func (g *procGroup) terminate() error { return windows.TerminateJobObject(g.job, 1) }
func (g *procGroup) kill() error      { return windows.TerminateJobObject(g.job, 1) }
func (g *procGroup) close()           { windows.CloseHandle(g.job) }

// GroupAlive reports whether the suite process pid is still running.
func GroupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// ProcessStart returns the creation time of process pid. Together with the
// pid it identifies a process, since a pid can be reused once its process
// exits.
func ProcessStart(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", fmt.Errorf("read start time of process %d: %w", pid, err)
	}
	return strconv.FormatInt(created.Nanoseconds(), 10), nil
}
