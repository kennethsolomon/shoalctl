# shoal: Design Spec

> Status: **pre-alpha, design stage.** Nothing in this document is implemented yet. It describes what shoal is meant to do.

shoal is a lightweight CLI for running several Claude Code sessions on one repo at the same time without them stepping on each other, and for keeping the reasons behind changes around after a session ends.

Locked design choices:
- Our own CLI. Two ideas are borrowed from octogent (MIT): scoped context per job, and `todo.md` as the task list.
- No web UI, no server, no daemon.
- tmux is the terminal manager. shoal only launches and tracks windows in it.
- Works with any stack. Each project's config declares its test/lint/build commands.
- Written in Go, shipped as a single static binary.

---

## 1. Goals / Non-goals

### Goals
| # | Goal |
|---|------|
| G1 | Each task gets its own branch, worktree, and tmux window |
| G2 | Two lanes: **full** for features, **light** for bugs. QA loop capped at 3 rounds |
| G3 | Catch file overlap at planning time, not at merge time |
| G4 | Merge one task at a time, rebase the others, re-run their tests |
| G5 | A per-project memory of WHY (root causes, decisions, conventions, lessons), linked to SHAs and file paths |
| G6 | Session-start brief under ~1k tokens. Everything else is pulled in only when the related file is touched |
| G7 | A short report for every task. The report is the DB record, rendered to markdown |
| G8 | Project data never leaves `~/.shoal/` unless you opt in per project |

### Non-goals
- No web UI, dashboard, or server
- No re-implementation of tmux, git, or Claude Code subagents
- No messaging between agents (sessions coordinate through SQLite only)
- No embeddings or vector DB in v1 (FTS5 only)
- No storing transcripts
- No copying git history into the DB (what changed and who changed it comes from `git` when needed)
- No CI replacement. Local gates only

---

## 2. Runtime: Go

| Reason | Detail |
|--------|--------|
| Startup latency | Hooks fire on every file read and edit. A Go binary starts in roughly 5 ms; a Python interpreter with imports is roughly 50-100 ms or more per call |
| Zero runtime deps | One binary. No Python or Node install needed on the machine |
| Cross-compile | macOS, Linux, Windows from one build |
| Distribution | Binary `shoalctl`. GitHub Releases, a Homebrew tap (`kennethsolomon/tap/shoalctl`, planned), and `go install github.com/kennethsolomon/shoalctl/cmd/shoalctl@latest`. Repo: `github.com/kennethsolomon/shoalctl` |
| SQLite | [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite): a CGo-free translation of C SQLite, so cross-compiling needs no C toolchain |

> FTS5 note: the modernc package documentation references FTS5 internals, and the package is built with FTS5 enabled. Phase 0 includes a spike test that creates an FTS5 virtual table on every release target before anything else depends on it.

Cost: the MVP is estimated at about one extra day compared with a Python prototype.

---

## 3. Reuse, Don't Rebuild

Most of the machinery already exists. shoal is mostly glue plus a SQLite file.

| Need | Existing feature we reuse | What shoal adds |
|------|---------------------------|-----------------|
| Isolation | `git worktree`, branches | Naming, creation, cleanup |
| Terminal multiplexing | tmux windows | `tmux new-window -n <task>` + status tracking |
| Role agents (backend, frontend, QA) | Claude Code subagents (`.claude/agents/*.md`) | A small default set of templates |
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
  projects/
    <project-id>/                           # id = hash of the canonical git remote URL (fallback: repo root path)
      memory.db                             # SQLite + FTS5
      context.md                            # promoted rules + active conventions, imported by CLAUDE.md
      config.toml                           # project config when the repo runs in stealth mode
      reports/                              # rendered markdown reports (default sink)
      worktrees/
        <task-slug>/                        # git worktrees live here, outside the repo tree
      tasks/
        <task-slug>/
          todo.md                           # execution surface
          context.md                        # scoped brief for this task only
          contract.md                       # API contract (full lane only)

<repo>/                                     # default (stealth): nothing committed
  .claude/settings.local.json               # hook wiring, excluded from git
  .shoal.toml                               # OPT-IN: committed stack commands, no secrets, no memory
```

### 4.2 Repo footprint

- **Default is stealth.** shoal commits nothing. Hooks go into `.claude/settings.local.json`, config lives in `~/.shoal/projects/<id>/config.toml`, and any file shoal creates in the repo is added to `.git/info/exclude`.
- **Committing `.shoal.toml` is opt-in** per project (`shoalctl init --commit-config`), for teams that want shared stack commands.

### 4.3 Project config (`.shoal.toml`)

```toml
[stack]
name = "laravel"                 # free text, used for lesson matching
test  = "php artisan test"
lint  = "vendor/bin/pint --test"
build = "npm run build"
test_file = "php artisan test --filter {name}"   # optional, for fast regression runs

[lanes]
feature = "full"
bugfix  = "light"
hotfix  = "light"
qa_max_rounds = 3

[merge]
base = "main"
strategy = "rebase"              # rebase | squash

[reports]
sink = "local"                   # local | notes | both
notes_path = ""                  # your notes folder, only used when sink includes notes

[privacy]
deny_globs = [".env*", "storage/**"]
```

### 4.4 CLI commands

| Command | What it does |
|---------|--------------|
| `shoalctl init [--commit-config]` | Registers the project, creates `~/.shoal/projects/<id>/`, wires hooks (stealth by default) |
| `shoalctl brief` | Prints the session-start brief (<1k tokens). Also run by the `SessionStart` hook |
| `shoalctl new <type> "<title>"` | Creates the task record (`feature`/`bugfix`/`hotfix`), assigns a lane |
| `shoalctl plan <task>` | Agent writes `todo.md` plus file claims. Runs the overlap check |
| `shoalctl start <task>` | Creates the branch and worktree, opens a tmux window, launches `claude` with the scoped context |
| `shoalctl status` | Board of tasks: lane, stage, QA round, claims, blocked-by |
| `shoalctl qa <task>` | Runs the gates and the adversarial QA subagent. Increments the round. Escalates after 3 |
| `shoalctl done <task>` | Writes the summary record, renders the report, enqueues the task for merge |
| `shoalctl merge` | Processes the merge queue one task at a time |
| `shoalctl recall <query \| --file path>` | FTS5 search over decisions, lessons, conventions, and bugs |
| `shoalctl why <file \| sha \| bug-id>` | Shows the stored WHY together with live `git log` for that file or SHA |
| `shoalctl convention propose\|agree\|retire` | Manages the convention lifecycle |
| `shoalctl gc` | Removes merged worktrees, closes tmux windows, flags stale records |
| `shoalctl export <id>` / `shoalctl purge <id>` | Offboarding. `purge` asks for confirmation |
| `shoalctl uninstall` | Removes hook entries and the managed CLAUDE.md block, nothing else |

### 4.5 SQLite schema (`memory.db`)

Store the reasons. Point at git for everything else.

| Table | Key columns | Notes |
|-------|-------------|-------|
| `tasks` | `id, slug, type, lane, title, ask, stage, qa_round, branch, worktree, tmux_window, base_sha, merged_sha, blocked_by, created_at, closed_at` | One row per task |
| `file_claims` | `task_id, agent, path_glob, kind(expected\|actual), released_at` | Planning claims. `agent` lets subagents in one worktree own separate files |
| `summaries` | `task_id, decision, alternatives_rejected, why, verification, risks, followups, commits(json), files(json), true_at_sha` | **The report.** Single source of truth |
| `bugs` | `id, task_id, symptom, root_cause, fix_sha, files(json), regression_test, true_at_sha, stale` | Regression lookups |
| `decisions` | `id, task_id, topic, choice, rationale, files(json), true_at_sha, stale, superseded_by` | ADR-lite |
| `lessons` | `id, pattern, fix, stack, path_glob, hits, last_hit_at, promoted, true_at_sha, stale` | One row per QA failure |
| `conventions` | `id, rule, scope_glob, status(proposed\|active\|retired), proposed_at, agreed_at, source_task_id` | e.g. "use the strategy pattern from now on" |
| `qa` | `id, question, answer, files(json), task_id, true_at_sha` | Questions you answered during work |
| `merge_queue` | `task_id, position, state(waiting\|rebasing\|testing\|merged\|failed)` | Serial merge |
| `fts_*` | FTS5 virtual tables over summaries, bugs, decisions, lessons, conventions, qa | Search |

The DB does **not** store diffs, authors, commit messages, blame, or transcripts. shoal runs `git log`, `git show`, or `git blame` live when needed.

### 4.6 Hooks

Inject only what matches the file being touched, and only once per session.

| Hook | Trigger | Injects (via `additionalContext`) | Budget |
|------|---------|-----------------------------------|--------|
| `SessionStart` | Session opens | `shoalctl brief`: active task, its `todo.md` head, pending proposed conventions, open blockers | ≤ 1k tokens |
| `PreToolUse` (Read/Edit/Write) | A file is about to be touched | Conventions, lessons, decisions whose globs match that path. Each record is tagged "verify against current code" | ≤ 300 tokens per file, deduped per session |
| `PostToolUse` (Edit/Write) | A file was changed | Records an actual claim. Warns if another open task claims the path | ~1 line |
| `Stop` | Session ends | Asks for a structured summary (not the transcript) when the task is at `done` | n/a |

Claude Code caps a hook's `additionalContext` at 10,000 characters; shoal's own budgets sit well below that.

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
- Context arrives through hooks registered in `.claude/settings.local.json`: `SessionStart` for the brief, file-scoped `PreToolUse`/`PostToolUse` for everything else.
- Claude Code adds `settings.local.json` to your global git excludes only when Claude Code itself creates the file. shoal may create it first, so shoal adds it to `.git/info/exclude`. It never edits the committed `.gitignore`. `.git/info/exclude` lives in the shared git dir, so it covers every worktree.
- Optional: a `CLAUDE.local.md` containing the import line, also added to `.git/info/exclude`. A gitignored `CLAUDE.local.md` only exists in the worktree where it was created, so `shoalctl start` writes one into each task worktree it creates.

### 5.3 Existing rules win
- At install, shoal reads the existing `CLAUDE.md` (and `CLAUDE.local.md`, `.claude/rules/`).
- When a proposed convention or promoted lesson contradicts an existing rule, shoal does not override it. It records the conflict as a `proposed` convention, which forces a brainstorm at the next session start. Project rules win until you change them.

### 5.4 Existing hooks are merged, never replaced
- shoal reads `.claude/settings.json` and `.claude/settings.local.json` before writing, and adds its entries alongside any existing ones.
- Claude Code itself merges hook entries across user, project, and local settings, so a shoalctl hook in the local file runs next to a team hook in the committed file.
- Each shoal entry is tagged with a `shoalctl hook` command prefix, which is how `uninstall` finds and removes only its own entries.

---

## 6. Task Lifecycle

```
new -> planned -> (blocked | ready) -> in-progress -> qa[1..3] -> done -> queued -> merged
                                                     \-> escalated (after round 3)
```

### 6.1 Full lane (features)
1. **Spec**: agent writes `todo.md` plus file claims. You confirm scope
2. **API contract**: `contract.md` (endpoints, payloads, events), frozen before parallel work starts
3. **Parallel build**: backend / frontend / integration subagents share **one worktree per feature**. Each agent claims its own files, and `PostToolUse` warns an agent that writes outside its claims
4. **Docs**: update README, runbooks, and API docs the change affects
5. **Adversarial QA loop**: gates plus a QA subagent that tries to break the change. Each failure is recorded as a lesson. Max 3 rounds, then escalate
6. **Report**: `done` writes the summary and renders the report

### 6.2 Light lane (bugs, hotfixes)
1. **Trace**: reproduce, find the root cause, write a `bugs` row
2. **Fix**: smallest change that fixes it
3. **Regression test**: required. Its path is stored on the bug record
4. **QA**: gates plus a short adversarial pass. Max 3 rounds
5. **Report**

### 6.3 Escalation (after QA round 3)
- The task goes to `escalated`, and the tmux window title changes to `!<task>`
- The report gets a "Blocked" section: what failed, the 3 attempts, the best hypothesis
- No further automatic retries

---

## 7. Coordination and Conflict Flow

Sessions don't talk to each other. They read and write one SQLite file. Overlap is caught at planning time.

### 7.1 File claims
- During `plan`, every task declares `expected` path globs
- `plan` checks them against open claims of other tasks
- While a task runs, `PostToolUse` records `actual` paths and warns on paths held by another task

### 7.2 Overlap resolution: always ask, one keypress

| Key | Action |
|-----|--------|
| `s` | **Serialize**: the later task gets `blocked_by` the earlier one and starts after it merges |
| `m` | **Merge**: same concern (same root cause, same screen), fold into one task |
| `p` | **Parallel anyway**: accept the risk, conflict is resolved at merge time |
| `?` | Show the overlapping paths and both `todo.md` heads |

shoal never resolves an overlap silently. Overlap found late (from actual claims) warns both tasks, and the later one is queued behind the earlier one at merge time.

### 7.3 Concurrency safety
- SQLite runs in WAL mode. Claims are taken inside `BEGIN IMMEDIATE`, so two `plan` runs can't both win the same path
- Nobody writes to the base branch except `shoalctl merge`

### 7.4 Merge queue
1. Take the first `waiting` task
2. Rebase onto base and run the full gates
3. Pass: fast-forward base, record `merged_sha`, release claims
4. Fail: back to `qa` with the round count kept
5. Other open tasks whose claims overlap the merged files are marked `needs-rebase`; their next `qa` rebases and re-runs tests

`merge` updates the local base branch only. `git push` is always a human action.

---

## 8. Memory and Staleness Rules

A record holds what was true at a specific commit. The code is what's true now.

### 8.1 Code over record
- Every record stores `true_at_sha` and `files`
- Every injected record carries: *"Recorded at `<sha>`. Verify against current code before relying on it."*
- When code disagrees with a record, **the code wins**. The agent runs `shoalctl flag-stale <id> "<reason>"`. The record stays for history but is no longer injected

### 8.2 Cheap staleness pre-check (no LLM)
- Before injecting, run `git diff --quiet <true_at_sha> HEAD -- <files>`
- Unchanged: inject as-is. Changed: inject with a `[files changed since record]` tag

### 8.3 Regression lookup ("the bug came back")
1. `shoalctl why bug:<id>` returns root cause, `fix_sha`, files, regression test
2. shoal runs `git log <fix_sha>..HEAD -- <files>` live
3. Output shows the commits that touched those files after the fix, author and SHA straight from git
4. Then checks whether the regression test still exists and still passes

### 8.4 Lessons
- **Recorded** on every QA failure: pattern, fix, stack, path_glob
- **Injected** only when a touched file matches `path_glob` or the stack matches
- **Proposed for CLAUDE.md** once `hits >= 3` across different tasks. **You approve every promotion.** On approval the rule goes into `context.md` (imported by the managed block, or injected by hook in stealth mode), `promoted = 1`, and the per-file injection stops
- **Decay**: not hit in 90 days, no longer injected. Still searchable

### 8.5 Conventions
1. You say "use the strategy pattern from now on". The agent saves it as `proposed` with a scope glob
2. The next session opens with a brainstorm: the brief lists proposed conventions, and the agent discusses scope and exceptions **before doing anything else**
3. Once agreed it becomes `active` and is injected when files in scope are touched
4. Retiring is a command, and the record keeps why

---

## 9. Task Report

Written once, to the `summaries` row. Markdown is just a rendering.

```markdown
---
task: <slug>
type: feature|bugfix|hotfix
lane: full|light
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
- Regression test: `tests/...` (bugs only)

## Risks / Follow-ups
- <item>
```

Target is about 25 lines. A longer report is a transcript; cut it.

Reports land in `~/.shoal/projects/<id>/reports/` by default. Setting `sink = "notes"` or `"both"` with a `notes_path` also writes them to your notes folder. That is set per project, explicitly.

---

## 10. Privacy

Nothing from a project is shipped, committed, or copied elsewhere by default.

- `memory.db`, reports, worktrees, and task folders live under `~/.shoal/`, never in the repo
- Release artifacts contain the binary and templates only. A CI check fails the release if any `.db` or `reports/` path appears
- No telemetry and no network calls. Claude Code itself is the only thing that talks to the network
- Hook output never includes secrets. `.env*` and `deny_globs` paths are never indexed
- `export` and `purge` cover offboarding

---

## 11. Worked Example (generic app)

Four tasks arrive on the same day in a Laravel + Vue app.

| Task | Lane | Expected claims |
|------|------|-----------------|
| Feature A: Dropbox integration | full | `app/Integrations/Dropbox/**`, `config/services.php`, `routes/api.php`, `resources/js/settings/integrations/**` |
| Feature B: Clients CRUD | full | `app/Http/Controllers/ClientController.php`, `app/Models/Client.php`, `routes/web.php`, `resources/js/clients/**` |
| Bug A: Users page shows wrong display name | light | `app/Models/User.php`, `resources/js/users/**` |
| Bug B: Users page sort broken | light | `resources/js/users/**`, `app/Http/Controllers/UserController.php` |

1. `plan` runs for all four. Features A and B touch different route files: both `ready`. Bugs A and B both claim `resources/js/users/**`
2. shoal asks, one keypress. Different root causes, so you press `s`: Bug B is `blocked_by` Bug A
3. `start` opens three tmux windows, each in its own worktree under `~/.shoal/`
4. Bug A: `User::displayName` falls back to email when `first_name` is null. Fix plus regression test. QA round 1 fails because the list view reads a different accessor. Lesson recorded. Round 2 passes. `done`, queued
5. `merge`: Bug A lands. Bug B unblocks, starts from the new base, and its brief includes Bug A's lesson because the paths match
6. Feature A writes `contract.md`; backend and frontend subagents share one worktree, each claiming its own files. QA attacks token expiry
7. Feature B merges after Feature A, rebases cleanly, re-runs tests
8. Three weeks later Bug A regresses. `shoalctl why bug:A` shows the fix SHA and a later commit by another developer that rewrote the accessor and deleted the regression test in the same commit

---

## 12. Phased Build Plan

| Phase | Scope | Done when | Est. |
|-------|-------|-----------|------|
| **0: MVP** | Go skeleton, modernc FTS5 spike, `init` (stealth), `new`, `start`, `status`, `done` with report render, `tasks` + `summaries` | Two tasks run in parallel on a real project, two reports produced | 2-3 days |
| **1: Claims + queue** | `plan` with claims, one-keypress overlap prompt, `merge` queue, `needs-rebase` | The Bug A / Bug B scenario works end to end | 2-3 days |
| **2: Memory + hooks** | `bugs`, `decisions`, `lessons`, FTS5, `SessionStart` + `PreToolUse`, staleness pre-check, `why`, `recall` | Brief < 1k tokens measured; a lesson is injected on a matching file touch | 3-4 days |
| **3: Lanes + QA loop** | Lane templates, QA subagent, 3-round cap, escalation | One feature runs through the full lane | 2-3 days |
| **4: Conventions + promotion** | Convention lifecycle, brainstorm-first brief, managed CLAUDE.md block, conflict detection | "Strategy pattern" example works end to end | 1-2 days |
| **5: Publish** | `export`/`purge`/`uninstall`, release CI guard, Homebrew tap, cross-compiled releases | Installed on a project with zero repo footprint | 2 days |

If Phase 0 doesn't save time on a real week of work, stop and rethink before Phase 1.

---

## 13. Resolved Decisions

| # | Question | Decision |
|---|----------|----------|
| 1 | Runtime | Go, single static binary, `modernc.org/sqlite` |
| 2 | Repo footprint | Commit nothing by default (stealth). Committing `.shoal.toml` is opt-in per project |
| 3 | Overlap resolution | Always ask, one keypress |
| 4 | Parallel subagents in the full lane | One shared worktree per feature; each agent claims its own files |
| 5 | Lesson promotion | Proposed after 3 repeats across tasks; you approve every time |
| 6 | Project identity | Hash of the git remote URL, fallback to repo path |
| 7 | Worktree location | `~/.shoal/projects/<id>/worktrees/` |
