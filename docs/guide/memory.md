Memory: the WHY that outlives a task. Each record is stamped with HEAD.

Record while you work (agents usually do this):
  shoalctl record lesson <task> --pattern "<what went wrong>" --fix "<fix>"
     tied to the newest failed QA round; --glob <glob> ties it to files,
     without one it applies to the whole stack (brief only)
  shoalctl record decision <task> --topic "<t>" --choice "<c>" --why "<w>"
     add --file <path> (repeatable) for the files it is about
  shoalctl record qa <task> --question "<q>" --answer "<a>" [--file <path>]
  done also writes one bug record per finished bugfix or hotfix.

Look things up:
  shoalctl why <file>              stored WHY plus live git log for a file
  shoalctl why <sha>               the same for a commit
  shoalctl why lesson:<id>         one record: bug, decision, lesson, qa,
                                   convention
  shoalctl recall <query>          full-text search over bugs, decisions,
                                   lessons, answers, summaries, conventions
  shoalctl recall --file <path>    every record about one file

A record no longer matches the code:
  shoalctl flag-stale lesson:<id> "<reason>"
     kinds: bug, decision, lesson, qa. Still searchable, never injected.

Rules from now on (conventions):
  shoalctl convention propose --rule "<rule>" --scope <glob>
     anyone can propose; the next session opens with a brainstorm about
     its scope and exceptions
  shoalctl convention list         what waits for you, and the rules
  shoalctl convention agree <id>   human only: the rule goes live
  shoalctl convention retire <id> --why "<reason>"   human only

Promote a lesson:
  A lesson shown in 3 different tasks is proposed for promotion.
  shoalctl promote lesson:<id>     human only: shows the line, asks y/N,
                                   then writes the rule into context.md
