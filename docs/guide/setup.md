Set up a project. Do this once per repository.

1. Install the agent team (once per machine, not per project):
      shoalctl agents install
   It writes the shoal-* agents to ~/.claude/agents/ (with --commit: the
   repo's .claude/agents/ instead).
2. Trust the repo in Claude Code. In the repo's main checkout run claude,
   choose "Yes, I trust this folder", then exit.
3. Register the project:
      cd your-repo
      shoalctl init
   It wires the hooks. Stealth by default, so nothing is committed. init
   prints the project id and the config path,
   ~/.shoal/projects/<id>/config.toml. Keep the id: export and purge take it.
4. Set the test command. Open the config (for example on macOS:
   open -e ~/.shoal/projects/<id>/config.toml) and set command and format
   under [tests]. init fills them in only for Go. For the value per stack
   run: shoalctl guide tests
5. Add setup if the tests need ignored files. If the suite needs .env,
   vendor/, node_modules/ or a virtualenv, set [tests].setup too, for example
      setup = "cp {root}/.env . && composer install"
6. Check it:
      shoalctl doctor
   It checks hooks, DB, stuck tasks and runs; --fix repairs what is safe.
7. Open tmux. Run shoal from a terminal inside tmux: start opens each task
   in its own tmux window.

Then run: shoalctl guide flow
