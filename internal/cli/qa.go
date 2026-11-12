package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
)

// failureRows labels every blocking row of a gate result with its class.
func (g gateResult) failureRows() []string {
	classOf := map[string]testrun.Class{}
	for _, c := range testrun.BlockingClasses {
		for _, id := range g.classes[c] {
			classOf[id] = c
		}
	}
	out := make([]string, 0, len(g.blocking))
	for _, b := range g.blocking {
		if c, ok := classOf[b]; ok {
			out = append(out, fmt.Sprintf("%s: %s", c, b))
		} else {
			out = append(out, "blocked: "+b)
		}
	}
	return out
}

// cmdQA runs one QA round (§6.4): contract hash, the suite against the
// baseline, lint and build. A failed round at the cap escalates the task.
func (env Env) cmdQA(ctx context.Context, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("qa", flag.ContinueOnError), args, 1)
	if err != nil {
		return err
	}
	// A round is the human's QA: an agent must not count one (§16 row 88).
	if k := env.claudeCodeSession(); k != "" {
		return fail(ExitState, "qa refused: this process runs inside a Claude Code session (%s is set), and a QA round is the human's. "+
			"Agents check with shoalctl test %s --kind qa; the human runs shoalctl qa in their own terminal", k, pos[0])
	}
	p, t, lock, err := env.lockedTask(ctx, pos[0], "qa")
	if err != nil {
		return err
	}
	defer p.close()
	defer lock.Release()
	from := []string{store.StageInProgress, store.StageQA, store.StageNeedsRebase}
	if t.Stage == store.StageEscalated {
		return fail(ExitState, "task %s is escalated after %d QA rounds; no further automatic rounds. A human decides what happens next (see %s)",
			t.Slug, t.QARound, filepath.Join(p.dir, "reports", t.Slug+".md"))
	}
	if !contains(from, t.Stage) {
		return fail(ExitState, "task %s is at %q; qa needs in-progress, qa or needs-rebase", t.Slug, t.Stage)
	}
	if err := env.reapAbandoned(ctx, p, t); err != nil {
		return err
	}
	if t.Stage == store.StageNeedsRebase {
		if t, err = env.rebaseForQA(ctx, p, t); err != nil {
			return err
		}
	}
	head, err := requireClean(ctx, t.Worktree, "a QA round")
	if err != nil {
		return err
	}
	round := t.QARound + 1
	maxRounds := p.cfg.Lanes.QAMaxRounds
	fmt.Fprintf(env.Stdout, "qa round %d of %d for %s at %s\n", round, maxRounds, t.Slug, shortSHA(head))

	var failures []string
	if prob, err := contractProblem(ctx, p, t); err != nil {
		return err
	} else if prob != "" {
		failures = append(failures, "contract: "+prob)
	}
	var runID int64
	if p.cfg.Tests.Format != "none" {
		run, err := env.runSuite(ctx, p, t, "qa", head, t.Worktree)
		if err != nil {
			return err
		}
		if run.Status == testrun.StatusCancelled {
			return fail(ExitInterrupted, "interrupted: qa run %d was cancelled; the round was not counted", run.ID)
		}
		runID = run.ID
		base, err := p.db.GetRun(ctx, t.BaselineRunID)
		if err != nil {
			return err
		}
		changed, err := changedPaths(ctx, t.Worktree, t.BaseSHA)
		if err != nil {
			return err
		}
		g, err := env.evaluate(ctx, p, base, run, changed)
		if err != nil {
			return err
		}
		g.print(env.Stdout, "qa")
		failures = append(failures, g.failureRows()...)
	}
	stepDir := filepath.Join(p.dir, "runs", "qa-"+t.Slug, fmt.Sprint(round))
	if runID != 0 {
		stepDir = p.runDir(runID)
	}
	for _, s := range []struct{ name, cmd string }{{"lint", p.cfg.Stack.Lint}, {"build", p.cfg.Stack.Build}} {
		res := env.step(ctx, p, t.Worktree, s.name, s.cmd, stepDir)
		fmt.Fprintf(env.Stdout, "%s: %s\n", s.name, res)
		if strings.HasPrefix(res, "FAIL") || strings.HasPrefix(res, "ERROR") {
			failures = append(failures, s.name+": "+res)
		}
	}
	if ctx.Err() != nil {
		return fail(ExitInterrupted, "interrupted; the round was not counted")
	}
	if err := requireUnchanged(ctx, t.Worktree, head, "qa "+t.Slug); err != nil {
		return err
	}
	tree, err := gitx.Run(ctx, t.Worktree, "rev-parse", head+"^{tree}")
	if err != nil {
		return err
	}

	passed := len(failures) == 0
	stage := store.StageQA
	if !passed && round >= maxRounds {
		stage = store.StageEscalated
	}
	enc, _ := json.Marshal(nonNilStrings(failures))
	t, err = p.db.Update(ctx, t, from, "stage = ?, qa_round = ?", []any{stage, round}, func(tx *sql.Tx) error {
		if err := store.InsertQARound(ctx, tx, store.QARound{TaskID: t.ID, Round: round, RunID: runID, Passed: passed, Failures: string(enc),
			HeadSHA: head, Tree: tree}); err != nil {
			return err
		}
		if passed {
			return nil
		}
		_, err := store.InsertMessage(ctx, tx, store.Message{TaskID: t.ID, From: "shoal-qa-regression", To: leadAgent, Kind: "qa-failure",
			Subject: fmt.Sprintf("QA round %d of %d failed", round, maxRounds),
			Body: truncateRunes(strings.Join(failures, "\n")+"\n\nOnce the cause is known: shoalctl record lesson "+t.Slug+
				" --pattern \"<what went wrong>\" --fix \"<what fixed it>\" --glob <files>", 4000)})
		return err
	})
	if err != nil {
		return err
	}
	if passed {
		fmt.Fprintf(env.Stdout, "qa round %d passed; run shoalctl done %s when the work is finished\n", round, t.Slug)
		return nil
	}
	if stage == store.StageEscalated {
		return env.escalate(ctx, p, t, failures)
	}
	return fail(ExitGate, "qa round %d of %d failed (sent to shoal-lead as a qa-failure message):\n  %s\n"+
		"Once the cause is known, record the lesson: shoalctl record lesson %s --pattern \"<what went wrong>\" --fix \"<what fixed it>\" --glob <files>",
		round, maxRounds, strings.Join(failures, "\n  "), t.Slug)
}

// rebaseForQA rebases a needs-rebase task onto its target's tip and moves
// its base and baseline there, so the round compares against what it will
// merge into (§9.4 step 5). A conflict is aborted and the task stays put.
func (env Env) rebaseForQA(ctx context.Context, p *project, t store.Task) (store.Task, error) {
	target, ok := mergeTarget(p, t)
	if !ok {
		return t, nil
	}
	if rebasing, err := rebaseInProgress(ctx, t.Worktree); err != nil {
		return t, err
	} else if rebasing {
		return t, fail(ExitState, "a rebase is in progress in %s; finish it (git rebase --continue) or abort it (git rebase --abort), then run shoalctl qa %s again",
			t.Worktree, t.Slug)
	}
	if _, err := requireClean(ctx, t.Worktree, "rebasing for QA"); err != nil {
		return t, err
	}
	tip, err := gitx.Run(ctx, p.root, "rev-parse", "--verify", "--quiet", "refs/heads/"+target+"^{commit}")
	if err != nil || tip == "" {
		return t, fail(ExitConfig, "merge target %q is not a local branch", target)
	}
	if err := env.rebase(ctx, p, t, tip); err != nil {
		if errors.Is(err, errConflict) {
			return t, fail(ExitGate, "%s: rebase onto %s@%s conflicts; rebase aborted, the branch is unchanged and the task stays needs-rebase. "+
				"Rebase it yourself in %s (git rebase %s), then run shoalctl qa %s again", t.Slug, target, shortSHA(tip), t.Worktree, target, t.Slug)
		}
		return t, err
	}
	if tip == t.BaseSHA {
		return t, nil
	}
	base, err := env.baselineAt(ctx, p, t, tip)
	if err != nil {
		return t, err
	}
	if base.Status != testrun.StatusCompleted {
		if base.Status == testrun.StatusCancelled {
			return t, fail(ExitInterrupted, "interrupted: baseline run %d was cancelled", base.ID)
		}
		return t, fail(ExitBaseline, "baseline run %d at %s %s; see %s", base.ID, shortSHA(tip), base.Status, base.OutputPath)
	}
	t, err = p.db.Update(ctx, t, []string{store.StageNeedsRebase}, "base_sha = ?, baseline_run_id = ?", []any{tip, base.ID}, nil)
	if err != nil {
		return t, err
	}
	fmt.Fprintf(env.Stdout, "qa: rebased %s onto %s@%s; baseline is now run %d\n", t.Slug, target, shortSHA(tip), base.ID)
	return t, nil
}

// escalate marks the tmux window and writes the escalation report (§6.4).
// tmux reuses window ids after a server restart, so the window is renamed
// only when it carries the task's tag; otherwise it is left as it was.
func (env Env) escalate(ctx context.Context, p *project, t store.Task, failures []string) error {
	if t.TmuxWindow != "" {
		e := env.taskTmux(t)
		ours, err := e.isTaskWindow(ctx, p, t)
		switch {
		case err != nil:
			fmt.Fprintf(env.Stderr, "warning: left tmux window %s as it was: could not read its tag: %v\n", t.TmuxWindow, err)
		case !ours:
			fmt.Fprintf(env.Stderr, "warning: left tmux window %s as it was: it is not tagged as %s's window (tmux reuses window ids, or the task predates tags)\n", t.TmuxWindow, t.Slug)
		default:
			if _, err := e.tmux(ctx, "rename-window", "-t", t.TmuxWindow, "!"+t.Slug); err != nil {
				fmt.Fprintf(env.Stderr, "warning: could not rename tmux window %s: %v\n", t.TmuxWindow, err)
			}
		}
	}
	rounds, err := p.db.QARounds(ctx, t.ID)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\ntask: %s\nlane: %s\nstatus: escalated\nbranch: %s\n---\n# %s\n\n## Asked\n%s\n\n", t.Slug, t.Lane, t.Branch, t.Title, t.Ask)
	b.WriteString(blockedSection(rounds, failures))
	path := filepath.Join(p.dir, "reports", t.Slug+".md")
	if err := writePrivate(path, []byte(b.String())); err != nil {
		return err
	}
	return fail(ExitGate, "qa round %d failed; task %s is escalated after %d rounds, no further automatic retries. Report: %s", t.QARound, t.Slug, t.QARound, path)
}

func blockedSection(rounds []store.QARound, last []string) string {
	var b strings.Builder
	b.WriteString("## Blocked\n")
	fmt.Fprintf(&b, "- What failed: %s\n", strings.Join(last, "; "))
	seen := map[string]int{}
	failed := 0
	for _, r := range rounds {
		var fs []string
		_ = json.Unmarshal([]byte(r.Failures), &fs)
		state := "passed"
		if !r.Passed {
			state = "failed: " + strings.Join(fs, "; ")
			failed++
			for _, f := range fs {
				seen[f]++
			}
		}
		fmt.Fprintf(&b, "- Attempt %d: %s\n", r.Round, state)
	}
	var every []string
	for _, f := range last {
		if seen[f] == failed && failed > 1 {
			every = append(every, f)
		}
	}
	if len(every) > 0 {
		fmt.Fprintf(&b, "- Best hypothesis: these failed in every failed round, so the fixes so far missed their cause: %s\n", strings.Join(every, "; "))
	} else {
		b.WriteString("- Best hypothesis: the failures moved between rounds; each fix broke something else. Re-plan before another round\n")
	}
	return b.String()
}

func nonNilStrings(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}
