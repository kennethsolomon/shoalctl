Task lifecycle. <task> is the number or the slug that new printed.

1. shoalctl new <feature|bugfix|hotfix> "<title>"   create the task
2. shoalctl plan <task> <glob>...   optional: expected files; asks on
   overlap with another task (s/m/p/?)
3. shoalctl start <task>   branch, worktree, baseline test run, tmux
   window, claude and the attention watcher
4. shoalctl status   the board. When the agent says it is finished, exit
   claude in its window with /exit.
5. shoalctl qa <task>   one QA round: contract, suite vs baseline, lint,
   build. After 3 rounds the task is escalated.
6. shoalctl done <task>   final run and checks, summary and report, then
   queued for merge
7. shoalctl merge   merges queued tasks in order through the gate; never
   pushes
8. shoalctl gc   closes merged tasks' windows, removes their worktrees

Feature: every new behaviour needs a new passing test or a decided manual
check. See the case matrix with: shoalctl scope show <task>

Bugfix: prove the bug, then the fix.
  1. Commit a regression test that fails without the fix.
  2. shoalctl repro red <task> --test <id>     must fail (exit 9 if not)
  3. Fix the bug and commit.
  4. shoalctl repro green <task> --test <id>   must pass on a later commit
  done refuses (exit 10) without the red/green proof.

Hotfix: the bugfix steps, with three differences.
  - The branch starts from [hotfix].production_ref (a branch, a tag, or
    tag:<glob>), resolved locally; shoal never fetches.
  - Minimal diff: at most [hotfix].max_diff_lines (default 100) added plus
    removed lines outside test files, else done refuses unless you pass
    shoalctl done <task> --accept-large-diff --reason "<why>"
  - Full QA, no shortcuts.
  With [hotfix].backport = true (the default), done creates a bugfix task
  "Backport: <title>" to bring the fix to [merge].base.
