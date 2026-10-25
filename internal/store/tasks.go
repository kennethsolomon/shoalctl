package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Stages used in Phase 0.
const (
	StageNew            = "new"
	StageStarting       = "starting"
	StageBaselineFailed = "baseline-failed"
	StageInProgress     = "in-progress"
	StageDone           = "done"
)

type Task struct {
	ID            int64
	Slug          string
	Lane          string
	Title         string
	Ask           string
	Stage         string
	BaseRef       string
	BaseSHA       string
	Branch        string
	Worktree      string
	TmuxSession   string
	TmuxWindow    string
	Model         string
	BaselineRunID int64
	FinalRunID    int64
	SessionID     string
	LastSeenAt    string
	Version       int64
	CreatedAt     string
	UpdatedAt     string
	StartedAt     string
	ClosedAt      string
}

const taskColumns = `id, slug, lane, title, ask, stage,
	COALESCE(base_ref, ''), COALESCE(base_sha, ''), COALESCE(branch, ''), COALESCE(worktree, ''),
	COALESCE(tmux_session, ''), COALESCE(tmux_window, ''), COALESCE(model, ''),
	COALESCE(baseline_run_id, 0), COALESCE(final_run_id, 0),
	COALESCE(session_id, ''), COALESCE(last_seen_at, ''), version, created_at, updated_at,
	COALESCE(started_at, ''), COALESCE(closed_at, '')`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Slug, &t.Lane, &t.Title, &t.Ask, &t.Stage,
		&t.BaseRef, &t.BaseSHA, &t.Branch, &t.Worktree, &t.TmuxSession, &t.TmuxWindow, &t.Model,
		&t.BaselineRunID, &t.FinalRunID, &t.SessionID, &t.LastSeenAt, &t.Version, &t.CreatedAt, &t.UpdatedAt,
		&t.StartedAt, &t.ClosedAt)
	return t, err
}

// CreateTask inserts a task at stage new. slug derives the slug from the new id.
func (s *Store) CreateTask(ctx context.Context, lane, title, ask string, slug func(id int64) string) (Task, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) + 1 FROM tasks`).Scan(&id); err != nil {
			return fmt.Errorf("next task id: %w", err)
		}
		ts := now()
		_, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, slug, lane, title, ask, stage, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, slug(id), lane, title, ask, StageNew, ts, ts)
		if err != nil {
			return fmt.Errorf("insert task %q: %w", title, err)
		}
		return nil
	})
	if err != nil {
		return Task{}, err
	}
	return s.taskByID(ctx, id)
}

// GetTask finds a task by numeric id or slug.
func (s *Store) GetTask(ctx context.Context, ref string) (Task, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return s.taskByID(ctx, id)
	}
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE slug = ?`, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("task %q: %w", ref, ErrNotFound)
	}
	if err != nil {
		return Task{}, fmt.Errorf("read task %q: %w", ref, err)
	}
	return t, nil
}

func (s *Store) taskByID(ctx context.Context, id int64) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("task %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Task{}, fmt.Errorf("read task %d: %w", id, err)
	}
	return t, nil
}

// ListTasks returns open tasks and tasks closed at or after closedSince, newest first.
func (s *Store) ListTasks(ctx context.Context, closedSince string) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE closed_at IS NULL OR closed_at >= ? ORDER BY id DESC`, closedSince)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	return out, nil
}

// Update applies set (a fixed column list such as "stage = ?, base_sha = ?")
// to the task when its stage is one of from, guarded by the version column
// (§10.2). extra runs in the same transaction after a successful update. On a
// version conflict it re-reads, re-checks the stage, and retries up to 3 times.
func (s *Store) Update(ctx context.Context, t Task, from []string, set string, args []any, extra func(*sql.Tx) error) (Task, error) {
	version := t.Version
	for attempt := 0; ; attempt++ {
		var changed int64
		err := s.write(ctx, func(tx *sql.Tx) error {
			q := `UPDATE tasks SET ` + set + `, version = version + 1, updated_at = ?
				WHERE id = ? AND version = ? AND stage IN (` + placeholders(len(from)) + `)`
			all := append(append(append([]any{}, args...), now(), t.ID, version), anys(from)...)
			res, err := tx.ExecContext(ctx, q, all...)
			if err != nil {
				return fmt.Errorf("update task %s: %w", t.Slug, err)
			}
			if changed, err = res.RowsAffected(); err != nil {
				return fmt.Errorf("update task %s: %w", t.Slug, err)
			}
			if changed == 1 && extra != nil {
				return extra(tx)
			}
			return nil
		})
		if err != nil {
			return Task{}, err
		}
		if changed == 1 {
			return s.taskByID(ctx, t.ID)
		}
		cur, err := s.taskByID(ctx, t.ID)
		if err != nil {
			return Task{}, err
		}
		if !contains(from, cur.Stage) {
			return cur, fmt.Errorf("%w: task %s is at %q, expected one of %s", ErrStage, t.Slug, cur.Stage, strings.Join(from, ", "))
		}
		if attempt == 3 {
			return cur, fmt.Errorf("%w: task %s changed under us (read version %d, now %d)", ErrConflict, t.Slug, t.Version, cur.Version)
		}
		version = cur.Version
	}
}

// Heartbeat records hook activity on a task. sessionID is stored when non-empty.
func (s *Store) Heartbeat(ctx context.Context, id int64, slug, sessionID string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		ts := now()
		res, err := tx.ExecContext(ctx, `UPDATE tasks SET last_seen_at = ?, updated_at = ?, version = version + 1,
			session_id = CASE WHEN ? = '' THEN session_id ELSE ? END
			WHERE id = ? AND slug = ?`, ts, ts, sessionID, sessionID, id, slug)
		if err != nil {
			return fmt.Errorf("heartbeat task %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("heartbeat task %d: %w", id, err)
		}
		if n != 1 {
			return fmt.Errorf("heartbeat: task %d (%s): %w", id, slug, ErrNotFound)
		}
		return nil
	})
}

type Summary struct {
	TaskID               int64
	Decision             string
	AlternativesRejected string
	Why                  string
	Verification         string
	Risks                string
	Followups            string
	ChangedNotes         string
	Commits              string
	Files                string
	TrueAtSHA            string
	ReportPath           string
}

// InsertSummary writes the summaries row inside a caller's transaction.
func InsertSummary(ctx context.Context, tx *sql.Tx, sm Summary) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO summaries (task_id, decision, alternatives_rejected, why, verification,
		risks, followups, changed_notes, commits, files, true_at_sha, report_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sm.TaskID, sm.Decision, sm.AlternativesRejected, sm.Why, sm.Verification, sm.Risks, sm.Followups,
		sm.ChangedNotes, sm.Commits, sm.Files, sm.TrueAtSHA, sm.ReportPath, now())
	if err != nil {
		return fmt.Errorf("insert summary for task %d: %w", sm.TaskID, err)
	}
	return nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func checksum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
