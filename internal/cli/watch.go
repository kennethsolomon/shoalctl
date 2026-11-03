package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/attention"
	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// watchInterval is how often the watcher captures the session's pane.
const watchInterval = 2 * time.Second

// watchStages are the stages in which a task's session window is watched.
var watchStages = []string{store.StageStarting, store.StageInProgress, store.StageQA, store.StageNeedsRebase, store.StageEscalated}

// cmdWatch is the attention watcher start launches for a task: it polls the
// task's tmux pane and sets tasks.attention while the pane shows a dialog or
// prompt waiting for a human, clearing it once the pane moves on. It stops
// when the window or its pane is gone, the task leaves its working stages,
// or on SIGINT, SIGTERM or SIGHUP. One watcher per task: a second one exits
// at once.
func (env Env) cmdWatch(ctx context.Context, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("watch", flag.ContinueOnError), args, 1)
	if err != nil {
		return err
	}
	p, t, err := env.openTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	lock, err := lockfile.Acquire(filepath.Join(p.dir, "locks", fmt.Sprintf("watch-%d.lock", t.ID)), 0,
		lockfile.Info{PID: os.Getpid(), Command: "watch", Task: t.Slug, StartedAt: nowUTC()})
	if errors.Is(err, lockfile.ErrHeld) {
		fmt.Fprintf(env.Stdout, "%s is already watched (%v)\n", t.Slug, err)
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Release()
	return env.watchTask(ctx, p, t.ID, watchInterval)
}

// watchTask is the watcher's loop: one capture per tick, never busier.
func (env Env) watchTask(ctx context.Context, p *project, id int64, every time.Duration) error {
	defer func() {
		// Whatever ended the watch, a flag nobody updates any more must go.
		if _, err := p.db.SetAttention(context.WithoutCancel(ctx), id, ""); err != nil {
			fmt.Fprintf(env.Stderr, "watch: clear the attention flag of task %d: %v\n", id, err)
		}
	}()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		t, err := p.db.GetTask(ctx, fmt.Sprint(id))
		if err != nil {
			return quietOnCancel(ctx, err)
		}
		if !contains(watchStages, t.Stage) || t.TmuxWindow == "" {
			return nil
		}
		dead, err := env.tmux(ctx, "display-message", "-p", "-t", t.TmuxWindow, "#{pane_dead}")
		if err != nil {
			if ctx.Err() != nil || windowGone(err) {
				return nil
			}
			return err
		}
		if dead == "1" {
			return nil // claude exited; remain-on-exit keeps the dead pane
		}
		screen, err := env.tmux(ctx, "capture-pane", "-p", "-t", t.TmuxWindow)
		if err != nil {
			if ctx.Err() != nil || windowGone(err) {
				return nil
			}
			return err
		}
		reason, _ := attention.Detect(screen)
		changed, err := p.db.SetAttention(ctx, id, reason)
		if err != nil {
			return quietOnCancel(ctx, err)
		}
		if changed && reason != "" {
			fmt.Fprintf(env.Stdout, "%s %s needs attention: %s\n", nowUTC(), t.Slug, reason)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func quietOnCancel(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// windowGone reports a tmux error that means the window, or the whole
// server, no longer exists.
func windowGone(err error) bool {
	msg := err.Error()
	for _, s := range []string{"can't find", "no server running", "error connecting", "no such window", "no such session"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// startWatcher launches the attention watcher for a started task.
func (env Env) startWatcher(p *project, t store.Task) error {
	if env.SpawnWatcher != nil {
		return env.SpawnWatcher(p, t)
	}
	return spawnWatcher(env.Exe, p, t)
}

// spawnWatcher runs `<exe> watch <task>` detached from this process and its
// terminal (a new session on unix), logging to logs/watch-<id>.log. It does
// not wait: start exits while the watcher keeps running.
func spawnWatcher(exe string, p *project, t store.Task) error {
	logPath := filepath.Join(p.dir, "logs", fmt.Sprintf("watch-%d.log", t.ID))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(logPath), err)
	}
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", logPath, err)
	}
	defer log.Close()
	cmd := exec.Command(exe, "watch", t.Slug)
	cmd.Dir = p.root
	cmd.Env = append(os.Environ(), "SHOAL_HOME="+p.home)
	cmd.Stdout, cmd.Stderr = log, log
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s watch %s: %w", exe, t.Slug, err)
	}
	go func() { _ = cmd.Wait() }() // reap it if this process outlives it; its own log has its errors
	return nil
}
