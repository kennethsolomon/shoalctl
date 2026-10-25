package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

const goodSummary = `{"decision": "Stream rows to CSV", "alternatives_rejected": [{"option": "Build in memory", "why": "large exports"}],
 "why": "Exports must not exhaust memory", "risks": ["Slow disks"], "followups": ["Add XLSX"],
 "changed": [{"path": "feature.txt", "note": "the feature"}]}`

var reportSections = []string{"---\ntask: ", "\n# ", "\n## Asked\n", "\n## Decision\n", "\n## Why\n", "\n## Changed\n", "\n## Verified\n",
	"\n## Regression check (informational)\n", "\n## Pre-existing failures\n", "\n## Risks / Follow-ups\n", "\n## Fidelity and limits\n"}

func assertSections(t *testing.T, report string) {
	t.Helper()
	pos := -1
	for _, s := range reportSections {
		i := strings.Index(report, s)
		if i <= pos {
			t.Fatalf("section %q missing or out of order in:\n%s", s, report)
		}
		pos = i
	}
}

func startedTask(t *testing.T, h *harness, lane, title string) store.Task {
	t.Helper()
	slug := strings.TrimSpace(h.mustRun("new", lane, title))
	h.mustRun("start", slug)
	return h.task(slug)
}

func commitIn(t *testing.T, dir, file, content string) {
	t.Helper()
	testutil.WriteFile(t, filepath.Join(dir, file), content)
	testutil.Git(t, dir, "add", file)
	testutil.Git(t, dir, "commit", "-q", "-m", "change "+file)
}

func TestDoneRendersReport(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig + "[stack]\nlint = \"true\"\nbuild = \"exit 4\"\n")
	task := startedTask(t, h, "feature", "Add export")
	commitIn(t, task.Worktree, "feature.txt", "x")
	commitIn(t, task.Worktree, "broken.txt", "x")
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "summary.json"), goodSummary)

	out := h.mustRun("done", task.Slug)
	reportPath := filepath.Join(p.dir, "reports", task.Slug+".md")
	if !strings.Contains(out, reportPath) {
		t.Fatalf("done output should print the report path:\n%s", out)
	}
	report := readFile(t, reportPath)
	assertSections(t, report)
	for _, want := range []string{
		"task: " + task.Slug, "lane: feature", "status: done", "branch: feature/" + task.Slug, "base: main@" + shortSHA(task.BaseSHA),
		"# Add export", "Stream rows to CSV", "- Rejected: Build in memory (large exports)", "Exports must not exhaust memory",
		"- A `feature.txt` - the feature", "- A `broken.txt`",
		"- Lint: PASS", "- Build: FAIL (exit 4",
		"1 regression", "regression: `C::a`", "1 new,", "`C::pre`", "- Risk: Slow disks", "- Follow-up: Add XLSX",
		"Coverage and untested areas are not computed in this version", "Flaky re-runs are not applied in this version",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}

	done := h.task(task.Slug)
	if done.Stage != "done" || done.ClosedAt == "" || done.FinalRunID == 0 {
		t.Fatalf("task after done: %+v", done)
	}
	db := openRaw(t, p.dbPath())
	defer db.Close()
	var commits, files, ver, trueAt, path string
	if err := db.QueryRow(`SELECT commits, files, verification, true_at_sha, report_path FROM summaries WHERE task_id = ?`, done.ID).
		Scan(&commits, &files, &ver, &trueAt, &path); err != nil {
		t.Fatal(err)
	}
	var shas []string
	json.Unmarshal([]byte(commits), &shas)
	head := testutil.Git(t, task.Worktree, "rev-parse", "HEAD")
	if len(shas) != 2 || shas[1] != head || trueAt != head || path != reportPath || !strings.Contains(files, `"path":"feature.txt"`) {
		t.Fatalf("summary row: commits %s files %s true_at %s path %s", commits, files, trueAt, path)
	}
	var v verification
	if err := json.Unmarshal([]byte(ver), &v); err != nil || v.Classes[string(testrun.Regression)] != 1 || v.Final.Status != "completed" {
		t.Fatalf("verification %s (%v)", ver, err)
	}
	if n := countRows(t, p, `SELECT count(*) FROM test_runs WHERE kind = 'final' AND task_id = ?`, done.ID); n != 1 {
		t.Fatalf("final runs = %d", n)
	}

	h.expectCode(ExitState, "done", task.Slug)
	var rows []statusRow
	json.Unmarshal([]byte(h.mustRun("status", "--json")), &rows)
	if len(rows) != 1 || rows[0].Stage != "done" {
		t.Fatalf("status after done: %+v", rows)
	}
}

func TestDoneRefusals(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	h.mustRun("new", "feature", "Not started")
	h.expectCode(ExitState, "done", "1")

	task := startedTask(t, h, "bugfix", "Fix it")
	summary := filepath.Join(task.Worktree, ".shoal", "summary.json")

	if errOut := h.expectCode(ExitState, "done", task.Slug); !strings.Contains(errOut, "no commits") {
		t.Errorf("no commits: %s", errOut)
	}
	commitIn(t, task.Worktree, "fix.txt", "x")
	testutil.WriteFile(t, filepath.Join(task.Worktree, "dirty.txt"), "x")
	if errOut := h.expectCode(ExitState, "done", task.Slug); !strings.Contains(errOut, "uncommitted") {
		t.Errorf("dirty: %s", errOut)
	}
	os.Remove(filepath.Join(task.Worktree, "dirty.txt"))

	for name, body := range map[string]string{
		"missing":     "",
		"invalid":     "{",
		"unknown key": `{"decision": "d", "why": "w", "extra": 1}`,
		"nested key":  `{"decision": "d", "why": "w", "changed": [{"path": "p", "note": "n", "x": 1}]}`,
		"empty why":   `{"decision": "d", "why": "  "}`,
		"trailing":    `{"decision": "d", "why": "w"} {}`,
	} {
		os.Remove(summary)
		if body != "" {
			testutil.WriteFile(t, summary, body)
		}
		errOut := h.expectCode(ExitState, "done", task.Slug)
		if !strings.Contains(errOut, summary) || !strings.Contains(errOut, "expected shape") {
			t.Errorf("%s: stderr %s", name, errOut)
		}
	}
	if got := h.task(task.Slug); got.Stage != "in-progress" {
		t.Fatalf("refused done changed the stage: %+v", got)
	}

	testutil.WriteFile(t, summary, `{"decision": "d", "why": "w"}`)
	h.mustRun("done", task.Slug)
}

func TestDoneFinalRunErrorIsReported(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Breaks suite")
	commitIn(t, task.Worktree, "test.sh", "#!/bin/sh\nexit 1\n")
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "summary.json"), `{"decision": "d", "why": "w"}`)
	h.mustRun("done", task.Slug)
	report := readFile(t, filepath.Join(p.dir, "reports", task.Slug+".md"))
	assertSections(t, report)
	if !strings.Contains(report, "Not computed: the final run ended with status error") || !strings.Contains(report, "Lint: not configured") {
		t.Fatalf("report:\n%s", report)
	}
}

func TestDoneNotesSink(t *testing.T) {
	h := newHarness(t).withSession()
	notes := t.TempDir()
	h.initWith(junitConfig + "[reports]\nsink = \"both\"\nnotes_path = " + `"` + filepath.ToSlash(notes) + `"` + "\n")
	task := startedTask(t, h, "feature", "Notes")
	commitIn(t, task.Worktree, "feature.txt", "x")
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "summary.json"), goodSummary)
	h.mustRun("done", task.Slug)
	assertSections(t, readFile(t, filepath.Join(notes, task.Slug+".md")))
}

func TestReportFidelityLines(t *testing.T) {
	base := reportInput{task: store.Task{Slug: "1-x", Lane: "bugfix", Title: "X", Ask: "a", Branch: "bugfix/1-x", BaseRef: "main", BaseSHA: "abcdef0123"},
		sum: summaryFile{Decision: "d", Why: "w"}, date: "2026-10-25"}

	suite := base
	suite.base = store.Run{Fidelity: "suite", Status: "completed", Failed: 1}
	suite.final = store.Run{Fidelity: "suite", Status: "completed"}
	suite.classes = map[testrun.Class][]string{testrun.Fixed: {testrun.SuiteID}}
	r := renderReport(suite)
	assertSections(t, r)
	if !strings.Contains(r, reducedFidelity) {
		t.Errorf("suite fidelity report lacks the reduced-fidelity line:\n%s", r)
	}

	none := base
	none.base = store.Run{Fidelity: "none", Status: "completed"}
	none.final = none.base
	r = renderReport(none)
	assertSections(t, r)
	if !strings.Contains(r, "# X\n\n> "+protocolInactive) || strings.Count(r, protocolInactive) != 2 {
		t.Errorf("none report must state the inactive protocol first and in limits:\n%s", r)
	}
}
