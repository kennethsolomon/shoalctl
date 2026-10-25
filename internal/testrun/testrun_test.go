package testrun

import (
	"context"
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

func TestCommandHash(t *testing.T) {
	if CommandHash("ab", "c", "") == CommandHash("a", "bc", "") {
		t.Fatal("hash must separate fields")
	}
	if CommandHash("x", "junit", "") != CommandHash("x", "junit", "") {
		t.Fatal("hash must be deterministic")
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

func TestRunCommandPathWithSpaces(t *testing.T) {
	spec, _ := runSpec(t, "junit", `printf '%s' '<testsuite><testcase classname="C" name="a"/></testsuite>' > {junit}`)
	spec.RunDir = filepath.Join(t.TempDir(), "dir with 'quote' and space", "1")
	if out := Run(context.Background(), spec); out.Status != StatusCompleted {
		t.Fatalf("status %s: %s", out.Status, out.Problem)
	}
}
