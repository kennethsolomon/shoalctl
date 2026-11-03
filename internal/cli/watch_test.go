package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// fakeTmux answers display-message with the contents of <dir>/dead and
// capture-pane with <dir>/screen; when <dir>/gone exists it fails like tmux
// does for a closed window.
func fakeTmux(t *testing.T, dir string) string {
	t.Helper()
	testutil.RequireSh(t)
	path := filepath.Join(t.TempDir(), "tmux")
	script := fmt.Sprintf(`#!/bin/sh
d='%s'
if [ -f "$d/gone" ]; then echo "can't find window: @1" >&2; exit 1; fi
for a in "$@"; do
  case "$a" in
    display-message) cat "$d/dead" 2>/dev/null || echo 0; exit 0 ;;
    capture-pane) cat "$d/screen" 2>/dev/null; exit 0 ;;
  esac
done
exit 0
`, dir)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	return readFile(t, filepath.Join("..", "attention", "testdata", name))
}

type watchRig struct {
	h    *harness
	p    *project
	dir  string
	slug string
	id   int64
}

func newWatchRig(t *testing.T) *watchRig {
	t.Helper()
	h := newHarness(t)
	p := h.initWith(junitConfig)
	slug := newTask(t, h, "feature", "Watched")
	sqlExec(t, p, `UPDATE tasks SET stage = 'in-progress', tmux_window = '@1' WHERE slug = ?`, slug)
	r := &watchRig{h: h, p: p, dir: t.TempDir(), slug: slug, id: h.task(slug).ID}
	h.env.Tmux, h.env.TmuxArgs = fakeTmux(t, r.dir), nil
	return r
}

func (r *watchRig) screen(t *testing.T, s string) {
	t.Helper()
	testutil.WriteFile(t, filepath.Join(r.dir, "screen"), s)
}

// run starts the loop in the background; the returned channel yields its error.
func (r *watchRig) run(t *testing.T, ctx context.Context) <-chan error {
	t.Helper()
	p, err := r.h.env.openProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.close)
	done := make(chan error, 1)
	env := r.h.env
	env.Stdout, env.Stderr = os.Stderr, os.Stderr
	go func() { done <- env.watchTask(ctx, p, r.id, 10*time.Millisecond) }()
	return done
}

func (r *watchRig) attention(t *testing.T) string {
	t.Helper()
	return r.h.task(r.slug).Attention
}

func wait(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not stop")
	}
}

func TestWatchFlagsAndClearsAttention(t *testing.T) {
	r := newWatchRig(t)
	r.screen(t, fixture(t, "idle.txt"))
	done := r.run(t, context.Background())
	r.screen(t, fixture(t, "trust.txt"))
	waitFor(t, func() bool { return r.attention(t) == "dialog: Do you trust the files in this folder?" })
	mustContain(t, "status", r.h.mustRun("status"), "NEEDS ATTENTION: dialog: Do you trust the files in this folder?")
	if v := r.h.task(r.slug).Version; v != 1 {
		t.Errorf("the flag bumped the task version to %d", v)
	}
	r.screen(t, fixture(t, "working.txt"))
	waitFor(t, func() bool { return r.attention(t) == "" })
	r.screen(t, fixture(t, "bash-permission.txt"))
	waitFor(t, func() bool { return r.attention(t) == "dialog: Do you want to proceed?" })
	testutil.WriteFile(t, filepath.Join(r.dir, "gone"), "")
	wait(t, done)
	if a := r.attention(t); a != "" {
		t.Fatalf("flag left after the window closed: %q", a)
	}
}

func TestWatchStopsOnSignalDeadPaneAndStage(t *testing.T) {
	r := newWatchRig(t)
	r.screen(t, fixture(t, "trust.txt"))
	ctx, cancel := context.WithCancel(context.Background())
	done := r.run(t, ctx)
	waitFor(t, func() bool { return r.attention(t) != "" })
	cancel()
	wait(t, done)
	if a := r.attention(t); a != "" {
		t.Fatalf("flag left after a signal: %q", a)
	}

	testutil.WriteFile(t, filepath.Join(r.dir, "dead"), "1\n")
	wait(t, r.run(t, context.Background()))
	os.Remove(filepath.Join(r.dir, "dead"))

	sqlExec(t, r.p, `UPDATE tasks SET stage = 'done' WHERE id = ?`, r.id)
	wait(t, r.run(t, context.Background()))
}

func TestWatchRunsOncePerTask(t *testing.T) {
	r := newWatchRig(t)
	l, err := lockfile.Acquire(filepath.Join(r.p.dir, "locks", fmt.Sprintf("watch-%d.lock", r.id)), 0, lockfile.Info{PID: 1, Command: "watch"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	mustContain(t, "second watcher", r.h.mustRun("watch", r.slug), "already watched")
}

// TestDoctorClearsAnAttentionFlagNobodyWatches: a watcher killed with
// SIGKILL never clears its flag. doctor reports it when watch-<id>.lock is
// free and --fix clears it; a flag whose watcher still holds the lock stays.
func TestDoctorClearsAnAttentionFlagNobodyWatches(t *testing.T) {
	h := newHarness(t)
	p := h.initWith(junitConfig)
	a := newTask(t, h, "bugfix", "Watcher killed")
	sqlExec(t, p, `UPDATE tasks SET attention = 'Allow edit?', attention_at = ? WHERE slug = ?`, nowUTC(), a)

	_, out, errOut := h.run("doctor")
	mustContain(t, "doctor", errOut, a+": attention flag \"Allow edit?\" is set, but no watcher runs", "--fix clears it")
	h.run("doctor", "--fix")
	if got := h.task(a); got.Attention != "" || got.AttentionAt != "" {
		t.Fatalf("flag kept after --fix: %+v\n%s", got, out)
	}

	sqlExec(t, p, `UPDATE tasks SET attention = 'Allow edit?', attention_at = ? WHERE slug = ?`, nowUTC(), a)
	lock, err := lockfile.Acquire(filepath.Join(p.dir, "locks", fmt.Sprintf("watch-%d.lock", h.task(a).ID)), 0, lockfile.Info{PID: os.Getpid(), Command: "watch"})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	_, out, errOut = h.run("doctor", "--fix")
	if got := h.task(a); got.Attention != "Allow edit?" {
		t.Fatalf("a live watcher's flag was cleared: %+v\n%s\n%s", got, out, errOut)
	}
	mustContain(t, "doctor with a watcher", out, "attention flags")
}
