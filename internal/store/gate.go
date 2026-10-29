package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Quarantine is one row of test_quarantine (§8.7).
type Quarantine struct {
	TestID        string
	DetectedRunID int64
	Reason        string
	QuarantinedAt string
	ReleasedAt    string
}

// QuarantineTests puts flaky tests of a baseline run into quarantine; a test
// already in open quarantine is left as it is. It returns how many it added.
func (s *Store) QuarantineTests(ctx context.Context, runID int64, flaky []Result) (int, error) {
	added := 0
	err := s.write(ctx, func(tx *sql.Tx) error {
		added = 0
		ts := now()
		for _, r := range flaky {
			reason := "flaky at baseline: failed, then passed on a re-run"
			if r.Message != "" {
				reason += ": " + r.Message
			}
			res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO test_quarantine (test_id, detected_run_id, reason, quarantined_at)
				VALUES (?, ?, ?, ?)`, r.TestID, runID, reason, ts)
			if err != nil {
				return fmt.Errorf("quarantine %s: %w", r.TestID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("quarantine %s: %w", r.TestID, err)
			}
			added += int(n)
		}
		return nil
	})
	return added, err
}

// Quarantined returns quarantine rows, open ones only unless all is set.
func (s *Store) Quarantined(ctx context.Context, all bool) ([]Quarantine, error) {
	q := `SELECT test_id, detected_run_id, reason, quarantined_at, COALESCE(released_at, '') FROM test_quarantine`
	if !all {
		q += ` WHERE released_at IS NULL`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY test_id, id`)
	if err != nil {
		return nil, fmt.Errorf("read quarantine: %w", err)
	}
	defer rows.Close()
	var out []Quarantine
	for rows.Next() {
		var r Quarantine
		if err := rows.Scan(&r.TestID, &r.DetectedRunID, &r.Reason, &r.QuarantinedAt, &r.ReleasedAt); err != nil {
			return nil, fmt.Errorf("scan quarantine: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReleaseQuarantine releases a test from open quarantine.
func (s *Store) ReleaseQuarantine(ctx context.Context, testID string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE test_quarantine SET released_at = ? WHERE test_id = ? AND released_at IS NULL`, now(), testID)
		if err != nil {
			return fmt.Errorf("release %s from quarantine: %w", testID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("quarantined test %q: %w", testID, ErrNotFound)
		}
		return nil
	})
}

// CoverageFile is one row of coverage_files.
type CoverageFile struct {
	Path         string
	LinesTotal   int
	LinesCovered int
	Uncovered    string // line ranges like "4-9,12"
}

// SetCoverage stores a run's per-file coverage.
func (s *Store) SetCoverage(ctx context.Context, runID int64, files []CoverageFile) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, f := range files {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO coverage_files (run_id, path, lines_total, lines_covered, uncovered)
				VALUES (?, ?, ?, ?, ?)`, runID, f.Path, f.LinesTotal, f.LinesCovered, f.Uncovered); err != nil {
				return fmt.Errorf("store coverage of %s for run %d: %w", f.Path, runID, err)
			}
		}
		return nil
	})
}

// Coverage returns a run's per-file coverage keyed by path, or nil when the
// run recorded none.
func (s *Store) Coverage(ctx context.Context, runID int64) (map[string]CoverageFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, lines_total, lines_covered, uncovered FROM coverage_files WHERE run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("read coverage of run %d: %w", runID, err)
	}
	defer rows.Close()
	var out map[string]CoverageFile
	for rows.Next() {
		var f CoverageFile
		if err := rows.Scan(&f.Path, &f.LinesTotal, &f.LinesCovered, &f.Uncovered); err != nil {
			return nil, fmt.Errorf("scan coverage of run %d: %w", runID, err)
		}
		if out == nil {
			out = map[string]CoverageFile{}
		}
		out[f.Path] = f
	}
	return out, rows.Err()
}

// Scope item sources and case checks (§8.4, §8.6).
const (
	ScopeUser      = "user"
	ScopeBehaviour = "behaviour"
	CheckAutomated = "automated"
	CheckManual    = "manual"
)

type ScopeItem struct {
	ID     int64
	TaskID int64
	Source string
	Text   string
	Cases  []ScopeCase
}

type ScopeCase struct {
	ID          int64
	ItemID      int64
	Category    string
	Description string
	Check       string
	TestID      string
	ManualStep  string
	Status      string // pending | pass | fail | waived (manual decisions and waivers)
	WaiveReason string
	Note        string
	DecidedBy   string
	DecidedAt   string
	Version     int64
}

func insertScopeItem(ctx context.Context, tx *sql.Tx, taskID int64, source, text string) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO scope_items (task_id, source, text, created_at) VALUES (?, ?, ?, ?)`, taskID, source, text, now())
	if err != nil {
		return 0, fmt.Errorf("add %s scope item %q: %w", source, text, err)
	}
	return res.LastInsertId()
}

// AddScopeItem adds a user scope item or a feature behaviour to a task.
func (s *Store) AddScopeItem(ctx context.Context, taskID int64, source, text string) (int64, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = insertScopeItem(ctx, tx, taskID, source, text)
		return err
	})
	return id, err
}

// AddScopeCase adds a case row to a scope item of the task.
func (s *Store) AddScopeCase(ctx context.Context, taskID int64, c ScopeCase) (int64, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		var owner int64
		err := tx.QueryRowContext(ctx, `SELECT task_id FROM scope_items WHERE id = ?`, c.ItemID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != taskID) {
			return fmt.Errorf("scope item %d of this task: %w", c.ItemID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read scope item %d: %w", c.ItemID, err)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO scope_cases (scope_item_id, category, description, "check", test_id, manual_step, note, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, c.ItemID, c.Category, c.Description, c.Check, nullable(c.TestID), nullable(c.ManualStep), nullable(c.Note), now())
		if err != nil {
			return fmt.Errorf("add case to scope item %d: %w", c.ItemID, err)
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// ScopeItems returns a task's scope items with their cases, in id order.
func (s *Store) ScopeItems(ctx context.Context, taskID int64) ([]ScopeItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, source, text FROM scope_items WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read scope of task %d: %w", taskID, err)
	}
	var items []ScopeItem
	index := map[int64]int{}
	for rows.Next() {
		var it ScopeItem
		if err := rows.Scan(&it.ID, &it.TaskID, &it.Source, &it.Text); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan scope item: %w", err)
		}
		index[it.ID] = len(items)
		items = append(items, it)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	crows, err := s.db.QueryContext(ctx, `SELECT c.id, c.scope_item_id, c.category, c.description, c."check", COALESCE(c.test_id, ''),
		COALESCE(c.manual_step, ''), c.status, COALESCE(c.waive_reason, ''), COALESCE(c.note, ''), COALESCE(c.decided_by, ''),
		COALESCE(c.decided_at, ''), c.version
		FROM scope_cases c JOIN scope_items i ON i.id = c.scope_item_id WHERE i.task_id = ? ORDER BY c.id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read scope cases of task %d: %w", taskID, err)
	}
	defer crows.Close()
	for crows.Next() {
		var c ScopeCase
		if err := crows.Scan(&c.ID, &c.ItemID, &c.Category, &c.Description, &c.Check, &c.TestID, &c.ManualStep, &c.Status,
			&c.WaiveReason, &c.Note, &c.DecidedBy, &c.DecidedAt, &c.Version); err != nil {
			return nil, fmt.Errorf("scan scope case: %w", err)
		}
		i := index[c.ItemID]
		items[i].Cases = append(items[i].Cases, c)
	}
	return items, crows.Err()
}

// DecideCase records a human decision on a case row (pass, fail or waived),
// guarded by the row's version (§10.2).
func (s *Store) DecideCase(ctx context.Context, c ScopeCase, status, reason, note, by string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE scope_cases SET status = ?, waive_reason = ?, note = COALESCE(?, note), decided_by = ?, decided_at = ?,
			version = version + 1 WHERE id = ? AND version = ?`, status, nullable(reason), nullable(note), by, now(), c.ID, c.Version)
		if err != nil {
			return fmt.Errorf("decide scope case %d: %w", c.ID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: scope case %d changed under us (read version %d)", ErrConflict, c.ID, c.Version)
		}
		return nil
	})
}

// Proof is a red/green regression proof (§8.3).
type Proof struct {
	TaskID      int64
	TestID      string
	TestFile    string
	RedStatus   string // fail | not-reproducible
	RedRunID    int64
	RedSHA      string
	GreenRunID  int64
	GreenSHA    string
	TestChanged bool
}

// SetRed records (or restarts) the red half of a proof, clearing any green.
func (s *Store) SetRed(ctx context.Context, p Proof) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO regression_proofs (task_id, test_id, test_file, red_status, red_run_id, red_sha)
			VALUES (?, ?, ?, ?, ?, ?)`, p.TaskID, p.TestID, p.TestFile, p.RedStatus, p.RedRunID, p.RedSHA)
		if err != nil {
			return fmt.Errorf("record red run of %s: %w", p.TestID, err)
		}
		return nil
	})
}

// SetGreen records the green half of a proof whose red half exists.
func (s *Store) SetGreen(ctx context.Context, taskID int64, testID string, runID int64, sha string, changed bool) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE regression_proofs SET green_run_id = ?, green_sha = ?, test_changed_after_red = ?
			WHERE task_id = ? AND test_id = ?`, runID, sha, changed, taskID, testID)
		if err != nil {
			return fmt.Errorf("record green run of %s: %w", testID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("red run of %s: %w", testID, ErrNotFound)
		}
		return nil
	})
}

// Proofs returns a task's regression proofs.
func (s *Store) Proofs(ctx context.Context, taskID int64) ([]Proof, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, test_id, test_file, red_status, red_run_id, red_sha, COALESCE(green_run_id, 0),
		COALESCE(green_sha, ''), test_changed_after_red FROM regression_proofs WHERE task_id = ? ORDER BY test_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read proofs of task %d: %w", taskID, err)
	}
	defer rows.Close()
	var out []Proof
	for rows.Next() {
		var p Proof
		if err := rows.Scan(&p.TaskID, &p.TestID, &p.TestFile, &p.RedStatus, &p.RedRunID, &p.RedSHA, &p.GreenRunID, &p.GreenSHA, &p.TestChanged); err != nil {
			return nil, fmt.Errorf("scan proof: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Override is one accepted regression (§8.5).
type Override struct {
	TaskID     int64
	GateRunID  int64
	Target     string
	Reason     string
	AcceptedBy string
	AcceptedAt string
}

// InsertOverride records an accepted regression inside a caller's transaction.
func InsertOverride(ctx context.Context, tx *sql.Tx, o Override) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO regression_overrides (task_id, gate_run_id, target, reason, accepted_by, accepted_at)
		VALUES (?, ?, ?, ?, ?, ?)`, o.TaskID, o.GateRunID, o.Target, o.Reason, o.AcceptedBy, now()); err != nil {
		return fmt.Errorf("record accepted regression %s: %w", o.Target, err)
	}
	return nil
}

// Overrides returns a task's accepted regressions.
func (s *Store) Overrides(ctx context.Context, taskID int64) ([]Override, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, gate_run_id, target, reason, accepted_by, accepted_at FROM regression_overrides
		WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read overrides of task %d: %w", taskID, err)
	}
	defer rows.Close()
	var out []Override
	for rows.Next() {
		var o Override
		if err := rows.Scan(&o.TaskID, &o.GateRunID, &o.Target, &o.Reason, &o.AcceptedBy, &o.AcceptedAt); err != nil {
			return nil, fmt.Errorf("scan override: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// QARound is one shoalctl qa round (§6.4).
type QARound struct {
	TaskID   int64
	Round    int
	RunID    int64
	Passed   bool
	Failures string // JSON array of strings
}

// InsertQARound records a round inside a caller's transaction.
func InsertQARound(ctx context.Context, tx *sql.Tx, r QARound) error {
	var run any
	if r.RunID != 0 {
		run = r.RunID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO qa_rounds (task_id, round, run_id, passed, failures, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.TaskID, r.Round, run, r.Passed, r.Failures, now()); err != nil {
		return fmt.Errorf("record qa round %d of task %d: %w", r.Round, r.TaskID, err)
	}
	return nil
}

// QARounds returns a task's QA rounds in order.
func (s *Store) QARounds(ctx context.Context, taskID int64) ([]QARound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, round, COALESCE(run_id, 0), passed, failures FROM qa_rounds WHERE task_id = ? ORDER BY round`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read qa rounds of task %d: %w", taskID, err)
	}
	defer rows.Close()
	var out []QARound
	for rows.Next() {
		var r QARound
		if err := rows.Scan(&r.TaskID, &r.Round, &r.RunID, &r.Passed, &r.Failures); err != nil {
			return nil, fmt.Errorf("scan qa round: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetSummary returns a task's summaries row.
func (s *Store) GetSummary(ctx context.Context, taskID int64) (Summary, error) {
	var sm Summary
	err := s.db.QueryRowContext(ctx, `SELECT task_id, decision, alternatives_rejected, why, verification, risks, followups, changed_notes,
		commits, files, true_at_sha, report_path FROM summaries WHERE task_id = ?`, taskID).Scan(&sm.TaskID, &sm.Decision,
		&sm.AlternativesRejected, &sm.Why, &sm.Verification, &sm.Risks, &sm.Followups, &sm.ChangedNotes, &sm.Commits, &sm.Files,
		&sm.TrueAtSHA, &sm.ReportPath)
	if errors.Is(err, sql.ErrNoRows) {
		return Summary{}, fmt.Errorf("summary of task %d: %w", taskID, ErrNotFound)
	}
	if err != nil {
		return Summary{}, fmt.Errorf("read summary of task %d: %w", taskID, err)
	}
	return sm, nil
}

// UpdateSummaryCommits rewrites a summary's commits and true_at_sha after a
// rebase, inside a caller's transaction.
func UpdateSummaryCommits(ctx context.Context, tx *sql.Tx, taskID int64, commits, trueAt, verification string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE summaries SET commits = ?, true_at_sha = ?, verification = ? WHERE task_id = ?`,
		commits, trueAt, verification, taskID); err != nil {
		return fmt.Errorf("update summary of task %d: %w", taskID, err)
	}
	return nil
}

// DoneTasks returns tasks at stage done, oldest done first (the merge order).
func (s *Store) DoneTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE stage = ? ORDER BY closed_at, id`, StageDone)
	if err != nil {
		return nil, fmt.Errorf("list done tasks: %w", err)
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
	return out, rows.Err()
}

// Children returns the tasks whose parent_task_id is id.
func (s *Store) Children(ctx context.Context, id int64) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE parent_task_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("list children of task %d: %w", id, err)
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
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
