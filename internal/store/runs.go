package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type Run struct {
	ID          int64
	TaskID      int64
	Kind        string
	SHA         string
	Command     string
	CommandHash string
	Format      string
	Fidelity    string
	Status      string
	ExitCode    *int
	Total       int
	Passed      int
	Failed      int
	Skipped     int
	OutputPath  string
	PID         int    // the suite's pid (its process group on unix), 0 until it starts
	PIDStart    string // the start time of the process at PID, "" when unknown
	StartedAt   string
	FinishedAt  string
}

type Result struct {
	TestID     string
	Status     string
	DurationMS int64
	Message    string
	Attempt    int // 1 for the first run of the suite, 2.. for flaky re-runs (§8.7); 0 means 1
}

const runColumns = `id, COALESCE(task_id, 0), kind, sha, command, command_hash, format, fidelity, status, exit_code,
	total, passed, failed, skipped, output_path, COALESCE(pid, 0), COALESCE(pid_start, ''), started_at, COALESCE(finished_at, '')`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var exit sql.NullInt64
	err := row.Scan(&r.ID, &r.TaskID, &r.Kind, &r.SHA, &r.Command, &r.CommandHash, &r.Format, &r.Fidelity, &r.Status,
		&exit, &r.Total, &r.Passed, &r.Failed, &r.Skipped, &r.OutputPath, &r.PID, &r.PIDStart, &r.StartedAt, &r.FinishedAt)
	if exit.Valid {
		code := int(exit.Int64)
		r.ExitCode = &code
	}
	return r, err
}

// StartRun inserts a running test_runs row. outputPath maps the new run id to
// the run's output.log path.
func (s *Store) StartRun(ctx context.Context, r Run, outputPath func(id int64) string) (Run, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO test_runs (task_id, kind, sha, command, command_hash, format, fidelity,
			status, output_path, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'running', '', ?)`,
			r.TaskID, r.Kind, r.SHA, r.Command, r.CommandHash, r.Format, r.Fidelity, now())
		if err != nil {
			return fmt.Errorf("insert %s run for task %d: %w", r.Kind, r.TaskID, err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("insert %s run: %w", r.Kind, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE test_runs SET output_path = ? WHERE id = ?`, outputPath(id), id); err != nil {
			return fmt.Errorf("set output path of run %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return Run{}, err
	}
	return s.GetRun(ctx, id)
}

// SetRunPID records the pid of a running suite and that process's start
// time, so a later command can tell whether a run its owner abandoned is
// still going and that the pid was not reused.
func (s *Store) SetRunPID(ctx context.Context, id int64, pid int, start string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE test_runs SET pid = ?, pid_start = ? WHERE id = ? AND status = 'running'`, pid, start, id); err != nil {
			return fmt.Errorf("set pid of run %d: %w", id, err)
		}
		return nil
	})
}

// RunningRuns returns a task's runs still marked running, oldest first.
func (s *Store) RunningRuns(ctx context.Context, taskID int64) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM test_runs WHERE task_id = ? AND status = 'running' ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list running runs of task %d: %w", taskID, err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run of task %d: %w", taskID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list running runs of task %d: %w", taskID, err)
	}
	return out, nil
}

// FinishRun stores the outcome and per-test results of a run, every attempt
// included, in one transaction. The pass/fail/skip counts are of each test's
// effective status (a flaky test counts as passed).
func (s *Store) FinishRun(ctx context.Context, id int64, status string, exitCode *int, results []Result) (Run, error) {
	var passed, failed, skipped int
	attempts := 1
	eff := effective(results)
	for _, r := range eff {
		switch r.Status {
		case "pass", StatusFlaky:
			passed++
		case "fail":
			failed++
		case "skip":
			skipped++
		}
	}
	for _, r := range results {
		if r.Attempt > attempts {
			attempts = r.Attempt
		}
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO test_results (run_id, test_id, attempt, status, duration_ms, message)
			VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare results insert for run %d: %w", id, err)
		}
		defer stmt.Close()
		for _, r := range results {
			if _, err := stmt.ExecContext(ctx, id, r.TestID, max(r.Attempt, 1), r.Status, r.DurationMS, r.Message); err != nil {
				return fmt.Errorf("insert result %q (attempt %d) for run %d: %w", r.TestID, max(r.Attempt, 1), id, err)
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE test_runs SET status = ?, exit_code = ?, attempts = ?, total = ?, passed = ?, failed = ?,
			skipped = ?, finished_at = ? WHERE id = ?`, status, exitCode, attempts, len(eff), passed, failed, skipped, now(), id)
		if err != nil {
			return fmt.Errorf("finish run %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return Run{}, err
	}
	return s.GetRun(ctx, id)
}

// StatusFlaky is the effective status of a test that failed on the first
// attempt of a run and passed on a re-run (§8.7).
const StatusFlaky = "flaky"

// effective reduces per-attempt results to one per test, in first-attempt
// order: the attempt-1 status, except that a test failing at attempt 1 is
// flaky if a later attempt passed and fail otherwise (also when a re-run
// did not report it). The message is the first failure's.
func effective(results []Result) []Result {
	var order []string
	first := map[string]Result{}
	last := map[string]Result{}
	for _, r := range results {
		a := max(r.Attempt, 1)
		if a == 1 {
			if _, dup := first[r.TestID]; !dup {
				order = append(order, r.TestID)
			}
			first[r.TestID] = r
			continue
		}
		if prev, ok := last[r.TestID]; !ok || max(prev.Attempt, 1) <= a {
			last[r.TestID] = r
		}
	}
	out := make([]Result, 0, len(order))
	for _, id := range order {
		r := first[id]
		if r.Status == "fail" {
			if l, ok := last[id]; ok && l.Status == "pass" {
				r.Status = StatusFlaky
			}
		}
		r.Attempt = 1
		out = append(out, r)
	}
	return out
}

// EffectiveResults returns one result per test of a run, in test id order,
// with status pass, fail, skip or flaky (see effective).
func (s *Store) EffectiveResults(ctx context.Context, runID int64) ([]Result, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT test_id, attempt, status, COALESCE(duration_ms, 0), COALESCE(message, '')
		FROM test_results WHERE run_id = ? ORDER BY test_id, attempt`, runID)
	if err != nil {
		return nil, fmt.Errorf("read results of run %d: %w", runID, err)
	}
	defer rows.Close()
	var all []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.TestID, &r.Attempt, &r.Status, &r.DurationMS, &r.Message); err != nil {
			return nil, fmt.Errorf("scan result of run %d: %w", runID, err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read results of run %d: %w", runID, err)
	}
	return effective(all), nil
}

// LatestRun returns the task's newest completed run of one of kinds.
func (s *Store) LatestRun(ctx context.Context, taskID int64, kinds ...string) (Run, bool, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM test_runs
		WHERE task_id = ? AND status = 'completed' AND kind IN (`+placeholders(len(kinds))+`) ORDER BY id DESC LIMIT 1`,
		append([]any{taskID}, anys(kinds)...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("latest %v run of task %d: %w", kinds, taskID, err)
	}
	return r, true, nil
}

func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM test_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("test run %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Run{}, fmt.Errorf("read test run %d: %w", id, err)
	}
	return r, nil
}

// FindBaseline returns the newest completed baseline for sha and commandHash.
func (s *Store) FindBaseline(ctx context.Context, sha, commandHash string) (Run, bool, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM test_runs
		WHERE sha = ? AND command_hash = ? AND kind = 'baseline' AND status = 'completed' ORDER BY id DESC LIMIT 1`, sha, commandHash))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("find baseline for %s: %w", sha, err)
	}
	return r, true, nil
}

// Results returns a run's results in test id order.
func (s *Store) Results(ctx context.Context, runID int64) ([]Result, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT test_id, status, COALESCE(duration_ms, 0), COALESCE(message, '')
		FROM test_results WHERE run_id = ? AND attempt = 1 ORDER BY test_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("read results of run %d: %w", runID, err)
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		if err := rows.Scan(&r.TestID, &r.Status, &r.DurationMS, &r.Message); err != nil {
			return nil, fmt.Errorf("scan result of run %d: %w", runID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read results of run %d: %w", runID, err)
	}
	return out, nil
}
