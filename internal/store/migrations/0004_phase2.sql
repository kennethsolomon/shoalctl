-- Phase 2: cross-task claims, merge queue, full doctor, attention watcher (spec §15).

ALTER TABLE tasks ADD COLUMN prev_stage TEXT;                          -- stage before a transient or worktree-missing stage
ALTER TABLE tasks ADD COLUMN folded_into INTEGER REFERENCES tasks(id); -- plan's "m": the task this one was folded into
ALTER TABLE tasks ADD COLUMN attention TEXT;                           -- what the session window waits for, NULL when nothing
ALTER TABLE tasks ADD COLUMN attention_at TEXT;

-- One row per "s" (serialize) choice; tasks.blocked_by mirrors the oldest open blocker.
CREATE TABLE task_blockers (
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  blocker_id INTEGER NOT NULL REFERENCES tasks(id),
  created_at TEXT NOT NULL,
  PRIMARY KEY (task_id, blocker_id),
  CHECK (task_id <> blocker_id)
) WITHOUT ROWID;
CREATE INDEX task_blockers_blocker ON task_blockers (blocker_id);

-- The serial merge queue (§9.4). The target branch is derived from the task
-- (lane and base ref), so one table holds every target's queue.
CREATE TABLE merge_queue (
  task_id    INTEGER PRIMARY KEY REFERENCES tasks(id),
  position   INTEGER NOT NULL,
  state      TEXT NOT NULL CHECK (state IN ('waiting','rebasing','testing','merged','failed')),
  updated_at TEXT NOT NULL
);

INSERT INTO merge_queue (task_id, position, state, updated_at)
SELECT id, ROW_NUMBER() OVER (ORDER BY closed_at, id), 'waiting', updated_at
FROM tasks WHERE stage IN ('done', 'merging');

CREATE INDEX file_claims_open ON file_claims (released_at, task_id);
