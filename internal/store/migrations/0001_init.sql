CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  checksum   TEXT NOT NULL,
  applied_at TEXT NOT NULL
);

CREATE TABLE tasks (
  id              INTEGER PRIMARY KEY,
  slug            TEXT NOT NULL UNIQUE,
  lane            TEXT NOT NULL CHECK (lane IN ('feature','bugfix','hotfix')),
  title           TEXT NOT NULL,
  ask             TEXT NOT NULL,
  stage           TEXT NOT NULL DEFAULT 'new',
  base_ref        TEXT,
  base_sha        TEXT,
  branch          TEXT,
  worktree        TEXT,
  tmux_session    TEXT,
  tmux_window     TEXT,
  model           TEXT,
  baseline_run_id INTEGER REFERENCES test_runs(id),
  final_run_id    INTEGER REFERENCES test_runs(id),
  session_id      TEXT,
  last_seen_at    TEXT,
  version         INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  started_at      TEXT,
  closed_at       TEXT
);

CREATE TABLE test_runs (
  id           INTEGER PRIMARY KEY,
  task_id      INTEGER REFERENCES tasks(id),   -- task that triggered it; baselines are shared by sha
  kind         TEXT NOT NULL,                  -- baseline | final (MVP); gate | qa | repro later
  sha          TEXT NOT NULL,
  command      TEXT NOT NULL,
  command_hash TEXT NOT NULL,
  format       TEXT NOT NULL,
  fidelity     TEXT NOT NULL,                  -- per-test | suite | none
  status       TEXT NOT NULL,                  -- running | completed | error | timeout
  exit_code    INTEGER,
  attempts     INTEGER NOT NULL DEFAULT 1,
  total        INTEGER NOT NULL DEFAULT 0,
  passed       INTEGER NOT NULL DEFAULT 0,
  failed       INTEGER NOT NULL DEFAULT 0,
  skipped      INTEGER NOT NULL DEFAULT 0,
  output_path  TEXT NOT NULL,
  pid          INTEGER,
  started_at   TEXT NOT NULL,
  finished_at  TEXT
);
CREATE INDEX test_runs_reuse ON test_runs (sha, command_hash, kind, status);

CREATE TABLE test_results (
  run_id      INTEGER NOT NULL REFERENCES test_runs(id) ON DELETE CASCADE,
  test_id     TEXT NOT NULL,
  attempt     INTEGER NOT NULL DEFAULT 1,
  status      TEXT NOT NULL CHECK (status IN ('pass','fail','skip')),
  duration_ms INTEGER,
  message     TEXT,
  PRIMARY KEY (run_id, test_id, attempt)
) WITHOUT ROWID;

CREATE TABLE summaries (
  task_id               INTEGER PRIMARY KEY REFERENCES tasks(id),
  decision              TEXT NOT NULL,
  alternatives_rejected TEXT NOT NULL DEFAULT '[]',  -- JSON
  why                   TEXT NOT NULL,
  verification          TEXT NOT NULL,               -- JSON: baseline + final counts, classes, lint, build
  risks                 TEXT NOT NULL DEFAULT '[]',  -- JSON
  followups             TEXT NOT NULL DEFAULT '[]',  -- JSON
  changed_notes         TEXT NOT NULL DEFAULT '[]',  -- JSON
  commits               TEXT NOT NULL,               -- JSON array of SHAs
  files                 TEXT NOT NULL,               -- JSON array of {status, path}
  true_at_sha           TEXT NOT NULL,
  report_path           TEXT NOT NULL,
  created_at            TEXT NOT NULL
);
