Task lifecycle, worked through one bug. <task> is the number or the slug.

1. shoalctl new bugfix "POST /api/orders saves 150 instead of 150.50" \
     --details "Paid 150.50, the order shows 150. Keep the cents."
   prints 1-post-api-orders-saves-150-instead-of-150, so the task is 1.
   Lanes: feature, bugfix, hotfix. --details-file <path> reads the details
   from a file; the old names --ask and --ask-file still work.
2. Optional: shoalctl plan 1 'app/Orders/**'   expected files; asks on
   overlap with another task (s/m/p/?)
3. shoalctl start 1   branch, worktree, baseline test run, tmux window,
   claude and the attention watcher
4. The agent proves the bug, then the fix (shoalctl status: in-progress):
   commits TestOrderKeepsCents, which fails without the fix, then runs
     shoalctl repro red 1 --test TestOrderKeepsCents     must fail, else exit 9
   fixes the rounding and commits again, then runs
     shoalctl repro green 1 --test TestOrderKeepsCents   must pass, new commit
   and writes .shoal/summary.json. It tells you when it is finished.
5. /exit in its window. Optional: shoalctl qa 1   one QA round: contract,
   suite vs baseline, lint, build. After 3 rounds the task is escalated.
6. shoalctl done 1   final run and checks, summary and report, queued for
   merge. Refuses (exit 10) without the red/green proof.
7. shoalctl merge   merges queued tasks in order through the gate; never
   pushes. shoalctl gc   closes merged tasks' windows and worktrees.
Any time before merge: shoalctl abandon 1 --reason "<why>" gives the
task up (asks y/N; status --all still lists it; branch and records stay)

Feature: every new behaviour needs a new passing test or a decided manual
check. See the case matrix with: shoalctl scope show <task>

Hotfix: the bugfix steps, with three differences.
  - The branch starts from [hotfix].production_ref (a branch, a tag, or
    tag:<glob>), resolved locally; shoal never fetches.
  - Minimal diff: at most [hotfix].max_diff_lines (default 100) added plus
    removed lines outside test files, else done refuses unless you pass
    shoalctl done <task> --accept-large-diff --reason "<why>"
  - Full QA, no shortcuts.
  With [hotfix].backport = true (the default), done creates a bugfix task
  "Backport: <title>" to bring the fix to [merge].base.
