package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestPhase2Schema(t *testing.T) {
	s, _ := openTest(t)
	snap := schemaSnapshot(t, s)
	for _, want := range []string{"table:task_blockers:", "table:merge_queue:", "prev_stage TEXT", "folded_into INTEGER", "attention TEXT", "attention_at TEXT",
		"index:file_claims_open:", "'waiting','rebasing','testing','merged','failed'"} {
		if !strings.Contains(snap, want) {
			t.Errorf("schema lacks %q", want)
		}
	}
	migs, _ := Migrations()
	if len(migs) != 4 || migs[3].Name != "phase2" {
		t.Fatalf("migrations: %+v", migs)
	}
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 4 {
		t.Fatalf("user_version = %d, %v", v, err)
	}
}

// A Phase 1 database with done tasks gets them queued in done order.
func TestPhase2MigrationQueuesDoneTasks(t *testing.T) {
	migs, _ := Migrations()
	path := filepath.Join(t.TempDir(), "memory.db")
	ctx := context.Background()
	old, err := open(ctx, path, testOpts(), migs[:3])
	if err != nil {
		t.Fatal(err)
	}
	for i, closed := range []string{"2026-01-03T00:00:00Z", "2026-01-01T00:00:00Z", ""} {
		stage := StageDone
		if i == 2 {
			stage = StageInProgress
		}
		if _, err := old.db.Exec(`INSERT INTO tasks (id, slug, lane, title, ask, stage, closed_at, created_at, updated_at)
			VALUES (?, ?, 'feature', 'T', 'ask', ?, NULLIF(?, ''), 'x', 'x')`, i+1, slugger(int64(i+1)), stage, closed); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	s, err := Open(ctx, path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 4 {
		t.Fatalf("user_version = %d, %v", v, err)
	}
	q, err := s.Queue(ctx)
	if err != nil || len(q) != 2 || q[0].TaskID != 2 || q[1].TaskID != 1 || q[0].State != QueueWaiting {
		t.Fatalf("queue %+v, %v", q, err)
	}
	done, err := s.QueuedTasks(ctx)
	if err != nil || len(done) != 2 || done[0].ID != 2 {
		t.Fatalf("queued tasks %+v, %v", done, err)
	}
}

func planReq(t Task, globs []string, decisions map[int64]byte) PlanRequest {
	return PlanRequest{Task: t, Globs: globs, Decisions: decisions, Human: "human",
		Overlaps: func(a, b string) bool {
			return a == b || strings.HasPrefix(b, strings.TrimSuffix(a, "**")) || strings.HasPrefix(a, strings.TrimSuffix(b, "**"))
		},
		Stale: func(Task, Claim) bool { return false }}
}

func newTasks(t *testing.T, s *Store, n int) []Task {
	t.Helper()
	var out []Task
	for i := 0; i < n; i++ {
		tk, err := s.CreateTaskWith(context.Background(), NewTask{Lane: "bugfix", Title: "Bug", Ask: "fix it", Scope: []string{"also test sort"}}, slugger)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tk)
	}
	return out
}

func mustPlan(t *testing.T, s *Store, req PlanRequest) PlanResult {
	t.Helper()
	cur, err := s.taskByID(context.Background(), req.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.Task = cur
	res, err := s.PlanClaims(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestPlanClaimsChoices(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	ts := newTasks(t, s, 4)
	a, b, c, d := ts[0], ts[1], ts[2], ts[3]

	if res := mustPlan(t, s, planReq(a, []string{"app/User.php", "js/users/**"}, nil)); !res.Applied || res.Task.Stage != StageReady {
		t.Fatalf("plan a: %+v", res)
	}
	// b overlaps a: nothing is written until the human decides.
	res := mustPlan(t, s, planReq(b, []string{"js/users/**", "app/UserController.php"}, nil))
	if res.Applied || len(res.Undecided) != 1 || res.Undecided[0].Task.ID != a.ID || res.Undecided[0].Pairs[0] != [2]string{"js/users/**", "js/users/**"} {
		t.Fatalf("overlap: %+v", res)
	}
	if cl, _ := s.Claims(ctx, b.ID); len(cl) != 0 {
		t.Fatalf("undecided plan wrote claims: %+v", cl)
	}
	res = mustPlan(t, s, planReq(b, []string{"js/users/**", "app/UserController.php"}, map[int64]byte{a.ID: ChoiceSerialize}))
	if !res.Applied || res.Task.Stage != StageBlocked || res.Task.BlockedBy != a.ID {
		t.Fatalf("serialize: %+v", res.Task)
	}
	// a re-planned onto b's paths cannot serialize behind b: b waits on a.
	res = mustPlan(t, s, planReq(a, []string{"app/UserController.php"}, nil))
	if len(res.Undecided) != 1 || res.Undecided[0].CanSerialize || !res.Undecided[0].CanFold {
		t.Fatalf("cycle: %+v", res.Undecided)
	}
	if res = mustPlan(t, s, planReq(a, []string{"app/UserController.php"}, map[int64]byte{b.ID: ChoiceSerialize})); res.Applied {
		t.Fatal("a cycle-forming serialize was applied")
	}
	// c runs in parallel with both.
	res = mustPlan(t, s, planReq(c, []string{"js/users/**"}, map[int64]byte{a.ID: ChoiceParallel, b.ID: ChoiceParallel}))
	if !res.Applied || res.Task.Stage != StageReady {
		t.Fatalf("parallel: %+v", res)
	}
	// d is folded into a: claims, scope and the ask move; d closes.
	res = mustPlan(t, s, planReq(d, []string{"js/users/list.js", "css/users.css"},
		map[int64]byte{a.ID: ChoiceMerge, b.ID: ChoiceParallel, c.ID: ChoiceParallel}))
	if !res.Applied || res.Folded == nil || res.Folded.ID != a.ID || res.Task.Stage != StageFolded || res.Task.FoldedInto != a.ID || res.Task.ClosedAt == "" {
		t.Fatalf("fold: %+v", res)
	}
	got, _ := s.taskByID(ctx, a.ID)
	if !strings.Contains(got.Ask, "Folded in from task 4-task") {
		t.Fatalf("a's ask: %q", got.Ask)
	}
	items, _ := s.ScopeItems(ctx, a.ID)
	if len(items) != 2 {
		t.Fatalf("a's scope items: %+v", items)
	}
	cl, _ := s.Claims(ctx, a.ID)
	if len(cl) != 4 || cl[3].Glob != "css/users.css" || cl[3].Agent != PlanAgent {
		t.Fatalf("a's claims: %+v", cl)
	}
	if cl, _ := s.Claims(ctx, d.ID); len(cl) != 0 {
		t.Fatalf("folded task kept claims: %+v", cl)
	}
	msgs, _ := s.Inbox(ctx, a.ID, "shoal-lead", true)
	if len(msgs) != 1 || msgs[0].Kind != "info" || !strings.Contains(msgs[0].Body, "css/users.css") {
		t.Fatalf("fold message: %+v", msgs)
	}
	// A stale overlap never blocks.
	req := planReq(newTasks(t, s, 1)[0], []string{"js/users/**"}, nil)
	req.Stale = func(Task, Claim) bool { return true }
	if res := mustPlan(t, s, req); !res.Applied || len(res.Overlaps) != 3 || !res.Overlaps[0].Stale {
		t.Fatalf("stale: %+v", res)
	}
}

func TestPlanClaimsVersionConflict(t *testing.T) {
	s, _ := openTest(t)
	a := newTasks(t, s, 1)[0]
	if _, err := s.db.Exec(`UPDATE tasks SET version = version + 1 WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.PlanClaims(context.Background(), planReq(a, []string{"x"}, nil))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if cl, _ := s.Claims(context.Background(), a.ID); len(cl) != 0 {
		t.Fatalf("conflicting plan wrote claims: %+v", cl)
	}
}

func TestUnblockAfterMergeWaitsForEveryBlocker(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	ts := newTasks(t, s, 3)
	mustPlan(t, s, planReq(ts[0], []string{"a/**"}, nil))
	mustPlan(t, s, planReq(ts[1], []string{"b/**"}, nil))
	res := mustPlan(t, s, planReq(ts[2], []string{"a/**", "b/**"}, map[int64]byte{ts[0].ID: ChoiceSerialize, ts[1].ID: ChoiceSerialize}))
	if res.Task.BlockedBy != ts[0].ID {
		t.Fatalf("blocked by %d", res.Task.BlockedBy)
	}
	unblock := func(id int64) []int64 {
		var ready []int64
		if err := s.write(ctx, func(tx *sql.Tx) error {
			var err error
			ready, err = UnblockAfterMerge(ctx, tx, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ready
	}
	if ready := unblock(ts[0].ID); len(ready) != 0 {
		t.Fatalf("unblocked early: %v", ready)
	}
	if got, _ := s.taskByID(ctx, ts[2].ID); got.Stage != StageBlocked || got.BlockedBy != ts[1].ID {
		t.Fatalf("after first merge: %+v", got)
	}
	if ready := unblock(ts[1].ID); len(ready) != 1 || ready[0] != ts[2].ID {
		t.Fatalf("ready = %v", ready)
	}
	if got, _ := s.taskByID(ctx, ts[2].ID); got.Stage != StageReady || got.BlockedBy != 0 {
		t.Fatalf("after second merge: %+v", got)
	}
}

func TestPlanClaimsDoNotConflictWithAgentClaims(t *testing.T) {
	s, _ := openTest(t)
	a := newTasks(t, s, 1)[0]
	mustPlan(t, s, planReq(a, []string{"api/**"}, nil))
	if _, err := s.AddClaims(context.Background(), a.ID, "shoal-backend", []string{"api/**"}, func(x, y string) bool { return x == y }); err != nil {
		t.Fatalf("agent claim inside a planned glob: %v", err)
	}
}

func TestQueueAttentionAndStaleRelease(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	ts := newTasks(t, s, 2)
	for _, tk := range []Task{ts[1], ts[0]} {
		if err := s.write(ctx, func(tx *sql.Tx) error { return Enqueue(ctx, tx, tk.ID) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetQueue(ctx, ts[1].ID, QueueFailed); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error { return Enqueue(ctx, tx, ts[1].ID) }); err != nil {
		t.Fatal(err)
	}
	q, _ := s.Queue(ctx)
	if len(q) != 2 || q[0].TaskID != ts[0].ID || q[1].TaskID != ts[1].ID || q[1].State != QueueWaiting || q[1].Position != 3 {
		t.Fatalf("re-enqueued task should go last: %+v", q)
	}

	if changed, err := s.SetAttention(ctx, ts[0].ID, "dialog: Do you trust the files in this folder?"); err != nil || !changed {
		t.Fatalf("set attention: %v %v", changed, err)
	}
	if changed, _ := s.SetAttention(ctx, ts[0].ID, "dialog: Do you trust the files in this folder?"); changed {
		t.Fatal("same attention reported as a change")
	}
	got, _ := s.taskByID(ctx, ts[0].ID)
	if got.Attention == "" || got.AttentionAt == "" || got.Version != ts[0].Version {
		t.Fatalf("attention: %+v", got)
	}
	s.SetAttention(ctx, ts[0].ID, "")
	if got, _ := s.taskByID(ctx, ts[0].ID); got.Attention != "" || got.AttentionAt != "" {
		t.Fatalf("cleared attention: %+v", got)
	}

	mustPlan(t, s, planReq(ts[0], []string{"x/**"}, nil))
	cl, _ := s.Claims(ctx, ts[0].ID)
	stale := true
	check := func(Task, Claim) bool { return stale }
	stale = false
	if ok, err := s.ReleaseStaleClaim(ctx, cl[0], check); err != nil || ok {
		t.Fatalf("released a claim that became live: %v %v", ok, err)
	}
	stale = true
	if ok, err := s.ReleaseStaleClaim(ctx, cl[0], check); err != nil || !ok {
		t.Fatalf("release stale: %v %v", ok, err)
	}
	if ok, _ := s.ReleaseStaleClaim(ctx, cl[0], check); ok {
		t.Fatal("released twice")
	}
}
