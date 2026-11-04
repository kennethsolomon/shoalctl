package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPhase3Schema(t *testing.T) {
	s, _ := openTest(t)
	snap := schemaSnapshot(t, s)
	for _, want := range []string{"table:bugs:", "table:decisions:", "table:lessons:", "table:qa:", "table:hook_marks:",
		"table:memory_fts:", "USING fts5", "trigger:summaries_fts_insert:", "regression_tests TEXT", "path_glob    TEXT"} {
		if !strings.Contains(snap, want) {
			t.Errorf("schema lacks %q", want)
		}
	}
	migs, _ := Migrations()
	if len(migs) < 6 || migs[5].Name != "memory" {
		t.Fatalf("migrations: %+v", migs)
	}
}

// A summary written before migration 6 is searchable after it.
func TestPhase3MigrationIndexesOldSummaries(t *testing.T) {
	migs, _ := Migrations()
	path := filepath.Join(t.TempDir(), "memory.db")
	ctx := context.Background()
	old, err := open(ctx, path, testOpts(), migs[:5])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`INSERT INTO tasks (id, slug, lane, title, ask, stage, created_at, updated_at) VALUES (1, '1-x', 'feature', 'T', 'a', 'done', 'x', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`INSERT INTO summaries (task_id, decision, why, verification, commits, files, true_at_sha, report_path, created_at)
		VALUES (1, 'use a strategy object', 'pluggable exporters', '{}', '[]', '[]', 'abc', 'r', 'x')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(ctx, path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hits, err := s.Search(ctx, "exporters", 10)
	if err != nil || len(hits) != 1 || hits[0] != (SearchHit{Kind: KindSummary, Ref: 1}) {
		t.Fatalf("hits %+v, %v", hits, err)
	}
}

func memoryTask(t *testing.T, s *Store, title string) Task {
	t.Helper()
	task, err := s.CreateTask(context.Background(), "bugfix", title, title, slugger)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestRecordsAndSearch(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := memoryTask(t, s, "Users page")
	lid, err := s.AddLesson(ctx, Lesson{TaskID: task.ID, QARound: 1, Pattern: "the list view reads another accessor", Fix: "use displayName everywhere",
		Stack: "laravel", PathGlob: "resources/js/users/**", TrueAtSHA: "aaa"})
	if err != nil {
		t.Fatal(err)
	}
	did, err := s.AddDecision(ctx, Decision{TaskID: task.ID, Topic: "exports", Choice: "strategy pattern", Rationale: "pluggable", Files: []string{"app/Export.php"}, TrueAtSHA: "bbb"})
	if err != nil {
		t.Fatal(err)
	}
	qid, err := s.AddQANote(ctx, QANote{TaskID: task.ID, Question: "why queue emails?", Answer: "the SMTP relay throttles", TrueAtSHA: "ccc"})
	if err != nil {
		t.Fatal(err)
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		return UpsertBugTx(ctx, tx, Bug{TaskID: task.ID, Symptom: "wrong display name", RootCause: "falls back to email", FixSHA: "ddd",
			Files: []string{"app/Models/User.php"}, Tests: []RegressionTest{{Test: "C::bug", File: "tests/bug.t"}}, TrueAtSHA: "ddd"})
	})
	if err != nil {
		t.Fatal(err)
	}
	// A second done replaces the bug record instead of adding one.
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := UpsertBugTx(ctx, tx, Bug{TaskID: task.ID, Symptom: "wrong display name", RootCause: "first_name null falls back to email", FixSHA: "eee", TrueAtSHA: "eee"}); err != nil {
			return err
		}
		return MoveBugFixTx(ctx, tx, task.ID, "fff")
	})
	if err != nil {
		t.Fatal(err)
	}
	bugs, err := s.Bugs(ctx)
	if err != nil || len(bugs) != 1 || bugs[0].FixSHA != "fff" || bugs[0].TrueAtSHA != "fff" || !strings.Contains(bugs[0].RootCause, "first_name") || len(bugs[0].Files) != 0 {
		t.Fatalf("bugs %+v, %v", bugs, err)
	}

	for _, tt := range []struct {
		q    string
		want SearchHit
	}{
		{"accessor", SearchHit{KindLesson, lid}},
		{"strategy", SearchHit{KindDecision, did}},
		{"throttled smtp", SearchHit{KindQA, qid}}, // porter stemming: throttled ~ throttles
		{"first_name", SearchHit{KindBug, bugs[0].ID}},
	} {
		hits, err := s.Search(ctx, tt.q, 10)
		if err != nil || len(hits) != 1 || hits[0] != tt.want {
			t.Errorf("search %q: %+v, %v; want %+v", tt.q, hits, err, tt.want)
		}
	}
	// FTS5 syntax in user input is matched literally, never parsed.
	for _, q := range []string{`"unbalanced`, `a OR b NOT`, `col:x*`, `(`} {
		if _, err := s.Search(ctx, q, 10); err != nil {
			t.Errorf("search %q: %v", q, err)
		}
	}
	if hits, err := s.Search(ctx, "  ", 10); err != nil || hits != nil {
		t.Errorf("blank search: %+v, %v", hits, err)
	}

	changed, err := s.FlagStale(ctx, KindLesson, lid, "accessor renamed")
	if err != nil || !changed {
		t.Fatalf("flag stale: %v %v", changed, err)
	}
	if changed, err := s.FlagStale(ctx, KindLesson, lid, "again"); err != nil || changed {
		t.Fatalf("second flag: %v %v", changed, err)
	}
	ls, _ := s.Lessons(ctx)
	if len(ls) != 1 || !ls[0].Stale || ls[0].StaleReason != "accessor renamed" || ls[0].QARound != 1 {
		t.Fatalf("lessons %+v", ls)
	}
	if _, err := s.FlagStale(ctx, KindDecision, 99, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("flag a missing record: %v", err)
	}
	if hits, _ := s.Search(ctx, "accessor", 10); len(hits) != 1 {
		t.Fatalf("a stale record must stay searchable: %+v", hits)
	}
}

func TestParseRecordRef(t *testing.T) {
	for _, ok := range []string{"bug:1", "decision:22", "lesson:3", "qa:4"} {
		if _, _, err := ParseRecordRef(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"bug", "bug:", "bug:0", "bug:-1", "bug:x", "summary:1", "tasks:1", "lesson:1;DROP"} {
		if _, _, err := ParseRecordRef(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestMarkCountsLessonHitsOncePerSession(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := memoryTask(t, s, "T")
	lid, err := s.AddLesson(ctx, Lesson{TaskID: task.ID, Pattern: "p", Fix: "f", PathGlob: "a/**", TrueAtSHA: "x"})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{LessonKey(lid), "outside:shoal-backend:a/b.go"}
	got, err := s.Mark(ctx, "s1", task.ID, keys)
	if err != nil || len(got) != 2 {
		t.Fatalf("first mark: %v %v", got, err)
	}
	if got, err := s.Mark(ctx, "s1", task.ID, keys); err != nil || len(got) != 0 {
		t.Fatalf("same session again: %v %v", got, err)
	}
	if got, err := s.Mark(ctx, "s2", task.ID, keys[:1]); err != nil || !got[LessonKey(lid)] {
		t.Fatalf("new session: %v %v", got, err)
	}
	ls, _ := s.Lessons(ctx)
	if ls[0].Hits != 2 || ls[0].LastHitAt == "" {
		t.Fatalf("hits %+v", ls[0])
	}
}

func TestResetInjectedReinjectsWithoutAnotherHit(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := memoryTask(t, s, "T")
	lid, err := s.AddLesson(ctx, Lesson{TaskID: task.ID, Pattern: "p", Fix: "f", PathGlob: "a/**", TrueAtSHA: "x"})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{LessonKey(lid), "decision:1", "outside:shoal-backend:a/b.go"}
	if _, err := s.Mark(ctx, "s1", task.ID, keys); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ { // a second compaction resets again
		if err := s.ResetInjected(ctx, "s1"); err != nil {
			t.Fatal(err)
		}
		got, err := s.Mark(ctx, "s1", task.ID, keys)
		if err != nil || len(got) != 2 || !got[LessonKey(lid)] || !got["decision:1"] {
			t.Fatalf("round %d: after the reset only the records mark again (warnings stay once): %v %v", round, got, err)
		}
	}
	ls, _ := s.Lessons(ctx)
	if ls[0].Hits != 1 {
		t.Fatalf("re-injection after a reset counted a hit: %d hits", ls[0].Hits)
	}
}

func TestFireOnceWindow(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := memoryTask(t, s, "T")
	if first, err := s.FireOnce(ctx, "s", "fire:session-start:startup", task.ID, time.Minute); err != nil || !first {
		t.Fatalf("first: %v %v", first, err)
	}
	if first, err := s.FireOnce(ctx, "s", "fire:session-start:startup", task.ID, time.Minute); err != nil || first {
		t.Fatalf("double fire: %v %v", first, err)
	}
	if first, err := s.FireOnce(ctx, "s", "fire:session-start:compact", task.ID, time.Minute); err != nil || !first {
		t.Fatalf("other source: %v %v", first, err)
	}
	old := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE hook_marks SET created_at = ? WHERE key = 'fire:session-start:startup'`, old); err != nil {
		t.Fatal(err)
	}
	if first, err := s.FireOnce(ctx, "s", "fire:session-start:startup", task.ID, time.Minute); err != nil || !first {
		t.Fatalf("after the window: %v %v", first, err)
	}
	ancient := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	if _, err := s.db.Exec(`INSERT INTO hook_marks (session_id, key, task_id, created_at) VALUES ('old', 'fire:x', ?, ?), ('old', 'lesson:1', ?, ?)`,
		task.ID, ancient, task.ID, ancient); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FireOnce(ctx, "s", "fire:y", task.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM hook_marks WHERE session_id = 'old'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pruning kept %d old marks (want only the lesson mark), %v", n, err)
	}
}

func TestRecordWriteLateOverlap(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	early := memoryTask(t, s, "Early")
	late := memoryTask(t, s, "Late")
	req := WriteRequest{Session: "s", TaskID: late.ID, Slug: late.Slug, Agent: "shoal-lead", Path: "src/a.go", ClaimActual: true,
		Behind: []Task{early}, Keys: []string{"overlap:1:src/a.go"}}
	res, err := s.RecordWrite(ctx, req, "shoal-lead")
	if err != nil || res.Overlap[early.ID] != OverlapQueued || !res.NewKeys["overlap:1:src/a.go"] {
		t.Fatalf("first write: %+v %v", res, err)
	}
	res, err = s.RecordWrite(ctx, req, "shoal-lead")
	if err != nil || res.Overlap[early.ID] != OverlapAlready || len(res.NewKeys) != 0 {
		t.Fatalf("second write: %+v %v", res, err)
	}
	cl, _ := s.Claims(ctx, late.ID)
	if len(cl) != 1 || cl[0].Kind != "actual" || cl[0].Glob != "src/a.go" || cl[0].Agent != "shoal-lead" {
		t.Fatalf("claims %+v", cl)
	}
	if b, _ := s.Blockers(ctx, late.ID); len(b) != 1 || b[0] != early.ID {
		t.Fatalf("blockers %+v", b)
	}
	if got, _ := s.taskByID(ctx, late.ID); got.BlockedBy != early.ID || got.Stage != StageNew {
		t.Fatalf("late task %+v", got)
	}
	msgs, err := s.Inbox(ctx, early.ID, "shoal-lead", true)
	if err != nil || len(msgs) != 1 || !strings.Contains(msgs[0].Body, "src/a.go") || msgs[0].Kind != "info" {
		t.Fatalf("messages to the early task: %+v %v", msgs, err)
	}

	// The early task writing a path the late one holds would close a cycle.
	back, err := s.RecordWrite(ctx, WriteRequest{Session: "t", TaskID: early.ID, Slug: early.Slug, Agent: "shoal-lead", Path: "src/a.go", Behind: []Task{late}}, "shoal-lead")
	if err != nil || back.Overlap[late.ID] != OverlapCycle {
		t.Fatalf("reverse write: %+v %v", back, err)
	}
	if b, _ := s.Blockers(ctx, early.ID); len(b) != 0 {
		t.Fatalf("a cycle was recorded: %+v", b)
	}

	// The merge of the early task clears the order, and blocked_by with it.
	err = s.write(ctx, func(tx *sql.Tx) error {
		_, err := UnblockAfterMerge(ctx, tx, early.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.taskByID(ctx, late.ID); got.BlockedBy != 0 || got.Stage != StageNew {
		t.Fatalf("after the early merge: %+v", got)
	}
}
