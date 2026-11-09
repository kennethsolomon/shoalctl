//go:build unix

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func executable(t *testing.T, path, script string) {
	t.Helper()
	testutil.WriteFile(t, path, script)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// watched counts the watchers start launches.
func watched(h *harness) *int {
	n := 0
	h.env.SpawnWatcher = func(*project, store.Task) error { n++; return nil }
	return &n
}

// TestStartWarnsWhenTheSocketLookupFailsAfterLaunch: the Claude window is
// already open, so start records the task in-progress (on the $TMUX socket
// when inside tmux, else none: the default server it launched on), warns,
// and still starts the watcher.
func TestStartWarnsWhenTheSocketLookupFailsAfterLaunch(t *testing.T) {
	t.Parallel()
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	wrapper := filepath.Join(t.TempDir(), "tmux")
	executable(t, wrapper, fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *socket_path*) echo 'injected socket failure' >&2; exit 1 ;; esac\nexec %q \"$@\"\n", realTmux))
	h.env.Tmux = wrapper
	n := watched(h)
	slug := newTask(t, h, "bugfix", "Socket lookup fails")
	h.setenv("TMUX", "")
	code, out, errOut := h.run("start", slug)
	if code != 0 {
		t.Fatalf("start exited %d\n%s\n%s", code, out, errOut)
	}
	mustContain(t, "warning", errOut, "warning", "injected socket failure", "default tmux server")
	got := h.task(slug)
	if got.Stage != store.StageInProgress || got.TmuxWindow == "" || got.TmuxSocket != "" || *n != 1 {
		t.Fatalf("after start: %+v, watchers %d", got, *n)
	}

}

func TestEnvSocketReadsTMUXOnlyWithoutAnOverride(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/work,123,0")
	if got := (Env{}).envSocket(); got != "/tmp/tmux-501/work" {
		t.Fatalf("envSocket = %q", got)
	}
	if got := (Env{TmuxArgs: []string{"-L", "x"}}).envSocket(); got != "" {
		t.Fatalf("envSocket with -L = %q", got)
	}
}

// TestStartWarnsWhenTheTargetCheckFailsAfterLaunch: a merge lands during the
// baseline and the needs-rebase check after launch fails. The window is
// open and the task in-progress, so start warns instead of failing, and the
// watcher runs.
func TestStartWarnsWhenTheTargetCheckFailsAfterLaunch(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t).withSession()
	testutil.Git(t, h.repo, "checkout", "-q", "-b", "landing")
	commitIn(t, h.repo, "api.go", "changed by a merge")
	landing := testutil.Git(t, h.repo, "rev-parse", "HEAD")
	testutil.Git(t, h.repo, "checkout", "-q", "--detach", "main")
	old := testutil.Git(t, h.repo, "rev-parse", "main")
	land := filepath.Join(t.TempDir(), "land.sh")
	testutil.WriteFile(t, land, fmt.Sprintf("#!/bin/sh\ngit -C %q update-ref refs/heads/main %s %s 2>/dev/null || true\nexec sh ./test.sh \"$1\"\n", h.repo, landing, old))
	h.initWith(strings.Replace(junitConfig, `"sh ./test.sh {junit}"`, fmt.Sprintf("%q", "sh "+land+" {junit}"), 1))

	bin := t.TempDir()
	executable(t, filepath.Join(bin, "git"), fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n*\" diff --name-status \"*) echo 'injected diff failure' >&2; exit 1 ;;\nesac\nexec %q \"$@\"\n", realGit))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	n := watched(h)
	b := newTask(t, h, "bugfix", "Check fails")
	h.mustRun("plan", b, "api.go")
	code, out, errOut := h.run("start", b)
	if code != 0 {
		t.Fatalf("start exited %d\n%s\n%s", code, out, errOut)
	}
	mustContain(t, "warning", errOut, "warning", "injected diff failure", "rebase")
	if got := h.task(b); got.Stage != store.StageInProgress || got.TmuxWindow == "" || *n != 1 {
		t.Fatalf("after start: %+v, watchers %d", got, *n)
	}
}
