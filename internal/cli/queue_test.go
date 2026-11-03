package cli

import (
	"context"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func queueEntry(t *testing.T, h *harness, id int64) store.QueueEntry {
	t.Helper()
	q, err := h.db().Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range q {
		if e.TaskID == id {
			return e
		}
	}
	return store.QueueEntry{}
}

// A merge marks other open tasks whose claims cover a merged file
// needs-rebase; their next qa rebases them onto the new base first.
func TestMergeMarksOverlappingTasksNeedsRebase(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	a := startedTask(t, h, "feature", "Writes shared")
	b := startedTask(t, h, "feature", "Claims shared")
	c := startedTask(t, h, "feature", "Claims elsewhere")
	h.mustRun("claim", b.Slug, "--as", "shoal-backend", "shared/**")
	h.mustRun("claim", c.Slug, "--as", "shoal-backend", "other/**")
	commitIn(t, a.Worktree, "shared/x.txt", "from a\n")
	commitIn(t, b.Worktree, "b.txt", "b\n")
	finish(t, h, a.Slug, a.Worktree)
	if e := queueEntry(t, h, a.ID); e.State != store.QueueWaiting || e.Position != 1 {
		t.Fatalf("queued after done: %+v", e)
	}

	mustContain(t, "merge", h.mustRun("merge"), "merged "+a.Slug, b.Slug+": needs-rebase: its claims overlap shared/x.txt, which "+a.Slug+" changed")
	if e := queueEntry(t, h, a.ID); e.State != store.QueueMerged {
		t.Fatalf("queue after merge: %+v", e)
	}
	if got := h.task(b.Slug); got.Stage != store.StageNeedsRebase {
		t.Fatalf("b: %+v", got)
	}
	if got := h.task(c.Slug); got.Stage != store.StageInProgress {
		t.Fatalf("c was touched: %+v", got)
	}

	mainTip := testutil.Git(t, h.repo, "rev-parse", "main")
	mustContain(t, "qa", h.mustRun("qa", b.Slug), "qa: rebased "+b.Slug+" onto main@"+shortSHA(mainTip), "qa round 1 passed")
	got := h.task(b.Slug)
	if got.Stage != store.StageQA || got.BaseSHA != mainTip {
		t.Fatalf("b after qa: %+v", got)
	}
	testutil.Git(t, b.Worktree, "merge-base", "--is-ancestor", mainTip, "HEAD") // fails the test when b does not sit on main
}

func TestQARebaseConflictStaysNeedsRebase(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	a := startedTask(t, h, "feature", "First")
	b := startedTask(t, h, "feature", "Second")
	h.mustRun("claim", b.Slug, "--as", "shoal-backend", "shared.txt")
	commitIn(t, a.Worktree, "shared.txt", "from a\n")
	commitIn(t, b.Worktree, "shared.txt", "from b\n")
	finish(t, h, a.Slug, a.Worktree)
	h.mustRun("merge")
	head := testutil.Git(t, b.Worktree, "rev-parse", "HEAD")
	mustContain(t, "conflict", h.expectCode(ExitGate, "qa", b.Slug), "conflicts; rebase aborted", "stays needs-rebase")
	if got := h.task(b.Slug); got.Stage != store.StageNeedsRebase || got.QARound != 0 || testutil.Git(t, b.Worktree, "rev-parse", "HEAD") != head {
		t.Fatalf("b after conflict: %+v", got)
	}
	if st := testutil.Git(t, b.Worktree, "status", "--porcelain"); st != "" {
		t.Fatalf("aborted rebase left changes: %q", st)
	}
}

// A blocked merge marks the queue row failed; done again re-queues it last.
func TestMergeQueueFailedThenRequeued(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	a := startedTask(t, h, "feature", "Breaks a")
	b := startedTask(t, h, "feature", "Fine")
	commitIn(t, a.Worktree, "broken.txt", "x")
	commitIn(t, b.Worktree, "feature.txt", "x")
	finish(t, h, a.Slug, a.Worktree)
	h.expectCode(ExitGate, "merge")
	if e := queueEntry(t, h, a.ID); e.State != store.QueueFailed {
		t.Fatalf("blocked: %+v", e)
	}
	finish(t, h, b.Slug, b.Worktree)
	h.mustRun("done", "--force", a.Slug)
	if ea, eb := queueEntry(t, h, a.ID), queueEntry(t, h, b.ID); ea.State != store.QueueWaiting || ea.Position <= eb.Position {
		t.Fatalf("re-queued a %+v should follow b %+v", ea, eb)
	}
	code, out, _ := h.run("merge")
	if code != ExitGate {
		t.Fatalf("merge: %d %s", code, out)
	}
	mustContain(t, "order", out, "merged "+b.Slug)
}
