package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testrun"
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

// Done when 1: the Bug A / Bug B scenario (spec §14 steps 1-5). Both bugs
// claim resources/js/users/**; the human presses s, so Bug B waits for Bug
// A. Bug A goes red, green, fails a QA round, passes the next, is queued and
// merged; Bug B is unblocked and starts from the new base, and the lesson
// Bug A recorded after its failed round reaches Bug B's brief (step 5).
func TestPhase2BugABugB(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	bugA := newTask(t, h, "bugfix", "Users page shows wrong display name")
	bugB := newTask(t, h, "bugfix", "Users page sort broken")
	todoA := filepath.Join(t.TempDir(), "todo-a.md")
	testutil.WriteFile(t, todoA, "1. User::displayName falls back to email\n2. red test, fix, green\n")
	todoB := filepath.Join(t.TempDir(), "todo-b.md")
	testutil.WriteFile(t, todoB, "1. sort by last name in the users list\n")

	mustContain(t, "plan A", h.mustRun("plan", bugA, "--todo", todoA, "app/Models/User.php", "resources/js/users/**"), "ready")
	h.env.Terminal = func() bool { return true }
	h.env.Stdin = strings.NewReader("?\ns\n")
	out := h.mustRun("plan", bugB, "--todo", todoB, "resources/js/users/**", "app/Http/Controllers/UserController.php")
	mustContain(t, "plan B", out, "overlap: "+bugB+" and "+bugA, "resources/js/users/**  overlaps  resources/js/users/**",
		"todo.md of "+bugA+":", "User::displayName falls back to email", "todo.md of "+bugB+":", "sort by last name",
		"blocked: it starts after "+bugA)
	h.env.Terminal, h.env.Stdin = nil, nil
	mustContain(t, "status", h.mustRun("status"), "blocked by "+bugA)
	mustContain(t, "B cannot start", h.expectCode(ExitState, "start", bugB), "blocked", bugA)

	h.mustRun("start", bugA)
	a := h.task(bugA)
	mustContain(t, "A brief", readFile(t, filepath.Join(a.Worktree, ".shoal", "brief.md")), "## Plan", ".shoal/todo.md")
	mustContain(t, "A todo", readFile(t, filepath.Join(a.Worktree, ".shoal", "todo.md")), "falls back to email")

	commitIn(t, a.Worktree, "tests/bug.t", "x")
	h.mustRun("repro", "red", bugA, "--test", "C::bug", "--file", "tests/bug.t")
	commitIn(t, a.Worktree, "fix.txt", "x")
	commitIn(t, a.Worktree, "resources/js/users/list.js", "export const name = u => u.first_name ?? u.email\n")
	h.mustRun("repro", "green", bugA, "--test", "C::bug")
	commitIn(t, a.Worktree, "broken.txt", "the list view reads another accessor")
	mustContain(t, "qa 1", h.expectCode(ExitGate, "qa", bugA), "record the lesson: shoalctl record lesson "+bugA) // QA round 1 fails
	mustContain(t, "lesson", h.mustRun("record", "lesson", bugA, "--pattern", "the list view reads a different accessor than User::displayName",
		"--fix", "make the list view use User::displayName", "--glob", "resources/js/users/**"), "recorded lesson:1 for "+bugA)
	testutil.Git(t, a.Worktree, "rm", "-q", "broken.txt")
	testutil.Git(t, a.Worktree, "commit", "-q", "-m", "list view uses the same accessor")
	mustContain(t, "qa 2", h.mustRun("qa", bugA), "qa round 2 passed")
	finish(t, h, bugA, a.Worktree)
	mustContain(t, "queued", h.mustRun("status"), "queued main #1")

	out = h.mustRun("merge")
	mustContain(t, "merge", out, "merged "+bugA+" into main", bugB+": unblocked, ready to start from the new base")
	if got := h.task(bugA); got.Stage != store.StageMerged {
		t.Fatalf("A: %+v", got)
	}
	if cl, _ := h.db().Claims(context.Background(), a.ID); len(cl) != 0 {
		t.Fatalf("A's claims not released: %+v", cl)
	}
	b := h.task(bugB)
	if b.Stage != store.StageReady || b.BlockedBy != 0 {
		t.Fatalf("B after A merged: %+v", b)
	}

	h.mustRun("start", bugB)
	b = h.task(bugB)
	if main := testutil.Git(t, h.repo, "rev-parse", "main"); b.BaseSHA != main {
		t.Fatalf("B starts from %s, want the new main %s", b.BaseSHA, main)
	}
	if _, err := os.Stat(filepath.Join(b.Worktree, "resources", "js", "users", "list.js")); err != nil {
		t.Fatalf("B's worktree lacks Bug A's fix: %v", err)
	}
	if cl, _ := h.db().Claims(context.Background(), b.ID); len(cl) != 2 {
		t.Fatalf("B's claims: %+v", cl)
	}
	// Step 5: Bug B's brief carries Bug A's lesson, because the paths match:
	// in brief.md at start, and in the brief SessionStart injects.
	mustContain(t, "B brief.md", readFile(t, filepath.Join(b.Worktree, ".shoal", "brief.md")), "## Lessons from earlier tasks",
		"- lesson:1 (task "+bugA, "on resources/js/users/**", "the list view reads a different accessor", "verify it against the current code")
	start := inHook(t, h, b.Worktree, "session-start", `{"session_id": "bug-b", "source": "startup"}`)
	mustContain(t, "B session brief", start.HookSpecificOutput.AdditionalContext, "shoal task "+bugB, "- lesson:1 (task "+bugA,
		"make the list view use User::displayName")
	if ls, err := h.db().Lessons(context.Background()); err != nil || len(ls) != 1 || ls[0].Hits != 1 || ls[0].QARound != 1 {
		t.Fatalf("Bug A's lesson after Bug B's session started: %+v %v", ls, err)
	}
	// Bug A's fix is recorded as a bug, pointing at the commit on main.
	mustContain(t, "why bug:1", h.mustRun("why", "bug:1"), "bug:1 (task "+bugA, "Root cause:", "Fix: "+shortSHA(h.task(bugA).MergedSHA),
		"Regression test C::bug (tests/bug.t): still in HEAD")
}

// Done when 2: concurrent plans claiming one path; exactly one wins and
// the rest see the overlap (no terminal: exit 5, nothing written). Run once
// with goroutines in one process (for -race) and once with real processes.
func TestPhase2ConcurrentPlansOneWinner(t *testing.T) {
	check := func(t *testing.T, h *harness, p *project, codes []int, stderrs []string) {
		t.Helper()
		sort.Ints(codes)
		if codes[0] != ExitOK || codes[1] != ExitState || codes[len(codes)-1] != ExitState {
			t.Fatalf("exit codes %v: want one 0 and the rest %d\n%s", codes, ExitState, strings.Join(stderrs, "\n"))
		}
		if n := countRows(t, p, `SELECT count(*) FROM file_claims WHERE released_at IS NULL AND path_glob = 'shared/**'`); n != 1 {
			t.Fatalf("%d open claims on shared/**, want 1", n)
		}
		if n := countRows(t, p, `SELECT count(*) FROM tasks WHERE stage = 'ready'`); n != 1 {
			t.Fatalf("%d ready tasks, want 1", n)
		}
	}
	t.Run("goroutines", func(t *testing.T) {
		h := newHarness(t)
		p := h.initWith(junitConfig)
		const n = 8
		slugs := make([]string, n)
		for i := range slugs {
			slugs[i] = newTask(t, h, "bugfix", fmt.Sprintf("Racer %d", i))
		}
		codes, stderrs := make([]int, n), make([]string, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range slugs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				codes[i], _, stderrs[i] = h.run("plan", slugs[i], "shared/**")
			}(i)
		}
		close(start)
		wg.Wait()
		check(t, h, p, codes, stderrs)
	})
	t.Run("processes", func(t *testing.T) {
		testutil.RequireSh(t)
		h := newHarness(t)
		p := h.initWith(junitConfig)
		const n = 4
		var cmds []*bytes.Buffer
		var procs []func() int
		for i := 0; i < n; i++ {
			slug := newTask(t, h, "bugfix", fmt.Sprintf("Racer %d", i))
			cmd := h.command("plan", slug, "shared/**")
			var errb bytes.Buffer
			cmd.Stderr = &errb
			cmds = append(cmds, &errb)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			procs = append(procs, func() int {
				_ = cmd.Wait()
				return cmd.ProcessState.ExitCode()
			})
		}
		codes, stderrs := make([]int, n), make([]string, n)
		for i, wait := range procs {
			codes[i], stderrs[i] = wait(), cmds[i].String()
		}
		check(t, h, p, codes, stderrs)
	})
}

// Done when 3: doctor --fix recovers a task killed mid-start (stage
// starting, its baseline run left running) and releases a stale claim.
func TestPhase2DoctorRecoversKilledStartAndStaleClaim(t *testing.T) {
	h := newHarness(t).withSession()
	h.env.Exe = fakeBinary(t, t.TempDir())
	pids := t.TempDir()
	pidFile := filepath.Join(pids, "sh.pid")
	p := h.initWith(fmt.Sprintf("[tests]\ncommand = \"echo $$ > '%s'; sleep 2\"\nformat = \"exit-code\"\n", pidFile))

	// A task whose session died hours ago still claims api/**.
	gone := newTask(t, h, "bugfix", "Abandoned session")
	h.mustRun("plan", gone, "api/**")
	h.mustRun("start", gone)
	g := h.task(gone)
	h.tmuxOut("kill-window", "-t", g.TmuxWindow)
	old := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
	sqlExec(t, p, `UPDATE tasks SET last_seen_at = ?, started_at = ? WHERE id = ?`, old, old, g.ID)
	sqlExec(t, p, `UPDATE file_claims SET created_at = ? WHERE task_id = ?`, old, g.ID)

	// start is killed (SIGKILL) inside its baseline run.
	killed := newTask(t, h, "feature", "Killed mid-start")
	commitIn(t, h.repo, "moved.txt", "main moves, so this start cannot reuse a baseline\n")
	os.Remove(pidFile)
	cmd := h.command("start", killed)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	suite := readPID(t, pidFile)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitFor(t, func() bool { return !testrun.GroupAlive(suite) }) // the orphaned suite finishes on its own
	if got := h.task(killed); got.Stage != store.StageStarting {
		t.Fatalf("killed start left %+v", got)
	}

	errOut := h.expectCode(ExitDoctor, "doctor")
	mustContain(t, "doctor", errOut, killed+" is at starting, but no start is running", "--fix rolls it back to new",
		"baseline run", "no shoalctl holds "+killed, gone+": claim api/** ((plan)) is stale")

	out := h.mustRun("doctor", "--fix")
	mustContain(t, "fixed", out, "fixed "+killed+" rolled back from starting to new", "fixed "+gone+": claim api/** ((plan)) is stale", "released",
		"fixed "+gone+": forgot the gone tmux window")
	if got := h.task(killed); got.Stage != store.StageNew {
		t.Fatalf("after --fix: %+v", got)
	}
	if n := countRows(t, p, `SELECT count(*) FROM test_runs WHERE status = 'running'`); n != 0 {
		t.Fatalf("%d run(s) still running", n)
	}
	if cl, _ := h.db().Claims(context.Background(), g.ID); len(cl) != 0 {
		t.Fatalf("stale claim kept: %+v", cl)
	}

	// The killed task resumes, and api/** is free to plan.
	h.mustRun("start", killed)
	if got := h.task(killed); got.Stage != store.StageInProgress {
		t.Fatalf("resumed start: %+v", got)
	}
	mustContain(t, "plan after release", h.mustRun("plan", newTask(t, h, "bugfix", "Next on api"), "api/x.go"), "ready")
	h.mustRun("doctor")
}

// The attention watcher end to end: start launches the real watcher process;
// a fake claude shows Claude Code's trust dialog in a real tmux pane; status
// flags the task until the dialog is answered.
func TestPhase2AttentionFlag(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	dialog := filepath.Join("..", "attention", "testdata", "trust.txt")
	abs, err := filepath.Abs(dialog)
	if err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(t.TempDir(), "claude")
	testutil.WriteFile(t, claude, "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"2.1.310 (Claude Code)\"; exit 0; fi\n"+
		"cat '"+abs+"'\nread answer\nprintf '\\033[2J\\033[H'\necho 'Working on it.'\nexec sleep 60\n")
	if err := os.Chmod(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	h.env.Claude = claude
	// start runs in-process with the real spawner; the spawned watcher is
	// this test binary acting as shoalctl.
	h.env.SpawnWatcher, h.env.Exe = nil, os.Args[0]
	t.Setenv(asBinaryEnv, "1")
	t.Setenv("SHOALCTL_TEST_EXE", fakeExe)
	t.Setenv("SHOALCTL_TEST_CLAUDE", claude)
	t.Setenv("SHOALCTL_TEST_TMUX_ARGS", strings.Join(h.env.TmuxArgs, " "))

	slug := newTask(t, h, "feature", "Watched session")
	h.mustRun("start", slug)
	task := h.task(slug)
	p := h.project()
	watchLock := filepath.Join(p.dir, "locks", fmt.Sprintf("watch-%d.lock", task.ID))
	t.Cleanup(func() {
		// The watcher must be gone before the temp dirs are removed.
		h.tmuxOut("kill-window", "-t", task.TmuxWindow)
		waitFor(t, func() bool {
			l, err := lockfile.Acquire(watchLock, 0, lockfile.Info{PID: os.Getpid(), Command: "test"})
			if err == nil {
				l.Release()
			}
			return err == nil
		})
	})
	want := "dialog: Do you trust the files in this folder?"
	waitFor(t, func() bool { return h.task(slug).Attention == want })
	mustContain(t, "status", h.mustRun("status"), "NEEDS ATTENTION: "+want)
	var rows []statusRow
	if err := json.Unmarshal([]byte(h.mustRun("status", "--json")), &rows); err != nil || len(rows) != 1 || rows[0].Attention != want {
		t.Fatalf("status --json: %+v %v", rows, err)
	}

	h.tmuxOut("send-keys", "-t", task.TmuxWindow, "1", "Enter")
	waitFor(t, func() bool { return h.task(slug).Attention == "" })
	mustContain(t, "watch log", readFile(t, filepath.Join(p.dir, "logs", fmt.Sprintf("watch-%d.log", task.ID))), slug+" needs attention: "+want)
}
