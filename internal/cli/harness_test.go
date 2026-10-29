package cli

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// fakeExe is an absolute binary path on this OS that never exists; init only
// requires it to be absolute.
var fakeExe = func() string {
	if runtime.GOOS == "windows" {
		return `C:\opt\shoal\bin\shoalctl.exe`
	}
	return "/opt/shoal/bin/shoalctl"
}()

// testScript emits JUnit for a tiny fake suite. C::pre fails at baseline;
// feature.txt adds a passing test and broken.txt breaks C::a. Committing
// tests/bug.t adds the regression test C::bug, which fails until fix.txt
// exists.
const testScript = `#!/bin/sh
out="$1"
{
echo '<testsuite name="S">'
if [ -f broken.txt ]; then echo '<testcase classname="C" name="a"><failure message="broken"/></testcase>'; else echo '<testcase classname="C" name="a"/>'; fi
echo '<testcase classname="C" name="pre"><failure message="old bug"/></testcase>'
echo '<testcase classname="C" name="later"><skipped/></testcase>'
if [ -f feature.txt ]; then echo '<testcase classname="C" name="new"/>'; fi
if [ -f tests/bug.t ]; then if [ -f fix.txt ]; then echo '<testcase classname="C" name="bug"/>'; else echo '<testcase classname="C" name="bug"><failure message="bug"/></testcase>'; fi; fi
echo '</testsuite>'
} > "$out"
`

const junitConfig = "[tests]\ncommand = \"sh ./test.sh {junit}\"\nformat = \"junit\"\n"

type harness struct {
	t       *testing.T
	home    string
	repo    string
	argsLog string
	env     Env
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := testutil.Isolate(t)
	repo := testutil.Repo(t)
	testutil.WriteFile(t, filepath.Join(repo, "test.sh"), testScript)
	testutil.Git(t, repo, "add", "test.sh")
	testutil.Git(t, repo, "commit", "-q", "-m", "add test script")
	h := &harness{t: t, home: home, repo: repo, argsLog: filepath.Join(t.TempDir(), "claude-args.log")}
	h.env = Env{Dir: repo, Exe: fakeExe, Claude: "claude-not-installed-for-this-test", Tmux: "tmux",
		TmuxArgs: []string{"-L", "shoal-test-unused-" + filepath.Base(t.TempDir())}}
	return h
}

// withSession wires a fake claude, a private tmux server and workspace trust.
func (h *harness) withSession() *harness {
	h.t.Helper()
	h.env.Claude = testutil.FakeClaude(h.t, h.argsLog)
	h.env.TmuxArgs = testutil.Tmux(h.t)
	testutil.TrustProject(h.t, h.home, h.repo)
	return h
}

func (h *harness) run(args ...string) (int, string, string) {
	h.t.Helper()
	var out, errb bytes.Buffer
	env := h.env
	env.Stdout, env.Stderr = &out, &errb
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	code := Main(args, env, "test")
	return code, out.String(), errb.String()
}

func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	code, out, errOut := h.run(args...)
	if code != 0 {
		h.t.Fatalf("shoalctl %v exited %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	return out
}

func (h *harness) expectCode(want int, args ...string) string {
	h.t.Helper()
	code, out, errOut := h.run(args...)
	if code != want {
		h.t.Fatalf("shoalctl %v exited %d, want %d\nstdout: %s\nstderr: %s", args, code, want, out, errOut)
	}
	return errOut
}

func (h *harness) project() *project {
	h.t.Helper()
	p, err := h.env.identify(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// initWith runs init and replaces the generated config with body.
func (h *harness) initWith(body string) *project {
	h.t.Helper()
	h.mustRun("init")
	p := h.project()
	testutil.WriteFile(h.t, filepath.Join(p.dir, "config.toml"), body)
	return p
}

func (h *harness) db() *store.Store {
	h.t.Helper()
	p := h.project()
	s, err := store.Open(context.Background(), p.dbPath(), store.Options{Migrate: true, BusyTimeout: 5e9})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { s.Close() })
	return s
}

func (h *harness) task(ref string) store.Task {
	h.t.Helper()
	t, err := h.db().GetTask(context.Background(), ref)
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
