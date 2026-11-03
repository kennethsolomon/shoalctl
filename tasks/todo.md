# Phase 2: cross-task claims, merge queue, full doctor, attention watcher

Acceptance bar: spec §15 Phase 2 row. Each "Done when" item has an end-to-end
test in `internal/cli/phase2_e2e_test.go`. Phase 1 plan: done at `6d33f87`
(see git history of this file).

Conventions kept: every state change in one `BEGIN IMMEDIATE` write; `tasks.version`
and `file_claims.version` guard updates (§10.2); lock order `merge.lock` ->
`task-<id>.lock` -> `git.lock`; argv-only git/tmux; no transaction across a prompt,
a git, tmux or test step; human-only actions need a terminal.

## Schema

- [x] 1. Migration `0004_phase2.sql` (store test: user_version 4, every new column/table).
  - `tasks` + `prev_stage` (stage before a transient or `worktree-missing` stage),
    `folded_into` (FK tasks), `attention`, `attention_at`.
  - `task_blockers(task_id, blocker_id, created_at)` PK both: every `s` choice; `tasks.blocked_by`
    mirrors the oldest open blocker.
  - `merge_queue(task_id PK, position, state waiting|rebasing|testing|merged|failed, updated_at)`;
    backfilled from tasks at `done`/`merging` in `closed_at, id` order.
  - Index `file_claims_open (released_at, task_id)` for cross-task reads.

## Pure logic

- [x] 2. `globx.Overlaps`: exact for wildcard vs wildcard (segment product automaton,
  `**` as any number of segments, `*`/`?`/classes per segment). Literal vs literal also
  overlaps when one is a directory prefix of the other (consistent with `under`). Tests.
- [x] 3. `internal/attention.Detect(screen)`: a `❯ N.` numbered menu with a second option
  near the bottom of the screen, or a y/n prompt on the last line. Fixtures in testdata.

## Store

- [x] 4. `PlanClaims` (one tx: overlaps vs other open tasks' claims, stale callback, decisions
  `s`/`m`/`p`, version-guarded stage change to `ready`/`blocked`/`folded`); `UnblockAfterMerge`;
  queue (`Enqueue`, `SetQueueState`, `QueuedTasks`); `SetAttention`; `ReleaseStaleClaim`
  (claim version guard + staleness recheck); `OpenClaims`. Store tests incl. claim conflicts.

## Commands

- [x] 5. `plan <task> [--todo <file>] <glob>...` (§9.2): stages new/ready/blocked; prompt per
  overlapping task (s/m/p/?), terminal only (no terminal + overlap = exit 5); stale claims
  shown, never block; `s` refused when it would form a cycle; `m` only into an unfinished task.
  `start` refuses `blocked` (exit 5), accepts `ready`; copies the plan's todo.md into `.shoal/`.
- [x] 6. `done` enqueues; `merge` walks `merge_queue` by position, grouped per target branch;
  queue states follow the run; a merge releases claims, unblocks tasks serialized behind it and
  marks in-progress/qa tasks of the same target whose claims overlap the merged files `needs-rebase`.
- [x] 7. `qa` on `needs-rebase`: rebase onto the target tip (conflict: aborted, stays, exit 9;
  rebase already in progress: exit 5), new base + scratch baseline, then the round.
- [x] 8. Merge output: accepted rows printed with class and reason before `merged`; partial
  acceptance lists accepted, still-blocking, and "back at qa".
- [x] 9. Full `doctor [--fix]` (§10.4): hooks (Phase 0 check kept), integrity + FK, migration
  checksums, merge.lock-guarded queue rows at `rebasing` (asks) + `settleMerging` reuse,
  `starting` rollback via `prev_stage` with `reapAbandoned` reuse, orphaned `running` runs,
  worktree missing (mark/restore), orphan worktree dirs (report), `git worktree prune`,
  gone tmux windows, hook entries in worktrees, stale claims. Exit 11 while anything remains.
- [x] 10. `watch <task>` (internal) + `start` spawns it detached: polls `tmux capture-pane`
  every 2 s, sets/clears `attention`, exits when the window or working stage ends, or on a
  signal; one watcher per task (`watch-<id>.lock`). `status` shows NOTE (attention, blocked
  by, queue position, claims) and JSON fields.

## Acceptance (end-to-end)

- [x] 11. `TestPhase2BugABugB` (Done-when 1)
- [x] 12. `TestPhase2ConcurrentPlansOneWinner` (2, goroutines and processes, -race)
- [x] 13. `TestPhase2DoctorRecoversKilledStartAndStaleClaim` (3)
- [x] 14. `TestPhase2AttentionFlag` (real tmux, fake claude showing a dialog, real watcher process)
- [x] 15. `TestPhase2MergeOutputAcceptance` (full and partial acceptance messages)

## Docs and gates

- [x] 16. README (Phase 2 built, plan/doctor/status, `[tests].setup = "true"` one-liner),
  spec §4.4, §15 Phase 2 row (watcher), §16 rows 46+, progress.md.
- [x] 17. Gates from a fresh clone: vet x3 GOOS, staticcheck x3 GOOS, `go test -race -count=1`,
  `go test -c` linux/windows per package, `git ls-files -oi --exclude-standard`.

## Verification

```sh
go vet ./... ; GOOS=linux go vet ./... ; GOOS=windows go vet ./...
staticcheck ./... (x3 GOOS)
go test -race -count=1 ./...
for p in $(go list ./...); do GOOS=windows go test -c -o /dev/null $p; done   # and linux
git ls-files -oi --exclude-standard   # empty
```

## Risks

- Overlap prompt reads a line (key + Enter), like `scope review`, not a raw keypress.
- The attention detector is a heuristic over the visible pane; fixtures pin current Claude Code dialogs.
