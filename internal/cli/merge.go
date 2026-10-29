package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
)

type acceptance struct {
	target, reason string
	used           bool
}

// cmdMerge merges done tasks one at a time, oldest first, each through the
// merge gate (§8.5, §9.4). It never pushes.
func (env Env) cmdMerge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	var targets, reasons multiFlag
	only := fs.String("task", "", "merge only this done task; required with --accept-regression, so an acceptance covers exactly one task")
	fs.Var(&targets, "accept-regression", "a blocking target to accept: test id, coverage:<path>, (suite), (lint) or (build); repeatable")
	fs.Var(&reasons, "reason", "why; one --reason per --accept-regression, in the same order")
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}
	if len(targets) != len(reasons) {
		return fail(ExitUsage, "every --accept-regression needs its own --reason (got %d targets, %d reasons)", len(targets), len(reasons))
	}
	if len(targets) > 0 && *only == "" {
		return fail(ExitUsage, "--accept-regression covers one task: name it with --task <task> (shoalctl merge --task <task> --accept-regression <target> --reason \"<why>\")")
	}
	acc := make([]*acceptance, len(targets))
	for i := range targets {
		if strings.TrimSpace(targets[i]) == "" || strings.TrimSpace(reasons[i]) == "" {
			return fail(ExitUsage, "--accept-regression and --reason must not be empty")
		}
		acc[i] = &acceptance{target: targets[i], reason: strings.TrimSpace(reasons[i])}
	}
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	if p.cfg.Merge.Strategy != "rebase" {
		return fail(ExitConfig, "[merge].strategy %q is not built yet; only rebase merges", p.cfg.Merge.Strategy)
	}
	lock, err := lockfile.Acquire(filepath.Join(p.dir, "locks", "merge.lock"), 0,
		lockfile.Info{PID: os.Getpid(), Command: "merge", StartedAt: nowUTC()})
	if errors.Is(err, lockfile.ErrHeld) {
		return fail(ExitBusy, "merge already running: %v", err)
	}
	if err != nil {
		return err
	}
	defer lock.Release()

	stuck, err := env.settleMerging(ctx, p)
	if err != nil {
		return err
	}
	tasks, err := p.db.DoneTasks(ctx)
	if err != nil {
		return err
	}
	if *only != "" {
		t, err := p.db.GetTask(ctx, *only)
		if err != nil {
			return err
		}
		if t.Stage != store.StageDone {
			return fail(ExitState, "task %s is %s; merge --task needs a done task", t.Slug, t.Stage)
		}
		tasks = []store.Task{t}
	}
	merged := 0
	for _, t := range tasks {
		ok, err := env.mergeOne(ctx, p, t, acc)
		if err != nil {
			return err
		}
		if ok {
			merged++
		}
	}
	fmt.Fprintf(env.Stdout, "merged %d task(s); nothing was pushed\n", merged)
	var unused []string
	for _, a := range acc {
		if !a.used {
			unused = append(unused, a.target)
		}
	}
	if len(unused) > 0 {
		return fail(ExitUsage, "--accept-regression named target(s) that blocked nothing, so nothing was accepted under them: %s", strings.Join(unused, ", "))
	}
	if len(stuck) > 0 {
		return fail(ExitState, "task(s) left at merging by an earlier merge that could not be settled safely: %s", strings.Join(stuck, ", "))
	}
	return nil
}

// settleMerging recovers tasks an interrupted merge left at merging (§10.4):
// merged when the target branch already holds the task's branch head, back
// to done when it does not. Anything ambiguous is left at merging, explained
// on stderr, and its slug returned. The caller holds merge.lock.
func (env Env) settleMerging(ctx context.Context, p *project) ([]string, error) {
	tasks, err := p.db.TasksAt(ctx, store.StageMerging)
	if err != nil {
		return nil, err
	}
	var stuck []string
	for _, t := range tasks {
		why, err := env.settleOne(ctx, p, t)
		if err != nil {
			return stuck, err
		}
		if why != "" {
			fmt.Fprintf(env.Stderr, "%s: left at merging: %s; fix it by hand, then set it back to done\n", t.Slug, why)
			stuck = append(stuck, t.Slug)
		}
	}
	return stuck, nil
}

// settleOne settles one task left at merging. It returns why the task was
// left alone, or "" once it is settled.
func (env Env) settleOne(ctx context.Context, p *project, t store.Task) (string, error) {
	target := p.cfg.Merge.Base
	if t.Lane == "hotfix" {
		if strings.HasPrefix(t.BaseRef, "refs/tags/") {
			return "a hotfix cut from a tag has no merge target", nil
		}
		target = t.BaseRef
	}
	tlock, err := p.lockTask(t, "merge")
	if err != nil {
		return err.Error(), nil
	}
	defer tlock.Release()
	if t, err = p.db.GetTask(ctx, t.Slug); err != nil {
		return "", err
	}
	if t.Stage != store.StageMerging {
		return "", nil
	}
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		path, err := gitx.Run(ctx, t.Worktree, "rev-parse", "--path-format=absolute", "--git-path", dir)
		if err != nil {
			return fmt.Sprintf("cannot inspect worktree %s: %v", t.Worktree, err), nil
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Sprintf("a rebase is in progress in %s (run git rebase --abort there)", t.Worktree), nil
		}
	}
	head, err := gitx.Run(ctx, p.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+t.Branch+"^{commit}")
	if err != nil || head == "" {
		return fmt.Sprintf("its branch %s is not a local branch", t.Branch), nil
	}
	tip, err := gitx.Run(ctx, p.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+target+"^{commit}")
	if err != nil || tip == "" {
		return fmt.Sprintf("merge target %q is not a local branch", target), nil
	}
	if _, err := gitx.Run(ctx, p.root, "merge-base", "--is-ancestor", head, tip); err != nil {
		if _, err := p.db.Update(ctx, t, []string{store.StageMerging}, "stage = ?", []any{store.StageDone}, nil); err != nil {
			return "", err
		}
		fmt.Fprintf(env.Stdout, "%s: recovered: an earlier merge stopped before %s held %s; back to done\n", t.Slug, target, shortSHA(head))
		return "", nil
	}
	if _, err := p.db.Update(ctx, t, []string{store.StageMerging}, "stage = ?, merged_sha = ?", []any{store.StageMerged, head},
		func(tx *sql.Tx) error { return store.ReleaseTaskClaims(ctx, tx, t.ID) }); err != nil {
		return "", err
	}
	note := fmt.Sprintf("Recovered by shoalctl merge on %s: an earlier merge fast-forwarded %s to %s but stopped before recording it, so the task was marked merged. "+
		"That merge's gate run and any accepted regressions were not recorded; not pushed", nowUTC(), target, shortSHA(head))
	if err := env.appendReportNote(p, t.Slug, note); err != nil {
		return "", err
	}
	fmt.Fprintf(env.Stdout, "%s: recovered: %s already holds %s; marked merged\n", t.Slug, target, shortSHA(head))
	return "", nil
}

// appendReportNote marks a task's report merged and appends note to it.
func (env Env) appendReportNote(p *project, slug, note string) error {
	path := filepath.Join(p.dir, "reports", slug+".md")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read report %s: %w", path, err)
	}
	report := strings.Replace(string(old), "\nstatus: "+store.StageDone+"\n", "\nstatus: "+store.StageMerged+"\n", 1)
	report = strings.TrimRight(report, "\n") + "\n\n" + note + "\n"
	return env.writeReport(p, slug, strings.TrimLeft(report, "\n"))
}

// mergeOne takes one done task through the gate into its target branch. It
// returns false for a task with no merge target (a hotfix cut from a tag).
func (env Env) mergeOne(ctx context.Context, p *project, t store.Task, acc []*acceptance) (bool, error) {
	target := p.cfg.Merge.Base
	if t.Lane == "hotfix" {
		if strings.HasPrefix(t.BaseRef, "refs/tags/") {
			fmt.Fprintf(env.Stdout, "%s: skipped, its production ref %s is a tag; the branch %s is ready for a human to tag and release\n",
				t.Slug, strings.TrimPrefix(t.BaseRef, "refs/tags/"), t.Branch)
			return false, nil
		}
		target = t.BaseRef
	}
	tlock, err := p.lockTask(t, "merge")
	if err != nil {
		return false, err
	}
	defer tlock.Release()
	if t, err = p.db.Update(ctx, t, []string{store.StageDone}, "stage = ?", []any{store.StageMerging}, nil); err != nil {
		return false, err
	}
	wctx := context.WithoutCancel(ctx)
	back := func(stage string, cause error) (bool, error) {
		if _, err := p.db.Update(wctx, t, []string{store.StageMerging}, "stage = ?", []any{stage}, nil); err != nil {
			return false, errors.Join(cause, err)
		}
		return false, cause
	}
	fmt.Fprintf(env.Stdout, "%s: merging into %s\n", t.Slug, target)
	if _, err := requireClean(ctx, t.Worktree, "merging"); err != nil {
		return back(store.StageDone, err)
	}
	targetSHA, err := gitx.Run(ctx, p.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+target+"^{commit}")
	if err != nil || targetSHA == "" {
		return back(store.StageDone, fail(ExitConfig, "merge target %q is not a local branch", target))
	}

	if err := env.rebase(ctx, p, t, targetSHA); err != nil {
		if errors.Is(err, errConflict) {
			return back(store.StageNeedsRebase, fail(ExitGate, "%s: rebase onto %s@%s conflicts; rebase aborted, the branch is unchanged and the task is needs-rebase. "+
				"Rebase it yourself in %s, then run shoalctl done %s again", t.Slug, target, shortSHA(targetSHA), t.Worktree, t.Slug))
		}
		return back(store.StageDone, err)
	}
	head, err := gitx.Run(ctx, t.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return back(store.StageDone, err)
	}
	// The branch now sits on targetSHA, so that is the task's base from here on.
	if t, err = p.db.Update(ctx, t, []string{store.StageMerging}, "base_sha = ?", []any{targetSHA}, nil); err != nil {
		return false, err
	}

	base, err := env.baselineAt(ctx, p, t, targetSHA)
	if err != nil {
		return back(store.StageDone, err)
	}
	if base.Status != testrun.StatusCompleted {
		if base.Status == testrun.StatusCancelled {
			return back(store.StageDone, fail(ExitInterrupted, "interrupted: baseline run %d was cancelled", base.ID))
		}
		return back(store.StageDone, fail(ExitBaseline, "baseline run %d at %s %s; see %s", base.ID, shortSHA(targetSHA), base.Status, base.OutputPath))
	}
	if t, err = p.db.Update(ctx, t, []string{store.StageMerging}, "baseline_run_id = ?", []any{base.ID}, nil); err != nil {
		return false, err
	}
	fmt.Fprintf(env.Stdout, "gate: running the test suite at %s ...\n", shortSHA(head))
	gate, err := env.runSuite(ctx, p, t, "gate", head, t.Worktree)
	if err != nil {
		return back(store.StageDone, err)
	}
	if gate.Status == testrun.StatusCancelled {
		return back(store.StageDone, fail(ExitInterrupted, "interrupted: gate run %d was cancelled", gate.ID))
	}
	lint := env.step(ctx, p, t.Worktree, "lint", p.cfg.Stack.Lint, p.runDir(gate.ID))
	build := env.step(ctx, p, t.Worktree, "build", p.cfg.Stack.Build, p.runDir(gate.ID))
	if ctx.Err() != nil {
		return back(store.StageDone, fail(ExitInterrupted, "interrupted during lint or build"))
	}
	if err := requireUnchanged(ctx, t.Worktree, head, t.Slug); err != nil {
		return back(store.StageDone, err)
	}
	changed, err := changedPaths(ctx, t.Worktree, targetSHA)
	if err != nil {
		return back(store.StageDone, err)
	}
	g, err := env.evaluate(ctx, p, base, gate, changed)
	if err != nil {
		return back(store.StageDone, err)
	}
	for _, s := range []struct{ id, res string }{{"(lint)", lint}, {"(build)", build}} {
		if strings.HasPrefix(s.res, "FAIL") || strings.HasPrefix(s.res, "ERROR") {
			g.blocking = append(g.blocking, s.id)
			g.notes = append(g.notes, strings.Trim(s.id, "()")+": "+s.res)
		}
	}
	g.print(env.Stdout, "gate")

	var accepted []*acceptance
	var open []string
	for _, b := range g.blocking {
		var hit *acceptance
		for _, a := range acc {
			if a.target == b {
				hit = a
			}
		}
		if hit == nil {
			open = append(open, b)
			continue
		}
		hit.used = true
		accepted = append(accepted, hit)
	}
	if len(open) > 0 {
		if _, err := p.db.Update(wctx, t, []string{store.StageMerging}, "stage = ?, gate_run_id = ?", []any{store.StageQA, gate.ID}, nil); err != nil {
			return false, err
		}
		var flags []string
		for _, o := range open {
			flags = append(flags, fmt.Sprintf("--accept-regression %s --reason \"<why>\"", shellWord(o)))
		}
		return false, fail(ExitGate, "%s: the merge gate blocked on %s; nothing was merged and the task is back at qa (round %d kept). "+
			"Fix it, or a human accepts each target by name for this task: shoalctl merge --task %s %s", t.Slug, strings.Join(open, ", "), t.QARound, t.Slug, strings.Join(flags, " "))
	}

	sm, err := summaryFromDB(ctx, p, t.ID)
	if err != nil {
		return back(store.StageDone, err)
	}
	in, err := env.gatherReport(ctx, p, t, sm, head, targetSHA, base, gate)
	if err != nil {
		return back(store.StageDone, err)
	}
	by := humanName(ctx, p.root)
	for _, a := range accepted {
		in.overrides = append(in.overrides, store.Override{TaskID: t.ID, GateRunID: gate.ID, Target: a.target, Reason: a.reason, AcceptedBy: by})
	}
	in.status, in.finalLabel = store.StageMerged, "Gate"
	in.ver.Lint, in.ver.Build = lint, build
	in.notes = append(in.notes, fmt.Sprintf("Merged into %s at %s (fast-forward from %s); not pushed", target, shortSHA(head), shortSHA(targetSHA)))
	if t.LargeDiffReason != "" {
		n, err := productionDiffLines(ctx, t.Worktree, targetSHA)
		if err != nil {
			return back(store.StageDone, err)
		}
		in.notes = append(in.notes, largeDiffNote(n, p.cfg.Hotfix.MaxDiffLines, t.LargeDiffReason))
	}
	if t.Lane == "hotfix" {
		if kids, err := p.db.Children(ctx, t.ID); err == nil && len(kids) > 0 {
			in.notes = append(in.notes, "Backport: task "+kids[0].Slug)
		}
	}
	report := renderReport(in)

	if err := env.fastForward(ctx, p, t, target, targetSHA, head); err != nil {
		return back(store.StageDone, err)
	}
	commits, _ := json.Marshal(nonNilStrings(in.commits))
	ver, _ := json.Marshal(in.ver)
	if _, err := p.db.Update(wctx, t, []string{store.StageMerging}, "stage = ?, merged_sha = ?, gate_run_id = ?",
		[]any{store.StageMerged, head, gate.ID}, func(tx *sql.Tx) error {
			for _, o := range in.overrides {
				if o.GateRunID != gate.ID {
					continue
				}
				if err := store.InsertOverride(ctx, tx, o); err != nil {
					return err
				}
			}
			if err := store.ReleaseTaskClaims(ctx, tx, t.ID); err != nil {
				return err
			}
			return store.UpdateSummaryCommits(ctx, tx, t.ID, string(commits), head, string(ver))
		}); err != nil {
		return false, fmt.Errorf("%s was fast-forwarded to %s, but recording the merge failed: %w", target, shortSHA(head), err)
	}
	if err := env.writeReport(p, t.Slug, report); err != nil {
		return true, err
	}
	fmt.Fprintf(env.Stdout, "merged %s into %s at %s\n", t.Slug, target, shortSHA(head))
	return true, nil
}

var errConflict = errors.New("rebase conflict")

// rebase rebases the task branch onto targetSHA under git.lock; a conflict
// is always aborted (§10.5).
func (env Env) rebase(ctx context.Context, p *project, t store.Task, targetSHA string) error {
	glock, err := lockfile.Acquire(filepath.Join(p.dir, "locks", "git.lock"), 30*time.Second,
		lockfile.Info{PID: os.Getpid(), Command: "merge", Task: t.Slug, StartedAt: nowUTC()})
	if err != nil {
		return err
	}
	defer glock.Release()
	if _, err := gitx.Run(ctx, t.Worktree, "merge-base", "--is-ancestor", targetSHA, "HEAD"); err == nil {
		return nil // already on the target's tip
	}
	if _, err := gitx.Run(ctx, t.Worktree, "-c", "rebase.autoStash=false", "-c", "rebase.updateRefs=false", "rebase", "--no-autosquash", targetSHA); err != nil {
		if _, aerr := gitx.Run(context.WithoutCancel(ctx), t.Worktree, "rebase", "--abort"); aerr != nil {
			return fmt.Errorf("rebase of %s failed (%v) and rebase --abort failed too; fix %s by hand: %w", t.Branch, err, t.Worktree, aerr)
		}
		return fmt.Errorf("%w: %v", errConflict, err)
	}
	return nil
}

// baselineAt reuses a completed baseline for sha or records one in a
// scratch worktree detached at sha.
func (env Env) baselineAt(ctx context.Context, p *project, t store.Task, sha string) (store.Run, error) {
	hash := testrun.CommandHash(p.cfg.Tests.Command, p.cfg.Tests.Format, p.cfg.Tests.RerunCommand)
	if run, ok, err := p.db.FindBaseline(ctx, sha, hash); err != nil || ok {
		if ok {
			fmt.Fprintf(env.Stdout, "baseline: reusing run %d at %s\n", run.ID, shortSHA(sha))
		}
		return run, err
	}
	scratch := filepath.Join(p.dir, "scratch", fmt.Sprintf("baseline-%d-%d", t.ID, os.Getpid()))
	if err := env.withGitLock(p, t, func() error {
		if err := os.MkdirAll(filepath.Dir(scratch), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(scratch), err)
		}
		_, err := gitx.Run(ctx, p.root, "worktree", "add", "--detach", scratch, sha)
		return err
	}); err != nil {
		return store.Run{}, err
	}
	defer func() {
		// The scratch worktree is shoal's own and holds no work: remove it.
		_ = os.RemoveAll(scratch)
		_ = env.withGitLock(p, t, func() error {
			_, err := gitx.Run(context.WithoutCancel(ctx), p.root, "worktree", "prune")
			return err
		})
	}()
	fmt.Fprintf(env.Stdout, "baseline: running the test suite at %s (the merge target) ...\n", shortSHA(sha))
	return env.runSuite(ctx, p, t, "baseline", sha, scratch)
}

func (env Env) withGitLock(p *project, t store.Task, fn func() error) error {
	glock, err := lockfile.Acquire(filepath.Join(p.dir, "locks", "git.lock"), 30*time.Second,
		lockfile.Info{PID: os.Getpid(), Command: "merge", Task: t.Slug, StartedAt: nowUTC()})
	if err != nil {
		return err
	}
	defer glock.Release()
	return fn()
}

// fastForward moves target from old to head (§10.5): update-ref when the
// branch is checked out nowhere, merge --ff-only in a clean main checkout
// that has it checked out, and refusal otherwise.
func (env Env) fastForward(ctx context.Context, p *project, t store.Task, target, old, head string) error {
	return env.withGitLock(p, t, func() error {
		out, err := gitx.Run(ctx, p.root, "worktree", "list", "--porcelain")
		if err != nil {
			return err
		}
		where := ""
		cur := ""
		for _, line := range strings.Split(out, "\n") {
			if path, ok := strings.CutPrefix(line, "worktree "); ok {
				cur = path
			}
			if line == "branch refs/heads/"+target {
				where = cur
			}
		}
		switch {
		case where == "":
			_, err := gitx.Run(ctx, p.root, "update-ref", "-m", "shoalctl merge "+t.Slug, "refs/heads/"+target, head, old)
			if err != nil {
				return fail(ExitBusy, "%s moved while the gate ran (update-ref refused); run shoalctl merge again: %v", target, err)
			}
			return nil
		case sameDir(where, p.root):
			dirty, err := gitx.Run(ctx, p.root, "status", "--porcelain", "--untracked-files=no")
			if err != nil {
				return err
			}
			if dirty != "" {
				return fail(ExitState, "%s is checked out in %s with uncommitted changes; commit or stash them, then run shoalctl merge again", target, p.root)
			}
			now, err := gitx.Run(ctx, p.root, "rev-parse", "HEAD")
			if err != nil {
				return err
			}
			if now != old {
				return fail(ExitBusy, "%s moved while the gate ran (%s, now %s); run shoalctl merge again", target, shortSHA(old), shortSHA(now))
			}
			_, err = gitx.Run(ctx, p.root, "merge", "--ff-only", "--no-edit", head)
			return err
		}
		return fail(ExitState, "%s is checked out in another worktree (%s); shoal only fast-forwards it in the main checkout or when it is checked out nowhere", target, where)
	})
}

// summaryFromDB rebuilds the summary.json content done stored.
func summaryFromDB(ctx context.Context, p *project, taskID int64) (summaryFile, error) {
	row, err := p.db.GetSummary(ctx, taskID)
	if err != nil {
		return summaryFile{}, err
	}
	sm := summaryFile{Decision: row.Decision, Why: row.Why}
	for _, f := range []struct {
		raw string
		dst any
	}{{row.AlternativesRejected, &sm.AlternativesRejected}, {row.Risks, &sm.Risks}, {row.Followups, &sm.Followups}, {row.ChangedNotes, &sm.Changed}} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			return sm, fmt.Errorf("decode summary of task %d: %w", taskID, err)
		}
	}
	return sm, nil
}

// shellWord quotes s for a copy-pasteable shell command when needed.
func shellWord(s string) string {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r)) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
