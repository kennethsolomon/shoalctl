package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/lockfile"
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
	h.mustRun("merge")
}
