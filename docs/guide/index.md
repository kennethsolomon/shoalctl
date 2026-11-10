
Setup, once per repo (more: shoalctl guide setup)
   shoalctl agents install     install the shoal-* agents, once per machine
   claude                      in the repo: trust the folder, then exit
   shoalctl init               register this repo and wire the hooks
   Edit the config file and set [tests] (see: shoalctl guide tests):
      {config}
   shoalctl doctor             check that everything is wired
   tmux                        run shoal from a terminal inside tmux

When something is wrong
   shoalctl doctor [--fix]          find problems; --fix repairs the safe ones
   shoalctl status                  see which task is stuck or blocked
   shoalctl abandon <task>          give up on a task that has not merged
   shoalctl why <file>              stored reasons plus git log for a file
   shoalctl recall <query>          search lessons, decisions, bugs, answers

Optional. <task> is the number or the slug (more: shoalctl guide flow)
   shoalctl plan <task> <glob>...   before start: files you expect to touch
   shoalctl qa <task>               one QA round (escalated after 3)
   shoalctl gc                      close merged tasks' windows, worktrees

Topics:
   shoalctl guide setup             full setup, step by step
   shoalctl guide tests             choosing [tests].command and format
   shoalctl guide flow              worked bugfix example, feature, hotfix
   shoalctl guide memory            record, why, recall, conventions, promote
