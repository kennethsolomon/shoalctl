-- shoalctl abandon (spec §16 row 84): the human gives up on a task before it
-- merged. The task moves to stage abandoned (stage has no CHECK constraint)
-- and keeps who gave up on it, when and why; closed_at is set too when the
-- task had none, so status ages it out like any closed task.
ALTER TABLE tasks ADD COLUMN abandoned_by TEXT;
ALTER TABLE tasks ADD COLUMN abandoned_at TEXT;
ALTER TABLE tasks ADD COLUMN abandon_reason TEXT;
