package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/coverage"
	"github.com/kennethsolomon/shoalctl/internal/diffx"
	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
)

type reportInput struct {
	task        store.Task
	status      string // done | merged
	sum         summaryFile
	head        string
	commits     []string
	files       []changedFile
	base        store.Run
	final       store.Run
	finalLabel  string // Final (done) or Gate (merge)
	baseResults []store.Result
	classes     map[testrun.Class][]string
	blocking    []string
	quarantined []store.Quarantine
	coverage    []coverageRow
	ver         verification
	rounds      []store.QARound
	maxRounds   int
	proofs      []store.Proof
	scope       []itemEval
	untested    []string
	coverageOn  bool
	overrides   []store.Override
	notes       []string // extra Verified lines: large diff, backport, tag target
	date        string
}

const (
	reducedFidelity  = "Reduced fidelity: whole-suite pass/fail only. A failing baseline hides any new failure."
	protocolInactive = "Zero-regression protocol inactive: [tests].format = \"none\", so no tests ran."
	noCoverage       = "coverage not configured: shoal cannot tell which of these lines any test runs."
)

// gatherReport collects everything a report shows for task t at head, from
// base (the baseline run) to final (done's final run or merge's gate run).
// baseSHA is where the task's commits start.
func (env Env) gatherReport(ctx context.Context, p *project, t store.Task, sm summaryFile, head, baseSHA string, base, final store.Run) (reportInput, error) {
	in := reportInput{task: t, status: store.StageDone, sum: sm, head: head, base: base, final: final, finalLabel: "Final",
		maxRounds: p.cfg.Lanes.QAMaxRounds, coverageOn: p.cfg.Tests.CoverageFormat != "", date: time.Now().UTC().Format("2006-01-02")}
	wt := t.Worktree
	commitsOut, err := gitx.Run(ctx, wt, "rev-list", "--reverse", baseSHA+"..HEAD")
	if err != nil {
		return in, err
	}
	in.commits = strings.Fields(commitsOut)
	if in.files, err = changedFiles(ctx, wt, baseSHA); err != nil {
		return in, err
	}
	if in.baseResults, err = p.db.EffectiveResults(ctx, base.ID); err != nil {
		return in, err
	}
	changed, err := changedPaths(ctx, wt, baseSHA)
	if err != nil {
		return in, err
	}
	g, err := env.evaluate(ctx, p, base, final, changed)
	if err != nil {
		return in, err
	}
	in.classes, in.blocking, in.quarantined, in.coverage = g.classes, g.blocking, g.quarantined, g.coverage
	if in.rounds, err = p.db.QARounds(ctx, t.ID); err != nil {
		return in, err
	}
	if in.proofs, err = p.db.Proofs(ctx, t.ID); err != nil {
		return in, err
	}
	items, err := p.db.ScopeItems(ctx, t.ID)
	if err != nil {
		return in, err
	}
	var runMap map[string]string
	if final.Status == testrun.StatusCompleted {
		runMap = statusMap(g.runResults)
	}
	taskBase, err := baselineResults(ctx, p, t)
	if err != nil {
		return in, err
	}
	in.scope = evalScope(items, runMap, taskBase)
	if in.overrides, err = p.db.Overrides(ctx, t.ID); err != nil {
		return in, err
	}
	if in.untested, err = untestedAreas(ctx, p, wt, baseSHA, final); err != nil {
		return in, err
	}
	in.ver = verification{Baseline: summarizeRun(base), Final: summarizeRun(final), Classes: map[string]int{}}
	for c, ids := range in.classes {
		in.ver.Classes[string(c)] = len(ids)
	}
	return in, nil
}

// untestedAreas lists changed lines no test executed (§8.8): from coverage
// when the final run recorded it, otherwise every changed non-test file.
func untestedAreas(ctx context.Context, p *project, wt, baseSHA string, final store.Run) ([]string, error) {
	cov, err := p.db.Coverage(ctx, final.ID)
	if err != nil {
		return nil, err
	}
	diff, err := gitx.Run(ctx, wt, "diff", "-U0", "--no-color", "--no-ext-diff", "--src-prefix=a/", "--dst-prefix=b/", baseSHA+"...HEAD")
	if err != nil {
		return nil, err
	}
	lines, err := diffx.ChangedLines(diff)
	if err != nil {
		return nil, err
	}
	paths, err := changedPaths(ctx, wt, baseSHA)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, path := range paths {
		if diffx.IsTestFile(path) {
			continue
		}
		if cov == nil {
			out = append(out, "`"+path+"`")
			continue
		}
		f, ok := cov[path]
		if !ok {
			out = append(out, "`"+path+"` (not in the coverage report)")
			continue
		}
		uncovered, err := coverage.ParseRanges(f.Uncovered)
		if err != nil {
			return nil, fmt.Errorf("coverage of %s in run %d: %w", path, final.ID, err)
		}
		miss := map[int]bool{}
		for _, n := range uncovered {
			miss[n] = true
		}
		var hit []int
		for _, n := range lines[path] {
			if miss[n] {
				hit = append(hit, n)
			}
		}
		for _, r := range strings.Split(coverage.Ranges(hit), ",") {
			if r != "" {
				out = append(out, "`"+path+":"+r+"`")
			}
		}
	}
	return out, nil
}

// renderReport renders the task report (§12).
func renderReport(in reportInput) string {
	var b strings.Builder
	t := in.task
	status := in.status
	if status == "" {
		status = store.StageDone
	}
	finalLabel := in.finalLabel
	if finalLabel == "" {
		finalLabel = "Final"
	}
	short := make([]string, len(in.commits))
	for i, c := range in.commits {
		short[i] = shortSHA(c)
	}
	fmt.Fprintf(&b, "---\ntask: %s\nlane: %s\nstatus: %s\nbranch: %s\nbase: %s@%s\ncommits: [%s]\ndate: %s\n---\n",
		t.Slug, t.Lane, status, t.Branch, t.BaseRef, shortSHA(t.BaseSHA), strings.Join(short, ", "), in.date)
	fmt.Fprintf(&b, "# %s\n\n", t.Title)
	if in.base.Fidelity == "none" {
		b.WriteString("> " + protocolInactive + "\n\n")
	}

	fmt.Fprintf(&b, "## Asked\n%s\n\n", t.Ask)

	fmt.Fprintf(&b, "## Decision\n%s\n", in.sum.Decision)
	for i, a := range in.sum.AlternativesRejected {
		if i == 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- Rejected: %s (%s)\n", a.Option, a.Why)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "## Why\n%s\n\n", in.sum.Why)

	b.WriteString("## Changed\n")
	notes := map[string]string{}
	for _, c := range in.sum.Changed {
		notes[c.Path] = c.Note
	}
	for _, f := range in.files {
		path := "`" + f.Path + "`"
		if f.From != "" {
			path = "`" + f.From + "` -> `" + f.Path + "`"
		}
		line := fmt.Sprintf("- %s %s", f.Status, path)
		if n := notes[f.Path]; n != "" {
			line += " - " + n
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")

	b.WriteString("## Verified\n")
	fmt.Fprintf(&b, "- Baseline: %s\n", runLine(in.base))
	fmt.Fprintf(&b, "- %s: %s\n", finalLabel, runLine(in.final))
	fmt.Fprintf(&b, "- Lint: %s\n- Build: %s\n", in.ver.Lint, in.ver.Build)
	b.WriteString("- " + qaLine(in.rounds, in.maxRounds) + "\n")
	for _, pr := range in.proofs {
		b.WriteString("- " + proofLine(pr) + "\n")
	}
	if t.Lane != "feature" && len(in.proofs) == 0 {
		b.WriteString("- Regression proof: none recorded\n")
	}
	for _, n := range in.notes {
		b.WriteString("- " + n + "\n")
	}
	b.WriteString("\n")

	b.WriteString("## Regression check\n")
	switch {
	case in.base.Fidelity == "none":
		b.WriteString("Not computed: no test suite is configured.\n")
	case in.classes == nil:
		fmt.Fprintf(&b, "Not computed: the %s run ended with status %s.\n", strings.ToLower(finalLabel), in.final.Status)
	default:
		var counts []string
		for _, c := range testrun.Classes {
			counts = append(counts, fmt.Sprintf("%d %s", len(in.classes[c]), c))
		}
		b.WriteString("- " + strings.Join(counts, ", ") + "\n")
		for _, c := range append(append([]testrun.Class{}, testrun.BlockingClasses...), testrun.Fixed) {
			if ids := in.classes[c]; len(ids) > 0 {
				fmt.Fprintf(&b, "- %s: %s\n", c, codeList(ids))
			}
		}
	}
	for _, c := range in.coverage {
		switch {
		case c.Dropped:
			fmt.Fprintf(&b, "- Coverage dropped: `%s` %s -> %s\n", c.Path, pct(c.Base), pct(c.Gate))
		case c.New:
			fmt.Fprintf(&b, "- Coverage of new file `%s`: %s\n", c.Path, pct(c.Gate))
		}
	}
	if status == store.StageDone && len(in.blocking) > 0 {
		fmt.Fprintf(&b, "- Would block the merge gate: %s\n", codeList(in.blocking))
	}
	if len(in.quarantined) > 0 {
		var qs []string
		for _, q := range in.quarantined {
			qs = append(qs, fmt.Sprintf("`%s` (flaky at baseline, run #%d)", q.TestID, q.DetectedRunID))
		}
		b.WriteString("- Quarantined tests: " + strings.Join(qs, ", ") + "\n")
	}
	if len(in.overrides) > 0 {
		var accepted []string
		for _, o := range in.overrides {
			accepted = append(accepted, fmt.Sprintf("`%s`: %q (by %s)", o.Target, o.Reason, o.AcceptedBy))
		}
		b.WriteString("- Accepted regressions: " + strings.Join(accepted, ", ") + "\n")
	}
	b.WriteString("\n")

	b.WriteString("## Pre-existing failures\n")
	var failing []string
	for _, r := range in.baseResults {
		if r.Status == "fail" {
			failing = append(failing, r.TestID)
		}
	}
	if len(failing) == 0 {
		b.WriteString("None at baseline.\n\n")
	} else {
		fmt.Fprintf(&b, "Failing at baseline %s, not blamed on this task: %s\n\n", shortSHA(in.base.SHA), codeList(failing))
	}

	b.WriteString("## Scope\n")
	var waived []string
	if len(in.scope) == 0 {
		b.WriteString("No scope items.\n")
	} else {
		b.WriteString("| Item | Rows | Green | Waived |\n|------|------|-------|--------|\n")
		for _, it := range in.scope {
			green, w := 0, 0
			var why []string
			for _, r := range it.Rows {
				switch {
				case r.Green:
					green++
				case r.Waived:
					w++
					why = append(why, fmt.Sprintf("`%s`: %s", r.Category, r.WaiveReason))
					waived = append(waived, fmt.Sprintf("scope item %q, `%s` waived: %s", it.Text, r.Category, r.WaiveReason))
				}
			}
			cell := fmt.Sprint(w)
			if len(why) > 0 {
				cell += " (" + strings.Join(why, "; ") + ")"
			}
			fmt.Fprintf(&b, "| %s %s | %d | %d | %s |\n", it.Source, strings.ReplaceAll(it.Text, "|", "/"), len(it.Rows), green, cell)
		}
	}
	b.WriteString("\n")

	b.WriteString("## Untested areas\n")
	if in.base.Fidelity == "none" {
		b.WriteString("- Everything: no test suite is configured.\n")
	}
	for _, u := range in.untested {
		b.WriteString("- " + u + "\n")
	}
	if !in.coverageOn && len(in.untested) > 0 {
		b.WriteString("- Note: " + noCoverage + "\n")
	}
	for _, w := range waived {
		b.WriteString("- " + w + "\n")
	}
	for _, o := range in.overrides {
		fmt.Fprintf(&b, "- Accepted regression `%s`: %s\n", o.Target, o.Reason)
	}
	if len(in.untested)+len(waived)+len(in.overrides) == 0 && in.base.Fidelity != "none" {
		b.WriteString("None found.\n")
	}
	b.WriteString("\n")

	b.WriteString("## Risks / Follow-ups\n")
	for _, r := range in.sum.Risks {
		fmt.Fprintf(&b, "- Risk: %s\n", r)
	}
	for _, f := range in.sum.Followups {
		fmt.Fprintf(&b, "- Follow-up: %s\n", f)
	}
	for _, pr := range in.proofs {
		if pr.TestChanged {
			fmt.Fprintf(&b, "- Review: `%s` changed after its red run; a weakened test can turn green without a fix\n", pr.TestFile)
		}
	}
	if len(in.sum.Risks)+len(in.sum.Followups) == 0 {
		b.WriteString("None recorded.\n")
	}
	b.WriteString("\n")

	b.WriteString("## Fidelity and limits\n")
	switch in.base.Fidelity {
	case "suite":
		b.WriteString("- " + reducedFidelity + "\n")
	case "none":
		b.WriteString("- " + protocolInactive + "\n")
	}
	b.WriteString("- Zero regression covers only what the test suite can see; see Untested areas.\n")
	return b.String()
}

func qaLine(rounds []store.QARound, maxRounds int) string {
	if len(rounds) == 0 {
		return "QA rounds: none run"
	}
	var failed []string
	for _, r := range rounds {
		if !r.Passed {
			var fs []string
			_ = json.Unmarshal([]byte(r.Failures), &fs)
			failed = append(failed, fmt.Sprintf("round %d failed on %s", r.Round, strings.Join(fs, "; ")))
		}
	}
	line := fmt.Sprintf("QA rounds: %d/%d", len(rounds), maxRounds)
	if len(failed) > 0 {
		line += " - " + strings.Join(failed, ", ")
	}
	return line
}

func proofLine(pr store.Proof) string {
	if pr.RedStatus == "not-reproducible" {
		line := fmt.Sprintf("Regression proof: `%s` (`%s`) already passed at %s, so the bug was not reproducible on this base (backport)",
			pr.TestFile, pr.TestID, shortSHA(pr.RedSHA))
		if pr.GreenSHA != "" {
			line += fmt.Sprintf("; green at %s", shortSHA(pr.GreenSHA))
		}
		return line
	}
	line := fmt.Sprintf("Regression proof: `%s` (`%s`) red at %s", pr.TestFile, pr.TestID, shortSHA(pr.RedSHA))
	if pr.GreenSHA != "" {
		line += fmt.Sprintf(", green at %s", shortSHA(pr.GreenSHA))
	} else {
		line += ", no green run"
	}
	if pr.TestChanged {
		line += " (test changed after red: review it)"
	}
	return line
}

func runLine(r store.Run) string {
	if r.Fidelity == "none" {
		return "no suite configured"
	}
	if r.Status != testrun.StatusCompleted {
		return fmt.Sprintf("run #%d at `%s` %s, see %s", r.ID, shortSHA(r.SHA), r.Status, r.OutputPath)
	}
	return fmt.Sprintf("`%s`: %d pass, %d fail, %d skip (%s, run #%d)", shortSHA(r.SHA), r.Passed, r.Failed, r.Skipped, r.Fidelity, r.ID)
}

func codeList(ids []string) string {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "`" + id + "`"
	}
	return strings.Join(quoted, ", ")
}
