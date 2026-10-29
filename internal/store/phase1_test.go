package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPhase1Schema(t *testing.T) {
	s, _ := openTest(t)
	snap := schemaSnapshot(t, s)
	for _, want := range []string{"table:file_claims:", "table:messages:", "table:message_reads:", "table:contracts:",
		"table:test_quarantine:", "table:coverage_files:", "table:regression_proofs:", "table:scope_items:", "table:scope_cases:",
		"table:regression_overrides:", "table:qa_rounds:", "qa_round INTEGER NOT NULL DEFAULT 0", "parent_task_id", "merged_sha"} {
		if !strings.Contains(snap, want) {
			t.Errorf("schema lacks %q", want)
		}
	}
	migs, _ := Migrations()
	if len(migs) != 3 || migs[2].Name != "phase1" {
		t.Fatalf("migrations: %+v", migs)
	}
}

func TestCreateTaskWithScopeAndParent(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	parent, err := s.CreateTask(ctx, "hotfix", "Fix prod", "Fix prod", slugger)
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateTaskWith(ctx, NewTask{Lane: "bugfix", Title: "Backport", Ask: "x", ParentID: parent.ID,
		Scope: []string{"also test CSV", "also test empty"}}, slugger)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentTaskID != parent.ID || child.QARound != 0 {
		t.Fatalf("child: %+v", child)
	}
	items, err := s.ScopeItems(ctx, child.ID)
	if err != nil || len(items) != 2 || items[0].Source != ScopeUser || items[1].Text != "also test empty" {
		t.Fatalf("scope items %+v, %v", items, err)
	}
	kids, err := s.Children(ctx, parent.ID)
	if err != nil || len(kids) != 1 || kids[0].ID != child.ID {
		t.Fatalf("children %+v, %v", kids, err)
	}
}

func TestEffectiveResultsAndFlakyCounts(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "feature", "T", "T", slugger)
	run, err := s.StartRun(ctx, Run{TaskID: task.ID, Kind: "baseline", SHA: "abc", Command: "c", CommandHash: "h", Format: "junit", Fidelity: "per-test"},
		func(int64) string { return "/x/output.log" })
	if err != nil {
		t.Fatal(err)
	}
	res := []Result{
		{TestID: "a", Status: "pass"},
		{TestID: "flaky", Status: "fail", Message: "boom"},
		{TestID: "hard", Status: "fail"},
		{TestID: "gone", Status: "fail"},
		{TestID: "s", Status: "skip"},
		{TestID: "flaky", Status: "fail", Attempt: 2},
		{TestID: "hard", Status: "fail", Attempt: 2},
		{TestID: "flaky", Status: "pass", Attempt: 3},
		{TestID: "hard", Status: "fail", Attempt: 3},
	}
	got, err := s.FinishRun(ctx, run.ID, "completed", nil, res)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 5 || got.Passed != 2 || got.Failed != 2 || got.Skipped != 1 {
		t.Fatalf("counts %+v", got)
	}
	var attempts int
	if err := s.db.QueryRow(`SELECT attempts FROM test_runs WHERE id = ?`, run.ID).Scan(&attempts); err != nil || attempts != 3 {
		t.Fatalf("attempts %d %v", attempts, err)
	}
	eff, err := s.EffectiveResults(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "pass", "flaky": StatusFlaky, "hard": "fail", "gone": "fail", "s": "skip"}
	if len(eff) != len(want) {
		t.Fatalf("effective %+v", eff)
	}
	for _, r := range eff {
		if want[r.TestID] != r.Status {
			t.Errorf("%s: %s, want %s", r.TestID, r.Status, want[r.TestID])
		}
		if r.TestID == "flaky" && r.Message != "boom" {
			t.Errorf("flaky message %q", r.Message)
		}
	}
	if latest, ok, err := s.LatestRun(ctx, task.ID, "baseline"); err != nil || !ok || latest.ID != run.ID {
		t.Fatalf("latest %+v %v %v", latest, ok, err)
	}

	n, err := s.QuarantineTests(ctx, run.ID, []Result{{TestID: "flaky", Message: "boom"}})
	if err != nil || n != 1 {
		t.Fatalf("quarantine %d %v", n, err)
	}
	if n, _ := s.QuarantineTests(ctx, run.ID, []Result{{TestID: "flaky"}}); n != 0 {
		t.Fatalf("second quarantine of an open test added %d", n)
	}
	q, err := s.Quarantined(ctx, false)
	if err != nil || len(q) != 1 || q[0].TestID != "flaky" || q[0].DetectedRunID != run.ID || !strings.Contains(q[0].Reason, "boom") {
		t.Fatalf("quarantined %+v %v", q, err)
	}
	if err := s.ReleaseQuarantine(ctx, "flaky"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseQuarantine(ctx, "flaky"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second release: %v", err)
	}
	if q, _ := s.Quarantined(ctx, false); len(q) != 0 {
		t.Fatalf("open after release: %+v", q)
	}
	if q, _ := s.Quarantined(ctx, true); len(q) != 1 || q[0].ReleasedAt == "" {
		t.Fatalf("history: %+v", q)
	}
}

func TestInboxReadOnceAndBroadcast(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "feature", "T", "T", slugger)
	other, _ := s.CreateTask(ctx, "feature", "U", "U", slugger)
	q, err := s.SendMessage(ctx, Message{TaskID: task.ID, From: "shoal-backend", To: "shoal-lead", Kind: "question", Subject: "s", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, Message{TaskID: task.ID, From: "shoal-lead", To: "*", Kind: "info", Subject: "all", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, Message{TaskID: other.ID, From: "shoal-lead", To: "shoal-backend", Kind: "answer", Subject: "x", Body: "b", ReplyTo: q}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-task reply: %v", err)
	}
	if _, err := s.SendMessage(ctx, Message{TaskID: task.ID, From: "shoal-lead", To: "shoal-backend", Kind: "answer", Subject: "x", Body: "b"}); err == nil {
		t.Fatal("answer without reply_to accepted")
	}

	lead, err := s.Inbox(ctx, task.ID, "shoal-lead", false)
	if err != nil || len(lead) != 1 || lead[0].ID != q {
		t.Fatalf("lead inbox %+v %v (own broadcast must not show)", lead, err)
	}
	if again, _ := s.Inbox(ctx, task.ID, "shoal-lead", false); len(again) != 0 {
		t.Fatalf("read twice: %+v", again)
	}
	if all, _ := s.Inbox(ctx, task.ID, "shoal-lead", true); len(all) != 1 || !all[0].Read {
		t.Fatalf("--all: %+v", all)
	}
	for _, agent := range []string{"shoal-backend", "shoal-frontend"} {
		if n, _ := s.UnreadCount(ctx, task.ID, agent); n != 1 {
			t.Fatalf("%s unread = %d", agent, n)
		}
		if in, _ := s.Inbox(ctx, task.ID, agent, false); len(in) != 1 || in[0].To != "*" {
			t.Fatalf("%s inbox %+v", agent, in)
		}
	}
}

func TestFreezeContractVersions(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "feature", "T", "T", slugger)
	c, changed, err := s.FreezeContract(ctx, task.ID, "h1", "shoal-lead", "")
	if err != nil || !changed || c.Version != 1 {
		t.Fatalf("v1 %+v %v %v", c, changed, err)
	}
	if c, changed, _ := s.FreezeContract(ctx, task.ID, "h1", "shoal-lead", ""); changed || c.Version != 1 {
		t.Fatalf("same hash bumped: %+v", c)
	}
	if _, _, err := s.FreezeContract(ctx, task.ID, "h2", "shoal-lead", ""); !errors.Is(err, ErrStage) {
		t.Fatalf("v2 without note: %v", err)
	}
	c, changed, err = s.FreezeContract(ctx, task.ID, "h2", "shoal-lead", "renamed field id to client_id")
	if err != nil || !changed || c.Version != 2 {
		t.Fatalf("v2 %+v %v", c, err)
	}
	in, _ := s.Inbox(ctx, task.ID, "shoal-frontend", false)
	if len(in) != 1 || in[0].Kind != "contract-change" || in[0].Body != "renamed field id to client_id" {
		t.Fatalf("broadcast %+v", in)
	}
	if latest, ok, _ := s.LatestContract(ctx, task.ID); !ok || latest.SHA256 != "h2" {
		t.Fatalf("latest %+v", latest)
	}
}

func TestClaimsPerAgent(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "feature", "T", "T", slugger)
	same := func(a, b string) bool { return a == b }
	if n, err := s.AddClaims(ctx, task.ID, "shoal-backend", []string{"api/**", "db/x.sql"}, same); err != nil || n != 2 {
		t.Fatalf("claim %d %v", n, err)
	}
	if n, _ := s.AddClaims(ctx, task.ID, "shoal-backend", []string{"api/**"}, same); n != 0 {
		t.Fatalf("re-claim added %d", n)
	}
	if _, err := s.AddClaims(ctx, task.ID, "shoal-frontend", []string{"ui/**", "api/**"}, same); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("conflict: %v", err)
	}
	cl, _ := s.Claims(ctx, task.ID)
	if len(cl) != 2 {
		t.Fatalf("a refused call must add nothing: %+v", cl)
	}
	if n, err := s.ReleaseClaims(ctx, task.ID, "shoal-backend", []string{"api/**"}); err != nil || n != 1 {
		t.Fatalf("release %d %v", n, err)
	}
	if n, _ := s.AddClaims(ctx, task.ID, "shoal-frontend", []string{"api/**"}, same); n != 1 {
		t.Fatal("released glob not claimable")
	}
	if n, _ := s.ReleaseClaims(ctx, task.ID, "shoal-backend", nil); n != 1 {
		t.Fatalf("release all = %d", n)
	}
}

func TestScopeCasesAndProofs(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "feature", "T", "T", slugger)
	other, _ := s.CreateTask(ctx, "feature", "U", "U", slugger)
	item, err := s.AddScopeItem(ctx, task.ID, ScopeUser, "also test CSV")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddScopeCase(ctx, other.ID, ScopeCase{ItemID: item, Category: "happy", Description: "d", Check: CheckAutomated, TestID: "C::a"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("case on another task's item: %v", err)
	}
	if _, err := s.AddScopeCase(ctx, task.ID, ScopeCase{ItemID: item, Category: "happy", Description: "d", Check: CheckAutomated}); err == nil {
		t.Fatal("automated case without a test id accepted")
	}
	id, err := s.AddScopeCase(ctx, task.ID, ScopeCase{ItemID: item, Category: "ui-state", Description: "d", Check: CheckManual, ManualStep: "open it"})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := s.ScopeItems(ctx, task.ID)
	c := items[0].Cases[0]
	if c.ID != id || c.Status != "pending" {
		t.Fatalf("case %+v", c)
	}
	if err := s.DecideCase(ctx, c, "waived", "no UI", "", "Kenneth"); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideCase(ctx, c, "pass", "", "", "Kenneth"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	items, _ = s.ScopeItems(ctx, task.ID)
	if c := items[0].Cases[0]; c.Status != "waived" || c.WaiveReason != "no UI" || c.DecidedBy != "Kenneth" {
		t.Fatalf("decided %+v", c)
	}

	run, _ := s.StartRun(ctx, Run{TaskID: task.ID, Kind: "repro", SHA: "r", Command: "c", CommandHash: "h", Format: "junit", Fidelity: "per-test"},
		func(int64) string { return "/x" })
	if err := s.SetGreen(ctx, task.ID, "C::a", run.ID, "g", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("green before red: %v", err)
	}
	if err := s.SetRed(ctx, Proof{TaskID: task.ID, TestID: "C::a", RedStatus: "fail", RedRunID: run.ID, RedSHA: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGreen(ctx, task.ID, "C::a", run.ID, "g", true); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Proofs(ctx, task.ID)
	if len(ps) != 1 || ps[0].GreenSHA != "g" || !ps[0].TestChanged {
		t.Fatalf("proofs %+v", ps)
	}
	if err := s.SetRed(ctx, Proof{TaskID: task.ID, TestID: "C::a", RedStatus: "fail", RedRunID: run.ID, RedSHA: "r2"}); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.Proofs(ctx, task.ID); ps[0].GreenSHA != "" || ps[0].RedSHA != "r2" {
		t.Fatalf("new red must clear green: %+v", ps)
	}
}
