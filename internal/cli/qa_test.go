package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func TestQARoundsEscalateAfterCap(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig + "flaky_retries = 0\n[stack]\nlint = \"test ! -f lint-bad.txt\"\n")
	task := startedTask(t, h, "feature", "Hard one")

	if out := h.mustRun("qa", task.Slug); !strings.Contains(out, "qa round 1 passed") {
		t.Fatalf("round 1:\n%s", out)
	}
	commitIn(t, task.Worktree, "broken.txt", "x")
	code, _, errOut := h.run("qa", task.Slug)
	if code != ExitGate || !strings.Contains(errOut, "regression: C::a") {
		t.Fatalf("round 2: %d %s", code, errOut)
	}
	if got := h.task(task.Slug); got.Stage != "qa" || got.QARound != 2 {
		t.Fatalf("after round 2: %+v", got)
	}
	if in := h.mustRun("msg", "inbox", task.Slug, "--as", "shoal-lead"); !strings.Contains(in, "qa-failure") || !strings.Contains(in, "regression: C::a") {
		t.Fatalf("lead inbox:\n%s", in)
	}

	// A contract that was never frozen fails the round too; so does lint.
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".shoal", "contract.md"), "## Endpoints\n")
	commitIn(t, task.Worktree, "lint-bad.txt", "x")
	code, _, errOut = h.run("qa", task.Slug)
	if code != ExitGate || !strings.Contains(errOut, "escalated after 3 rounds") {
		t.Fatalf("round 3: %d %s", code, errOut)
	}
	got := h.task(task.Slug)
	if got.Stage != "escalated" || got.QARound != 3 {
		t.Fatalf("after round 3: %+v", got)
	}
	windows, err := h.env.tmux(t.Context(), "list-windows", "-a", "-F", "#{window_id} #{window_name}")
	if err != nil || !strings.Contains(windows, got.TmuxWindow+" !"+task.Slug) {
		t.Fatalf("window not renamed: %q %v", windows, err)
	}
	report := readFile(t, filepath.Join(p.dir, "reports", task.Slug+".md"))
	for _, want := range []string{"status: escalated", "## Blocked", "- Attempt 1: passed", "- Attempt 2: failed: regression: C::a",
		"contract: contract.md exists but was never frozen", "lint: FAIL", "Best hypothesis: these failed in every failed round, so the fixes so far missed their cause: regression: C::a"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	h.expectCode(ExitState, "qa", task.Slug)
	h.expectCode(ExitState, "done", "--force", task.Slug)
}
