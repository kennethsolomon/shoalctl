package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Dump is every row of every table in memory.db, read in one transaction.
// memory_fts is left out: triggers rebuild it from the tables it indexes.
type Dump struct {
	SchemaVersion int                         `json:"schema_version"`
	Tables        map[string][]map[string]any `json:"tables"`
}

// Dump reads the whole database for export (§13 offboarding). TEXT and BLOB
// values come out as strings, NULL as null.
func (s *Store) Dump(ctx context.Context) (Dump, error) {
	d := Dump{Tables: map[string][]map[string]any{}}
	err := s.write(ctx, func(tx *sql.Tx) error {
		v, err := s.userVersion(ctx, tx)
		if err != nil {
			return err
		}
		d.SchemaVersion = v
		names, err := tableNames(ctx, tx)
		if err != nil {
			return err
		}
		for _, name := range names {
			rows, err := dumpTable(ctx, tx, name)
			if err != nil {
				return err
			}
			d.Tables[name] = rows
		}
		return nil
	})
	return d, err
}

func tableNames(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name NOT LIKE 'memory\_fts%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan table name: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func dumpTable(ctx context.Context, tx *sql.Tx, name string) ([]map[string]any, error) {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM "`+name+`"`)
	if err != nil {
		return nil, fmt.Errorf("read table %s: %w", name, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns of %s: %w", name, err)
	}
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan a row of %s: %w", name, err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// WorkingRows counts what DropWorkingRows deleted, by table.
type WorkingRows map[string]int64

// DropWorkingRows deletes a closed (merged or folded) task's working rows,
// the ones only an open task reads: its messages and their read marks,
// file claims, hook marks, blocker rows on either side, and its merge-queue
// row. The task row, its summary and every record (bugs, decisions,
// lessons, answered questions, test runs, scope, contracts, QA rounds,
// overrides, proofs, lesson hits) stay (§7.4: gc drops them after merge,
// keeping the summary). One transaction; a task that is not closed is
// refused with ErrStage.
func (s *Store) DropWorkingRows(ctx context.Context, taskID int64) (WorkingRows, error) {
	out := WorkingRows{}
	err := s.write(ctx, func(tx *sql.Tx) error {
		var stage string
		if err := tx.QueryRowContext(ctx, `SELECT stage FROM tasks WHERE id = ?`, taskID).Scan(&stage); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("task %d: %w", taskID, ErrNotFound)
			}
			return fmt.Errorf("read task %d: %w", taskID, err)
		}
		if stage != StageMerged && stage != StageFolded {
			return fmt.Errorf("drop the working rows of task %d at %s: %w", taskID, stage, ErrStage)
		}
		for _, d := range []struct{ table, query string }{
			{"message_reads", `DELETE FROM message_reads WHERE message_id IN (SELECT id FROM messages WHERE task_id = ?1)`},
			{"messages", `DELETE FROM messages WHERE task_id = ?1`},
			{"file_claims", `DELETE FROM file_claims WHERE task_id = ?1`},
			{"hook_marks", `DELETE FROM hook_marks WHERE task_id = ?1`},
			{"task_blockers", `DELETE FROM task_blockers WHERE task_id = ?1 OR blocker_id = ?1`},
			{"merge_queue", `DELETE FROM merge_queue WHERE task_id = ?1`},
		} {
			res, err := tx.ExecContext(ctx, d.query, taskID)
			if err != nil {
				return fmt.Errorf("drop %s of task %d: %w", d.table, taskID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("drop %s of task %d: %w", d.table, taskID, err)
			}
			if n > 0 {
				out[d.table] = n
			}
		}
		return nil
	})
	return out, err
}
