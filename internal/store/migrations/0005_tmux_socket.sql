-- The tmux server socket a task's window lives on, recorded at start, so
-- liveness checks ask that server and not whatever default the caller has.
-- NULL for tasks started before this migration.
ALTER TABLE tasks ADD COLUMN tmux_socket TEXT;
