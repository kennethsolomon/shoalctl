package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/diffx"
	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
)

type alternative struct {
	Option string `json:"option"`
	Why    string `json:"why"`
}

type changedNote struct {
	Path string `json:"path"`
	Note string `json:"note"`
}

type summaryFile struct {
	Decision             string        `json:"decision"`
	AlternativesRejected []alternative `json:"alternatives_rejected"`
	Why                  string        `json:"why"`
	Risks                []string      `json:"risks"`
	Followups            []string      `json:"followups"`
	Changed              []changedNote `json:"changed"`
}

const summaryShape = `{"decision": "...", "alternatives_rejected": [{"option": "...", "why": "..."}], "why": "...", "risks": ["..."], "followups": ["..."], "changed": [{"path": "...", "note": "one line"}]}`

type changedFile struct {
	Status string `json:"status"`
	Path   string `json:"path"`
	From   string `json:"from,omitempty"`
}

type runSummary struct {
	RunID    int64  `json:"run_id"`
	SHA      string `json:"sha"`
	Status   string `json:"status"`
	Fidelity string `json:"fidelity"`
	Passed   int    `json:"passed"`
	Failed   int    `json:"failed"`
	Skipped  int    `json:"skipped"`
	Output   string `json:"output"`
}

type verification struct {
	Baseline runSummary     `json:"baseline"`
	Final    runSummary     `json:"final"`
	Classes  map[string]int `json:"classes"`
	Lint     string         `json:"lint"`
	Build    string         `json:"build"`
}

func (env Env) cmdDone(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("done", flag.ContinueOnError)
	force := fs.Bool("force", false, "run even though the task's claude session is still running")
	largeDiff := fs.Bool("accept-large-diff", false, "accept a hotfix over [hotfix].max_diff_lines (needs --reason)")
	reason := fs.String("reason", "", "why the large hotfix diff is needed; goes into the report")
	pos, err := parseArgs(fs, args, 1)
	if err != nil {
		return err
	}
	if *largeDiff && strings.TrimSpace(*reason) == "" {
		return fail(ExitUsage, "--accept-large-diff needs --reason \"<text>\"")
	}
	if !*largeDiff && *reason != "" {
		return fail(ExitUsage, "--reason goes with --accept-large-diff")
	}
	p, t, lock, err := env.lockedTask(ctx, pos[0], "done")
	if err != nil {
		return err
	}
	defer p.close()
	defer lock.Release()
	from := []string{store.StageInProgress, store.StageQA, store.StageNeedsRebase}
	if !contains(from, t.Stage) {
		return fail(ExitState, "task %s is at %q; done needs in-progress, qa or needs-rebase", t.Slug, t.Stage)
	}
	wt := t.Worktree
	if _, err := os.Stat(wt); err != nil {
		return fail(ExitState, "worktree %s of task %s is missing: %v", wt, t.Slug, err)
	}
	if err := waitIndexLock(ctx, wt); err != nil {
		return err
	}
	if dirty, err := gitx.Run(ctx, wt, "status", "--porcelain"); err != nil {
		return err
	} else if dirty != "" {
		return fail(ExitState, "worktree %s has uncommitted changes; commit them first:\n%s", wt, dirty)
	}
	if n, err := gitx.Run(ctx, wt, "rev-list", "--count", t.BaseSHA+"..HEAD"); err != nil {
		return err
	} else if n == "0" {
		return fail(ExitState, "branch %s has no commits after %s", t.Branch, shortSHA(t.BaseSHA))
	}
	sm, err := readSummary(filepath.Join(wt, ".shoal", "summary.json"))
	if err != nil {
		return err
	}
	if !*force {
		if err := env.requireSessionEnded(ctx, t); err != nil {
			return err
		}
	}
	head, err := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	// Refusals that need no test run come first (§8.3, §6.3, §7.4).
	var refusals []string
	if prob, err := proofProblem(ctx, p, t); err != nil {
		return err
	} else if prob != "" {
		refusals = append(refusals, prob)
	}
	diffLines := 0
	if t.Lane == "hotfix" {
		if diffLines, err = productionDiffLines(ctx, wt, t.BaseSHA); err != nil {
			return err
		}
		if diffLines > p.cfg.Hotfix.MaxDiffLines && !*largeDiff {
			refusals = append(refusals, fmt.Sprintf("hotfix diff is %d added + removed lines outside test files, over [hotfix].max_diff_lines = %d; "+
				"shrink it to the minimal fix, or pass --accept-large-diff --reason \"<why>\"", diffLines, p.cfg.Hotfix.MaxDiffLines))
		}
	}
	if prob, err := contractProblem(ctx, p, t); err != nil {
		return err
	} else if prob != "" {
		refusals = append(refusals, prob)
	}
	if len(refusals) > 0 {
		return fail(ExitDoneRefused, "refused:\n  %s", strings.Join(refusals, "\n  "))
	}

	if err := env.reapAbandoned(ctx, p, t); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "final: running the test suite at %s ...\n", shortSHA(head))
	final, err := env.runSuite(ctx, p, t, "final", head, wt)
	if err != nil {
		return err
	}
	if final.Status == testrun.StatusCancelled {
		return fail(ExitInterrupted, "interrupted: final run %d was cancelled and its processes stopped; the task is still %s, run shoalctl done %s again", final.ID, t.Stage, t.Slug)
	}
	lint := env.step(ctx, p, wt, "lint", p.cfg.Stack.Lint, p.runDir(final.ID))
	build := env.step(ctx, p, wt, "build", p.cfg.Stack.Build, p.runDir(final.ID))
	if ctx.Err() != nil {
		return fail(ExitInterrupted, "interrupted during lint or build; the task is still %s, run shoalctl done %s again", t.Stage, t.Slug)
	}
	if err := requireUnchanged(ctx, wt, head, t.Slug); err != nil {
		return err
	}
	base, err := p.db.GetRun(ctx, t.BaselineRunID)
	if err != nil {
		return err
	}
	in, err := env.gatherReport(ctx, p, t, sm, head, t.BaseSHA, base, final)
	if err != nil {
		return err
	}
	if open := scopeOpen(in.scope); len(open) > 0 {
		return fail(ExitDoneRefused, "refused: %d scope row(s) are not green or waived (final run %d):\n  %s\nCover them, or a human waives them with shoalctl scope review %s",
			len(open), final.ID, strings.Join(open, "\n  "), t.Slug)
	}
	in.ver.Lint, in.ver.Build = lint, build
	if *largeDiff {
		in.notes = append(in.notes, largeDiffNote(diffLines, p.cfg.Hotfix.MaxDiffLines, strings.TrimSpace(*reason)))
	}

	var backport *store.NewTask
	if t.Lane == "hotfix" {
		if strings.HasPrefix(t.BaseRef, "refs/tags/") {
			in.notes = append(in.notes, fmt.Sprintf("Production ref is the tag %s: the merge queue has no target. The branch %s is ready; tagging and releasing are human actions",
				strings.TrimPrefix(t.BaseRef, "refs/tags/"), t.Branch))
		}
		kids, err := p.db.Children(ctx, t.ID)
		if err != nil {
			return err
		}
		switch {
		case len(kids) > 0:
			in.notes = append(in.notes, "Backport: task "+kids[0].Slug)
		case p.cfg.Hotfix.Backport:
			nt := backportTask(t, p.cfg.Merge.Base, in.commits, in.proofs)
			backport = &nt
		}
	}
	report := renderReport(in)
	reportPath := filepath.Join(p.dir, "reports", t.Slug+".md")
	row := summaryRow(t.ID, sm, in.ver, in.commits, in.files, head, reportPath)
	var backportSlug string
	words := ""
	if backport != nil {
		words = slugWords(backport.Title)
	}
	t, err = p.db.Update(ctx, t, from, "stage = ?, final_run_id = ?, closed_at = ?, large_diff_reason = ?",
		[]any{store.StageDone, final.ID, nowUTC(), nullableString(strings.TrimSpace(*reason))}, func(tx *sql.Tx) error {
			if err := store.InsertSummary(ctx, tx, row); err != nil {
				return err
			}
			if backport == nil {
				return nil
			}
			id, err := store.CreateTaskTx(ctx, tx, *backport, func(id int64) string { return fmt.Sprintf("%d-%s", id, words) })
			backportSlug = fmt.Sprintf("%d-%s", id, words)
			return err
		})
	if err != nil {
		return err
	}
	if backportSlug != "" {
		in.notes = append(in.notes, "Backport: task "+backportSlug+" created (bugfix lane, not started)")
		report = renderReport(in)
	}
	if err := env.writeReport(p, t.Slug, report); err != nil {
		return err
	}
	if backportSlug != "" {
		fmt.Fprintf(env.Stdout, "backport task %s created; start it with shoalctl start %s\n", backportSlug, backportSlug)
	}
	fmt.Fprintln(env.Stdout, reportPath)
	return nil
}

// writeReport writes the rendered report to the local sink and, when
// configured, the notes folder.
func (env Env) writeReport(p *project, slug, report string) error {
	if err := writePrivate(filepath.Join(p.dir, "reports", slug+".md"), []byte(report)); err != nil {
		return err
	}
	if p.cfg.Reports.Sink != "local" {
		notes := filepath.Join(p.cfg.Reports.NotesPath, slug+".md")
		if err := writeFileAtomic(notes, []byte(report), 0o600); err != nil {
			return fail(ExitConfig, "copy report to [reports].notes_path: %v", err)
		}
	}
	return nil
}

func largeDiffNote(lines, budget int, reason string) string {
	return fmt.Sprintf("Large hotfix diff accepted: %d lines over the %d-line budget: %s", lines, budget, reason)
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// productionDiffLines counts added + removed lines outside test files since
// base, for the hotfix minimal-diff rule (§6.3).
func productionDiffLines(ctx context.Context, wt, base string) (int, error) {
	out, err := gitx.Run(ctx, wt, "diff", "--numstat", "-z", "--no-ext-diff", base+"...HEAD")
	if err != nil {
		return 0, err
	}
	stats, err := diffx.ParseNumstatZ(out)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range stats {
		if !diffx.IsTestFile(s.Path) {
			n += s.Added + s.Removed
		}
	}
	return n, nil
}

// backportTask is the bugfix task a finished hotfix leaves behind (§6.3).
func backportTask(t store.Task, base string, commits []string, proofs []store.Proof) store.NewTask {
	title := "Backport: " + t.Title
	if r := []rune(title); len(r) > 120 {
		title = string(r[:117]) + "..."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Backport hotfix %s (%s) to %s.\n\n", t.Slug, t.Title, base)
	fmt.Fprintf(&b, "1. Cherry-pick the hotfix commits, oldest first, onto %s:\n", base)
	for _, c := range commits {
		fmt.Fprintf(&b, "   - %s\n", c)
	}
	b.WriteString("2. The hotfix's regression test must exist on the base branch and pass")
	for i, pr := range proofs {
		if i == 0 {
			b.WriteString(":\n")
		}
		fmt.Fprintf(&b, "   - %s (%s)\n", pr.TestID, pr.TestFile)
	}
	b.WriteString("\n3. Record red first with shoalctl repro red. If the test already passes on the base branch before the cherry-pick " +
		"(the bug never existed there), repro red records not-reproducible and the report says why.\n")
	return store.NewTask{Lane: "bugfix", Title: title, Ask: b.String(), ParentID: t.ID}
}

// indexLockWait bounds how long done waits for a git command in the
// worktree (the agent committing) to release index.lock.
var indexLockWait = 10 * time.Second

func waitIndexLock(ctx context.Context, wt string) error {
	path, err := gitx.Run(ctx, wt, "rev-parse", "--git-path", "index.lock")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(wt, path)
	}
	deadline := time.Now().Add(indexLockWait)
	for {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("check %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return fail(ExitDoneRefused, "git index.lock exists at %s, so a git command (likely the agent committing) is running in %s; done waited %s. "+
				"Retry once it finishes. If no git process is running, the lock is stale: remove it and retry. --force does not skip this check",
				path, wt, indexLockWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// requireSessionEnded refuses while the task's claude session is still
// running: its tmux window exists with a live pane (claude is the pane's
// process, and remain-on-exit keeps the pane, dead, after claude exits).
func (env Env) requireSessionEnded(ctx context.Context, t store.Task) error {
	if t.TmuxWindow == "" {
		return nil
	}
	out, err := env.tmux(ctx, "list-panes", "-t", t.TmuxWindow, "-F", "#{pane_dead}")
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "no server running") || strings.Contains(msg, "can't find") || strings.Contains(msg, "error connecting") {
			return nil // the window or the whole tmux server is gone
		}
		return fmt.Errorf("check the claude session of task %s: %w", t.Slug, err)
	}
	for _, dead := range strings.Fields(out) {
		if dead == "0" {
			return fail(ExitDoneRefused, "the claude session in tmux window %s (session %s) is still running, so the agent may still be editing or committing. "+
				"Exit claude in that window (/exit), then run shoalctl done %s again. If the agent has finished, run shoalctl done --force %s",
				t.TmuxWindow, t.TmuxSession, t.Slug, t.Slug)
		}
	}
	return nil
}

// requireUnchanged refuses to record a task whose branch or worktree changed
// while the final suite ran, since the results would not describe HEAD.
func requireUnchanged(ctx context.Context, wt, head, slug string) error {
	now, err := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	dirty, err := gitx.Run(ctx, wt, "status", "--porcelain")
	if err != nil {
		return err
	}
	if now != head || dirty != "" {
		return fail(ExitDoneRefused, "the branch or worktree changed while done ran (HEAD %s, now %s; uncommitted: %q); nothing was recorded. "+
			"Make sure the agent has finished, then run shoalctl done %s again", shortSHA(head), shortSHA(now), dirty, slug)
	}
	return nil
}

func readSummary(path string) (summaryFile, error) {
	var sm summaryFile
	bad := func(format string, a ...any) error {
		return fail(ExitState, "%s: %s\nexpected shape: %s", path, fmt.Sprintf(format, a...), summaryShape)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return sm, bad("missing; the session writes it before done")
	}
	if err != nil {
		return sm, fmt.Errorf("read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sm); err != nil {
		return sm, bad("invalid: %v", err)
	}
	if dec.More() {
		return sm, bad("invalid: trailing data after the JSON object")
	}
	if strings.TrimSpace(sm.Decision) == "" || strings.TrimSpace(sm.Why) == "" {
		return sm, bad("decision and why must not be empty")
	}
	return sm, nil
}

// step runs [stack].lint or [stack].build; the result is informational.
func (env Env) step(ctx context.Context, p *project, wt, name, command, runDir string) string {
	if strings.TrimSpace(command) == "" {
		return "not configured"
	}
	out := testrun.Run(ctx, testrun.Spec{Command: command, Format: "exit-code", Timeout: p.cfg.TestTimeout(), Dir: wt,
		RunDir: filepath.Join(runDir, name)})
	switch {
	case out.Status != testrun.StatusCompleted:
		return fmt.Sprintf("ERROR (%s; %s)", out.Problem, out.OutputPath)
	case out.Results[0].Status == "pass":
		return "PASS"
	}
	return fmt.Sprintf("FAIL (exit %d; %s)", *out.ExitCode, out.OutputPath)
}

// changedFiles lists the files changed on the branch since base. -z keeps
// paths byte-exact: no quoting of unicode, tabs or quotes, and renames and
// copies come as status, source, destination.
func changedFiles(ctx context.Context, wt, base string) ([]changedFile, error) {
	out, err := gitx.Run(ctx, wt, "diff", "--name-status", "-z", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	return parseNameStatusZ(out)
}

func parseNameStatusZ(out string) ([]changedFile, error) {
	var files []changedFile
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if out == "" {
		return files, nil
	}
	for i := 0; i < len(fields); {
		status := fields[i]
		n := 2
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			n = 3
		}
		if i+n > len(fields) {
			return nil, fmt.Errorf("parse git diff --name-status -z: truncated entry at %q", status)
		}
		f := changedFile{Status: status, Path: fields[i+n-1]}
		if n == 3 {
			f.From = fields[i+1]
		}
		files = append(files, f)
		i += n
	}
	return files, nil
}

func statusMap(rs []store.Result) map[string]string {
	m := make(map[string]string, len(rs))
	for _, r := range rs {
		m[r.TestID] = r.Status
	}
	return m
}

func summarizeRun(r store.Run) runSummary {
	return runSummary{RunID: r.ID, SHA: r.SHA, Status: r.Status, Fidelity: r.Fidelity, Passed: r.Passed, Failed: r.Failed,
		Skipped: r.Skipped, Output: r.OutputPath}
}

func summaryRow(taskID int64, sm summaryFile, ver verification, commits []string, files []changedFile, head, reportPath string) store.Summary {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	nonNil := func(ss []string) []string {
		if ss == nil {
			return []string{}
		}
		return ss
	}
	if sm.AlternativesRejected == nil {
		sm.AlternativesRejected = []alternative{}
	}
	if sm.Changed == nil {
		sm.Changed = []changedNote{}
	}
	if files == nil {
		files = []changedFile{}
	}
	return store.Summary{TaskID: taskID, Decision: sm.Decision, AlternativesRejected: enc(sm.AlternativesRejected), Why: sm.Why,
		Verification: enc(ver), Risks: enc(nonNil(sm.Risks)), Followups: enc(nonNil(sm.Followups)), ChangedNotes: enc(sm.Changed),
		Commits: enc(nonNil(commits)), Files: enc(files), TrueAtSHA: head, ReportPath: reportPath}
}
