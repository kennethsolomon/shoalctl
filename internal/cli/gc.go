package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/globx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// cmdGC cleans up after closed (merged or folded) tasks (§4.4): it closes
// their tmux windows (only ones tagged as the task's), removes their
// worktrees with git worktree remove, and drops their working rows, keeping
// the summary and every record (§7.4). A task whose worktree holds changes
// is left alone, window included, and reported (exit 5). It then flags
// stale every record whose files are all gone from the base branch (§11.1).
// It never deletes a branch, and never touches an open task.
func (env Env) cmdGC(ctx context.Context, args []string) error {
	if _, err := parseArgs(flag.NewFlagSet("gc", flag.ContinueOnError), args, 0); err != nil {
		return err
	}
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	tasks, err := p.db.ListTasks(ctx, "")
	if err != nil {
		return err
	}
	var closed []store.Task
	for _, t := range tasks {
		if t.Stage == store.StageMerged || t.Stage == store.StageFolded {
			closed = append(closed, t)
		}
	}
	sort.Slice(closed, func(i, j int) bool { return closed[i].ID < closed[j].ID })
	g := &collector{env: env, p: p, wins: env.tmuxWindows(ctx, closed)}
	for _, t := range closed {
		if err := g.task(ctx, t); err != nil {
			return err
		}
	}
	if err := g.staleRecords(ctx); err != nil {
		return err
	}
	if g.did == 0 && len(g.left) == 0 {
		fmt.Fprintln(env.Stdout, "nothing to collect")
	}
	if len(g.left) > 0 {
		return fail(ExitState, "gc left %d thing(s) for you: %s", len(g.left), strings.Join(g.left, "; "))
	}
	return nil
}

type collector struct {
	env  Env
	p    *project
	wins tmuxServers
	did  int      // actions taken
	left []string // what needs a human
}

func (g *collector) say(format string, a ...any) {
	g.did++
	fmt.Fprintf(g.env.Stdout, format+"\n", a...)
}

func (g *collector) task(ctx context.Context, t store.Task) error {
	lock, err := g.p.lockTask(t, "gc")
	if codeOf(err) == ExitBusy {
		g.left = append(g.left, fmt.Sprintf("%s is busy in another shoalctl (%v); run gc again later", t.Slug, err))
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Release()
	if g.dirty(ctx, t) {
		return nil
	}
	if err := g.window(ctx, t); err != nil {
		return err
	}
	if err := g.worktree(ctx, t); err != nil {
		return err
	}
	rows, err := g.p.db.DropWorkingRows(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		var parts []string
		for _, table := range []string{"messages", "message_reads", "file_claims", "hook_marks", "task_blockers", "merge_queue"} {
			if n := rows[table]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, table))
			}
		}
		g.say("rows     %s: dropped %s; its summary and records stay", t.Slug, strings.Join(parts, ", "))
	}
	return nil
}

// window closes a closed task's tmux window, or forgets one already gone.
func (g *collector) window(ctx context.Context, t store.Task) error {
	if t.TmuxWindow == "" {
		return nil
	}
	alive, known := g.wins.alive(t)
	if !known {
		g.left = append(g.left, fmt.Sprintf("the window %s of %s%s cannot be checked: %s", t.TmuxWindow, t.Slug, socketNote(t.TmuxSocket), g.wins.why(t)))
		return nil
	}
	e := g.env.taskTmux(t)
	ours := false
	if alive {
		// tmux reuses window ids after a restart: close the window only
		// when it carries this task's tag.
		var err error
		if ours, err = e.isTaskWindow(ctx, g.p, t); err != nil {
			g.left = append(g.left, fmt.Sprintf("the window %s of %s%s cannot be checked: %v", t.TmuxWindow, t.Slug, socketNote(t.TmuxSocket), err))
			return nil
		}
	}
	if ours {
		if _, err := e.tmux(ctx, "kill-window", "-t", t.TmuxWindow); err != nil {
			g.left = append(g.left, fmt.Sprintf("close the tmux window %s of %s yourself: %v", t.TmuxWindow, t.Slug, err))
			return nil
		}
	}
	if _, err := g.p.db.ClearWindow(ctx, t.ID, t.TmuxWindow); err != nil {
		return err
	}
	switch {
	case ours:
		g.say("window   %s: closed tmux window %s", t.Slug, t.TmuxWindow)
	case alive:
		g.say("window   %s: left tmux window %s open and forgot it: it is not tagged as this task's (tmux reuses window ids, or the task predates tags); if it is the task's, close it yourself", t.Slug, t.TmuxWindow)
	default:
		g.say("window   %s: forgot the gone tmux window %s", t.Slug, t.TmuxWindow)
	}
	return nil
}

// dirty reports whether a closed task's worktree holds changes or untracked
// files. Such a task is left alone, window and rows included, and reported:
// someone may still be working in it.
func (g *collector) dirty(ctx context.Context, t store.Task) bool {
	if t.Worktree == "" || !fileExists(t.Worktree) {
		return false
	}
	st, err := gitx.Run(ctx, t.Worktree, "status", "--porcelain")
	if err != nil {
		g.left = append(g.left, fmt.Sprintf("kept %s's worktree %s and its window: git status failed there: %v", t.Slug, t.Worktree, err))
		return true
	}
	if st == "" {
		return false
	}
	g.left = append(g.left, fmt.Sprintf("kept %s's worktree %s and its window: it holds changes or untracked files. Commit or remove what is in it, then run gc again", t.Slug, t.Worktree))
	return true
}

// worktree removes a closed task's worktree with git worktree remove, which
// refuses one holding changes or untracked files: that one is kept.
func (g *collector) worktree(ctx context.Context, t store.Task) error {
	if t.Worktree == "" || !fileExists(t.Worktree) {
		return nil
	}
	if rel, err := filepath.Rel(filepath.Join(g.p.dir, "worktrees"), t.Worktree); err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		g.left = append(g.left, fmt.Sprintf("%s's worktree %s is not under %s; shoal does not remove it", t.Slug, t.Worktree, filepath.Join(g.p.dir, "worktrees")))
		return nil
	}
	var removeErr error
	if err := g.env.withGitLock(g.p, t, func() error {
		_, removeErr = gitx.Run(ctx, g.p.root, "worktree", "remove", t.Worktree)
		return nil
	}); err != nil {
		return err
	}
	if removeErr != nil {
		g.left = append(g.left, fmt.Sprintf("kept %s's worktree %s: %v. Commit or remove what is in it, then run gc again", t.Slug, t.Worktree, removeErr))
		return nil
	}
	g.say("worktree %s: removed %s (branch %s kept)", t.Slug, t.Worktree, t.Branch)
	return nil
}

// staleRecords flags each record whose files are all gone from the base
// branch: a bug, decision or answered question none of whose files exists
// there, or a lesson whose glob matches no file there. Records without
// files, and ones already stale, are skipped.
func (g *collector) staleRecords(ctx context.Context) error {
	base := g.p.cfg.Merge.Base
	sha, err := gitx.Run(ctx, g.p.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+base+"^{commit}")
	if err != nil {
		g.left = append(g.left, fmt.Sprintf("records not checked for gone files: [merge].base %s is not a local branch", base))
		return nil
	}
	out, err := gitx.Run(ctx, g.p.root, "ls-tree", "-r", "-z", "--name-only", sha)
	if err != nil {
		return err
	}
	tree := map[string]bool{}
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			tree[f] = true
		}
	}
	gone := func(files []string) bool {
		for _, f := range files {
			if tree[f] {
				return false
			}
		}
		return len(files) > 0
	}
	type cand struct {
		kind  string
		id    int64
		files []string
	}
	var cands []cand
	bugs, err := g.p.db.Bugs(ctx)
	if err != nil {
		return err
	}
	for _, b := range bugs {
		if !b.Stale && gone(b.Files) {
			cands = append(cands, cand{store.KindBug, b.ID, b.Files})
		}
	}
	decisions, err := g.p.db.Decisions(ctx)
	if err != nil {
		return err
	}
	for _, d := range decisions {
		if !d.Stale && gone(d.Files) {
			cands = append(cands, cand{store.KindDecision, d.ID, d.Files})
		}
	}
	notes, err := g.p.db.QANotes(ctx)
	if err != nil {
		return err
	}
	for _, q := range notes {
		if !q.Stale && gone(q.Files) {
			cands = append(cands, cand{store.KindQA, q.ID, q.Files})
		}
	}
	lessons, err := g.p.db.Lessons(ctx)
	if err != nil {
		return err
	}
	for _, l := range lessons {
		if l.Stale || l.PathGlob == "" || matchesAny(l.PathGlob, tree) {
			continue
		}
		cands = append(cands, cand{store.KindLesson, l.ID, []string{l.PathGlob}})
	}
	for _, c := range cands {
		reason := fmt.Sprintf("gc: %s no longer exist on %s at %s", strings.Join(c.files, ", "), base, shortSHA(sha))
		changed, err := g.p.db.FlagStale(ctx, c.kind, c.id, reason)
		if err != nil {
			return err
		}
		if changed {
			g.say("stale    %s:%d: %s", c.kind, c.id, reason)
		}
	}
	return nil
}

func matchesAny(glob string, tree map[string]bool) bool {
	for f := range tree {
		if globx.Match(glob, f) {
			return true
		}
	}
	return false
}
