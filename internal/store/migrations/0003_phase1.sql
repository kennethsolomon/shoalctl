-- Phase 1: agent team, QA loop, zero-regression merge gate (spec §15).

ALTER TABLE tasks ADD COLUMN qa_round INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN merged_sha TEXT;
ALTER TABLE tasks ADD COLUMN parent_task_id INTEGER REFERENCES tasks(id);
ALTER TABLE tasks ADD COLUMN blocked_by INTEGER REFERENCES tasks(id);
ALTER TABLE tasks ADD COLUMN gate_run_id INTEGER REFERENCES test_runs(id);
ALTER TABLE tasks ADD COLUMN large_diff_reason TEXT;

CREATE TABLE file_claims (
  id          INTEGER PRIMARY KEY,
  task_id     INTEGER NOT NULL REFERENCES tasks(id),
  agent       TEXT NOT NULL,
  path_glob   TEXT NOT NULL,
  kind        TEXT NOT NULL CHECK (kind IN ('expected','actual')),
  expires_at  TEXT,
  released_at TEXT,
  version     INTEGER NOT NULL DEFAULT 1,
  created_at  TEXT NOT NULL
);
CREATE INDEX file_claims_task ON file_claims (task_id, released_at);

CREATE TABLE messages (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  from_agent TEXT NOT NULL,
  to_agent   TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('question','answer','handoff','contract-change','blocker','qa-failure','info')),
  subject    TEXT NOT NULL CHECK (length(subject) <= 120),
  body       TEXT NOT NULL CHECK (length(body) <= 4000),
  reply_to   INTEGER REFERENCES messages(id),
  created_at TEXT NOT NULL,
  CHECK (kind <> 'answer' OR reply_to IS NOT NULL)
);
CREATE INDEX messages_task_to ON messages (task_id, to_agent, id);

CREATE TABLE message_reads (
  message_id INTEGER NOT NULL REFERENCES messages(id),
  agent      TEXT NOT NULL,
  read_at    TEXT NOT NULL,
  PRIMARY KEY (message_id, agent)
) WITHOUT ROWID;

CREATE TABLE contracts (
  task_id   INTEGER NOT NULL REFERENCES tasks(id),
  version   INTEGER NOT NULL,
  sha256    TEXT NOT NULL,
  frozen_at TEXT NOT NULL,
  frozen_by TEXT NOT NULL,
  PRIMARY KEY (task_id, version)
) WITHOUT ROWID;

CREATE TABLE test_quarantine (
  id              INTEGER PRIMARY KEY,
  test_id         TEXT NOT NULL,
  detected_run_id INTEGER NOT NULL REFERENCES test_runs(id),
  reason          TEXT NOT NULL,
  quarantined_at  TEXT NOT NULL,
  released_at     TEXT
);
CREATE UNIQUE INDEX test_quarantine_open ON test_quarantine (test_id) WHERE released_at IS NULL;

CREATE TABLE coverage_files (
  run_id        INTEGER NOT NULL REFERENCES test_runs(id) ON DELETE CASCADE,
  path          TEXT NOT NULL,
  lines_total   INTEGER NOT NULL,
  lines_covered INTEGER NOT NULL,
  uncovered     TEXT NOT NULL DEFAULT '',   -- uncovered line ranges, e.g. "4-9,12"
  PRIMARY KEY (run_id, path)
) WITHOUT ROWID;

CREATE TABLE regression_proofs (
  task_id                INTEGER NOT NULL REFERENCES tasks(id),
  test_id                TEXT NOT NULL,
  test_file              TEXT NOT NULL DEFAULT '',
  red_status             TEXT NOT NULL CHECK (red_status IN ('fail','not-reproducible')),
  red_run_id             INTEGER NOT NULL REFERENCES test_runs(id),
  red_sha                TEXT NOT NULL,
  green_run_id           INTEGER REFERENCES test_runs(id),
  green_sha              TEXT,
  test_changed_after_red INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (task_id, test_id)
) WITHOUT ROWID;

CREATE TABLE scope_items (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  source     TEXT NOT NULL CHECK (source IN ('user','behaviour')),
  text       TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE scope_cases (
  id            INTEGER PRIMARY KEY,
  scope_item_id INTEGER NOT NULL REFERENCES scope_items(id),
  category      TEXT NOT NULL,
  description   TEXT NOT NULL,
  "check"       TEXT NOT NULL CHECK ("check" IN ('automated','manual')),
  test_id       TEXT,
  manual_step   TEXT,
  status        TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','pass','fail','waived')),
  waive_reason  TEXT,
  note          TEXT,
  decided_by    TEXT,
  decided_at    TEXT,
  version       INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL,
  CHECK (("check" = 'automated' AND test_id IS NOT NULL) OR ("check" = 'manual' AND manual_step IS NOT NULL))
);

CREATE TABLE regression_overrides (
  id          INTEGER PRIMARY KEY,
  task_id     INTEGER NOT NULL REFERENCES tasks(id),
  gate_run_id INTEGER NOT NULL REFERENCES test_runs(id),
  target      TEXT NOT NULL,
  reason      TEXT NOT NULL,
  accepted_by TEXT NOT NULL,
  accepted_at TEXT NOT NULL
);

CREATE TABLE qa_rounds (
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  round      INTEGER NOT NULL,
  run_id     INTEGER REFERENCES test_runs(id),
  passed     INTEGER NOT NULL,
  failures   TEXT NOT NULL DEFAULT '[]',   -- JSON array of strings
  created_at TEXT NOT NULL,
  PRIMARY KEY (task_id, round)
) WITHOUT ROWID;
