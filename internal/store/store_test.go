package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testOpts() Options {
	return Options{Create: true, Migrate: true, BusyTimeout: time.Second, Retries: 2}
}

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := Open(context.Background(), path, testOpts())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func schemaSnapshot(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT type || ':' || name || ':' || COALESCE(sql, '') FROM sqlite_master ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func TestMigrationsEmbedded(t *testing.T) {
	migs, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) == 0 || migs[0].Version != 1 || migs[0].Name != "init" {
		t.Fatalf("unexpected migrations: %+v", migs)
	}
}

func TestOpenSettings(t *testing.T) {
	s, _ := openTest(t)
	var mode string
	var fk, timeout int
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, %v", fk, err)
	}
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 1000 {
		t.Fatalf("busy_timeout = %d, %v", timeout, err)
	}
}

func TestMigrateTwiceIsNoop(t *testing.T) {
	migs, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	s, path := openTest(t)
	before := schemaSnapshot(t, s)
	s.Close()

	s2, err := Open(context.Background(), path, testOpts())
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	if after := schemaSnapshot(t, s2); after != before {
		t.Fatalf("schema changed on second open:\n%s\nvs\n%s", before, after)
	}
	var n, v int
	if err := s2.db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n != len(migs) {
		t.Fatalf("schema_migrations rows = %d, %v", n, err)
	}
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != len(migs) {
		t.Fatalf("user_version = %d, %v", v, err)
	}
	if _, err := os.Stat(path + ".bak-1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no backup expected for a fresh DB, stat: %v", err)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	migs, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	s, path := openTest(t)
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, migrate := range []bool{true, false} {
		opts := testOpts()
		opts.Migrate = migrate
		_, err := Open(context.Background(), path, opts)
		if !errors.Is(err, ErrSchemaTooNew) || !strings.Contains(err.Error(), fmt.Sprintf("schema 99; this shoalctl knows %d", len(migs))) {
			t.Fatalf("migrate=%v: got %v, want ErrSchemaTooNew", migrate, err)
		}
	}
}

func TestLaterMigrationBacksUpAndApplies(t *testing.T) {
	migs, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	s, path := openTest(t)
	s.Close()

	v := len(migs) + 1
	next := append(append([]Migration{}, migs...), Migration{Version: v, Name: "extra", SQL: "CREATE TABLE extra (x INTEGER);"})
	s2, err := open(context.Background(), path, testOpts(), next)
	if err != nil {
		t.Fatalf("migrate to %d: %v", v, err)
	}
	defer s2.Close()
	if _, err := os.Stat(fmt.Sprintf("%s.bak-%d", path, v)); err != nil {
		t.Fatalf("backup before migration %d: %v", v, err)
	}
	var got int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&got); err != nil || got != v {
		t.Fatalf("user_version = %d, %v", got, err)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	migs, _ := Migrations()
	s, path := openTest(t)
	s.Close()
	bad := append(append([]Migration{}, migs...), Migration{Version: len(migs) + 1, Name: "bad", SQL: "CREATE TABLE ok (x); CREATE TABLE tasks (x);"})
	if _, err := open(context.Background(), path, testOpts(), bad); err == nil {
		t.Fatal("want error from a failing migration")
	}
	s2, err := Open(context.Background(), path, testOpts())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var n int
	if err := s2.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'ok'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial migration left table behind: %d, %v", n, err)
	}
}

func TestOpenWithoutCreateOrMigrate(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "memory.db")
	if _, err := Open(context.Background(), missing, Options{BusyTimeout: time.Second}); err == nil {
		t.Fatal("want error for a missing DB without Create")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open without Create created the file: %v", err)
	}

	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), empty, Options{BusyTimeout: time.Second}); !errors.Is(err, ErrSchemaChanged) {
		t.Fatalf("unmigrated DB without Migrate: got %v, want ErrSchemaChanged", err)
	}

	corrupt := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), corrupt, Options{BusyTimeout: time.Second}); err == nil {
		t.Fatal("want error for a corrupt DB")
	}
}

func slugger(id int64) string { return fmt.Sprintf("%d-task", id) }

func TestCreateAndGetTask(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a, err := s.CreateTask(ctx, "feature", "First", "ask", slugger)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTask(ctx, "bugfix", "Second", "ask", slugger)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != 1 || b.ID != 2 || a.Slug != "1-task" || a.Stage != StageNew || a.Version != 1 {
		t.Fatalf("unexpected tasks: %+v %+v", a, b)
	}
	for _, ref := range []string{"2", "2-task"} {
		got, err := s.GetTask(ctx, ref)
		if err != nil || got.ID != 2 {
			t.Fatalf("GetTask(%q) = %+v, %v", ref, got, err)
		}
	}
	if _, err := s.GetTask(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing slug: %v", err)
	}
	if _, err := s.CreateTask(ctx, "chore", "Bad lane", "ask", slugger); err == nil {
		t.Fatal("lane CHECK constraint not enforced")
	}
}

func TestUpdateOptimisticLocking(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, "feature", "T", "ask", slugger)
	if err != nil {
		t.Fatal(err)
	}

	stale := task
	if _, err := s.Update(ctx, task, []string{StageNew}, "base_ref = ?", []any{"main"}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Update(ctx, stale, []string{StageNew}, "stage = ?", []any{StageStarting}, nil)
	if err != nil {
		t.Fatalf("stale version with a still-valid stage should retry and win: %v", err)
	}
	if got.Stage != StageStarting || got.Version != 3 || got.BaseRef != "main" {
		t.Fatalf("after update: %+v", got)
	}

	if _, err := s.Update(ctx, stale, []string{StageNew}, "stage = ?", []any{StageDone}, nil); !errors.Is(err, ErrStage) {
		t.Fatalf("illegal transition: got %v, want ErrStage", err)
	}

	boom := errors.New("boom")
	_, err = s.Update(ctx, got, []string{StageStarting}, "stage = ?", []any{StageInProgress}, func(*sql.Tx) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("extra error: %v", err)
	}
	if again, _ := s.GetTask(ctx, "1"); again.Stage != StageStarting || again.Version != 3 {
		t.Fatalf("failed extra did not roll back the update: %+v", again)
	}
}

func TestBusyWriterGivesUp(t *testing.T) {
	_, path := openTest(t)
	ctx := context.Background()
	holder, err := Open(ctx, path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	waiter, err := Open(ctx, path, Options{Migrate: false, BusyTimeout: 50 * time.Millisecond, Retries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	_, err = waiter.CreateTask(ctx, "feature", "T", "ask", slugger)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
}

func TestHeartbeat(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "feature", "T", "ask", slugger)
	if err := s.Heartbeat(ctx, task.ID, task.Slug, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, task.ID, task.Slug, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(ctx, task.Slug)
	if got.SessionID != "sess-1" || got.LastSeenAt == "" || got.Version != task.Version {
		t.Fatalf("after heartbeat (heartbeats must not bump the version): %+v", got)
	}
	if err := s.Heartbeat(ctx, task.ID, "other-slug", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mismatched slug: %v", err)
	}
}

// TestHeartbeatDoesNotRaceStageChanges: a hook heartbeat landing between a
// command's read and its stage update must not cost that update a retry.
func TestHeartbeatDoesNotRaceStageChanges(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "feature", "T", "ask", slugger)
	for i := 0; i < 5; i++ {
		if err := s.Heartbeat(ctx, task.ID, task.Slug, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Update(ctx, task, []string{StageNew}, "stage = ?", []any{StageStarting}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != task.Version+1 || got.Stage != StageStarting {
		t.Fatalf("update after heartbeats: version %d (read %d), stage %s", got.Version, task.Version, got.Stage)
	}
}

func TestRunLifecycleAndBaselineReuse(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "feature", "T", "ask", slugger)

	if _, ok, err := s.FindBaseline(ctx, "abc", "h"); err != nil || ok {
		t.Fatalf("no baseline yet: ok=%v err=%v", ok, err)
	}
	r, err := s.StartRun(ctx, Run{TaskID: task.ID, Kind: "baseline", SHA: "abc", Command: "c", CommandHash: "h", Format: "junit", Fidelity: "per-test"},
		func(id int64) string { return fmt.Sprintf("/runs/%d/output.log", id) })
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "running" || r.OutputPath != fmt.Sprintf("/runs/%d/output.log", r.ID) {
		t.Fatalf("started run: %+v", r)
	}
	if _, ok, _ := s.FindBaseline(ctx, "abc", "h"); ok {
		t.Fatal("a running baseline must not be reused")
	}
	code := 1
	done, err := s.FinishRun(ctx, r.ID, "completed", &code, []Result{
		{TestID: "a::x", Status: "pass"}, {TestID: "a::y", Status: "fail", Message: "boom"}, {TestID: "a::z", Status: "skip"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.Total != 3 || done.Passed != 1 || done.Failed != 1 || done.Skipped != 1 || *done.ExitCode != 1 {
		t.Fatalf("finished run: %+v", done)
	}
	found, ok, err := s.FindBaseline(ctx, "abc", "h")
	if err != nil || !ok || found.ID != r.ID {
		t.Fatalf("baseline reuse: %+v ok=%v err=%v", found, ok, err)
	}
	if _, ok, _ := s.FindBaseline(ctx, "abc", "other"); ok {
		t.Fatal("different command hash must not match")
	}
	res, err := s.Results(ctx, r.ID)
	if err != nil || len(res) != 3 || res[1].Message != "boom" {
		t.Fatalf("results: %+v, %v", res, err)
	}
}
