package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A qa round keeps the commit and tree it ran on; an agent check is a qa run
// with a verdict, and a qa round's own run (no verdict) is never one.
func TestQACodeIdentity(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := memoryTask(t, s, "QA code")
	start := func(kind string) Run {
		t.Helper()
		r, err := s.StartRun(ctx, Run{TaskID: task.ID, Kind: kind, SHA: "c1", Command: "x", CommandHash: "h", Format: "junit", Fidelity: "per-test"},
			func(id int64) string { return "out.log" })
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if _, ok, err := s.LatestAgentQA(ctx, task.ID); err != nil || ok {
		t.Fatalf("no runs yet: ok %v, %v", ok, err)
	}
	human := start("qa")
	if err := s.write(ctx, func(tx *sql.Tx) error {
		return InsertQARound(ctx, tx, QARound{TaskID: task.ID, Round: 1, RunID: human.ID, Passed: true, Failures: "[]", HeadSHA: "c1", Tree: "t1"})
	}); err != nil {
		t.Fatal(err)
	}
	rounds, err := s.QARounds(ctx, task.ID)
	if err != nil || len(rounds) != 1 || rounds[0].HeadSHA != "c1" || rounds[0].Tree != "t1" || !rounds[0].Passed {
		t.Fatalf("rounds %+v, %v", rounds, err)
	}
	if _, ok, err := s.LatestAgentQA(ctx, task.ID); err != nil || ok {
		t.Fatalf("a qa round's run counted as an agent check: ok %v, %v", ok, err)
	}
	for i, passed := range []bool{false, true} {
		r := start("qa")
		if err := s.SetRunVerdict(ctx, r.ID, passed, "t2"); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.LatestAgentQA(ctx, task.ID)
		if err != nil || !ok || got != (AgentQA{RunID: r.ID, SHA: "c1", Tree: "t2", Passed: passed}) {
			t.Fatalf("check %d: %+v ok %v, %v", i, got, ok, err)
		}
	}
	gate := start("gate")
	if err := s.SetRunVerdict(ctx, gate.ID, false, "t3"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.LatestAgentQA(ctx, task.ID); got.RunID == gate.ID {
		t.Fatal("a gate run counted as an agent qa check")
	}
}

// Migration 9 keeps the qa rounds and test runs recorded before it and gives
// them an empty tree, so a pass from then shows as stale.
func TestQACodeMigrationKeepsOldRows(t *testing.T) {
	migs, _ := Migrations()
	if len(migs) < 9 || migs[8].Name != "qa_code" {
		t.Fatalf("migrations: %+v", migs)
	}
	path := filepath.Join(t.TempDir(), "memory.db")
	ctx := context.Background()
	old, err := open(ctx, path, testOpts(), migs[:8])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO tasks (id, slug, lane, title, ask, stage, created_at, updated_at) VALUES (1, '1-a', 'feature', 'A', 'a', 'qa', 'x', 'x')`,
		`INSERT INTO test_runs (id, task_id, kind, sha, command, command_hash, format, fidelity, status, output_path, started_at)
			VALUES (7, 1, 'qa', 'c1', 'x', 'h', 'junit', 'per-test', 'completed', 'out.log', 'x')`,
		`INSERT INTO qa_rounds (task_id, round, run_id, passed, failures, created_at) VALUES (1, 1, 7, 1, '[]', 'x')`,
	} {
		if _, err := old.db.Exec(q); err != nil {
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
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 9 {
		t.Fatalf("user_version = %d, %v", v, err)
	}
	var runSHA, runTree, verdict string
	if err := s.db.QueryRow(`SELECT sha, tree, verdict FROM test_runs WHERE id = 7`).Scan(&runSHA, &runTree, &verdict); err != nil ||
		runSHA != "c1" || runTree != "" || verdict != "" {
		t.Fatalf("test run: sha %q tree %q verdict %q, %v", runSHA, runTree, verdict, err)
	}
	rounds, err := s.QARounds(ctx, 1)
	if err != nil || len(rounds) != 1 || rounds[0].RunID != 7 || !rounds[0].Passed || rounds[0].HeadSHA != "" || rounds[0].Tree != "" {
		t.Fatalf("rounds %+v, %v", rounds, err)
	}
	if _, ok, err := s.LatestAgentQA(ctx, 1); err != nil || ok {
		t.Fatalf("an old qa run counted as an agent check: ok %v, %v", ok, err)
	}
}
