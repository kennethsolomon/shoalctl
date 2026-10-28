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
	pos, err := parseArgs(fs, args, 1)
	if err != nil {
		return err
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
	lock, err := p.lockTask(t, "done")
	if err != nil {
		return err
	}
	defer lock.Release()
	if t, err = p.db.GetTask(ctx, t.Slug); err != nil {
		return err
	}
	if t.Stage != store.StageInProgress {
		return fail(ExitState, "task %s is at %q; done needs in-progress", t.Slug, t.Stage)
	}
	wt := t.Worktree
	if _, err := os.Stat(wt); err != nil {
		return fail(ExitState, "worktree %s of task %s is missing: %v", wt, t.Slug, err)
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
	head, err := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return err
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
		return fail(ExitInterrupted, "interrupted: final run %d was cancelled and its processes stopped; the task is still in-progress, run shoalctl done %s again", final.ID, t.Slug)
	}
	lint := env.step(ctx, p, wt, "lint", p.cfg.Stack.Lint, p.runDir(final.ID))
	build := env.step(ctx, p, wt, "build", p.cfg.Stack.Build, p.runDir(final.ID))
	if ctx.Err() != nil {
		return fail(ExitInterrupted, "interrupted during lint or build; the task is still in-progress, run shoalctl done %s again", t.Slug)
	}

	commitsOut, err := gitx.Run(ctx, wt, "rev-list", "--reverse", t.BaseSHA+"..HEAD")
	if err != nil {
		return err
	}
	commits := strings.Fields(commitsOut)
	nameStatus, err := gitx.Run(ctx, wt, "diff", "--name-status", t.BaseSHA+"...HEAD")
	if err != nil {
		return err
	}
	files := parseNameStatus(nameStatus)

	base, err := p.db.GetRun(ctx, t.BaselineRunID)
	if err != nil {
		return err
	}
	baseResults, err := p.db.Results(ctx, base.ID)
	if err != nil {
		return err
	}
	finalResults, err := p.db.Results(ctx, final.ID)
	if err != nil {
		return err
	}
	var classes map[testrun.Class][]string
	if final.Status == testrun.StatusCompleted {
		classes = testrun.Compare(statusMap(baseResults), statusMap(finalResults))
	}

	ver := verification{Baseline: summarizeRun(base), Final: summarizeRun(final), Classes: map[string]int{}, Lint: lint, Build: build}
	for c, ids := range classes {
		ver.Classes[string(c)] = len(ids)
	}
	in := reportInput{task: t, sum: sm, head: head, commits: commits, files: files, base: base, final: final,
		baseResults: baseResults, classes: classes, ver: ver, date: time.Now().UTC().Format("2006-01-02")}
	report := renderReport(in)

	reportPath := filepath.Join(p.dir, "reports", t.Slug+".md")
	if err := writePrivate(reportPath, []byte(report)); err != nil {
		return err
	}
	if p.cfg.Reports.Sink != "local" {
		notes := filepath.Join(p.cfg.Reports.NotesPath, t.Slug+".md")
		if err := writeFileAtomic(notes, []byte(report), 0o600); err != nil {
			return fail(ExitConfig, "copy report to [reports].notes_path: %v", err)
		}
	}

	row := summaryRow(t.ID, sm, ver, commits, files, head, reportPath)
	if _, err := p.db.Update(ctx, t, []string{store.StageInProgress}, "stage = ?, final_run_id = ?, closed_at = ?",
		[]any{store.StageDone, final.ID, nowUTC()}, func(tx *sql.Tx) error { return store.InsertSummary(ctx, tx, row) }); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, reportPath)
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

func parseNameStatus(out string) []changedFile {
	var files []changedFile
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		switch {
		case len(parts) == 2:
			files = append(files, changedFile{Status: parts[0], Path: parts[1]})
		case len(parts) == 3:
			files = append(files, changedFile{Status: parts[0], From: parts[1], Path: parts[2]})
		}
	}
	return files
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
