# Phase 1 progress

Plan: [todo.md](todo.md). Base: `496e05c`.

## Where I am

- Done: 1-28 (schema, pure packages, store APIs, every Phase 1 command, the six
  end-to-end acceptance tests in `internal/cli/phase1_e2e_test.go`, README and spec).
- Remaining: 29 gates, 30 adversarial self-review.

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
