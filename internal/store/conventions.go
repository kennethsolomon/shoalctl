package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// KindConvention is the record kind of a convention (§11.5).
const KindConvention = "convention"

// Convention statuses.
const (
	ConventionProposed = "proposed"
	ConventionActive   = "active"
	ConventionRetired  = "retired"
)

type Convention struct {
	ID           int64
	Rule         string
	ScopeGlob    string
	Status       string
	Conflict     string // the existing project rule it contradicts; "" none
	SourceTaskID int64  // 0 when proposed outside a task
	LessonID     int64  // the lesson whose promotion it came from; 0 none
	ProposedAt   string
	AgreedBy     string
	AgreedAt     string
	RetiredBy    string
	RetiredAt    string
	RetireReason string
}

// ConventionKey is the hook mark of a convention injected in a session.
func ConventionKey(id int64) string { return fmt.Sprintf("%s:%d", KindConvention, id) }

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// ProposeConvention records a proposed convention and returns its id. A
// rule already open (proposed or active) with the same text and scope, or
// an open proposal from the same lesson, is not added twice: its id comes
// back with existing = true.
func (s *Store) ProposeConvention(ctx context.Context, c Convention) (id int64, existing bool, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		existing = false
		q := `SELECT id FROM conventions WHERE (status != 'retired' AND lower(trim(rule)) = lower(trim(?)) AND scope_glob = ?)`
		args := []any{c.Rule, c.ScopeGlob}
		if c.LessonID != 0 {
			q += ` OR (status = 'proposed' AND lesson_id = ?)`
			args = append(args, c.LessonID)
		}
		err := tx.QueryRowContext(ctx, q+` ORDER BY id LIMIT 1`, args...).Scan(&id)
		switch {
		case err == nil:
			existing = true
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("look for an open convention like %q: %w", c.Rule, err)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO conventions (rule, scope_glob, status, conflict, source_task_id, lesson_id, proposed_at)
			VALUES (?, ?, 'proposed', ?, ?, ?, ?)`, c.Rule, c.ScopeGlob, c.Conflict, nullID(c.SourceTaskID), nullID(c.LessonID), now())
		if err != nil {
			return fmt.Errorf("record convention %q: %w", c.Rule, err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("record convention %q: %w", c.Rule, err)
		}
		return nil
	})
	return id, existing, err
}

const conventionCols = `id, rule, scope_glob, status, conflict, COALESCE(source_task_id, 0), COALESCE(lesson_id, 0), proposed_at,
	COALESCE(agreed_by, ''), COALESCE(agreed_at, ''), COALESCE(retired_by, ''), COALESCE(retired_at, ''), COALESCE(retire_reason, '')`

func scanConvention(row interface{ Scan(...any) error }) (Convention, error) {
	var c Convention
	err := row.Scan(&c.ID, &c.Rule, &c.ScopeGlob, &c.Status, &c.Conflict, &c.SourceTaskID, &c.LessonID, &c.ProposedAt,
		&c.AgreedBy, &c.AgreedAt, &c.RetiredBy, &c.RetiredAt, &c.RetireReason)
	return c, err
}

// Conventions returns every convention, oldest first.
func (s *Store) Conventions(ctx context.Context) ([]Convention, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+conventionCols+` FROM conventions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read conventions: %w", err)
	}
	defer rows.Close()
	var out []Convention
	for rows.Next() {
		c, err := scanConvention(rows)
		if err != nil {
			return nil, fmt.Errorf("scan convention: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConvention returns one convention.
func (s *Store) GetConvention(ctx context.Context, id int64) (Convention, error) {
	return getConvention(ctx, s.db, id)
}

func getConvention(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (Convention, error) {
	c, err := scanConvention(q.QueryRowContext(ctx, `SELECT `+conventionCols+` FROM conventions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, fmt.Errorf("convention:%d: %w", id, ErrNotFound)
	}
	if err != nil {
		return c, fmt.Errorf("read convention:%d: %w", id, err)
	}
	return c, nil
}

// Agreement is the human's yes to a proposed convention, with the rule and
// scope as the brainstorm left them. Conflict is kept on the row when the
// human agreed despite an existing rule ("" otherwise).
type Agreement struct {
	Rule, ScopeGlob, Conflict, By string
}

// AgreeConvention makes a proposed convention active. One that came from a
// lesson promotes the lesson in the same transaction, so its per-file
// injection stops (the convention carries the rule now).
func (s *Store) AgreeConvention(ctx context.Context, id int64, a Agreement) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		c, err := getConvention(ctx, tx, id)
		if err != nil {
			return err
		}
		if c.Status != ConventionProposed {
			return fmt.Errorf("%w: convention:%d is %s; only a proposed convention can be agreed", ErrStage, id, c.Status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conventions SET status = 'active', rule = ?, scope_glob = ?, conflict = ?, agreed_by = ?, agreed_at = ?
			WHERE id = ?`, a.Rule, a.ScopeGlob, a.Conflict, a.By, now(), id); err != nil {
			return fmt.Errorf("agree convention:%d: %w", id, err)
		}
		if c.LessonID != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE lessons SET promoted = 1 WHERE id = ?`, c.LessonID); err != nil {
				return fmt.Errorf("promote lesson:%d with convention:%d: %w", c.LessonID, id, err)
			}
		}
		return nil
	})
}

// RetireConvention retires a proposed or active convention and keeps why.
// A lesson promoted through it is flagged stale with the same reason, so it
// is neither injected again nor rendered as a promoted lesson.
func (s *Store) RetireConvention(ctx context.Context, id int64, by, reason string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		c, err := getConvention(ctx, tx, id)
		if err != nil {
			return err
		}
		if c.Status == ConventionRetired {
			return fmt.Errorf("%w: convention:%d is already retired (%s)", ErrStage, id, c.RetireReason)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conventions SET status = 'retired', retired_by = ?, retired_at = ?, retire_reason = ? WHERE id = ?`,
			by, now(), reason, id); err != nil {
			return fmt.Errorf("retire convention:%d: %w", id, err)
		}
		if c.LessonID != 0 && c.Status == ConventionActive {
			if _, err := tx.ExecContext(ctx, `UPDATE lessons SET stale = 1, stale_reason = ? WHERE id = ? AND stale = 0`,
				truncate(fmt.Sprintf("retired with convention:%d: %s", id, reason), 500), c.LessonID); err != nil {
				return fmt.Errorf("retire lesson:%d with convention:%d: %w", c.LessonID, id, err)
			}
		}
		return nil
	})
}

// ReopenConvention sends an active convention back to proposed because it
// now contradicts an existing project rule (§5.3): the rule wins until the
// human decides again.
func (s *Store) ReopenConvention(ctx context.Context, id int64, conflict string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		c, err := getConvention(ctx, tx, id)
		if err != nil {
			return err
		}
		if c.Status != ConventionActive {
			return fmt.Errorf("%w: convention:%d is %s; only an active convention can be reopened", ErrStage, id, c.Status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conventions SET status = 'proposed', conflict = ? WHERE id = ?`, conflict, id); err != nil {
			return fmt.Errorf("reopen convention:%d: %w", id, err)
		}
		if c.LessonID != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE lessons SET promoted = 0 WHERE id = ?`, c.LessonID); err != nil {
				return fmt.Errorf("unpromote lesson:%d with convention:%d: %w", c.LessonID, id, err)
			}
		}
		return nil
	})
}

// LessonTaskHits returns, per lesson id, how many different tasks it was
// shown in.
func (s *Store) LessonTaskHits(ctx context.Context) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT lesson_id, count(*) FROM lesson_hits GROUP BY lesson_id`)
	if err != nil {
		return nil, fmt.Errorf("read lesson hits per task: %w", err)
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("scan lesson hits: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// PromoteLesson sets promoted = 1 on a lesson shown in at least minTasks
// different tasks, not stale and not promoted yet (§11.4). The check runs
// in the write transaction, so it cannot race a flag-stale.
func (s *Store) PromoteLesson(ctx context.Context, id int64, minTasks int) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var promoted, stale bool
		err := tx.QueryRowContext(ctx, `SELECT promoted, stale FROM lessons WHERE id = ?`, id).Scan(&promoted, &stale)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lesson:%d: %w", id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read lesson:%d: %w", id, err)
		}
		var tasks int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM lesson_hits WHERE lesson_id = ?`, id).Scan(&tasks); err != nil {
			return fmt.Errorf("read hits of lesson:%d: %w", id, err)
		}
		switch {
		case promoted:
			return fmt.Errorf("%w: lesson:%d is already promoted", ErrStage, id)
		case stale:
			return fmt.Errorf("%w: lesson:%d is flagged stale; a stale lesson is never promoted", ErrStage, id)
		case tasks < minTasks:
			return fmt.Errorf("%w: lesson:%d was shown in %d task(s); promotion needs %d different tasks", ErrStage, id, tasks, minTasks)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE lessons SET promoted = 1 WHERE id = ?`, id); err != nil {
			return fmt.Errorf("promote lesson:%d: %w", id, err)
		}
		return nil
	})
}

// ProposeFromPromotedLesson takes a promoted lesson back (promoted = 0) and
// records its rule as a proposed convention flagged with conflict, in one
// transaction: an existing project rule now contradicts it (§5.3). An open
// proposal from the lesson already present is reused.
func (s *Store) ProposeFromPromotedLesson(ctx context.Context, c Convention) (int64, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT id FROM conventions WHERE status = 'proposed' AND lesson_id = ?`, c.LessonID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			res, ierr := tx.ExecContext(ctx, `INSERT INTO conventions (rule, scope_glob, status, conflict, source_task_id, lesson_id, proposed_at)
				VALUES (?, ?, 'proposed', ?, ?, ?, ?)`, c.Rule, c.ScopeGlob, c.Conflict, nullID(c.SourceTaskID), c.LessonID, now())
			if ierr != nil {
				return fmt.Errorf("record the conflict of lesson:%d: %w", c.LessonID, ierr)
			}
			if id, ierr = res.LastInsertId(); ierr != nil {
				return fmt.Errorf("record the conflict of lesson:%d: %w", c.LessonID, ierr)
			}
		} else if err != nil {
			return fmt.Errorf("look for an open proposal of lesson:%d: %w", c.LessonID, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE lessons SET promoted = 0 WHERE id = ?`, c.LessonID); err != nil {
			return fmt.Errorf("unpromote lesson:%d: %w", c.LessonID, err)
		}
		return nil
	})
	return id, err
}

// ParseConventionRef reads "<id>" or "convention:<id>".
func ParseConventionRef(ref string) (int64, error) {
	num := strings.TrimPrefix(ref, KindConvention+":")
	var id int64
	if _, err := fmt.Sscan(num, &id); err != nil || id <= 0 || fmt.Sprint(id) != num {
		return 0, fmt.Errorf("convention %q must be <id> or convention:<id>", ref)
	}
	return id, nil
}
