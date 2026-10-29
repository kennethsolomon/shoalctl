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

Spec ambiguities resolved with the safest option; each is also a row in docs/spec.md §16 (25-36).

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
