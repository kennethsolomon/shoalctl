package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrClaimConflict is a claim another agent of the same task already holds.
var ErrClaimConflict = errors.New("path is claimed by another agent")

// Message is one row of the messages table (§7.4).
type Message struct {
	ID        int64
	TaskID    int64
	From      string
	To        string
	Kind      string
	Subject   string
	Body      string
	ReplyTo   int64
	CreatedAt string
	Read      bool // in Inbox: already read by the agent before this call
}

// SendMessage stores a message. A reply_to must name a message of the same task.
func (s *Store) SendMessage(ctx context.Context, m Message) (int64, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = InsertMessage(ctx, tx, m)
		return err
	})
	return id, err
}

// InsertMessage stores a message inside a caller's transaction.
func InsertMessage(ctx context.Context, tx *sql.Tx, m Message) (int64, error) {
	var reply any
	if m.ReplyTo != 0 {
		var task int64
		err := tx.QueryRowContext(ctx, `SELECT task_id FROM messages WHERE id = ?`, m.ReplyTo).Scan(&task)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && task != m.TaskID) {
			return 0, fmt.Errorf("reply-to message %d in this task: %w", m.ReplyTo, ErrNotFound)
		}
		if err != nil {
			return 0, fmt.Errorf("read message %d: %w", m.ReplyTo, err)
		}
		reply = m.ReplyTo
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO messages (task_id, from_agent, to_agent, kind, subject, body, reply_to, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, m.TaskID, m.From, m.To, m.Kind, m.Subject, m.Body, reply, now())
	if err != nil {
		return 0, fmt.Errorf("insert %s message from %s to %s: %w", m.Kind, m.From, m.To, err)
	}
	return res.LastInsertId()
}

// Inbox returns the messages addressed to agent (directly or by broadcast)
// that another agent sent, oldest first, and marks them read in the same
// transaction. With all it also returns messages agent already read.
func (s *Store) Inbox(ctx context.Context, taskID int64, agent string, all bool) ([]Message, error) {
	var out []Message
	err := s.write(ctx, func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `SELECT m.id, m.task_id, m.from_agent, m.to_agent, m.kind, m.subject, m.body,
			COALESCE(m.reply_to, 0), m.created_at, r.message_id IS NOT NULL
			FROM messages m LEFT JOIN message_reads r ON r.message_id = m.id AND r.agent = ?
			WHERE m.task_id = ? AND m.to_agent IN (?, '*') AND m.from_agent <> ? ORDER BY m.id`, agent, taskID, agent, agent)
		if err != nil {
			return fmt.Errorf("read inbox of %s: %w", agent, err)
		}
		for rows.Next() {
			var m Message
			if err := rows.Scan(&m.ID, &m.TaskID, &m.From, &m.To, &m.Kind, &m.Subject, &m.Body, &m.ReplyTo, &m.CreatedAt, &m.Read); err != nil {
				rows.Close()
				return fmt.Errorf("scan inbox of %s: %w", agent, err)
			}
			if all || !m.Read {
				out = append(out, m)
			}
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("read inbox of %s: %w", agent, err)
		}
		ts := now()
		for _, m := range out {
			if m.Read {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_reads (message_id, agent, read_at) VALUES (?, ?, ?)`, m.ID, agent, ts); err != nil {
				return fmt.Errorf("mark message %d read by %s: %w", m.ID, agent, err)
			}
		}
		return nil
	})
	return out, err
}

// UnreadCount counts the messages Inbox would return for agent.
func (s *Store) UnreadCount(ctx context.Context, taskID int64, agent string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages m WHERE m.task_id = ? AND m.to_agent IN (?, '*') AND m.from_agent <> ?
		AND NOT EXISTS (SELECT 1 FROM message_reads r WHERE r.message_id = m.id AND r.agent = ?)`, taskID, agent, agent, agent).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count unread messages of %s: %w", agent, err)
	}
	return n, nil
}

// Contract is one frozen version of a task's contract.md.
type Contract struct {
	TaskID   int64
	Version  int
	SHA256   string
	FrozenAt string
	FrozenBy string
}

// LatestContract returns the newest frozen contract version of a task.
func (s *Store) LatestContract(ctx context.Context, taskID int64) (Contract, bool, error) {
	var c Contract
	err := s.db.QueryRowContext(ctx, `SELECT task_id, version, sha256, frozen_at, frozen_by FROM contracts
		WHERE task_id = ? ORDER BY version DESC LIMIT 1`, taskID).Scan(&c.TaskID, &c.Version, &c.SHA256, &c.FrozenAt, &c.FrozenBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Contract{}, false, nil
	}
	if err != nil {
		return Contract{}, false, fmt.Errorf("read contract of task %d: %w", taskID, err)
	}
	return c, true, nil
}

// FreezeContract records sha as the next contract version unless it equals
// the latest one (changed is then false). A version after the first needs a
// change note, which is broadcast as a contract-change message in the same
// transaction (§7.4).
func (s *Store) FreezeContract(ctx context.Context, taskID int64, sha, by, note string) (Contract, bool, error) {
	var c Contract
	changed := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		changed = false
		var latest Contract
		err := tx.QueryRowContext(ctx, `SELECT version, sha256 FROM contracts WHERE task_id = ? ORDER BY version DESC LIMIT 1`, taskID).
			Scan(&latest.Version, &latest.SHA256)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read contract of task %d: %w", taskID, err)
		}
		if latest.SHA256 == sha {
			c = Contract{TaskID: taskID, Version: latest.Version, SHA256: sha}
			return nil
		}
		if latest.Version > 0 && strings.TrimSpace(note) == "" {
			return fmt.Errorf("%w: contract.md changed since version %d; a new version needs --note naming what changed", ErrStage, latest.Version)
		}
		c = Contract{TaskID: taskID, Version: latest.Version + 1, SHA256: sha, FrozenAt: now(), FrozenBy: by}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contracts (task_id, version, sha256, frozen_at, frozen_by) VALUES (?, ?, ?, ?, ?)`,
			c.TaskID, c.Version, c.SHA256, c.FrozenAt, c.FrozenBy); err != nil {
			return fmt.Errorf("freeze contract version %d of task %d: %w", c.Version, taskID, err)
		}
		if c.Version > 1 {
			subject := fmt.Sprintf("contract.md is now version %d", c.Version)
			if _, err := InsertMessage(ctx, tx, Message{TaskID: taskID, From: by, To: "*", Kind: "contract-change",
				Subject: subject, Body: note}); err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	return c, changed, err
}

// Claim is one row of file_claims.
type Claim struct {
	ID         int64
	TaskID     int64
	Agent      string
	Glob       string
	Kind       string
	ReleasedAt string
}

// Claims returns a task's open claims, oldest first.
func (s *Store) Claims(ctx context.Context, taskID int64) ([]Claim, error) {
	return claims(ctx, s.db, taskID)
}

func claims(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, taskID int64) ([]Claim, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, task_id, agent, path_glob, kind, COALESCE(released_at, '') FROM file_claims
		WHERE task_id = ? AND released_at IS NULL ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read claims of task %d: %w", taskID, err)
	}
	defer rows.Close()
	var out []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Agent, &c.Glob, &c.Kind, &c.ReleasedAt); err != nil {
			return nil, fmt.Errorf("scan claim of task %d: %w", taskID, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddClaims gives agent the expected claims globs inside one task. overlaps
// decides whether two globs can cover the same path; a glob that overlaps a
// claim of another agent refuses the whole call with ErrClaimConflict. A glob
// the agent already holds is kept as is.
func (s *Store) AddClaims(ctx context.Context, taskID int64, agent string, globs []string, overlaps func(a, b string) bool) (int, error) {
	added := 0
	err := s.write(ctx, func(tx *sql.Tx) error {
		added = 0
		open, err := claims(ctx, tx, taskID)
		if err != nil {
			return err
		}
		ts := now()
		for _, g := range globs {
			held := false
			for _, c := range open {
				switch {
				case c.Agent == agent && c.Glob == g:
					held = true
				case c.Agent != agent && overlaps(c.Glob, g):
					return fmt.Errorf("%w: %s overlaps %s, held by %s", ErrClaimConflict, g, c.Glob, c.Agent)
				}
			}
			if held {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO file_claims (task_id, agent, path_glob, kind, created_at) VALUES (?, ?, ?, 'expected', ?)`,
				taskID, agent, g, ts); err != nil {
				return fmt.Errorf("claim %s for %s: %w", g, agent, err)
			}
			open = append(open, Claim{TaskID: taskID, Agent: agent, Glob: g, Kind: "expected"})
			added++
		}
		return nil
	})
	return added, err
}

// ReleaseClaims releases agent's open claims on globs, or all of them when
// globs is empty, and returns how many it released.
func (s *Store) ReleaseClaims(ctx context.Context, taskID int64, agent string, globs []string) (int, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		q := `UPDATE file_claims SET released_at = ?, version = version + 1 WHERE task_id = ? AND agent = ? AND released_at IS NULL`
		args := []any{now(), taskID, agent}
		if len(globs) > 0 {
			q += ` AND path_glob IN (` + placeholders(len(globs)) + `)`
			args = append(args, anys(globs)...)
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("release claims of %s: %w", agent, err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

// ReleaseTaskClaims releases every open claim of a task inside a caller's transaction.
func ReleaseTaskClaims(ctx context.Context, tx *sql.Tx, taskID int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE file_claims SET released_at = ?, version = version + 1 WHERE task_id = ? AND released_at IS NULL`,
		now(), taskID); err != nil {
		return fmt.Errorf("release claims of task %d: %w", taskID, err)
	}
	return nil
}
