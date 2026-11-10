package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestDumpEveryTableButTheIndex(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := memoryTask(t, s, "Users page")
	if _, err := s.AddLesson(ctx, Lesson{TaskID: task.ID, Pattern: "p", Fix: "f\x00binary-safe", PathGlob: "a/**", TrueAtSHA: "x"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.Dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	migs, _ := Migrations()
	if d.SchemaVersion != migs[len(migs)-1].Version {
		t.Fatalf("schema_version %d", d.SchemaVersion)
	}
	for name := range d.Tables {
		if len(name) >= 10 && name[:10] == "memory_fts" {
			t.Errorf("dumped the derived index table %s", name)
		}
	}
	if len(d.Tables["tasks"]) != 1 || d.Tables["tasks"][0]["title"] != "Users page" {
		t.Fatalf("tasks: %+v", d.Tables["tasks"])
	}
	if l := d.Tables["lessons"]; len(l) != 1 || l[0]["fix"] != "f\x00binary-safe" || l[0]["stack"] != nil && l[0]["stack"] != "" {
		t.Fatalf("lessons: %+v", l)
	}
	if len(d.Tables["schema_migrations"]) != len(migs) || d.Tables["messages"] == nil {
		t.Fatalf("schema_migrations %d rows, messages %v (an empty table must dump as [])", len(d.Tables["schema_migrations"]), d.Tables["messages"])
	}
	var want int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite%' AND name NOT LIKE 'memory_fts%'`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if len(d.Tables) != want {
		t.Fatalf("dumped %d tables, the schema has %d", len(d.Tables), want)
	}
}

func TestDropWorkingRowsKeepsRecords(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a, b := memoryTask(t, s, "A"), memoryTask(t, s, "B")
	mid, err := s.SendMessage(ctx, Message{TaskID: a.ID, From: "shoal-backend", To: "shoal-lead", Kind: "handoff", Subject: "s", Body: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, Message{TaskID: a.ID, From: "shoal-lead", To: "shoal-backend", Kind: "answer", Subject: "s", Body: "ok", ReplyTo: mid}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Inbox(ctx, a.ID, "shoal-lead", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendMessage(ctx, Message{TaskID: b.ID, From: "shoal-lead", To: "*", Kind: "info", Subject: "s", Body: "b's"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddClaims(ctx, a.ID, "shoal-backend", []string{"api/**"}, func(x, y string) bool { return x == y }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mark(ctx, "sess", a.ID, []string{"k"}); err != nil {
		t.Fatal(err)
	}
	lid, err := s.AddLesson(ctx, Lesson{TaskID: a.ID, Pattern: "p", Fix: "f", TrueAtSHA: "x"})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO task_blockers (task_id, blocker_id, created_at) VALUES (?, ?, 'now')`, b.ID, a.ID)
	exec(`INSERT INTO merge_queue (task_id, position, state, updated_at) VALUES (?, 1, 'merged', 'now')`, a.ID)
	exec(`INSERT INTO summaries (task_id, decision, why, verification, commits, files, true_at_sha, report_path, created_at)
		VALUES (?, 'd', 'w', '{}', '[]', '[]', 'x', 'r.md', 'now')`, a.ID)

	if _, err := s.DropWorkingRows(ctx, a.ID); !errors.Is(err, ErrStage) {
		t.Fatalf("an open task's rows dropped: %v", err)
	}
	exec(`UPDATE tasks SET stage = 'merged' WHERE id = ?`, a.ID)
	got, err := s.DropWorkingRows(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := WorkingRows{"messages": 2, "message_reads": 1, "file_claims": 1, "hook_marks": 1, "task_blockers": 1, "merge_queue": 1}
	if len(got) != len(want) {
		t.Fatalf("dropped %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("dropped %v, want %v", got, want)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		return n
	}
	if count(`SELECT count(*) FROM summaries WHERE task_id = ?`, a.ID) != 1 || count(`SELECT count(*) FROM tasks`) != 2 ||
		count(`SELECT count(*) FROM lessons WHERE id = ?`, lid) != 1 || count(`SELECT count(*) FROM messages WHERE task_id = ?`, b.ID) != 1 {
		t.Fatal("dropped a record or another task's row")
	}
	if again, err := s.DropWorkingRows(ctx, a.ID); err != nil || len(again) != 0 {
		t.Fatalf("second drop: %v %v", again, err)
	}
	if _, err := s.DropWorkingRows(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
}
