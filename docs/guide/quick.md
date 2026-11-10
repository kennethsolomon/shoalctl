shoalctl guide: fix a bug in 5 commands. <n> is the task number.

1. shoalctl new bugfix "<title>"   prints the slug, e.g. 1-<title-words>
2. shoalctl start <n>   a tmux window opens with claude. Its first line,
   "Read .shoal/brief.md first...", is shoal giving claude its task: leave it.
3. Wait. The agent commits a failing test, runs shoalctl repro red, fixes
   the bug, runs shoalctl repro green. Check with shoalctl status: STAGE
   stays in-progress; NOTE says NEEDS ATTENTION when it waits for you.
4. When it says it is finished: /exit in its window, then shoalctl done <n>
   (no shoalctl qa needed; done refuses if the red/green proof is missing)
5. shoalctl merge   merges done tasks locally; never pushes
Everything else is optional.
