package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func TestMergeOldestFirstRebaseAndConflict(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	a := startedTask(t, h, "feature", "First")
	b := startedTask(t, h, "feature", "Second")
	c := startedTask(t, h, "feature", "Third")
	commitIn(t, a.Worktree, "shared.txt", "from a\n")
	commitIn(t, b.Worktree, "b.txt", "b\n")
	commitIn(t, c.Worktree, "shared.txt", "from c\n")
	finish(t, h, a.Slug, a.Worktree)
	finish(t, h, b.Slug, b.Worktree)
	finish(t, h, c.Slug, c.Worktree)
	cHead := testutil.Git(t, c.Worktree, "rev-parse", "HEAD")

	// Held merge.lock: exit 8.
	l, err := lockfile.Acquire(filepath.Join(p.dir, "locks", "merge.lock"), 0, lockfile.Info{PID: 1, Command: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	if errOut := h.expectCode(ExitBusy, "merge"); !strings.Contains(errOut, "merge already running") {
		t.Fatalf("busy: %s", errOut)
	}
	l.Release()

	code, out, errOut := h.run("merge")
	if code != ExitGate || !strings.Contains(out, "merged "+a.Slug) || !strings.Contains(out, "merged "+b.Slug) || !strings.Contains(errOut, "conflicts") {
		t.Fatalf("merge: %d\n%s\n%s", code, out, errOut)
	}
	if got := h.task(b.Slug); got.Stage != "merged" || testutil.Git(t, h.repo, "rev-parse", "main") != got.MergedSHA {
		t.Fatalf("b: %+v", got)
	}
	// b was rebased onto a, so main holds both.
	if _, err := os.Stat(filepath.Join(h.repo, "shared.txt")); err != nil {
		t.Fatal(err)
	}
	got := h.task(c.Slug)
	if got.Stage != "needs-rebase" || testutil.Git(t, c.Worktree, "rev-parse", "HEAD") != cHead {
		t.Fatalf("c after conflict: %+v", got)
	}
	if st := testutil.Git(t, c.Worktree, "status", "--porcelain"); st != "" {
		t.Fatalf("aborted rebase left changes: %q", st)
	}
}

func TestMergeRefusesDirtyMainCheckout(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Change")
	commitIn(t, task.Worktree, "feature.txt", "x")
	finish(t, h, task.Slug, task.Worktree)
	testutil.WriteFile(t, filepath.Join(h.repo, "README.md"), "edited\n")
	if errOut := h.expectCode(ExitState, "merge"); !strings.Contains(errOut, "uncommitted changes") {
		t.Fatalf("dirty main: %s", errOut)
	}
	if got := h.task(task.Slug); got.Stage != "done" {
		t.Fatalf("task after refused merge: %+v", got)
	}
	testutil.Git(t, h.repo, "checkout", "--", "README.md")
	h.env.Terminal = func() bool { return true }
	// An acceptance that blocks nothing is a usage error; the clean merge stands.
	errOut := h.expectCode(ExitUsage, "merge", "--task", task.Slug, "--accept-regression", "C::a", "--reason", "not needed")
	mustContain(t, "unused acceptance", errOut, "blocked nothing")
	if got := h.task(task.Slug); got.Stage != "merged" {
		t.Fatalf("task after clean merge: %+v", got)
	}
}

// An acceptance covers exactly one task: --accept-regression needs --task,
// and the named task is the only one merged in that run.
func TestMergeAcceptanceIsScopedToOneTask(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	a := startedTask(t, h, "feature", "First breaks a")
	b := startedTask(t, h, "feature", "Second breaks a")
	commitIn(t, a.Worktree, "broken.txt", "x")
	commitIn(t, b.Worktree, "broken.txt", "x")
	finish(t, h, a.Slug, a.Worktree)
	finish(t, h, b.Slug, b.Worktree)
	mainBefore := testutil.Git(t, h.repo, "rev-parse", "main")

	errOut := h.expectCode(ExitUsage, "merge", "--accept-regression", "C::a", "--reason", "meant for one task")
	mustContain(t, "unscoped acceptance", errOut, "--task")
	if testutil.Git(t, h.repo, "rev-parse", "main") != mainBefore || h.task(a.Slug).Stage != "done" || h.task(b.Slug).Stage != "done" {
		t.Fatal("an unscoped acceptance merged something")
	}

	h.env.Terminal = func() bool { return true }
	out := h.mustRun("merge", "--task", b.Slug, "--accept-regression", "C::a", "--reason", "b changes C::a on purpose")
	mustContain(t, "scoped merge", out, "merged "+b.Slug)
	if strings.Contains(out, a.Slug) {
		t.Fatalf("a scoped merge touched %s:\n%s", a.Slug, out)
	}
	if got := h.task(a.Slug); got.Stage != "done" {
		t.Fatalf("a after b's scoped merge: %+v", got)
	}
	if n := countRows(t, p, `SELECT count(*) FROM regression_overrides WHERE task_id = ?`, a.ID); n != 0 {
		t.Fatalf("b's acceptance was recorded for a: %d", n)
	}
	mustContain(t, "merged task named again", h.expectCode(ExitState, "merge", "--task", b.Slug), "merged")
	h.expectCode(ExitNotFound, "merge", "--task", "99-nope")
}

func setStage(t *testing.T, p *project, id int64, stage string) {
	t.Helper()
	db := openRaw(t, p.dbPath())
	defer db.Close()
	if _, err := db.Exec(`UPDATE tasks SET stage = ? WHERE id = ?`, stage, id); err != nil {
		t.Fatal(err)
	}
}

// A crash between the fast-forward and the DB update leaves a task at
// merging. The next merge settles it under merge.lock: merged when the target
// already holds the task head, back to done (and merged in this run) when it
// does not, and left alone with exit 5 when the worktree is mid-rebase.
func TestMergeSettlesTasksLeftAtMerging(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	landed := startedTask(t, h, "feature", "Landed before the crash")
	pending := startedTask(t, h, "feature", "Crashed before the fast-forward")
	commitIn(t, landed.Worktree, "feature.txt", "x")
	commitIn(t, pending.Worktree, "pending.txt", "x")
	finish(t, h, landed.Slug, landed.Worktree)
	finish(t, h, pending.Slug, pending.Worktree)

	// landed: main was fast-forwarded to its head, then the process died.
	head := testutil.Git(t, landed.Worktree, "rev-parse", "HEAD")
	testutil.Git(t, h.repo, "merge", "-q", "--ff-only", head)
	setStage(t, p, landed.ID, "merging")
	setStage(t, p, pending.ID, "merging")

	out := h.mustRun("merge")
	mustContain(t, "merge", out, landed.Slug+": recovered", "merged "+pending.Slug)
	got := h.task(landed.Slug)
	if got.Stage != "merged" || got.MergedSHA != head {
		t.Fatalf("landed after recovery: %+v (head %s)", got, head)
	}
	mustContain(t, "recovered report", reportOf(t, h, landed.Slug), "status: merged", "Recovered by shoalctl merge", shortSHA(head))
	if got := h.task(pending.Slug); got.Stage != "merged" {
		t.Fatalf("pending after recovery: %+v", got)
	}

	// Mid-rebase worktree: ambiguous, left at merging, exit 5.
	stuck := startedTask(t, h, "feature", "Crashed mid-rebase")
	commitIn(t, stuck.Worktree, "stuck.txt", "x")
	finish(t, h, stuck.Slug, stuck.Worktree)
	setStage(t, p, stuck.ID, "merging")
	if err := os.MkdirAll(testutil.Git(t, stuck.Worktree, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "stuck", h.expectCode(ExitState, "merge"), stuck.Slug, "rebase")
	if got := h.task(stuck.Slug); got.Stage != "merging" {
		t.Fatalf("stuck after merge: %+v", got)
	}
}

// A rebase that fails because ctx was cancelled (Ctrl-C) is an interrupt,
// not a conflict: any rebase in progress is aborted, the branch is back where
// it was, and the error carries the interrupted exit code.
func TestRebaseCancelledIsInterruptNotConflict(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Interrupted rebase")
	commitIn(t, task.Worktree, "shared.txt", "from task\n")
	commitIn(t, h.repo, "shared.txt", "from main\n")
	target := testutil.Git(t, h.repo, "rev-parse", "main")
	head := testutil.Git(t, task.Worktree, "rev-parse", "HEAD")
	// The interrupt arrives while git is mid-rebase, stopped on a conflict.
	if out, err := exec.Command("git", "-C", task.Worktree, "rebase", target).CombinedOutput(); err == nil {
		t.Fatalf("set-up rebase did not stop:\n%s", out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := h.env.rebase(ctx, h.project(), task, target)
	if errors.Is(err, errConflict) || codeOf(err) != ExitInterrupted {
		t.Fatalf("cancelled rebase: code %d, err %v", codeOf(err), err)
	}
	if got := testutil.Git(t, task.Worktree, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD after interrupted rebase = %s, want %s", got, head)
	}
	if st := testutil.Git(t, task.Worktree, "status", "--porcelain"); st != "" {
		t.Fatalf("interrupted rebase left changes: %q", st)
	}
}

// Accepting a regression is a human decision (G9, §8.5): an agent or any
// caller without a terminal is refused before anything is merged.
func TestMergeAcceptanceNeedsHuman(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Breaks a")
	commitIn(t, task.Worktree, "broken.txt", "x")
	finish(t, h, task.Slug, task.Worktree)
	mainBefore := testutil.Git(t, h.repo, "rev-parse", "main")

	h.env.Terminal = func() bool { return false }
	errOut := h.expectCode(ExitState, "merge", "--task", task.Slug, "--accept-regression", "C::a", "--reason", "agent says so")
	mustContain(t, "agent acceptance", errOut, "needs a human at a terminal")
	if testutil.Git(t, h.repo, "rev-parse", "main") != mainBefore || h.task(task.Slug).Stage != "done" {
		t.Fatal("a refused acceptance merged or moved the task")
	}
	if n := countRows(t, p, `SELECT count(*) FROM regression_overrides`); n != 0 {
		t.Fatalf("a refused acceptance recorded %d override(s)", n)
	}

	h.env.Terminal = func() bool { return true }
	mustContain(t, "human acceptance", h.mustRun("merge", "--task", task.Slug, "--accept-regression", "C::a", "--reason", "on purpose"), "merged "+task.Slug)
}

// envScript makes C::a need the ignored file .env, as a test that reads
// local secrets or a vendored dependency would.
func envScript() string {
	return strings.Replace(testScript, "if [ -f broken.txt ]; then", "if [ -f broken.txt ] || [ ! -f .env ]; then", 1)
}

// Vex's probe: C::a needs an ignored file, so a clean baseline worktree
// fails it. It passed in an earlier run of the task, the task then broke it,
// and the gate must call that a regression (exit 9), not pre-existing.
func TestGateBlocksTestThatPassedEarlierInTheTask(t *testing.T) {
	h := newHarness(t).withSession()
	testutil.WriteFile(t, filepath.Join(h.repo, "test.sh"), envScript())
	testutil.WriteFile(t, filepath.Join(h.repo, ".gitignore"), ".env\n")
	testutil.Git(t, h.repo, "add", "test.sh", ".gitignore")
	testutil.Git(t, h.repo, "commit", "-q", "-m", "C::a needs .env")
	testutil.WriteFile(t, filepath.Join(h.repo, ".env"), "SECRET=x\n")
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Breaks a behind .env")

	testutil.WriteFile(t, filepath.Join(task.Worktree, ".env"), "SECRET=x\n")
	mustContain(t, "qa with .env", h.mustRun("test", "--kind", "qa", task.Slug), "fixed: C::a")
	commitIn(t, task.Worktree, "broken.txt", "x")
	finish(t, h, task.Slug, task.Worktree)
	mainBefore := testutil.Git(t, h.repo, "rev-parse", "main")

	code, out, errOut := h.run("merge")
	if code != ExitGate || !strings.Contains(out, "regression: C::a") || !strings.Contains(out, "passed in run") {
		t.Fatalf("merge: exit %d\n%s\n%s", code, out, errOut)
	}
	if testutil.Git(t, h.repo, "rev-parse", "main") != mainBefore || h.task(task.Slug).Stage != "qa" {
		t.Fatal("the hidden regression was merged")
	}
}

// [tests].setup prepares every clean worktree a baseline runs in (start's
// fresh worktree, merge's scratch worktree); a failing setup records no
// baseline and fails closed.
func TestSetupPreparesCleanWorktrees(t *testing.T) {
	h := newHarness(t).withSession()
	testutil.WriteFile(t, filepath.Join(h.repo, "test.sh"), envScript())
	testutil.WriteFile(t, filepath.Join(h.repo, ".gitignore"), ".env\n")
	testutil.Git(t, h.repo, "add", "test.sh", ".gitignore")
	testutil.Git(t, h.repo, "commit", "-q", "-m", "C::a needs .env")
	testutil.WriteFile(t, filepath.Join(h.repo, ".env"), "SECRET=x\n")
	p := h.initWith(junitNoSetup + "setup = \"cp {root}/.env .\"\n")
	task := startedTask(t, h, "feature", "Setup copies .env")
	if n := countRows(t, p, `SELECT count(*) FROM test_results WHERE run_id = ? AND test_id = 'C::a' AND status = 'pass'`, h.task(task.Slug).BaselineRunID); n != 1 {
		t.Fatal("start's baseline ran without [tests].setup: C::a did not pass")
	}

	// A failing setup at merge's new base: no baseline, nothing merged.
	commitIn(t, task.Worktree, "feature.txt", "x")
	finish(t, h, task.Slug, task.Worktree)
	commitIn(t, h.repo, "other.txt", "moves main")
	mainBefore := testutil.Git(t, h.repo, "rev-parse", "main")
	testutil.WriteFile(t, filepath.Join(p.dir, "config.toml"), junitNoSetup+"setup = \"exit 3\"\n")
	mustContain(t, "failed setup", h.expectCode(ExitBaseline, "merge"), "[tests].setup failed")
	if testutil.Git(t, h.repo, "rev-parse", "main") != mainBefore || h.task(task.Slug).Stage != "done" {
		t.Fatal("a failed setup merged or moved the task")
	}

	testutil.WriteFile(t, filepath.Join(p.dir, "config.toml"), junitNoSetup+"setup = \"cp {root}/.env .\"\n")
	mustContain(t, "merge with setup", h.mustRun("merge"), "merged "+task.Slug)
	newBase := countRows(t, p, `SELECT max(id) FROM test_runs WHERE kind = 'baseline'`)
	if n := countRows(t, p, `SELECT count(*) FROM test_results WHERE run_id = ? AND test_id = 'C::a' AND status = 'pass'`, newBase); n != 1 {
		t.Fatal("merge's scratch baseline ran without [tests].setup: C::a did not pass")
	}
}

// envBreakInFirstCommit is Vex's round-3 probe: no [tests].setup, C::a needs
// the ignored .env (in the main checkout and, copied after start, in the task
// worktree), and the task breaks C::a in its first commit with no suite run
// before merge. Every baseline ran in a fresh worktree without .env.
func envBreakInFirstCommit(t *testing.T) (*harness, store.Task) {
	t.Helper()
	h := newHarness(t).withSession()
	testutil.WriteFile(t, filepath.Join(h.repo, "test.sh"), envScript())
	testutil.WriteFile(t, filepath.Join(h.repo, ".gitignore"), ".env\n")
	testutil.Git(t, h.repo, "add", "test.sh", ".gitignore")
	testutil.Git(t, h.repo, "commit", "-q", "-m", "C::a needs .env")
	testutil.WriteFile(t, filepath.Join(h.repo, ".env"), "SECRET=x\n")
	h.initWith(junitNoSetup)
	task := startedTask(t, h, "feature", "Breaks a in its first commit")
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".env"), "SECRET=x\n")
	commitIn(t, task.Worktree, "broken.txt", "x")
	finish(t, h, task.Slug, task.Worktree)
	return h, task
}

// Without [tests].setup a clean-baseline failure is no proof the test was
// failing before the task, so the gate blocks on it (exit 9).
func TestGateBlocksUnprovenBaselineFailureWithoutSetup(t *testing.T) {
	h, task := envBreakInFirstCommit(t)
	mainBefore := testutil.Git(t, h.repo, "rev-parse", "main")
	code, out, errOut := h.run("merge")
	if code != ExitGate {
		t.Fatalf("merge: exit %d\n%s\n%s", code, out, errOut)
	}
	mustContain(t, "gate output", out, "BLOCKED by: C::a", "[tests].setup")
	mustContain(t, "gate error", errOut, "--accept-regression C::a", "--task "+task.Slug)
	if testutil.Git(t, h.repo, "rev-parse", "main") != mainBefore || h.task(task.Slug).Stage != "qa" {
		t.Fatal("an unproven baseline failure was merged")
	}
}

// The human path still works: accepting each unproven failure by name for
// this task merges it; an agent cannot.
func TestHumanAcceptsUnprovenBaselineFailure(t *testing.T) {
	h, task := envBreakInFirstCommit(t)
	accept := []string{"merge", "--task", task.Slug,
		"--accept-regression", "C::a", "--reason", "checked by hand",
		"--accept-regression", "C::pre", "--reason", "old bug"}
	h.env.Terminal = func() bool { return false }
	mustContain(t, "agent acceptance", h.expectCode(ExitState, accept...), "needs a human at a terminal")
	h.env.Terminal = func() bool { return true }
	mustContain(t, "human acceptance", h.mustRun(accept...), "merged "+task.Slug)
	if got := h.task(task.Slug); got.Stage != "merged" || testutil.Git(t, h.repo, "rev-parse", "main") != got.MergedSHA {
		t.Fatalf("after acceptance: %+v", got)
	}
}
