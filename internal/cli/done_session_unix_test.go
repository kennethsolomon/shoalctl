//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// readyTask starts a task and leaves it committed with a summary, so only the
// in-flight checks stand between it and done.
func readyTask(t *testing.T, h *harness, title string) (*project, string, string) {
	t.Helper()
	p := h.initWith(quickSuite)
	task := startedTask(t, h, "feature", title)
	commitIn(t, task.Worktree, "feature.txt", "x")
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "summary.json"), `{"decision": "d", "why": "w"}`)
	return p, task.Slug, task.Worktree
}

func TestDoneRefusesWhileSessionRuns(t *testing.T) {
	h := newHarness(t).withSession()
	p, slug, _ := readyTask(t, h, "Live session")
	window := h.task(slug).TmuxWindow

	errOut := h.expectCode(ExitDoneRefused, "done", slug)
	for _, want := range []string{"still running", window, "--force"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q: %s", want, errOut)
		}
	}
	if n := countRows(t, p, `SELECT count(*) FROM test_runs WHERE kind = 'final'`); n != 0 {
		t.Fatalf("a refused done ran the final suite (%d runs)", n)
	}
	if got := h.task(slug); got.Stage != "in-progress" {
		t.Fatalf("refused done changed the task: %+v", got)
	}

	// The agent exits claude: the pane stays, dead (remain-on-exit).
	pid, err := strconv.Atoi(h.tmuxOut("list-panes", "-t", window, "-F", "#{pane_pid}"))
	if err != nil {
		t.Fatal(err)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	waitFor(t, func() bool { return h.tmuxOut("list-panes", "-t", window, "-F", "#{pane_dead}") == "1" })
	h.mustRun("done", slug)
}

func TestDoneProceedsWhenWindowIsGone(t *testing.T) {
	h := newHarness(t).withSession()
	_, slug, _ := readyTask(t, h, "Window gone")
	h.tmuxOut("kill-window", "-t", h.task(slug).TmuxWindow)
	h.mustRun("done", slug)
}

func TestDoneWaitsForIndexLock(t *testing.T) {
	h := newHarness(t).withSession()
	_, slug, wt := readyTask(t, h, "Index lock")
	lock := filepath.Join(testutil.Git(t, wt, "rev-parse", "--absolute-git-dir"), "index.lock")
	orig := indexLockWait
	indexLockWait = 500 * time.Millisecond
	t.Cleanup(func() { indexLockWait = orig })

	testutil.WriteFile(t, lock, "")
	errOut := h.expectCode(ExitDoneRefused, "done", "--force", slug)
	if !strings.Contains(errOut, "index.lock") || !strings.Contains(errOut, lock) {
		t.Fatalf("stderr: %s", errOut)
	}

	indexLockWait = 5 * time.Second
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Remove(lock)
	}()
	h.mustRun("done", "--force", slug)
}

func TestDoneRefusesWhenBranchMovesDuringFinalRun(t *testing.T) {
	h := newHarness(t).withSession()
	p, slug, _ := readyTask(t, h, "Sneaky commit")
	writeConfig(t, p, "[tests]\ncommand = \"git commit -q --allow-empty -m sneaky\"\nformat = \"exit-code\"\n")
	errOut := h.expectCode(ExitDoneRefused, "done", "--force", slug)
	if !strings.Contains(errOut, "changed while") {
		t.Fatalf("stderr: %s", errOut)
	}
	if got := h.task(slug); got.Stage != "in-progress" {
		t.Fatalf("done recorded a task whose branch moved: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(p.dir, "reports", slug+".md")); !os.IsNotExist(err) {
		t.Fatalf("report written for a moved branch: %v", err)
	}
}
