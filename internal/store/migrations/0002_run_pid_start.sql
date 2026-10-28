-- pid_start identifies the process behind test_runs.pid (its start time), so
-- a reused pid is never mistaken for a run's suite.
ALTER TABLE test_runs ADD COLUMN pid_start TEXT;
