package cli

import (
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// Phase 2 acceptance (spec §15, "Done when") plus the attention watcher and
// the merge-output fix: each test drives shoalctl through Main (and, where
// real concurrency or a real watcher process matters, as subprocesses)
// against a temp git repo, a private tmux server and a fake claude.

// An overridden merge says what was accepted (row, class, reason) before it
// says merged, and never prints BLOCKED; a partial acceptance lists what was
// accepted, what still blocks, and that the task went back to qa.
func TestPhase2MergeOutputAcceptance(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig + "[stack]\nlint = \"test ! -f lintfail.txt\"\n")
	task := startedTask(t, h, "feature", "Two blockers")
	commitIn(t, task.Worktree, "broken.txt", "x")
	commitIn(t, task.Worktree, "lintfail.txt", "x")
	finish(t, h, task.Slug, task.Worktree)
	h.env.Terminal = func() bool { return true }

	code, out, errOut := h.run("merge", "--task", task.Slug, "--accept-regression", "C::a", "--reason", "old behaviour on purpose")
	if code != ExitGate {
		t.Fatalf("partial acceptance: exit %d\n%s\n%s", code, out, errOut)
	}
	mustContain(t, "partial stdout", out, `accepted by shoal test for `+task.Slug+`: regression C::a: "old behaviour on purpose"`, "BLOCKED by: (lint)")
	mustContain(t, "partial stderr", errOut, `accepted regression C::a ("old behaviour on purpose")`, "still blocks on (lint)",
		"nothing was merged", "the acceptances were not recorded", "back at qa",
		"--accept-regression C::a --reason 'old behaviour on purpose'", `--accept-regression '(lint)' --reason "<why>"`)
	if strings.Contains(out, "BLOCKED by: C::a") {
		t.Errorf("an accepted row was printed as blocking:\n%s", out)
	}
	if got := h.task(task.Slug); got.Stage != "qa" {
		t.Fatalf("partial acceptance left the task at %s", got.Stage)
	}
	if n := countRows(t, h.project(), `SELECT count(*) FROM regression_overrides`); n != 0 {
		t.Fatalf("a partial acceptance recorded %d override(s)", n)
	}

	h.mustRun("done", "--force", task.Slug)
	out = h.mustRun("merge", "--task", task.Slug, "--accept-regression", "C::a", "--reason", "old behaviour on purpose",
		"--accept-regression", "(lint)", "--reason", "lint rule retired")
	if strings.Contains(out, "BLOCKED") {
		t.Errorf("a fully accepted merge printed BLOCKED:\n%s", out)
	}
	acc := strings.Index(out, `accepted by shoal test for `+task.Slug+`: regression C::a: "old behaviour on purpose"`)
	lint := strings.Index(out, `accepted by shoal test for `+task.Slug+`: (lint): "lint rule retired"`)
	merged := strings.Index(out, "merged "+task.Slug+" into main")
	if acc < 0 || lint < 0 || merged < 0 || acc > merged || lint > merged {
		t.Fatalf("want both acceptances, then merged:\n%s", out)
	}
	mustContain(t, "full", out, "every blocking row was accepted (2)")
	if got := h.task(task.Slug); got.Stage != "merged" || testutil.Git(t, h.repo, "rev-parse", "main") != got.MergedSHA {
		t.Fatalf("after full acceptance: %+v", got)
	}
}
