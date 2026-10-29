# Phase 1 progress

Plan: [todo.md](todo.md). Base: `496e05c`.

## Where I am

- Done: 1-30. Every Phase 1 command is built; the six Done-when rows have end-to-end
  tests in `internal/cli/phase1_e2e_test.go`.
- Gates (darwin host): `go vet` with GOOS=darwin|linux|windows clean; `go test -race -count=1 ./...`
  all ok; staticcheck clean (all three GOOS); `go test -c` for every package with GOOS=linux and
  GOOS=windows compiles. Workflows unchanged, so actionlint not needed.
- Self-review: git and tmux calls are argv-only (test ids reach the shell only shell-quoted through
  `rerun_command`); claim globs, `--file` and `coverage_path` are validated against traversal and
  `coverage_path` is symlink-checked before deletion; lock order is merge.lock -> task lock -> git.lock;
  every multi-row change is one transaction; nothing pushes.
- Next: Phase 2 (not started).

## Decisions

Spec ambiguities resolved with the safest option; each is also a row in docs/spec.md §16 (25-40).

- `qa` runs the gates only (contract hash, suite vs baseline, lint, build); the QA agents run in the session.
- `done` does not require a passed QA round, nor at least one behaviour for features (keeps Phase 0 flows working).
- Scope rows are judged against `done`'s own final run; flaky is never green, never a valid red/green.
- Lint/build failures and missing coverage block `merge` as `(lint)`, `(build)`, `coverage:(unavailable)`, overridable by name.
- `--accept-regression`/`--reason` pair by order; unmatched targets exit 2 after the run.
- Only rebase merges (squash exits 3); scratch baseline worktrees are shoal-owned and removed without a prompt; tag-based hotfixes are skipped by `merge`.
- After a clean rebase the task's base and baseline move to the merge target.
- Waivers and manual checks need a terminal; decisions are recorded under git's user name.
- `repro red` needs `--file`; proofs need per-test formats.
- Claim overlap for two wildcard globs is conservative (fail closed).
- `--accept-regression` needs `--task <task>`; `--task` merges only that done task (row 37).
- `merge` settles tasks left at `merging` before the queue; anything ambiguous stays put and exits 5 (row 38).
- A rebase interrupted by a signal is an interrupt (exit 130, task stays `done`), never a conflict (row 39).
- `test --kind baseline` runs in a scratch worktree at the base commit (row 40).

## Vex HOLD fixes

Vex returned HOLD on `dce413c` with four risks; each fixed test-first in its own commit.

| Risk | Fix | Test |
|------|-----|------|
| HIGH: one `--accept-regression` covered every task in a multi-task `merge` | `--accept-regression` requires `--task`; only that task is merged | `TestMergeAcceptanceIsScopedToOneTask` |
| MED: a crash between fast-forward and DB update left a task at `merging` forever | `merge` settles `merging` tasks under `merge.lock` first: merged if the target holds the head, else `done`; ambiguous -> left, exit 5 | `TestMergeSettlesTasksLeftAtMerging` |
| LOW: a rebase cancelled by Ctrl-C was classified as a conflict (needs-rebase, exit 9) | `ctx.Err()` checked first; rebase aborted, stage `done`, exit 130 | `TestRebaseCancelledIsInterruptNotConflict` |
| LOW: ignored files in the task worktree could poison a baseline and quarantine a test | `test --kind baseline` runs in the scratch-worktree helper at the base commit | `TestBaselineIgnoresWorktreeOnlyFiles` |

Remaining: `start`'s own baseline still runs in the task worktree (fresh on first start, reused on a retry after `baseline-failed`); a recovered merge cannot recover the interrupted run's gate result or accepted regressions (the report says so). Scratch baselines lack ignored dependency directories (`node_modules`, `vendor`), as merge's new-base baseline already did.
