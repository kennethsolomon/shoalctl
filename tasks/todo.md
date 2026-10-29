# Phase 1: agent team, QA loop, zero-regression merge gate

Acceptance bar: spec §15 Phase 1 row. Each "Done when" item has an end-to-end test
against a temp git repo in `internal/cli/phase1_e2e_test.go`.

Conventions kept from Phase 0: every state change in one `BEGIN IMMEDIATE` write
(`store.write`), `tasks.version` guarded updates (`store.Update`), lock order
`merge.lock` -> `task-<id>.lock` -> `git.lock`, argv-only git/tmux calls, exit
codes from §4.7 (new: 9 gate blocked), errors name the operation.

## Schema

- [x] 1. Migration `0003_phase1.sql` (auto-registered by the embed loader; test:
  `TestMigrationsEmbedded` sees 3 versions; `TestPhase1Schema` checks every new table
  and column).
  - `tasks` + `qa_round INTEGER NOT NULL DEFAULT 0`, `merged_sha`, `parent_task_id`
    (FK tasks), `blocked_by`, `gate_run_id` (FK test_runs), `large_diff_reason`.
  - `file_claims(id, task_id, agent, path_glob, kind, expires_at, released_at, version, created_at)`
  - `messages`, `message_reads` exactly as §7.4
  - `contracts(task_id, version, sha256, frozen_at, frozen_by)` PK (task_id, version)
  - `test_quarantine(id, test_id, detected_run_id, reason, quarantined_at, released_at)`,
    one open row per test (partial unique index)
  - `coverage_files(run_id, path, lines_total, lines_covered, uncovered)` (`uncovered`:
    line ranges `1-3,7`, needed for "Untested areas")
  - `regression_proofs(task_id, test_id, test_file, red_status, red_run_id, red_sha,
    green_run_id, green_sha, test_changed_after_red)` PK (task_id, test_id)
  - `scope_items(id, task_id, source, text, created_at)`
  - `scope_cases(id, scope_item_id, category, description, check, test_id, manual_step,
    status, waive_reason, note, decided_by, decided_at, version, created_at)`
  - `regression_overrides(id, task_id, gate_run_id, target, reason, accepted_by, accepted_at)`
  - `qa_rounds(task_id, round, run_id, passed, failures, created_at)` PK (task_id, round)

## Pure logic (unit tests, no git)

- [x] 2. `testrun.Compare` gains `flaky` input status and class `unstable`; `Blocking`
  lists blocking targets; quarantined ids excluded (test: table test of every §8.5 row).
- [x] 3. `testrun.RerunCommand` substitutes `{tests}` shell-quoted and `{junit}` (test).
- [x] 4. `internal/coverage`: lcov, cobertura, go-cover parsers to per-file
  total/covered/uncovered ranges; `Ranges` compaction (tests per format).
- [x] 5. `internal/diffx`: `--numstat` parse, `-U0` hunk parse to changed line ranges,
  `IsTestFile` heuristic (tests).
- [x] 6. `internal/globx`: claim glob validation (relative, no `..`, no abs/`~`/drive,
  no control chars) and `**` matcher (tests incl. traversal attempts).

## Store

- [x] 7. Results per attempt (`FinishRun` takes attempts), `EffectiveResults`
  (pass/fail/skip/flaky per test), quarantine add/list/release (store tests).
- [x] 8. Tasks: new columns scanned; `CreateTask` takes parent id and user scope items
  in one transaction (test).
- [x] 9. Messages, contracts, claims, scope, proofs, overrides, qa_rounds store APIs
  (store tests for inbox read-once semantics and broadcast, claim conflicts).

## Commands (each with CLI tests through `Main`)

- [x] 10. Flaky re-runs in `runSuite` (`[tests].flaky_retries`, `rerun_command` or whole
  suite, attempts stored); baseline flaky -> quarantine; coverage collected when
  `coverage_format` set.
- [x] 11. `test <task> [--kind gate|qa|baseline]` (prints classes vs baseline, exit 9 on a
  blocking row) and `test quarantine list|release <test>`.
- [x] 12. `msg send|inbox`.
- [x] 13. `contract freeze` (version bump needs `--note`, auto broadcast `contract-change`).
- [x] 14. `claim` / `release` (per agent, one task, conflicts refused exit 5).
- [x] 15. `agents install [--commit]` / `agents list`: eight embedded `shoal-*` defs,
  `model: inherit`, `shoal_version`, sha256 manifest, edited files left alone.
- [x] 16. `new --scope`, `scope add [--behaviour]`, `scope case`, `scope show`,
  `scope review` (terminal only, `w`/`k`), `scope check` (terminal only).
- [x] 17. `repro red|green` with regression_proofs, backport not-reproducible.
- [x] 18. `qa`: contract hash check, suite vs baseline, lint/build; round counter;
  3-round cap -> `escalated`, window renamed `!<slug>`, escalation report with "Blocked".
- [x] 19. `done`: allowed from in-progress/qa; refuses (10) without red/green proof,
  over hotfix line budget (`--accept-large-diff --reason`), open scope rows, untested
  behaviours, unfrozen contract; hotfix creates backport task; report gains QA rounds,
  proof, scope table, quarantined tests, untested areas.
- [x] 20. `merge [--accept-regression <t> --reason <r>]...`: merge.lock, done tasks oldest
  first, rebase onto target tip (conflict -> abort, `needs-rebase`, 9), baseline at new
  base (temp detached worktree), gate run, compare + coverage + lint/build, overrides
  (unknown target -> 2), fast-forward (update-ref or ff-only in a clean main checkout),
  hotfix -> production branch, tag-based hotfix skipped, report re-rendered. Never pushes.
- [x] 21. Brief: team section per lane (§7.3), scope items; status shows QA round.

## Acceptance (end-to-end, temp git repo + private tmux + fake claude)

- [x] 22. `TestPhase1FeatureLaneWithTeam` (Done-when 1)
- [x] 23. `TestPhase1ScopeBlocksDone` (2)
- [x] 24. `TestPhase1BugfixNeedsProof` (3)
- [x] 25. `TestPhase1MergeGateRegression` (4)
- [x] 26. `TestPhase1FlakyQuarantined` (5)
- [x] 27. `TestPhase1HotfixMergesAndBackports` (6)

## Docs and gates

- [x] 28. README (Phase 1 built, commands, exit 9), spec status line + deviations.
- [x] 29. Gates: vet x3 GOOS, `go test -race`, staticcheck, cross test-compile.
- [x] 30. Adversarial self-review (injection, traversal, lock order) and fixes.
