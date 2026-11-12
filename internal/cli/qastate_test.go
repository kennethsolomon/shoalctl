package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// qaStatus is the task's status row (JSON) and its line of the table.
func qaStatus(t *testing.T, h *harness, slug string) (statusRow, string) {
	t.Helper()
	var rows []statusRow
	if err := json.Unmarshal([]byte(h.mustRun("status", "--json")), &rows); err != nil {
		t.Fatal(err)
	}
	var row statusRow
	for _, r := range rows {
		if r.Slug == slug {
			row = r
		}
	}
	if row.Slug == "" {
		t.Fatalf("status --json lacks %s: %+v", slug, rows)
	}
	for _, line := range strings.Split(h.mustRun("status"), "\n") {
		if strings.HasPrefix(line, slug+" ") {
			return row, line
		}
	}
	t.Fatalf("status lacks a line for %s", slug)
	return row, ""
}

// expectQA checks the task's QA state in status --json and its QA cell in
// the table, and returns the row with its table line.
func expectQA(t *testing.T, h *harness, slug, step, state, cell string, round int) (statusRow, string) {
	t.Helper()
	row, line := qaStatus(t, h, slug)
	if row.QAState != state || row.QARound != round || !strings.Contains(line, "  "+cell+"  ") {
		t.Fatalf("%s: qa_state %q qa_round %d, want %q %d; line lacks %q:\n%s", step, row.QAState, row.QARound, state, round, cell, line)
	}
	return row, line
}

// The QA column follows the code: none, pass on this code, stale after a
// commit or an uncommitted edit, pass again after a round (or after the edit
// is undone), and fail.
func TestStatusQATracksTheCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig + "[lanes]\nqa_max_rounds = 5\n")
	task := startedTask(t, h, "feature", "Track QA")
	hint := "code changed since QA: run shoalctl qa " + task.Slug

	row, line := expectQA(t, h, task.Slug, "never ran", qaNone, "-", 0)
	if row.QASHA != "" || row.AgentQAState != qaNone || strings.Contains(line, hint) {
		t.Fatalf("never ran: %+v", row)
	}

	commitIn(t, task.Worktree, "feature.txt", "x")
	h.mustRun("qa", task.Slug)
	head := testutil.Git(t, task.Worktree, "rev-parse", "HEAD")
	row, line = expectQA(t, h, task.Slug, "after round 1", qaPass, "pass 1/5", 1)
	if row.QASHA != head || strings.Contains(line, hint) {
		t.Fatalf("after round 1: qa_sha %s (want %s), line %q", row.QASHA, head, line)
	}
	// shoal's own files are ignored: a session writing them changes nothing.
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "summary.json"), goodSummary)
	expectQA(t, h, task.Slug, "after .shoal/ changed", qaPass, "pass 1/5", 1)

	commitIn(t, task.Worktree, "more.txt", "y")
	row, line = expectQA(t, h, task.Slug, "after a commit", qaStale, "stale 1/5", 1)
	if row.QASHA != head || !strings.HasSuffix(line, hint) {
		t.Fatalf("after a commit: qa_sha %s, line %q", row.QASHA, line)
	}
	out := h.mustRun("status", "--json")
	for _, key := range []string{`"qa_round": 1`, `"qa_state": "stale"`, `"qa_sha": "` + head + `"`, `"agent_qa_state": "none"`, `"agent_qa_sha": ""`} {
		if !strings.Contains(out, key) {
			t.Errorf("status --json lacks %s:\n%s", key, out)
		}
	}

	h.mustRun("qa", task.Slug)
	expectQA(t, h, task.Slug, "after round 2", qaPass, "pass 2/5", 2)

	testutil.WriteFile(t, filepath.Join(task.Worktree, "more.txt"), "edited, not committed")
	expectQA(t, h, task.Slug, "after an uncommitted edit", qaStale, "stale 2/5", 2)
	testutil.WriteFile(t, filepath.Join(task.Worktree, "more.txt"), "y")
	expectQA(t, h, task.Slug, "after the edit was undone", qaPass, "pass 2/5", 2)
	testutil.WriteFile(t, filepath.Join(task.Worktree, "untracked.txt"), "new")
	expectQA(t, h, task.Slug, "after a new untracked file", qaStale, "stale 2/5", 2)
	if err := os.Remove(filepath.Join(task.Worktree, "untracked.txt")); err != nil {
		t.Fatal(err)
	}

	commitIn(t, task.Worktree, "broken.txt", "x")
	h.expectCode(ExitGate, "qa", task.Slug)
	_, line = expectQA(t, h, task.Slug, "after a failed round", qaFail, "fail 3/5", 3)
	if strings.Contains(line, hint) {
		t.Fatalf("a failed round got the stale hint: %q", line)
	}
}

// A worktree status cannot read warns on stderr and shows a pass as stale,
// with no hint to run shoalctl qa, which cannot succeed there.
func TestStatusQAOnAnUnreadableWorktree(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Gone worktree")
	commitIn(t, task.Worktree, "feature.txt", "x")
	h.mustRun("qa", task.Slug)
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(task.Worktree, moved); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := h.run("status")
	if code != 0 {
		t.Fatalf("status: exit %d\n%s", code, errOut)
	}
	mustContain(t, "status stderr", errOut, "warning: "+task.Slug+": cannot read the worktree's code")
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, task.Slug+" ") {
			line = l
		}
	}
	if !strings.Contains(line, "  stale 1/3  ") || strings.Contains(line, "code changed since QA") {
		t.Fatalf("unreadable worktree: line %q", line)
	}
}

// An agent's QA check (shoalctl test --kind qa) shows as "agent ..." and
// never counts as the human's round; shoalctl qa refuses inside Claude Code.
func TestAgentQAIsNotAHumanRound(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Agent QA")
	commitIn(t, task.Worktree, "feature.txt", "x")

	h.mustRun("test", task.Slug, "--kind", "qa")
	head := testutil.Git(t, task.Worktree, "rev-parse", "HEAD")
	row, _ := expectQA(t, h, task.Slug, "agent pass", qaNone, "agent pass", 0)
	if row.AgentQAState != qaPass || row.AgentQASHA != head || row.QASHA != "" {
		t.Fatalf("agent pass: %+v", row)
	}
	commitIn(t, task.Worktree, "more.txt", "y")
	row, line := expectQA(t, h, task.Slug, "agent stale", qaNone, "agent stale", 0)
	if row.AgentQAState != qaStale || strings.Contains(line, "code changed since QA") {
		t.Fatalf("agent stale: %+v, line %q", row, line)
	}
	commitIn(t, task.Worktree, "broken.txt", "x")
	h.expectCode(ExitGate, "test", task.Slug, "--kind", "qa")
	expectQA(t, h, task.Slug, "agent fail", qaNone, "agent fail", 0)
	// A gate run is no QA check.
	h.expectCode(ExitGate, "test", task.Slug)
	expectQA(t, h, task.Slug, "after a gate run", qaNone, "agent fail", 0)

	// Inside Claude Code, shoalctl qa refuses before it counts anything.
	h.setenv("CLAUDECODE", "1")
	errOut := h.expectCode(ExitState, "qa", task.Slug)
	mustContain(t, "qa in Claude Code", errOut, "Claude Code session", "shoalctl test "+task.Slug+" --kind qa")
	h.setenv("CLAUDECODE", "")
	if got := h.task(task.Slug); got.QARound != 0 || got.Stage != "in-progress" {
		t.Fatalf("a refused qa changed the task: %+v", got)
	}
	if n := countRows(t, p, `SELECT count(*) FROM qa_rounds`); n != 0 {
		t.Fatalf("qa_rounds has %d rows after agent checks only", n)
	}

	// The human's round wins the column; a later agent check shows beside it.
	h.expectCode(ExitGate, "qa", task.Slug)
	expectQA(t, h, task.Slug, "human fail", qaFail, "fail 1/3", 1)
	testutil.Git(t, task.Worktree, "rm", "-q", "broken.txt")
	testutil.Git(t, task.Worktree, "commit", "-q", "-m", "fix")
	h.mustRun("test", task.Slug, "--kind", "qa")
	row, _ = expectQA(t, h, task.Slug, "human fail, agent pass", qaFail, "fail 1/3, agent pass", 1)
	if row.AgentQAState != qaPass {
		t.Fatalf("human fail, agent pass: %+v", row)
	}
	// An agent pass is no human pass: done still warns.
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "summary.json"), goodSummary)
	_, _, errOut = h.run("done", "--force", task.Slug)
	mustContain(t, "done after an agent pass", errOut, "warning: "+task.Slug+": QA round 1 failed and no round passed since (QA is optional)")
}

// An agent QA check whose code changed while the suite ran records no
// verdict, as qa records no round.
func TestAgentQARefusesCodeChangedMidRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Moving code")
	commitIn(t, task.Worktree, "feature.txt", "x")
	cfg := strings.Replace(junitConfig, "sh ./test.sh {junit}", "sh ./test.sh {junit}; echo late > late.txt", 1)
	if err := os.WriteFile(filepath.Join(p.dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	errOut := h.expectCode(ExitDoneRefused, "test", task.Slug, "--kind", "qa")
	mustContain(t, "changed mid-run", errOut, "changed while test ran", "nothing was recorded", "run shoalctl test "+task.Slug+" --kind qa again")
	if n := countRows(t, p, `SELECT count(*) FROM test_runs WHERE verdict != ''`); n != 0 {
		t.Fatalf("%d run(s) got a verdict over code that changed mid-run", n)
	}
	expectQA(t, h, task.Slug, "after the refused check", qaNone, "-", 0)
}

// done and merge warn on stderr, and go on, when no human QA round passed on
// the code: never ran or stale. A fresh pass gets no warning.
func TestDoneAndMergeWarnWithoutQAOnTheCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	never := startedTask(t, h, "feature", "Never QAed")
	fresh := startedTask(t, h, "feature", "Fresh QA")
	stale := startedTask(t, h, "feature", "Stale QA")
	commitIn(t, never.Worktree, "never.txt", "n")
	commitIn(t, fresh.Worktree, "fresh.txt", "f")
	commitIn(t, stale.Worktree, "stale.txt", "s")
	h.mustRun("qa", fresh.Slug)
	h.mustRun("qa", stale.Slug)
	commitIn(t, stale.Worktree, "stale2.txt", "s")

	done := func(slug, wt string) string {
		t.Helper()
		testutil.WriteFile(t, filepath.Join(wt, ".shoal", "summary.json"), goodSummary)
		code, _, errOut := h.run("done", "--force", slug)
		if code != 0 {
			t.Fatalf("done %s: exit %d\n%s", slug, code, errOut)
		}
		return errOut
	}
	stop := "; to run one first, stop done now (Ctrl-C) and run shoalctl qa "
	mustContain(t, "done never", done(never.Slug, never.Worktree),
		"warning: "+never.Slug+": no QA round ran on this code (QA is optional)"+stop+never.Slug+"\n")
	if errOut := done(fresh.Slug, fresh.Worktree); strings.Contains(errOut, "warning") {
		t.Fatalf("done after a fresh pass warned:\n%s", errOut)
	}
	mustContain(t, "done stale", done(stale.Slug, stale.Worktree),
		"warning: "+stale.Slug+": the code changed since QA round 1 passed (QA is optional)"+stop+stale.Slug+"\n")

	code, out, errOut := h.run("merge")
	if code != 0 || !strings.Contains(out, "merged 3 task(s)") {
		t.Fatalf("merge: exit %d\n%s\n%s", code, out, errOut)
	}
	mustContain(t, "merge", errOut,
		"warning: "+never.Slug+": no QA round ran on this code (QA is optional); merging anyway; the merge gate still runs\n",
		"warning: "+stale.Slug+": the code changed since QA round 1 passed (QA is optional); merging anyway; the merge gate still runs\n")
	if strings.Contains(errOut, fresh.Slug) {
		t.Fatalf("merge warned about the fresh pass:\n%s", errOut)
	}
	// Merged tasks show their last result as recorded.
	expectQA(t, h, fresh.Slug, "merged", qaPass, "pass 1/3", 1)
}
