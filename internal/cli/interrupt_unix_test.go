//go:build unix

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// longSuite writes the suite shell's pid (the process group leader) and a
// child's pid into dir, then runs for 30 s.
func longSuite(dir string) string {
	return fmt.Sprintf("[tests]\ncommand = \"echo $$ > '%s/sh.pid'; sleep 30 & echo $! > '%s/child.pid'; wait\"\nformat = \"exit-code\"\n", dir, dir)
}

const quickSuite = "[tests]\ncommand = \"true\"\nformat = \"exit-code\"\n"

func readPID(t *testing.T, path string) int {
	t.Helper()
	waitFor(t, func() bool {
		data, err := os.ReadFile(path)
		return err == nil && strings.TrimSpace(string(data)) != ""
	})
	data, _ := os.ReadFile(path)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("pid in %s: %v", path, err)
	}
	return pid
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, within time.Duration, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(within)
	for _, pid := range pids {
		for alive(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("process %d is still running", pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func lastRunStatus(t *testing.T, p *project) string {
	t.Helper()
	db := openRaw(t, p.dbPath())
	defer db.Close()
	var status string
	if err := db.QueryRow(`SELECT status FROM test_runs ORDER BY id DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestStartInterruptStopsBaseline(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			h := newHarness(t).withSession()
			pids := t.TempDir()
			p := h.initWith(longSuite(pids))
			h.mustRun("new", "feature", "Interrupt me")

			cmd := h.command("start", "1")
			var errb bytes.Buffer
			cmd.Stderr = &errb
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			sh, child := readPID(t, filepath.Join(pids, "sh.pid")), readPID(t, filepath.Join(pids, "child.pid"))
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var err error
			select {
			case err = <-done:
			case <-time.After(15 * time.Second):
				cmd.Process.Kill()
				t.Fatal("start did not exit after the signal")
			}
			if code := exitCode(err); code != ExitInterrupted {
				t.Fatalf("start exited %d, want %d\nstderr: %s", code, ExitInterrupted, errb.String())
			}
			if !strings.Contains(errb.String(), "interrupted") {
				t.Errorf("stderr: %s", errb.String())
			}
			waitGone(t, 2*time.Second, sh, child)
			if got := lastRunStatus(t, p); got != "cancelled" {
				t.Errorf("test run status %q, want cancelled", got)
			}
			if got := h.task("1"); got.Stage != "baseline-failed" || got.TmuxWindow != "" {
				t.Errorf("task after interrupt: %+v", got)
			}
			if n := len(claudeLaunches(t, h.argsLog)); n != 0 {
				t.Errorf("claude launched %d times after an interrupted baseline", n)
			}

			writeConfig(t, p, quickSuite)
			h.mustRun("start", "1")
			if got := lastRunStatus(t, p); got != "completed" {
				t.Errorf("resumed start's baseline: %q", got)
			}
		})
	}
}

func TestDoneInterruptStopsFinalRun(t *testing.T) {
	h := newHarness(t).withSession()
	pids := t.TempDir()
	p := h.initWith(quickSuite)
	task := startedTask(t, h, "feature", "Interrupt done")
	commitIn(t, task.Worktree, "feature.txt", "x")
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "summary.json"), `{"decision": "d", "why": "w"}`)
	writeConfig(t, p, longSuite(pids))

	cmd := h.command("done", task.Slug)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sh, child := readPID(t, filepath.Join(pids, "sh.pid")), readPID(t, filepath.Join(pids, "child.pid"))
	cmd.Process.Signal(os.Interrupt)
	if code := exitCode(cmd.Wait()); code != ExitInterrupted {
		t.Fatalf("done exited %d, want %d\nstderr: %s", code, ExitInterrupted, errb.String())
	}
	waitGone(t, 2*time.Second, sh, child)
	if got := lastRunStatus(t, p); got != "cancelled" {
		t.Errorf("final run status %q, want cancelled", got)
	}
	if got := h.task(task.Slug); got.Stage != "in-progress" {
		t.Errorf("interrupted done changed the task: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "reports", task.Slug+".md")); !os.IsNotExist(err) {
		t.Errorf("interrupted done wrote a report: %v", err)
	}
}

func TestStartRefusesWhileAbandonedSuiteRuns(t *testing.T) {
	h := newHarness(t).withSession()
	pids := t.TempDir()
	p := h.initWith(longSuite(pids))
	h.mustRun("new", "feature", "Abandon me")

	cmd := h.command("start", "1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sh := readPID(t, filepath.Join(pids, "sh.pid"))
	readPID(t, filepath.Join(pids, "child.pid"))
	cmd.Process.Kill() // SIGKILL: no chance to stop the suite
	cmd.Wait()
	t.Cleanup(func() { syscall.Kill(-sh, syscall.SIGKILL) })
	if !alive(sh) {
		t.Fatal("expected the suite to outlive a SIGKILLed shoalctl")
	}

	errOut := h.expectCode(ExitBusy, "start", "1")
	for _, want := range []string{"still running", strconv.Itoa(sh)} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q: %s", want, errOut)
		}
	}
	if n := countRows(t, p, `SELECT count(*) FROM test_runs`); n != 1 {
		t.Fatalf("a second suite was started next to the abandoned one: %d runs", n)
	}

	syscall.Kill(-sh, syscall.SIGKILL)
	waitGone(t, 2*time.Second, sh)
	writeConfig(t, p, quickSuite)
	h.mustRun("start", "1")
	db := openRaw(t, p.dbPath())
	defer db.Close()
	var status string
	if err := db.QueryRow(`SELECT status FROM test_runs WHERE id = 1`).Scan(&status); err != nil || status != "error" {
		t.Fatalf("abandoned run status %q (%v), want error", status, err)
	}
	log := readFile(t, filepath.Join(p.runDir(1), "output.log"))
	if !strings.Contains(log, "abandoned") {
		t.Errorf("output.log of the abandoned run should say so:\n%s", log)
	}
}

func writeConfig(t *testing.T, p *project, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
