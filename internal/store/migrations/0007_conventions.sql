-- Phase 4: conventions and lesson promotion (spec §4.5, §5.3, §11.4, §11.5).

-- A rule the human agreed to follow from now on ("use the strategy pattern
-- for payment providers"). An agent proposes it, the human agrees or
-- retires it. Retired rows keep their reason. A convention is a rule going
-- forward, not a fact about the code at a commit, so it has no true_at_sha.
-- conflict names the existing project rule it contradicts (§5.3), '' none;
-- an active row with a conflict was agreed by the human despite it.
-- lesson_id links a lesson whose promotion was turned into a proposal.
CREATE TABLE conventions (
  id             INTEGER PRIMARY KEY,
  rule           TEXT NOT NULL,
  scope_glob     TEXT NOT NULL,
  status         TEXT NOT NULL CHECK (status IN ('proposed', 'active', 'retired')),
  conflict       TEXT NOT NULL DEFAULT '',
  source_task_id INTEGER REFERENCES tasks(id),
  lesson_id      INTEGER REFERENCES lessons(id),
  proposed_at    TEXT NOT NULL,
  agreed_by      TEXT,
  agreed_at      TEXT,
  retired_by     TEXT,
  retired_at     TEXT,
  retire_reason  TEXT
);

CREATE INDEX conventions_status ON conventions (status);

-- Which tasks a lesson was shown in: promotion needs hits in 3 different
-- tasks (§11.4), which lessons.hits alone cannot tell.
CREATE TABLE lesson_hits (
  lesson_id   INTEGER NOT NULL REFERENCES lessons(id),
  task_id     INTEGER NOT NULL REFERENCES tasks(id),
  hits        INTEGER NOT NULL,
  last_hit_at TEXT NOT NULL,
  PRIMARY KEY (lesson_id, task_id)
) WITHOUT ROWID;

-- Hits counted before this migration: one per session that was shown the
-- lesson, whether its mark is still "lesson:<id>" or was turned into
-- "shown:lesson:<id>" by a compaction.
INSERT INTO lesson_hits (lesson_id, task_id, hits, last_hit_at)
SELECT lesson_id, task_id, count(DISTINCT session_id), max(created_at) FROM (
  SELECT session_id, task_id, created_at, CAST(substr(key, length('lesson:') + 1) AS INTEGER) AS lesson_id
  FROM hook_marks WHERE key LIKE 'lesson:%'
  UNION ALL
  SELECT session_id, task_id, created_at, CAST(substr(key, length('shown:lesson:') + 1) AS INTEGER)
  FROM hook_marks WHERE key LIKE 'shown:lesson:%'
)
WHERE lesson_id IN (SELECT id FROM lessons)
GROUP BY lesson_id, task_id;

CREATE TRIGGER conventions_fts_insert AFTER INSERT ON conventions BEGIN
  INSERT INTO memory_fts (kind, ref, body) VALUES ('convention', new.id,
    new.rule || ' ' || new.scope_glob || ' ' || new.conflict || ' ' || COALESCE(new.retire_reason, ''));
END;
CREATE TRIGGER conventions_fts_update AFTER UPDATE OF rule, scope_glob, conflict, retire_reason ON conventions BEGIN
  DELETE FROM memory_fts WHERE kind = 'convention' AND ref = old.id;
  INSERT INTO memory_fts (kind, ref, body) VALUES ('convention', new.id,
    new.rule || ' ' || new.scope_glob || ' ' || new.conflict || ' ' || COALESCE(new.retire_reason, ''));
END;
