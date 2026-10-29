package cli

import (
	"strings"
	"testing"
)

func TestReproRedGreen(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig + "rerun_command = \"sh ./test.sh {junit} {tests}\"\n")
	task := startedTask(t, h, "bugfix", "Fix bug")

	h.expectCode(ExitUsage, "repro", "red", task.Slug, "--test", "C::bug")
	h.expectCode(ExitUsage, "repro", "red", task.Slug, "--test", "C::bug", "--file", "../x")
	h.expectCode(ExitNotFound, "repro", "red", task.Slug, "--test", "C::bug", "--file", "tests/bug.t")
	h.expectCode(ExitState, "repro", "green", task.Slug, "--test", "C::bug")

	commitIn(t, task.Worktree, "tests/bug.t", "x")
	// A passing test does not reproduce the bug.
	h.expectCode(ExitGate, "repro", "red", task.Slug, "--test", "C::a", "--file", "tests/bug.t")
	h.expectCode(ExitNotFound, "repro", "red", task.Slug, "--test", "C::nope", "--file", "tests/bug.t")
	if out := h.mustRun("repro", "red", task.Slug, "--test", "C::bug", "--file", "tests/bug.t"); !strings.Contains(out, "red recorded") {
		t.Fatalf("red: %s", out)
	}
	h.expectCode(ExitGate, "repro", "green", task.Slug, "--test", "C::bug")

	commitIn(t, task.Worktree, "fix.txt", "x")
	commitIn(t, task.Worktree, "tests/bug.t", "weakened")
	out := h.mustRun("repro", "green", task.Slug, "--test", "C::bug")
	if !strings.Contains(out, "green recorded") || !strings.Contains(out, "changed after the red run") {
		t.Fatalf("green: %s", out)
	}
	ps, err := h.db().Proofs(t.Context(), task.ID)
	if err != nil || len(ps) != 1 || ps[0].GreenSHA == "" || !ps[0].TestChanged || ps[0].RedStatus != "fail" {
		t.Fatalf("proofs %+v %v", ps, err)
	}

	feature := startedTask(t, h, "feature", "Feat")
	h.expectCode(ExitState, "repro", "red", feature.Slug, "--test", "C::a", "--file", "test.sh")
}
