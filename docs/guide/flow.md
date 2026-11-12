Task lifecycle, worked through one bug. <task> is the number or the slug.

1. shoalctl new bugfix "POST /api/orders saves 150 instead of 150.50" \
     --details "Paid 150.50, the order shows 150. Keep the cents."
   prints 1-post-api-orders-saves-150-instead-of-150, so the task is 1.
   Lanes: feature, bugfix, hotfix. --details-file <path> reads the details
   from a file; the old names --ask and --ask-file still work.
2. Optional: shoalctl plan 1 'app/Orders/**'   expected files; asks on overlap
3. shoalctl start 1   branch, worktree, baseline test run, tmux window,
   claude and the attention watcher
4. The agent proves the bug, then the fix (shoalctl status: in-progress).
   It commits a test that fails without the fix, then runs
     shoalctl repro red 1 --file orders/order_test.go \
       --test example.com/shop/orders::TestOrderKeepsCents
   The id is the test as the report names it: package::Test (go-json) or
   classname::name (junit). Red must fail: a pass is exit 9, an id not in
   the report exit 4. It fixes the rounding, commits, then runs
     shoalctl repro green 1 --test example.com/shop/orders::TestOrderKeepsCents
   (green must pass), writes .shoal/summary.json, tells you it is finished.
5. /exit in its window. Optional: shoalctl qa 1   one QA round: contract,
   suite vs baseline, lint, build; escalated after 3. status QA then reads
   pass 1/3, fail 1/3, or stale 1/3 once the code changes (qa it again).
6. shoalctl done 1   final run and checks, summary and report, queued for
   merge. Refuses (exit 10) without the red/green proof.
7. shoalctl merge   merges queued tasks in order through the gate; never
   pushes. shoalctl gc   closes merged tasks' windows and worktrees.
Any time before merge, shoalctl abandon 1 --reason "<why>"  gives the
task up (asks y/N; status --all still lists it; branch and records stay)

Feature: every new behaviour needs a new passing test or a decided manual
check. See the case matrix with: shoalctl scope show <task>

Hotfix: the bugfix steps and QA, with three differences.
  - It branches from [hotfix].production_ref (a branch, a tag or
    tag:<glob>; never fetched) and merges back into it (a tag: merge skips).
  - Minimal diff: at most [hotfix].max_diff_lines (default 100) added plus
    removed lines outside test files, else done refuses unless you pass
    shoalctl done <task> --accept-large-diff --reason "<why>"
  - With [hotfix].backport = true (the default), done creates a bugfix task
    "Backport: <title>" to bring the fix to [merge].base.
