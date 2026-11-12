-- Which code each QA result describes (spec §16 row 88). A shoalctl qa round
-- (the human's) keeps the commit it ran at and that commit's tree; a
-- shoalctl test --kind qa run (an agent's check) keeps its tree and its
-- verdict, pass or fail against the baseline. status compares the tree with
-- the worktree's code now, so a result on older code shows as stale. Rows
-- from before this migration keep '' and count as stale.
ALTER TABLE qa_rounds ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE qa_rounds ADD COLUMN tree TEXT NOT NULL DEFAULT '';
ALTER TABLE test_runs ADD COLUMN tree TEXT NOT NULL DEFAULT '';
ALTER TABLE test_runs ADD COLUMN verdict TEXT NOT NULL DEFAULT '';
