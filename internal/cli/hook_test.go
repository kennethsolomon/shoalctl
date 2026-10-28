package cli

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// fakeWorktree registers a task and creates its worktree dir with task.json,
// which is all the hooks look at.
func fakeWorktree(t *testing.T, h *harness) (string, *project) {
	t.Helper()
	h.mustRun("init")
	h.mustRun("new", "feature", "Hooked")
	p := h.project()
	wt := p.worktree("1-hooked")
	if err := writeTaskFile(wt, taskFile{ProjectID: p.id, TaskID: 1, Slug: "1-hooked"}); err != nil {
		t.Fatal(err)
	}
	return wt, p
}

func (h *harness) hook(stdin io.Reader, args ...string) (int, string, string) {
	h.t.Helper()
	h.env.Stdin = stdin
	defer func() { h.env.Stdin = nil }()
	return h.run(append([]string{"hook"}, args...)...)
}

func hookLog(t *testing.T, p *project) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(p.home, "logs", "hooks.log"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHookHeartbeat(t *testing.T) {
	h := newHarness(t)
	wt, p := fakeWorktree(t, h)
	t.Setenv("CLAUDE_PROJECT_DIR", wt)

	code, out, errOut := h.hook(strings.NewReader(`{"session_id": "sess-42", "cwd": "/elsewhere", "hook_event_name": "SessionStart"}`), "session-start")
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("session-start: code %d stdout %q stderr %q", code, out, errOut)
	}
	got := h.task("1")
	if got.SessionID != "sess-42" || got.LastSeenAt == "" {
		t.Fatalf("after session-start: %+v", got)
	}
	if code, out, errOut := h.hook(strings.NewReader(`{"session_id": "other"}`), "stop"); code != 0 || out != "" || errOut != "" {
		t.Fatalf("stop: code %d stdout %q stderr %q", code, out, errOut)
	}
	if again := h.task("1"); again.SessionID != "sess-42" || again.Version != got.Version {
		t.Fatalf("after stop: %+v", again)
	}
	if log := hookLog(t, p); log != "" {
		t.Fatalf("unexpected hook log:\n%s", log)
	}
}

func TestHookUsesCwdWhenProjectDirUnset(t *testing.T) {
	h := newHarness(t)
	wt, _ := fakeWorktree(t, h)
	if code, _, _ := h.hook(strings.NewReader(`{"session_id": "via-cwd", "cwd": "`+filepath.ToSlash(wt)+`"}`), "session-start"); code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := h.task("1"); got.SessionID != "via-cwd" {
		t.Fatalf("session id %q", got.SessionID)
	}
}

func TestHookSilentOutsideShoalWorktrees(t *testing.T) {
	h := newHarness(t)
	wt, p := fakeWorktree(t, h)
	before := h.task("1")

	notTask := filepath.Join(filepath.Dir(wt), "no-task-json")
	if err := os.MkdirAll(notTask, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{h.repo, notTask, t.TempDir(), filepath.Join(wt, ".shoal")} {
		t.Setenv("CLAUDE_PROJECT_DIR", dir)
		for _, ev := range []string{"session-start", "stop", "bogus"} {
			code, out, errOut := h.hook(strings.NewReader(`not even json`), ev)
			if code != 0 || out != "" || errOut != "" {
				t.Fatalf("%s in %s: code %d stdout %q stderr %q", ev, dir, code, out, errOut)
			}
		}
	}
	if after := h.task("1"); after.Version != before.Version {
		t.Fatalf("a hook outside a shoal worktree wrote to memory.db: %+v", after)
	}
	if log := hookLog(t, p); log != "" {
		t.Fatalf("hooks outside shoal worktrees must not log:\n%s", log)
	}
}

func TestHookFailsOpen(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, p *project)
		stdin string
		event string
		want  string
	}{
		{"memory.db renamed away", func(t *testing.T, p *project) {
			if err := os.Rename(p.dbPath(), p.dbPath()+".away"); err != nil {
				t.Fatal(err)
			}
		}, `{}`, "session-start", "memory.db"},
		{"corrupt memory.db", func(t *testing.T, p *project) {
			for _, suffix := range []string{"-wal", "-shm"} {
				os.Remove(p.dbPath() + suffix)
			}
			testutil.WriteFile(t, p.dbPath(), strings.Repeat("garbage!", 512))
		}, `{}`, "stop", "stop"},
		{"bad hook input", func(*testing.T, *project) {}, `{"session_id": `, "session-start", "read hook input"},
		{"unknown event", func(*testing.T, *project) {}, `{}`, "pre-tool-use", "unknown hook event"},
		{"task row missing", func(t *testing.T, p *project) {
			if err := writeTaskFile(p.worktree("1-hooked"), taskFile{ProjectID: p.id, TaskID: 77, Slug: "1-hooked"}); err != nil {
				t.Fatal(err)
			}
		}, `{}`, "stop", "not found"},
		{"newer schema", func(t *testing.T, p *project) {
			db := openRaw(t, p.dbPath())
			if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
				t.Fatal(err)
			}
			db.Close()
		}, `{}`, "stop", "schema 99"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			wt, p := fakeWorktree(t, h)
			tt.setup(t, p)
			t.Setenv("CLAUDE_PROJECT_DIR", wt)
			code, out, errOut := h.hook(strings.NewReader(tt.stdin), tt.event)
			if code != 0 || out != "" || errOut != "" {
				t.Fatalf("code %d stdout %q stderr %q", code, out, errOut)
			}
			log := hookLog(t, p)
			if strings.Count(log, "\n") != 1 || !strings.Contains(log, tt.want) || !strings.Contains(log, wt) {
				t.Fatalf("want one log line mentioning %q and %s, got:\n%s", tt.want, wt, log)
			}
			if _, err := os.Stat(p.dbPath()); tt.name == "memory.db renamed away" && !os.IsNotExist(err) {
				t.Fatalf("hook recreated memory.db: %v", err)
			}
		})
	}
}

func TestHookDeadline(t *testing.T) {
	h := newHarness(t)
	wt, p := fakeWorktree(t, h)
	t.Setenv("CLAUDE_PROJECT_DIR", wt)
	r, w := io.Pipe()
	defer w.Close()
	start := time.Now()
	if code, out, errOut := h.hook(r, "stop"); code != 0 || out != "" || errOut != "" {
		t.Fatalf("code %d stdout %q stderr %q", code, out, errOut)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("hook took %s, want about 2s", took)
	}
	if log := hookLog(t, p); strings.Count(log, "\n") != 1 {
		t.Fatalf("want one log line, got:\n%s", log)
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("boom") }

func TestHookRecoversPanic(t *testing.T) {
	h := newHarness(t)
	wt, p := fakeWorktree(t, h)
	t.Setenv("CLAUDE_PROJECT_DIR", wt)
	if code, out, errOut := h.hook(panicReader{}, "stop"); code != 0 || out != "" || errOut != "" {
		t.Fatalf("code %d stdout %q stderr %q", code, out, errOut)
	}
	if log := hookLog(t, p); strings.Count(log, "\n") != 1 || !strings.Contains(log, "panic") {
		t.Fatalf("want one log line mentioning the panic, got:\n%s", log)
	}
}

func TestHookUnwritableLogStillExitsZero(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	h := newHarness(t)
	wt, p := fakeWorktree(t, h)
	os.Rename(p.dbPath(), p.dbPath()+".away")
	logs := filepath.Join(p.home, "logs")
	if err := os.Chmod(logs, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(logs, 0o700) })
	t.Setenv("CLAUDE_PROJECT_DIR", wt)
	if code, out, errOut := h.hook(strings.NewReader(`{}`), "session-start"); code != 0 || out != "" || errOut != "" {
		t.Fatalf("code %d stdout %q stderr %q", code, out, errOut)
	}
}

func TestStatusWarnsAboutNewHookFailures(t *testing.T) {
	h := newHarness(t)
	wt, p := fakeWorktree(t, h)
	t.Setenv("CLAUDE_PROJECT_DIR", wt)
	h.hook(strings.NewReader(`{`), "stop")

	if _, _, errOut := h.run("status"); !strings.Contains(errOut, "1 new hook failure") {
		t.Fatalf("first status stderr: %q", errOut)
	}
	if _, _, errOut := h.run("status"); errOut != "" {
		t.Fatalf("second status should be quiet, stderr: %q", errOut)
	}
	h.hook(strings.NewReader(`{`), "stop")
	h.hook(strings.NewReader(`{`), "stop")
	if _, _, errOut := h.run("status"); !strings.Contains(errOut, "2 new hook failure") || !strings.Contains(errOut, filepath.Join(p.home, "logs", "hooks.log")) {
		t.Fatalf("third status stderr: %q", errOut)
	}
}

// blockLogWriter makes every hooks.log write hang, as on a stuck network
// filesystem, until the test ends.
func blockLogWriter(t *testing.T) {
	t.Helper()
	release := make(chan struct{})
	orig := logHook
	logHook = func(string, string, error) { <-release }
	t.Cleanup(func() {
		close(release)
		logHook = orig
	})
}

// hookWithin runs a hook and fails unless it returns 0 silently within limit.
func hookWithin(t *testing.T, h *harness, limit time.Duration, stdin io.Reader, event string) {
	t.Helper()
	type result struct {
		code     int
		out, err string
	}
	ch := make(chan result, 1)
	start := time.Now()
	go func() {
		code, out, errOut := h.hook(stdin, event)
		ch <- result{code, out, errOut}
	}()
	select {
	case r := <-ch:
		if r.code != 0 || r.out != "" || r.err != "" {
			t.Fatalf("code %d stdout %q stderr %q", r.code, r.out, r.err)
		}
		if took := time.Since(start); took > limit {
			t.Fatalf("hook took %s, limit %s", took, limit)
		}
	case <-time.After(limit + 2*time.Second):
		t.Fatalf("hook still running after %s: the post-deadline path is unbounded", limit+2*time.Second)
	}
}

func TestHookDeadlineBoundedWithBlockedLogWriter(t *testing.T) {
	h := newHarness(t)
	wt, _ := fakeWorktree(t, h)
	t.Setenv("CLAUDE_PROJECT_DIR", wt)
	blockLogWriter(t)
	r, w := io.Pipe()
	defer w.Close()
	hookWithin(t, h, hookDeadline+200*time.Millisecond, r, "stop")
}

func TestHookErrorBoundedWithBlockedLogWriter(t *testing.T) {
	h := newHarness(t)
	wt, _ := fakeWorktree(t, h)
	t.Setenv("CLAUDE_PROJECT_DIR", wt)
	blockLogWriter(t)
	hookWithin(t, h, hookDeadline+200*time.Millisecond, strings.NewReader(`{"session_id": `), "stop")
}
