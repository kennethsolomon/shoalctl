package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// Phase 5 acceptance (spec §15, "Done when"): installed on a project with
// zero repo footprint. shoal is installed into a temp git repo in stealth
// mode, a task runs its whole life (start in tmux with a fake claude, done,
// merge, gc), and at every step git sees nothing of shoal: no tracked
// change, no untracked or ignored file. The only change to tracked files is
// the task's own merged commit. uninstall then leaves git's view of the repo
// exactly as it was before init.
func TestPhase5ZeroRepoFootprint(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	headBefore := testutil.Git(t, h.repo, "rev-parse", "HEAD")
	before := testutil.Git(t, h.repo, "status", "--porcelain=v1", "--ignored", "--untracked-files=all")
	clean := func(step string) {
		t.Helper()
		if st := testutil.Git(t, h.repo, "status", "--porcelain=v1", "--untracked-files=all"); st != "" {
			t.Fatalf("after %s, git status --porcelain shows shoal: %q", step, st)
		}
	}
	tracked := func(step, want string) {
		t.Helper()
		if got := testutil.Git(t, h.repo, "diff", "--name-only", headBefore, "HEAD"); got != want {
			t.Fatalf("after %s, tracked files changed: %q, want %q", step, got, want)
		}
	}

	h.initWith(junitConfig)
	clean("init")
	h.mustRun("agents", "install")
	clean("agents install")
	slug := strings.TrimSpace(h.mustRun("new", "feature", "Clients export"))
	h.mustRun("plan", slug, "feature.txt")
	h.mustRun("start", slug)
	task := h.task(slug)
	clean("start")
	h.mustRun("msg", "send", task.Slug, "--from", "shoal-backend", "--to", "shoal-lead", "--kind", "handoff", "feature.txt done")
	h.mustRun("record", "decision", task.Slug, "--topic", "export", "--choice", "stream", "--why", "memory", "--file", "feature.txt")
	commitIn(t, task.Worktree, "feature.txt", "x")
	clean("work in the task worktree")
	tracked("work in the task worktree", "")
	finish(t, h, task.Slug, task.Worktree)
	clean("done")
	tracked("done", "")
	mustContain(t, "merge", h.mustRun("merge"), "merged "+task.Slug+" into main")
	clean("merge")
	tracked("merge", "feature.txt")
	mustContain(t, "gc", h.mustRun("gc"), "removed "+task.Worktree)
	clean("gc")
	h.mustRun("status")
	// The harness's shoalctl path does not exist, so doctor reports the hook
	// entries (exit 11); it must still see the repo as shoal left it.
	h.expectCode(ExitDoctor, "doctor")
	clean("status and doctor")

	h.mustRun("uninstall")
	if got := testutil.Git(t, h.repo, "status", "--porcelain=v1", "--ignored", "--untracked-files=all"); got != before {
		t.Fatalf("after uninstall git sees:\n%q\nbefore init it saw:\n%q", got, before)
	}
	tracked("uninstall", "feature.txt")
	if fileExists(filepath.Join(h.repo, ".claude")) || fileExists(filepath.Join(h.repo, ".shoal")) || fileExists(filepath.Join(h.repo, ".shoal.toml")) {
		t.Fatal("uninstall left shoal files in the repo")
	}
	if wts := testutil.Git(t, h.repo, "worktree", "list", "--porcelain"); strings.Count(wts, "worktree ") != 1 {
		t.Fatalf("git still records task worktrees:\n%s", wts)
	}
}

// gc after a merge: closes the window, removes the worktree (kept while it
// holds an untracked file), drops the working rows but keeps the summary,
// and flags a record whose files are gone.
func TestPhase5GCAfterMerge(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Clients export")
	h.mustRun("msg", "send", task.Slug, "--from", "shoal-backend", "--to", "shoal-lead", "--kind", "handoff", "feature.txt done")
	h.mustRun("claim", task.Slug, "--as", "shoal-backend", "feature.txt")
	h.mustRun("record", "lesson", task.Slug, "--pattern", "legacy importer drops rows", "--fix", "batch them", "--glob", "legacy/**")
	h.mustRun("record", "decision", task.Slug, "--topic", "export", "--choice", "stream", "--why", "memory", "--file", "feature.txt")
	commitIn(t, task.Worktree, "feature.txt", "x")
	finish(t, h, task.Slug, task.Worktree)
	h.mustRun("merge")
	if out := h.tmuxOut("list-windows", "-a", "-F", "#{window_id}"); !strings.Contains(out, task.TmuxWindow) {
		t.Fatalf("the task's window %s is not open before gc: %q", task.TmuxWindow, out)
	}

	// A dirty worktree is checked first: gc then leaves the whole task alone,
	// window and working rows included, and says so (exit 5).
	testutil.WriteFile(t, filepath.Join(task.Worktree, "notes.txt"), "mine")
	errOut := h.expectCode(ExitState, "gc")
	mustContain(t, "gc with an untracked file", errOut, "kept "+task.Slug+"'s worktree "+task.Worktree, "run gc again")
	if !fileExists(filepath.Join(task.Worktree, "notes.txt")) {
		t.Fatal("gc deleted an untracked file")
	}
	if out := h.tmuxOut("list-windows", "-a", "-F", "#{window_id}"); !strings.Contains(out, task.TmuxWindow) {
		t.Fatalf("gc closed window %s of a task whose worktree it kept: %q", task.TmuxWindow, out)
	}
	if h.task(task.Slug).TmuxWindow != task.TmuxWindow {
		t.Fatal("gc forgot the window of a task whose worktree it kept")
	}
	db := h.db()
	ctx := context.Background()
	if msgs, _ := db.Inbox(ctx, task.ID, "shoal-lead", true); len(msgs) != 1 {
		t.Fatalf("gc dropped the rows of a task whose worktree it kept: %+v", msgs)
	}

	os.Remove(filepath.Join(task.Worktree, "notes.txt"))
	out := h.mustRun("gc")
	mustContain(t, "gc", out, "window   "+task.Slug+": closed tmux window "+task.TmuxWindow,
		"worktree "+task.Slug+": removed "+task.Worktree+" (branch "+task.Branch+" kept)")
	if wins := h.tmuxOut("list-windows", "-a", "-F", "#{window_id}"); strings.Contains(wins, task.TmuxWindow) {
		t.Fatalf("window %s still open after gc: %q", task.TmuxWindow, wins)
	}
	merged := h.task(task.Slug)
	if merged.TmuxWindow != "" {
		t.Fatalf("window not forgotten: %+v", merged)
	}
	if msgs, _ := db.Inbox(ctx, merged.ID, "shoal-lead", true); len(msgs) != 0 {
		t.Fatalf("messages kept: %+v", msgs)
	}
	if cl, _ := db.Claims(ctx, merged.ID); len(cl) != 0 {
		t.Fatalf("claims kept: %+v", cl)
	}
	if sm, err := db.GetSummary(ctx, merged.ID); err != nil || sm.Decision == "" {
		t.Fatalf("summary gone: %+v %v", sm, err)
	}
	lessons, _ := db.Lessons(ctx)
	decisions, _ := db.Decisions(ctx)
	if len(lessons) != 1 || !lessons[0].Stale || !strings.Contains(lessons[0].StaleReason, "legacy/** no longer exist on main") {
		t.Fatalf("lesson on a gone glob not flagged: %+v", lessons)
	}
	if len(decisions) != 1 || decisions[0].Stale {
		t.Fatalf("decision on a live file flagged: %+v", decisions)
	}

	if fileExists(task.Worktree) {
		t.Fatal("worktree still on disk")
	}
	if testutil.Git(t, h.repo, "rev-parse", "--verify", "refs/heads/"+task.Branch) == "" {
		t.Fatal("gc deleted the task branch")
	}
	mustContain(t, "third gc", h.mustRun("gc"), "nothing to collect")
	code, out, _ := h.run("doctor") // exit 11: the harness's shoalctl path does not exist
	if code != ExitDoctor || !strings.Contains(out, "ok   task worktrees present") || !strings.Contains(out, "ok   git worktree list matches the disk") {
		t.Fatalf("doctor after gc: %d\n%s", code, out)
	}
}

// gc leaves open tasks alone.
func TestPhase5GCSkipsOpenTasks(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Still open")
	h.mustRun("msg", "send", task.Slug, "--from", "shoal-backend", "--to", "shoal-lead", "--kind", "handoff", "wip")
	mustContain(t, "gc", h.mustRun("gc"), "nothing to collect")
	if !fileExists(task.Worktree) || h.task(task.Slug).TmuxWindow == "" {
		t.Fatal("gc touched an open task")
	}
	if msgs, _ := h.db().Inbox(context.Background(), task.ID, "shoal-lead", true); len(msgs) != 1 {
		t.Fatalf("gc dropped an open task's messages: %+v", msgs)
	}
}

// mergedTask runs a task through start, done and merge, its window still open.
func mergedTask(t *testing.T, h *harness, title, file string) store.Task {
	t.Helper()
	task := startedTask(t, h, "feature", title)
	commitIn(t, task.Worktree, file, "x")
	finish(t, h, task.Slug, task.Worktree)
	h.mustRun("merge")
	return task
}

// tmux reuses window ids (they restart at @0 with the server), so the id gc
// recorded can name someone else's window by now. gc closes a window only
// when it carries the task's @shoal_task tag; any other window under that id
// is left open, and the record is forgotten.
func TestPhase5GCLeavesAForeignWindowOpen(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := mergedTask(t, h, "Clients export", "feature.txt")
	h.tmuxOut("set-option", "-w", "-t", task.TmuxWindow, "@shoal_task", "someone else's window")

	out := h.mustRun("gc")
	mustContain(t, "gc", out, "window   "+task.Slug+": left tmux window "+task.TmuxWindow+" open")
	if wins := h.tmuxOut("list-windows", "-a", "-F", "#{window_id}"); !strings.Contains(wins, task.TmuxWindow) {
		t.Fatalf("gc closed a window that is not the task's: %q", wins)
	}
	if h.task(task.Slug).TmuxWindow != "" {
		t.Fatal("gc did not forget the recorded window")
	}
	if fileExists(task.Worktree) {
		t.Fatal("worktree not removed")
	}
}

// A task started before windows were tagged has no tag: gc cannot tell its
// window from a foreign one, so it closes nothing and says so.
func TestPhase5GCLeavesAnUntaggedWindowOpen(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := mergedTask(t, h, "Clients export", "feature.txt")
	h.tmuxOut("set-option", "-w", "-u", "-t", task.TmuxWindow, "@shoal_task")

	out := h.mustRun("gc")
	mustContain(t, "gc", out, "window   "+task.Slug+": left tmux window "+task.TmuxWindow+" open", "close it yourself")
	if wins := h.tmuxOut("list-windows", "-a", "-F", "#{window_id}"); !strings.Contains(wins, task.TmuxWindow) {
		t.Fatalf("gc closed an untagged window: %q", wins)
	}
	if h.task(task.Slug).TmuxWindow != "" {
		t.Fatal("gc did not forget the recorded window")
	}
}

// start tags the window it opens with the task, which gc checks.
func TestStartTagsItsWindow(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Tagged")
	if got, want := h.tmuxOut("show-options", "-w", "-v", "-t", task.TmuxWindow, "@shoal_task"), windowTag(p.id, task.Slug); got != want {
		t.Fatalf("window tag %q, want %q", got, want)
	}
}
