package testrun

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

const junitXML = `<?xml version="1.0"?>
<testsuites>
  <testsuite name="Suite" file="tests/a.php">
    <testcase classname="Users" name="lists" time="0.25"/>
    <testcase classname="Users" name="sorts"><failure message="expected asc">diff here</failure></testcase>
    <testcase classname="Users" name="sorts"/>
    <testcase classname="" name="from-file"/>
    <testcase classname="Users" name="later"><skipped/></testcase>
    <testcase classname="Users" name="crash"><error message="panic"/></testcase>
    <testsuite name="Nested">
      <testcase name="inner"/>
    </testsuite>
  </testsuite>
  <testsuite name="Bare"><testcase name="only-suite"/></testsuite>
</testsuites>`

func statuses(rs []Result) map[string]string {
	m := map[string]string{}
	for _, r := range rs {
		m[r.TestID] = r.Status
	}
	return m
}

func TestParseJUnit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.xml")
	if err := os.WriteFile(p, []byte(junitXML), 0o600); err != nil {
		t.Fatal(err)
	}
	rs, err := ParseJUnitFiles([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Users::lists":           "pass",
		"Users::sorts":           "fail",
		"Users::sorts#2":         "pass",
		"tests/a.php::from-file": "pass",
		"Users::later":           "skip",
		"Users::crash":           "fail",
		"tests/a.php::inner":     "pass",
		"Bare::only-suite":       "pass",
	}
	if got := statuses(rs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if rs[0].DurationMS != 250 {
		t.Errorf("duration = %d, want 250", rs[0].DurationMS)
	}
	if !strings.Contains(rs[1].Message, "expected asc") || !strings.Contains(rs[1].Message, "diff here") {
		t.Errorf("failure message = %q", rs[1].Message)
	}
}

func TestParseJUnitSingleSuiteRootAndErrors(t *testing.T) {
	dir := t.TempDir()
	single := filepath.Join(dir, "single.xml")
	os.WriteFile(single, []byte(`<testsuite name="S"><testcase classname="C" name="x"/></testsuite>`), 0o600)
	rs, err := ParseJUnitFiles([]string{single})
	if err != nil || len(rs) != 1 || rs[0].TestID != "C::x" {
		t.Fatalf("single suite root: %+v, %v", rs, err)
	}

	for name, body := range map[string]string{"garbage.xml": "not xml <", "wrong.xml": "<html></html>"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o600)
		if _, err := ParseJUnitFiles([]string{p}); err == nil {
			t.Errorf("%s: want parse error", name)
		}
	}
}

func TestMessageTruncated(t *testing.T) {
	long := strings.Repeat("é", 2000)
	if got := []rune(truncate(long)); len(got) != 1000 {
		t.Fatalf("truncated to %d runes, want 1000", len(got))
	}
}

const goJSON = `{"Action":"start","Package":"ex/a"}
{"Action":"run","Package":"ex/a","Test":"TestOK"}
{"Action":"pass","Package":"ex/a","Test":"TestOK","Elapsed":0.5}
{"Action":"run","Package":"ex/a","Test":"TestBad"}
{"Action":"output","Package":"ex/a","Test":"TestBad","Output":"bad_test.go:9: boom\n"}
{"Action":"run","Package":"ex/a","Test":"TestBad/sub"}
{"Action":"fail","Package":"ex/a","Test":"TestBad/sub"}
{"Action":"fail","Package":"ex/a","Test":"TestBad"}
{"Action":"run","Package":"ex/a","Test":"TestSkip"}
{"Action":"skip","Package":"ex/a","Test":"TestSkip"}
{"Action":"run","Package":"ex/a","Test":"TestHang"}
{"Action":"output","Package":"ex/a","Test":"TestHang","Output":"panic: test timed out\n"}
{"Action":"fail","Package":"ex/a"}
not json at all
{"Action":"output","Package":"ex/broken","Output":"# ex/broken\nsyntax error\n"}
{"Action":"fail","Package":"ex/broken"}
{"Action":"skip","Package":"ex/empty"}
`

func TestParseGoJSON(t *testing.T) {
	rs, events, err := ParseGoJSON(strings.NewReader(goJSON))
	if err != nil {
		t.Fatal(err)
	}
	if events != 16 {
		t.Errorf("events = %d, want 16", events)
	}
	want := map[string]string{
		"ex/a::TestOK":         "pass",
		"ex/a::TestBad":        "fail",
		"ex/a::TestBad/sub":    "fail",
		"ex/a::TestSkip":       "skip",
		"ex/a::TestHang":       "fail",
		"ex/broken::(package)": "fail",
	}
	if got := statuses(rs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for _, r := range rs {
		switch r.TestID {
		case "ex/a::TestBad":
			if !strings.Contains(r.Message, "boom") {
				t.Errorf("TestBad message = %q", r.Message)
			}
		case "ex/a::TestHang":
			if !strings.Contains(r.Message, "timed out") {
				t.Errorf("TestHang message = %q", r.Message)
			}
		case "ex/broken::(package)":
			if !strings.Contains(r.Message, "syntax error") {
				t.Errorf("package message = %q", r.Message)
			}
		case "ex/a::TestOK":
			if r.DurationMS != 500 {
				t.Errorf("duration = %d", r.DurationMS)
			}
		}
	}
}

func TestCompare(t *testing.T) {
	base := map[string]string{"reg": "pass", "gone": "pass", "skp": "pass", "pre": "fail", "fix": "fail", "ok": "pass", "s2f": "skip", "s2p": "skip"}
	final := map[string]string{"reg": "fail", "skp": "skip", "pre": "fail", "fix": "pass", "ok": "pass", "s2f": "fail", "s2p": "pass", "nf": "fail", "nw": "pass", "ns": "skip"}
	got := Compare(base, final)
	want := map[Class][]string{
		Regression:   {"reg"},
		Missing:      {"gone"},
		NewlySkipped: {"skp"},
		NewFailure:   {"nf", "s2f"},
		PreExisting:  {"pre"},
		Fixed:        {"fix"},
		New:          {"nw"},
		OK:           {"ns", "ok", "s2p"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestCompareFlakyAndBlocking(t *testing.T) {
	base := map[string]string{"race": "pass", "bfl": Flaky, "fixfl": "fail", "skipfl": "skip", "q": "pass", "reg": "pass"}
	final := map[string]string{"race": Flaky, "bfl": "pass", "fixfl": Flaky, "skipfl": Flaky, "newfl": Flaky, "reg": "fail", "q": "fail"}
	got := Compare(Without(base, map[string]bool{"q": true}), Without(final, map[string]bool{"q": true}))
	want := map[Class][]string{
		Unstable:   {"newfl", "race", "skipfl"},
		OK:         {"bfl"},
		Fixed:      {"fixfl"},
		Regression: {"reg"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if b := Blocking(got); !reflect.DeepEqual(b, []string{"newfl", "race", "reg", "skipfl"}) {
		t.Fatalf("blocking %v", b)
	}
	if b := Blocking(Compare(map[string]string{"a": "fail"}, map[string]string{"a": "fail", "n": "pass"})); b != nil {
		t.Fatalf("pre-existing and new must not block: %v", b)
	}
}

func TestRerunCommand(t *testing.T) {
	got := RerunCommand("pytest --junitxml {junit} {tests}", []string{"C::a", "it's"})
	if got != `pytest --junitxml {junit} 'C::a' 'it'\''s'` {
		t.Fatalf("got %s", got)
	}
}

func TestCommandHash(t *testing.T) {
	if CommandHash("ab", "c", "", "") == CommandHash("a", "bc", "", "") {
		t.Fatal("hash must separate fields")
	}
	if CommandHash("x", "junit", "", "cp {root}/.env .") == CommandHash("x", "junit", "", "") {
		t.Fatal("hash must change with [tests].setup")
	}
	if CommandHash("x", "junit", "a", "") == CommandHash("x", "junit", "", "a") {
		t.Fatal("hash must separate rerun_command from setup")
	}
	first, second := CommandHash("x", "junit", "", ""), CommandHash("x", "junit", "", "")
	if first != second || len(first) != 64 {
		t.Fatalf("hash must be a deterministic SHA-256 hex digest, got %q and %q", first, second)
	}
}

func runSpec(t *testing.T, format, command string) (Spec, string) {
	t.Helper()
	testutil.RequireSh(t)
	dir := t.TempDir()
	return Spec{Command: command, Format: format, JunitPath: "{junit}", Timeout: 10 * time.Second,
		Dir: dir, RunDir: filepath.Join(t.TempDir(), "runs", "1")}, dir
}

func TestRunFormats(t *testing.T) {
	junitOK := `printf '%s' '<testsuite name="S"><testcase classname="C" name="a"/></testsuite>' > {junit}`
	tests := []struct {
		name, format, command string
		status                string
		results               map[string]string
		exit                  int
	}{
		{"junit pass", "junit", junitOK, StatusCompleted, map[string]string{"C::a": "pass"}, 0},
		{"junit nonzero but all pass", "junit", junitOK + "; exit 3", StatusCompleted, map[string]string{"C::a": "pass", SuiteID: "fail"}, 3},
		{"junit missing file", "junit", "exit 1", StatusError, nil, 1},
		{"junit missing file exit 0", "junit", "true", StatusError, nil, 0},
		{"exit-code pass", "exit-code", "true", StatusCompleted, map[string]string{SuiteID: "pass"}, 0},
		{"exit-code fail", "exit-code", "exit 2", StatusCompleted, map[string]string{SuiteID: "fail"}, 2},
		{"go-json", "go-json", `printf '%s\n' '{"Action":"pass","Package":"p","Test":"TestA"}'; echo noise >&2`, StatusCompleted, map[string]string{"p::TestA": "pass"}, 0},
		{"go-json no events", "go-json", "echo hello", StatusError, nil, 0},
		{"none", "none", "", StatusCompleted, map[string]string{}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, _ := runSpec(t, tt.format, tt.command)
			out := Run(context.Background(), spec)
			if out.Status != tt.status {
				t.Fatalf("status = %s (%s), want %s", out.Status, out.Problem, tt.status)
			}
			if tt.results != nil {
				if got := statuses(out.Results); !reflect.DeepEqual(got, tt.results) {
					t.Fatalf("results = %v, want %v", got, tt.results)
				}
			}
			if tt.exit >= 0 && (out.ExitCode == nil || *out.ExitCode != tt.exit) {
				t.Fatalf("exit code = %v, want %d", out.ExitCode, tt.exit)
			}
			if _, err := os.Stat(out.OutputPath); err != nil {
				t.Fatalf("output.log: %v", err)
			}
		})
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are unix-only")
	}
	spec, dir := runSpec(t, "exit-code", "sleep 30 & echo $! > child.pid; wait")
	spec.Timeout = 500 * time.Millisecond
	start := time.Now()
	out := Run(context.Background(), spec)
	if out.Status != StatusTimeout {
		t.Fatalf("status = %s, want timeout", out.Status)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	pid, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatalf("child pid: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run() == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child process %s survived the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// pidsGone waits until every pid in the files has exited.
func pidsGone(t *testing.T, within time.Duration, files ...string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for _, f := range files {
		pid, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("pid file: %v", err)
		}
		for exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run() == nil {
			if time.Now().After(deadline) {
				t.Fatalf("process %s from %s survived the interrupt", strings.TrimSpace(string(pid)), filepath.Base(f))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunCancelStopsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sh and kill; the Windows job object is covered by compile checks")
	}
	tests := []struct{ name, command string }{
		{"polite suite", "echo $$ > sh.pid; sleep 30 & echo $! > child.pid; wait"},
		{"child ignores SIGTERM", `echo $$ > sh.pid; sh -c 'trap "" TERM; echo $$ > child.pid; while :; do sleep 1; done' & wait`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, dir := runSpec(t, "exit-code", tt.command)
			var started int
			spec.OnStart = func(pid int) error { started = pid; return nil }
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan Outcome, 1)
			go func() { done <- Run(ctx, spec) }()
			waitForFile(t, filepath.Join(dir, "child.pid"))
			begin := time.Now()
			cancel()
			out := <-done
			if out.Status != StatusCancelled {
				t.Fatalf("status = %s (%s), want cancelled", out.Status, out.Problem)
			}
			if took := time.Since(begin); took > killGrace+3*time.Second {
				t.Fatalf("cancel took %s", took)
			}
			if started == 0 {
				t.Fatal("OnStart was not called with the suite pid")
			}
			pidsGone(t, 2*time.Second, filepath.Join(dir, "sh.pid"), filepath.Join(dir, "child.pid"))
			if GroupAlive(started) {
				t.Fatalf("process group %d still alive", started)
			}
		})
	}
}

func TestRunKillsLeftoverProcessesAfterSuiteExits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sh and kill")
	}
	spec, dir := runSpec(t, "exit-code", "sleep 30 & echo $! > child.pid; exit 0")
	if out := Run(context.Background(), spec); out.Status != StatusCompleted {
		t.Fatalf("status %s: %s", out.Status, out.Problem)
	}
	pidsGone(t, 2*time.Second, filepath.Join(dir, "child.pid"))
}

func TestRunDeletesStaleUntrackedReports(t *testing.T) {
	testutil.Isolate(t)
	repo := testutil.Repo(t)
	spec, _ := runSpec(t, "junit", "exit 1")
	spec.Dir = repo
	spec.JunitPath = "reports/*.xml"
	stale := filepath.Join(repo, "reports", "old.xml")
	testutil.WriteFile(t, stale, `<testsuite><testcase classname="C" name="a"/></testsuite>`)

	out := Run(context.Background(), spec)
	if out.Status != StatusError {
		t.Fatalf("stale report was read: status %s, results %v", out.Status, out.Results)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("stale untracked report not deleted")
	}

	testutil.WriteFile(t, stale, `<testsuite/>`)
	testutil.Git(t, repo, "add", "reports/old.xml")
	testutil.Git(t, repo, "commit", "-q", "-m", "tracked report")
	out = Run(context.Background(), spec)
	if out.Status != StatusError || !strings.Contains(out.Problem, "git tracks") {
		t.Fatalf("tracked report: status %s, problem %q", out.Status, out.Problem)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("tracked file was deleted")
	}
}

// TestRunNeverDeletesOutsideWorktree reproduces a junit_path that reaches a
// user's files: absolute, through .., or through a symlinked directory.
func TestRunNeverDeletesOutsideWorktree(t *testing.T) {
	testutil.Isolate(t)
	tests := []struct {
		name  string
		setup func(t *testing.T, repo, victim string) string // returns junit_path
	}{
		{"absolute path", func(t *testing.T, repo, victim string) string { return filepath.Join(victim, "*.xml") }},
		{"parent dir", func(t *testing.T, repo, victim string) string {
			rel, err := filepath.Rel(repo, victim)
			if err != nil {
				t.Fatal(err)
			}
			return filepath.Join(rel, "*.xml")
		}},
		{"symlinked dir", func(t *testing.T, repo, victim string) string {
			if err := os.Symlink(victim, filepath.Join(repo, "reports")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return "reports/*.xml"
		}},
		{"symlinked file", func(t *testing.T, repo, victim string) string {
			if err := os.MkdirAll(filepath.Join(repo, "reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(victim, "keep.xml"), filepath.Join(repo, "reports", "link.xml")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return "reports/*.xml"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := testutil.Repo(t)
			victim := testutil.Resolve(t, t.TempDir())
			keep := filepath.Join(victim, "keep.xml")
			testutil.WriteFile(t, keep, "important")
			spec, _ := runSpec(t, "junit", "true")
			spec.Dir = repo
			spec.JunitPath = tt.setup(t, repo, victim)

			if err := CheckJunitPath(spec.Dir, spec.RunDir, spec.JunitPath); err == nil && tt.name != "symlinked file" {
				t.Errorf("CheckJunitPath(%q) accepted a path outside the worktree", spec.JunitPath)
			}
			out := Run(context.Background(), spec)
			if out.Status != StatusError || !strings.Contains(out.Problem, "outside") {
				t.Errorf("status %s, problem %q; want an error naming the path outside the worktree", out.Status, out.Problem)
			}
			if got, err := os.ReadFile(keep); err != nil || string(got) != "important" {
				t.Fatalf("user file outside the worktree was touched: %q, %v", got, err)
			}
		})
	}
}

func TestCheckJunitPathAcceptsWorktreeAndRunDir(t *testing.T) {
	spec, dir := runSpec(t, "junit", "true")
	for _, p := range []string{"{junit}", "reports/*.xml", "missing/dir/*.xml"} {
		if err := CheckJunitPath(dir, spec.RunDir, p); err != nil {
			t.Errorf("CheckJunitPath(%q): %v", p, err)
		}
	}
}

func TestRunCommandPathWithSpaces(t *testing.T) {
	spec, _ := runSpec(t, "junit", `printf '%s' '<testsuite><testcase classname="C" name="a"/></testsuite>' > {junit}`)
	spec.RunDir = filepath.Join(t.TempDir(), "dir with 'quote' and space", "1")
	if out := Run(context.Background(), spec); out.Status != StatusCompleted {
		t.Fatalf("status %s: %s", out.Status, out.Problem)
	}
}

// TestJunitPathScopedToXMLReports reproduces junit_path globs that reached
// dotfiles, the worktree's .git file, and other untracked files.
func TestJunitPathScopedToXMLReports(t *testing.T) {
	testutil.Isolate(t)
	t.Run("pattern that cannot match xml", func(t *testing.T) {
		repo := testutil.Repo(t)
		env := filepath.Join(repo, ".env")
		testutil.WriteFile(t, env, "SECRET=1")
		spec, _ := runSpec(t, "junit", "true")
		spec.Dir = repo
		spec.JunitPath = ".e?v"
		if err := CheckJunitPath(spec.Dir, spec.RunDir, spec.JunitPath); err == nil || !strings.Contains(err.Error(), ".xml") {
			t.Errorf("CheckJunitPath(%q) = %v, want an error naming .xml", spec.JunitPath, err)
		}
		out := Run(context.Background(), spec)
		if out.Status != StatusError || !strings.Contains(out.Problem, ".xml") {
			t.Errorf("status %s, problem %q; want an error naming .xml", out.Status, out.Problem)
		}
		if _, err := os.Stat(env); err != nil {
			t.Fatalf(".env was deleted: %v", err)
		}
	})
	t.Run("star", func(t *testing.T) {
		repo := testutil.Repo(t)
		gitFile := filepath.Join(repo, ".git")
		notes := filepath.Join(repo, "notes.txt")
		testutil.WriteFile(t, notes, "keep")
		spec, _ := runSpec(t, "junit", "true")
		spec.Dir = repo
		spec.JunitPath = "*"
		if err := CheckJunitPath(spec.Dir, spec.RunDir, spec.JunitPath); err == nil {
			t.Errorf("CheckJunitPath(%q) accepted a pattern that can match non-xml files", spec.JunitPath)
		}
		Run(context.Background(), spec)
		for _, p := range []string{gitFile, notes} {
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("%s was deleted: %v", p, err)
			}
		}
	})
	t.Run("star xml skips dotfiles, git and dirs", func(t *testing.T) {
		repo := testutil.Repo(t)
		keep := []string{
			filepath.Join(repo, ".hidden.xml"),
			filepath.Join(repo, ".git", "x.xml"),
			filepath.Join(repo, ".cache", "y.xml"),
			filepath.Join(repo, "dir.xml", "inner.txt"),
		}
		for _, p := range keep {
			testutil.WriteFile(t, p, "keep")
		}
		stale := filepath.Join(repo, "old.xml")
		testutil.WriteFile(t, stale, "<testsuite/>")
		spec, _ := runSpec(t, "junit", "true")
		spec.Dir = repo
		for _, pattern := range []string{"*.xml", "*/*.xml", ".git/*.xml"} {
			spec.JunitPath = pattern
			Run(context.Background(), spec)
		}
		for _, p := range keep {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s was deleted: %v", p, err)
			}
		}
		if _, err := os.Stat(stale); err == nil {
			t.Error("stale old.xml not deleted")
		}
	})
	t.Run("validates every match before deleting any", func(t *testing.T) {
		repo := testutil.Repo(t)
		first := filepath.Join(repo, "reports", "a.xml")
		testutil.WriteFile(t, first, "<testsuite/>")
		tracked := filepath.Join(repo, "reports", "b.xml")
		testutil.WriteFile(t, tracked, "<testsuite/>")
		testutil.Git(t, repo, "add", "reports/b.xml")
		testutil.Git(t, repo, "commit", "-q", "-m", "tracked report")
		spec, _ := runSpec(t, "junit", "true")
		spec.Dir = repo
		spec.JunitPath = "reports/*.xml"
		if out := Run(context.Background(), spec); out.Status != StatusError || !strings.Contains(out.Problem, "git tracks") {
			t.Fatalf("status %s, problem %q", out.Status, out.Problem)
		}
		if _, err := os.Stat(first); err != nil {
			t.Fatalf("a.xml deleted although b.xml was refused: %v", err)
		}
	})
}

func TestProcessStartIdentifiesProcess(t *testing.T) {
	a, err := ProcessStart(os.Getpid())
	if err != nil || a == "" {
		t.Fatalf("ProcessStart(self) = %q, %v", a, err)
	}
	if b, err := ProcessStart(os.Getpid()); err != nil || b != a {
		t.Fatalf("ProcessStart(self) changed: %q then %q (%v)", a, b, err)
	}
	if _, err := ProcessStart(-1); err == nil {
		t.Fatal("ProcessStart(-1) succeeded")
	}
}

func TestComparableStart(t *testing.T) {
	for _, tt := range []struct {
		name           string
		recorded, live string
		want           bool
	}{
		{"unparsable but equal", "x", "x", true},
		{"recorded unparsable", "garbage-not-a-date", "Wed Oct 28 10:11:12 2026", false},
		{"live unparsable", "Wed Oct 28 10:11:12 2026", " ", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ComparableStart(tt.recorded, tt.live); got != tt.want {
				t.Errorf("ComparableStart(%q, %q) = %v, want %v", tt.recorded, tt.live, got, tt.want)
			}
		})
	}
}

func TestSameStart(t *testing.T) {
	for _, tt := range []struct {
		name           string
		recorded, live string
		want           bool
	}{
		{"unparsable but equal", "x", "x", true},
		{"unparsable and different", "x", "y", true},
		{"recorded unparsable", "garbage-not-a-date", "Wed Oct 28 10:11:12 2026", true},
		{"live unparsable", "Wed Oct 28 10:11:12 2026", " ", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := SameStart(tt.recorded, tt.live); got != tt.want {
				t.Errorf("SameStart(%q, %q) = %v, want %v", tt.recorded, tt.live, got, tt.want)
			}
		})
	}
}

// TestRunHoldsSuiteUntilOnStartReturns: the suite's pid must be recorded
// before the suite does anything, or a shoalctl killed in between leaves a
// live suite that no later command knows about. Unix holds it at a pipe,
// Windows suspended.
func TestRunHoldsSuiteUntilOnStartReturns(t *testing.T) {
	spec, dir := runSpec(t, "exit-code", "echo ran > ran")
	ran := filepath.Join(dir, "ran")
	var early bool
	spec.OnStart = func(int) error {
		time.Sleep(300 * time.Millisecond)
		_, err := os.Stat(ran)
		early = err == nil
		return nil
	}
	if out := Run(context.Background(), spec); out.Status != StatusCompleted {
		t.Fatalf("status = %s (%s), want completed", out.Status, out.Problem)
	}
	if early {
		t.Fatal("the suite ran before OnStart returned")
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("the suite did not run after OnStart returned: %v", err)
	}
}

func TestRunRefusesSuiteWhenOnStartFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sh")
	}
	spec, dir := runSpec(t, "exit-code", "echo ran > ran")
	spec.OnStart = func(int) error { return errors.New("db is locked") }
	out := Run(context.Background(), spec)
	if out.Status != StatusError || !strings.Contains(out.Problem, "db is locked") {
		t.Fatalf("outcome = %s (%s), want error naming the OnStart failure", out.Status, out.Problem)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); !os.IsNotExist(err) {
		t.Fatalf("the suite ran although its pid was not recorded: %v", err)
	}
}
