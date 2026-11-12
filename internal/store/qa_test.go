package store

import (
	"context"
	"database/sql"
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
