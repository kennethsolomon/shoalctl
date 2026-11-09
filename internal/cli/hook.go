package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/store"
)

// hookDeadline bounds a whole hook process, logging included. The handler
// gets hookDeadline - logBudget; failure logging gets whatever is left.
const (
	hookDeadline = 2 * time.Second
	logBudget    = 300 * time.Millisecond
)

// logHook writes one hooks.log line under env's SHOAL_HOME; tests replace it.
var logHook = logHookFailure

// runHook is the entry point Claude Code calls. It always returns 0 within
// hookDeadline and never writes to stdout or stderr: failures go to
// $SHOAL_HOME/logs/hooks.log (§4.6). Everything that can block (the handler,
// and on failure even resolving where to log) runs on other goroutines, so a
// hung filesystem can only cost a log line, never the deadline.
func runHook(args []string, env Env) int {
	deadline := time.Now().Add(hookDeadline)
	event := strings.Join(args, " ")
	done := make(chan hookResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- hookResult{err: fmt.Errorf("panic: %v", r), unresolved: true}
			}
		}()
		done <- env.hook(args, deadline.Add(-logBudget))
	}()
	var res hookResult
	select {
	case res = <-done:
	case <-time.After(time.Until(deadline.Add(-logBudget))):
		res = hookResult{err: fmt.Errorf("deadline of %s exceeded", hookDeadline-logBudget), unresolved: true}
	}
	if res.err == nil && res.out != "" && env.Stdout != nil {
		// Nothing else is ever written to stdout: Claude Code reads it as the
		// hook's JSON answer, and only a handler that succeeded answers.
		if _, werr := io.WriteString(env.Stdout, res.out); werr != nil {
			res.err = fmt.Errorf("write hook output: %w", werr)
		}
	}
	if res.err == nil {
		return ExitOK
	}
	logLine := logHook // read here: the goroutine below may outlive this call
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		defer func() { _ = recover() }()
		if res.unresolved {
			res = env.resolveFailure(res.err)
		}
		if res.shoal {
			logLine(env, event, res.dir, res.err)
		}
	}()
	select {
	case <-logged:
	case <-time.After(time.Until(deadline)):
	}
	return ExitOK
}

// resolveFailure finds where a failure that happened before or outside the
// guard belongs, logging it only when CLAUDE_PROJECT_DIR is a shoal worktree.
func (env Env) resolveFailure(err error) hookResult {
	home, herr := env.shoalHome()
	if herr != nil {
		return hookResult{}
	}
	wt, ok := shoalWorktree(env.getenv("CLAUDE_PROJECT_DIR"), home)
	return hookResult{err: err, dir: wt, shoal: ok}
}

type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Source    string `json:"source"`     // SessionStart: startup, resume, clear or compact
	ToolName  string `json:"tool_name"`  // PreToolUse, PostToolUse
	AgentType string `json:"agent_type"` // set when a subagent (or --agent) runs the tool
	ToolInput struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

type hookResult struct {
	err        error
	out        string // JSON for Claude Code, written to stdout only on success
	dir        string
	shoal      bool // the session runs in a shoal worktree, so failures are worth logging
	unresolved bool // dir and shoal are not known yet
}

// hookArgs are the events shoal registers, by their hook argument.
var hookArgs = []string{"session-start", "stop", "pre-tool-use", "post-tool-use"}

// hookCtx is what every handler gets once the guard passed.
type hookCtx struct {
	in      hookInput
	event   string
	wt      string // the task worktree, symlinks resolved
	tf      taskFile
	projDir string
	home    string
}

func (env Env) hook(args []string, deadline time.Time) hookResult {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	var in hookInput
	raw, readErr := readAllContext(ctx, env.Stdin)
	jsonErr := readErr
	if readErr == nil && len(strings.TrimSpace(string(raw))) > 0 {
		jsonErr = json.Unmarshal(raw, &in)
	}

	dir := env.getenv("CLAUDE_PROJECT_DIR")
	if dir == "" {
		dir = in.Cwd
	}
	home, err := env.shoalHome()
	if err != nil || dir == "" {
		return hookResult{}
	}
	wt, ok := shoalWorktree(dir, home)
	if !ok {
		return hookResult{}
	}
	res := hookResult{dir: wt, shoal: true}
	if jsonErr != nil {
		res.err = fmt.Errorf("read hook input: %w", jsonErr)
		return res
	}
	if len(args) != 1 || !contains(hookArgs, args[0]) {
		res.err = fmt.Errorf("unknown hook event %q", strings.Join(args, " "))
		return res
	}

	tf, err := readTaskFile(wt)
	if err != nil {
		res.err = err
		return res
	}
	projDir := filepath.Dir(filepath.Dir(wt))
	if tf.ProjectID != filepath.Base(projDir) {
		res.err = fmt.Errorf("task.json project_id %q does not match project dir %s", tf.ProjectID, projDir)
		return res
	}
	hc := hookCtx{in: in, event: args[0], wt: wt, tf: tf, projDir: projDir, home: home}
	switch hc.event {
	case "session-start":
		res.out, res.err = env.hookSessionStart(ctx, hc)
	case "stop":
		res.err = hc.heartbeat(ctx, "")
	case "pre-tool-use":
		res.out, res.err = env.hookPreToolUse(ctx, hc)
	case "post-tool-use":
		res.out, res.err = env.hookPostToolUse(ctx, hc)
	}
	return res
}

// openDB opens memory.db for a hook: a 1-second busy_timeout, no retries,
// and never a migration (§10.1).
func (hc hookCtx) openDB(ctx context.Context) (*store.Store, error) {
	return store.Open(ctx, filepath.Join(hc.projDir, "memory.db"), store.Options{BusyTimeout: time.Second})
}

func (hc hookCtx) heartbeat(ctx context.Context, sessionID string) error {
	db, err := hc.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Heartbeat(ctx, hc.tf.TaskID, hc.tf.Slug, sessionID)
}

// shoalWorktree reports whether dir is a task worktree: directly under
// $SHOAL_HOME/projects/<id>/worktrees/ and holding .shoal/task.json.
func shoalWorktree(dir, home string) (string, bool) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	projects, err := filepath.EvalSymlinks(filepath.Join(home, "projects"))
	if err != nil {
		return "", false
	}
	parent := filepath.Dir(real)
	if filepath.Base(parent) != "worktrees" || filepath.Dir(filepath.Dir(parent)) != projects {
		return "", false
	}
	info, err := os.Lstat(filepath.Join(real, ".shoal", "task.json"))
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return real, true
}

func readTaskFile(wt string) (taskFile, error) {
	var tf taskFile
	path := filepath.Join(wt, ".shoal", "task.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return tf, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &tf); err != nil {
		return tf, fmt.Errorf("parse %s: %w", path, err)
	}
	if tf.TaskID <= 0 || tf.Slug == "" || tf.ProjectID == "" {
		return tf, fmt.Errorf("%s is missing project_id, task_id or slug", path)
	}
	return tf, nil
}

func readAllContext(ctx context.Context, r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- result{nil, fmt.Errorf("panic reading hook input: %v", p)}
			}
		}()
		data, err := io.ReadAll(io.LimitReader(r, 1<<20))
		ch <- result{data, err}
	}()
	select {
	case res := <-ch:
		return res.data, res.err
	case <-ctx.Done():
		return nil, errors.New("timed out reading hook input")
	}
}

// logHookFailure appends one line to hooks.log. If that fails too there is
// nowhere left to report, and the hook still exits 0.
func logHookFailure(env Env, event, dir string, err error) {
	home, herr := env.shoalHome()
	if herr != nil {
		return
	}
	logDir := filepath.Join(home, "logs")
	if os.MkdirAll(logDir, 0o700) != nil {
		return
	}
	f, ferr := os.OpenFile(filepath.Join(logDir, "hooks.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	msg := strings.Join(strings.Fields(err.Error()), " ")
	fmt.Fprintf(f, "%s %s %s %s\n", nowUTC(), event, dir, msg)
}
