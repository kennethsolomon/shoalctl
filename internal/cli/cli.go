// Package cli implements the shoalctl commands of the Phase 0 MVP (spec §15.1).
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// Exit codes (spec §4.7).
const (
	ExitOK         = 0
	ExitInternal   = 1
	ExitUsage      = 2
	ExitConfig     = 3
	ExitNotFound   = 4
	ExitState      = 5
	ExitBaseline   = 6
	ExitDependency = 7
	ExitBusy       = 8
	// ExitGate (9) is declared with the gate in gate.go.
	ExitDoneRefused = 10
	ExitDoctor      = 11
	ExitUntrusted   = 12
	// ExitInterrupted follows the shell convention of 128 + SIGINT.
	ExitInterrupted = 130
)

// Env holds everything a command touches outside its arguments, so tests can
// isolate it.
type Env struct {
	Dir      string // working directory the command runs from
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Exe      string   // absolute path of this binary, written into hook entries
	Claude   string   // claude binary name or path
	Tmux     string   // tmux binary name or path
	TmuxArgs []string // global tmux args, e.g. -L <socket>
	// Terminal reports whether a human is at the keyboard; nil means the
	// real check: stdin and stdout are both terminals. Waivers and manual
	// checks need one (§8.6).
	Terminal func() bool
	// SpawnWatcher starts the attention watcher of a started task; nil runs
	// `<Exe> watch <task>` detached.
	SpawnWatcher func(p *project, t store.Task) error
	// Getenv reads the environment variables shoalctl itself consults
	// (HOME, SHOAL_HOME, CLAUDE_*, TMUX); nil means os.Getenv. Subprocesses
	// still inherit the process environment. Tests set it so they can run in
	// parallel without changing the process environment.
	Getenv func(key string) string
}

func (env Env) getenv(key string) string {
	if env.Getenv != nil {
		return env.Getenv(key)
	}
	return os.Getenv(key)
}

// userHomeDir is os.UserHomeDir, reading the variable through Getenv when
// that is set.
func (env Env) userHomeDir() (string, error) {
	if env.Getenv == nil {
		return os.UserHomeDir()
	}
	key := "HOME"
	if runtime.GOOS == "windows" {
		key = "USERPROFILE"
	}
	if v := env.Getenv(key); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("$%s is not defined", key)
}

// claudeCodeEnv are the variables Claude Code sets in the shells it runs
// for an agent (its Bash tool): a process that sees one is an agent's.
var claudeCodeEnv = []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"}

// claudeCodeSession returns the Claude Code variable set in this process's
// environment, or "".
func (env Env) claudeCodeSession() string {
	for _, k := range claudeCodeEnv {
		if env.getenv(k) != "" {
			return k
		}
	}
	return ""
}

// interactive reports whether a human is at a terminal: not inside a Claude
// Code session, and stdin and stdout are terminals (a character device such
// as /dev/null is not one). A pty wrapper such as script(1) still passes;
// see §16 row 69.
func (env Env) interactive() bool {
	if env.claudeCodeSession() != "" {
		return false
	}
	if env.Terminal != nil {
		return env.Terminal()
	}
	return isTerminal(env.Stdin) && isTerminal(env.Stdout)
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}

// DefaultEnv is the environment of a real invocation.
func DefaultEnv() (Env, error) {
	dir, err := os.Getwd()
	if err != nil {
		return Env{}, fmt.Errorf("working directory: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return Env{}, fmt.Errorf("locate shoalctl binary: %w", err)
	}
	// Hook entries name this resolved path, so a symlink swapped later (for
	// example by a package manager) does not change what they run.
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return Env{}, fmt.Errorf("resolve shoalctl binary path: %w", err)
	}
	return Env{Dir: dir, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Exe: exe, Claude: "claude", Tmux: "tmux"}, nil
}

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func fail(code int, format string, a ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, a...)}
}

func withCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

// codeOf maps an error to its exit code. Store and lock sentinels map to their
// spec codes; anything unclassified is an internal error.
func codeOf(err error) int {
	var ee *exitError
	switch {
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, store.ErrSchemaTooNew), errors.Is(err, store.ErrSchemaChanged),
		errors.Is(err, gitx.ErrNotRepo), errors.Is(err, gitx.ErrBareRepo):
		return ExitConfig
	case errors.Is(err, store.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, store.ErrStage), errors.Is(err, store.ErrProposalRefused):
		return ExitState
	case errors.Is(err, store.ErrBusy), errors.Is(err, store.ErrConflict), errors.Is(err, lockfile.ErrHeld):
		return ExitBusy
	}
	return ExitInternal
}

const usage = `New here? Run: shoalctl guide
usage: shoalctl <command> [args]

commands:
  init [--commit-config] [--rebind]
  new <feature|bugfix|hotfix> "<title>" [--ask "<text>" | --ask-file <path>] [--scope "<text>"]...
  plan <task> [--todo <file>] <glob>...    expected file claims; asks on overlap (s/m/p/?)
  start <task> [--model <model>]
  status [--json] [--all]                  --all: every task, abandoned and long-closed ones too
  test <task> [--kind gate|qa|baseline]
  test quarantine list [--all] | release <test>
  qa <task>
  done <task> [--force] [--accept-large-diff --reason "<text>"]
  merge [--task <task> [--accept-regression <target> --reason "<text>"]...]
  repro red|green <task> --test <id> [--file <path>]
  scope add <task> [--behaviour] "<text>"
  scope case <task> <item-id> --category <c> --desc "<text>" (--test <id> | --manual "<step>" | --na "<reason>")
  scope show|review <task>
  scope check <task> <case-id> --pass|--fail [--note "<text>"]
  agents install [--commit] | agents list
  contract freeze <task> [--as <agent>] [--note "<what changed>"]
  claim <task> --as <agent> <glob>...
  release <task> --as <agent> [<glob>...]
  msg send <task> --from <agent> --to <agent|*> --kind <kind> [--subject "<s>"] [--reply-to <id>] "<body>"
  msg inbox <task> --as <agent> [--all]
  brief [<task>]                           the session-start brief (default: this worktree's task)
  record lesson <task> --pattern "<what went wrong>" --fix "<what fixed it>" [--glob <glob>]
  record decision <task> --topic "<t>" --choice "<c>" --why "<rationale>" [--file <path>]...
  record qa <task> --question "<q>" --answer "<a>" [--file <path>]...
  flag-stale <bug|decision|lesson|qa>:<id> "<reason>"
  why <file | sha | bug:<id> | decision:<id> | lesson:<id> | qa:<id> | convention:<id>>
  recall <query> | recall --file <path>
  convention propose [<task>] --rule "<text>" --scope <glob>
  convention agree <id> [--rule "<text>"] [--scope <glob>] [--force]   human only
  convention retire <id> --why "<reason>"                               human only
  convention list [--all]
  promote lesson:<id>      human only: a lesson shown in 3 tasks becomes a project rule (context.md)
  doctor [--fix]      check shoal's state; --fix repairs what is safe (exit 11 while a problem remains)
  watch <task>        internal: the attention watcher start launches
  abandon <task> [--reason "<text>"] [--force]   human only: give up on a task that has not merged
  gc                  close merged tasks' windows, remove their worktrees (git-ignored files such as .env go too) and working rows; flag records whose files are gone
  export <project-id> [--out <file>]   every record and report of a project, as JSON
  purge <project-id>  human only: delete the project's data under $SHOAL_HOME (asks for the id)
  uninstall           remove shoal's hook entries, the CLAUDE.md block and unedited shoal-* agents; nothing else
  guide [<topic>]     the step-by-step manual; topics: setup, tests, flow, memory
  version
`

// Main runs one shoalctl invocation and returns its exit code.
func Main(args []string, env Env, version string) int {
	if len(args) > 0 && args[0] == "hook" {
		return runHook(args[1:], env)
	}
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	// SIGINT, SIGTERM and SIGHUP (the terminal closed) cancel ctx instead of
	// killing shoalctl, so a running suite is stopped and recorded as
	// cancelled (testrun forwards the signal to the suite's process group).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	var err error
	switch args[0] {
	case "version", "--version":
		if len(args) > 1 {
			err = fail(ExitUsage, "%s takes no arguments, got %q", args[0], args[1:])
		} else {
			fmt.Fprintln(env.Stdout, "shoalctl "+version)
		}
	case "init":
		err = env.cmdInit(ctx, args[1:])
	case "new":
		err = env.cmdNew(ctx, args[1:])
	case "plan":
		err = env.cmdPlan(ctx, args[1:])
	case "start":
		err = env.cmdStart(ctx, args[1:])
	case "status":
		err = env.cmdStatus(ctx, args[1:])
	case "done":
		err = env.cmdDone(ctx, args[1:])
	case "doctor":
		err = env.cmdDoctor(ctx, args[1:])
	case "watch":
		err = env.cmdWatch(ctx, args[1:])
	case "test":
		err = env.cmdTest(ctx, args[1:])
	case "merge":
		err = env.cmdMerge(ctx, args[1:])
	case "qa":
		err = env.cmdQA(ctx, args[1:])
	case "repro":
		err = env.cmdRepro(ctx, args[1:])
	case "scope":
		err = env.cmdScope(ctx, args[1:])
	case "agents":
		err = env.cmdAgents(ctx, args[1:])
	case "msg":
		err = env.cmdMsg(ctx, args[1:])
	case "contract":
		err = env.cmdContract(ctx, args[1:])
	case "claim":
		err = env.cmdClaim(ctx, args[1:])
	case "release":
		err = env.cmdRelease(ctx, args[1:])
	case "brief":
		err = env.cmdBrief(ctx, args[1:])
	case "record":
		err = env.cmdRecord(ctx, args[1:])
	case "flag-stale":
		err = env.cmdFlagStale(ctx, args[1:])
	case "why":
		err = env.cmdWhy(ctx, args[1:])
	case "recall":
		err = env.cmdRecall(ctx, args[1:])
	case "convention":
		err = env.cmdConvention(ctx, args[1:])
	case "promote":
		err = env.cmdPromote(ctx, args[1:])
	case "uninstall":
		err = env.cmdUninstall(ctx, args[1:])
	case "abandon":
		err = env.cmdAbandon(ctx, args[1:])
	case "gc":
		err = env.cmdGC(ctx, args[1:])
	case "export":
		err = env.cmdExport(ctx, args[1:])
	case "purge":
		err = env.cmdPurge(ctx, args[1:])
	case "guide":
		err = env.cmdGuide(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
	default:
		fmt.Fprintf(env.Stderr, "shoalctl: unknown command %q\n%s", args[0], usage)
		return ExitUsage
	}
	if err == nil {
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "shoalctl %s: %v\n", args[0], err)
	if ctx.Err() != nil {
		return ExitInterrupted
	}
	return codeOf(err)
}

// parseArgs parses flags that may appear before, between, or after positional
// arguments, and checks the positional count.
func parseArgs(fs *flag.FlagSet, args []string, positional int) ([]string, error) {
	return parseArgsRange(fs, args, positional, positional)
}

// parseArgsRange is parseArgs with between min and max positional arguments
// (max < 0: no limit).
func parseArgsRange(fs *flag.FlagSet, args []string, min, max int) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fail(ExitUsage, "%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	switch {
	case min == max && len(pos) != min:
		return nil, fail(ExitUsage, "expected %d argument(s), got %d: %q", min, len(pos), pos)
	case len(pos) < min || (max >= 0 && len(pos) > max):
		return nil, fail(ExitUsage, "expected at least %d argument(s), got %d: %q", min, len(pos), pos)
	}
	return pos, nil
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
