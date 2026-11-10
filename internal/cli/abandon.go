package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// cmdAbandon gives up on a task before it merged (§16 row 84). Only a human
// at a terminal may, after a y/N prompt naming the task. Every check runs
// before anything changes, so a refusal (a merged task, a worktree holding
// changes without --force, a declined prompt) leaves the task, its claims
// and its queue place as they were. Then one transaction marks the task
// abandoned (who, when, why), releases its claims, drops it from the merge
// queue and releases the tasks serialized behind it. Last, the cleanup gc
// does for a closed task runs: its tagged tmux window closes, its worktree
// is removed (--force: changes and all) and its working rows go; its
// attention watcher stops by itself once the task leaves the watched stages
// and its window is gone, and abandon waits for it. The prompt comes before
// the task lock; the checks are redone under it, and a task that changed
// meanwhile is refused with nothing changed. What that cleanup
// cannot do is reported (exit 5) and left to gc. The branch, the summary
// and every record stay.
func (env Env) cmdAbandon(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("abandon", flag.ContinueOnError)
	reason := fs.String("reason", "", "why the task is given up; kept with it")
	force := fs.Bool("force", false, "remove the worktree even when it holds changes or untracked files")
	pos, err := parseArgs(fs, args, 1)
	if err != nil {
		return err
	}
	if err := env.requireHuman("abandon"); err != nil {
		return err
	}
	p, t, err := env.openTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	dirty, err := abandonChecks(ctx, t, *force)
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "abandon task %d %s %q (%s, %s):\n", t.ID, t.Slug, t.Title, t.Lane, t.Stage)
	fmt.Fprintln(env.Stdout, "  its claims are released, it leaves the merge queue, tasks waiting on it are released")
	if t.TmuxWindow != "" {
		fmt.Fprintf(env.Stdout, "  its tmux window %s closes\n", t.TmuxWindow)
	}
	if t.Worktree != "" && fileExists(t.Worktree) {
		lost := ""
		if dirty {
			lost = ", WITH THE UNCOMMITTED CHANGES AND UNTRACKED FILES IN IT"
		}
		fmt.Fprintf(env.Stdout, "  its worktree %s is removed%s\n", t.Worktree, lost)
	}
	if t.Branch != "" {
		fmt.Fprintf(env.Stdout, "  branch %s, ", t.Branch)
	} else {
		fmt.Fprint(env.Stdout, "  ")
	}
	fmt.Fprintln(env.Stdout, "the summary and every record stay")
	// The prompt runs without the task lock, so a merge or another command
	// is never held up while the human decides; the lock is taken after the
	// answer and every check is redone under it.
	ok, err := env.confirm("Abandon it? [y/N]: ")
	if err != nil {
		return err
	}
	if !ok {
		return fail(ExitState, "task %s not abandoned; nothing changed", t.Slug)
	}
	lock, err := p.lockTask(t, "abandon")
	if err != nil {
		return err
	}
	defer lock.Release()
	asked := t
	if t, err = p.db.GetTask(ctx, asked.Slug); err != nil {
		return err
	}
	if t.Stage != asked.Stage || t.Version != asked.Version {
		return fail(ExitState, "task %s changed while you were deciding (now %s); nothing changed. Run shoalctl abandon %s again", t.Slug, t.Stage, t.Slug)
	}
	nowDirty, err := abandonChecks(ctx, t, true)
	if err != nil {
		return err
	}
	if nowDirty && !dirty {
		return fail(ExitState, "task %s's worktree %s took changes while you were deciding; nothing changed. Run shoalctl abandon %s again", t.Slug, t.Worktree, t.Slug)
	}

	watched := t.TmuxWindow != ""
	t, ready, err := p.db.AbandonTask(ctx, t, humanName(ctx, p.root), strings.TrimSpace(*reason))
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "abandoned %s (by %s)\n", t.Slug, t.AbandonedBy)
	for _, id := range ready {
		w, err := p.db.GetTask(ctx, fmt.Sprint(id))
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "ready    %s: no longer waits on %s; start it with shoalctl start %s\n", w.Slug, t.Slug, w.Slug)
	}
	g := &collector{env: env, p: p, wins: env.tmuxWindows(ctx, []store.Task{t}), force: *force}
	if err := g.collect(ctx, t); err != nil {
		return err
	}
	if watched {
		if err := g.watcherStopped(t); err != nil {
			return err
		}
	}
	if len(g.left) > 0 {
		return fail(ExitState, "%s is abandoned, but %d thing(s) are left for you: %s", t.Slug, len(g.left), strings.Join(g.left, "; "))
	}
	return nil
}

// watcherStopped waits for the task's attention watcher to exit: it stops
// on its next tick once the task has left the watched stages, and releases
// its lock.
func (g *collector) watcherStopped(t store.Task) error {
	l, err := lockfile.Acquire(watchLockPath(g.p, t.ID), 3*watchInterval,
		lockfile.Info{PID: os.Getpid(), Command: "abandon", Task: t.Slug, StartedAt: nowUTC()})
	if errors.Is(err, lockfile.ErrHeld) {
		g.left = append(g.left, fmt.Sprintf("the attention watcher of %s still runs (%v); it stops by itself once it sees the task abandoned", t.Slug, err))
		return nil
	}
	if err != nil {
		return err
	}
	return l.Release()
}

// abandonChecks refuses a task abandon cannot take (§16 row 84) and reports
// whether its worktree holds changes; a dirty one is refused without force.
func abandonChecks(ctx context.Context, t store.Task, force bool) (bool, error) {
	switch {
	case t.Stage == store.StageAbandoned:
		return false, fail(ExitState, "task %s was already abandoned by %s at %s", t.Slug, t.AbandonedBy, t.AbandonedAt)
	case t.Stage == store.StageMerged:
		return false, fail(ExitState, "task %s is merged (%s); a merged task cannot be abandoned: revert its commit instead", t.Slug, shortSHA(t.MergedSHA))
	case !contains(store.Abandonable, t.Stage):
		return false, fail(ExitState, "task %s is at %s; abandon accepts a task at %s", t.Slug, t.Stage, strings.Join(store.Abandonable, ", "))
	}
	dirty, err := dirtyWorktree(ctx, t)
	if err != nil {
		return false, fmt.Errorf("check the worktree %s of task %s: %w", t.Worktree, t.Slug, err)
	}
	if dirty && !force {
		return false, fail(ExitState, "task %s's worktree %s holds changes or untracked files; nothing changed. "+
			"Commit or remove them, or pass --force to delete them with the worktree", t.Slug, t.Worktree)
	}
	return dirty, nil
}
