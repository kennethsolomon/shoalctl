package cli

import (
	"context"
	"path/filepath"
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
	h := newHarness(t)
	p := h.initWith(junitConfig)
	a, b := newTask(t, h, "bugfix", "Abandoned"), newTask(t, h, "bugfix", "Next")
	h.mustRun("plan", a, "api/**")
	// a started long ago and its window is gone.
	sqlExec(t, p, `UPDATE tasks SET stage = 'in-progress', tmux_window = '@999', last_seen_at = '2026-01-01T00:00:00Z', started_at = '2026-01-01T00:00:00Z' WHERE slug = ?`, a)
	sqlExec(t, p, `UPDATE file_claims SET created_at = '2026-01-01T00:00:00Z'`)
	mustContain(t, "stale", h.mustRun("plan", b, "api/x.go"), "stale: "+a+" (in-progress)", "doctor --fix releases", "ready")
	// A recent heartbeat makes the claim live again.
	sqlExec(t, p, `UPDATE tasks SET last_seen_at = ? WHERE slug = ?`, nowUTC(), a)
	h.expectCode(ExitState, "plan", b, "api/x.go")
	// Started tasks are not re-planned.
	h.expectCode(ExitState, "plan", a, "api/**")
}
