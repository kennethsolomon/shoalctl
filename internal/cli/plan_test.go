package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func newTask(t *testing.T, h *harness, lane, title string) string {
	t.Helper()
	return strings.TrimSpace(h.mustRun("new", lane, title))
}

func sqlExec(t *testing.T, p *project, q string, args ...any) {
	t.Helper()
	db := openRaw(t, p.dbPath())
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func TestPlanWithoutOverlapIsReady(t *testing.T) {
	h := newHarness(t)
	h.initWith(junitConfig)
	a := newTask(t, h, "bugfix", "Display name")
	todo := filepath.Join(t.TempDir(), "todo.md")
	testutil.WriteFile(t, todo, "1. trace User::displayName\n2. red test\n")
	mustContain(t, "plan", h.mustRun("plan", a, "--todo", todo, "app/Models/User.php", "resources/js/users/**", "app/Models/User.php"),
		"planned (2 glob(s)): ready")
	got := h.task(a)
	cl, _ := h.db().Claims(context.Background(), got.ID)
	if got.Stage != store.StageReady || len(cl) != 2 || cl[0].Agent != store.PlanAgent {
		t.Fatalf("after plan: %+v %+v", got, cl)
	}
	mustContain(t, "stored todo", readFile(t, planTodoPath(h.project(), a)), "red test")
	// Re-planning replaces the plan's claims.
	h.mustRun("plan", a, "app/Models/User.php")
	if cl, _ := h.db().Claims(context.Background(), got.ID); len(cl) != 1 {
		t.Fatalf("re-plan kept old claims: %+v", cl)
	}
	h.expectCode(ExitUsage, "plan", a, "../etc/passwd")
	h.expectCode(ExitUsage, "plan", a)
	h.expectCode(ExitNotFound, "plan", "99-nope", "x")
}

func TestPlanOverlapNeedsAHuman(t *testing.T) {
	h := newHarness(t)
	h.initWith(junitConfig)
	a, b := newTask(t, h, "bugfix", "Bug A"), newTask(t, h, "bugfix", "Bug B")
	h.mustRun("plan", a, "resources/js/users/**")
	errOut := h.expectCode(ExitState, "plan", b, "resources/js/users/list.js")
	mustContain(t, "agent plan", errOut, "nothing was planned", "A human resolves overlaps at a terminal", a+" (ready): resources/js/users/list.js ~ resources/js/users/**")
	if got := h.task(b); got.Stage != store.StageNew {
		t.Fatalf("b changed: %+v", got)
	}
	h.env.Terminal = func() bool { return true }
	h.env.Stdin = strings.NewReader("")
	mustContain(t, "EOF", h.expectCode(ExitState, "plan", b, "resources/js/users/list.js"), "no choice")

	h.env.Stdin = strings.NewReader("x\n?\ns\n")
	out := h.mustRun("plan", b, "resources/js/users/list.js")
	mustContain(t, "prompt", out, "overlap: "+b+" and "+a, "[s] serialize", "[m] merge", "[p] parallel anyway", "type one of",
		"todo.md of "+a+":", "(no todo.md)", "and blocked: it starts after "+a)
	got := h.task(b)
	if got.Stage != store.StageBlocked || got.BlockedBy != h.task(a).ID {
		t.Fatalf("serialized: %+v", got)
	}
	mustContain(t, "start blocked", h.expectCode(ExitState, "start", b), "is blocked", a+" (ready)", "press p")

	// a cannot serialize behind b now; parallel works.
	h.env.Stdin = strings.NewReader("p\n")
	mustContain(t, "cycle", h.mustRun("plan", a, "resources/js/users/**"), "[s] not available: "+b+" already waits on "+a, "ready")
}

func TestPlanFoldsIntoEarlierTask(t *testing.T) {
	h := newHarness(t)
	h.initWith(junitConfig)
	a := newTask(t, h, "bugfix", "Users page broken")
	slugB := strings.TrimSpace(h.mustRun("new", "bugfix", "Users page sort", "--scope", "also test sort by name"))
	h.mustRun("plan", a, "resources/js/users/**")
	h.env.Terminal = func() bool { return true }
	h.env.Stdin = strings.NewReader("m\n")
	mustContain(t, "fold", h.mustRun("plan", slugB, "resources/js/users/**", "app/Http/UserController.php"), slugB+" folded into "+a)
	ta, tb := h.task(a), h.task(slugB)
	if tb.Stage != store.StageFolded || tb.FoldedInto != ta.ID || !strings.Contains(ta.Ask, "Users page sort") {
		t.Fatalf("fold: a=%+v b=%+v", ta, tb)
	}
	items, _ := h.db().ScopeItems(context.Background(), ta.ID)
	if len(items) != 1 || items[0].Text != "also test sort by name" {
		t.Fatalf("scope moved: %+v", items)
	}
	h.expectCode(ExitState, "start", slugB)
	h.expectCode(ExitState, "claim", slugB, "--as", "shoal-backend", "x")
}

func TestPlanIgnoresStaleClaims(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed: without it no claim is ever stale")
	}
	h := newHarness(t)
	p := h.initWith(junitConfig)
	a, b := newTask(t, h, "bugfix", "Abandoned"), newTask(t, h, "bugfix", "Next")
	h.mustRun("plan", a, "api/**")
	// a started long ago and its window is gone.
	sqlExec(t, p, `UPDATE tasks SET stage = 'in-progress', tmux_window = '@999', tmux_socket = ?, last_seen_at = '2026-01-01T00:00:00Z', started_at = '2026-01-01T00:00:00Z' WHERE slug = ?`, shortSocketPath(t), a)
	sqlExec(t, p, `UPDATE file_claims SET created_at = '2026-01-01T00:00:00Z'`)
	mustContain(t, "stale", h.mustRun("plan", b, "api/x.go"), "stale: "+a+" (in-progress)", "doctor --fix releases", "ready")
	// A recent heartbeat makes the claim live again.
	sqlExec(t, p, `UPDATE tasks SET last_seen_at = ? WHERE slug = ?`, nowUTC(), a)
	h.expectCode(ExitState, "plan", b, "api/x.go")
	// Started tasks are not re-planned.
	h.expectCode(ExitState, "plan", a, "api/**")
}

// TestPlanSeesWildcardReachingIntoALiteralDirectory: a plan on the literal
// directory src/pkg holds every file below it, so a wildcard plan that can
// match one of them is an overlap, not a second ready task.
func TestPlanSeesWildcardReachingIntoALiteralDirectory(t *testing.T) {
	for _, pair := range [][2]string{{"src/pkg", "**/x.go"}, {"src/pkg", "src/*/x.go"}, {"internal/store", "**/*_test.go"},
		{"**/x.go", "src/pkg"}} {
		h := newHarness(t)
		h.initWith(junitConfig)
		a, b := newTask(t, h, "bugfix", "Bug A"), newTask(t, h, "bugfix", "Bug B")
		h.mustRun("plan", a, pair[0])
		mustContain(t, "overlap "+pair[0]+" vs "+pair[1], h.expectCode(ExitState, "plan", b, pair[1]), "nothing was planned", a+" (ready)")
		if got := h.task(b); got.Stage != store.StageNew {
			t.Fatalf("%v: b planned anyway: %+v", pair, got)
		}
	}
}

// TestPlanRefusesAFoldThatWouldDeadlock: x waits on me, y waits on x; folding
// me into y would make x wait on y. Neither s nor m is offered, only p.
func TestPlanRefusesAFoldThatWouldDeadlock(t *testing.T) {
	h := newHarness(t)
	h.initWith(junitConfig)
	me, x, y := newTask(t, h, "bugfix", "Me"), newTask(t, h, "bugfix", "X"), newTask(t, h, "bugfix", "Y")
	h.env.Terminal = func() bool { return true }
	h.mustRun("plan", me, "m/**")
	h.env.Stdin = strings.NewReader("s\n")
	h.mustRun("plan", x, "m/**", "x/**")
	h.env.Stdin = strings.NewReader("s\n")
	h.mustRun("plan", y, "x/**", "y/**")
	h.env.Stdin = strings.NewReader("m\np\n")
	out := h.mustRun("plan", me, "y/**")
	mustContain(t, "fold refused", out, "[m] not available: "+x+" waits on "+me+" and "+y+" already waits on it", "type one of", "ready")
	if got := h.task(me); got.Stage != store.StageReady {
		t.Fatalf("me: %+v", got)
	}
	if got := h.task(x); got.Stage != store.StageBlocked || got.BlockedBy != h.task(me).ID {
		t.Fatalf("x: %+v", got)
	}
}

// TestLiveWindowKeepsAnOldClaim: a started task whose heartbeat is long past
// stale_after but whose tmux window is still open is alive. doctor --fix must
// not release its claim and plan must still see the overlap.
func TestLiveWindowKeepsAnOldClaim(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	a, b := newTask(t, h, "bugfix", "Long run"), newTask(t, h, "bugfix", "Next")
	h.mustRun("plan", a, "api/**")
	h.mustRun("start", a)
	if h.task(a).TmuxWindow == "" {
		t.Fatal("start recorded no tmux window")
	}
	sqlExec(t, p, `UPDATE tasks SET last_seen_at = '2026-01-01T00:00:00Z', started_at = '2026-01-01T00:00:00Z' WHERE slug = ?`, a)
	sqlExec(t, p, `UPDATE file_claims SET created_at = '2026-01-01T00:00:00Z'`)
	mustContain(t, "plan overlap", h.expectCode(ExitState, "plan", b, "api/x.go"), "nothing was planned", a+" (in-progress)")
	_, out, errOut := h.run("doctor", "--fix")
	if strings.Contains(out+errOut, "is stale") || !strings.Contains(out, "no stale claims") {
		t.Fatalf("doctor --fix treated a live window's claim as stale:\n%s\n%s", out, errOut)
	}
	if cl, _ := h.db().Claims(context.Background(), h.task(a).ID); len(cl) != 1 {
		t.Fatalf("claims of the live task: %+v", cl)
	}
	mustContain(t, "plan overlap", h.expectCode(ExitState, "plan", b, "api/x.go"), "nothing was planned", a+" (in-progress)")
}

// TestStartRecordsTheTmuxSocket: doctor run from cron or an IDE has a
// different (or no) default tmux server; it still finds the live window on
// the socket start recorded, and keeps the claim.
func TestStartRecordsTheTmuxSocket(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	a, b := newTask(t, h, "bugfix", "Long run"), newTask(t, h, "bugfix", "Next")
	h.mustRun("plan", a, "api/**")
	h.mustRun("start", a)
	if got := h.task(a); got.TmuxSocket == "" || !filepath.IsAbs(got.TmuxSocket) {
		t.Fatalf("start recorded socket %q", got.TmuxSocket)
	}
	sqlExec(t, p, `UPDATE tasks SET last_seen_at = '2026-01-01T00:00:00Z', started_at = '2026-01-01T00:00:00Z' WHERE slug = ?`, a)
	sqlExec(t, p, `UPDATE file_claims SET created_at = '2026-01-01T00:00:00Z'`)
	h.env.TmuxArgs = []string{"-S", filepath.Join(t.TempDir(), "other-server")} // not the server the task runs on
	_, out, errOut := h.run("doctor", "--fix")
	if strings.Contains(out+errOut, "is stale") || strings.Contains(out+errOut, "is gone") {
		t.Fatalf("doctor from another tmux server released a live claim:\n%s\n%s", out, errOut)
	}
	h.expectCode(ExitState, "plan", b, "api/x.go")
}

// TestUnknownTmuxNeverExpiresClaims: a missing tmux binary or an unexpected
// tmux error is unknown, not "no windows"; only a clean "no server" on the
// recorded socket lets an old claim go stale.
func TestUnknownTmuxNeverExpiresClaims(t *testing.T) {
	h := newHarness(t)
	p := h.initWith(junitConfig)
	a, b := newTask(t, h, "bugfix", "Abandoned"), newTask(t, h, "bugfix", "Next")
	h.mustRun("plan", a, "api/**")
	gone := shortSocketPath(t)
	sqlExec(t, p, `UPDATE tasks SET stage = 'in-progress', tmux_window = '@7', tmux_socket = ?, last_seen_at = '2026-01-01T00:00:00Z', started_at = '2026-01-01T00:00:00Z' WHERE slug = ?`, gone, a)
	sqlExec(t, p, `UPDATE file_claims SET created_at = '2026-01-01T00:00:00Z'`)

	h.env.Tmux = filepath.Join(t.TempDir(), "no-tmux-here")
	h.expectCode(ExitState, "plan", b, "api/x.go")
	_, out, errOut := h.run("doctor", "--fix")
	mustContain(t, "doctor without tmux", out+errOut, "tmux did not answer")
	if strings.Contains(out+errOut, "released") || strings.Contains(out+errOut, "forgot") {
		t.Fatalf("doctor without tmux released claims:\n%s\n%s", out, errOut)
	}
	if got := h.task(a); got.TmuxWindow != "@7" {
		t.Fatalf("window forgotten without tmux: %+v", got)
	}

	if _, err := exec.LookPath("tmux"); err == nil && runtime.GOOS != "windows" {
		fake := filepath.Join(t.TempDir(), "tmux")
		testutil.WriteFile(t, fake, "#!/bin/sh\necho 'error connecting to "+gone+" (Permission denied)' >&2\nexit 1\n")
		if err := os.Chmod(fake, 0o755); err != nil {
			t.Fatal(err)
		}
		h.env.Tmux = fake
		h.expectCode(ExitState, "plan", b, "api/x.go")

		h.env.Tmux = "tmux"
		mustContain(t, "clean no server", h.mustRun("plan", b, "api/x.go"), "stale: "+a, "ready")
	}
}

// shortSocketPath is a socket path nothing listens on, short enough for a
// unix socket (t.TempDir() on macOS is not).
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(shortTempRoot(), "shoal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "dead-server")
}

func shortTempRoot() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return "/tmp"
}
