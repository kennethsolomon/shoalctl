package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.ProcessState.Pid()
}

func TestDoctorFullChecksAndFixes(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.env.Exe = fakeBinary(t, t.TempDir())
	p := h.initWith(junitConfig)
	a := startedTask(t, h, "feature", "Window closes")
	b := startedTask(t, h, "feature", "Worktree deleted")
	c := startedTask(t, h, "feature", "Merge interrupted")
	mustContain(t, "healthy", h.mustRun("doctor"), "ok   no task stuck at starting", "ok   no stale claims", "ok   merge queue")

	h.tmuxOut("kill-window", "-t", a.TmuxWindow)
	if err := os.RemoveAll(b.Worktree); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(p.dir, "worktrees", "stray")
	testutil.WriteFile(t, filepath.Join(stray, "notes.txt"), "mine")
	commitIn(t, c.Worktree, "feature.txt", "x")
	finish(t, h, c.Slug, c.Worktree)
	setStage(t, p, c.ID, store.StageMerging)
	sqlExec(t, p, `UPDATE merge_queue SET state = 'rebasing' WHERE task_id = ?`, c.ID)
	sqlExec(t, p, `INSERT INTO test_runs (task_id, kind, sha, command, command_hash, format, fidelity, status, output_path, started_at)
		VALUES (?, 'gate', 'x', 'true', 'h', 'junit', 'per-test', 'running', ?, '2026-01-01T00:00:00Z')`,
		a.ID, filepath.Join(t.TempDir(), "output.log"))
	mustContain(t, "no pid blocks start", h.expectCode(ExitBusy, "qa", a.Slug), "no recorded pid")
	sqlExec(t, p, `INSERT INTO test_runs (task_id, kind, sha, command, command_hash, format, fidelity, status, output_path, pid, started_at)
		VALUES (?, 'qa', 'x', 'true', 'h', 'junit', 'per-test', 'running', ?, ?, '2026-01-01T00:00:00Z')`,
		a.ID, filepath.Join(t.TempDir(), "output.log"), deadPID(t))
	settings := readFile(t, filepath.Join(h.repo, ".claude", "settings.local.json"))
	wtSettings := filepath.Join(a.Worktree, ".claude", "settings.local.json")
	testutil.WriteFile(t, wtSettings, settings)

	errOut := h.expectCode(ExitDoctor, "doctor")
	mustContain(t, "report", errOut, a.Slug+": tmux window "+a.TmuxWindow+" is gone", b.Slug+" (in-progress): its worktree "+b.Worktree+" is missing",
		stray+" has no task", "git still lists 1 gone shoal worktree", "merge queue: "+c.Slug+" is at rebasing", c.Slug+" is at merging",
		"no shoalctl holds "+a.Slug, "registers shoal hooks a second time")
	if h.task(c.Slug).Stage != store.StageMerging || h.task(b.Slug).Stage != store.StageInProgress {
		t.Fatal("doctor without --fix changed a task")
	}

	code, out, errOut := h.run("doctor", "--fix")
	if code != ExitDoctor {
		t.Fatalf("doctor --fix: %d\n%s\n%s", code, out, errOut)
	}
	mustContain(t, "fixed", out, "fixed "+a.Slug+": forgot the gone tmux window", "fixed git worktree prune", "fixed merge queue: "+c.Slug+" from rebasing back to waiting",
		c.Slug+": recovered", "fixed abandoned run(s) of "+a.Slug, "never recorded a pid (its suite never ran); marked error", "fixed removed shoal's hook entries from "+wtSettings)
	mustContain(t, "left for a human", errOut, b.Slug+": its worktree "+b.Worktree+" is missing; marked worktree-missing (was in-progress)", stray+" has no task")
	if got := h.task(c.Slug); got.Stage != store.StageDone {
		t.Fatalf("c not settled: %+v", got)
	}
	if got := h.task(a.Slug); got.TmuxWindow != "" {
		t.Fatalf("a window: %+v", got)
	}
	if strings.Contains(readFile(t, wtSettings), "hook session-start") {
		t.Fatal("worktree settings still register shoal")
	}
	if n := countRows(t, p, `SELECT count(*) FROM test_runs WHERE status = 'running'`); n != 0 {
		t.Fatalf("%d run(s) still running", n)
	}

	// The human recreates b's worktree and removes the stray directory.
	testutil.Git(t, h.repo, "worktree", "add", b.Worktree, b.Branch)
	os.RemoveAll(stray)
	mustContain(t, "restore", h.mustRun("doctor", "--fix"), "fixed "+b.Slug+": worktree "+b.Worktree+" is back; restored to in-progress")
	if got := h.task(b.Slug); got.Stage != store.StageInProgress {
		t.Fatalf("b: %+v", got)
	}
	h.mustRun("doctor")
}

// A merge killed mid-rebase leaves the worktree rebasing: doctor asks before
// aborting it, and only a human at a terminal can say yes.
func TestDoctorAbortsAnInterruptedMergeRebaseAfterAsking(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.env.Exe = fakeBinary(t, t.TempDir())
	p := h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Rebase dies")
	commitIn(t, task.Worktree, "shared.txt", "from task\n")
	finish(t, h, task.Slug, task.Worktree)
	commitIn(t, h.repo, "shared.txt", "from main\n")
	head := testutil.Git(t, task.Worktree, "rev-parse", "HEAD")
	cmd := exec.Command("git", "rebase", "main")
	cmd.Dir = task.Worktree
	if cmd.Run() == nil {
		t.Fatal("expected a conflicting rebase")
	}
	setStage(t, p, task.ID, store.StageMerging)
	sqlExec(t, p, `UPDATE merge_queue SET state = 'rebasing' WHERE task_id = ?`, task.ID)

	mustContain(t, "agent", h.expectCode(ExitDoctor, "doctor", "--fix"), "a rebase in progress in "+task.Worktree, "needs a human at a terminal")
	h.env.Terminal = func() bool { return true }
	h.env.Stdin = strings.NewReader("n\n")
	mustContain(t, "declined", h.expectCode(ExitDoctor, "doctor", "--fix"), "left as it is")
	h.env.Stdin = strings.NewReader("y\n")
	out := h.mustRun("doctor", "--fix")
	mustContain(t, "aborted", out, "Abort it? [y/N]", "fixed "+task.Slug+": rebase aborted", task.Slug+": recovered")
	if got := h.task(task.Slug); got.Stage != store.StageDone || testutil.Git(t, task.Worktree, "rev-parse", "HEAD") != head {
		t.Fatalf("after abort: %+v", got)
	}
}
