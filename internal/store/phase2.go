package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PlanAgent owns the task-level expected claims that plan records (§9.1).
// It is not an agent name, so it never collides with the per-agent claims
// inside a task, and those never conflict with it.
const PlanAgent = "(plan)"

// Plan choices of the overlap prompt (§9.2).
const (
	ChoiceSerialize = 's'
	ChoiceMerge     = 'm'
	ChoiceParallel  = 'p'
)

// plannable are the stages plan accepts: the task has not started yet.
var plannable = []string{StageNew, StageReady, StageBlocked}

// unfoldable are the stages a task cannot be folded into: its work is
// finished or gone.
var unfoldable = []string{StageDone, StageMerging, StageMerged, StageFolded, StageWorktreeMissing}

// closedOwnerStages excludes tasks whose claims no longer count.
var closedOwnerStages = []string{StageMerged, StageFolded}

// OwnedClaim is an open claim together with the task holding it.
type OwnedClaim struct {
	Claim
	Owner Task
}

// OpenClaims returns every open claim of every task that is not merged or
// folded, oldest task first.
func (s *Store) OpenClaims(ctx context.Context) ([]OwnedClaim, error) {
	return openClaims(ctx, s.db, 0)
}

// querier is what *sql.DB and *sql.Tx share.
type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func openClaims(ctx context.Context, q querier, except int64) ([]OwnedClaim, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.id, c.task_id, c.agent, c.path_glob, c.kind, COALESCE(c.released_at, ''), c.created_at, c.version
		FROM file_claims c JOIN tasks t ON t.id = c.task_id
		WHERE c.released_at IS NULL AND c.task_id <> ? AND t.stage NOT IN (`+placeholders(len(closedOwnerStages))+`)
		ORDER BY c.task_id, c.id`, append([]any{except}, anys(closedOwnerStages)...)...)
	if err != nil {
		return nil, fmt.Errorf("read open claims: %w", err)
	}
	var out []OwnedClaim
	for rows.Next() {
		var c OwnedClaim
		if err := scanClaim(rows, &c.Claim); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan open claim: %w", err)
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read open claims: %w", err)
	}
	owners := map[int64]Task{}
	for i := range out {
		id := out[i].TaskID
		if _, ok := owners[id]; !ok {
			t, err := scanTask(q.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
			if err != nil {
				return nil, fmt.Errorf("read task %d of a claim: %w", id, err)
			}
			owners[id] = t
		}
		out[i].Owner = owners[id]
	}
	return out, nil
}

// Overlap is another task whose open claims overlap the globs being planned.
type Overlap struct {
	Task  Task
	Pairs [][2]string // {planned glob, the other task's glob}
	// Stale: every overlapping claim of the task is stale (§10.4), so it is
	// shown but never blocks.
	Stale bool
	// CanSerialize is false when the other task already waits on this one
	// (directly or through others), so s would deadlock both.
	CanSerialize bool
	// CanFold is false when the other task is finished (done or later) or
	// when folding would close a wait cycle (FoldCycle set).
	CanFold bool
	// FoldCycle is the slug of a task waiting on this one that the other
	// task already waits on: moving its wait over (row 49) would deadlock
	// both. Empty: none.
	FoldCycle string
}

// PlanRequest is one attempt of plan's single transaction.
type PlanRequest struct {
	Task     Task // as read before; its version guards the stage change
	Globs    []string
	Overlaps func(a, b string) bool
	// Stale reports whether a claim of owner no longer blocks planning.
	Stale     func(owner Task, c Claim) bool
	Decisions map[int64]byte // other task id -> s, m or p
	Human     string         // who decided, for the fold message
}

// PlanResult says what one attempt found and did.
type PlanResult struct {
	Overlaps  []Overlap // every overlapping task, stale ones included
	Undecided []Overlap // overlaps that still need a choice; nothing was written
	Applied   bool
	Task      Task // the task after the change
	Blockers  []int64
	Folded    *Task // the task this one was folded into
}

// PlanClaims records the task's expected claims and resolves its overlaps
// with other open tasks in one BEGIN IMMEDIATE transaction (§9.3), so two
// plans can never both win one path. With an overlap that has no decision it
// writes nothing and returns the overlaps for the human to decide. A changed
// task version returns ErrConflict.
func (s *Store) PlanClaims(ctx context.Context, req PlanRequest) (PlanResult, error) {
	var res PlanResult
	err := s.write(ctx, func(tx *sql.Tx) error {
		res = PlanResult{}
		others, err := openClaims(ctx, tx, req.Task.ID)
		if err != nil {
			return err
		}
		byTask := map[int64]*Overlap{}
		var order []int64
		for _, c := range others {
			for _, g := range req.Globs {
				if !req.Overlaps(g, c.Glob) {
					continue
				}
				o, ok := byTask[c.TaskID]
				if !ok {
					o = &Overlap{Task: c.Owner, Stale: true}
					byTask[c.TaskID] = o
					order = append(order, c.TaskID)
				}
				o.Pairs = append(o.Pairs, [2]string{g, c.Glob})
				if !req.Stale(c.Owner, c.Claim) {
					o.Stale = false
				}
			}
		}
		for _, id := range order {
			o := byTask[id]
			cycle, err := waitsOn(ctx, tx, id, req.Task.ID)
			if err != nil {
				return err
			}
			o.CanSerialize = !cycle
			if o.FoldCycle, err = foldCycle(ctx, tx, req.Task.ID, id); err != nil {
				return err
			}
			o.CanFold = !contains(unfoldable, o.Task.Stage) && o.FoldCycle == ""
			res.Overlaps = append(res.Overlaps, *o)
			d := req.Decisions[id]
			valid := (d == ChoiceSerialize && o.CanSerialize) || (d == ChoiceMerge && o.CanFold) || d == ChoiceParallel
			if !o.Stale && !valid {
				res.Undecided = append(res.Undecided, *o)
			}
		}
		if len(res.Undecided) > 0 {
			return nil
		}
		var fold *Overlap
		var blockers []int64
		for _, o := range res.Overlaps {
			if o.Stale {
				continue
			}
			switch req.Decisions[o.Task.ID] {
			case ChoiceMerge:
				if fold == nil {
					fold = &o
				}
			case ChoiceSerialize:
				blockers = append(blockers, o.Task.ID)
			}
		}
		if fold != nil {
			into, err := foldTask(ctx, tx, req, fold.Task)
			if err != nil {
				return err
			}
			res.Folded = &into
		} else if err := planTask(ctx, tx, req, blockers); err != nil {
			return err
		}
		res.Applied, res.Blockers = true, blockers
		return nil
	})
	if err != nil || !res.Applied {
		return res, err
	}
	res.Task, err = s.taskByID(ctx, req.Task.ID)
	return res, err
}

// planTask moves the task to ready or blocked and replaces its plan claims.
func planTask(ctx context.Context, tx *sql.Tx, req PlanRequest, blockers []int64) error {
	stage := StageReady
	var blockedBy any
	if len(blockers) > 0 {
		stage, blockedBy = StageBlocked, minID(blockers)
	}
	if err := guardedStage(ctx, tx, req.Task, stage, "blocked_by = ?", blockedBy); err != nil {
		return err
	}
	if err := replacePlanClaims(ctx, tx, req.Task.ID, req.Globs); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_blockers WHERE task_id = ?`, req.Task.ID); err != nil {
		return fmt.Errorf("clear blockers of task %s: %w", req.Task.Slug, err)
	}
	ts := now()
	for _, b := range blockers {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_blockers (task_id, blocker_id, created_at) VALUES (?, ?, ?)`, req.Task.ID, b, ts); err != nil {
			return fmt.Errorf("serialize task %s behind task %d: %w", req.Task.Slug, b, err)
		}
	}
	return nil
}

// foldTask closes the planned task into the earlier one (plan's m): its
// globs, user scope items and waiting tasks move over, the earlier task gets
// a message, and its ask grows when it has not started.
func foldTask(ctx context.Context, tx *sql.Tx, req PlanRequest, into Task) (Task, error) {
	me := req.Task
	if err := guardedStage(ctx, tx, me, StageFolded, "folded_into = ?, blocked_by = NULL, closed_at = ?", into.ID, now()); err != nil {
		return Task{}, err
	}
	if err := ReleaseTaskClaims(ctx, tx, me.ID); err != nil {
		return Task{}, err
	}
	held, err := claims(ctx, tx, into.ID)
	if err != nil {
		return Task{}, err
	}
	ts := now()
	for _, g := range req.Globs {
		dup := false
		for _, c := range held {
			dup = dup || (c.Agent == PlanAgent && c.Glob == g)
		}
		if dup {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO file_claims (task_id, agent, path_glob, kind, created_at) VALUES (?, ?, ?, 'expected', ?)`,
			into.ID, PlanAgent, g, ts); err != nil {
			return Task{}, fmt.Errorf("move claim %s to task %s: %w", g, into.Slug, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scope_items SET task_id = ? WHERE task_id = ? AND source = ?`, into.ID, me.ID, ScopeUser); err != nil {
		return Task{}, fmt.Errorf("move scope items to task %s: %w", into.Slug, err)
	}
	note := fmt.Sprintf("Folded in from task %s (%s):\n%s", me.Slug, me.Title, me.Ask)
	if contains(plannable, into.Stage) {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET ask = ask || ?, version = version + 1, updated_at = ? WHERE id = ?`,
			"\n\n"+note, ts, into.ID); err != nil {
			return Task{}, fmt.Errorf("extend the ask of task %s: %w", into.Slug, err)
		}
	}
	body := note + "\nClaims: " + strings.Join(req.Globs, ", ")
	if _, err := InsertMessage(ctx, tx, Message{TaskID: into.ID, From: req.Human, To: "*", Kind: "info",
		Subject: truncate("Task "+me.Slug+" was folded into this task", 120), Body: truncate(body, 4000)}); err != nil {
		return Task{}, err
	}
	// Tasks serialized behind me now wait on the task I was folded into.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_blockers (task_id, blocker_id, created_at)
		SELECT task_id, ?, created_at FROM task_blockers WHERE blocker_id = ? AND task_id <> ?`, into.ID, me.ID, into.ID); err != nil {
		return Task{}, fmt.Errorf("move blockers to task %s: %w", into.Slug, err)
	}
	waiting, err := blockedOn(ctx, tx, me.ID)
	if err != nil {
		return Task{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_blockers WHERE blocker_id = ? OR task_id = ?`, me.ID, me.ID); err != nil {
		return Task{}, fmt.Errorf("clear blockers of task %s: %w", me.Slug, err)
	}
	for _, id := range waiting {
		if _, err := refreshBlocked(ctx, tx, id); err != nil {
			return Task{}, err
		}
	}
	return into, nil
}

// guardedStage changes the planned task's stage under its version and the
// plannable stages (§10.2); zero rows is ErrConflict.
func guardedStage(ctx context.Context, tx *sql.Tx, t Task, stage, set string, args ...any) error {
	q := `UPDATE tasks SET stage = ?, ` + set + `, version = version + 1, updated_at = ?
		WHERE id = ? AND version = ? AND stage IN (` + placeholders(len(plannable)) + `)`
	all := append(append(append([]any{stage}, args...), now(), t.ID, t.Version), anys(plannable)...)
	res, err := tx.ExecContext(ctx, q, all...)
	if err != nil {
		return fmt.Errorf("plan task %s: %w", t.Slug, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("plan task %s: %w", t.Slug, err)
	} else if n != 1 {
		return fmt.Errorf("%w: task %s changed while plan ran (read version %d)", ErrConflict, t.Slug, t.Version)
	}
	return nil
}

func replacePlanClaims(ctx context.Context, tx *sql.Tx, taskID int64, globs []string) error {
	ts := now()
	if _, err := tx.ExecContext(ctx, `UPDATE file_claims SET released_at = ?, version = version + 1
		WHERE task_id = ? AND agent = ? AND released_at IS NULL`, ts, taskID, PlanAgent); err != nil {
		return fmt.Errorf("release old plan claims of task %d: %w", taskID, err)
	}
	for _, g := range globs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO file_claims (task_id, agent, path_glob, kind, created_at) VALUES (?, ?, ?, 'expected', ?)`,
			taskID, PlanAgent, g, ts); err != nil {
			return fmt.Errorf("claim %s for task %d: %w", g, taskID, err)
		}
	}
	return nil
}

// waitsOn reports whether task from waits on task target, directly or
// through a chain of serialize choices.
func waitsOn(ctx context.Context, tx *sql.Tx, from, target int64) (bool, error) {
	seen := map[int64]bool{from: true}
	queue := []int64{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		next, err := blockersOf(ctx, tx, cur)
		if err != nil {
			return false, err
		}
		for _, id := range next {
			if id == target {
				return true, nil
			}
			if !seen[id] {
				seen[id] = true
				queue = append(queue, id)
			}
		}
	}
	return false, nil
}

// foldCycle returns the slug of a task waiting on me that into already
// waits on, other than through me (whose own waits the fold drops). Folding
// me into into would make that task wait on into: a cycle. Empty: safe.
func foldCycle(ctx context.Context, tx *sql.Tx, me, into int64) (string, error) {
	waiting, err := blockedOn(ctx, tx, me)
	if err != nil {
		return "", err
	}
	targets := map[int64]bool{}
	for _, w := range waiting {
		if w != into {
			targets[w] = true
		}
	}
	if len(targets) == 0 {
		return "", nil
	}
	seen := map[int64]bool{into: true, me: true}
	queue := []int64{into}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		next, err := blockersOf(ctx, tx, cur)
		if err != nil {
			return "", err
		}
		for _, id := range next {
			if targets[id] {
				var slug string
				if err := tx.QueryRowContext(ctx, `SELECT slug FROM tasks WHERE id = ?`, id).Scan(&slug); err != nil {
					return "", fmt.Errorf("read slug of task %d: %w", id, err)
				}
				return slug, nil
			}
			if !seen[id] {
				seen[id] = true
				queue = append(queue, id)
			}
		}
	}
	return "", nil
}

func blockersOf(ctx context.Context, tx *sql.Tx, id int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT blocker_id FROM task_blockers WHERE task_id = ? ORDER BY blocker_id`, id)
	if err != nil {
		return nil, fmt.Errorf("read blockers of task %d: %w", id, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("scan blocker: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func blockedOn(ctx context.Context, tx *sql.Tx, blocker int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT task_id FROM task_blockers WHERE blocker_id = ? ORDER BY task_id`, blocker)
	if err != nil {
		return nil, fmt.Errorf("read tasks waiting on task %d: %w", blocker, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan waiting task: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// refreshBlocked points blocked_by at the oldest remaining blocker, or moves
// a blocked task with none left to ready. It reports whether it unblocked.
func refreshBlocked(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	var oldest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(blocker_id) FROM task_blockers WHERE task_id = ?`, id).Scan(&oldest); err != nil {
		return false, fmt.Errorf("read blockers of task %d: %w", id, err)
	}
	ts := now()
	if oldest.Valid {
		_, err := tx.ExecContext(ctx, `UPDATE tasks SET blocked_by = ?, updated_at = ? WHERE id = ?`, oldest.Int64, ts, id)
		if err != nil {
			return false, fmt.Errorf("update blocker of task %d: %w", id, err)
		}
		return false, nil
	}
	// A started task waits only for its merge turn (a late overlap): its
	// stage stays, its blocked_by clears.
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET blocked_by = NULL, updated_at = ? WHERE id = ? AND stage <> ? AND blocked_by IS NOT NULL`,
		ts, id, StageBlocked); err != nil {
		return false, fmt.Errorf("clear the merge order of task %d: %w", id, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET stage = ?, blocked_by = NULL, version = version + 1, updated_at = ?
		WHERE id = ? AND stage = ?`, StageReady, ts, id, StageBlocked)
	if err != nil {
		return false, fmt.Errorf("unblock task %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UnblockAfterMerge drops every serialize choice behind the merged task and
// returns the tasks that became ready, inside the merge's transaction. The
// merged task's own waits go too: merge --task can land it ahead of them.
func UnblockAfterMerge(ctx context.Context, tx *sql.Tx, merged int64) ([]int64, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_blockers WHERE task_id = ?`, merged); err != nil {
		return nil, fmt.Errorf("drop the waits of merged task %d: %w", merged, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET blocked_by = NULL WHERE id = ?`, merged); err != nil {
		return nil, fmt.Errorf("clear blocked_by of merged task %d: %w", merged, err)
	}
	waiting, err := blockedOn(ctx, tx, merged)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_blockers WHERE blocker_id = ?`, merged); err != nil {
		return nil, fmt.Errorf("release tasks waiting on task %d: %w", merged, err)
	}
	var ready []int64
	for _, id := range waiting {
		ok, err := refreshBlocked(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			ready = append(ready, id)
		}
	}
	return ready, nil
}

// MarkNeedsRebase moves the tasks among ids still at in-progress or qa to
// needs-rebase and returns the ones it moved (§9.4 step 5).
func MarkNeedsRebase(ctx context.Context, tx *sql.Tx, ids []int64) ([]int64, error) {
	var moved []int64
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE tasks SET stage = ?, version = version + 1, updated_at = ?
			WHERE id = ? AND stage IN (?, ?)`, StageNeedsRebase, now(), id, StageInProgress, StageQA)
		if err != nil {
			return nil, fmt.Errorf("mark task %d needs-rebase: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n == 1 {
			moved = append(moved, id)
		}
	}
	return moved, nil
}

// Queue states (§4.5 merge_queue).
const (
	QueueWaiting  = "waiting"
	QueueRebasing = "rebasing"
	QueueTesting  = "testing"
	QueueMerged   = "merged"
	QueueFailed   = "failed"
)

// QueueEntry is one row of merge_queue.
type QueueEntry struct {
	TaskID   int64
	Position int
	State    string
}

// Enqueue puts the task at the end of the merge queue as waiting, inside the
// caller's transaction (done).
func Enqueue(ctx context.Context, tx *sql.Tx, taskID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO merge_queue (task_id, position, state, updated_at)
		VALUES (?, (SELECT COALESCE(MAX(position), 0) + 1 FROM merge_queue), ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET position = excluded.position, state = excluded.state, updated_at = excluded.updated_at`,
		taskID, QueueWaiting, now())
	if err != nil {
		return fmt.Errorf("enqueue task %d: %w", taskID, err)
	}
	return nil
}

// SetQueueState records where the task's merge stands, keeping its
// position; a task with no row joins the end of the queue.
func SetQueueState(ctx context.Context, tx *sql.Tx, taskID int64, state string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO merge_queue (task_id, position, state, updated_at)
		VALUES (?, (SELECT COALESCE(MAX(position), 0) + 1 FROM merge_queue), ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET state = excluded.state, updated_at = excluded.updated_at`,
		taskID, state, now())
	if err != nil {
		return fmt.Errorf("set merge queue state of task %d to %s: %w", taskID, state, err)
	}
	return nil
}

// SetQueue is SetQueueState in its own transaction.
func (s *Store) SetQueue(ctx context.Context, taskID int64, state string) error {
	return s.write(ctx, func(tx *sql.Tx) error { return SetQueueState(ctx, tx, taskID, state) })
}

// Queue returns every merge_queue row by position.
func (s *Store) Queue(ctx context.Context) ([]QueueEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, position, state FROM merge_queue ORDER BY position, task_id`)
	if err != nil {
		return nil, fmt.Errorf("read merge queue: %w", err)
	}
	defer rows.Close()
	var out []QueueEntry
	for rows.Next() {
		var e QueueEntry
		if err := rows.Scan(&e.TaskID, &e.Position, &e.State); err != nil {
			return nil, fmt.Errorf("scan merge queue: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// QueuedTasks returns the done tasks in merge order: queue position first,
// then (for a done task with no queue row) the time it was done.
func (s *Store) QueuedTasks(ctx context.Context) ([]Task, error) {
	pos := `(SELECT q.position FROM merge_queue q WHERE q.task_id = tasks.id)`
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE stage = ? ORDER BY `+pos+` IS NULL, `+pos+`, closed_at, id`, StageDone)
	if err != nil {
		return nil, fmt.Errorf("list queued tasks: %w", err)
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

// SetAttention records what the task's session window waits for ("" clears
// it). Like a heartbeat it leaves version alone. It reports whether the
// value changed.
func (s *Store) SetAttention(ctx context.Context, id int64, reason string) (bool, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		var at any
		if reason != "" {
			at = now()
		}
		res, err := tx.ExecContext(ctx, `UPDATE tasks SET attention = ?, attention_at = ? WHERE id = ? AND COALESCE(attention, '') <> ?`,
			nullable(reason), at, id, reason)
		if err != nil {
			return fmt.Errorf("set attention of task %d: %w", id, err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return n == 1, err
}

// ReleaseStaleClaim releases one claim when it is still stale, re-checked
// inside the transaction against the current task and claim rows and
// guarded by the claim's version (§10.2). It reports whether it released.
func (s *Store) ReleaseStaleClaim(ctx context.Context, c Claim, stale func(owner Task, c Claim) bool) (bool, error) {
	released := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		released = false
		var cur Claim
		err := scanClaim(tx.QueryRowContext(ctx, `SELECT `+claimColumns+` FROM file_claims WHERE id = ?`, c.ID), &cur)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read claim %d: %w", c.ID, err)
		}
		owner, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, cur.TaskID))
		if err != nil {
			return fmt.Errorf("read task %d of claim %d: %w", cur.TaskID, c.ID, err)
		}
		if cur.ReleasedAt != "" || !stale(owner, cur) {
			return nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE file_claims SET released_at = ?, version = version + 1 WHERE id = ? AND version = ? AND released_at IS NULL`,
			now(), cur.ID, cur.Version)
		if err != nil {
			return fmt.Errorf("release claim %d: %w", c.ID, err)
		}
		n, err := res.RowsAffected()
		released = n == 1
		return err
	})
	return released, err
}

// ClearWindow forgets a tmux window that is gone, unless another command
// recorded a different one meanwhile. Version is left alone.
func (s *Store) ClearWindow(ctx context.Context, id int64, window string) (bool, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE tasks SET tmux_window = NULL WHERE id = ? AND tmux_window = ?`, id, window)
		if err != nil {
			return fmt.Errorf("clear tmux window of task %d: %w", id, err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return n == 1, err
}

// Blockers returns the tasks the task waits on, oldest first.
func (s *Store) Blockers(ctx context.Context, id int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT blocker_id FROM task_blockers WHERE task_id = ? ORDER BY blocker_id`, id)
	if err != nil {
		return nil, fmt.Errorf("read blockers of task %d: %w", id, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("scan blocker: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// IntegrityProblems runs PRAGMA integrity_check and foreign_key_check.
func (s *Store) IntegrityProblems(ctx context.Context) ([]string, error) {
	var out []string
	rows, err := s.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return nil, fmt.Errorf("integrity_check: %w", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return nil, fmt.Errorf("integrity_check: %w", err)
		}
		if line != "ok" {
			out = append(out, "integrity_check: "+line)
		}
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, fmt.Errorf("foreign_key_check: %w", err)
		}
		out = append(out, fmt.Sprintf("foreign_key_check: %s row %d points at a missing %s row", table, rowid.Int64, parent))
	}
	return out, rows.Err()
}

// MigrationProblems compares the applied migrations with the binary's.
func (s *Store) MigrationProblems(ctx context.Context) ([]string, error) {
	migs, err := Migrations()
	if err != nil {
		return nil, err
	}
	want := map[int]Migration{}
	for _, m := range migs {
		want[m.Version] = m
	}
	rows, err := s.db.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	var out []string
	seen := map[int]bool{}
	for rows.Next() {
		var v int
		var name, sum string
		if err := rows.Scan(&v, &name, &sum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		seen[v] = true
		m, ok := want[v]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("migration %d (%s) is applied but unknown to this binary", v, name))
		case checksum(m.SQL) != sum:
			out = append(out, fmt.Sprintf("migration %04d_%s was applied with checksum %s, the binary's is %s", v, name, sum[:12], checksum(m.SQL)[:12]))
		}
	}
	for _, m := range migs {
		if !seen[m.Version] {
			out = append(out, fmt.Sprintf("migration %04d_%s is not recorded in schema_migrations", m.Version, m.Name))
		}
	}
	return out, rows.Err()
}

// RunningRunsAll returns every run still marked running, of any task.
func (s *Store) RunningRunsAll(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM test_runs WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list running runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func minID(ids []int64) int64 {
	m := ids[0]
	for _, id := range ids[1:] {
		if id < m {
			m = id
		}
	}
	return m
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}
