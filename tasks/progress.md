# Phase 2 progress

Plan: [todo.md](todo.md). Base: `6d33f87`.

## Where I am

- Done: 1-17. `plan`, the merge queue, the full `doctor`, the attention watcher and the merge-output
  fix are built; the three Done-when rows plus the watcher and merge output have end-to-end tests in
  `internal/cli/phase2_e2e_test.go`.
- Gates run from a fresh clone at the final HEAD (see the report): vet and staticcheck x3 GOOS,
  `go test -race -count=1 ./...`, `go test -c` per package for linux and windows, no ignored source file.
- Vex HOLD round 1 on `783d968`: seven findings fixed test-first, one commit each (table below); the rest are open risks.
- Next: Phase 3 (not started).

## Decisions

Spec ambiguities resolved with the safest option; each is a row in docs/spec.md §16 (46-57).

- `plan` runs before `start` (new/ready/blocked); planning stays optional, `start` still runs from `new`.
- Only a human at a terminal resolves an overlap; otherwise nothing is written, exit 5. Key + Enter, like `scope review`.
- Plan claims belong to `(plan)` and never conflict with per-agent claims of the same task.
- `s` against several tasks: one `task_blockers` row each; ready when all merged; `s` refused when it would deadlock.
- `m` folds claims, user scope items and the ask into an unfinished task, messages its agents, closes the task as `folded`.
- Stale = started task, window gone, last sign of life older than `stale_after`; computed on read (`expires_at` unused).
- Wildcard overlap is now exact (fixes the Phase 1 open risk); literal globs cover their subtree against literals too.
- Queue target derived per task, not stored; `merge` keeps Phase 1's stop-at-first-error.
- After a merge only in-progress/qa tasks of the same target are marked `needs-rebase`; `qa` rebases them first.
- `doctor`: rollback via `prev_stage`; settle via `settleMerging`; abort a merge's rebase only after a human `y`;
  no-pid running runs marked error (the suite gate means they never ran); prune only shoal's gone worktrees;
  `worktree-missing` stays a problem until the worktree is back.
- Watcher: detached `shoalctl watch`, 2 s ticker, `watch-<id>.lock`, flag without a version bump, stops on
  window/pane gone, stage change or signal; spawn failure is a warning.

## Deviations from the spec

- Spec §14 step 5 (Bug A's lesson in Bug B's brief) needs the Phase 3 lessons table; the e2e covers steps 1-5 otherwise.
- "Overlap found late (from actual claims)" in §9.2 needs `PostToolUse` (Phase 3).
- The overlap prompt reads a line (key + Enter), not a raw keypress.

## Vex HOLD round 1

Vex returned HOLD on `783d968`; each fix is test-first in its own commit (spec §16 rows 48-54, 56, §9.4).

| Issue | Fix | Test |
|-------|-----|------|
| HIGH: a literal directory claim missed wildcards reaching into it (`src/pkg` vs `**/x.go`), so `plan` made both tasks ready | `globx.under` also meets the wildcard against `dir/**` | `TestLiteralDirectoryMeetsWildcardsReachingIntoIt`, `TestOverlapsNeverMissesACoveredPath`, `TestPlanSeesWildcardReachingIntoALiteralDirectory` |
| MED: fold moved waiters with no cycle check (x->me, y->x, fold me into y: x->y->x) | `foldCycle`: `m` not offered (`FoldCycle` names the waiter) when the target already waits on a waiter | `TestFoldRefusedWhenMovingAWaitClosesACycle`, `TestFoldMovesWaitersWhenNoCycle`, `TestPlanRefusesAFoldThatWouldDeadlock` |
| MED: `claimsHit` failing after the fast-forward rolled a landed task back to `done` | hits computed before `fastForward`; nothing after it calls `back` | `TestFailureAfterFastForwardNeverRollsBackALandedTask` (git diff injected to fail after update-ref) |
| MED: the live-window guard in `staleCheck` was untested | test only | `TestLiveWindowKeepsAnOldClaim` (mutation-checked: fails with the guard disabled) |
| tmux missing or unreachable read as "no windows", so `doctor --fix` from cron could release live claims | `start` records `tmux_socket` (migration 0005); liveness asks that socket; only a clean "no server" is gone, else unknown and nothing expires | `TestStartRecordsTheTmuxSocket`, `TestUnknownTmuxNeverExpiresClaims` (mutation-checked) |
| A task at `starting` when an overlapping merge landed was never flagged | merge counts `starting` candidates; `start`, after recording `in-progress`, checks whether the target moved and changed a claimed file (`rebaseIfTargetMoved`) | `TestMergeLandingDuringStartFlagsNeedsRebase`, `TestClaimsHitIncludesStartingTasks` |
| A SIGKILLed watcher left its attention flag forever | `doctor` reports a flag whose `watch-<id>.lock` is free; `--fix` clears it | `TestDoctorClearsAnAttentionFlagNobodyWatches` |

## Vex HOLD round 2

Vex returned HOLD on `ccfa093`; each fix is test-first in its own commit (spec §16 rows 50-51, §9.2 step 6).

| Issue | Fix | Test |
|-------|-----|------|
| HIGH: bracketed route directories (`app/[slug]`, `app/[...slug]`, `app/[[...slug]]`, `app/(shop)/[slug]`, `src/routes/[id]`) read only as patterns, so `plan` and the needs-rebase hits missed `app/[slug]/page.tsx` and `**/page.tsx` | `globx.Overlaps` reads a glob with `[` both as a pattern and as an exact name (brackets literal, literal subtree) and overlaps when either does; parser takes `\` escapes internally | `TestBracketedRouteDirectories`, `TestOverlapsNeverMissesABracketedPath` (brute force, both orders), `TestPlanSeesBracketedRouteDirectories`, `TestClaimsHitMatchesFilesInBracketedDirectories` |
| LOW: a task with no recorded socket never expired after a reboot (default socket file gone: "error connecting to ... (No such file or directory)"); doctor said only "tmux did not answer" | `noTmuxServer` accepts a missing `tmux-<uid>/default` only for the default server (no `-S`/`-L` override); `windowList.why` carries the reason and doctor prints it | `TestMissingDefaultSocketMeansNoServer`, `TestUnknownTmuxNeverExpiresClaims` |
| LOW: the watcher opened a write transaction every tick | skip `SetAttention` when `t.Attention` already equals the reason | `TestWatchSteadyStateDoesNotWrite` (another connection holds the write lock; the watcher keeps capturing) |
| LOW: a failed socket lookup or target check after the Claude window opened returned an error and left the task at `starting` | warn and go on: socket falls back to `$TMUX`'s (else none, the default server), a missed needs-rebase is left to `merge`, which rebases every task it lands; task is `in-progress`, watcher starts | `TestStartWarnsWhenTheSocketLookupFailsAfterLaunch`, `TestStartWarnsWhenTheTargetCheckFailsAfterLaunch`, `TestEnvSocketReadsTMUXOnlyWithoutAnOverride` |

## Open risks

- The detector is a heuristic over the visible pane; a Claude Code redesign of its dialogs needs new fixtures.
- A start killed between opening the tmux window and recording it leaves a window doctor cannot link to the task.
- `needs-rebase` hits are computed from claims read before the fast-forward (merge.lock held); a claim added before
  the merge's transaction is missed, and that task is still rebased by `merge` itself when its turn comes.
- `merge` stops at the first error across all targets, so a blocked `main` task holds back a hotfix queued after it
  (spec row 52, §9.4; README no longer says "per target"). `merge --task <task>` merges one task.
- Watcher: not restarted when a task goes back from `done` to `qa`; writes only when the flag changes; exits on the
  first DB error (its flag is then cleared, or left for `doctor --fix` if it was killed).
- Tasks started before migration 0005 have no recorded socket: their liveness asks the caller's default tmux server,
  accepting "no server running on", or a missing default socket file (`tmux-<uid>/default`), as gone. A window on another server reads as gone once that server says so.
- A failed socket lookup at `start` outside tmux records no socket; with a `-L` override that is the wrong server (tests only;
  production never sets one).
- `status` WINDOW still asks the caller's default tmux server (display only; never releases anything).

---

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

Spec ambiguities resolved with the safest option; each is also a row in docs/spec.md §16 (25-43).

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

## Vex BLOCK round 2

Vex returned BLOCK on `fa19076` with five items; each fixed test-first in its own commit (spec §16 rows 39, 41-43).

| Item | Fix | Test |
|------|-----|------|
| A clean clone did not build: `coverage.*` ignored `internal/coverage/coverage.go` | `!*.go` in `.gitignore`; the file is committed. No other source file is ignored | clean `git clone` + all gates |
| `merge --accept-regression` had no human check | `requireHuman` before the project opens; refused -> exit 5, nothing merged | `TestMergeAcceptanceNeedsHuman` |
| A test needing an ignored file failed in the clean baseline worktree, so the task breaking it passed as pre-existing | (a) baseline fail + gate fail is a `regression` when the test passed in an earlier completed run of the task; (b) optional `[tests].setup` (`{root}` = main checkout) runs in every clean baseline worktree; a failing setup records no baseline, exit 6 | `TestGateBlocksTestThatPassedEarlierInTheTask`, `TestSetupPreparesCleanWorktrees`, `TestLoadErrors` |
| Ctrl-C reaching git first was classified as a conflict | `interrupted()`: ctx cancelled, git killed by a signal, or exit 130 -> rebase aborted, task `done`, exit 130; an interrupted fast-forward leaves the task at `merging` for the next merge to settle | `TestRebaseKilledBySignalIsInterrupt`, `TestFastForwardKilledBySignalIsSettled` |
| `repro green` could be recorded on the red commit | after a `fail` red, green needs a later commit with a different tree; else exit 9, nothing recorded (`not-reproducible` backports exempt) | `TestReproGreenNeedsALaterCommitWithChanges` |

Remaining: `[tests].setup` is not part of `command_hash`, so a baseline cached before a setup change is reused (rule (a) still applies). Rule (a) only catches a test that passed in some earlier run of the task; a task that breaks a setup-dependent test before any run with the file, with no `[tests].setup`, still reads as pre-existing.

## Vex HOLD round 3

Vex returned HOLD on `d14fc2d` with three items; each fixed test-first in its own commit (spec §16 rows 41, 44, 45).

| Item | Fix | Test |
|------|-----|------|
| A task breaking an ignored-file test (`.env`) in its first commit, before any run, merged with no warning when no `[tests].setup` was set | Fallback chosen: with no `setup`, every `pre-existing` row of a gate run (`merge`, `test --kind gate`) blocks with exit 9; the note says to configure `[tests].setup` or have a human accept with `merge --task <task> --accept-regression <test>`. The preferred "prove it on a run with the same file set" is unreliable: `start`'s baseline runs in the freshly created worktree before `.env` is copied in, so it is a clean baseline too, and no ignored-file inventory is recorded. Test fixtures that rely on pre-existing failures now declare `setup = "true"` (`junitConfig`); `junitNoSetup` keeps the bare config | `TestGateBlocksUnprovenBaselineFailureWithoutSetup`, `TestHumanAcceptsUnprovenBaselineFailure` |
| `[tests].setup` was not in the baseline cache key | `CommandHash(command, format, rerun_command, setup)`; start, merge and every run use `project.commandHash()` | `TestCommandHash`, `TestSetupChangeInvalidatesCachedBaseline` |
| `repro green` cannot prove a real fix | Documented risk in README and spec §8.3, §16 row 45 | docs only |

Remaining: with no `setup`, any project with a known failing test needs a human acceptance (or `setup = "true"`) at every merge. `setup = "true"` reopens the ignored-file hole by the user's explicit choice; rule (a) still applies. Changing the hash input re-runs every cached baseline once.
