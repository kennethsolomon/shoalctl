package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
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

// A green run must come from a later commit that changes something: a test
// that passes on the red commit itself (here through an ignored file) or on
// an empty commit proves nothing about a fix (§8.3).
func TestReproGreenNeedsALaterCommitWithChanges(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig + "rerun_command = \"sh ./test.sh {junit} {tests}\"\n")
	task := startedTask(t, h, "bugfix", "Fake proof")
	commitIn(t, task.Worktree, "tests/bug.t", "x")
	h.mustRun("repro", "red", task.Slug, "--test", "C::bug", "--file", "tests/bug.t")

	exclude := testutil.Git(t, task.Worktree, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	testutil.WriteFile(t, exclude, readFile(t, exclude)+"\nfix.txt\n")
	testutil.WriteFile(t, filepath.Join(task.Worktree, "fix.txt"), "x")
	mustContain(t, "green on the red commit", h.expectCode(ExitGate, "repro", "green", task.Slug, "--test", "C::bug"), "same commit")

	testutil.Git(t, task.Worktree, "commit", "-q", "--allow-empty", "-m", "fake fix")
	mustContain(t, "green on an empty commit", h.expectCode(ExitGate, "repro", "green", task.Slug, "--test", "C::bug"), "changes nothing")
	if ps, err := h.db().Proofs(t.Context(), task.ID); err != nil || len(ps) != 1 || ps[0].GreenSHA != "" {
		t.Fatalf("a refused green was recorded: %+v %v", ps, err)
	}

	testutil.Git(t, task.Worktree, "add", "-f", "fix.txt")
	testutil.Git(t, task.Worktree, "commit", "-q", "-m", "real fix")
	mustContain(t, "real green", h.mustRun("repro", "green", task.Slug, "--test", "C::bug"), "green recorded")
}
