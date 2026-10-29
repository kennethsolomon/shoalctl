package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
)

// ExitGate is "gate blocked" (§4.7, §8.9).
const ExitGate = 9

// gateResult compares one run with its baseline (§8.5).
type gateResult struct {
	base, run   store.Run
	runResults  []store.Result // effective results of run
	classes     map[testrun.Class][]string
	blocking    []string // test ids, coverage:<path>, (suite)
	coverage    []coverageRow
	quarantined []store.Quarantine
	notes       []string
}

type coverageRow struct {
	Path       string
	Base, Gate float64
	New        bool
	Dropped    bool
}

func pct(r float64) string { return fmt.Sprintf("%.1f%%", r*100) }

func rate(f store.CoverageFile) float64 {
	if f.LinesTotal == 0 {
		return 1
	}
	return float64(f.LinesCovered) / float64(f.LinesTotal)
}

// evaluate classifies run against base, ignoring quarantined tests, and adds
// the reduced-fidelity and coverage rules. changed lists the files the task
// changed, for the coverage rule.
func (env Env) evaluate(ctx context.Context, p *project, base, run store.Run, changed []string) (gateResult, error) {
	g := gateResult{base: base, run: run}
	var err error
	if g.quarantined, err = p.db.Quarantined(ctx, false); err != nil {
		return g, err
	}
	if base.Fidelity == "none" {
		g.notes = append(g.notes, protocolInactive)
		return g, nil
	}
	if run.Status != testrun.StatusCompleted {
		g.blocking = []string{testrun.SuiteID}
		g.notes = append(g.notes, fmt.Sprintf("run %d ended with status %s; see %s", run.ID, run.Status, run.OutputPath))
		return g, nil
	}
	baseRes, err := p.db.EffectiveResults(ctx, base.ID)
	if err != nil {
		return g, err
	}
	if g.runResults, err = p.db.EffectiveResults(ctx, run.ID); err != nil {
		return g, err
	}
	drop := map[string]bool{}
	for _, q := range g.quarantined {
		drop[q.TestID] = true
	}
	baseMap := testrun.Without(statusMap(baseRes), drop)
	g.classes = testrun.Compare(baseMap, testrun.Without(statusMap(g.runResults), drop))
	if err := g.blamePassedEarlier(ctx, p); err != nil {
		return g, err
	}
	g.blocking = testrun.Blocking(g.classes)
	if base.Fidelity == "suite" && baseMap[testrun.SuiteID] == "fail" && !contains(g.blocking, testrun.SuiteID) {
		g.blocking = append(g.blocking, testrun.SuiteID)
		g.notes = append(g.notes, reducedFidelity)
	}
	if p.cfg.Tests.CoverageFormat != "" {
		if err := g.compareCoverage(ctx, p, changed); err != nil {
			return g, err
		}
	}
	return g, nil
}

// blamePassedEarlier turns a pre-existing failure into a regression when the
// test passed in an earlier run of the same task. A baseline can fail a test
// only because its clean worktree lacks an ignored file (.env, vendor), so
// "failed at baseline" alone is no proof the task did not break it (§8.5).
func (g *gateResult) blamePassedEarlier(ctx context.Context, p *project) error {
	pre := g.classes[testrun.PreExisting]
	if len(pre) == 0 || g.run.TaskID == 0 {
		return nil
	}
	passed, err := p.db.PassedEarlier(ctx, g.run.TaskID, g.run.ID)
	if err != nil {
		return err
	}
	var keep []string
	for _, id := range pre {
		run, ok := passed[id]
		if !ok {
			keep = append(keep, id)
			continue
		}
		g.classes[testrun.Regression] = append(g.classes[testrun.Regression], id)
		g.notes = append(g.notes, fmt.Sprintf("%s failed at baseline run %d but passed in run %d of this task, so it counts as a regression "+
			"(the clean baseline worktree may lack an ignored file; see [tests].setup)", id, g.base.ID, run))
	}
	g.classes[testrun.PreExisting] = keep
	sort.Strings(g.classes[testrun.Regression])
	return nil
}

func (g *gateResult) compareCoverage(ctx context.Context, p *project, changed []string) error {
	baseCov, err := p.db.Coverage(ctx, g.base.ID)
	if err != nil {
		return err
	}
	runCov, err := p.db.Coverage(ctx, g.run.ID)
	if err != nil {
		return err
	}
	if baseCov == nil || runCov == nil {
		g.blocking = append(g.blocking, "coverage:(unavailable)")
		g.notes = append(g.notes, fmt.Sprintf("coverage is configured but run %d or baseline %d recorded none; the coverage rule cannot be checked", g.run.ID, g.base.ID))
		return nil
	}
	for _, path := range changed {
		now, ok := runCov[path]
		if !ok {
			continue
		}
		before, existed := baseCov[path]
		row := coverageRow{Path: path, Gate: rate(now), New: !existed}
		if existed {
			row.Base = rate(before)
			if rate(now) < rate(before) {
				row.Dropped = true
				g.blocking = append(g.blocking, "coverage:"+path)
			}
		}
		g.coverage = append(g.coverage, row)
	}
	return nil
}

// print writes the class counts and the blocking rows.
func (g gateResult) print(w io.Writer, label string) {
	if g.classes != nil {
		var counts []string
		for _, c := range testrun.Classes {
			if n := len(g.classes[c]); n > 0 {
				counts = append(counts, fmt.Sprintf("%d %s", n, c))
			}
		}
		fmt.Fprintf(w, "%s run %d at %s vs baseline run %d at %s: %s\n", label, g.run.ID, shortSHA(g.run.SHA), g.base.ID, shortSHA(g.base.SHA), strings.Join(counts, ", "))
		for _, c := range append(append([]testrun.Class{}, testrun.BlockingClasses...), testrun.Fixed) {
			if ids := g.classes[c]; len(ids) > 0 {
				fmt.Fprintf(w, "  %s: %s\n", c, strings.Join(ids, ", "))
			}
		}
	}
	for _, c := range g.coverage {
		switch {
		case c.Dropped:
			fmt.Fprintf(w, "  coverage dropped: %s %s -> %s\n", c.Path, pct(c.Base), pct(c.Gate))
		case c.New:
			fmt.Fprintf(w, "  coverage of new file %s: %s\n", c.Path, pct(c.Gate))
		}
	}
	for _, n := range g.notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
	if len(g.quarantined) > 0 {
		ids := make([]string, len(g.quarantined))
		for i, q := range g.quarantined {
			ids[i] = q.TestID
		}
		fmt.Fprintf(w, "  quarantined (ignored): %s\n", strings.Join(ids, ", "))
	}
	if len(g.blocking) > 0 {
		fmt.Fprintf(w, "BLOCKED by: %s\n", strings.Join(g.blocking, ", "))
	}
}

// changedPaths lists files changed between base and HEAD in wt.
func changedPaths(ctx context.Context, wt, base string) ([]string, error) {
	files, err := changedFiles(ctx, wt, base)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		if !strings.HasPrefix(f.Status, "D") {
			out = append(out, f.Path)
		}
	}
	sort.Strings(out)
	return out, nil
}

// requireClean refuses when wt has uncommitted changes, since a run must
// describe a commit.
func requireClean(ctx context.Context, wt, what string) (string, error) {
	if _, err := os.Stat(wt); err != nil {
		return "", fail(ExitState, "worktree %s is missing: %v", wt, err)
	}
	dirty, err := gitx.Run(ctx, wt, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if dirty != "" {
		return "", fail(ExitState, "worktree %s has uncommitted changes; commit them before %s:\n%s", wt, what, dirty)
	}
	return gitx.Run(ctx, wt, "rev-parse", "HEAD")
}

// workingStages are the stages in which a task has a worktree and a baseline.
var workingStages = []string{store.StageInProgress, store.StageQA, store.StageEscalated, store.StageNeedsRebase}

func (env Env) cmdTest(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "quarantine" {
		return env.cmdQuarantine(ctx, args[1:])
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	kind := fs.String("kind", "gate", "gate, qa or baseline")
	pos, err := parseArgs(fs, args, 1)
	if err != nil {
		return err
	}
	if !contains([]string{"gate", "qa", "baseline"}, *kind) {
		return fail(ExitUsage, "--kind %q must be gate, qa or baseline", *kind)
	}
	p, t, lock, err := env.lockedTask(ctx, pos[0], "test")
	if err != nil {
		return err
	}
	defer p.close()
	defer lock.Release()
	if !contains(workingStages, t.Stage) {
		return fail(ExitState, "task %s is at %q; test needs one of %s", t.Slug, t.Stage, strings.Join(workingStages, ", "))
	}
	if p.cfg.Tests.Format == "none" {
		return fail(ExitConfig, "no test suite is configured ([tests].format = \"none\" in %s)", p.cfgPath)
	}
	head, err := requireClean(ctx, t.Worktree, "running the suite")
	if err != nil {
		return err
	}
	if *kind == "baseline" && head != t.BaseSHA {
		return fail(ExitState, "a baseline must run at the base commit %s, but HEAD is %s", shortSHA(t.BaseSHA), shortSHA(head))
	}
	if err := env.reapAbandoned(ctx, p, t); err != nil {
		return err
	}
	var run store.Run
	if *kind == "baseline" {
		// A baseline can quarantine tests project-wide, so it runs in a clean
		// scratch worktree, never in the task worktree with its ignored files.
		fmt.Fprintf(env.Stdout, "baseline: running the test suite at %s in a clean scratch worktree ...\n", shortSHA(head))
		run, err = env.baselineInScratch(ctx, p, t, t.BaseSHA)
	} else {
		fmt.Fprintf(env.Stdout, "%s: running the test suite at %s ...\n", *kind, shortSHA(head))
		run, err = env.runSuite(ctx, p, t, *kind, head, t.Worktree)
	}
	if err != nil {
		return err
	}
	if run.Status == testrun.StatusCancelled {
		return fail(ExitInterrupted, "interrupted: %s run %d was cancelled", *kind, run.ID)
	}
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
	g.print(env.Stdout, *kind)
	if len(g.blocking) > 0 {
		return fail(ExitGate, "%d blocking row(s) against the baseline: %s", len(g.blocking), strings.Join(g.blocking, ", "))
	}
	return nil
}

func (env Env) cmdQuarantine(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fail(ExitUsage, "usage: test quarantine list [--all] | test quarantine release <test>")
	}
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("test quarantine list", flag.ContinueOnError)
		all := fs.Bool("all", false, "include released tests")
		if _, err := parseArgs(fs, args[1:], 0); err != nil {
			return err
		}
		qs, err := p.db.Quarantined(ctx, *all)
		if err != nil {
			return err
		}
		if len(qs) == 0 {
			fmt.Fprintln(env.Stdout, "no quarantined tests")
		}
		for _, q := range qs {
			state := "quarantined " + q.QuarantinedAt
			if q.ReleasedAt != "" {
				state = "released " + q.ReleasedAt
			}
			fmt.Fprintf(env.Stdout, "%s\trun #%d\t%s\t%s\n", q.TestID, q.DetectedRunID, state, q.Reason)
		}
		return nil
	case "release":
		fs := flag.NewFlagSet("test quarantine release", flag.ContinueOnError)
		pos, err := parseArgs(fs, args[1:], 1)
		if err != nil {
			return err
		}
		if err := p.db.ReleaseQuarantine(ctx, pos[0]); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "released %s from quarantine; it counts in gate decisions again\n", pos[0])
		return nil
	}
	return fail(ExitUsage, "unknown quarantine command %q: use list or release", args[0])
}
