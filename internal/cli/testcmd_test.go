package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// flakyScript is testScript plus C::flaky, which fails the first time it
// runs (it leaves a marker file at counter) and passes afterwards.
func flakyScript(counter string) string {
	return strings.Replace(testScript, "echo '</testsuite>'",
		`if [ -f '`+counter+`' ]; then echo '<testcase classname="C" name="flaky"/>'; else : > '`+counter+`'; echo '<testcase classname="C" name="flaky"><failure message="race"/></testcase>'; fi
echo '</testsuite>'`, 1)
}

func TestTestCommandBlocksOnRegression(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig + "rerun_command = \"sh ./test.sh {junit} {tests}\"\n")
	task := startedTask(t, h, "feature", "Gate me")

	out := h.mustRun("test", task.Slug)
	if !strings.Contains(out, "pre-existing") || strings.Contains(out, "BLOCKED") {
		t.Fatalf("clean gate:\n%s", out)
	}
	commitIn(t, task.Worktree, "broken.txt", "x")
	code, out, errOut := h.run("test", "--kind", "qa", task.Slug)
	if code != ExitGate || !strings.Contains(out, "regression: C::a") || !strings.Contains(errOut, "C::a") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	// C::pre and C::a failed, so both were re-run twice with rerun_command.
	run := countRows(t, p, `SELECT max(id) FROM test_runs WHERE kind = 'qa'`)
	if n := countRows(t, p, `SELECT max(attempt) FROM test_results WHERE run_id = ?`, run); n != 3 {
		t.Fatalf("attempts = %d", n)
	}
	if _, err := os.Stat(filepath.Join(p.runDir(int64(run)), "attempt-3", "output.log")); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, filepath.Join(task.Worktree, "dirty.txt"), "x")
	h.expectCode(ExitState, "test", task.Slug)
	h.expectCode(ExitUsage, "test", "--kind", "final", task.Slug)
	h.expectCode(ExitNotFound, "test", "99-nope")
}

func TestFlakyBaselineIsQuarantined(t *testing.T) {
	h := newHarness(t).withSession()
	counter := filepath.Join(t.TempDir(), "flaky-ran")
	testutil.WriteFile(t, filepath.Join(h.repo, "test.sh"), flakyScript(counter))
	testutil.Git(t, h.repo, "commit", "-q", "-am", "flaky test")
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Flaky")

	list := h.mustRun("test", "quarantine", "list")
	if !strings.Contains(list, "C::flaky") || !strings.Contains(list, "race") {
		t.Fatalf("quarantine list:\n%s", list)
	}
	// Flaky again at the gate, but quarantined: ignored, listed.
	os.Remove(counter)
	out := h.mustRun("test", task.Slug)
	if !strings.Contains(out, "quarantined (ignored): C::flaky") {
		t.Fatalf("gate:\n%s", out)
	}
	h.mustRun("test", "quarantine", "release", "C::flaky")
	h.expectCode(ExitNotFound, "test", "quarantine", "release", "C::flaky")
	if !strings.Contains(h.mustRun("test", "quarantine", "list"), "no quarantined tests") {
		t.Fatal("released test still listed")
	}
	// Released: the baseline counts it as a pass, so flaky at the gate is unstable and blocks.
	os.Remove(counter)
	code, out, _ := h.run("test", task.Slug)
	if code != ExitGate || !strings.Contains(out, "unstable: C::flaky") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}
