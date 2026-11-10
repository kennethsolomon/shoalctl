package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

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

	testutil.WriteFile(t, filepath.Join(task.Worktree, "notes.txt"), "mine")
	errOut := h.expectCode(ExitState, "gc")
	mustContain(t, "gc with an untracked file", errOut, "kept "+task.Slug+"'s worktree "+task.Worktree, "run gc again")
	if !fileExists(filepath.Join(task.Worktree, "notes.txt")) {
		t.Fatal("gc deleted an untracked file")
	}
	if out := h.tmuxOut("list-windows", "-a", "-F", "#{window_id}"); strings.Contains(out, task.TmuxWindow) {
		t.Fatalf("window %s still open after gc: %q", task.TmuxWindow, out)
	}
	merged := h.task(task.Slug)
	if merged.TmuxWindow != "" {
		t.Fatalf("window not forgotten: %+v", merged)
	}
	db := h.db()
	ctx := context.Background()
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

	os.Remove(filepath.Join(task.Worktree, "notes.txt"))
	out := h.mustRun("gc")
	mustContain(t, "gc", out, "worktree "+task.Slug+": removed "+task.Worktree+" (branch "+task.Branch+" kept)")
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
