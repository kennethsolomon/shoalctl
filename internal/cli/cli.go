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
	"syscall"
	"time"

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
	ExitUntrusted  = 12
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
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
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
	case errors.Is(err, store.ErrStage):
		return ExitState
	case errors.Is(err, store.ErrBusy), errors.Is(err, store.ErrConflict), errors.Is(err, lockfile.ErrHeld):
		return ExitBusy
	}
	return ExitInternal
}

const usage = `usage: shoalctl <command> [args]

commands:
  init [--commit-config] [--rebind]
  new <feature|bugfix|hotfix> "<title>" [--ask "<text>" | --ask-file <path>]
  start <task> [--model <model>]
  status [--json]
  done <task>
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
	// SIGINT and SIGTERM cancel ctx instead of killing shoalctl, so a running
	// suite is stopped and recorded as cancelled (testrun forwards the signal
	// to the suite's process group).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	case "start":
		err = env.cmdStart(ctx, args[1:])
	case "status":
		err = env.cmdStatus(ctx, args[1:])
	case "done":
		err = env.cmdDone(ctx, args[1:])
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
	if len(pos) != positional {
		return nil, fail(ExitUsage, "expected %d argument(s), got %d: %q", positional, len(pos), pos)
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
