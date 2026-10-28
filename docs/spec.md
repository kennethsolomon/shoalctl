# shoal: Design Spec

> Status: **pre-alpha, Spec v2.** The de-risk spikes are done ([phase-0-results.md](phase-0-results.md)). The Phase 0 MVP commands (`init`, `new`, `start`, `status`, `done`, plus the internal `hook`) are implemented per §15.1; every other command below is not built yet and exits 2 as an unknown command.

shoal is a lightweight CLI for running several Claude Code sessions on one repo at the same time without them stepping on each other, and for keeping the reasons behind changes around after a session ends.

Locked design choices:
- Our own CLI. Two ideas are borrowed from octogent (MIT): scoped context per job, and `todo.md` as the task list.
- No web UI, no server, no daemon.
- tmux is the terminal manager. shoal only launches and tracks windows in it.
- Works with any stack. Each project's config declares its test/lint/build commands.
- Written in Go, shipped as a single static binary.
- Zero regression: a test that passed before a task started can't fail when it merges unless a human accepts it by name (§8).

---

## 1. Goals / Non-goals

### Goals
| # | Goal |
|---|------|
| G1 | Each task gets its own branch, worktree, and tmux window |
| G2 | Three lanes: **feature**, **bugfix**, **hotfix**. QA loop capped at 3 rounds in every lane |
| G3 | Catch file overlap at planning time, not at merge time |
| G4 | Merge one task at a time, rebase the others, re-run their tests |
| G5 | A per-project memory of WHY (root causes, decisions, conventions, lessons), linked to SHAs and file paths |
| G6 | Session-start brief under ~1k tokens. Everything else is pulled in only when the related file is touched |
| G7 | A short report for every task. The report is the DB record, rendered to markdown |
| G8 | Project data never leaves `~/.shoal/` unless you opt in per project |
| G9 | Zero regression: no test that passed at baseline fails at merge unless a human accepts it by name, with a reason (§8) |

### Non-goals
- No web UI, dashboard, or server
- No re-implementation of tmux, git, or Claude Code subagents
- No direct messaging between sessions or agents. They coordinate through the SQLite store only: claims, the contract file, and the `messages` table
- No embeddings or vector DB in v1 (FTS5 only)
- No storing transcripts
- No copying git history into the DB (what changed and who changed it comes from `git` when needed)
- No CI replacement. Local gates only

---

## 2. Runtime: Go

| Reason | Detail |
|--------|--------|
| Startup latency | Hooks fire on every file read and edit. Measured in Phase 0 (Apple M2 Max, median of 300 runs, process spawn included): the bare binary takes about 5-10 ms, and a hook-shaped call (open `memory.db`, one FTS5 query) about 8-12 ms (p95 under 20 ms). `python3 -c "import sqlite3"` alone takes about 40-65 ms. See [phase-0-results.md](phase-0-results.md) |
| Zero runtime deps | One binary. No Python or Node install needed on the machine |
| Cross-compile | macOS, Linux, Windows from one build |
| Distribution | Binary `shoalctl`. GitHub Releases, a Homebrew tap (`kennethsolomon/tap/shoalctl`, planned), and `go install github.com/kennethsolomon/shoalctl/cmd/shoalctl@latest`. Repo: `github.com/kennethsolomon/shoalctl` |
| SQLite | [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite): a CGo-free translation of C SQLite, so cross-compiling needs no C toolchain |

> FTS5 note: confirmed in Phase 0. `modernc.org/sqlite` v1.61.0 creates, fills, and `MATCH`-queries an FTS5 table with `CGO_ENABLED=0` (`internal/spike/fts5_test.go`). The test passed on darwin/arm64 and darwin/amd64. It cross-compiles for linux/amd64, linux/arm64, and windows/amd64, but has not run there yet; release CI (Phase 5) must run it on every target.

Cost: the MVP is estimated at about one extra day compared with a Python prototype.

---

## 3. Reuse, Don't Rebuild

Most of the machinery already exists. shoal is mostly glue plus a SQLite file.

| Need | Existing feature we reuse | What shoal adds |
|------|---------------------------|-----------------|
| Isolation | `git worktree`, branches | Naming, creation, cleanup |
| Terminal multiplexing | tmux windows | `tmux new-window -n <task>` + status tracking |
| Role agents (lead, backend, frontend, integration, docs, QA) | Claude Code subagents (`.claude/agents/*.md`) | Eight language-agnostic `shoal-*` definitions (§7) |
| Context injection | Claude Code hooks (`SessionStart`, `PreToolUse`, `PostToolUse`, `Stop`) | Hook handlers that query SQLite |
| Project rules | `CLAUDE.md` | Proposes recurring lessons and active conventions for it |
| Quality gates | The project's own test/lint/build | Runs them from `.shoal.toml`, records pass/fail |
| Search | SQLite FTS5 | Schema + queries |
| Non-interactive runs | `claude -p` | Used for summaries and QA rounds |

---

## 4. Architecture

### 4.1 File layout

```
~/.shoal/                                   # never inside the repo
  config.toml                               # global: default model, report sink, privacy defaults
  logs/hooks.log                            # hook warnings (§4.6)
  projects/
    <project-id>/                           # id = hash of the canonical git remote URL (fallback: repo root path)
      memory.db                             # SQLite + FTS5, WAL mode (memory.db-wal, memory.db-shm)
      locks/                                # merge.lock, git.lock (§10.3)
      runs/<run-id>/                        # raw test output and JUnit copies for each test run
      context.md                            # promoted rules + active conventions, imported by CLAUDE.md
      config.toml                           # project config when the repo runs in stealth mode
      reports/                              # rendered markdown reports (default sink)
      worktrees/
        <task-slug>/                        # git worktree for one task, outside the repo tree
          .shoal/                           # per-task files, listed in .git/info/exclude, never committed
            task.json                       # {"project_id", "task_id", "slug"}: marks this as a shoal worktree
            brief.md                        # task brief passed to claude at start
            summary.json                    # written by the session before `done`
            todo.md                         # execution surface
            contract.md                     # API contract owned by shoal-lead (feature lane)

~/.claude/agents/shoal-*.md                 # stealth default: agent team definitions (§7)

<repo>/                                     # default (stealth): nothing committed
  .claude/settings.local.json               # hook wiring, excluded from git
  .shoal.toml                               # OPT-IN: committed stack commands, no secrets, no memory
  .claude/agents/shoal-*.md                 # OPT-IN: committed agent team definitions
```

### 4.2 Repo footprint

- **Default is stealth.** shoal commits nothing. Hooks go into `.claude/settings.local.json`, config lives in `~/.shoal/projects/<id>/config.toml`, and any file shoal creates in the repo is added to `.git/info/exclude`.
- **Committing `.shoal.toml` is opt-in** per project (`shoalctl init --commit-config`), for teams that want shared stack commands.

### 4.3 Project config (`.shoal.toml`)

```toml
[stack]
name = "laravel"                 # free text, used for lesson matching
lint  = "vendor/bin/pint --test"
build = "npm run build"

[tests]                          # the zero-regression protocol (§8) reads this table
command = "php artisan test --log-junit {junit}"  # full suite; {junit} = absolute path shoal expects the report at
format = "junit"                 # junit | go-json | exit-code | none
junit_path = "{junit}"           # where the JUnit XML lands; a path or glob relative to the worktree, or {junit}
rerun_command = "php artisan test --log-junit {junit} --filter {tests}"  # optional: re-run only named tests
flaky_retries = 2                # re-runs of a failing run before a failure counts (0 = no retries)
timeout = "30m"                  # whole-suite timeout per run
coverage_format = ""             # "" (off) | lcov | cobertura | go-cover
coverage_path = ""               # coverage report path, relative to the worktree

[session]
model = "claude-opus-5-5"        # model for sessions shoalctl launches; agent definitions inherit it

[install]
stealth = true                   # true: no CLAUDE.md edits, agents in ~/.claude/agents/. false: managed block (Phase 4)

[lanes]
qa_max_rounds = 3                # same cap in every lane

[merge]
base = "main"
strategy = "rebase"              # rebase | squash

[hotfix]
production_ref = "production"    # branch name, tag name, or "tag:<glob>" for the newest matching tag
backport = true                  # create a backport task to [merge].base when the hotfix is done
max_diff_lines = 100             # minimal-diff rule: added + removed lines, excluding test files

[claims]
stale_after = "2h"               # a claim with no heartbeat for this long, and no live tmux window, is stale

[reports]
sink = "local"                   # local | notes | both
notes_path = ""                  # your notes folder, only used when sink includes notes

[privacy]
deny_globs = [".env*", "storage/**"]
```

### 4.4 CLI commands

| Command | What it does |
|---------|--------------|
| `shoalctl init [--commit-config] [--rebind]` | Registers the project, creates `~/.shoal/projects/<id>/`, applies migrations, wires hooks once in the main checkout (§4.6). Stealth by default |
| `shoalctl brief` | Prints the session-start brief (<1k tokens). Also run by the `SessionStart` hook |
| `shoalctl new <lane> "<title>" [--ask "<text>"] [--scope "<text>"]...` | Creates the task record in lane `feature`, `bugfix`, or `hotfix` |
| `shoalctl plan <task>` | Agent writes `todo.md` plus file claims. Runs the overlap check |
| `shoalctl start <task> [--model <model>]` | Creates the branch and worktree, records the baseline test run (§8.2), opens a tmux window, launches `claude` with the task brief (§15.1) |
| `shoalctl hook <event>` | Internal: the hook handlers Claude Code calls. Always exit 0 |
| `shoalctl status` | Board of tasks: lane, stage, QA round, claims, blocked-by |
| `shoalctl qa <task>` | Runs the gates and the adversarial QA subagent. Increments the round. Escalates after 3 |
| `shoalctl done <task>` | Writes the summary record, renders the report, enqueues the task for merge |
| `shoalctl merge [--accept-regression <test> --reason "<text>"]...` | Processes the merge queue one task at a time through the merge gate (§8.5). The flag is the only override, may repeat, and is recorded in the report |
| `shoalctl test <task> [--kind baseline\|gate\|qa]` | Runs the configured suite in the task worktree and records per-test results |
| `shoalctl repro red\|green <task> --test <id>` | Records the red (failing) or green (passing) run of a bugfix or hotfix regression test |
| `shoalctl scope add <task> "<text>"` | Adds a required scope item ("also test X"). `new --scope "<text>"` does the same at creation |
| `shoalctl scope show\|review <task>` | Shows the case matrix; `review` walks every non-green row: `w` waive (asks a reason), `k` keep |
| `shoalctl scope check <task> <case-id> --pass\|--fail [--note "<text>"]` | Records the result of a manual-check row |
| `shoalctl test quarantine list\|release <test>` | Lists or releases quarantined flaky tests |
| `shoalctl agents install [--commit]` / `shoalctl agents list` | Installs the `shoal-*` agent definitions: `~/.claude/agents/` by default, `<repo>/.claude/agents/` with `--commit` |
| `shoalctl contract freeze <task>` | Records the contract hash and version; later edits must bump the version |
| `shoalctl claim <task> --as <agent> <glob>...` / `shoalctl release <task> --as <agent> [<glob>...]` | Per-agent file claims inside one task |
| `shoalctl msg send <task> --from <agent> --to <agent\|*> --kind <kind> "<body>"` | Leaves a message in the store |
| `shoalctl msg inbox <task> --as <agent> [--all]` | Prints unread messages for that agent and marks them read |
| `shoalctl recall <query \| --file path>` | FTS5 search over decisions, lessons, conventions, and bugs |
| `shoalctl why <file \| sha \| bug-id>` | Shows the stored WHY together with live `git log` for that file or SHA |
| `shoalctl convention propose\|agree\|retire` | Manages the convention lifecycle |
| `shoalctl doctor [--fix]` | Checks DB integrity, migrations, stuck stages and runs, worktrees, tmux windows, hook registration, stale claims; repairs what is safe (§10.4). A minimal version that checks only the hook entries ships early, with the MVP (§15.1) |
| `shoalctl gc` | Removes merged worktrees, closes tmux windows, flags stale records |
| `shoalctl export <id>` / `shoalctl purge <id>` | Offboarding. `purge` asks for confirmation |
| `shoalctl uninstall` | Removes hook entries and the managed CLAUDE.md block, nothing else |

### 4.5 SQLite schema (`memory.db`)

Store the reasons. Point at git for everything else.

| Table | Key columns | Notes |
|-------|-------------|-------|
| `tasks` | `id, slug, lane(feature\|bugfix\|hotfix), title, ask, stage, qa_round, branch, worktree, tmux_window, base_ref, base_sha, baseline_run_id, merged_sha, blocked_by, parent_task_id, session_id, last_seen_at, version, created_at, updated_at, closed_at` | One row per task. `parent_task_id` links a backport to its hotfix |
| `file_claims` | `id, task_id, agent, path_glob, kind(expected\|actual), expires_at, released_at, version` | Planning claims. `agent` lets subagents in one worktree own separate files |
| `summaries` | `task_id, decision, alternatives_rejected, why, verification, risks, followups, commits(json), files(json), true_at_sha` | **The report.** Single source of truth |
| `bugs` | `id, task_id, symptom, root_cause, fix_sha, files(json), regression_test, true_at_sha, stale` | Regression lookups |
| `decisions` | `id, task_id, topic, choice, rationale, files(json), true_at_sha, stale, superseded_by` | ADR-lite |
| `lessons` | `id, pattern, fix, stack, path_glob, hits, last_hit_at, promoted, true_at_sha, stale` | One row per QA failure |
| `conventions` | `id, rule, scope_glob, status(proposed\|active\|retired), proposed_at, agreed_at, source_task_id` | e.g. "use the strategy pattern from now on" |
| `qa` | `id, question, answer, files(json), task_id, true_at_sha` | Questions you answered during work |
| `schema_migrations` | `version, name, checksum, applied_at` | Applied migrations; `PRAGMA user_version` mirrors the max (§10.6) |
| `merge_queue` | `task_id, position, state(waiting\|rebasing\|testing\|merged\|failed)` | Serial merge |
| `messages` | `id, task_id, from_agent, to_agent, kind, subject, body, reply_to, created_at` | Agent-to-agent notes inside one task (§7.4). `to_agent = '*'` is a broadcast |
| `message_reads` | `message_id, agent, read_at` | Per-recipient read state, so a broadcast is read once by each agent |
| `contracts` | `task_id, version, sha256, frozen_at, frozen_by` | One row per frozen contract version |
| `test_runs` | `id, task_id, kind(baseline\|gate\|qa\|repro\|final), sha, command, command_hash, format, fidelity(per-test\|suite), status(running\|completed\|error\|timeout\|cancelled), exit_code, attempts, total, passed, failed, skipped, output_path, pid, started_at, finished_at` | One row per suite run (§8.1) |
| `test_results` | `run_id, test_id, attempt, status(pass\|fail\|skip), duration_ms, message` | One row per test per attempt. PK `(run_id, test_id, attempt)` |
| `test_quarantine` | `test_id, detected_run_id, reason, quarantined_at, released_at` | Flaky-at-baseline tests (§8.7) |
| `coverage_files` | `run_id, path, lines_total, lines_covered` | Per-file coverage, when configured |
| `regression_proofs` | `task_id, test_id, red_run_id, red_sha, green_run_id, green_sha, test_changed_after_red` | Red-then-green proof for bugfix and hotfix (§8.3) |
| `scope_items` | `id, task_id, source(user\|behaviour), text, created_at` | "also test X" and feature behaviours |
| `scope_cases` | `id, scope_item_id, category, description, check(automated\|manual), test_id, manual_step, status(pending\|pass\|fail\|waived), waive_reason, decided_by, decided_at, version` | The case matrix (§8.6) |
| `regression_overrides` | `task_id, gate_run_id, target, reason, accepted_by, accepted_at` | Every `--accept-regression`. `target` is a test id, `coverage:<path>`, or `(suite)` |
| `fts_*` | FTS5 virtual tables over summaries, bugs, decisions, lessons, conventions, qa | Search |

The DB does **not** store diffs, authors, commit messages, blame, or transcripts. shoal runs `git log`, `git show`, or `git blame` live when needed.

### 4.6 Hooks

Inject only what matches the file being touched, and only once per session. The MVP registers only `SessionStart` and `Stop`, as heartbeats with no output (§15.1); the injecting handlers below arrive in Phase 3.

| Hook | Trigger | Injects (via `additionalContext`) | Budget |
|------|---------|-----------------------------------|--------|
| `SessionStart` | Session opens | `shoalctl brief`: active task, its `todo.md` head, pending proposed conventions, open blockers | ≤ 1k tokens |
| `PreToolUse` (Read/Edit/Write) | A file is about to be touched | Conventions, lessons, decisions whose globs match that path. Each record is tagged "verify against current code" | ≤ 300 tokens per file, deduped per session |
| `PostToolUse` (Edit/Write) | A file was changed | Records an actual claim. Warns if another open task claims the path | ~1 line |
| `Stop` | Session ends | Asks for a structured summary (not the transcript) when the task is at `done` | n/a |

Claude Code caps a hook's `additionalContext` at 10,000 characters; shoal's own budgets sit well below that.

**Where hooks are registered (decided).** Every hook is registered exactly once, in the **main checkout's** `.claude/settings.local.json`. shoal never writes hook entries into a task worktree's settings. Phase 0 showed that a worktree under `~/.shoal/` loads the main checkout's local settings as well as its own, so a second registration would fire every hook twice. With one registration, every task worktree gets the hooks for free.

**Guard: exit early outside shoal worktrees.** Because the hooks live in the main checkout, they also fire in Claude sessions you run there without shoal. Each handler starts with the same check:

1. Read `CLAUDE_PROJECT_DIR` (Phase 0: it is the worktree path, not the main checkout). If unset, use the `cwd` field of the hook input.
2. Resolve symlinks. The directory is a shoal worktree only if it sits directly under `$SHOAL_HOME/projects/<id>/worktrees/` (`$SHOAL_HOME` defaults to `~/.shoal`) **and** contains `.shoal/task.json`.
3. If not, exit 0 with no stdout and no stderr. The session behaves as if shoal were not installed.

**Fail open.** A shoal hook must never break or block your Claude session.
- On any internal error (DB locked past `busy_timeout`, missing task row, bad JSON input, panic), the handler appends one warning line to `$SHOAL_HOME/logs/hooks.log` (time, event, project dir, error) and exits 0 with no output.
- Handlers never exit 2 (Claude Code treats exit 2 as "block"). The binary alone cannot promise that: a Go runtime fatal error (stack overflow, out of memory, concurrent map write) exits 2, and a crash, a kill, or a moved binary (127) never reach shoal's code. So the registered command itself maps every exit status to 0: `<path> hook <event> || true` in bash on macOS and Linux, `& '<path>' hook <event>; exit 0` in PowerShell on Windows. The exec form (`"args"`) is not used because it runs no shell and so cannot remap the status.
- Each hook process has a hard 2-second deadline, below the `timeout` set on the hook entry, and it covers logging too: the handler gets 1.7 s, and the failure log line (written on another goroutine, along with any filesystem work to find where it goes) gets what is left. A log write that cannot finish in time is dropped. The process exits 0 within 2 s whatever the filesystem or database does.
- `shoalctl status` shows a warning when `hooks.log` has new lines since the last `status`, so failures stay visible without touching the session.

**Entry shape.** Each entry calls the absolute path of the binary captured at `init`, for example `"/usr/local/bin/shoalctl hook session-start || true"`, with `"timeout": 10`. The path is quoted for the shell Claude Code runs the command in: on macOS and Linux that is bash (Claude Code's default), so a path with anything beyond `[A-Za-z0-9_@%+=:,./-]` is single-quoted with `'\''` for a quote. On Windows the default shell depends on whether Git Bash is installed, so the entry pins `"shell": "powershell"` and the command is `& '<path>' hook <event>`, with PowerShell's single-quote rule (every `'`, and the typographic single quotes PowerShell also accepts, doubled); `%`, `$`, backticks, spaces and parentheses are literal inside it. `uninstall` finds shoal's entries by matching a command whose program basename is `shoalctl` and whose first argument is `hook`. Re-running `init` (for example after upgrading the binary) rewrites only those entries.

### 4.7 Exit codes
Every command uses the same codes. Hooks are the exception: they always exit 0 (§4.6).

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Internal error (bug in shoal). Message includes the operation and the underlying error |
| 2 | Usage error: unknown command, bad flag, bad argument |
| 3 | Config error: not a git repo, project not initialized, invalid or conflicting config, DB newer than the binary |
| 4 | Not found: unknown task slug, test id, or case id |
| 5 | Invalid state: the task is in a stage that doesn't allow the command, uncommitted changes, or a destructive step that needs confirmation and no terminal is attached |
| 6 | Baseline could not be recorded |
| 7 | Missing dependency: `git`, `tmux`, or `claude` not on `PATH`, or older than the minimum version |
| 8 | Busy: a lock file is held, the DB stayed locked past retries, or an optimistic-lock conflict persisted after 3 retries |
| 9 | Gate blocked |
| 10 | `done` refused |
| 11 | `doctor` found problems it did not fix |
| 12 | Claude Code has not trusted the main checkout, or its trust state can't be read (`start` preflight, §15.1) |
| 130 | Interrupted by SIGINT or SIGTERM. A running suite was stopped and its run recorded as `cancelled` |

---

## 5. Installing into a project that already has a CLAUDE.md

shoal never rewrites your content.

### 5.1 The managed block (non-stealth projects)
- shoal owns exactly one marked block in `CLAUDE.md`:
  ```
  <!-- shoal:begin -->
  @~/.shoal/projects/<id>/context.md
  <!-- shoal:end -->
  ```
- The block is one import line. Claude Code supports `@path` imports in CLAUDE.md (relative or absolute, home paths allowed, up to four hops of nesting), so shoal's content lives in `~/.shoal/`, outside the repo.
- Claude Code shows a one-time approval dialog the first time a project imports a file outside the working directory. shoal's `init` output tells you to expect it.
- Install is idempotent: re-running `init` finds the markers and leaves the block alone. `shoalctl uninstall` removes the markers and the lines between them, nothing else.

### 5.2 Stealth mode (default)
- The committed `CLAUDE.md` is not touched at all.
- Context arrives through hooks registered once in the main checkout's `.claude/settings.local.json` (§4.6): `SessionStart` for the brief, file-scoped `PreToolUse`/`PostToolUse` for everything else.
- Claude Code adds `settings.local.json` to your global git excludes only when Claude Code itself creates the file. shoal may create it first, so shoal adds it to `.git/info/exclude`. It never edits the committed `.gitignore`. `.git/info/exclude` lives in the shared git dir, so it covers every worktree (confirmed in Phase 0).
- Hooks in worktrees outside the repo (confirmed in Phase 0, Claude Code 2.1.310, headless): a worktree under `~/.shoal/` loads its own `.claude/settings.local.json`, **and it also loads the main checkout's `.claude/settings.local.json`**. If both files contain the hook, it fires twice. So shoal registers each hook in exactly one place, the main checkout (§4.6). `CLAUDE_PROJECT_DIR` is the worktree path in both cases, which is what the hook guard uses.
- Optional: a `CLAUDE.local.md` containing the import line, also added to `.git/info/exclude`. A gitignored `CLAUDE.local.md` only exists in the worktree where it was created, so `shoalctl start` writes one into each task worktree it creates.

### 5.3 Existing rules win
- At install, shoal reads the existing `CLAUDE.md` (and `CLAUDE.local.md`, `.claude/rules/`).
- When a proposed convention or promoted lesson contradicts an existing rule, shoal does not override it. It records the conflict as a `proposed` convention, which forces a brainstorm at the next session start. Project rules win until you change them.

### 5.4 Existing hooks are merged, never replaced
- shoal reads `.claude/settings.json` and `.claude/settings.local.json` before writing, and adds its entries alongside any existing ones.
- Claude Code itself merges hook entries across user, project, and local settings, so a shoalctl hook in the local file runs next to a team hook in the committed file.
- Each shoal entry runs `<abs path>/shoalctl hook <event>`, which is how `uninstall` finds and removes only its own entries (§4.6).

---

## 6. Task Lifecycle

```
new -> planned -> (blocked | ready) -> in-progress -> qa[1..3] -> done -> queued -> merged
                                                     \-> escalated (after round 3)
```

Every task is in exactly one lane, chosen at `shoalctl new <lane>`. The lane decides where the branch starts, which rules apply, and what happens after `done`. All three lanes run the same QA loop with the same 3-round cap.

| | feature | bugfix | hotfix |
|---|---------|--------|--------|
| Branches from | `[merge].base` | `[merge].base` | `[hotfix].production_ref` |
| Branch name | `feature/<slug>` | `bugfix/<slug>` | `hotfix/<slug>` |
| Merges into | `[merge].base` | `[merge].base` | the production branch (see §6.3) |
| Test rule (§8) | tests for every new behaviour | red, then green regression test | red, then green regression test |
| Diff rule | none | smallest change that fixes it | minimal-diff rule, enforced |
| QA | full | full | full, no shortcuts |
| After `done` | enqueue for merge | enqueue for merge | enqueue for merge, create backport task |

### 6.1 Feature lane
1. **Spec**: `shoal-lead` writes `todo.md`, the list of new behaviours, and file claims. You confirm scope
2. **API contract**: `shoal-lead` writes `contract.md` (endpoints, payloads, events, errors) and freezes it before parallel work starts
3. **Parallel build**: `shoal-backend`, `shoal-frontend`, and `shoal-integration` share **one worktree per feature**. Each agent claims its own files, and `PostToolUse` warns an agent that writes outside its claims
4. **Tests**: every new behaviour in the list gets at least one automated test, or a manual-check line you waive explicitly
5. **Docs**: `shoal-docs` updates README, runbooks, and API docs the change affects
6. **Adversarial QA loop**: gates plus `shoal-qa-breaker`, `shoal-qa-regression`, and `shoal-qa-scope`. Each failure is recorded as a lesson. Max 3 rounds, then escalate
7. **Report**: `done` writes the summary and renders the report

### 6.2 Bugfix lane
1. **Trace**: reproduce, find the root cause, write a `bugs` row
2. **Red**: write a regression test that reproduces the bug and fails on the unfixed code. shoal records the failing run
3. **Fix**: smallest change that fixes it
4. **Green**: the same test passes. shoal records the passing run. The test path is stored on the bug record
5. **QA**: gates plus the QA agents. Max 3 rounds
6. **Report**

### 6.3 Hotfix lane
A hotfix repairs what is running in production right now. It follows the bugfix steps, with three differences.

1. **Start point.** The branch starts from `[hotfix].production_ref`, resolved locally (shoal never fetches):
   - a branch name: the tip of that local branch
   - a tag name: that tag
   - `tag:<glob>`: the newest tag matching the glob, by `git tag --list '<glob>' --sort=-v:refname`
   If the ref does not resolve, `start` stops with a config error. When the ref is a branch, the hotfix merges back into that branch. When it is a tag, the merge queue has no target: `done` leaves the branch ready and the report says so; tagging and releasing stay human actions.
2. **Minimal-diff rule.** The fix changes only what the bug needs:
   - no refactors, renames, reformatting, or unrelated cleanup
   - no dependency or lockfile changes unless the bug is in a dependency
   - total added + removed lines outside test files must not exceed `[hotfix].max_diff_lines`
   `done` checks the line budget with `git diff --numstat <base_sha>...HEAD`. Over budget, `done` refuses until the diff shrinks or you pass `--accept-large-diff --reason "<text>"`, which goes into the report.
3. **Full QA, no shortcuts.** Every gate and every QA agent runs, with the same 3-round cap. Production urgency is not a reason to skip a check.

**Backport.** When a hotfix reaches `done` and `[hotfix].backport = true`, shoal creates a bugfix-lane task titled `Backport: <hotfix title>` with `parent_task_id` set to the hotfix. It starts in `new` and is never started automatically. Its brief says:
- cherry-pick the hotfix commits (listed by SHA) onto `[merge].base`
- the hotfix's regression test must exist on the base branch and pass
- if that test already passes on the base branch before the cherry-pick (the bug never existed there), record that in the report instead of a red run

### 6.4 Escalation (after QA round 3)
- The task goes to `escalated`, and the tmux window title changes to `!<task>`
- The report gets a "Blocked" section: what failed, the 3 attempts, the best hypothesis
- No further automatic retries

---

## 7. Agent Team per Task

Each task gets a small team of Claude Code subagents. They are plain `.md` agent definitions; shoal ships them, installs them, and gives them a shared place to coordinate. It does not run its own agent runtime.

### 7.1 Install location
- **Stealth (default):** `shoalctl agents install` writes the definitions to `~/.claude/agents/shoal-<role>.md`. They are user-level, so every project sees them and no repo changes.
- **Opt-in:** `shoalctl agents install --commit` writes them to `<repo>/.claude/agents/shoal-<role>.md` for teams that want them in version control.
- The `shoal-` prefix keeps them from colliding with your own agents.
- Each file carries `shoal_version: <n>` in its frontmatter. shoal records the SHA-256 of every file it writes. On upgrade it replaces a file only if the file still matches a hash it wrote; a file you edited is left alone and reported. `uninstall` removes only unmodified files.
- Definitions are language-agnostic. Stack details (test command, paths, conventions) come from `.shoal.toml`, the task brief, and hook context, never from the agent file.
- Every definition uses `model: inherit`, so the team runs on the session's model (§4.3 `[session].model`).

### 7.2 Roles

| Agent | Job | Writes | Tools |
|-------|-----|--------|-------|
| `shoal-lead` | Plans the task, writes `todo.md` and the behaviour list, splits work between agents, assigns file claims, and **owns the API contract** between frontend and backend. The only agent that edits `contract.md` | `todo.md`, `contract.md`, claims | Read, Grep, Glob, Edit, Write, Bash, Task |
| `shoal-backend` | Server side: data model, business logic, API endpoints, jobs. Implements the contract exactly | Its claimed files | Read, Grep, Glob, Edit, Write, Bash |
| `shoal-frontend` | Client side: UI, state, API calls against the contract | Its claimed files | Read, Grep, Glob, Edit, Write, Bash |
| `shoal-integration` | Third-party APIs: auth flows, webhooks, retries, rate limits, timeouts, idempotency, error mapping | Its claimed files | Read, Grep, Glob, Edit, Write, Bash, WebFetch |
| `shoal-docs` | README, runbooks, API docs, changelog entries the change affects | Docs files only | Read, Grep, Glob, Edit, Write |
| `shoal-qa-breaker` | Adversarial testing. Tries to break the change: edge cases, bad input, auth and permission abuse, concurrency and double-submit, empty and huge data, network and dependency failure. Every break becomes a failing test | Test files only | Read, Grep, Glob, Write, Edit, Bash |
| `shoal-qa-regression` | Runs the full configured suite through `shoalctl test` and compares it with the baseline (§8). Reports regressions; does not fix them | Nothing | Read, Grep, Glob, Bash |
| `shoal-qa-scope` | Turns every user-specified scope item into a case matrix and covers each row with an automated test or a manual-check line (§8.6) | Test files, scope rows | Read, Grep, Glob, Write, Edit, Bash |

QA agents never edit production code. A failure goes back to the lead as a message, and the lead routes the fix.

### 7.3 Team per lane

| Lane | Agents |
|------|--------|
| feature | `shoal-lead`, then whichever of `shoal-backend` / `shoal-frontend` / `shoal-integration` the plan needs, `shoal-docs`, and all three QA agents |
| bugfix | `shoal-lead` (trace + red test), one builder, `shoal-qa-breaker`, `shoal-qa-regression`, plus `shoal-qa-scope` when the task has scope items |
| hotfix | Same as bugfix. All three QA agents run when scope items exist; `shoal-qa-breaker` and `shoal-qa-regression` always run |

The brief tells the main session to act as `shoal-lead` and to spawn the others with the Task tool.

### 7.4 Coordination
Agents can't message each other directly. Subagents of one session report only to their parent, and separate sessions share nothing. Everything goes through three channels.

1. **Contract file.** `<worktree>/.shoal/contract.md`, owned by `shoal-lead`. Sections: `Endpoints`, `Events`, `Types`, `Errors`, `Open questions`. `shoalctl contract freeze <task>` stores its SHA-256 as version 1 before parallel build starts. After a freeze:
   - only the lead edits it; a `PreToolUse` warning fires when another agent opens it for writing
   - any change requires `shoalctl contract freeze` again, which bumps the version, and a broadcast `contract-change` message naming what changed
   - `shoalctl qa` fails if the file's hash differs from the latest frozen version
2. **File claims per agent.** The lead assigns claims with `shoalctl claim <task> --as <agent> <glob>`. Builders edit only inside their claims. `PostToolUse` records the actual path; if the hook input identifies the subagent, the actual claim is attributed to it, otherwise to `shoal-lead`. A write outside the writer's claims produces a one-line warning, not a block.
3. **Messages in the store.** `shoalctl msg send` and `shoalctl msg inbox`. Agents check their inbox at the start of each step and before reporting back. The `SessionStart` brief and the lead's `PreToolUse` context include an unread count.

**Message shape** (`messages` table):

| Column | Type | Rule |
|--------|------|------|
| `id` | INTEGER PK | |
| `task_id` | INTEGER NOT NULL | FK `tasks.id`. Messages never cross tasks |
| `from_agent` | TEXT NOT NULL | `shoal-<role>` or `human` |
| `to_agent` | TEXT NOT NULL | `shoal-<role>`, `human`, or `*` (everyone in the task) |
| `kind` | TEXT NOT NULL | `question`, `answer`, `handoff`, `contract-change`, `blocker`, `qa-failure`, `info` |
| `subject` | TEXT NOT NULL | ≤ 120 chars |
| `body` | TEXT NOT NULL | ≤ 4,000 chars. Point at files and line numbers; don't paste them |
| `reply_to` | INTEGER NULL | FK `messages.id`. `answer` must set it |
| `created_at` | TEXT NOT NULL | RFC 3339 UTC |

Read state lives in `message_reads(message_id, agent, read_at)`, primary key `(message_id, agent)`. `msg inbox --as X` returns messages where `to_agent IN (X, '*')`, `from_agent != X`, and no `message_reads` row exists for X, oldest first, then inserts the read rows in the same transaction. Messages are never edited or deleted while the task is open; `gc` drops them with the task's other working rows after merge, keeping the summary.

---

## 8. Zero-Regression Protocol

The rule: **no test that passed before a task started may fail when the task merges, unless a human says so by name and gives a reason.** This section defines how shoal proves that. It is the most important part of the spec; where another section disagrees, this one wins.

### 8.1 Getting per-test results from any stack
shoal does not parse test-runner output. The project's configured suite command (`[tests].command`) must produce results in one of these formats:

| `format` | How shoal reads it | Fidelity |
|----------|--------------------|----------|
| `junit` | JUnit XML, the universal format. Most runners emit it natively or with a plugin (pytest `--junitxml`, PHPUnit `--log-junit`, Jest `jest-junit`, Vitest `--reporter=junit`, RSpec `rspec_junit_formatter`, Maven Surefire, cargo-nextest, gotestsum). shoal reads every file matching `junit_path` after the command exits | per-test |
| `go-json` | `go test -json` on stdout, parsed natively | per-test |
| `exit-code` | Exit status only. Fallback when the runner has no per-test format | suite (reduced) |
| `none` | No suite. The protocol is inactive and every report says so in its first line | none |

Placeholders in `command`, `rerun_command`, and `junit_path`: `{junit}` is an absolute path inside `$SHOAL_HOME/projects/<id>/runs/<run-id>/junit.xml`; `{tests}` is the list of test ids to re-run, space-separated and shell-quoted. shoal deletes stale files matching `junit_path` before every run, so an old report is never read.

**Test identity.** Every result gets a stable `test_id`:
- `junit`: `<classname>::<name>` from each `<testcase>`. If `classname` is empty, the `file` attribute, then the enclosing `<testsuite name>`, is used instead. A duplicate id within one run gets `#2`, `#3` suffixes in document order.
- `go-json`: `<Package>::<Test>`, subtests included as `<Package>::<Test>/<sub>`. A package that fails without any test result (build error, panic in `init`) records the synthetic id `<Package>::(package)` as `fail`.

**Status mapping.** `pass`; `fail` (JUnit `<failure>` or `<error>`, Go `fail`, or a Go test with no terminal action, such as a timeout or panic); `skip` (JUnit `<skipped>`, Go `skip`). The first 1,000 characters of the failure text go into `test_results.message`.

**Run outcome.** A run is `completed` when results were read. It is `error` when the command could not start, or exited non-zero with no readable results file, and `timeout` when it ran past `[tests].timeout` (the process group is killed). If the command exits non-zero but every parsed test passed, shoal records the synthetic id `(suite)` as `fail`, so a crash after the last test, or a coverage threshold failure, is not lost. Raw stdout and stderr go to `runs/<run-id>/output.log`.

**Reduced fidelity.** With `exit-code`, the only result is `(suite)` with `pass` or `fail`. Every comparison then works on that one id, and every report carries: "Reduced fidelity: whole-suite pass/fail only. A failing baseline hides any new failure."

### 8.2 (a) Baseline
- **When:** during `shoalctl start`, after the worktree is created at `base_sha` and before `claude` is launched. Work does not start without a baseline.
- **What:** the full configured suite, once (plus flaky re-runs, §8.7), on the base commit, in the task worktree.
- **Recorded:** a `test_runs` row with `kind = 'baseline'` and `sha = base_sha`, and one `test_results` row per test. `tasks.baseline_run_id` points at it.
- **Reuse:** if a `completed` baseline already exists for the same `sha` and the same `command_hash` (SHA-256 of `command` + `format` + `rerun_command`), shoal reuses it instead of running again. Two tasks started from the same base share one baseline.
- **Pre-existing failures** are recorded as `fail` at baseline, listed in the brief and the report under "Pre-existing failures", and never counted against the task.
- **If the baseline can't be recorded** (`error` or `timeout`), `start` stops with exit code 6, leaves the worktree in place, and sets the task to `baseline-failed`. Fix the config or the environment, then run `start` again; it resumes.

### 8.3 (b) Bugfix and hotfix: red, then green
1. The agent writes a regression test that reproduces the bug, without the fix, and commits it.
2. `shoalctl repro red <task> --test <id>` runs that test (with `rerun_command` if set, otherwise the full suite) on the current `HEAD`. It must report the test as `fail`. shoal stores `red_run_id` and `red_sha` in `regression_proofs`. If the test passes, `red` exits 9: the test does not reproduce the bug.
3. The agent fixes the bug and commits.
4. `shoalctl repro green <task> --test <id>` runs it again on `HEAD`, which must descend from `red_sha`. It must report `pass`. shoal stores `green_run_id` and `green_sha`.
5. shoal checks whether the test file changed between `red_sha` and `green_sha` (`git diff --quiet red_sha green_sha -- <test file>`). If it did, `test_changed_after_red = 1` and the report flags it for review, because a weakened test can turn green without the bug being fixed.

A bugfix or hotfix task without a complete red/green proof cannot pass `done` (exit 10). The one exception is a backport whose test already passes on the base branch (§6.3): `repro red` then records `not-reproducible` and the report says why.

### 8.4 (c) Feature: a test for every new behaviour
- `shoal-lead` lists each new behaviour as a `scope_items` row with `source = 'behaviour'` (one observable behaviour per row, for example "POST /clients rejects a duplicate email with 422").
- Each behaviour needs at least one `scope_cases` row with `check = 'automated'` whose `test_id` is **new** (absent from the baseline) and passes in the task's latest gate run.
- A behaviour that can't be tested automatically gets a `manual` row, which counts only after you mark it with `scope check` or waive it.
- `done` refuses (exit 10) while any behaviour lacks a passing test or a decided manual row.

### 8.5 (d) Merge gate
Run by `shoalctl merge` for each queued task, after rebasing it onto the latest base branch:

1. Rebase the task branch onto the current tip of the target branch (`new_base_sha`). On conflict, `git rebase --abort`, set the task to `needs-rebase`, exit 9.
2. Get the baseline for `new_base_sha`: reuse a cached one (§8.2) or run it now. The comparison is always against the base the task will actually land on, not the base it started from.
3. Run the full suite on the rebased head (`kind = 'gate'`), with flaky re-runs (§8.7).
4. Compare every `test_id` in either run, ignoring quarantined tests:

| Baseline | Gate | Class | Effect |
|----------|------|-------|--------|
| pass | fail | `regression` | **blocks** |
| pass | missing | `missing` (deleted or renamed) | **blocks** |
| pass | skip | `newly-skipped` | **blocks** |
| pass | flaky (failed, then passed on re-run) | `unstable` | **blocks** (it may be a new race) |
| absent | fail | `new-failure` | **blocks** |
| fail | fail | `pre-existing` | reported, not blamed |
| fail | pass | `fixed` | reported |
| absent | pass | `new` | reported |
| pass | pass | `ok` | counted |

5. **Coverage**, only when `coverage_format` is set: for every file that exists in both runs and appears in `git diff --name-only new_base_sha...HEAD`, line coverage (`lines_covered / lines_total`) must not drop. Any drop blocks as `coverage:<path>`. New files are reported with their coverage but do not block.
6. **Reduced fidelity**: if the baseline `(suite)` is `fail`, shoal cannot prove the absence of a regression, so the gate blocks as `(suite)` whatever the gate run shows.
7. Any blocking row stops the merge: the task returns to `qa` with its round count kept, the gate result is stored, and `merge` exits 9.

**The only override** is a human passing `--accept-regression <target> --reason "<text>"` to `merge`, once per blocking target (a test id, `coverage:<path>`, or `(suite)`). There is no global skip, no config key, and no environment variable. Each override is written to `regression_overrides` with the gate run id and `git config user.name`, and appears in the report under "Accepted regressions". An override that names a target not in the blocking set is an error (exit 2), so a typo can't silently pass.

### 8.6 (e) User-specified test scope
When you say "also test X", it becomes a **required scope item** (`scope_items.source = 'user'`), added with `new --scope` or `scope add`.

`shoal-qa-scope` expands each item into a case matrix. Every item must have at least one row in each category, or a row marked not applicable with a one-line reason:

| Category | Covers |
|----------|--------|
| `happy` | The normal path works |
| `boundary` | Limits: min, max, off-by-one, exact length, first and last page |
| `invalid` | Wrong types, malformed input, injection strings |
| `empty` | Empty, null, missing fields, zero items |
| `permission` | Every role, logged out, another user's resource |
| `concurrency` | Double-submit, two users editing at once, retries |
| `failure` | Errors and network failure: timeouts, 5xx from a dependency, partial writes |
| `ui-state` | Loading, empty, error, success, disabled states (when there is a UI) |

Each row maps to either an automated test (`check = 'automated'`, `test_id` set) or a manual-check line (`check = 'manual'`, `manual_step` describing exactly what to do and what to expect). A not-applicable row is stored as `manual` with the reason and needs a human waiver like any other non-green row.

A row is green when its test passes in the task's latest gate or QA run, or when you record `scope check --pass`. **The task can't reach `done` until every row is green or explicitly waived by a human** through `shoalctl scope review`; `done` exits 10 and lists the open rows. Agents can't waive.

### 8.7 (f) Flaky tests
- When a run has failures and `flaky_retries = N > 0`, shoal re-runs the failing tests up to N times: with `rerun_command` if configured, otherwise the whole suite. Each attempt is stored with its `attempt` number.
- A test that fails on attempt 1 and passes on a later attempt is **flaky** in that run.
- **Flaky at baseline:** the test goes into `test_quarantine` with the run id and its failure text. Quarantined tests are excluded from gate decisions and **listed in every report** under "Quarantined tests" until released with `shoalctl test quarantine release <test>`. Nothing is silently ignored.
- **Flaky at the gate** for a test that was a stable pass at baseline: class `unstable`, which blocks (§8.5).
- A test that fails on every attempt is a plain `fail`.

### 8.8 (g) The honest limit
Zero regression means zero regressions **that the test suite can see**. Every report ends with an "Untested areas" section:
- With coverage configured: each changed line range (from `git diff -U0 base_sha...HEAD`) that no test executed, listed as `path:start-end`.
- Without coverage: every changed non-test file, with the note "coverage not configured: shoal cannot tell which of these lines any test runs."
- Every waived scope row and every accepted regression, repeated here.

### 8.9 Exit codes used by the protocol
| Code | Meaning |
|------|---------|
| 6 | Baseline could not be recorded (`error` or `timeout`) |
| 9 | Gate blocked: a regression, a missing proof, a failed `repro`, or a rebase conflict. The blocking rows are printed |
| 10 | `done` refused: open scope rows, untested behaviours, a missing red/green proof, or the hotfix line budget |

The full exit code list is in §4.7.

---

## 9. Coordination and Conflict Flow

Sessions don't talk to each other. They read and write one SQLite file. Overlap is caught at planning time.

### 9.1 File claims
- During `plan`, every task declares `expected` path globs
- `plan` checks them against open claims of other tasks
- While a task runs, `PostToolUse` records `actual` paths and warns on paths held by another task

### 9.2 Overlap resolution: always ask, one keypress

| Key | Action |
|-----|--------|
| `s` | **Serialize**: the later task gets `blocked_by` the earlier one and starts after it merges |
| `m` | **Merge**: same concern (same root cause, same screen), fold into one task |
| `p` | **Parallel anyway**: accept the risk, conflict is resolved at merge time |
| `?` | Show the overlapping paths and both `todo.md` heads |

shoal never resolves an overlap silently. Overlap found late (from actual claims) warns both tasks, and the later one is queued behind the earlier one at merge time.

### 9.3 Concurrency safety
- Claims are taken inside `BEGIN IMMEDIATE`, so two `plan` runs can't both win the same path. The full rules are in §10
- Nobody writes to the base branch except `shoalctl merge`

### 9.4 Merge queue
There is one queue per target branch: `[merge].base`, plus the production branch for hotfixes. `merge` holds the merge lock (§10.3) for its whole run.

1. Take the first `waiting` task
2. Rebase onto base and run the merge gate (§8.5) plus lint and build
3. Pass: fast-forward the target branch (§10.5), record `merged_sha`, release claims
4. Fail: back to `qa` with the round count kept. Only `--accept-regression` overrides a blocking row
5. Other open tasks whose claims overlap the merged files are marked `needs-rebase`; their next `qa` rebases and re-runs tests

`merge` updates the local base branch only. `git push` is always a human action.

---

## 10. Multi-Agent Concurrency in One Project

Many sessions, many subagents, and the human all run `shoalctl` against the same project at once. Each process opens `memory.db` itself; there is no daemon. These rules keep that safe.

### 10.1 SQLite settings
Every connection runs, in order:
```sql
PRAGMA journal_mode = WAL;      -- readers never block the writer
PRAGMA busy_timeout = 5000;     -- wait up to 5 s for the write lock instead of failing
PRAGMA foreign_keys = ON;
PRAGMA synchronous = NORMAL;    -- safe with WAL; a power cut can lose the last commit, never corrupt
```
- **Every state change runs in one transaction**, opened with `BEGIN IMMEDIATE` so the write lock is taken up front. A command that changes several rows (a stage change plus a claim release, for example) changes them all or none.
- No transaction stays open across a slow step: a test run, a git command, a tmux call, or a prompt. Read, do the slow work, then write in a short transaction guarded by §10.2.
- Writers retry `SQLITE_BUSY` after `busy_timeout` twice with jitter (50-200 ms), then exit 8.
- Hooks use a 1-second `busy_timeout` and fail open (§4.6).

### 10.2 Optimistic locking
`tasks`, `file_claims`, and `scope_cases` have `version INTEGER NOT NULL DEFAULT 1`.
- Every update is `UPDATE ... SET ..., version = version + 1, updated_at = ? WHERE id = ? AND version = ?`, using the version read before the slow step.
- Zero rows changed means another process got there first. The command re-reads the row, checks the change is still valid (for example, the task is still in the stage it expected), and retries up to 3 times. After that it exits 8 and names the row and both versions.
- Stage changes also check the current stage in the `WHERE` clause, so an illegal transition can't win a race.

### 10.3 Lock files
Some work can't be one SQLite transaction. It uses advisory lock files under `$SHOAL_HOME/projects/<id>/locks/`:

| Lock | Held by | Wait | On contention |
|------|---------|------|---------------|
| `merge.lock` | `shoalctl merge`, for the whole queue run | none | exit 8: "merge already running (pid N, started T)" |
| `git.lock` | Any command that changes shared git state: `worktree add/remove`, branch create, rebase, ref update, `worktree prune` | up to 30 s | exit 8 |
| `task-<id>.lock` | `start` and `done` for that task, for the whole command (baseline or final suite included) | none | exit 8: "task <slug> is already starting or running (held by pid N: shoalctl start, ...)" |

- Locks use `flock(2)` (`LockFileEx` on Windows), so the OS releases them when a process dies. A crashed process never leaves a lock stuck.
- The holder writes `{"pid", "command", "task", "started_at"}` into the file for `status` and `doctor` to show. The content is informational; the OS lock is the truth.
- Lock order is always `merge.lock`, then `task-<id>.lock`, then `git.lock`, never the reverse, so they can't deadlock.
- Hook heartbeats update `last_seen_at` and `session_id` only. They never bump `tasks.version`, so a heartbeat cannot make a stage change conflict (§10.2).
- Git commands that only read (`rev-parse`, `diff`, `log`, `status`) take no lock.

### 10.4 Crash recovery
Commands that span several steps record a transient stage first (`starting`, `rebasing`, `merging`) and the final stage last. A crash leaves the transient stage behind, which `doctor` can see.

- **Heartbeat.** Hooks update `tasks.last_seen_at`. Claims carry `expires_at = last_seen_at + [claims].stale_after` (default `2h`).
- **Stale claims.** An open claim past `expires_at` whose tmux window no longer exists is stale. Stale claims don't block `plan`; the overlap prompt shows them marked `stale`. `doctor --fix` releases them.
- **`shoalctl doctor [--fix]`** checks, and with `--fix` repairs what is safe to repair:

| Check | Safe fix (`--fix`) |
|-------|--------------------|
| `PRAGMA integrity_check` and `foreign_key_check` | none: reports and exits 11 |
| Migrations: applied checksums match the binary's | none: reports |
| Task in a transient stage with no live lock holder | roll back to the previous stage |
| `test_runs` stuck at `running` with a dead `pid` | mark `error` (`start` and `done` already do this for their own task; a live `pid` blocks them with exit 8) |
| Merge queue row at `rebasing` with no `merge.lock` holder | asks, then `git rebase --abort` in that worktree and resets the row to `waiting` |
| Worktree recorded in the DB but missing on disk | mark the task `worktree-missing`; the human decides |
| Directory under `worktrees/` with no task row | report only; never deleted |
| `git worktree list` entries whose path is gone | `git worktree prune` |
| tmux window recorded but gone | clear `tmux_window` |
| Hook entries duplicated, or present in a worktree's settings | remove the extra entries |
| Stale claims | release |

`doctor` exits 0 when everything is healthy or fixed, and 11 when a problem remains.

### 10.5 Git safety
- Git operations that change shared state are serialized per repo with `git.lock`.
- **Never without explicit confirmation:** `reset --hard`, `push --force` (and any push: shoal never pushes), branch deletion (`branch -d` or `-D`), `worktree remove --force`, `clean`, `checkout -- <path>`, `restore`, or `stash drop`. The prompt names the branch, worktree, and commits affected. With no terminal attached, the command exits 5 instead of asking.
- A rebase that conflicts is always aborted (`git rebase --abort`), which returns the branch to its pre-rebase commit. shoal never resolves conflicts itself.
- **Fast-forwarding a target branch:** if the branch is not checked out anywhere, shoal uses `git update-ref refs/heads/<branch> <new> <old>`, which fails if the branch moved. If it is checked out in the main checkout, shoal runs `git merge --ff-only` there only when that checkout is clean; otherwise `merge` exits 5 and asks you to commit or stash.

### 10.6 Schema migrations
Versioned from the first release.
- Migrations are numbered SQL files embedded in the binary (`internal/store/migrations/0001_init.sql`, `0002_...`). They are append-only: an applied migration is never edited.
- Table `schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`. `checksum` is the SHA-256 of the file.
- `PRAGMA user_version` mirrors the highest applied version, so a version check needs no table read.
- On every open, under `BEGIN IMMEDIATE`: read `user_version`; apply each newer migration in order, each in its own transaction that also inserts its `schema_migrations` row and sets `user_version`. Two processes opening at once can't both apply one migration.
- If `user_version` is higher than the newest migration the binary knows, the command exits 3: "memory.db is schema N; this shoalctl knows M. Upgrade shoalctl." It never downgrades.
- Before applying any migration to a non-empty DB, shoal copies it to `memory.db.bak-<version>` with `VACUUM INTO`.

## 11. Memory and Staleness Rules

A record holds what was true at a specific commit. The code is what's true now.

### 11.1 Code over record
- Every record stores `true_at_sha` and `files`
- Every injected record carries: *"Recorded at `<sha>`. Verify against current code before relying on it."*
- When code disagrees with a record, **the code wins**. The agent runs `shoalctl flag-stale <id> "<reason>"`. The record stays for history but is no longer injected

### 11.2 Cheap staleness pre-check (no LLM)
- Before injecting, run `git diff --quiet <true_at_sha> HEAD -- <files>`
- Unchanged: inject as-is. Changed: inject with a `[files changed since record]` tag

### 11.3 Regression lookup ("the bug came back")
1. `shoalctl why bug:<id>` returns root cause, `fix_sha`, files, regression test
2. shoal runs `git log <fix_sha>..HEAD -- <files>` live
3. Output shows the commits that touched those files after the fix, author and SHA straight from git
4. Then checks whether the regression test still exists and still passes

### 11.4 Lessons
- **Recorded** on every QA failure: pattern, fix, stack, path_glob
- **Injected** only when a touched file matches `path_glob` or the stack matches
- **Proposed for CLAUDE.md** once `hits >= 3` across different tasks. **You approve every promotion.** On approval the rule goes into `context.md` (imported by the managed block, or injected by hook in stealth mode), `promoted = 1`, and the per-file injection stops
- **Decay**: not hit in 90 days, no longer injected. Still searchable

### 11.5 Conventions
1. You say "use the strategy pattern from now on". The agent saves it as `proposed` with a scope glob
2. The next session opens with a brainstorm: the brief lists proposed conventions, and the agent discusses scope and exceptions **before doing anything else**
3. Once agreed it becomes `active` and is injected when files in scope are touched
4. Retiring is a command, and the record keeps why

---

## 12. Task Report

Written once, to the `summaries` row. Markdown is just a rendering.

```markdown
---
task: <slug>
lane: feature|bugfix|hotfix
status: merged|escalated
commits: [abc1234, def5678]
date: YYYY-MM-DD
---
# <Title>

## Asked
<one or two lines>

## Decision
<choice> - Rejected: <alt 1> (why), <alt 2> (why)

## Changed
- `path/to/file` - <one line>

## Verified
- Gates: test PASS · lint PASS · build PASS
- QA rounds: 2/3 - round 1 failed on <x>, lesson #12 recorded
- Regression proof: `tests/...` red at abc1234, green at def5678 (bugfix, hotfix)

## Regression check
- Baseline `<sha>`: 412 pass, 3 fail, 2 skip (per-test)
- Gate `<sha>`: 0 regression, 0 missing, 1 fixed, 6 new
- Pre-existing failures: `<test id>`, ...
- Quarantined tests: `<test id>` (flaky at baseline, run #41)
- Accepted regressions: `<target>`: "<reason>" (by <name>)

## Scope
| Item | Rows | Green | Waived |
|------|------|-------|--------|
| also test CSV export | 9 | 8 | 1 (`ui-state`: no UI, waived) |

## Untested areas
- `app/Exports/Csv.php:40-52`

## Risks / Follow-ups
- <item>
```

Target is about 40 lines, including the regression and scope sections. A longer report is a transcript; cut it.

Reports land in `~/.shoal/projects/<id>/reports/` by default. Setting `sink = "notes"` or `"both"` with a `notes_path` also writes them to your notes folder. That is set per project, explicitly.

---

## 13. Privacy

Nothing from a project is shipped, committed, or copied elsewhere by default.

- `memory.db`, reports, worktrees, and task folders live under `~/.shoal/`, never in the repo
- Release artifacts contain the binary and templates only. A CI check fails the release if any `.db` or `reports/` path appears
- No telemetry and no network calls. Claude Code itself is the only thing that talks to the network
- Hook output never includes secrets. `.env*` and `deny_globs` paths are never indexed
- `export` and `purge` cover offboarding

---

## 14. Worked Example (generic app)

Four tasks arrive on the same day in a Laravel + Vue app.

| Task | Lane | Expected claims |
|------|------|-----------------|
| Feature A: Dropbox integration | feature | `app/Integrations/Dropbox/**`, `config/services.php`, `routes/api.php`, `resources/js/settings/integrations/**` |
| Feature B: Clients CRUD | feature | `app/Http/Controllers/ClientController.php`, `app/Models/Client.php`, `routes/web.php`, `resources/js/clients/**` |
| Bug A: Users page shows wrong display name | bugfix | `app/Models/User.php`, `resources/js/users/**` |
| Bug B: Users page sort broken | bugfix | `resources/js/users/**`, `app/Http/Controllers/UserController.php` |

1. `plan` runs for all four. Features A and B touch different route files: both `ready`. Bugs A and B both claim `resources/js/users/**`
2. shoal asks, one keypress. Different root causes, so you press `s`: Bug B is `blocked_by` Bug A
3. `start` opens three tmux windows, each in its own worktree under `~/.shoal/`
4. Bug A: `User::displayName` falls back to email when `first_name` is null. Fix plus regression test. QA round 1 fails because the list view reads a different accessor. Lesson recorded. Round 2 passes. `done`, queued
5. `merge`: Bug A lands. Bug B unblocks, starts from the new base, and its brief includes Bug A's lesson because the paths match
6. Feature A writes `contract.md`; backend and frontend subagents share one worktree, each claiming its own files. QA attacks token expiry
7. Feature B merges after Feature A, rebases cleanly, re-runs tests
8. Three weeks later Bug A regresses. `shoalctl why bug:A` shows the fix SHA and a later commit by another developer that rewrote the accessor and deleted the regression test in the same commit

---

## 15. Phased Build Plan

Every feature in this spec lands in exactly one phase. Commands not yet built are unknown commands (exit 2) until their phase ships.

| Phase | Scope | Done when | Est. |
|-------|-------|-----------|------|
| **Spikes (done)** | Go skeleton, modernc FTS5 spike, hook loading in outside worktrees, startup timing ([phase-0-results.md](phase-0-results.md)) | All four spikes recorded | done |
| **0: MVP** | Exactly: `init` (stealth; installs hooks by option 1), `new` (lane `feature\|bugfix\|hotfix`), `start` (worktree + tmux window + `claude` launched with the task brief, default model `claude-opus-5-5`, configurable), `status`, `done` (rendered report), and a minimal `doctor [--fix]` that checks and repairs only the hook entries (shipped early from Phase 2, because a moved binary otherwise silently disables every hook). Tables `schema_migrations`, `tasks`, `summaries`, `test_runs`, `test_results`. The baseline test run (§8.2) wired into `start`. Full build spec in §15.1 | The acceptance checks in §15.1 pass on a real project | 3-4 days |
| **1: Agent team, QA loop, zero-regression merge gate** | `agents install` (stealth and `--commit`), the eight `shoal-*` definitions (§7); `messages`, `message_reads`, `msg`; `contracts` and `contract freeze`; per-agent `claim`/`release` inside one task (the `file_claims` table); `qa` with the 3-round cap and escalation; feature behaviour rule (§8.4); user scope matrix, `scope add\|show\|review\|check`, `new --scope` (§8.6); hotfix minimal-diff check and automatic backport task (§6.3). The merge gate (§8.5) with `--accept-regression` and `regression_overrides`; `repro red\|green` and `regression_proofs` (§8.3); flaky re-runs and `test_quarantine` (§8.7); coverage comparison and `coverage_files`; "Untested areas" (§8.8); `test` command; hotfixes merging into the production branch; a simple serialized `merge` that takes `done` tasks oldest first, one at a time, under `merge.lock`. Warnings that need `PreToolUse`/`PostToolUse` (writes outside claims, contract edits by non-leads) arrive with those hooks in Phase 3 | One feature runs through the feature lane with backend and frontend agents and a frozen contract; an "also test X" item blocks `done` until every row is green or waived; a bugfix without a red/green proof is refused by `done`; breaking a baseline-passing test blocks `merge` with exit 9, and `--accept-regression` lets it through with the override in the report; a test made flaky on purpose is quarantined and listed; a finished hotfix merges into the production branch and creates its backport task | 7-9 days |
| **2: Claims + queue** | `plan` with expected `file_claims` across tasks and the one-keypress overlap prompt (§9.2); `merge_queue` per target branch and `needs-rebase` for tasks whose claims overlap a merge (§9.4); optimistic locking on claims; stale-claim expiry; the full `doctor [--fix]` (§10.4), extending the minimal one from Phase 0 | The Bug A / Bug B scenario works end to end; two concurrent `plan` runs can't both win one path; `doctor --fix` recovers a task killed mid-`start` and releases a stale claim | 3-4 days |
| **3: Memory + hooks** | `bugs`, `decisions`, `lessons`, `qa`, FTS5; `SessionStart` brief, `PreToolUse` and `PostToolUse` handlers (including the claim and contract warnings from Phase 1); staleness pre-check; `why`, `recall`; lesson hit counting | Brief < 1k tokens measured; a lesson is injected on a matching file touch; a write outside an agent's claims prints a one-line warning | 3-4 days |
| **4: Conventions + promotion** | Convention lifecycle, brainstorm-first brief, lesson promotion proposed after 3 repeats, non-stealth mode (`[install].stealth = false`, managed CLAUDE.md block), conflict detection | "Strategy pattern" example works end to end | 1-2 days |
| **5: Publish** | `export`/`purge`/`uninstall`, `gc`, release CI guard, FTS5 test run on every target, Homebrew tap, cross-compiled releases | Installed on a project with zero repo footprint | 2 days |

If Phase 0 doesn't save time on a real week of work, stop and rethink before Phase 1.

### 15.1 Phase 0 MVP: build spec
This subsection is the contract for the MVP. Where it is more specific than the rest of the document, it wins for Phase 0.

#### Requirements
- `git` 2.31 or newer (`rev-parse --path-format`), `tmux` 3.2 or newer, `claude` on `PATH` (`claude --version` must exit 0). A missing or older tool: exit 7, naming the tool and the version found.
- macOS and Linux. Windows is not supported in the MVP (tmux), but the binary builds there and CI runs every test that needs no tmux or POSIX shell on Windows.

#### Paths
| What | Path |
|------|------|
| Home | `$SHOAL_HOME`, default `~/.shoal`. Directories are created `0700`, files `0600` |
| DB | `$SHOAL_HOME/projects/<id>/memory.db` |
| Project metadata | `$SHOAL_HOME/projects/<id>/project.json`: `{"id", "remote", "root", "created_at"}` |
| Project config (stealth) | `$SHOAL_HOME/projects/<id>/config.toml` |
| Worktree | `$SHOAL_HOME/projects/<id>/worktrees/<slug>/` |
| Task files | `<worktree>/.shoal/` (`task.json`, `brief.md`, `summary.json`) |
| Test run output | `$SHOAL_HOME/projects/<id>/runs/<run-id>/` (`output.log`, `junit.xml` copies) |
| Reports | `$SHOAL_HOME/projects/<id>/reports/<slug>.md` |
| Hook log | `$SHOAL_HOME/logs/hooks.log` |
| Git lock | `$SHOAL_HOME/projects/<id>/locks/git.lock` |

#### Project id
1. Remote: `remote.origin.url`; if there is no `origin`, the alphabetically first remote; if there are none, the absolute path of the main checkout.
2. Canonical form of a URL: drop the scheme, any `user@` and port; turn `host:owner/repo` (scp style) into `host/owner/repo`; lowercase the host; strip a trailing `/` and `.git`. So `https://github.com/acme/app.git` and `git@github.com:acme/app` both become `github.com/acme/app`.
3. `id` = the first 12 hex characters of SHA-256 of the canonical form.

The main checkout is the parent of `git rev-parse --path-format=absolute --git-common-dir`. Bare repos are rejected (exit 3).

#### Config keys used by the MVP
Lookup: `<repo>/.shoal.toml` if it exists, otherwise `$SHOAL_HOME/projects/<id>/config.toml`. Both at once is a config error (exit 3). Precedence for each key: command flag, then project config, then `$SHOAL_HOME/config.toml`, then the default. Unknown keys are a config error, so typos surface.

| Key | Type | Default | Notes |
|-----|------|---------|-------|
| `[session].model` | string | `"claude-opus-5-5"` | Overridden by `start --model` |
| `[install].stealth` | bool | `true` | `false` is rejected with exit 3 until Phase 4 |
| `[tests].command` | string | none | Required unless `format = "none"`. Run with `sh -c` in the worktree |
| `[tests].format` | enum | `"junit"` | `junit`, `go-json`, `exit-code`, `none` |
| `[tests].junit_path` | string | `"{junit}"` | Used when `format = "junit"`. Must be `{junit}` or a glob relative to the worktree with no `..`, `~`, root, or drive; anything else is a config error (exit 3). `start` also exits 3 when its directory resolves outside the worktree through a symlink. Before each run, stale matches are deleted only when they resolve inside the worktree (or shoal's run dir) and git does not track them |
| `[tests].timeout` | duration | `"30m"` | Process group killed on timeout |
| `[tests].flaky_retries` | int ≥ 0 | `2` | Validated in the MVP, applied from Phase 1 |
| `[merge].base` | string | `"main"` | Must resolve to a local branch |
| `[hotfix].production_ref` | string | `""` | Required to `start` a hotfix; resolution in §6.3 |
| `[stack].lint`, `[stack].build` | string | `""` | Run by `done` when set; results are informational |
| `[reports].sink`, `[reports].notes_path` | string | `"local"`, `""` | `notes` or `both` also copies the report to `notes_path` |

Other keys in §4.3 are parsed and validated but have no effect until their phase.

#### Task stages in the MVP
`new` → `starting` → `in-progress` → `done`, with `baseline-failed` as a resumable side stage of `starting`. Stage is a plain `TEXT` column validated in code, so later phases add stages without rebuilding the table.

#### `shoalctl init [--commit-config] [--rebind]`
1. Resolve the main checkout and project id. Not a git repo or a bare repo: exit 3. Then a preflight that runs before anything is written: if git tracks `.claude/settings.local.json` (an `info/exclude` entry cannot hide changes to a tracked file, so stealth would break), or the committed `.claude/settings.json` already registers shoal hooks, exit 3 with the fix and change nothing.
2. If `project.json` exists with a different `root`, exit 3 ("project <id> is registered at <root>"), unless `--rebind`, which updates `root`.
3. Create the directories, open `memory.db`, apply migrations (§10.6).
4. If no config exists, write a commented template: to `<repo>/.shoal.toml` with `--commit-config` (shoal does not commit it), otherwise to the stealth path. If `go.mod` exists at the root, the template sets `command = "go test -json ./..."` and `format = "go-json"`; otherwise `command` is empty and `init` prints the file to edit.
5. Merge hook entries into `<main checkout>/.claude/settings.local.json` (§4.6): create the file if missing; refuse with exit 3 if it is not valid JSON, never overwriting it; keep all other keys; add `SessionStart` and `Stop` entries running `<abs path>/shoalctl hook session-start|stop`, wrapped so the status is always 0 (§4.6), with `"timeout": 10` only if they are not already present; an existing shoal entry with a different binary path or duplicates is replaced by exactly one entry. The edit is a targeted splice: only shoal's own entries change, and every other byte (key order, escapes such as `\u0026`, unicode, indentation, CRLF line endings, a missing final newline) is kept; inserted entries copy the file's indentation and line endings, and nothing is HTML-escaped. The write goes through a temp file in the same directory, fsynced and renamed, keeping the file's mode (a symlinked file is written at its target). The first time `init` changes an existing file it keeps the original at `$SHOAL_HOME/projects/<id>/backups/settings.local.json.bak` (outside the repo, so stealth holds); later runs never overwrite that backup. If the committed `.claude/settings.json` already registers shoal hooks, `init` exits 3 instead, since both files load and every hook would fire twice.
6. Append `/.claude/settings.local.json` and `/.shoal/` to `<git-common-dir>/info/exclude` if missing.
7. Print the project id, DB path, config path, and the hook file. Exit 0.

Running `init` twice changes nothing the second time. Exit codes: 0, 3, 7, 8.

#### `shoalctl new <feature|bugfix|hotfix> "<title>" [--ask "<text>" | --ask-file <path>]`
- Title: 1-120 characters. `ask` defaults to the title.
- Inserts a `tasks` row at stage `new` and prints the slug.
- Slug: `<id>-<words>`, where `<words>` is the title lowercased, runs of anything outside `[a-z0-9]` replaced by `-`, trimmed of `-`, and cut to at most 40 characters at a `-` boundary (`task` if empty). Example: `7-users-page-sort-broken`.
- Exit codes: 0, 2 (bad lane or title), 3.

Commands take a task as its slug or its numeric id.

#### `shoalctl start <task> [--model <model>]`
Allowed from `new` (fresh) or `starting` / `baseline-failed` (resume). Any other stage: exit 5.

1. Check dependencies (exit 7) and config (exit 3; empty `tests.command` with a format other than `none` is a config error). Then the **trust preflight**: read `~/.claude.json` (`$CLAUDE_CONFIG_DIR/.claude.json` when that is set) read-only and require `projects[<main checkout>].hasTrustDialogAccepted == true`, checking the path as git reports it and with symlinks resolved. A missing file, invalid JSON, or no such entry exits 12 with the fix: "run `claude` once in <main checkout> and choose *Yes, I trust this folder*, then retry". Trust of a parent folder does not count. shoal never writes trust entries and never answers the dialog. The preflight runs before any task row, branch, or worktree changes.
2. Resolve the base: `[merge].base` for feature and bugfix, `[hotfix].production_ref` for hotfix. Record `base_ref` and `base_sha`. Set stage `starting` (optimistic update, §10.2).
3. Under `git.lock`: if the worktree doesn't exist yet, `git worktree add -b <lane>/<slug> <worktree> <base_sha>`. If the branch already exists and the worktree doesn't, exit 5; shoal never reuses or deletes a branch it didn't create in this run.
4. Write `<worktree>/.shoal/task.json` and `brief.md`.
5. **Baseline** (§8.2): reuse a completed baseline for the same `base_sha` and `command_hash`, or run the suite now in the worktree, printing progress. Store `tasks.baseline_run_id`. On `error` or `timeout`: stage `baseline-failed`, print the run's `output.log` path, exit 6. On SIGINT or SIGTERM: stage `baseline-failed`, run `cancelled`, exit 130 (see "Stopping a suite" below).
6. Open the tmux window (below) and record `tmux_session`, `tmux_window` (the `#{window_id}`, for example `@12`), `model`, `started_at`.
7. Set stage `in-progress`. Print the window and, if shoal created the tmux session, the attach command. Exit 0.

**Stopping a suite.** Every suite (baseline, final, lint, build) runs in its own process group (a kill-on-close job object on Windows), and its pid is stored in `test_runs.pid`. On timeout, SIGINT, or SIGTERM, shoalctl sends SIGTERM to the whole group, then SIGKILL after 3 s, and records the run as `timeout` or `cancelled`; Ctrl-C at the terminal reaches shoalctl, which forwards it this way. Processes a suite leaves behind are killed when it exits. Before running a suite, `start` and `done` (holding the task lock, §10.3) look for runs of the task still marked `running`: their owner has exited, so a run whose process group is gone is marked `error` with a note in its `output.log`, and one whose suite is still alive (its shoalctl was SIGKILLed) makes the command exit 8 with the pid to stop. Two suites never run in one worktree at once.

**tmux launch.** Inside tmux (`$TMUX` set), shoal uses the current session. Otherwise it uses session `shoal-<first 8 of id>`, creating it detached if needed (`tmux new-session -d -s <name> -n shoal`). Then:
```
tmux new-window -d -t <session> -n <slug> -c <worktree> -P -F '#{window_id}' -- \
  claude --model <model> "Read .shoal/brief.md first. It is your task brief. Follow it."
tmux set-option -w -t <window_id> remain-on-exit on
```
Arguments are passed as an argv list, never through a shell string. The window also gets `SHOAL_HOME` in its environment so the hooks find the same home. Workspace trust is settled by the preflight in step 1: a worktree outside the main checkout opens without the trust dialog exactly when the main checkout is trusted ([phase-0-results.md](phase-0-results.md#workspace-trust-in-outside-worktrees)). Other interactive dialogs, such as approving an MCP server configured outside the project, can still appear and need you to attach.

**Brief file** (`<worktree>/.shoal/brief.md`), written from a template, at most about 1k tokens:
```markdown
# Task <slug>: <title>
Lane: <lane> · Branch: <lane>/<slug> · Base: <base_ref> @ <base_sha short>

## Ask
<ask>

## Baseline (<fidelity>)
<passed> pass, <failed> fail, <skipped> skip at <base_sha short>.
Pre-existing failures (not yours to fix unless the ask says so):
- <test id> (up to 20, then "and N more")

## Rules for this lane
<feature: write an automated test for every new behaviour you add.>
<bugfix: write a test that reproduces the bug and fails first, commit it, then fix.>
<hotfix: same as bugfix, plus: smallest possible diff, no refactors, no dependency changes.>
- Do not break any test listed as passing at baseline.
- Test command: `<tests.command>`

## When you finish
1. Commit all work on this branch. Do not push. Do not merge.
2. Write `.shoal/summary.json` (format below).
3. Tell the human you are finished: they exit this session, then run `shoalctl done <slug>`.

summary.json:
{"decision": "...", "alternatives_rejected": [{"option": "...", "why": "..."}],
 "why": "...", "risks": ["..."], "followups": ["..."],
 "changed": [{"path": "...", "note": "one line"}]}
```

#### Hooks in the MVP
- `hook session-start`: guard (§4.6); then set `tasks.session_id` from the hook input and `last_seen_at`. No output; the brief already arrives as the first prompt.
- `hook stop`: guard; then set `last_seen_at`. No output.
- Both fail open. `PreToolUse` and `PostToolUse` entries are added by `init` in Phase 3.

#### `shoalctl status [--json]`
One row per task not older than 14 days after `done`, newest first:
```
SLUG                     LANE     STAGE        BASE            BASELINE        WINDOW  LAST SEEN
7-users-page-sort-broken bugfix   in-progress  main@3f2a1c9    412/3F/2S       alive   4m ago
6-dropbox-integration    feature  done         main@3f2a1c9    412/3F/2S       gone    2h ago
```
- `BASELINE` is pass/fail/skip, `(suite) pass|fail` for `exit-code`, `none` for `format = "none"`, `-` before start.
- `WINDOW` is `alive` if `tmux_window` appears in `tmux list-windows -a -F '#{window_id}'`, otherwise `gone`. No tmux server counts as `gone`.
- A last line (on stderr) warns when `hooks.log` has new lines since the previous `status`. The byte offset already seen is kept in `hooks.log.seen` next to the log.
- `--json` prints an array of objects with the same fields, snake_case, plus `id` and `worktree`.
- Exit codes: 0, 3.

#### `shoalctl done <task> [--force]`
Allowed only from `in-progress` (exit 5 otherwise). Holds `task-<id>.lock` throughout (§10.3).
1. If the worktree's `index.lock` (`git rev-parse --git-path index.lock`) exists, a git command, most likely the agent committing, is running: wait up to 10 s for it to go, then exit 10 naming the lock and saying how to clear a stale one. `--force` does not skip this.
2. The worktree must be clean (`git status --porcelain` empty; `.shoal/` is excluded) and have at least one commit after `base_sha`. Otherwise exit 5.
3. Read `.shoal/summary.json`. Missing, invalid JSON, unknown keys, or empty `decision` or `why`: exit 5, printing the path and the expected shape.
4. Unless `--force`: if the task's tmux window still has a live pane, the claude session is running and may still be editing or committing, so exit 10 and ask the human to exit claude (`/exit`) first, or to pass `--force` when the agent has finished. A dead pane (claude exited; `remain-on-exit` keeps it), a closed window, or no tmux server counts as ended. A recent hook heartbeat is not used as the signal: `Stop` fires at the end of every turn, which is exactly when the human runs `done`.
5. Run the suite on `HEAD` (`kind = 'final'`) and compare it with the baseline using the classes in §8.5. **In the MVP this comparison is informational:** it is reported, never blocking. The blocking gate arrives in Phase 1. A final run that errors is reported as such and does not stop `done`.
6. Run `[stack].lint` and `[stack].build` if set; record pass or fail.
7. If `HEAD` moved or the worktree became dirty while steps 5-6 ran, exit 10 and record nothing: the results would not describe the commits being reported.
8. In one transaction: insert the `summaries` row (`commits` from `git rev-list --reverse base_sha..HEAD`, `files` from `git diff --name-status -z base_sha...HEAD`, so unicode, spaces, tabs and renames survive byte-exact, `true_at_sha = HEAD`), set `final_run_id`, stage `done`, `closed_at`.
9. Render the report to `reports/<slug>.md` (and to `notes_path` if the sink says so). Print its path. The tmux window is left open. Exit 0.

Exit codes: 0, 3, 4, 5, 8 (task lock held, or an abandoned suite still running), 10 (work may be in flight: `index.lock`, a live session, or the branch moved during the run), 130 (interrupted).

**Report sections**, in this order:
1. Frontmatter: `task`, `lane`, `status: done`, `branch`, `base` (`<ref>@<sha>`), `commits`, `date`
2. `# <title>`
3. `## Asked`: the ask
4. `## Decision`: the decision, then `Rejected: <option> (<why>)` for each alternative
5. `## Why`
6. `## Changed`: one line per file from `git diff --name-status`, with the agent's note when `changed` has one
7. `## Verified`: baseline counts, final run counts, lint and build results
8. `## Regression check (informational)`: counts per class, then the test ids for `regression`, `missing`, `newly-skipped`, `new-failure`, and `fixed`
9. `## Pre-existing failures`: baseline failures, not blamed on the task
10. `## Risks / Follow-ups`
11. `## Fidelity and limits`: the reduced-fidelity line when it applies; "Coverage and untested areas are not computed in this version"; "Flaky re-runs are not applied in this version"

#### `shoalctl doctor [--fix]` (minimal)
Shipped early from Phase 2 and limited to hook registration. Hooks fail open (§4.6), so a broken entry never shows up in a session: the session just runs without shoal's heartbeats. This is how you find out.
- Reads `<main checkout>/.claude/settings.local.json` and checks, for `SessionStart` and `Stop`: exactly one shoal entry; its binary path is absolute, exists, is a regular file and is executable; the entry has the current shape (wrapper, quoting, `shell` on Windows, timeout).
- Prints `ok` and the binary path when healthy. Otherwise lists each problem on stderr and exits 11, naming `--fix`.
- `--fix` runs the same merge as `init` step 5 with the running binary's resolved absolute path (`os.Executable` with symlinks resolved), then checks again. It keeps every other byte of the file and refuses, like `init`, a tracked `settings.local.json`.
- Exit codes: 0 (healthy or fixed), 3 (settings file is not valid JSON, or is tracked), 11 (problems and no `--fix`).

`init` also refuses (exit 1) to register a binary path that is not absolute, and always registers the symlink-resolved path, so a later swap of a symlink cannot change what hooks run.

#### Migration `0001_init.sql`
```sql
CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  checksum   TEXT NOT NULL,
  applied_at TEXT NOT NULL
);

CREATE TABLE tasks (
  id              INTEGER PRIMARY KEY,
  slug            TEXT NOT NULL UNIQUE,
  lane            TEXT NOT NULL CHECK (lane IN ('feature','bugfix','hotfix')),
  title           TEXT NOT NULL,
  ask             TEXT NOT NULL,
  stage           TEXT NOT NULL DEFAULT 'new',
  base_ref        TEXT,
  base_sha        TEXT,
  branch          TEXT,
  worktree        TEXT,
  tmux_session    TEXT,
  tmux_window     TEXT,
  model           TEXT,
  baseline_run_id INTEGER REFERENCES test_runs(id),
  final_run_id    INTEGER REFERENCES test_runs(id),
  session_id      TEXT,
  last_seen_at    TEXT,
  version         INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  started_at      TEXT,
  closed_at       TEXT
);

CREATE TABLE test_runs (
  id           INTEGER PRIMARY KEY,
  task_id      INTEGER REFERENCES tasks(id),   -- task that triggered it; baselines are shared by sha
  kind         TEXT NOT NULL,                  -- baseline | final (MVP); gate | qa | repro later
  sha          TEXT NOT NULL,
  command      TEXT NOT NULL,
  command_hash TEXT NOT NULL,
  format       TEXT NOT NULL,
  fidelity     TEXT NOT NULL,                  -- per-test | suite | none
  status       TEXT NOT NULL,                  -- running | completed | error | timeout
  exit_code    INTEGER,
  attempts     INTEGER NOT NULL DEFAULT 1,
  total        INTEGER NOT NULL DEFAULT 0,
  passed       INTEGER NOT NULL DEFAULT 0,
  failed       INTEGER NOT NULL DEFAULT 0,
  skipped      INTEGER NOT NULL DEFAULT 0,
  output_path  TEXT NOT NULL,
  pid          INTEGER,
  started_at   TEXT NOT NULL,
  finished_at  TEXT
);
CREATE INDEX test_runs_reuse ON test_runs (sha, command_hash, kind, status);

CREATE TABLE test_results (
  run_id      INTEGER NOT NULL REFERENCES test_runs(id) ON DELETE CASCADE,
  test_id     TEXT NOT NULL,
  attempt     INTEGER NOT NULL DEFAULT 1,
  status      TEXT NOT NULL CHECK (status IN ('pass','fail','skip')),
  duration_ms INTEGER,
  message     TEXT,
  PRIMARY KEY (run_id, test_id, attempt)
) WITHOUT ROWID;

CREATE TABLE summaries (
  task_id               INTEGER PRIMARY KEY REFERENCES tasks(id),
  decision              TEXT NOT NULL,
  alternatives_rejected TEXT NOT NULL DEFAULT '[]',  -- JSON
  why                   TEXT NOT NULL,
  verification          TEXT NOT NULL,               -- JSON: baseline + final counts, classes, lint, build
  risks                 TEXT NOT NULL DEFAULT '[]',  -- JSON
  followups             TEXT NOT NULL DEFAULT '[]',  -- JSON
  changed_notes         TEXT NOT NULL DEFAULT '[]',  -- JSON
  commits               TEXT NOT NULL,               -- JSON array of SHAs
  files                 TEXT NOT NULL,               -- JSON array of {status, path}
  true_at_sha           TEXT NOT NULL,
  report_path           TEXT NOT NULL,
  created_at            TEXT NOT NULL
);
```
All timestamps are RFC 3339 UTC text. Columns other phases need (`qa_round`, `merged_sha`, `blocked_by`, `parent_task_id`, and so on) arrive in later numbered migrations.

#### Concurrency in the MVP
WAL, `busy_timeout`, `BEGIN IMMEDIATE` per state change, and the `tasks.version` check (§10.1, §10.2) are in from day one, as is `git.lock` around `git worktree add` and the per-task `task-<id>.lock` held by `start` and `done` (§10.3), so two commands can never run a suite for one task at once. `merge.lock` and per-agent claims come in Phase 1; cross-task claims and `doctor` in Phase 2.

#### Acceptance checks
1. `init` on a real project, twice: the second run changes no file (compare checksums of `settings.local.json` and `info/exclude`).
2. Two tasks (one feature, one bugfix) are created and started back to back from the same base. Both get a tmux window running `claude --model claude-opus-5-5`; the second reuses the first's baseline (one `baseline` row).
3. Each session finishes, writes `summary.json`, and `done` produces a report with all 11 sections.
4. A Claude session opened in the main checkout produces no shoal output and writes nothing to `memory.db`.
5. With `memory.db` renamed away, a hook inside a task worktree exits 0, prints nothing, and adds one line to `hooks.log`.
6. A test command that exits non-zero without writing JUnit makes `start` exit 6 and leaves the task at `baseline-failed`; fixing the command and re-running `start` resumes it.
7. `start --model <other>` launches that model and records it on the task.

---

## 16. Resolved Decisions

| # | Question | Decision |
|---|----------|----------|
| 1 | Runtime | Go, single static binary, `modernc.org/sqlite` |
| 2 | Repo footprint | Commit nothing by default (stealth). Committing `.shoal.toml` is opt-in per project |
| 3 | Overlap resolution | Always ask, one keypress (`s`/`m`/`p`/`?`) |
| 4 | Parallel subagents in the feature lane | One shared worktree per feature; each agent claims its own files |
| 5 | Lesson promotion | Proposed after 3 repeats across tasks; you approve every time |
| 6 | Project identity | Hash of the canonical git remote URL, fallback to repo path |
| 7 | Worktree location | `~/.shoal/projects/<id>/worktrees/` |
| 8 | Lanes | Three lanes: feature, bugfix, hotfix. The old full/light split is gone |
| 9 | Hotfix start point | `[hotfix].production_ref` in `.shoal.toml`: a branch, a tag, or `tag:<glob>` |
| 10 | Hotfix diff and QA | Minimal-diff rule with a line budget; full QA, no shortcuts; a backport task to the base branch is created on `done` |
| 11 | Hook registration | Option 1: once, in the main checkout's `.claude/settings.local.json`. Handlers exit 0 silently when `CLAUDE_PROJECT_DIR` is not a shoal worktree, so nothing double-fires |
| 12 | Hook failure mode | Fail open: on internal error, log a warning to `$SHOAL_HOME/logs/hooks.log` and exit 0. Never exit 2: the registered command wraps the binary so any exit status, including a Go runtime fatal error's 2, becomes 0 |
| 13 | Zero regression | Per-test baseline at `start`; red/green proof for bugfix and hotfix; tests for every feature behaviour; merge gate blocks any baseline-pass test that now fails, is missing, skipped, or unstable, and any coverage drop on touched files. Only override: `merge --accept-regression <target> --reason`, recorded |
| 14 | Test result format | JUnit XML as the universal format, `go test -json` natively, whole-suite exit code as a flagged reduced-fidelity fallback |
| 15 | Flaky tests | Re-run up to `flaky_retries`. Flaky at baseline: quarantined and listed in every report. Flaky at the gate after a stable baseline pass: blocks |
| 16 | Concurrency | SQLite WAL + `busy_timeout`, every state change in one `BEGIN IMMEDIATE` transaction, `version` columns for optimistic locking, `flock` lock files for the merge queue and git, `doctor` for crash recovery |
| 17 | Destructive git | Never `reset --hard`, force-push, branch deletion, or forced worktree removal without explicit confirmation; shoal never pushes |
| 18 | Schema migrations | Numbered, embedded, append-only; `schema_migrations` table plus `PRAGMA user_version`; never downgrade |
| 19 | Default model | `claude-opus-5-5` for every session shoalctl launches, set by `[session].model` and overridable with `start --model`. Agent definitions use `model: inherit` |
| 20 | QA cap | 3 rounds in every lane, then `escalated` |
| 21 | MVP scope | `init`, `new`, `start`, `status`, `done`, and a minimal hook-only `doctor`; tables `schema_migrations`, `tasks`, `summaries`, `test_runs`, `test_results`; baseline wired into `start`. The rest of the zero-regression gate is Phase 1 |
| 22 | Test command | Required. A project without tests must say so with `format = "none"`, and every report then states the protocol is inactive |
| 23 | Task files | Live in `<worktree>/.shoal/`, excluded through `info/exclude`. The brief is passed to `claude` as a one-line first prompt pointing at `.shoal/brief.md` |
| 24 | Workspace trust | `start` checks, read-only, that Claude Code trusts the main checkout and exits 12 otherwise. Worktrees inherit that trust. shoal never writes trust into `~/.claude.json` and never answers the dialog, whose default is "No, exit" |
