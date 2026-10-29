package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// Phase 1 acceptance (spec §15, "Done when"): each test drives shoalctl
// through Main against a temp git repo, a private tmux server and a fake
// claude, the way a session and its agents would.

func finish(t *testing.T, h *harness, slug, wt string, extra ...string) string {
	t.Helper()
	testutil.WriteFile(t, filepath.Join(wt, ".shoal", "summary.json"), goodSummary)
	return h.mustRun(append([]string{"done", "--force", slug}, extra...)...)
}

func reportOf(t *testing.T, h *harness, slug string) string {
	t.Helper()
	return readFile(t, filepath.Join(h.project().dir, "reports", slug+".md"))
}

func mustContain(t *testing.T, what, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, s)
		}
	}
}

// Done when 1: a feature runs through the feature lane with backend and
// frontend agents and a frozen contract.
func TestPhase1FeatureLaneWithTeam(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	mustContain(t, "agents install", h.mustRun("agents", "install"), "shoal-lead", "shoal-backend", "shoal-frontend", "shoal-qa-scope")
	task := startedTask(t, h, "feature", "Clients CRUD")
	mustContain(t, "brief", readFile(t, filepath.Join(task.Worktree, ".shoal", "brief.md")),
		"## Team", "shoal-lead", "shoal-backend", "shoal-frontend", "shoal-qa-breaker", "shoal-qa-regression", "shoalctl contract freeze "+task.Slug)

	// shoal-lead: behaviours, contract, claims.
	h.mustRun("scope", "add", task.Slug, "--behaviour", "the clients feature adds C::new")
	h.mustRun("scope", "case", task.Slug, "1", "--category", "happy", "--test", "C::new")
	contract := filepath.Join(task.Worktree, ".shoal", "contract.md")
	testutil.WriteFile(t, contract, "## Endpoints\nPOST /clients -> 201 {id}\n## Errors\n422 duplicate email\n")
	mustContain(t, "freeze", h.mustRun("contract", "freeze", task.Slug), "version 1")
	h.mustRun("claim", task.Slug, "--as", "shoal-backend", "api/**", "feature.txt")
	h.mustRun("claim", task.Slug, "--as", "shoal-frontend", "ui/**")
	h.expectCode(ExitState, "claim", task.Slug, "--as", "shoal-frontend", "api/routes.go")

	// Builders work inside their claims and hand off through the store.
	commitIn(t, task.Worktree, "api/clients.go", "package api\n")
	commitIn(t, task.Worktree, "feature.txt", "x")
	h.mustRun("msg", "send", task.Slug, "--from", "shoal-backend", "--to", "shoal-lead", "--kind", "handoff", "POST /clients done: api/clients.go")
	commitIn(t, task.Worktree, "ui/clients.js", "export {}\n")
	h.mustRun("msg", "send", task.Slug, "--from", "shoal-frontend", "--to", "shoal-lead", "--kind", "handoff", "form posts to /clients: ui/clients.js")
	mustContain(t, "lead inbox", h.mustRun("msg", "inbox", task.Slug, "--as", "shoal-lead"), "shoal-backend -> shoal-lead [handoff]", "shoal-frontend -> shoal-lead [handoff]")

	// The contract changes without a re-freeze: qa fails, then the lead re-freezes.
	testutil.WriteFile(t, contract, "## Endpoints\nPOST /clients -> 201 {id, name}\n## Errors\n422 duplicate email\n")
	code, _, errOut := h.run("qa", task.Slug)
	if code != ExitGate || !strings.Contains(errOut, "contract.md changed since frozen version 1") {
		t.Fatalf("qa with a changed contract: %d %s", code, errOut)
	}
	h.mustRun("contract", "freeze", task.Slug, "--note", "POST /clients also returns name")
	mustContain(t, "frontend inbox", h.mustRun("msg", "inbox", task.Slug, "--as", "shoal-frontend"), "contract-change", "also returns name")
	mustContain(t, "qa", h.mustRun("qa", task.Slug), "qa round 2 passed")

	finish(t, h, task.Slug, task.Worktree)
	mustContain(t, "report", reportOf(t, h, task.Slug), "status: done", "QA rounds: 2/3 - round 1 failed on contract:", "| behaviour the clients feature adds C::new | 1 | 1 | 0 |")

	mustContain(t, "merge", h.mustRun("merge"), "merged "+task.Slug+" into main", "nothing was pushed")
	merged := h.task(task.Slug)
	if merged.Stage != "merged" || merged.MergedSHA == "" || testutil.Git(t, h.repo, "rev-parse", "main") != merged.MergedSHA {
		t.Fatalf("after merge: %+v (main %s)", merged, testutil.Git(t, h.repo, "rev-parse", "main"))
	}
	if _, err := os.Stat(filepath.Join(h.repo, "ui", "clients.js")); err != nil {
		t.Fatalf("main checkout not fast-forwarded: %v", err)
	}
	if cl, _ := h.db().Claims(context.Background(), merged.ID); len(cl) != 0 {
		t.Fatalf("claims not released: %+v", cl)
	}
	mustContain(t, "merged report", reportOf(t, h, task.Slug), "status: merged", "- Gate: ", "Merged into main at")
}

// Done when 2: an "also test X" scope item blocks done until every row is
// green or waived.
func TestPhase1ScopeBlocksDone(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	slug := strings.TrimSpace(h.mustRun("new", "feature", "CSV export", "--scope", "also test CSV export"))
	h.mustRun("start", slug)
	task := h.task(slug)
	mustContain(t, "brief", readFile(t, filepath.Join(task.Worktree, ".shoal", "brief.md")), "also test CSV export", "shoal-qa-scope")
	commitIn(t, task.Worktree, "feature.txt", "x")
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "summary.json"), goodSummary)

	errOut := h.expectCode(ExitDoneRefused, "done", "--force", slug)
	mustContain(t, "refusal", errOut, "has no happy row", "has no ui-state row")

	// shoal-qa-scope fills the matrix.
	h.mustRun("scope", "case", slug, "1", "--category", "happy", "--test", "C::new")
	h.mustRun("scope", "case", slug, "1", "--category", "boundary", "--test", "C::a")
	h.mustRun("scope", "case", slug, "1", "--category", "invalid", "--manual", "upload a .txt: expect a 422")
	for _, c := range []string{"empty", "permission", "concurrency", "failure"} {
		h.mustRun("scope", "case", slug, "1", "--category", c, "--test", "C::later")
	}
	h.mustRun("scope", "case", slug, "1", "--category", "ui-state", "--na", "export has no UI")
	errOut = h.expectCode(ExitDoneRefused, "done", "--force", slug)
	mustContain(t, "refusal", errOut, "(item #1, invalid): manual check not done", "(item #1, empty): test C::later is skip", "(item #1, ui-state)")
	if strings.Contains(errOut, "(item #1, happy)") {
		t.Errorf("a passing row was listed as open:\n%s", errOut)
	}

	// Agents can't waive; the human checks and waives at a terminal.
	h.expectCode(ExitState, "scope", "review", slug)
	h.env.Terminal = func() bool { return true }
	h.mustRun("scope", "check", slug, "3", "--pass")
	h.env.Stdin = strings.NewReader("w\nskipped on CI, covered by staging\nw\nsame\nw\nsame\nk\nk\n")
	h.mustRun("scope", "review", slug)
	errOut = h.expectCode(ExitDoneRefused, "done", "--force", slug)
	mustContain(t, "refusal", errOut, "2 scope row(s)", "(item #1, failure)", "(item #1, ui-state)")
	h.env.Stdin = strings.NewReader("w\nsame\nw\nno UI\n")
	h.mustRun("scope", "review", slug)

	h.mustRun("done", "--force", slug)
	mustContain(t, "report", reportOf(t, h, slug), "| user also test CSV export | 8 | 3 | 5 (", "`ui-state`: no UI",
		"scope item \"also test CSV export\", `ui-state` waived: no UI")
}

// Done when 3: a bugfix without a red/green proof is refused by done.
func TestPhase1BugfixNeedsProof(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := startedTask(t, h, "bugfix", "Wrong display name")
	commitIn(t, task.Worktree, "tests/bug.t", "x")
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "summary.json"), goodSummary)
	mustContain(t, "no proof", h.expectCode(ExitDoneRefused, "done", "--force", task.Slug), "no red/green regression proof")

	h.mustRun("repro", "red", task.Slug, "--test", "C::bug", "--file", "tests/bug.t")
	commitIn(t, task.Worktree, "fix.txt", "x")
	mustContain(t, "red only", h.expectCode(ExitDoneRefused, "done", "--force", task.Slug), "has no green run yet")
	h.mustRun("repro", "green", task.Slug, "--test", "C::bug")

	h.mustRun("done", "--force", task.Slug)
	ps, _ := h.db().Proofs(context.Background(), task.ID)
	mustContain(t, "report", reportOf(t, h, task.Slug), "Regression proof: `tests/bug.t` (`C::bug`) red at "+shortSHA(ps[0].RedSHA)+", green at "+shortSHA(ps[0].GreenSHA))
}

// Done when 4: breaking a baseline-passing test blocks merge with exit 9;
// --accept-regression lets it through with the override in the report.
func TestPhase1MergeGateRegression(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Risky change")
	commitIn(t, task.Worktree, "broken.txt", "x")
	finish(t, h, task.Slug, task.Worktree)
	mainBefore := testutil.Git(t, h.repo, "rev-parse", "main")

	code, out, errOut := h.run("merge")
	if code != ExitGate || !strings.Contains(out, "regression: C::a") || !strings.Contains(errOut, "merge --task "+task.Slug+" --accept-regression C::a") {
		t.Fatalf("merge: %d\n%s\n%s", code, out, errOut)
	}
	if got := h.task(task.Slug); got.Stage != "qa" || got.GateRunID == 0 {
		t.Fatalf("blocked task: %+v", got)
	}
	if testutil.Git(t, h.repo, "rev-parse", "main") != mainBefore {
		t.Fatal("a blocked merge moved main")
	}

	h.mustRun("done", "--force", task.Slug)
	h.env.Terminal = func() bool { return true } // accepting a regression is a human's call
	// A typo is not an acceptance: C::a still blocks.
	h.expectCode(ExitGate, "merge", "--task", task.Slug, "--accept-regression", "C::aa", "--reason", "typo")
	h.mustRun("done", "--force", task.Slug)
	h.expectCode(ExitUsage, "merge", "--task", task.Slug, "--accept-regression", "C::a")
	out = h.mustRun("merge", "--task", task.Slug, "--accept-regression", "C::a", "--reason", "C::a asserted the old behaviour on purpose")
	mustContain(t, "merge", out, "merged "+task.Slug)
	mustContain(t, "report", reportOf(t, h, task.Slug), "status: merged",
		"Accepted regressions: `C::a`: \"C::a asserted the old behaviour on purpose\" (by shoal test)",
		"- Accepted regression `C::a`: C::a asserted the old behaviour on purpose")
	if n := countRows(t, p, `SELECT count(*) FROM regression_overrides WHERE target = 'C::a' AND accepted_by = 'shoal test'`); n != 1 {
		t.Fatalf("overrides = %d", n)
	}
	// An override for a task that is no longer done is refused.
	h.expectCode(ExitState, "merge", "--task", task.Slug, "--accept-regression", "C::a", "--reason", "again")
}

// Done when 5: a test made flaky on purpose is quarantined and listed.
func TestPhase1FlakyQuarantined(t *testing.T) {
	h := newHarness(t).withSession()
	counter := filepath.Join(t.TempDir(), "flaky-ran")
	testutil.WriteFile(t, filepath.Join(h.repo, "test.sh"), flakyScript(counter))
	testutil.Git(t, h.repo, "commit", "-q", "-am", "flaky test")
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Ship with a flaky suite")
	mustContain(t, "quarantine", h.mustRun("test", "quarantine", "list"), "C::flaky", "flaky at baseline")

	commitIn(t, task.Worktree, "feature.txt", "x")
	os.Remove(counter) // flaky again in the final run: still ignored
	finish(t, h, task.Slug, task.Worktree)
	mustContain(t, "report", reportOf(t, h, task.Slug), "Quarantined tests: `C::flaky` (flaky at baseline, run #1)")
	if strings.Contains(reportOf(t, h, task.Slug), "Would block the merge gate") {
		t.Fatal("a quarantined test blocked")
	}
	os.Remove(counter)
	h.mustRun("merge")
	mustContain(t, "merged report", reportOf(t, h, task.Slug), "status: merged", "Quarantined tests: `C::flaky`")
}

// Done when 6: a finished hotfix merges into the production branch and
// creates its backport task.
func TestPhase1HotfixMergesAndBackports(t *testing.T) {
	h := newHarness(t).withSession()
	testutil.Git(t, h.repo, "branch", "production")
	commitIn(t, h.repo, "main-only.txt", "x") // main moves on past production
	h.initWith(junitConfig + "[hotfix]\nproduction_ref = \"production\"\nmax_diff_lines = 5\n")
	mainBefore := testutil.Git(t, h.repo, "rev-parse", "main")
	prodBefore := testutil.Git(t, h.repo, "rev-parse", "production")

	task := startedTask(t, h, "hotfix", "Checkout crashes")
	if task.BaseRef != "production" || task.BaseSHA != prodBefore {
		t.Fatalf("hotfix base: %+v", task)
	}
	commitIn(t, task.Worktree, "tests/bug.t", "x")
	h.mustRun("repro", "red", task.Slug, "--test", "C::bug", "--file", "tests/bug.t")
	commitIn(t, task.Worktree, "fix.txt", "1\n2\n3\n4\n5\n6\n7\n8\n")
	h.mustRun("repro", "green", task.Slug, "--test", "C::bug")
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "summary.json"), goodSummary)
	mustContain(t, "budget", h.expectCode(ExitDoneRefused, "done", "--force", task.Slug), "8 added + removed lines outside test files", "max_diff_lines = 5")
	h.expectCode(ExitUsage, "done", "--force", "--accept-large-diff", task.Slug)
	out := h.mustRun("done", "--force", "--accept-large-diff", "--reason", "the fix needs the full table", task.Slug)
	mustContain(t, "done", out, "backport task 2-backport-checkout-crashes created")

	back := h.task("2-backport-checkout-crashes")
	head := testutil.Git(t, task.Worktree, "rev-parse", "HEAD")
	if back.Lane != "bugfix" || back.Stage != "new" || back.ParentTaskID != task.ID || back.Title != "Backport: Checkout crashes" {
		t.Fatalf("backport task: %+v", back)
	}
	mustContain(t, "backport ask", back.Ask, "onto main", head, "C::bug (tests/bug.t)", "not-reproducible")

	mustContain(t, "merge", h.mustRun("merge"), "merged "+task.Slug+" into production")
	if got := testutil.Git(t, h.repo, "rev-parse", "production"); got != head {
		t.Fatalf("production = %s, want the hotfix head %s", got, head)
	}
	if testutil.Git(t, h.repo, "rev-parse", "main") != mainBefore {
		t.Fatal("a hotfix merge moved main")
	}
	mustContain(t, "report", reportOf(t, h, task.Slug), "status: merged", "Merged into production at", "Backport: task 2-backport-checkout-crashes",
		"Large hotfix diff accepted: 8 lines over the 5-line budget: the fix needs the full table")
	if got := h.task(task.Slug); got.Stage != "merged" || got.LargeDiffReason == "" {
		t.Fatalf("hotfix: %+v", got)
	}
}
