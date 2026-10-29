package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/globx"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
)

// cmdRepro records the red or green half of a regression proof (§8.3).
func (env Env) cmdRepro(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "red" && args[0] != "green") {
		return fail(ExitUsage, "usage: repro red <task> --test <id> --file <path> | repro green <task> --test <id>")
	}
	half := args[0]
	fs := flag.NewFlagSet("repro "+half, flag.ContinueOnError)
	testID := fs.String("test", "", "the regression test's id, as the test report names it")
	file := fs.String("file", "", "the regression test's file, relative to the repo root (red)")
	pos, err := parseArgs(fs, args[1:], 1)
	if err != nil {
		return err
	}
	if *testID == "" {
		return fail(ExitUsage, "--test <id> is required")
	}
	if half == "red" {
		if err := globx.Validate(*file); err != nil {
			return fail(ExitUsage, "--file: %v", err)
		}
	} else if *file != "" {
		return fail(ExitUsage, "--file belongs to repro red; green reuses the file red recorded")
	}
	p, t, lock, err := env.lockedTask(ctx, pos[0], "repro "+half)
	if err != nil {
		return err
	}
	defer p.close()
	defer lock.Release()
	if t.Lane == "feature" {
		return fail(ExitState, "task %s is a feature; red/green proofs are for bugfix and hotfix tasks (features list behaviours: scope add --behaviour)", t.Slug)
	}
	if !contains(workingStages, t.Stage) {
		return fail(ExitState, "task %s is at %q; repro needs a started task", t.Slug, t.Stage)
	}
	if p.cfg.Tests.Format == "none" || p.cfg.Tests.Format == "exit-code" {
		return fail(ExitConfig, "repro needs per-test results; [tests].format is %q", p.cfg.Tests.Format)
	}
	head, err := requireClean(ctx, t.Worktree, "recording a proof")
	if err != nil {
		return err
	}
	var proof store.Proof
	if half == "red" {
		if _, err := gitx.Run(ctx, t.Worktree, "cat-file", "-e", "HEAD:"+*file); err != nil {
			return fail(ExitNotFound, "%s is not committed at HEAD; commit the regression test first", *file)
		}
	} else {
		proofs, err := p.db.Proofs(ctx, t.ID)
		if err != nil {
			return err
		}
		found := false
		for _, pr := range proofs {
			if pr.TestID == *testID {
				proof, found = pr, true
			}
		}
		if !found {
			return fail(ExitState, "no red run of %s is recorded; run shoalctl repro red %s --test %s --file <path> first", *testID, t.Slug, *testID)
		}
		if _, err := gitx.Run(ctx, t.Worktree, "merge-base", "--is-ancestor", proof.RedSHA, head); err != nil {
			return fail(ExitState, "HEAD %s does not descend from the red commit %s; record red again on this history", shortSHA(head), shortSHA(proof.RedSHA))
		}
		// A not-reproducible red (backport) proves no bug, so there is no fix to
		// demand; a red that failed needs a later commit that changes something.
		if proof.RedStatus == "fail" {
			if err := requireFixCommit(ctx, t.Worktree, proof.RedSHA, head); err != nil {
				return err
			}
		}
	}

	command := p.cfg.Tests.Command
	if p.cfg.Tests.RerunCommand != "" {
		command = testrun.RerunCommand(p.cfg.Tests.RerunCommand, []string{*testID})
	}
	if err := env.reapAbandoned(ctx, p, t); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "repro %s: running %s at %s ...\n", half, *testID, shortSHA(head))
	run, err := env.runSuiteWith(ctx, p, t, "repro", head, t.Worktree, command)
	if err != nil {
		return err
	}
	if run.Status != testrun.StatusCompleted {
		return fail(ExitGate, "repro run %d ended with status %s; see %s", run.ID, run.Status, run.OutputPath)
	}
	results, err := p.db.EffectiveResults(ctx, run.ID)
	if err != nil {
		return err
	}
	status := statusMap(results)[*testID]
	if status == "" {
		return fail(ExitNotFound, "test %s is not in the results of run %d; check the id against %s", *testID, run.ID, run.OutputPath)
	}

	if half == "red" {
		switch {
		case status == "fail":
			if err := p.db.SetRed(ctx, store.Proof{TaskID: t.ID, TestID: *testID, TestFile: *file, RedStatus: "fail", RedRunID: run.ID, RedSHA: head}); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "red recorded: %s fails at %s (run %d). Fix the bug, commit, then run shoalctl repro green %s --test %s\n",
				*testID, shortSHA(head), run.ID, t.Slug, *testID)
			return nil
		case status == "pass" && t.ParentTaskID != 0:
			if err := p.db.SetRed(ctx, store.Proof{TaskID: t.ID, TestID: *testID, TestFile: *file, RedStatus: "not-reproducible", RedRunID: run.ID, RedSHA: head}); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "not reproducible: %s already passes at %s (run %d), so the bug never existed on this base; recorded for the report\n",
				*testID, shortSHA(head), run.ID)
			return nil
		}
		return fail(ExitGate, "%s is %s at %s, not fail: the test does not reproduce the bug", *testID, status, shortSHA(head))
	}

	if status != "pass" {
		return fail(ExitGate, "%s is %s at %s, not pass", *testID, status, shortSHA(head))
	}
	diff, err := gitx.Run(ctx, t.Worktree, "diff", "--name-only", proof.RedSHA, head, "--", proof.TestFile)
	if err != nil {
		return err
	}
	changed := diff != ""
	if err := p.db.SetGreen(ctx, t.ID, *testID, run.ID, head, changed); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "green recorded: %s passes at %s (run %d)\n", *testID, shortSHA(head), run.ID)
	if changed {
		fmt.Fprintf(env.Stdout, "warning: %s changed after the red run; the report flags it for review\n", proof.TestFile)
	}
	return nil
}

// requireFixCommit refuses a green run unless head is a later commit than
// red with a non-empty diff from it (§8.3): a test that passes on the red
// commit itself, or on an empty commit, proves no fix.
func requireFixCommit(ctx context.Context, wt, red, head string) error {
	if head == red {
		return fail(ExitGate, "HEAD is the same commit as the red run (%s): a green run needs a later commit with the fix; commit the fix, then run repro green again", shortSHA(head))
	}
	trees, err := gitx.Run(ctx, wt, "rev-parse", red+"^{tree}", head+"^{tree}")
	if err != nil {
		return err
	}
	if t := strings.Fields(trees); len(t) == 2 && t[0] == t[1] {
		return fail(ExitGate, "HEAD %s changes nothing since the red commit %s (empty diff): a green run needs a commit with the fix", shortSHA(head), shortSHA(red))
	}
	return nil
}

// proofProblem says why a bugfix or hotfix lacks a complete red/green proof,
// or "" when it has one (§8.3).
func proofProblem(ctx context.Context, p *project, t store.Task) (string, error) {
	if t.Lane == "feature" {
		return "", nil
	}
	proofs, err := p.db.Proofs(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if len(proofs) == 0 {
		return fmt.Sprintf("no red/green regression proof: commit a failing test, run shoalctl repro red %s --test <id> --file <path>, fix, then shoalctl repro green", t.Slug), nil
	}
	for _, pr := range proofs {
		if pr.GreenRunID != 0 {
			return "", nil
		}
	}
	return fmt.Sprintf("the red run of %s has no green run yet: shoalctl repro green %s --test %s", proofs[0].TestID, t.Slug, proofs[0].TestID), nil
}
