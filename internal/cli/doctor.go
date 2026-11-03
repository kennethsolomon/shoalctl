package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
)

// cmdDoctor checks shoal's state and, with --fix, repairs what is safe to
// repair (§10.4): hook entries, the database, the merge queue, tasks a crash
// left at a transient stage, abandoned test runs, worktrees, tmux windows
// and stale claims. It exits 11 while any problem remains.
func (env Env) cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fix := fs.Bool("fix", false, "repair what is safe to repair")
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}
	p, err := env.identify(ctx)
	if err != nil {
		return err
	}
	d := &doctor{env: env, fix: *fix, in: bufio.NewReader(env.Stdin)}
	if err := d.hooks(ctx, p); err != nil {
		return err
	}
	if _, ok, err := readMeta(p.metaPath()); err != nil {
		return err
	} else if ok {
		proj, err := env.openProject(ctx)
		if err != nil {
			return err
		}
		defer proj.close()
		if err := d.project(ctx, proj); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(env.Stdout, "skip project checks: %s is not initialized\n", p.root)
	}
	if d.problems == 0 {
		return nil
	}
	if !d.fix {
		return fail(ExitDoctor, "%d problem(s) found; run shoalctl doctor --fix to repair what is safe to repair (hooks fail open, so a broken hook entry only costs heartbeats; hook entries are pointed at %s)",
			d.problems, env.Exe)
	}
	return fail(ExitDoctor, "%d problem(s) remain after --fix; each FAIL line above says what a human has to decide", d.problems)
}

// doctor collects the outcome of every check.
type doctor struct {
	env      Env
	fix      bool
	in       *bufio.Reader
	problems int
}

func (d *doctor) ok(format string, a ...any) { fmt.Fprintf(d.env.Stdout, "ok   "+format+"\n", a...) }

func (d *doctor) fixed(format string, a ...any) {
	fmt.Fprintf(d.env.Stdout, "fixed "+format+"\n", a...)
}

func (d *doctor) problem(format string, a ...any) {
	d.problems++
	fmt.Fprintf(d.env.Stderr, "FAIL "+format+"\n", a...)
}

// hooks is the Phase 0 check: shoal's entries in the main checkout.
func (d *doctor) hooks(ctx context.Context, p *project) error {
	path := filepath.Join(p.root, ".claude", "settings.local.json")
	problems, binaries, err := hookProblems(path)
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		d.ok("hooks in %s run %v", path, binaries)
		return nil
	}
	if !d.fix {
		for _, pr := range problems {
			d.problem("%s", pr)
		}
		return nil
	}
	if err := requireAbsoluteExe(d.env.Exe); err != nil {
		return err
	}
	if err := hookPreflight(ctx, p.root); err != nil {
		return err
	}
	if _, _, err := installHooks(p.root, d.env.Exe, filepath.Join(p.dir, "backups", "settings.local.json.bak")); err != nil {
		return err
	}
	left, _, err := hookProblems(path)
	if err != nil {
		return err
	}
	for _, pr := range left {
		d.problem("%s (after --fix)", pr)
	}
	if len(left) == 0 {
		d.fixed("hooks in %s now run %s", path, d.env.Exe)
	}
	return nil
}

func (d *doctor) project(ctx context.Context, p *project) error {
	for _, check := range []func(context.Context, *project) error{
		d.database, d.mergeQueue, d.starting, d.runs, d.worktrees, d.gitWorktrees, d.windows, d.worktreeHooks, d.staleClaims,
	} {
		if err := check(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (d *doctor) database(ctx context.Context, p *project) error {
	probs, err := p.db.IntegrityProblems(ctx)
	if err != nil {
		return err
	}
	more, err := p.db.MigrationProblems(ctx)
	if err != nil {
		return err
	}
	for _, pr := range append(probs, more...) {
		d.problem("memory.db: %s (no automatic fix; restore %s from a memory.db.bak-* copy)", pr, p.dbPath())
	}
	if len(probs)+len(more) == 0 {
		d.ok("memory.db integrity, foreign keys and migration checksums")
	}
	return nil
}

// mergeQueue checks queue rows and tasks an interrupted merge left behind.
// It holds merge.lock throughout, so it never races a running merge; the
// task-level recovery is merge's own settleMerging (§16 row 38).
func (d *doctor) mergeQueue(ctx context.Context, p *project) error {
	lock, err := lockfile.Acquire(filepath.Join(p.dir, "locks", "merge.lock"), 0,
		lockfile.Info{PID: os.Getpid(), Command: "doctor", StartedAt: nowUTC()})
	if errors.Is(err, lockfile.ErrHeld) {
		d.ok("merge queue: skipped, a merge is running (%v)", err)
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Release()
	queue, err := p.db.Queue(ctx)
	if err != nil {
		return err
	}
	clean := true
	for _, e := range queue {
		if e.State != store.QueueRebasing && e.State != store.QueueTesting {
			continue
		}
		t, err := p.db.GetTask(ctx, fmt.Sprint(e.TaskID))
		if err != nil {
			return err
		}
		clean = false
		if e.State == store.QueueRebasing && t.Worktree != "" && fileExists(t.Worktree) {
			rebasing, err := rebaseInProgress(ctx, t.Worktree)
			if err != nil {
				return err
			}
			if rebasing {
				if err := d.abortRebase(ctx, p, t); err != nil {
					return err
				}
				continue
			}
		}
		state := store.QueueWaiting
		switch t.Stage {
		case store.StageMerged:
			state = store.QueueMerged
		case store.StageDone, store.StageMerging:
		default:
			state = store.QueueFailed
		}
		if !d.fix {
			d.problem("merge queue: %s is at %s, but no merge is running", t.Slug, e.State)
			continue
		}
		if err := p.db.SetQueue(ctx, t.ID, state); err != nil {
			return err
		}
		d.fixed("merge queue: %s from %s back to %s", t.Slug, e.State, state)
	}
	merging, err := p.db.TasksAt(ctx, store.StageMerging)
	if err != nil {
		return err
	}
	if len(merging) > 0 {
		clean = false
		if !d.fix {
			for _, t := range merging {
				d.problem("%s is at merging, but no merge is running (an interrupted merge); --fix settles it like shoalctl merge does", t.Slug)
			}
		} else {
			stuck, err := d.env.settleMerging(ctx, p)
			if err != nil {
				return err
			}
			for _, slug := range stuck {
				d.problem("%s is still at merging (see above)", slug)
			}
		}
	}
	if clean {
		d.ok("merge queue: no merge left half done")
	}
	return nil
}

// abortRebase undoes the rebase an interrupted merge left in a worktree,
// after asking: it names the branch, the worktree and what is lost (§10.5).
func (d *doctor) abortRebase(ctx context.Context, p *project, t store.Task) error {
	what := fmt.Sprintf("%s: an interrupted merge left a rebase in progress in %s (branch %s)", t.Slug, t.Worktree, t.Branch)
	if !d.fix {
		d.problem("%s; --fix asks, then runs git rebase --abort there", what)
		return nil
	}
	if !d.env.interactive() {
		d.problem("%s; aborting it needs a human at a terminal: run shoalctl doctor --fix yourself", what)
		return nil
	}
	fmt.Fprintf(d.env.Stdout, "%s.\ngit rebase --abort returns %s to its commit before the rebase and drops any conflict resolution made since. Abort it? [y/N]: ", what, t.Branch)
	line, err := d.in.ReadString('\n')
	if strings.TrimSpace(strings.ToLower(line)) != "y" {
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read the answer: %w", err)
		}
		d.problem("%s; left as it is", what)
		return nil
	}
	if err := d.env.withGitLock(p, t, func() error {
		_, err := gitx.Run(ctx, t.Worktree, "rebase", "--abort")
		return err
	}); err != nil {
		return err
	}
	if err := p.db.SetQueue(ctx, t.ID, store.QueueWaiting); err != nil {
		return err
	}
	d.fixed("%s: rebase aborted, merge queue row back to waiting", t.Slug)
	return nil
}

// starting rolls back tasks a killed start left at starting (§10.4): no
// live start holds the task lock, so whatever ran has died.
func (d *doctor) starting(ctx context.Context, p *project) error {
	tasks, err := p.db.TasksAt(ctx, store.StageStarting)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		lock, err := p.lockTask(t, "doctor")
		if err != nil {
			d.ok("%s is starting in another shoalctl (%v)", t.Slug, err)
			continue
		}
		err = d.rollBackStart(ctx, p, t)
		lock.Release()
		if err != nil {
			return err
		}
	}
	if len(tasks) == 0 {
		d.ok("no task stuck at starting")
	}
	return nil
}

func (d *doctor) rollBackStart(ctx context.Context, p *project, t store.Task) error {
	t, err := p.db.GetTask(ctx, t.Slug) // re-read under the task lock
	if err != nil || t.Stage != store.StageStarting {
		return err
	}
	prev := t.PrevStage
	if prev == "" || prev == store.StageStarting {
		prev = store.StageNew
	}
	if !d.fix {
		d.problem("%s is at starting, but no start is running (killed mid-start); --fix rolls it back to %s", t.Slug, prev)
		return nil
	}
	if err := d.env.reapAbandoned(ctx, p, t); err != nil {
		if codeOf(err) == ExitBusy {
			d.problem("%s is at starting: %v", t.Slug, err)
			return nil
		}
		return err
	}
	if _, err := p.db.Update(ctx, t, []string{store.StageStarting}, "stage = ?, prev_stage = NULL", []any{prev}, nil); err != nil {
		return err
	}
	d.fixed("%s rolled back from starting to %s; its worktree is kept, run shoalctl start %s to resume", t.Slug, prev, t.Slug)
	return nil
}

// runs clears test runs left at running by a shoalctl that died (§8,
// §10.4), with the same pid checks start and done use.
func (d *doctor) runs(ctx context.Context, p *project) error {
	runs, err := p.db.RunningRunsAll(ctx)
	if err != nil {
		return err
	}
	done := map[int64]bool{}
	for _, r := range runs {
		if r.TaskID == 0 {
			d.problem("run %d is marked running and belongs to no task; check its output %s", r.ID, r.OutputPath)
			continue
		}
		if done[r.TaskID] {
			continue
		}
		done[r.TaskID] = true
		t, err := p.db.GetTask(ctx, fmt.Sprint(r.TaskID))
		if err != nil {
			return err
		}
		lock, err := p.lockTask(t, "doctor")
		if err != nil {
			d.ok("%s run %d of %s is running in a live shoalctl", r.Kind, r.ID, t.Slug)
			continue
		}
		if !d.fix {
			d.problem("%s run %d of %s is marked running, but no shoalctl holds %s; --fix marks it error once its suite is gone", r.Kind, r.ID, t.Slug, t.Slug)
		} else if err := d.clearUnstarted(ctx, p, t, runs); err != nil {
			lock.Release()
			return err
		} else if err := d.env.reapAbandoned(ctx, p, t); err != nil {
			if codeOf(err) != ExitBusy {
				lock.Release()
				return err
			}
			d.problem("%v", err)
		} else {
			d.fixed("abandoned run(s) of %s marked error", t.Slug)
		}
		lock.Release()
	}
	if len(runs) == 0 {
		d.ok("no test run stuck at running")
	}
	return nil
}

// clearUnstarted marks error the task's running runs that never recorded a
// pid. The caller holds the task lock, so their shoalctl is gone, and a suite
// runs nothing until its pid is recorded (§15.1 "Stopping a suite"), so none
// of them ever ran. start and done refuse them (exit 8); doctor --fix is the
// documented way out.
func (d *doctor) clearUnstarted(ctx context.Context, p *project, t store.Task, runs []store.Run) error {
	for _, r := range runs {
		if r.TaskID != t.ID || r.PID > 0 {
			continue
		}
		if f, err := os.OpenFile(r.OutputPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
			fmt.Fprintf(f, "shoal: doctor --fix at %s: marked error; its shoalctl exited before recording a pid, so the suite never ran\n", nowUTC())
			f.Close()
		}
		if _, err := p.db.FinishRun(ctx, r.ID, testrun.StatusError, nil, nil); err != nil {
			return err
		}
		d.fixed("%s run %d of %s never recorded a pid (its suite never ran); marked error", r.Kind, r.ID, t.Slug)
	}
	return nil
}

// worktreeStages are the stages in which a task's recorded worktree must
// exist on disk.
var worktreeStages = []string{store.StageBaselineFailed, store.StageInProgress, store.StageQA, store.StageEscalated,
	store.StageNeedsRebase, store.StageDone}

func (d *doctor) worktrees(ctx context.Context, p *project) error {
	tasks, err := p.db.ListTasks(ctx, "")
	if err != nil {
		return err
	}
	known := map[string]bool{}
	clean := true
	for _, t := range tasks {
		if t.Worktree != "" {
			known[filepath.Clean(t.Worktree)] = true
		}
		switch {
		case t.Stage == store.StageWorktreeMissing && fileExists(t.Worktree):
			clean = false
			if !d.fix {
				d.problem("%s is worktree-missing, but %s is back; --fix restores it to %s", t.Slug, t.Worktree, t.PrevStage)
				continue
			}
			if _, err := p.db.Update(ctx, t, []string{store.StageWorktreeMissing}, "stage = COALESCE(prev_stage, ?), prev_stage = NULL",
				[]any{store.StageInProgress}, nil); err != nil {
				return err
			}
			d.fixed("%s: worktree %s is back; restored to %s", t.Slug, t.Worktree, t.PrevStage)
		case t.Stage == store.StageWorktreeMissing:
			clean = false
			d.problem("%s is worktree-missing: %s is gone. A human decides: recreate it (git worktree add %s %s) and run doctor --fix, or abandon the task",
				t.Slug, t.Worktree, t.Worktree, t.Branch)
		case t.Worktree != "" && contains(worktreeStages, t.Stage) && !fileExists(t.Worktree):
			clean = false
			if !d.fix {
				d.problem("%s (%s): its worktree %s is missing; --fix marks the task worktree-missing", t.Slug, t.Stage, t.Worktree)
				continue
			}
			if _, err := p.db.Update(ctx, t, worktreeStages, "prev_stage = stage, stage = ?", []any{store.StageWorktreeMissing}, nil); err != nil {
				return err
			}
			d.problem("%s: its worktree %s is missing; marked worktree-missing (was %s). Recreate it (git worktree add %s %s) and run doctor --fix, or abandon the task",
				t.Slug, t.Worktree, t.Stage, t.Worktree, t.Branch)
		}
	}
	entries, err := os.ReadDir(filepath.Join(p.dir, "worktrees"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("list %s: %w", filepath.Join(p.dir, "worktrees"), err)
	}
	for _, e := range entries {
		path := filepath.Join(p.dir, "worktrees", e.Name())
		if !known[filepath.Clean(path)] {
			clean = false
			d.problem("%s has no task; shoal never deletes it: look inside, then remove it yourself if it holds nothing you need", path)
		}
	}
	if clean {
		d.ok("task worktrees present")
	}
	return nil
}

// gitWorktrees prunes git's records of shoal worktrees whose directory is
// gone. A gone worktree outside shoal's directory is only reported, since
// git worktree prune would drop it too.
func (d *doctor) gitWorktrees(ctx context.Context, p *project) error {
	out, err := gitx.Run(ctx, p.root, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	var ours, foreign []string
	for _, line := range strings.Split(out, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok || fileExists(path) {
			continue
		}
		if rel, err := filepath.Rel(p.dir, path); err == nil && !strings.HasPrefix(rel, "..") {
			ours = append(ours, path)
		} else {
			foreign = append(foreign, path)
		}
	}
	for _, f := range foreign {
		d.problem("git lists worktree %s, which is gone and not shoal's; run git worktree prune yourself if it is not on a removable drive", f)
	}
	switch {
	case len(ours) == 0 && len(foreign) == 0:
		d.ok("git worktree list matches the disk")
	case len(ours) == 0:
	case !d.fix:
		d.problem("git still lists %d gone shoal worktree(s): %s; --fix runs git worktree prune", len(ours), strings.Join(ours, ", "))
	case len(foreign) > 0:
		d.problem("git still lists gone shoal worktree(s) %s; not pruned, because git worktree prune would also drop %s", strings.Join(ours, ", "), strings.Join(foreign, ", "))
	default:
		if err := d.env.withGitLock(p, store.Task{Slug: "doctor"}, func() error {
			_, err := gitx.Run(ctx, p.root, "worktree", "prune")
			return err
		}); err != nil {
			return err
		}
		d.fixed("git worktree prune: dropped %s", strings.Join(ours, ", "))
	}
	return nil
}

func (d *doctor) windows(ctx context.Context, p *project) error {
	tasks, err := p.db.ListTasks(ctx, "")
	if err != nil {
		return err
	}
	wins := d.env.tmuxWindows(ctx, tasks)
	clean := true
	for _, t := range tasks {
		if t.TmuxWindow == "" || t.Stage == store.StageMerged || t.Stage == store.StageFolded {
			continue
		}
		alive, known := wins.alive(t)
		if !known {
			d.problem("tmux did not answer%s, so the window %s of %s cannot be checked", socketNote(t.TmuxSocket), t.TmuxWindow, t.Slug)
			clean = false
			continue
		}
		if alive {
			continue
		}
		clean = false
		if !d.fix {
			d.problem("%s: tmux window %s is gone; --fix forgets it", t.Slug, t.TmuxWindow)
			continue
		}
		if _, err := p.db.ClearWindow(ctx, t.ID, t.TmuxWindow); err != nil {
			return err
		}
		d.fixed("%s: forgot the gone tmux window %s", t.Slug, t.TmuxWindow)
	}
	if clean {
		d.ok("tmux windows of open tasks")
	}
	return nil
}

// worktreeHooks removes shoal entries from a task worktree's own settings:
// hooks live once in the main checkout, and a second copy fires twice (§4.6).
func (d *doctor) worktreeHooks(ctx context.Context, p *project) error {
	tasks, err := p.db.ListTasks(ctx, "")
	if err != nil {
		return err
	}
	clean := true
	for _, t := range tasks {
		if t.Worktree == "" {
			continue
		}
		path := filepath.Join(t.Worktree, ".claude", "settings.local.json")
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		has, err := hasShoalHooks(data)
		if err != nil {
			clean = false
			d.problem("%s is not valid settings JSON (%v); fix it by hand", path, err)
			continue
		}
		if !has {
			continue
		}
		clean = false
		if tracked, _ := gitx.Run(ctx, t.Worktree, "ls-files", "--", ".claude/settings.local.json"); tracked != "" {
			d.problem("%s registers shoal hooks a second time, and git tracks it; remove the shoal entries by hand", path)
			continue
		}
		if !d.fix {
			d.problem("%s registers shoal hooks a second time, so they fire twice; --fix removes those entries", path)
			continue
		}
		events, err := decodeHooks(data)
		if err != nil {
			return fail(ExitConfig, "%s: %v; fix it by hand", path, err)
		}
		for ev := range events {
			data = removeShoalEntries(data, ev)
		}
		if err := writeFileAtomic(path, data, 0o600); err != nil {
			return err
		}
		d.fixed("removed shoal's hook entries from %s", path)
	}
	if clean {
		d.ok("no hook entries in task worktrees")
	}
	return nil
}

func socketNote(socket string) string {
	if socket == "" {
		return ""
	}
	return " on socket " + socket
}

func (d *doctor) staleClaims(ctx context.Context, p *project) error {
	stale, err := d.env.staleCheck(ctx, p)
	if err != nil {
		return err
	}
	open, err := p.db.OpenClaims(ctx)
	if err != nil {
		return err
	}
	clean := true
	for _, c := range open {
		if !stale(c.Owner, c.Claim) {
			continue
		}
		what := fmt.Sprintf("%s: claim %s (%s) is stale: window gone, last seen %s", c.Owner.Slug, c.Glob, c.Agent, ago(c.Owner.LastSeenAt))
		if !d.fix {
			clean = false
			d.problem("%s; --fix releases it", what)
			continue
		}
		released, err := p.db.ReleaseStaleClaim(ctx, c.Claim, stale)
		if err != nil {
			return err
		}
		if released {
			d.fixed("%s; released", what)
		}
	}
	if clean {
		d.ok("no stale claims")
	}
	return nil
}

// hookProblems lists what is wrong with shoal's entries in a settings file:
// an event with no entry or several, a binary that is missing or not
// executable, or an entry that is not in the current shape.
func hookProblems(path string) (problems, binaries []string, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{fmt.Sprintf("%s does not exist: SessionStart and Stop hooks are not registered", path)}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	events, err := decodeHooks(data)
	if err != nil {
		return nil, nil, fail(ExitConfig, "%s: %v; fix it by hand, shoal will not overwrite it", path, err)
	}
	for _, h := range hookEvents {
		var cmds []string
		for _, g := range events[h.event] {
			entries, _ := groupEntries(g)
			for _, e := range entries {
				if c := entryCommand(e); isShoalCommand(c) {
					cmds = append(cmds, c)
				}
			}
		}
		switch len(cmds) {
		case 0:
			problems = append(problems, fmt.Sprintf("%s: shoal hook not registered", h.event))
			continue
		case 1:
		default:
			problems = append(problems, fmt.Sprintf("%s: %d shoal entries, so the hook would run %d times", h.event, len(cmds), len(cmds)))
			continue
		}
		prog, _ := commandProgram(cmds[0])
		binaries = append(binaries, prog)
		if msg := binaryProblem(prog); msg != "" {
			problems = append(problems, fmt.Sprintf("%s: %s", h.event, msg))
			continue
		}
		if !exactlyOne(events[h.event], entryFor(runtime.GOOS, prog, h.arg)) {
			problems = append(problems, fmt.Sprintf("%s: entry is outdated; want %q", h.event, hookCommandFor(runtime.GOOS, prog, h.arg)))
		}
	}
	return problems, binaries, nil
}

func binaryProblem(prog string) string {
	if !filepath.IsAbs(prog) {
		return fmt.Sprintf("binary path %q is not absolute", prog)
	}
	info, err := os.Stat(prog)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("binary %s is missing (moved or deleted)", prog)
	case err != nil:
		return fmt.Sprintf("binary %s: %v", prog, err)
	case !info.Mode().IsRegular():
		return fmt.Sprintf("binary %s is not a regular file", prog)
	case runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0:
		return fmt.Sprintf("binary %s is not executable", prog)
	}
	return ""
}

// requireAbsoluteExe refuses to register a binary path that would depend on
// the hook's working directory or PATH.
func requireAbsoluteExe(exe string) error {
	if !filepath.IsAbs(exe) {
		return fail(ExitInternal, "shoalctl binary path %q is not absolute; hooks must name the binary by absolute path", exe)
	}
	return nil
}
