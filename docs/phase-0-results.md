# Phase 0 Results: De-risk Spikes

Date: 2026-10-25. Machine: Apple M2 Max, macOS 26, Go 1.27.3, Claude Code 2.1.310.

| Spike | Question | Result |
|-------|----------|--------|
| Skeleton | Go module + `cmd/shoalctl` builds and prints a version | PASS |
| A | `modernc.org/sqlite` supports FTS5 without CGo | PASS (darwin/arm64, darwin/amd64 run; linux + windows cross-compile only) |
| B | Hooks in `.claude/settings.local.json` load in a worktree outside the repo | PASS, with a double-fire caveat |
| C | Real startup time of the binary | about 5-10 ms bare, 8-12 ms hook-shaped |

## Skeleton

- Module `github.com/kennethsolomon/shoalctl`, entry point `cmd/shoalctl/main.go`.
- `go build`, `go vet ./...`, and `gofmt -l .` are clean.

## Spike A: FTS5 on modernc.org/sqlite

- Test: `internal/spike/fts5_test.go`. It creates an `fts5(path, body)` table, inserts 3 rows, checks an `OR` MATCH ranked by `rank` (2 hits) and a column-filtered MATCH (1 hit).
- `modernc.org/sqlite` v1.61.0, `CGO_ENABLED=0`.
- It ran and passed on darwin/arm64 natively and on darwin/amd64 under Rosetta.
- It cross-compiles for linux/amd64, linux/arm64, and windows/amd64. It was **not run** there: the local Docker daemon (OrbStack) was off. Release CI in Phase 5 must run this test on every target.

## Spike B: hooks in an outside worktree

Setup: a throwaway repo in a temp dir, plus two worktrees at `~/.shoal/worktrees/phase0-spikeb-{local,inherit}`. Each `settings.local.json` had a `SessionStart` hook and a `PreToolUse` (matcher `Read`) hook, and each hook wrote a marker file. Each worktree was run headless: `claude -p "... read a.txt ..." --model haiku --allowedTools Read`.

| Case | Hooks fired |
|------|-------------|
| Worktree has its own `.claude/settings.local.json`, and so does the main checkout | Worktree hooks **and** main-checkout hooks: both SessionStart and PreToolUse ran **twice** |
| Worktree has no settings file; only the main checkout has one | Main-checkout hooks (SessionStart + PreToolUse) |

Other observations:
- In both cases `CLAUDE_PROJECT_DIR` and the hook's `cwd` are the **worktree** path, not the main checkout.
- `.claude/settings.local.json` listed in the main repo's `.git/info/exclude` keeps `git status` clean inside the worktree, so the shared exclude covers worktrees.

Implication for design: these headless runs suggest (inferred, not confirmed) that Claude Code also loads the main checkout's local settings, so shoal must register each hook in exactly **one** place. Two options, to be decided by the maintainer in Phase 2:
1. Main checkout only. Every task worktree inherits the hooks for free, but they also fire in sessions you run in the main checkout without shoal. The hook can exit early when `CLAUDE_PROJECT_DIR` is not a shoal worktree.
2. Worktree only, written by `shoalctl start`. This requires that the main checkout's local file has no shoal hooks.

Not covered by this test: interactive sessions. `-p` skips the workspace trust dialog. In an interactive session, a never-opened directory under `~/.shoal/` may show the trust prompt before any hooks run. 2-minute manual check:

1. `git worktree add ~/.shoal/worktrees/trust-check -b trust-check` in any throwaway repo, then add a `SessionStart` hook (`touch /tmp/shoal-trust-marker`) to that worktree's `.claude/settings.local.json`.
2. `cd ~/.shoal/worktrees/trust-check && claude`, and note whether a trust prompt appears.
3. Quit, `ls /tmp/shoal-trust-marker`, then clean up with `git worktree remove --force ~/.shoal/worktrees/trust-check`.

## Spike C: startup time

`hyperfine` is not installed. A Python harness ran each command with `subprocess.run`: 20 warmup runs, then 300 timed runs (100 for Python). There were 3 back-to-back passes, so spawn overhead is included and the machine was not idle.

| Command | Median | p95 |
|---------|--------|-----|
| `/usr/bin/true` (spawn floor) | 3.9-6.7 ms | 4.9-11 ms |
| `shoalctl` (prints version) | 4.6-9.6 ms | 6.3-16 ms |
| `hookprobe`: open `memory.db`, `CREATE ... IF NOT EXISTS` fts5, one MATCH | 8.3-11.8 ms | 11-19 ms |
| `python3 -c "import sqlite3"` (3.14) | 40-65 ms | 50-114 ms |

`internal/spike/hookprobe` is the hook-shaped probe. Binary sizes: `shoalctl` 2.4 MB, `hookprobe` 10 MB (modernc sqlite linked).

The earlier claim of "roughly 5 ms" holds for the bare binary only. A real hook that opens the DB costs about 8-12 ms, which is still about 4-6x faster than Python before Python does any work. The spec and README now cite these numbers.

## Out of scope / still open

- The spec's Phase 0 MVP commands (`init`, `new`, `start`, `status`, `done`) were not started. This pass covered only the de-risk spikes.
- FTS5 has not been executed on linux and windows.
- The interactive trust-dialog behaviour in outside worktrees (manual check above).
