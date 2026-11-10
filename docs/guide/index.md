shoalctl guide: the short manual. Each topic below fits one screen.

1. First-time setup, once per repo (more: shoalctl guide setup)
   shoalctl agents install     install the shoal-* agents, once per machine
   claude                      in the repo: trust the folder, then exit
   shoalctl init               register this repo and wire the hooks
   Edit the config file and set [tests] (see: shoalctl guide tests):
      {config}
   shoalctl doctor             check that everything is wired
   tmux                        run shoal from a terminal inside tmux

2. Daily task flow (more: shoalctl guide flow)
   shoalctl new feature "<title>"   create a task; prints its slug
   shoalctl plan <task> <glob>...   optional: files you expect to touch
   shoalctl start <task>            worktree, baseline run, window, claude
   shoalctl status                  board of all tasks
   /exit                            in the task window, when claude is done
   shoalctl qa <task>               one QA round (escalated after 3)
   shoalctl done <task>             final checks and report; queues merge
   shoalctl merge                   merge queued tasks via the gate; no push
   shoalctl gc                      close merged tasks' windows, worktrees
   <task> is the number or the slug that new printed.

3. When something is wrong
   shoalctl doctor                  find problems (exit 11 while one stays)
   shoalctl doctor --fix            repair what is safe
   shoalctl status                  see which task is stuck or blocked
   shoalctl abandon <task>          give up on a task that has not merged
   shoalctl why <file>              stored reasons plus git log for a file
   shoalctl recall <query>          search lessons, decisions, bugs, answers

Topics:
   shoalctl guide setup             full setup, step by step
   shoalctl guide tests             choosing [tests].command and format
   shoalctl guide flow              task lifecycle, bugfix and hotfix rules
   shoalctl guide memory            record, why, recall, conventions, promote
