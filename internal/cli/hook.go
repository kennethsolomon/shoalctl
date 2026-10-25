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

const hookDeadline = 2 * time.Second

type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
}

// runHook is the entry point Claude Code calls. It always returns 0 and never
// writes to stdout or stderr: failures go to $SHOAL_HOME/logs/hooks.log (§4.6).
func runHook(args []string, env Env) int {
	event := strings.Join(args, " ")
	done := make(chan hookResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- unguarded(fmt.Errorf("panic: %v", r))
			}
		}()
		done <- env.hook(args)
	}()
	var res hookResult
	select {
	case res = <-done:
	case <-time.After(hookDeadline):
		res = unguarded(fmt.Errorf("deadline of %s exceeded", hookDeadline))
	}
	if res.err != nil && res.shoal {
		logHookFailure(event, res.dir, res.err)
	}
	return ExitOK
}

// unguarded builds the result for a failure that happened before or outside
// the guard, logging it only when CLAUDE_PROJECT_DIR is a shoal worktree.
func unguarded(err error) hookResult {
	home, herr := shoalHome()
	if herr != nil {
		return hookResult{}
	}
	wt, ok := shoalWorktree(os.Getenv("CLAUDE_PROJECT_DIR"), home)
	return hookResult{err: err, dir: wt, shoal: ok}
}

type hookResult struct {
	err   error
	dir   string
	shoal bool // the session runs in a shoal worktree, so failures are worth logging
}

func (env Env) hook(args []string) hookResult {
	ctx, cancel := context.WithTimeout(context.Background(), hookDeadline)
	defer cancel()

	var in hookInput
	raw, readErr := readAllContext(ctx, env.Stdin)
	jsonErr := readErr
	if readErr == nil && len(strings.TrimSpace(string(raw))) > 0 {
		jsonErr = json.Unmarshal(raw, &in)
	}

	dir := os.Getenv("CLAUDE_PROJECT_DIR")
	if dir == "" {
		dir = in.Cwd
	}
	home, err := shoalHome()
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
	if len(args) != 1 || (args[0] != "session-start" && args[0] != "stop") {
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
	db, err := store.Open(ctx, filepath.Join(projDir, "memory.db"), store.Options{BusyTimeout: time.Second})
	if err != nil {
		res.err = err
		return res
	}
	defer db.Close()
	sessionID := ""
	if args[0] == "session-start" {
		sessionID = in.SessionID
	}
	res.err = db.Heartbeat(ctx, tf.TaskID, tf.Slug, sessionID)
	return res
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
func logHookFailure(event, dir string, err error) {
	home, herr := shoalHome()
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
