# Phase 5 progress

Base: `6bc02ca` (Phase 4 shipped, Vex round 5 SHIP). Approved by Kenneth 2026-11-10. Acceptance bar: spec §15 Phase 5 row, Done when "Installed on a project with zero repo footprint". Nothing was pushed, tagged or published; no repo, secret or GitHub release was created.

## Where I am

- Done: `version` (ldflags plus build info), `uninstall`, `export`, `purge`, `gc`, the release guard, `.goreleaser.yaml`, `.github/workflows/release.yml` (tag push only; `ci.yml` untouched), README install docs (still pre-alpha), spec §16 rows 77-82.
- Vex HOLD round 1 on `d8dc033`: five findings fixed (section below).
- Next: Vex re-review; then Kenneth's outward steps (below) for the first tag.

| Scope item | Where | Proof |
|---|---|---|
| 1 `export <id>`, `purge <id>` | `internal/cli/offboard.go`, `store.Dump` | `TestExportWritesEveryTableAndReport`, `TestPurgeNeedsAHumanWhoTypesTheID` (agent refused exit 5, wrong answer exit 5, worktree left exit 5, lock held exit 8, unknown id exit 4, bad id exit 2), `TestDumpEveryTableButTheIndex` |
| 2 `uninstall` | `internal/cli/uninstall.go`, `removeHooks`, `withoutManagedBlock`, `agents.Remove` | `TestRemoveHooksRoundTrip` (merge then remove is byte-identical for 9 shapes), `TestRemoveHooksMatchesOnlyShoal`, `TestWithoutManagedBlockRoundTrip`, `TestUninstallRestoresAStealthRepo`, `TestUninstallKeepsTheUsersSettings`, `TestUninstallRemovesTheManagedBlock`, `TestUninstallRefusesBeforeChangingAnything`, `TestUninstallRemovesOnlyUneditedAgents`, `TestRemoveKeepsEditedFiles` |
| 3 `gc` | `internal/cli/gc.go`, `store.DropWorkingRows` | `TestPhase5GCAfterMerge` (dirty worktree: task left alone, window and rows kept, exit 5; then window closed and worktree removed, rows dropped, summary kept, gone-glob lesson flagged, live decision not), `TestPhase5GCSkipsOpenTasks`, `TestPhase5GCLeavesAForeignWindowOpen`, `TestPhase5GCLeavesAnUntaggedWindowOpen`, `TestStartTagsItsWindow`, `TestDropWorkingRowsKeepsRecords` |
| 4 `version` | `internal/buildinfo`, `cmd/shoalctl/main.go` | `TestResolve`, `TestString`, `TestVersionLdflags` (builds with the release ldflags) |
| 5 Release pipeline | `.goreleaser.yaml` | `goreleaser release --snapshot --clean`: 6 archives + `checksums.txt` + formula (below); two runs gave identical checksums |
| 6 Release guard | `internal/releaseguard` | `TestCheckFailsOnAPlantedDatabaseOrReport` (8 plants), `TestCheckVerifiesChecksums`; guard PASS on the real snapshot dist, FAIL on a copy with `memory.db` planted in the linux/amd64 tarball (it also caught macOS tar's `._memory.db`) |
| 7 FTS5 on every target | `release.yml` job `test` | Six native runners, `CGO_ENABLED=0`; actionlint clean. Not run on GitHub (no tag pushed) |
| 8 `release.yml`, tag only | `.github/workflows/release.yml` | `on: push: tags: ["v*"]` only; `git diff 6bc02ca -- .github/workflows/ci.yml` empty |
| 9 README install | README Quick start | go install, `brew install kennethsolomon/tap/shoalctl`, archive + `shasum -a 256 --check --ignore-missing checksums.txt` (verified on the snapshot dist) |
| 10 Zero footprint | `internal/cli/phase5_e2e_test.go` | `TestPhase5ZeroRepoFootprint`; manual dry run below |

## Decisions (spec §16)

| Row | Decision |
|-----|----------|
| 77 | uninstall: hook entries (any event), the managed block, unedited `shoal-*` agents (user-level only when no other project is registered); byte-exact; shoal-created files deleted; a repeated `hooks` key (top level, event or inside a group) refused with exit 3, by init and doctor --fix too; data, exclude lines and branches kept ("already gone" after purge) |
| 78 | gc: closed tasks only, per task lock; dirty worktree first (task left alone, exit 5); window killed only when tagged `@shoal_task` = `<project id>/<slug>@<SHOAL_HOME>`, else left open and forgotten; `git worktree remove` without `--force` (kept, exit 5; git-ignored files go with it, said in help and output); working rows dropped; records whose files are all gone from `[merge].base` flagged stale; branches kept |
| 79 | export/purge take the project id and work without the checkout; purge: human types the id, refused with worktrees left or locks held |
| 80 | release.yml: FTS5 on six native runners; one build to a draft release (a re-run replaces a stale draft), guard, draft assets checked against the guarded checksums, draft published; actions pinned by SHA, GoReleaser v2.20.0 |
| 81 | version: ldflags, then `debug.ReadBuildInfo`, then `dev` |
| 82 | Homebrew formula via `brews` (deprecated in GoReleaser 2.10+); GoReleaser never pushes it; release.yml's own `formula` job pushes it after publishing (re-runnable alone, `concurrency: homebrew-tap`, never over a newer version), for stable tags with a non-empty tap token only; manual push command in spec row 82 |

## Snapshot release (local, `goreleaser 2.20.0`, `--snapshot --clean`, no token)

`goreleaser check`: exit 2 (2.20.0), "configuration is valid, but uses deprecated properties" (`brews` only). Snapshot: exit 0 in 18 s; publish, announce and validate skipped by snapshot mode.

```
dist/shoalctl_0.0.1-snapshot+f110372_{darwin,linux}_{amd64,arm64}.tar.gz   LICENSE README.md shoalctl
dist/shoalctl_0.0.1-snapshot+f110372_windows_{amd64,arm64}.zip             LICENSE README.md shoalctl.exe
dist/checksums.txt                                                         sha256 of the 6 archives
dist/homebrew/Formula/shoalctl.rb                                          written, not pushed
```

The linux/amd64 binary is statically linked; the darwin/arm64 one prints `shoalctl 0.0.1-snapshot+f110372 (commit f110372a2f39, built 2026-11-10T03:55:50Z)`. A second snapshot run produced the same `checksums.txt` byte for byte. `dist/` is gitignored.

## Vex HOLD round 1 (on `d8dc033`)

| # | Finding | Fix | Commit | Test |
|---|---|---|---|---|
| 1 HIGH | gc killed a window by recorded id only; tmux reuses ids after a restart | `start` tags its window `@shoal_task` = `<project id>/<slug>` (`@<SHOAL_HOME>` appended in Vex round 2); gc kills only a window with that tag; another tag or none (pre-tag tasks): left open, record forgotten, reported. Only gc kills windows, and only a tagged one. `qa` renames an escalated task's window only when it is tagged (Vex round 2, `TestEscalateRenamesOnlyTheTasksWindow`). The rest act on the recorded id without the tag but only read or forget: doctor, status, the stale check and the watcher read; `done` reads the window's panes and refuses (exit 10) while one is live, so a reused id fails safe: done refuses until the human checks or passes `--force` | `77950a9` | `TestPhase5GCLeavesAForeignWindowOpen`, `TestPhase5GCLeavesAnUntaggedWindowOpen`, `TestStartTagsItsWindow` |
| 2 MED | gc killed the window before finding the worktree dirty | worktree checked first (`git status --porcelain`); dirty or unreadable: task left alone (window, worktree, rows), exit 5 | `06d9807` | `TestPhase5GCAfterMerge` (asserts the window and rows survive the dirty run) |
| 3 MED | guard checked build 1, publish rebuilt; archive bytes followed LICENSE/README mtimes | archived files pinned (commit date, mode, `root:root`); one build to a draft release, guard on that `dist/`, draft assets downloaded and checked against `dist/checksums.txt` (`::error::`, exit 1), then `gh release edit --draft=false` | `ccbceb1`, `3150b56` | snapshot evidence below; asset-check logic run locally: pass, extra asset fails, tampered archive fails |
| 4 LOW | formula pushed for prerelease tags | GoReleaser never pushes (`skip_upload: "true"`); release.yml pushes `dist/homebrew/Formula/shoalctl.rb` after publishing, only for a tag without `-` and with the token | `3150b56` | actionlint clean; not runnable without a tag |
| 5 LOW | unpinned actions; uninstall after purge said "kept"; duplicate `hooks` keys lost both; progress said `check` exits 1 | actions pinned to SHAs (checkout v4.4.0, setup-go v5.6.0, goreleaser-action v6.4.0), GoReleaser `v2.20.0`; "data none: ... already gone (purged)"; repeated top-level `hooks` or event key refused with exit 3 before any change; exit 2 recorded | `06782e3`, `e20d333`, `0e3efc7`, `e050a57` | `TestUninstallAfterPurge`, `TestUninstallRefusesBeforeChangingAnything` (two duplicate shapes) |

Reproducibility (`goreleaser 2.20.0 --snapshot --clean`, commit `e050a57`): two runs in a fresh clone with `touch LICENSE README.md` and `chmod 600 README.md` between them gave byte-identical `checksums.txt`; a third run in the main checkout (different path, clean tree) matched too. The old config (`d8dc033`) gave different checksums after the same touch. A dirty tree still changes the binary (Go embeds `vcs.modified=true`); the release builds from a clean tag checkout.

Gates (fresh clone of `e050a57` in `/private/tmp`, macOS, run alone): `gofmt -l .` empty; `go vet` and `staticcheck` clean for GOOS darwin, linux, windows; `GOOS=linux|windows go test -c ./...` exit 0 (15 test binaries each); `caffeinate -i go test -race -count=1 ./...` pass, 189 s wall, `internal/cli` 185.6 s (three new tmux e2e tests); `goreleaser check` exit 2 (deprecated `brews` only); snapshot exit 0; guard `TestReleaseArtifacts` PASS on its `dist/`; `ci.yml` byte-identical to `6bc02ca`; actionlint on `release.yml` clean (no shellcheck installed).

## Manual dry run (2026-11-10, snapshot binary, scratch repo and SHOAL_HOME)

```sh
git init -b main && commit a.txt; before=$(git status --porcelain --ignored --untracked-files=all)
shoalctl version; shoalctl init      # git status --porcelain: empty
shoalctl new feature "Dry run"; shoalctl status; shoalctl gc          # "nothing to collect"
shoalctl export <id> --out export.json   # 25 tables, schema 7, 1 task
shoalctl export bogus                    # exit 2
shoalctl uninstall                       # 4 entries and the settings file shoal created removed
[ "$before" = "$(git status --porcelain --ignored --untracked-files=all)" ]   # identical
shoalctl purge <id> </dev/null           # exit 5: needs a human at a terminal
(typed the id at a pty) shoalctl purge <id>   # project directory gone
```

It found one wording bug, fixed in `02db19c` (uninstall named a missing CLAUDE.md as "no shoal block in"). Not in the dry run: `start` with a real `claude` (covered by the e2e test with tmux and a fake claude).

## Gates (fresh clone of `2d47b8b`, macOS, run alone)

| Gate | Result |
|---|---|
| `gofmt -l .` | empty |
| `go vet ./...` and `staticcheck ./...`, GOOS darwin, linux, windows | clean |
| `GOOS=linux` and `GOOS=windows go test -c` per package | all compile (9 s) |
| build-tag ignored files (`go list -test`) | only the OS pairs (`*_unix`/`*_windows`) and `race_on_test.go`; no Phase 5 file |
| `git ls-files -oi --exclude-standard` | empty |
| `caffeinate -i go test -race -count=1 ./...` | pass, 153 s wall; `internal/cli` 149.2 s (Phase 4: about 148 s) |

## Spec drift fixed

- §13 said artifacts hold "the binary and templates only": there is no templates directory; the agent definitions and config template are embedded. Now "binary, LICENSE and README.md".
- The spec status line still listed Phases 3 and 4 as unbuilt; README's mockup note called `shoalctl why` planned.
- `export <id>`/`purge <id>` did not say what the id is: now the project id, explicit in §4.4 and row 79.
- `tasks/todo.md` was still the Phase 2 plan; replaced with Phase 5's.

## Kenneth's outward steps (not done; none approved)

1. Create the public repo `kennethsolomon/homebrew-tap` (may be empty; release.yml pushes `Formula/shoalctl.rb` to `main`).
2. Create a fine-grained personal access token: resource owner kennethsolomon, repository access only `homebrew-tap`, permission Contents read and write (Metadata read is implied). Add it to `kennethsolomon/shoalctl` as the Actions secret `HOMEBREW_TAP_GITHUB_TOKEN`. The release itself uses the workflow's `GITHUB_TOKEN` (`contents: write`, also used by `gh` to check and publish the draft); nothing else is needed.
3. Push main, then tag: `git tag -a v0.1.0 -m "shoalctl v0.1.0" && git push origin v0.1.0` (a tag with a suffix such as `-rc.1` is marked prerelease and gets no formula).
4. Check the run: all six test jobs green, then `brew install kennethsolomon/tap/shoalctl && shoalctl version`. If only the `formula` job failed (token, tap), fix it and use "Re-run failed jobs", never "Re-run all jobs"; after the 7-day artifact expiry use the manual push in spec row 82.

## Open risks

- Runner labels `macos-15-intel`, `ubuntu-24.04-arm` and `windows-11-arm` were not exercised (no tag pushed). If one is unavailable the release waits on it; drop that row (its target still cross-compiles) or swap the label.
- `brews` is deprecated in GoReleaser 2.10+; a future v3 removes it. A cask (`homebrew_casks`) would change the install to `brew install --cask` and is macOS-first.
- The draft flow (upload to a draft, `gh release download` of a draft by tag, `gh release edit --draft=false`) and the formula push step could not be exercised without a tag; the asset check's shell logic was run locally against a copy of `dist/` (pass; an extra asset and a tampered archive both fail). If the guard or the asset check fails, the draft stays (invisible to the public); re-running the release job replaces it (`replace_existing_draft: true`, in GoReleaser 2.20.0's schema, `goreleaser check` still exit 2 for `brews` only), or delete it with `gh release delete <tag>`.
- gc trusts the `@shoal_task` tag. A window of a task started before this change has no tag, so gc leaves it open (and says so); close those by hand once. `status`, `doctor` and the stale-claim check still treat any live window under the recorded id as the task's; they never close a window, so the worst case is a claim that does not go stale (unchanged behaviour).
- `gc` keeps task branches and `uninstall` keeps the `.git/info/exclude` lines: invisible to `git status`, but they remain in `.git`.
- `withGitLock` records `merge` as the lock's command for every caller (doctor, gc too); cosmetic, pre-existing, not changed.

# Phase 4 progress

Base: `eca06da` (Phase 3 shipped) plus the CI fix `c789a52`. Acceptance bar: spec §15 Phase 4 row ("Strategy pattern" end to end) and the five Done-when items of the build brief.

## CI fix before Phase 4 (`c789a52`)

CI (ubuntu, windows) failed `internal/gitx` with "Author identity unknown". Cause: `TestChanged` called `testutil.Repo` without `testutil.Isolate`, so its commits used the host's git identity; CI runners have none, and the macOS dev machine's global identity masked it. Fix: `TestChanged` calls `Isolate`, and `testutil.Repo` now fails the test unless `Isolate` set `GIT_AUTHOR_EMAIL`, so a missing `Isolate` fails on every OS, not only on CI. Every test that calls `Repo` must call `Isolate` first, or run in a package whose `TestMain` calls `testutil.IsolateProcess` (`internal/cli`, see Test speed).

## Where I am

- Done: migration 0007 (`conventions`, `lesson_hits` with a backfill from `hook_marks`, FTS triggers into `memory_fts`); the store lifecycle (propose, agree, retire, reopen, promote); `convention propose|agree|retire|list`, `promote lesson:<id>`; active conventions in `PreToolUse`, `recall`, `why`; the brainstorm-first brief; `context.md` rendering; `[install].stealth = false` and the managed CLAUDE.md block in `init`; the §5.3 conflict check at propose, agree, promote and every `init`.
- The previous builder's uncommitted store WIP was reviewed and committed as `35e65ee` (one fix: parenthesised the rule-and-scope match in `ProposeConvention`'s duplicate lookup; migration registration is by embedded file name like 0001-0006; no delete triggers, consistent with 0006, nothing deletes records before Phase 5).
- E2E (`internal/cli/phase4_e2e_test.go`), one per Done-when item: TestPhase4StrategyPatternEndToEnd, TestPhase4LessonPromotion, TestPhase4ManagedBlock, TestPhase4StealthDefault, TestPhase4ConflictWithProjectRule; plus TestPhase4ConventionInjectionLimits (deny_globs, `.git/`, `.shoal/`, 300-token budget, once per session). Unit: TestBriefBudgetKeepsTheBrainstorm, TestWithManagedBlock*, TestInstallManagedBlockFile, TestContextImport, TestReadProjectRules, TestConflictWith, TestStealthOffAccepted; store: TestPhase4*, TestConvention*, TestMarkCountsLessonHitsPerTask, TestProposeFromPromotedLesson.
- Windows: every Phase 4 test runs on Windows. The e2e tests drive the hooks in-process against task directories holding only `task.json` (all the hooks read), so they need neither tmux nor sh. Per-test skips: none. One sub-check skips on Windows: the symlinked-CLAUDE.md refusal in TestInstallManagedBlockFile (creating a symlink needs a privilege Windows runners do not grant by default). TestPhase3HookLatency, extended with conventions, still skips there (it starts real tasks with the sh fake claude and tmux).
- Decisions: spec §16 rows 63-68.
- Vex BLOCK round 1 on `d26c7ba`: four FAILs and five RISKs fixed test-first, one commit each (table below).
- Vex HOLD round 2 on `3ccca1e`: two FAILs and three RISKs fixed test-first (table below); spec §16 rows 71-74.
- Vex HOLD round 3 on `4af9714` and round 4 on `dc7103a`: fixed test-first (tables below); round 4 replaced the marker indent rule with column 0 (row 75).
- Vex round 5 on `bf0b735`: SHIP (section below).
- Next: Phase 5 not started.

## Vex BLOCK round 1

Vex returned BLOCK on `d26c7ba` with four FAILs and five RISKs; each fixed test-first in its own commit (spec §5.1, §5.3, §16 rows 63-70).

| Item | Fix | Test |
|------|-----|------|
| 1 `requireHuman` passed `/dev/null` (a character device) as a human | Real terminal check on stdin and stdout (`go-isatty`, already in the module graph); any process with `CLAUDECODE` or `CLAUDE_CODE_ENTRYPOINT` set is refused with "run it in your own terminal". Applies to every caller (scope, merge, plan, doctor, conventions). pty wrappers documented as residual (row 69, README) | `TestRequireHumanRefusesNullStdin` (subprocess, stdin = `os.DevNull`, agree/retire/merge), `TestInteractiveRealCheck`, `TestRequireHumanRefusesClaudeCodeSession`; `TestMergeAcceptanceNeedsHuman` still green |
| 2 Agent rule text reached context.md verbatim (`@~/.aws/credentials`, unclosed `<!--`) | `inert` breaks word-initial `@` and `<!--`/`-->`, one line; `literal` puts rule, scope and conflict text in a code span longer than any backtick run. `agree` and `promote` print the exact line and ask `y/N`; anything else exits 5, nothing changed | `TestPhase4RuleTextIsInert`, `TestPhase4PromoteShowsRule` |
| 3 Markers inside a code fence were taken as the block | Shared `fences` scanner (``` and ~~~; made CommonMark in round 2) in `ruleLines`, `withManagedBlock` and `importsContext`; a file ending inside an open fence is refused (exit 3) | `TestWithManagedBlockIgnoresFences` |
| 4 Non-stealth task sessions could miss the rules (unapproved import) | The brief carries the rules in every task worktree; only a main-checkout session with the block relies on the import. init, §5.1, §5.2, rows 66 and 68 reworded | `TestPhase4ManagedBlock` |
| 5 Proposals crowded out agreed rules; unbounded proposals; retired rules silently reopened | Drop order keeps one proposal of each kind and then the agreed rules (conventions before promoted lessons, which have no per-file path); `maxOpenProposals` = 10, propose past it exits 5; an identical retired rule and scope exits 5 naming it and why (`store.ErrProposalRefused`) | `TestBriefBudgetKeepsTheBrainstorm`, `TestPhase4ProposalsDoNotCrowdOutRules`, `TestProposeConventionLimits` |
| 6 "No tabs" / "spaces, not tabs" missed; `.claude/CLAUDE.md` unread | A negated one-word clause conflicts with any opposite clause naming the word; `.claude/CLAUDE.md` read. Limits in §5.3 (synonyms, "prefer A over B", "X not Y" without a comma, nested CLAUDE.md) | `TestConflictWithOneWordNegation`, `TestReadProjectRulesDotClaude`, `TestConflictWith` unchanged |
| 7 `promote` accepted a lesson whose convention the human retired | `promote` refuses any non-candidate before the prompt, with the reason; `PromoteLesson` refuses a lesson carried by any convention in its transaction | `TestPhase4ConflictWithProjectRule`, `TestPromoteLessonRefusesRetiredConvention` |
| 8 A BOM before the marker on line 1 gave "unpaired, lines []" | A leading BOM is dropped for matching, on line 1 only (now in `markerText`, round 4); the bytes are kept | `TestWithManagedBlockBOM` |
| 9 `internal/cli` -race near the 600 s timeout | Hook latency check runs fewer times per event under `-race` (build tag `race`), 15 otherwise. The 1.5 s bound stays asserted, but by statistic (round 2 item 5, row 74): under -race the blocking Linux and macOS jobs assert the median of 5; the p95 of 15 is asserted only in local non-race runs (the test needs sh via `testutil.RequireSh` and skips on Windows, so the non-blocking Windows job does not run it). At the time `t.Parallel` was not used (every e2e test went through `Isolate`'s `t.Setenv`); superseded by "Test speed" below (`b2e623d`) | `TestPhase3HookLatency`: 87.6 s -> 30.8 s under -race; package time in Measurements |

## Vex HOLD round 2

Vex returned HOLD on `3ccca1e`: two FAILs (fence scanner) and three cheap RISKs. Each fixed test-first in its own commit (spec §5.1, §5.3, §16 rows 71-74).

| Item | Fix | Test |
|------|-----|------|
| 1 Fence scanner not CommonMark: a ````md example holding a ``` fence with the markers was rewritten as the block, no real block appended, importsContext true, the main-checkout brief dropped the rules; same for ``` closed by ~~~ or ```bash | `fences` follows CommonMark §4.5: opener 0-3 spaces, 3+ backticks or tildes, no backtick in a backtick info string; closer same character, at least as long, 0-3 spaces, only whitespace after; 4+ spaces or a tab is not a fence. One scanner for ruleLines, withManagedBlock, importsContext. An unclosed fence runs to the end: its markers are not a block, its lines not rules, and init still refuses (exit 3) to append inside it, so user bytes are never rewritten (row 71) | `TestFencesCommonMark` (table: nesting, mismatched characters, shorter closer, closer with an info string, longer closer, trailing space, 3-space and 4-space indent, tab, backtick info string, tildes holding backticks, inline triple backticks, unclosed, CRLF, BOM), `TestFencedMarkersRepros` (every repro through withManagedBlock, importsContext and ruleLines) |
| 2 init refused valid markdown (exit 3): ````md holding an unclosed ```go, a 4-space-indented ```; nested code read as rules | Fixed by item 1 | `TestFencedMarkersRepros` cases "unclosed inner fence", "four-space indented ticks", and the nested rules check (no false conflict) |
| 3 Retired-rule refusal matched exact text only; a trailing period got past it | `store.RuleKey` (lower case, collapsed whitespace, trailing punctuation dropped), compared in Go in `ProposeConvention` for both the open-duplicate and the retired checks (row 72) | `TestProposeConventionLimits` (four variants of the retired rule, an open rule with a period), `TestRuleKey` |
| 4 `promote` asked y/N before the conflict check, so y gave a proposal | Conflict check first; the prompt names the conflict and asks to record a proposed convention instead; a decline changes nothing (row 73) | `TestPhase4ConflictWithProjectRule` (prompt text, no "Promote it", decline records nothing, y records convention:2) |
| 5 -race latency "p95 of 3" was the max (1.04-1.09 s vs 1.5 s) | Under -race the 1.5 s bound holds for the median of 5, decided once 3 runs fall on one side (3 processes per event when healthy); plain runs keep the p95 of 15 (row 74). Still asserted in the blocking -race jobs | `TestPhase3HookLatency`: 24.8 s under -race (3 runs per event, medians 1.03-1.07 s) |

Accepted residuals, documented: the pty plus `env -u CLAUDECODE` bypass of `requireHuman` (row 69, README); one-word negation false positives such as "No globals" vs "Read globals from config" (§5.3, README; `--force` covers it).

## Vex HOLD round 3

Vex returned HOLD on `4af9714`: one FAIL (indented markers) and one RISK (RuleKey), plus docs. Each fixed test-first in its own commit (spec §5.1, §5.3, §16 rows 71, 72, 75).

| Item | Fix | Test |
|------|-----|------|
| 1 Markers indented 4+ spaces (indented code block, list-item fence) were taken as the block: `    @~/OLD` rewritten, importsContext true | `markerText`: a marker counts only after 0-3 spaces and no tab, the fenceRun rule, in withManagedBlock, importsContext and ruleLines (no uninstall code exists yet) (row 75) | `TestWithManagedBlockMarkerIndent` (indented code block, list-item fence, tab: block appended, example kept, importsContext false; 3 spaces: counts; an indented begin marker does not hide the rules after it) |
| 2 `RuleKey` stripped any trailing punctuation: "Use C#" == "Use C", "Call init()" == "Call init" | Only trailing `. ! ? , ; :` and `。` dropped; no NFC (no x/text), composed vs decomposed documented (row 72) | `TestRuleKey` (C#, init(), quotes, leading `...`, inner punctuation kept; `;:,` and `。` dropped; unequal pairs) |
| 3 Docs | Blockquote and list-item fences not tracked, with the false-conflict consequence (§5.3, row 71, README); round 1 item 9 latency wording; README marker claim matches item 1 | Docs only |

Accepted, not fixed: an HTML-comment ``` opening a "fence" that makes init refuse (the safe direction); an NBSP after a closer. Documented: blockquote and list-item fences not tracked (§5.3, row 71); no NFC in RuleKey (row 72).

## Vex HOLD round 4

Vex returned HOLD on `dc7103a`, the third round on CLAUDE.md marker parsing. Instead of chasing more CommonMark cases, the class is closed with a simpler rule. Each item fixed test-first in its own commit (spec §5.1, §5.3, §16 rows 71, 72, 74, 75, 76).

| Item | Fix | Test |
|------|-----|------|
| 1 A fence opened on a list-marker line (`` - ``` ``, `` 1. ```md ``) was not seen: its 2-3-space markers counted, `@~/OLD` inside the user's fence was rewritten, no real block appended, importsContext true | A marker counts only at column 0 (after a BOM on line 1), the exact form shoal writes, and outside a tracked fence (`markerText`, used by withManagedBlock, importsContext, ruleLines). Any leading whitespace (spaces, tab, NBSP) is plain text, so list-item content and indented code can never match. Round 3's "0-3 spaces count" is reversed (row 75). `fences` also tracks a fence opened right after a list marker (`-`, `*`, `+`, `1.`, `1)`): it ends at its closer or at the first non-blank line indented less than the item's content, so its code is not read as rules; an unclosed one does not make init refuse, since the appended column-0 block ends it (row 71) | `TestWithManagedBlockMarkerColumnZero` (both repros, `*`/`+`/`1)` variants, 1/2/3/4 spaces, tab, NBSP, BOM plus column 0, BOM on line 2, CRLF, trailing whitespace, unclosed list fence, indented example before a real column-0 block; withManagedBlock, rerun idempotence, importsContext before and after, ruleLines; an indented begin hides no rules), `TestFencesCommonMark` (13 list-item fence cases) |
| 2 An unpaired column-0 begin marker hid every later rule from ruleLines, so agree, promote and propose's conflict check was blind | `ruleLines` holds the lines after a begin until its end; with no end it reads them as rules (init still refuses the file, exit 3) (row 76) | `TestRuleLinesReadPastUnpairedBegin` (unpaired begin, stray end, paired then unpaired, CRLF, a fenced end, readProjectRules plus conflictWith) |
| 3 Docs: progress round 1 item 9 said the p95 of 15 runs in the non-blocking Windows job | `TestPhase3HookLatency` calls `testutil.RequireSh` and skips on Windows: the p95 of 15 is asserted only in local non-race runs (here and row 74). README and §5.1, §5.3, rows 71, 75, 76 for the column-0 rule and list-item fences; RuleKey keeps fullwidth `！` `？` and `…` as a documented limit, no code change (row 72) | Docs only |

## Vex round 5: SHIP on `bf0b735`

0 failures. Three doc fixes in this file: round 1 item 9 (`t.Parallel` note superseded by Test speed, `b2e623d`), round 1 item 8 (the BOM is dropped by `markerText`, on line 1 only), and the Test speed counts (172 top-level tests in `internal/cli`, 145 parallel, 27 serial). One ACCEPT-WITH-DOCS: a marker inside a blockquote or nested-list fence followed by a column-0 marker is handled correctly per CommonMark (a column-0 line ends the container), but the code lines may be read as rules and raise a false conflict, which `--force` covers; documented at spec §5.3 "Not tracked", rows 71 and 75.

## Decisions (spec §16)

| Row | Decision |
|-----|----------|
| 63 | Anyone proposes; agree, retire and promote need a human at a terminal (`requireHuman`). Retire keeps who, when and why, searchable |
| 64 | Brief drop order: lessons, todo lines, proposals past the first of each kind, active convention lines, promoted lesson lines, the last proposals; every cut rule or proposal is counted in a `+N more` line. At most 10 open proposals; a retired rule and scope is refused |
| 65 | Promotion counts different tasks (`lesson_hits`); a lesson with any linked convention is not a candidate (a retired one means declined) |
| 66 | The SessionStart brief carries context.md's rules in every task worktree, in both modes (a worktree's import may be unapproved); no CLAUDE.local.md import |
| 67 | Conflict check: clause-and-polarity word overlap, deterministic; `agree` refuses while a conflict stands unless `--force` |
| 68 | Managed block: appended once, bytes outside the markers kept, CRLF and final newline kept, malformed markers and symlinks refused with exit 3; fenced markers ignored, a BOM kept |
| 69 | `requireHuman`: stdin and stdout real terminals (isatty), and no Claude Code session (`CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT`); pty wrappers remain a documented residual |
| 70 | Rule text in context.md is inert (word-initial `@`, `<!--`, `-->` broken) and in a code span; agree and promote print the line and ask `y/N` |
| 71 | One CommonMark §4.5 fence scanner; an unclosed fence runs to the end and init refuses to append inside it |
| 72 | Conventions compare by `RuleKey` (case, whitespace, trailing sentence punctuation only; no NFC) for duplicates and retired refusals |
| 73 | `promote` checks conflicts before asking; with one, the prompt offers a proposal, never a promotion |
| 74 | Hook latency bound 1.5 s: p95 of 15 plain, median of 5 (early stop at 3) under -race |
| 75 | A shoal marker counts only at column 0 (after a BOM on line 1), outside a tracked fence; any leading whitespace is plain text (round 4; round 3 allowed 0-3 spaces) |
| 76 | An unpaired begin marker hides no rules from ruleLines; init still refuses the file |

## Measurements

Hook latency (TestPhase3HookLatency, now seeded with 20 lessons, 20 active and 5 proposed conventions; 15 runs, process start included, macOS): p50/p95 stop 5.2/5.6 ms, session-start 6.0/6.9 ms, pre-tool-use 17.8/20.4 ms, no-match 5.1/5.6 ms, post-tool-use 5.0/5.3 ms. Phase 3 p95: 5.1, 5.7, 19.3, 5.2, 5.3 ms.

`go test -race -count=1 ./internal/cli`: 404.8 s at `c789a52` (run alone), 451.1 s at `6781ccc` (inside the full `./...` run of the fresh-clone gate, packages in parallel). The Phase 4 tests add about 13 s without `-race`. RISK: above 450 s, about 75% of Go's default 10-minute `go test` timeout; CI config was not touched. After Vex round 2 (`a98f1b6`, fresh clone, full `./...` run): 384.5 s. Superseded by "Test speed" below: about 148 s.

## Test speed (2026-11-09)

Goal: `go test -race -count=1 ./internal/cli` at most 240 s on an idle Mac, with test-code changes only (CI workflows untouched). Every timing here was taken alone on the machine under `caffeinate -i`.

**Cause.** At `1af5516`, run alone with `-timeout 30m`, the package passed in 410.5 s: no test hangs or polls to a long timeout, and no single test dominates (172 tests; the slowest, `TestPhase3HookLatency`, 27.1 s; 86 tests take 2-10 s each). Round 4 added nothing slow. The 10-minute timeouts came from runs sharing the machine (the full `./...` gate runs packages in parallel, about 451 s even alone) or a Mac sleeping mid-run: 410 s leaves little headroom. The time goes to subprocesses, not the race detector: the same tests take about 75% as long without `-race`, and the test process is busy only 28-44% of the wall time. One e2e test runs about 250 git commands, and on macOS `/usr/bin/git` is an xcrun trampoline: 10.5 ms a call against 3.4 ms for the real binary. Every test ran serially because `testutil.Isolate` uses `t.Setenv`.

**Changes** (`e2d23ff`, `455a2cd`, `b2e623d`):

1. `TestMain` puts a symlink to the real git (`xcrun -f git`) first on PATH when `git` resolves to `/usr/bin/git` (macOS only; no other tool from the developer directory goes on PATH). 410.5 s -> 264.0 s.
2. Per-test environment instead of `t.Setenv`: `Env.Getenv` (nil = `os.Getenv`/`os.UserHomeDir`, so production is unchanged) is the one place shoalctl reads HOME/USERPROFILE, SHOAL_HOME, CLAUDE_CONFIG_DIR, CLAUDE_PROJECT_DIR, CLAUDECODE, CLAUDE_CODE_ENTRYPOINT and TMUX. The harness keeps `testutil.IsolatedEnv(home)` per test (`harness.vars`, `h.setenv`), hands a copy to every in-process run and appends it to subprocess env. `TestMain` isolates the process environment once (`testutil.IsolateProcess`: fake home, isolated git identity, idle SHELL for tmux), so git, tmux and `testutil.Git` never see the real home. `testutil.Repo`'s guard is unchanged: it still fails unless the isolated identity is in the environment, so no test can commit with the host identity. Serial: 262.0 s.
3. `t.Parallel` on 145 of the 172 top-level tests and on the per-case subtests of `TestStartTrustPreflight`, `TestHookFailsOpen`, `TestRequireHumanRefusesClaudeCodeSession` and `TestPhase2ConcurrentPlansOneWinner`. 27 tests stay serial (Go runs them before the parallel batch), for one of two reasons. They change process state: `t.Setenv` of PATH wrappers or the watcher's spawn env, `testutil.Isolate`, or the package vars `logHook` and `indexLockWait`. Or they assert on timing or a short window: `TestPhase3HookLatency`, the hook deadline tests, the concurrent-start tests, all of `interrupt_unix_test.go` and `watch_test.go`. 154.4 s: a 77.6 s serial phase, then the parallel batch.

No fixed sleep, poll interval or fixture size changed. The remaining sleeps hold a process inside a window (the 2 s concurrent-start suite, the doctor test's 2 s suite) or check that nothing happens within one (300 ms), and they run serially. No assertion changed.

| Test(s) | Change | Why the intent is kept | Before -> after (s, -race) |
|---|---|---|---|
| All `internal/cli` tests | Real git on PATH in `TestMain` (macOS) | Same git version and behaviour, minus the xcrun lookup; no test checks which git binary runs | package 410.5 -> 264.0 |
| 144 harness and unit tests | `t.Parallel()` | Each owns its temp dirs, repo, tmux server (unique `-L` name; the unused one now hashes the temp path) and env map; none touches package vars, cwd, umask or signal handlers | package 262.0 -> 154.4 |
| `TestHookHeartbeat`, `TestHookSilentOutsideShoalWorktrees`, `TestHookFailsOpen`, `TestHookDeadline`, `TestHookRecoversPanic`, `TestHookUnwritableLogStillExitsZero`, `TestStatusWarnsAboutNewHookFailures`, `TestPhase3*` via `inHook`, `TestPhase3CompactionReinjectsLessons`, `TestPhase3HooksNeverDoubleFire`, `TestPhase3HookLatency` | `t.Setenv("CLAUDE_PROJECT_DIR")` -> `h.setenv` | Main reads the same value through `Env.Getenv`; `h.command` subprocesses get it in `cmd.Env` | see the package rows |
| `TestRequireHumanRefusesClaudeCodeSession` | `t.Setenv(CLAUDECODE/…)` -> `h.setenv` | `requireHuman` and `interactive` read the variable through `Env.Getenv` | as above |
| `TestStartHonoursClaudeConfigDir`, `TestStartWarnsWhenTheSocketLookupFailsAfterLaunch` | `CLAUDE_CONFIG_DIR`, `TMUX` via `h.setenv` | Same lookups, through `Env.Getenv` | as above |
| `TestContextImport` | HOME through `Env{Getenv: …}` instead of `t.Setenv`; parallel | Same home and both assertions | as above |
| `TestHookDeadlineBoundedWithBlockedLogWriter`, `TestHookErrorBoundedWithBlockedLogWriter` | `logHook` stub takes the new `Env` argument | Still blocks every log write; still serial | as above |

Top 10 at `1af5516` (run 1), after the git change (run 2) and after `t.Parallel` (run 3). In run 3 a parallel test's elapsed time includes waiting for the CPU behind 11 others, so only the package total compares. `TestStartTrustPreflight` reports 0.0 because its subtests run after the parent returns.

| Test | 1af5516 | git | parallel |
|---|---|---|---|
| `TestPhase3HookLatency` | 27.1 | 21.0 | 21.5 (serial) |
| `TestPhase3HooksNeverDoubleFire` | 10.3 | 9.0 | 16.5 |
| `TestPhase2DoctorRecoversKilledStartAndStaleClaim` | 9.7 | 8.3 | 16.9 |
| `TestPlanSeesBracketedRouteDirectories` | 8.9 | 5.0 | 34.3 |
| `TestStartTrustPreflight` | 7.5 | 5.3 | 0.0 (subtests parallel) |
| `TestStartRefusesUnparseableStartTime` | 5.8 | 4.2 | 4.6 (serial) |
| `TestPhase2AttentionFlag` | 5.8 | 5.2 | 5.2 (serial) |
| `TestRebaseKilledBySignalIsInterrupt` | 5.7 | 3.9 | 4.4 (serial) |
| `TestPhase3CompactionReinjectsLessons` | 5.5 | 3.2 | 11.3 |
| `TestPlanSeesWildcardReachingIntoALiteralDirectory` | 5.5 | 3.2 | 19.4 |

Flakiness check at `b2e623d` (default timeout): 148.4 s, 148.2 s, 145.1 s back to back; `-shuffle=on`: 145.2 s with seed 1794212192046239000 (an earlier shuffled run, seed not captured, 142.6 s). All passed. `go test -count=1 ./...` without race: every package ok, `internal/cli` 82.4 s.

Not changed (production behaviour, out of scope): on a fresh `memory.db`, `store.migrate` runs a `VACUUM INTO` backup before every migration after the first, which is about 60% of `init`'s 0.38 s under `-race`; skipping backups when the starting version was 0 would save about 0.2 s per test. CI runners have 2-4 cores, so the parallel batch gains less there than on this 12-core Mac.

## Open risks

- `internal/cli` under `-race` was about 410-451 s, near the 600 s default timeout; now about 148 s alone on a 12-core Mac (see Test speed). On a 2-4 core CI runner the parallel batch gains less; not measured there.
- The conflict heuristic misses paraphrases, synonyms, "prefer A over B", "X not Y" without a comma and nested CLAUDE.md files, and can flag unrelated lines that share two words with opposite polarity (spec §5.3). A false positive costs a brainstorm and `agree --force`.
- `context.md` is rendered by each command from the DB it read; two humans agreeing at the same instant could leave the older render last. The brief reads the DB directly, so only the non-stealth import can lag, until the next agree, retire, promote or `init`.
- Non-stealth: if a new task worktree shows Claude Code's external-import dialog, an unattended session waits on it; the brief carries the rules, but the dialog itself needs an answer in the tmux window. Whether a worktree reuses the main checkout's approval is unconfirmed.
- `requireHuman` is a guard rail, not a sandbox: a pty wrapper (`script(1)`, `expect`) fakes a terminal and an agent can unset `CLAUDECODE`; the `y/N` prompts of agree and promote need a deliberate answer on top.
- `context.md` has no size cap; a project with hundreds of agreed rules makes every non-stealth session pay for them (the stealth brief caps them under its budget).

# Phase 3 progress

Base: `9b3e7ad`. Acceptance bar: spec §15 Phase 3 row plus the two Phase 2 deferrals.

## Where I am

- Done: migration 0006 (bugs, decisions, lessons, qa, hook_marks, memory_fts); `record`, `flag-stale`,
  `why`, `recall`, `brief`; the SessionStart, PreToolUse and PostToolUse handlers; init registers the
  tool hooks with matchers; late overlap orders the merge.
- E2E (`internal/cli/phase3_e2e_test.go`): TestPhase3BriefUnderBudget, TestPhase3LessonInjectedOncePerSession,
  TestPhase3WriteOutsideClaimsWarns, TestPhase3LateOverlapFromEdits, TestPhase3HooksNeverDoubleFire,
  TestPhase3HookLatency, TestPhase3WhyBugCameBack, TestPhase3MergeTaskAheadOfStuckHolder,
  TestPhase3CompactionReinjectsLessons; `brief_test.go`: TestPhase3BriefUnderBudgetCJKAndBlobs;
  TestPhase2BugABugB covers spec §14 step 5.
- Decisions: spec §16 rows 58-62.
- Vex HOLD round 1 on `aff9059`: five items fixed test-first, one commit each (table below).
- Next: Vex re-review. Phase 4 not started.

## Vex HOLD round 1

Vex returned HOLD on `aff9059` with five items; four fixed test-first in their own commits, the fifth is a documented limit.

| Item | Fix | Test |
|------|-----|------|
| REQUIRED: the stuck-holder escape (`merge --task X` past a holder E that never finishes) was untested | Plain `merge`'s skip line names `shoalctl merge --task <writer>`. `merge --task` already merged X and moved E to `needs-rebase`, but left X's own `task_blockers` row and `blocked_by`; `UnblockAfterMerge` now drops a merged task's own waits | `TestPhase3MergeTaskAheadOfStuckHolder` |
| `approxTokens` counted a CJK character as 1/3 token and a hash as one word: CJK lessons passed at 819 estimated while bytes/3 said 1218 | Han, Hiragana, Katakana and Hangul runes count one token each; a run of more than 16 letters or digits counts one per 2 characters (spec §4.6 "Token measurement") | `TestApproxTokensCountsCJKAndBlobs`, `TestPhase3BriefUnderBudgetCJKAndBlobs` (6 lessons of 600 CJK characters plus hex lessons, title and ask: 797 estimated, bytes/3 598) |
| The brief picked the 5 oldest matching lessons | Matches sorted newest first, ties by id descending, then capped at 5 (brief and `brief.md`) | `TestTaskLessonsNewestFirst` (7 lessons), `TestPhase3BriefUnderBudget`, `TestPhase3BriefUnderBudgetCJKAndBlobs` |
| PreToolUse marks survived compaction, so dropped lessons were never re-injected | `SessionStart` with source `compact` or `clear` turns the session's `lesson:`/`decision:`/`bug:` marks into `shown:<key>`; records inject again and `Mark` skips the hit for a key already shown. Warnings stay once per session. Only the registration that wins `FireOnce` resets | `TestPhase3CompactionReinjectsLessons` (two concurrent compact registrations answer once), `TestResetInjectedReinjectsWithoutAnotherHit` |
| Every Phase 3 e2e skips on Windows via `withSession()` | Not ported: the harness's fake `tmux` and `claude` are sh scripts. Only CI on a Windows runner can prove the hooks there; the store tests (`TestResetInjectedReinjectsWithoutAnotherHit`, `TestMarkCountsLessonHitsOncePerSession`) and `TestApproxTokensCountsCJKAndBlobs`, `TestTaskLessonsNewestFirst` do run on Windows | none on Windows |

Hook latency after the fixes (15 runs, process start included, macOS): p95 stop 5.1 ms, session-start 5.7 ms, pre-tool-use 19.3 ms, no-match 5.2 ms, post-tool-use 5.3 ms; before: 5.4, 6.2, 18.5, 5.5, 5.7 ms.

Judgment calls: `clear` resets marks too, since `/clear` drops context the same way. A session that was mid-flight across the upgrade has no `shown:` marks, so its first post-compaction re-injection counts one extra hit.

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

## Vex round 2 residuals

Vex returned SHIP on `1548f0b` with two residual risks; each closed in its own commit.

| Item | Fix | Test |
|------|-----|------|
| `approxTokens` undercounted Thai (and other no-space scripts outside the CJK ranges) and emoji runs | The estimate is the largest of ceil(UTF-8 bytes / 3), the character count and the piece count, inside `approxTokens`, so the brief budget and the per-file injection budget both use it. Spec §4.6 token measurement updated. No existing budget test changed its expected content | `TestApproxTokensAtLeastBytesOverThree` |
| `340b6be` removed the only assertion that a stack-wide lesson reaches the brief | Unit coverage restored: a recent stack-wide lesson is shown in date order, an old one is cut by the 5-lesson limit, one of another stack is never shown | `TestTaskLessonsNewestFirst` |
