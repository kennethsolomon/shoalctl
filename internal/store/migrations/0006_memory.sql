-- Phase 3: memory (the WHY behind commits) and hook bookkeeping (spec §4.5, §4.6, §11).
-- Every record stores true_at_sha: what was true at that commit. git stays the
-- source of WHAT changed; these rows hold only the reasons.

-- One row per finished bugfix or hotfix task, written by done; merge moves
-- fix_sha to the commit that landed on the target branch.
CREATE TABLE bugs (
  id               INTEGER PRIMARY KEY,
  task_id          INTEGER NOT NULL UNIQUE REFERENCES tasks(id),
  symptom          TEXT NOT NULL,
  root_cause       TEXT NOT NULL,
  fix_sha          TEXT NOT NULL,
  files            TEXT NOT NULL DEFAULT '[]',  -- JSON array of repo paths
  regression_tests TEXT NOT NULL DEFAULT '[]',  -- JSON array of {"test", "file"}
  true_at_sha      TEXT NOT NULL,
  stale            INTEGER NOT NULL DEFAULT 0,
  stale_reason     TEXT,
  created_at       TEXT NOT NULL
);

CREATE TABLE decisions (
  id            INTEGER PRIMARY KEY,
  task_id       INTEGER NOT NULL REFERENCES tasks(id),
  topic         TEXT NOT NULL,
  choice        TEXT NOT NULL,
  rationale     TEXT NOT NULL,
  files         TEXT NOT NULL DEFAULT '[]',     -- JSON array of repo paths
  true_at_sha   TEXT NOT NULL,
  stale         INTEGER NOT NULL DEFAULT 0,
  stale_reason  TEXT,
  superseded_by INTEGER REFERENCES decisions(id),
  created_at    TEXT NOT NULL
);

-- path_glob '' means the lesson applies to the whole stack (brief only).
CREATE TABLE lessons (
  id           INTEGER PRIMARY KEY,
  task_id      INTEGER NOT NULL REFERENCES tasks(id),
  qa_round     INTEGER,
  pattern      TEXT NOT NULL,
  fix          TEXT NOT NULL,
  stack        TEXT NOT NULL DEFAULT '',
  path_glob    TEXT NOT NULL DEFAULT '',
  hits         INTEGER NOT NULL DEFAULT 0,
  last_hit_at  TEXT,
  promoted     INTEGER NOT NULL DEFAULT 0,
  true_at_sha  TEXT NOT NULL,
  stale        INTEGER NOT NULL DEFAULT 0,
  stale_reason TEXT,
  created_at   TEXT NOT NULL
);

CREATE TABLE qa (
  id           INTEGER PRIMARY KEY,
  task_id      INTEGER NOT NULL REFERENCES tasks(id),
  question     TEXT NOT NULL,
  answer       TEXT NOT NULL,
  files        TEXT NOT NULL DEFAULT '[]',
  true_at_sha  TEXT NOT NULL,
  stale        INTEGER NOT NULL DEFAULT 0,
  stale_reason TEXT,
  created_at   TEXT NOT NULL
);

-- What a hook already did in a session: a record injected ("lesson:3"), a
-- warning shown ("outside:shoal-backend:src/a.go"), or a SessionStart fired
-- ("fire:session-start:startup"). Makes injection once per session and makes
-- a hook registered twice act once.
CREATE TABLE hook_marks (
  session_id TEXT NOT NULL,
  key        TEXT NOT NULL,
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  created_at TEXT NOT NULL,
  PRIMARY KEY (session_id, key)
) WITHOUT ROWID;

-- One full-text index over every kind of record. kind is bug, decision,
-- lesson, qa or summary; ref is the record id (task_id for a summary).
CREATE VIRTUAL TABLE memory_fts USING fts5(kind UNINDEXED, ref UNINDEXED, body, tokenize = 'porter unicode61');

CREATE TRIGGER bugs_fts_insert AFTER INSERT ON bugs BEGIN
  INSERT INTO memory_fts (kind, ref, body) VALUES ('bug', new.id, new.symptom || ' ' || new.root_cause || ' ' || new.files || ' ' || new.regression_tests);
END;
CREATE TRIGGER bugs_fts_update AFTER UPDATE OF symptom, root_cause, files, regression_tests ON bugs BEGIN
  DELETE FROM memory_fts WHERE kind = 'bug' AND ref = old.id;
  INSERT INTO memory_fts (kind, ref, body) VALUES ('bug', new.id, new.symptom || ' ' || new.root_cause || ' ' || new.files || ' ' || new.regression_tests);
END;

CREATE TRIGGER decisions_fts_insert AFTER INSERT ON decisions BEGIN
  INSERT INTO memory_fts (kind, ref, body) VALUES ('decision', new.id, new.topic || ' ' || new.choice || ' ' || new.rationale || ' ' || new.files);
END;

CREATE TRIGGER lessons_fts_insert AFTER INSERT ON lessons BEGIN
  INSERT INTO memory_fts (kind, ref, body) VALUES ('lesson', new.id, new.pattern || ' ' || new.fix || ' ' || new.stack || ' ' || new.path_glob);
END;

CREATE TRIGGER qa_fts_insert AFTER INSERT ON qa BEGIN
  INSERT INTO memory_fts (kind, ref, body) VALUES ('qa', new.id, new.question || ' ' || new.answer || ' ' || new.files);
END;

-- summaries are written with INSERT OR REPLACE, which fires no delete
-- trigger, so the insert trigger drops the old entry itself.
CREATE TRIGGER summaries_fts_insert AFTER INSERT ON summaries BEGIN
  DELETE FROM memory_fts WHERE kind = 'summary' AND ref = new.task_id;
  INSERT INTO memory_fts (kind, ref, body) VALUES ('summary', new.task_id, new.decision || ' ' || new.why || ' ' || new.alternatives_rejected || ' ' || new.changed_notes);
END;

INSERT INTO memory_fts (kind, ref, body)
SELECT 'summary', task_id, decision || ' ' || why || ' ' || alternatives_rejected || ' ' || changed_notes FROM summaries;
