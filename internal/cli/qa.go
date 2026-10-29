package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

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
	head, err := requireClean(ctx, t.Worktree, "a QA round")
	if err != nil {
		return err
	}
	if err := env.reapAbandoned(ctx, p, t); err != nil {
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
	if err := requireUnchanged(ctx, t.Worktree, head, t.Slug); err != nil {
		return err
	}

	passed := len(failures) == 0
	stage := store.StageQA
	if !passed && round >= maxRounds {
		stage = store.StageEscalated
	}
	enc, _ := json.Marshal(nonNilStrings(failures))
	t, err = p.db.Update(ctx, t, from, "stage = ?, qa_round = ?", []any{stage, round}, func(tx *sql.Tx) error {
		if err := store.InsertQARound(ctx, tx, store.QARound{TaskID: t.ID, Round: round, RunID: runID, Passed: passed, Failures: string(enc)}); err != nil {
			return err
		}
		if passed {
			return nil
		}
		_, err := store.InsertMessage(ctx, tx, store.Message{TaskID: t.ID, From: "shoal-qa-regression", To: leadAgent, Kind: "qa-failure",
			Subject: fmt.Sprintf("QA round %d of %d failed", round, maxRounds), Body: truncateRunes(strings.Join(failures, "\n"), 4000)})
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
	return fail(ExitGate, "qa round %d of %d failed (sent to shoal-lead as a qa-failure message):\n  %s", round, maxRounds, strings.Join(failures, "\n  "))
}

// escalate marks the tmux window and writes the escalation report (§6.4).
func (env Env) escalate(ctx context.Context, p *project, t store.Task, failures []string) error {
	if t.TmuxWindow != "" {
		if _, err := env.tmux(ctx, "rename-window", "-t", t.TmuxWindow, "!"+t.Slug); err != nil {
			fmt.Fprintf(env.Stderr, "warning: could not rename tmux window %s: %v\n", t.TmuxWindow, err)
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
