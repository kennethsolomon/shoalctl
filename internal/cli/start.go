package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/config"
	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
)

const sessionPrompt = "Read .shoal/brief.md first. It is your task brief. Follow it."

type taskFile struct {
	ProjectID string `json:"project_id"`
	TaskID    int64  `json:"task_id"`
	Slug      string `json:"slug"`
}

func (env Env) cmdStart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	model := fs.String("model", "", "model for the session (default [session].model)")
	pos, err := parseArgs(fs, args, 1)
	if err != nil {
		return err
	}
	if strings.HasPrefix(*model, "-") {
		return fail(ExitUsage, "--model %q is not a model name", *model)
	}

	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	t, err := p.db.GetTask(ctx, pos[0])
	if err != nil {
		return err
	}
	lock, err := p.lockTask(t, "start")
	if err != nil {
		return err
	}
	defer lock.Release()
	if t, err = p.db.GetTask(ctx, t.Slug); err != nil {
		return err
	}
	resumable := []string{store.StageNew, store.StageStarting, store.StageBaselineFailed}
	if !contains(resumable, t.Stage) {
		return fail(ExitState, "task %s is at %q; start needs new, starting or baseline-failed", t.Slug, t.Stage)
	}

	if err := env.checkTmux(ctx); err != nil {
		return err
	}
	claude, err := env.checkClaude(ctx)
	if err != nil {
		return err
	}
	if p.cfg.Tests.Format != "none" && strings.TrimSpace(p.cfg.Tests.Command) == "" {
		return fail(ExitConfig, "[tests].command is empty in %s; set it, or set format = \"none\" if the project has no tests", p.cfgPath)
	}
	if err := checkTrust(p.root); err != nil {
		return err
	}
	if *model == "" {
		*model = p.cfg.Session.Model
	}

	baseRef, baseSHA := t.BaseRef, t.BaseSHA
	if baseSHA == "" {
		if baseRef, baseSHA, err = resolveBase(ctx, p, t.Lane); err != nil {
			return err
		}
	}
	branch := t.Lane + "/" + t.Slug
	wt := p.worktree(t.Slug)
	if t, err = p.db.Update(ctx, t, resumable, "stage = ?, base_ref = ?, base_sha = ?, branch = ?, worktree = ?",
		[]any{store.StageStarting, baseRef, baseSHA, branch, wt}, nil); err != nil {
		return err
	}

	if err := env.ensureWorktree(ctx, p, t, branch, wt, baseSHA); err != nil {
		return err
	}
	if p.cfg.Tests.Format == "junit" {
		if err := testrun.CheckJunitPath(wt, p.runDir(0), p.cfg.Tests.JunitPath); err != nil {
			return fail(ExitConfig, "%v; fix [tests].junit_path in %s", err, p.cfgPath)
		}
	}
	if err := writeTaskFile(wt, taskFile{ProjectID: p.id, TaskID: t.ID, Slug: t.Slug}); err != nil {
		return err
	}

	if err := env.reapAbandoned(ctx, p, t); err != nil {
		return err
	}
	base, err := env.baseline(ctx, p, t, wt)
	if err != nil {
		return err
	}
	if base.Status != testrun.StatusCompleted {
		// Recorded even after an interrupt, so the task shows as resumable.
		if _, uerr := p.db.Update(context.WithoutCancel(ctx), t, []string{store.StageStarting}, "stage = ?", []any{store.StageBaselineFailed}, nil); uerr != nil {
			return uerr
		}
		if base.Status == testrun.StatusCancelled {
			return fail(ExitInterrupted, "interrupted: baseline run %d was cancelled and its processes stopped; run shoalctl start %s again to resume", base.ID, t.Slug)
		}
		return fail(ExitBaseline, "baseline run %d %s at %s; output: %s\nFix the test command or environment, then run shoalctl start %s again",
			base.ID, base.Status, shortSHA(baseSHA), base.OutputPath, t.Slug)
	}
	if t, err = p.db.Update(ctx, t, []string{store.StageStarting}, "baseline_run_id = ?", []any{base.ID}, nil); err != nil {
		return err
	}
	results, err := p.db.Results(ctx, base.ID)
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(wt, ".shoal", "brief.md"), []byte(renderBrief(t, p.cfg, base, results))); err != nil {
		return err
	}

	session, window, created, err := env.launch(ctx, p, t, wt, claude, *model)
	if err != nil {
		return err
	}
	if _, err = p.db.Update(ctx, t, []string{store.StageStarting},
		"stage = ?, tmux_session = ?, tmux_window = ?, model = ?, started_at = ?",
		[]any{store.StageInProgress, session, window, *model, nowUTC()}, nil); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "started %s in tmux window %s (session %s, model %s)\nworktree %s\n", t.Slug, window, session, *model, wt)
	if created {
		fmt.Fprintf(env.Stdout, "attach with: tmux attach -t %s\n", session)
	}
	return nil
}

// resolveBase finds the start point: [merge].base for feature and bugfix,
// [hotfix].production_ref for hotfix (§6.3). It never fetches.
func resolveBase(ctx context.Context, p *project, lane string) (string, string, error) {
	if lane != "hotfix" {
		sha, err := gitx.Run(ctx, p.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+p.cfg.Merge.Base+"^{commit}")
		if err != nil || sha == "" {
			return "", "", fail(ExitConfig, "[merge].base %q is not a local branch; set [merge].base in %s", p.cfg.Merge.Base, p.cfgPath)
		}
		return p.cfg.Merge.Base, sha, nil
	}
	ref := p.cfg.Hotfix.ProductionRef
	if ref == "" {
		return "", "", fail(ExitConfig, "hotfix tasks need [hotfix].production_ref in %s", p.cfgPath)
	}
	if glob, ok := strings.CutPrefix(ref, "tag:"); ok {
		out, err := gitx.Run(ctx, p.root, "tag", "--list", glob, "--sort=-v:refname")
		if err != nil {
			return "", "", err
		}
		newest, _, _ := strings.Cut(out, "\n")
		if newest == "" {
			return "", "", fail(ExitConfig, "[hotfix].production_ref %q matches no tag", ref)
		}
		sha, err := gitx.Run(ctx, p.root, "rev-parse", "--verify", "refs/tags/"+newest+"^{commit}")
		if err != nil {
			return "", "", fail(ExitConfig, "tag %s does not point at a commit: %v", newest, err)
		}
		return "refs/tags/" + newest, sha, nil
	}
	branchSHA, berr := gitx.Run(ctx, p.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+ref+"^{commit}")
	tagSHA, terr := gitx.Run(ctx, p.root, "rev-parse", "--verify", "--quiet", "refs/tags/"+ref+"^{commit}")
	switch {
	case berr == nil && terr == nil:
		return "", "", fail(ExitConfig, "[hotfix].production_ref %q is both a branch and a tag; use tag:%s or rename one", ref, ref)
	case berr == nil:
		return ref, branchSHA, nil
	case terr == nil:
		return "refs/tags/" + ref, tagSHA, nil
	}
	return "", "", fail(ExitConfig, "[hotfix].production_ref %q is not a local branch or tag", ref)
}

func (env Env) ensureWorktree(ctx context.Context, p *project, t store.Task, branch, wt, baseSHA string) error {
	lock, err := lockfile.Acquire(filepath.Join(p.dir, "locks", "git.lock"), 30*time.Second,
		lockfile.Info{PID: os.Getpid(), Command: "start", Task: t.Slug, StartedAt: nowUTC()})
	if err != nil {
		return err
	}
	defer lock.Release()

	if _, err := os.Stat(wt); err == nil {
		head, err := gitx.Run(ctx, wt, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil || head != branch {
			return fail(ExitState, "%s exists but is not the worktree of branch %s (HEAD %q); move it away and retry", wt, branch, head)
		}
		return nil
	}
	if _, err := gitx.Run(ctx, p.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return fail(ExitState, "branch %s already exists but worktree %s does not; shoal will not reuse a branch it did not create", branch, wt)
	}
	if _, err := gitx.Run(ctx, p.root, "worktree", "add", "-b", branch, wt, baseSHA); err != nil {
		return fmt.Errorf("create worktree for %s: %w", t.Slug, err)
	}
	return nil
}

func writeTaskFile(wt string, tf taskFile) error {
	body, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(wt, ".shoal", "task.json"), append(body, '\n'))
}

func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	return writeFileAtomic(path, data, 0o600)
}

// baseline reuses a completed baseline for the same sha and command, or runs
// the suite now in the worktree (§8.2).
func (env Env) baseline(ctx context.Context, p *project, t store.Task, wt string) (store.Run, error) {
	hash := testrun.CommandHash(p.cfg.Tests.Command, p.cfg.Tests.Format, p.cfg.Tests.RerunCommand)
	if run, ok, err := p.db.FindBaseline(ctx, t.BaseSHA, hash); err != nil || ok {
		if ok {
			fmt.Fprintf(env.Stdout, "baseline: reusing run %d at %s (%d pass, %d fail, %d skip)\n", run.ID, shortSHA(t.BaseSHA), run.Passed, run.Failed, run.Skipped)
		}
		return run, err
	}
	fmt.Fprintf(env.Stdout, "baseline: running the test suite at %s ...\n", shortSHA(t.BaseSHA))
	return env.runSuite(ctx, p, t, "baseline", t.BaseSHA, wt)
}

// runSuite records a test run: insert running, run with no transaction open,
// then store the outcome and results in one transaction.
func (env Env) runSuite(ctx context.Context, p *project, t store.Task, kind, sha, wt string) (store.Run, error) {
	cfg := p.cfg.Tests
	run, err := p.db.StartRun(ctx, store.Run{TaskID: t.ID, Kind: kind, SHA: sha, Command: cfg.Command,
		CommandHash: testrun.CommandHash(cfg.Command, cfg.Format, cfg.RerunCommand), Format: cfg.Format,
		Fidelity: testrun.Fidelity(cfg.Format)},
		func(id int64) string { return filepath.Join(p.runDir(id), "output.log") })
	if err != nil {
		return store.Run{}, err
	}
	// Bookkeeping writes outlive an interrupt, so the run is never left at
	// running with no owner.
	wctx := context.WithoutCancel(ctx)
	var pidErr error
	out := testrun.Run(ctx, testrun.Spec{Command: cfg.Command, Format: cfg.Format, JunitPath: cfg.JunitPath,
		Timeout: p.cfg.TestTimeout(), Dir: wt, RunDir: p.runDir(run.ID),
		OnStart: func(pid int) {
			start, err := testrun.ProcessStart(pid)
			if err != nil {
				fmt.Fprintf(env.Stderr, "%s run %d: %v; a later command will treat any live process group at this pid as this suite\n", kind, run.ID, err)
			}
			pidErr = p.db.SetRunPID(wctx, run.ID, pid, start)
		}})
	if pidErr != nil {
		fmt.Fprintf(env.Stderr, "%s run %d: %v\n", kind, run.ID, pidErr)
	}
	if out.Problem != "" {
		fmt.Fprintf(env.Stderr, "%s run %d: %s\n", kind, run.ID, out.Problem)
	}
	results := make([]store.Result, len(out.Results))
	for i, r := range out.Results {
		results[i] = store.Result(r)
	}
	return p.db.FinishRun(wctx, run.ID, out.Status, out.ExitCode, results)
}

// reapAbandoned handles runs of this task still marked running. The caller
// holds the task lock, so their owner has exited. A run whose suite is still
// alive, or might be (its start time cannot be read or was never recorded),
// blocks the command: two suites must never share a worktree. A dead one, or
// one whose pid now belongs to another process (its start time differs from
// the recorded one), is marked error, with a note in its output.log.
func (env Env) reapAbandoned(ctx context.Context, p *project, t store.Task) error {
	runs, err := p.db.RunningRuns(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if testrun.GroupAlive(r.PID) {
			live, err := testrun.ProcessStart(r.PID)
			if r.PIDStart == "" || err != nil || live == r.PIDStart {
				return fail(ExitBusy, "%s run %d of task %s is still running (pid %d) although the shoalctl that started it exited; "+
					"stop it (on unix: kill -TERM -- -%d), then retry", r.Kind, r.ID, t.Slug, r.PID, r.PID)
			}
			fmt.Fprintf(env.Stderr, "%s run %d of task %s: pid %d is alive but started at a different time than this run's suite (the pid was reused); leaving that process alone\n",
				r.Kind, r.ID, t.Slug, r.PID)
		}
		if f, err := os.OpenFile(r.OutputPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
			fmt.Fprintf(f, "shoal: abandoned: the shoalctl that started this run exited before it finished; marked error at %s\n", nowUTC())
			f.Close()
		}
		if _, err := p.db.FinishRun(ctx, r.ID, testrun.StatusError, nil, nil); err != nil {
			return err
		}
		fmt.Fprintf(env.Stderr, "%s run %d of task %s was abandoned by an exited shoalctl; marked error\n", r.Kind, r.ID, t.Slug)
	}
	return nil
}

// launch opens a detached tmux window running claude in the worktree.
func (env Env) launch(ctx context.Context, p *project, t store.Task, wt, claude, model string) (session, window string, created bool, err error) {
	if os.Getenv("TMUX") != "" {
		session, err = env.tmux(ctx, "display-message", "-p", "#{session_name}")
		if err != nil {
			return "", "", false, err
		}
	} else {
		session = "shoal-" + p.id[:8]
		if _, herr := env.tmux(ctx, "has-session", "-t", "="+session); herr != nil {
			if _, err = env.tmux(ctx, "new-session", "-d", "-s", session, "-n", "shoal"); err != nil {
				return "", "", false, err
			}
			created = true
		}
	}
	window, err = env.tmux(ctx, "new-window", "-d", "-t", "="+session+":", "-n", t.Slug, "-c", wt,
		"-e", "SHOAL_HOME="+p.home, "-P", "-F", "#{window_id}", "--", claude, "--model", model, sessionPrompt)
	if err != nil {
		return "", "", false, err
	}
	if _, err = env.tmux(ctx, "set-option", "-w", "-t", window, "remain-on-exit", "on"); err != nil {
		return "", "", false, fmt.Errorf("tmux window %s closed right after launch, so claude likely exited at once; run `%s --model %s` in %s to see why: %w",
			window, claude, model, wt, err)
	}
	return session, window, created, nil
}

func (env Env) tmux(ctx context.Context, args ...string) (string, error) {
	full := append(append([]string{}, env.TmuxArgs...), args...)
	cmd := exec.CommandContext(ctx, env.Tmux, full...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		detail := ""
		if errors.As(err, &ee) {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(full, " "), err, detail)
	}
	return strings.TrimSpace(string(out)), nil
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

var laneRules = map[string]string{
	"feature": "Write an automated test for every new behaviour you add.",
	"bugfix":  "Write a test that reproduces the bug and fails first, commit it, then fix.",
	"hotfix":  "Write a test that reproduces the bug and fails first, commit it, then fix. Smallest possible diff: no refactors, no dependency changes.",
}

func renderBrief(t store.Task, cfg config.Config, base store.Run, results []store.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Task %s: %s\nLane: %s · Branch: %s · Base: %s @ %s\n\n## Ask\n%s\n\n",
		t.Slug, t.Title, t.Lane, t.Branch, t.BaseRef, shortSHA(t.BaseSHA), t.Ask)
	fmt.Fprintf(&b, "## Baseline (%s)\n", base.Fidelity)
	if base.Fidelity == "none" {
		b.WriteString("No test suite is configured ([tests].format = \"none\"). The zero-regression protocol is inactive.\n\n")
	} else {
		fmt.Fprintf(&b, "%d pass, %d fail, %d skip at %s.\n", base.Passed, base.Failed, base.Skipped, shortSHA(base.SHA))
		var failing []string
		for _, r := range results {
			if r.Status == "fail" {
				failing = append(failing, r.TestID)
			}
		}
		if len(failing) > 0 {
			b.WriteString("Pre-existing failures (not yours to fix unless the ask says so):\n")
			for i, id := range failing {
				if i == 20 {
					fmt.Fprintf(&b, "- and %d more\n", len(failing)-20)
					break
				}
				fmt.Fprintf(&b, "- %s\n", id)
			}
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "## Rules for this lane\n- %s\n- Do not break any test listed as passing at baseline.\n", laneRules[t.Lane])
	if cfg.Tests.Format != "none" {
		fmt.Fprintf(&b, "- Test command: `%s`\n", cfg.Tests.Command)
	}
	b.WriteString(`
## When you finish
1. Commit all work on this branch. Do not push. Do not merge.
2. Write ` + "`.shoal/summary.json`" + ` (format below).
3. Tell the human you are finished: they exit this session, then run ` + "`shoalctl done " + t.Slug + "`" + `.

summary.json:
{"decision": "...", "alternatives_rejected": [{"option": "...", "why": "..."}],
 "why": "...", "risks": ["..."], "followups": ["..."],
 "changed": [{"path": "...", "note": "one line"}]}
`)
	return b.String()
}
