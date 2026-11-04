package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Memory record kinds (§4.5): the WHY behind commits. Each kind maps to its
// table; a record is named "<kind>:<id>" on the command line.
const (
	KindBug      = "bug"
	KindDecision = "decision"
	KindLesson   = "lesson"
	KindQA       = "qa"
	KindSummary  = "summary"
)

// recordTables are the kinds a record id can name, and their tables.
var recordTables = map[string]string{KindBug: "bugs", KindDecision: "decisions", KindLesson: "lessons", KindQA: "qa"}

// RegressionTest is a bug's regression test and the file it lives in.
type RegressionTest struct {
	Test string `json:"test"`
	File string `json:"file"`
}

type Bug struct {
	ID          int64
	TaskID      int64
	Symptom     string
	RootCause   string
	FixSHA      string
	Files       []string
	Tests       []RegressionTest
	TrueAtSHA   string
	Stale       bool
	StaleReason string
	CreatedAt   string
}

type Decision struct {
	ID           int64
	TaskID       int64
	Topic        string
	Choice       string
	Rationale    string
	Files        []string
	TrueAtSHA    string
	Stale        bool
	StaleReason  string
	SupersededBy int64
	CreatedAt    string
}

type Lesson struct {
	ID          int64
	TaskID      int64
	QARound     int // 0 when not tied to a failed round
	Pattern     string
	Fix         string
	Stack       string
	PathGlob    string // "" applies to the whole stack
	Hits        int
	LastHitAt   string
	Promoted    bool
	TrueAtSHA   string
	Stale       bool
	StaleReason string
	CreatedAt   string
}

// QANote is a question answered during work (the qa table).
type QANote struct {
	ID          int64
	TaskID      int64
	Question    string
	Answer      string
	Files       []string
	TrueAtSHA   string
	Stale       bool
	StaleReason string
	CreatedAt   string
}

// SummaryRecord is the part of a task's summary that why and recall show.
type SummaryRecord struct {
	TaskID    int64
	Slug      string
	Title     string
	Decision  string
	Why       string
	Files     []string // paths changed (rename sources included)
	Commits   []string
	TrueAtSHA string
}

func encodeJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode %T: %w", v, err)
	}
	return string(b), nil
}

func nonNilList(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// AddDecision records a decision and returns its id.
func (s *Store) AddDecision(ctx context.Context, d Decision) (int64, error) {
	files, err := encodeJSON(nonNilList(d.Files))
	if err != nil {
		return 0, err
	}
	return s.insertRecord(ctx, "decision", `INSERT INTO decisions (task_id, topic, choice, rationale, files, true_at_sha, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, d.TaskID, d.Topic, d.Choice, d.Rationale, files, d.TrueAtSHA, now())
}

// AddLesson records a lesson and returns its id.
func (s *Store) AddLesson(ctx context.Context, l Lesson) (int64, error) {
	var round any
	if l.QARound > 0 {
		round = l.QARound
	}
	return s.insertRecord(ctx, "lesson", `INSERT INTO lessons (task_id, qa_round, pattern, fix, stack, path_glob, true_at_sha, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, l.TaskID, round, l.Pattern, l.Fix, l.Stack, l.PathGlob, l.TrueAtSHA, now())
}

// AddQANote records an answered question and returns its id.
func (s *Store) AddQANote(ctx context.Context, q QANote) (int64, error) {
	files, err := encodeJSON(nonNilList(q.Files))
	if err != nil {
		return 0, err
	}
	return s.insertRecord(ctx, "qa", `INSERT INTO qa (task_id, question, answer, files, true_at_sha, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, q.TaskID, q.Question, q.Answer, files, q.TrueAtSHA, now())
}

func (s *Store) insertRecord(ctx context.Context, kind, query string, args ...any) (int64, error) {
	var id int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("record %s: %w", kind, err)
		}
		id, err = res.LastInsertId()
		if err != nil {
			return fmt.Errorf("record %s: %w", kind, err)
		}
		return nil
	})
	return id, err
}

// UpsertBugTx writes the bug record of a finished bugfix or hotfix task
// inside done's transaction, replacing the one an earlier done wrote.
func UpsertBugTx(ctx context.Context, tx *sql.Tx, b Bug) error {
	files, err := encodeJSON(nonNilList(b.Files))
	if err != nil {
		return err
	}
	tests := b.Tests
	if tests == nil {
		tests = []RegressionTest{}
	}
	enc, err := encodeJSON(tests)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO bugs (task_id, symptom, root_cause, fix_sha, files, regression_tests, true_at_sha, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET symptom = excluded.symptom, root_cause = excluded.root_cause, fix_sha = excluded.fix_sha,
			files = excluded.files, regression_tests = excluded.regression_tests, true_at_sha = excluded.true_at_sha, stale = 0, stale_reason = NULL`,
		b.TaskID, b.Symptom, b.RootCause, b.FixSHA, files, enc, b.TrueAtSHA, now())
	if err != nil {
		return fmt.Errorf("record bug of task %d: %w", b.TaskID, err)
	}
	return nil
}

// MoveBugFixTx points a task's bug record at the commit that landed on the
// target branch, inside merge's transaction. A task without a bug record
// (a feature) is left alone.
func MoveBugFixTx(ctx context.Context, tx *sql.Tx, taskID int64, sha string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE bugs SET fix_sha = ?, true_at_sha = ? WHERE task_id = ?`, sha, sha, taskID); err != nil {
		return fmt.Errorf("move bug fix of task %d to %s: %w", taskID, sha, err)
	}
	return nil
}

func decodeList(raw, what string, dst any) error {
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return fmt.Errorf("decode %s: %w", what, err)
	}
	return nil
}

// Bugs returns every bug record, oldest first.
func (s *Store) Bugs(ctx context.Context) ([]Bug, error) {
	return s.bugs(ctx, "", nil)
}

// GetBug returns one bug record.
func (s *Store) GetBug(ctx context.Context, id int64) (Bug, error) {
	bs, err := s.bugs(ctx, "WHERE id = ?", []any{id})
	if err != nil {
		return Bug{}, err
	}
	if len(bs) == 0 {
		return Bug{}, fmt.Errorf("bug:%d: %w", id, ErrNotFound)
	}
	return bs[0], nil
}

func (s *Store) bugs(ctx context.Context, where string, args []any) ([]Bug, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, symptom, root_cause, fix_sha, files, regression_tests, true_at_sha, stale,
		COALESCE(stale_reason, ''), created_at FROM bugs `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("read bugs: %w", err)
	}
	defer rows.Close()
	var out []Bug
	for rows.Next() {
		var b Bug
		var files, tests string
		if err := rows.Scan(&b.ID, &b.TaskID, &b.Symptom, &b.RootCause, &b.FixSHA, &files, &tests, &b.TrueAtSHA, &b.Stale, &b.StaleReason, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan bug: %w", err)
		}
		if err := decodeList(files, fmt.Sprintf("files of bug:%d", b.ID), &b.Files); err != nil {
			return nil, err
		}
		if err := decodeList(tests, fmt.Sprintf("regression tests of bug:%d", b.ID), &b.Tests); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Decisions returns every decision record, oldest first.
func (s *Store) Decisions(ctx context.Context) ([]Decision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, topic, choice, rationale, files, true_at_sha, stale, COALESCE(stale_reason, ''),
		COALESCE(superseded_by, 0), created_at FROM decisions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read decisions: %w", err)
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		var files string
		if err := rows.Scan(&d.ID, &d.TaskID, &d.Topic, &d.Choice, &d.Rationale, &files, &d.TrueAtSHA, &d.Stale, &d.StaleReason, &d.SupersededBy, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan decision: %w", err)
		}
		if err := decodeList(files, fmt.Sprintf("files of decision:%d", d.ID), &d.Files); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Lessons returns every lesson record, oldest first.
func (s *Store) Lessons(ctx context.Context) ([]Lesson, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, COALESCE(qa_round, 0), pattern, fix, stack, path_glob, hits, COALESCE(last_hit_at, ''),
		promoted, true_at_sha, stale, COALESCE(stale_reason, ''), created_at FROM lessons ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read lessons: %w", err)
	}
	defer rows.Close()
	var out []Lesson
	for rows.Next() {
		var l Lesson
		if err := rows.Scan(&l.ID, &l.TaskID, &l.QARound, &l.Pattern, &l.Fix, &l.Stack, &l.PathGlob, &l.Hits, &l.LastHitAt,
			&l.Promoted, &l.TrueAtSHA, &l.Stale, &l.StaleReason, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan lesson: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// QANotes returns every answered question, oldest first.
func (s *Store) QANotes(ctx context.Context) ([]QANote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, question, answer, files, true_at_sha, stale, COALESCE(stale_reason, ''), created_at
		FROM qa ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read qa records: %w", err)
	}
	defer rows.Close()
	var out []QANote
	for rows.Next() {
		var q QANote
		var files string
		if err := rows.Scan(&q.ID, &q.TaskID, &q.Question, &q.Answer, &files, &q.TrueAtSHA, &q.Stale, &q.StaleReason, &q.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan qa record: %w", err)
		}
		if err := decodeList(files, fmt.Sprintf("files of qa:%d", q.ID), &q.Files); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// SummaryRecords returns the summary of every task that ran done, oldest first.
func (s *Store) SummaryRecords(ctx context.Context) ([]SummaryRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.task_id, t.slug, t.title, s.decision, s.why, s.files, s.commits, s.true_at_sha
		FROM summaries s JOIN tasks t ON t.id = s.task_id ORDER BY s.task_id`)
	if err != nil {
		return nil, fmt.Errorf("read summaries: %w", err)
	}
	defer rows.Close()
	var out []SummaryRecord
	for rows.Next() {
		var r SummaryRecord
		var files, commits string
		if err := rows.Scan(&r.TaskID, &r.Slug, &r.Title, &r.Decision, &r.Why, &files, &commits, &r.TrueAtSHA); err != nil {
			return nil, fmt.Errorf("scan summary: %w", err)
		}
		var changed []struct {
			Path string `json:"path"`
			From string `json:"from"`
		}
		if err := decodeList(files, fmt.Sprintf("files of the summary of task %d", r.TaskID), &changed); err != nil {
			return nil, err
		}
		for _, c := range changed {
			r.Files = append(r.Files, c.Path)
			if c.From != "" {
				r.Files = append(r.Files, c.From)
			}
		}
		if err := decodeList(commits, fmt.Sprintf("commits of the summary of task %d", r.TaskID), &r.Commits); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ParseRecordRef reads "<kind>:<id>" for a bug, decision, lesson or qa record.
func ParseRecordRef(ref string) (string, int64, error) {
	kind, num, ok := strings.Cut(ref, ":")
	if _, known := recordTables[kind]; !ok || !known {
		return "", 0, fmt.Errorf("record %q must be bug:<id>, decision:<id>, lesson:<id> or qa:<id>", ref)
	}
	id, err := strconv.ParseInt(num, 10, 64)
	if err != nil || id <= 0 {
		return "", 0, fmt.Errorf("record %q: the id must be a positive number", ref)
	}
	return kind, id, nil
}

// FlagStale marks a record as no longer matching the code (§11.1). It stays
// searchable but is never injected again. It reports false when the record
// was already stale.
func (s *Store) FlagStale(ctx context.Context, kind string, id int64, reason string) (bool, error) {
	table, ok := recordTables[kind]
	if !ok {
		return false, fmt.Errorf("unknown record kind %q", kind)
	}
	changed := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		var stale bool
		err := tx.QueryRowContext(ctx, `SELECT stale FROM `+table+` WHERE id = ?`, id).Scan(&stale)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s:%d: %w", kind, id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read %s:%d: %w", kind, id, err)
		}
		if stale {
			changed = false
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET stale = 1, stale_reason = ? WHERE id = ?`, reason, id); err != nil {
			return fmt.Errorf("flag %s:%d stale: %w", kind, id, err)
		}
		changed = true
		return nil
	})
	return changed, err
}

// SearchHit is one full-text match: a record kind and its id (the task id
// for a summary), best match first.
type SearchHit struct {
	Kind string
	Ref  int64
}

// Search runs an FTS5 query over every record kind. Each word of query is
// matched as a quoted term (all must match), so user input never reaches
// FTS5's query syntax.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	var terms []string
	for _, w := range strings.Fields(query) {
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
	}
	if len(terms) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, ref FROM memory_fts WHERE memory_fts MATCH ? ORDER BY bm25(memory_fts) LIMIT ?`,
		strings.Join(terms, " "), limit)
	if err != nil {
		return nil, fmt.Errorf("search memory for %q: %w", query, err)
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.Kind, &h.Ref); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// LessonKey is the hook mark of a lesson injected in a session; Mark counts
// a hit for each one it adds.
func LessonKey(id int64) string { return KindLesson + ":" + strconv.FormatInt(id, 10) }

// Mark records keys as done in a session and returns the ones that were
// new. A new lesson key counts one hit for that lesson (§11.4). A key
// already marked, by this process or by a second registration of the same
// hook, is not returned, so whatever it stands for happens once.
func (s *Store) Mark(ctx context.Context, session string, taskID int64, keys []string) (map[string]bool, error) {
	added := map[string]bool{}
	err := s.write(ctx, func(tx *sql.Tx) error {
		added = map[string]bool{}
		return markTx(ctx, tx, session, taskID, keys, added)
	})
	return added, err
}

func markTx(ctx context.Context, tx *sql.Tx, session string, taskID int64, keys []string, added map[string]bool) error {
	ts := now()
	for _, k := range keys {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO hook_marks (session_id, key, task_id, created_at) VALUES (?, ?, ?, ?)`, session, k, taskID, ts)
		if err != nil {
			return fmt.Errorf("mark %s in session %q: %w", k, session, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("mark %s in session %q: %w", k, session, err)
		}
		if n == 0 {
			continue
		}
		added[k] = true
		if num, ok := strings.CutPrefix(k, KindLesson+":"); ok {
			if _, err := tx.ExecContext(ctx, `UPDATE lessons SET hits = hits + 1, last_hit_at = ? WHERE id = ?`, ts, num); err != nil {
				return fmt.Errorf("count a hit for lesson %s: %w", num, err)
			}
		}
	}
	return nil
}

// FireOnce reports whether this is the first firing of key in the session
// within window. A second registration of the same hook fires within
// milliseconds and gets false; a later legitimate firing (SessionStart after
// a compaction) gets true again. Fire marks older than a day are pruned.
func (s *Store) FireOnce(ctx context.Context, session, key string, taskID int64, window time.Duration) (bool, error) {
	first := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		cur := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `DELETE FROM hook_marks WHERE key LIKE 'fire:%' AND created_at < ?`,
			cur.Add(-24*time.Hour).Format(time.RFC3339)); err != nil {
			return fmt.Errorf("prune old hook fire marks: %w", err)
		}
		var at string
		err := tx.QueryRowContext(ctx, `SELECT created_at FROM hook_marks WHERE session_id = ? AND key = ?`, session, key).Scan(&at)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read fire mark %s: %w", key, err)
		default:
			prev, perr := time.Parse(time.RFC3339, at)
			if perr != nil {
				return fmt.Errorf("fire mark %s has a bad time %q: %w", key, at, perr)
			}
			if cur.Sub(prev) < window {
				first = false
				return nil
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hook_marks (session_id, key, task_id, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (session_id, key) DO UPDATE SET created_at = excluded.created_at`, session, key, taskID, cur.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("record fire mark %s: %w", key, err)
		}
		first = true
		return nil
	})
	return first, err
}

// Overlap statuses of WriteResult.
const (
	OverlapQueued  = "queued"  // the writing task now merges after the other
	OverlapAlready = "already" // it already did
	OverlapCycle   = "cycle"   // the other task already waits on the writer, so no order was added
)

// WriteRequest is what PostToolUse records for one file write (§9.1, §9.2).
type WriteRequest struct {
	Session string
	TaskID  int64
	Slug    string
	Agent   string
	Path    string
	// ClaimActual records an actual claim on Path for Agent.
	ClaimActual bool
	// Behind are the other open tasks holding a claim on Path, oldest first:
	// the writer is the later task, so it merges after each of them.
	Behind []Task
	// Keys are hook marks for the warnings this write would show.
	Keys []string
}

// WriteResult says which warning keys were new in the session, and what
// happened to each task in Behind (by id).
type WriteResult struct {
	NewKeys map[string]bool
	Overlap map[int64]string
}

// RecordWrite records a write in one transaction: the actual claim, the
// late-overlap merge order with a message to each other task's lead (only
// the first time), and the session's warning marks. Every part is
// idempotent, so a hook registered twice records the write once.
func (s *Store) RecordWrite(ctx context.Context, w WriteRequest, lead string) (WriteResult, error) {
	var res WriteResult
	err := s.write(ctx, func(tx *sql.Tx) error {
		res = WriteResult{NewKeys: map[string]bool{}, Overlap: map[int64]string{}}
		ts := now()
		if w.ClaimActual {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM file_claims WHERE task_id = ? AND agent = ? AND path_glob = ? AND released_at IS NULL`,
				w.TaskID, w.Agent, w.Path).Scan(&n); err != nil {
				return fmt.Errorf("read claims of task %d on %s: %w", w.TaskID, w.Path, err)
			}
			if n == 0 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO file_claims (task_id, agent, path_glob, kind, created_at) VALUES (?, ?, ?, 'actual', ?)`,
					w.TaskID, w.Agent, w.Path, ts); err != nil {
					return fmt.Errorf("record actual claim on %s for %s: %w", w.Path, w.Agent, err)
				}
			}
		}
		for _, other := range w.Behind {
			cycle, err := waitsOn(ctx, tx, other.ID, w.TaskID)
			if err != nil {
				return err
			}
			if cycle {
				res.Overlap[other.ID] = OverlapCycle
				continue
			}
			ins, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_blockers (task_id, blocker_id, created_at) VALUES (?, ?, ?)`, w.TaskID, other.ID, ts)
			if err != nil {
				return fmt.Errorf("order task %d after task %d: %w", w.TaskID, other.ID, err)
			}
			n, err := ins.RowsAffected()
			if err != nil {
				return fmt.Errorf("order task %d after task %d: %w", w.TaskID, other.ID, err)
			}
			if n == 0 {
				res.Overlap[other.ID] = OverlapAlready
				continue
			}
			res.Overlap[other.ID] = OverlapQueued
			if _, err := refreshBlocked(ctx, tx, w.TaskID); err != nil {
				return err
			}
			body := fmt.Sprintf("%s (task %s) wrote %s, which this task claims. Late overlap (spec §9.2): %s merges after this task. "+
				"Coordinate with its lead if both change the same lines.", w.Agent, w.Slug, w.Path, w.Slug)
			if _, err := InsertMessage(ctx, tx, Message{TaskID: other.ID, From: "shoal", To: lead, Kind: "info",
				Subject: truncate("late overlap: "+w.Slug+" wrote "+w.Path, 120), Body: truncate(body, 4000)}); err != nil {
				return err
			}
		}
		return markTx(ctx, tx, w.Session, w.TaskID, w.Keys, res.NewKeys)
	})
	return res, err
}

// LastResult is the newest recorded result of one test in a completed run.
type LastResult struct {
	RunID  int64
	SHA    string
	Kind   string
	Status string
}

// LatestResult returns the newest completed run's result for testID, and
// false when no completed run recorded it.
func (s *Store) LatestResult(ctx context.Context, testID string) (LastResult, bool, error) {
	var r LastResult
	err := s.db.QueryRowContext(ctx, `SELECT r.id, r.sha, r.kind, x.status FROM test_results x JOIN test_runs r ON r.id = x.run_id
		WHERE x.test_id = ? AND r.status = 'completed' ORDER BY r.id DESC, x.attempt DESC LIMIT 1`, testID).Scan(&r.RunID, &r.SHA, &r.Kind, &r.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("read the latest result of %s: %w", testID, err)
	}
	return r, true, nil
}

// Marked returns which of keys the session already marked.
func (s *Store) Marked(ctx context.Context, session string, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM hook_marks WHERE session_id = ? AND key IN (`+placeholders(len(keys))+`)`,
		append([]any{session}, anys(keys)...)...)
	if err != nil {
		return nil, fmt.Errorf("read hook marks of session %q: %w", session, err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("scan hook mark: %w", err)
		}
		out[k] = true
	}
	return out, rows.Err()
}
