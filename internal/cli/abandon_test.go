package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// abandonAs runs abandon at a terminal with answer as the y/N reply.
func (h *harness) abandonAs(answer string, want int, args ...string) (string, string) {
	h.t.Helper()
	h.env.Terminal = func() bool { return true }
	h.env.Stdin = strings.NewReader(answer)
	defer func() { h.env.Terminal, h.env.Stdin = nil, nil }()
	code, out, errOut := h.run(append([]string{"abandon"}, args...)...)
	if code != want {
		h.t.Fatalf("abandon %v exited %d, want %d\nstdout: %s\nstderr: %s", args, code, want, out, errOut)
	}
	return out, errOut
}

func openClaimCount(t *testing.T, h *harness, id int64) int {
	t.Helper()
	return countRows(t, h.project(), `SELECT count(*) FROM file_claims WHERE task_id = ? AND released_at IS NULL`, id)
}

// A task that never started is only marked abandoned: who, when and why
// are kept, its plan claims are released, and status hides it unless --all.
func TestAbandonNeverStartedTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initWith(junitConfig)
	slug := newTask(t, h, "bugfix", "sample task")
	h.mustRun("plan", slug, "src/**")
	task := h.task(slug)

	out, _ := h.abandonAs("y\n", ExitOK, slug, "--reason", "a test task")
	mustContain(t, "abandon", out, `abandon task 1 `+slug+` "sample task" (bugfix, ready):`, "Abandon it? [y/N]: ", "abandoned "+slug)
	got := h.task(slug)
	if got.Stage != store.StageAbandoned || got.AbandonedBy == "" || got.AbandonedAt == "" || got.AbandonReason != "a test task" || got.ClosedAt == "" {
		t.Fatalf("abandoned task: %+v", got)
	}
	if n := countRows(t, h.project(), `SELECT count(*) FROM file_claims WHERE task_id = ?`, task.ID); n != 0 {
		t.Fatalf("%d claim row(s) left", n)
	}

	if out := h.mustRun("status"); strings.Contains(out, slug) {
		t.Fatalf("status shows an abandoned task:\n%s", out)
	}
	mustContain(t, "status --all", h.mustRun("status", "--all"), slug, "abandoned", "abandoned by "+got.AbandonedBy+": a test task")
	var rows []statusRow
	if err := json.Unmarshal([]byte(h.mustRun("status", "--json")), &rows); err != nil || len(rows) != 0 {
		t.Fatalf("status --json: %+v %v", rows, err)
	}
	if err := json.Unmarshal([]byte(h.mustRun("status", "--json", "--all")), &rows); err != nil || len(rows) != 1 || rows[0].Stage != store.StageAbandoned {
		t.Fatalf("status --json --all: %+v %v", rows, err)
	}

	_, errOut := h.abandonAs("y\n", ExitState, slug)
	mustContain(t, "again", errOut, "already abandoned by "+got.AbandonedBy)
	mustContain(t, "start", h.expectCode(ExitState, "start", slug), slug)
	mustContain(t, "gc", h.mustRun("gc"), "nothing to collect")
}

// A task serialized behind the abandoned one no longer waits: with no
// blocker left it becomes ready and can start.
func TestAbandonReleasesWaitingTasks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initWith(junitConfig)
	a, b := newTask(t, h, "bugfix", "Bug A"), newTask(t, h, "bugfix", "Bug B")
	h.mustRun("plan", a, "resources/js/users/**")
	h.env.Terminal = func() bool { return true }
	h.env.Stdin = strings.NewReader("s\n")
	h.mustRun("plan", b, "resources/js/users/list.js")
	if got := h.task(b); got.Stage != store.StageBlocked || got.BlockedBy != h.task(a).ID {
		t.Fatalf("b not serialized behind a: %+v", got)
	}

	out, _ := h.abandonAs("y\n", ExitOK, a)
	mustContain(t, "abandon", out, "ready    "+b+": no longer waits on "+a)
	got := h.task(b)
	if got.Stage != store.StageReady || got.BlockedBy != 0 {
		t.Fatalf("b after a was abandoned: %+v", got)
	}
	if n := countRows(t, h.project(), `SELECT count(*) FROM task_blockers`); n != 0 {
		t.Fatalf("%d task_blockers row(s) left", n)
	}
	// b's claim no longer overlaps anything: planning it again asks nothing.
	h.env.Stdin = strings.NewReader("")
	mustContain(t, "replan", h.mustRun("plan", b, "resources/js/users/list.js"), "ready")
}

// Nothing changes unless a human at a terminal says y.
func TestAbandonRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initWith(junitConfig)
	slug := newTask(t, h, "bugfix", "sample task")
	h.mustRun("plan", slug, "src/**")
	task := h.task(slug)
	unchanged := func(what string) {
		t.Helper()
		if got := h.task(slug); got.Stage != store.StageReady || got.Version != task.Version || openClaimCount(t, h, task.ID) != 1 {
			t.Fatalf("%s changed the task: %+v", what, got)
		}
	}

	h.env.Terminal = func() bool { return false }
	mustContain(t, "no terminal", h.expectCode(ExitState, "abandon", slug), "needs a human at a terminal")
	unchanged("no terminal")
	h.env.Terminal = func() bool { return true }
	h.setenv("CLAUDECODE", "1")
	mustContain(t, "agent", h.expectCode(ExitState, "abandon", slug), "inside a Claude Code session")
	unchanged("an agent")
	h.setenv("CLAUDECODE", "")

	for _, answer := range []string{"", "n\n", "yes please\n"} {
		_, errOut := h.abandonAs(answer, ExitState, slug)
		mustContain(t, "declined", errOut, "task "+slug+" not abandoned; nothing changed")
		unchanged("a declined prompt")
	}
	h.abandonAs("y\n", ExitNotFound, "no-such-task")
	h.abandonAs("y\n", ExitUsage, slug, "extra")
}

// A started task loses its window and worktree like a merged one does at
// gc; its branch stays, and gc and doctor find nothing of it left.
func TestAbandonStartedTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Clients export")
	h.mustRun("claim", task.Slug, "--as", "shoal-backend", "feature.txt")
	h.mustRun("msg", "send", task.Slug, "--from", "shoal-backend", "--to", "shoal-lead", "--kind", "handoff", "wip")
	h.mustRun("record", "decision", task.Slug, "--topic", "export", "--choice", "stream", "--why", "memory", "--file", "test.sh")
	commitIn(t, task.Worktree, "feature.txt", "x")
	if task.Stage != store.StageInProgress {
		t.Fatalf("started task at %s", task.Stage)
	}

	out, _ := h.abandonAs("y\n", ExitOK, task.Slug)
	mustContain(t, "abandon", out, "its tmux window "+task.TmuxWindow+" closes", "its worktree "+task.Worktree+" is removed",
		"branch "+task.Branch+", the summary and every record stay",
		"window   "+task.Slug+": closed tmux window "+task.TmuxWindow,
		"worktree "+task.Slug+": removed "+task.Worktree)
	if strings.Contains(out, "UNCOMMITTED") {
		t.Errorf("a clean worktree was announced as losing changes:\n%s", out)
	}
	got := h.task(task.Slug)
	if got.Stage != store.StageAbandoned || got.TmuxWindow != "" || got.AbandonReason != "" {
		t.Fatalf("abandoned task: %+v", got)
	}
	if fileExists(task.Worktree) {
		t.Fatal("worktree still on disk")
	}
	if wins := h.tmuxOut("list-windows", "-a", "-F", "#{window_id}"); strings.Contains(wins, task.TmuxWindow) {
		t.Fatalf("window %s still open: %q", task.TmuxWindow, wins)
	}
	if testutil.Git(t, h.repo, "rev-parse", "--verify", "refs/heads/"+task.Branch) == "" {
		t.Fatal("abandon deleted the task branch")
	}
	db := h.db()
	ctx := context.Background()
	if cl, _ := db.Claims(ctx, task.ID); len(cl) != 0 {
		t.Fatalf("claims kept: %+v", cl)
	}
	if ds, _ := db.Decisions(ctx); len(ds) != 1 {
		t.Fatalf("records lost: %+v", ds)
	}
	mustContain(t, "gc", h.mustRun("gc"), "nothing to collect")
	code, dout, _ := h.run("doctor") // exit 11: the harness's shoalctl path does not exist
	if code != ExitDoctor || !strings.Contains(dout, "ok   task worktrees present") || !strings.Contains(dout, "ok   git worktree list matches the disk") ||
		!strings.Contains(dout, "ok   tmux windows of open tasks") {
		t.Fatalf("doctor after abandon: %d\n%s", code, dout)
	}
}

// abandon waits for the task's attention watcher to let go of its lock.
func TestAbandonWaitsForTheWatcher(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Watched")
	l, err := lockfile.Acquire(watchLockPath(p, task.ID), 0, lockfile.Info{PID: 1, Command: "watch", Task: task.Slug})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		l.Release()
	}()
	h.abandonAs("y\n", ExitOK, task.Slug)
	if got := h.task(task.Slug); got.Stage != store.StageAbandoned {
		t.Fatalf("task at %s", got.Stage)
	}
}

// A worktree holding changes is refused (exit 5) before anything changes,
// as gc keeps one; --force removes it, changes and all.
func TestAbandonDirtyWorktree(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Half done")
	h.mustRun("claim", task.Slug, "--as", "shoal-backend", "feature.txt")
	testutil.WriteFile(t, filepath.Join(task.Worktree, "notes.txt"), "mine")

	_, errOut := h.abandonAs("y\n", ExitState, task.Slug)
	mustContain(t, "dirty", errOut, task.Worktree+" holds changes or untracked files; nothing changed", "--force")
	got := h.task(task.Slug)
	if got.Stage != store.StageInProgress || got.Version != task.Version || got.TmuxWindow != task.TmuxWindow || openClaimCount(t, h, task.ID) != 1 {
		t.Fatalf("a refused abandon changed the task: %+v", got)
	}
	if !fileExists(filepath.Join(task.Worktree, "notes.txt")) {
		t.Fatal("a refused abandon deleted an untracked file")
	}
	if wins := h.tmuxOut("list-windows", "-a", "-F", "#{window_id}"); !strings.Contains(wins, task.TmuxWindow) {
		t.Fatalf("a refused abandon closed window %s: %q", task.TmuxWindow, wins)
	}

	// Declining after --force's warning changes nothing either.
	out, _ := h.abandonAs("n\n", ExitState, task.Slug, "--force")
	mustContain(t, "forced prompt", out, "is removed, WITH THE UNCOMMITTED CHANGES AND UNTRACKED FILES IN IT")
	if !fileExists(filepath.Join(task.Worktree, "notes.txt")) || h.task(task.Slug).Stage != store.StageInProgress {
		t.Fatal("a declined forced abandon changed something")
	}

	h.abandonAs("y\n", ExitOK, task.Slug, "--force", "--reason", "wrong approach")
	if fileExists(task.Worktree) {
		t.Fatal("--force kept the worktree")
	}
	if got := h.task(task.Slug); got.Stage != store.StageAbandoned || got.AbandonReason != "wrong approach" {
		t.Fatalf("forced abandon: %+v", got)
	}
}

// A task in QA and a done task waiting in the merge queue can be
// abandoned; the done one leaves the queue and merge never sees it.
func TestAbandonQAAndQueuedTasks(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig + "flaky_retries = 0\n")
	inQA := startedTask(t, h, "feature", "In QA")
	commitIn(t, inQA.Worktree, "broken.txt", "x")
	h.expectCode(ExitGate, "qa", inQA.Slug)
	if got := h.task(inQA.Slug); got.Stage != store.StageQA {
		t.Fatalf("task not at qa: %+v", got)
	}
	queued := startedTask(t, h, "feature", "Queued")
	commitIn(t, queued.Worktree, "feature.txt", "x")
	finish(t, h, queued.Slug, queued.Worktree)
	if n := countRows(t, h.project(), `SELECT count(*) FROM merge_queue WHERE task_id = ?`, queued.ID); n != 1 {
		t.Fatalf("done task not queued (%d rows)", n)
	}

	for _, task := range []store.Task{inQA, queued} {
		h.abandonAs("y\n", ExitOK, task.Slug)
		if got := h.task(task.Slug); got.Stage != store.StageAbandoned || fileExists(task.Worktree) {
			t.Fatalf("%s after abandon: %+v", task.Slug, got)
		}
	}
	if n := countRows(t, h.project(), `SELECT count(*) FROM merge_queue`); n != 0 {
		t.Fatalf("%d merge_queue row(s) left", n)
	}
	if out := h.mustRun("merge"); strings.Contains(out, queued.Slug) {
		t.Fatalf("merge saw the abandoned task:\n%s", out)
	}
	if got := testutil.Git(t, h.repo, "log", "--oneline", "main"); strings.Contains(got, "feature.txt") {
		t.Fatalf("an abandoned task merged:\n%s", got)
	}
}

// A merged task cannot be abandoned.
func TestAbandonMergedRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := mergedTask(t, h, "Clients export", "feature.txt")
	_, errOut := h.abandonAs("y\n", ExitState, task.Slug)
	mustContain(t, "merged", errOut, "task "+task.Slug+" is merged", "cannot be abandoned")
	if got := h.task(task.Slug); got.Stage != store.StageMerged || got.AbandonedBy != "" {
		t.Fatalf("merged task changed: %+v", got)
	}
}
