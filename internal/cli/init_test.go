package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func TestSlugWords(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Users page sort broken", "users-page-sort-broken"},
		{"  Fix: the BUG!! (again) ", "fix-the-bug-again"},
		{"日本語", "task"},
		{"---", "task"},
		{"a very long title that keeps going past the forty character limit", "a-very-long-title-that-keeps-going-past"},
		{strings.Repeat("x", 50), strings.Repeat("x", 40)},
		{"exactly forty chars long title here abcd", "exactly-forty-chars-long-title-here-abcd"},
	}
	for _, tt := range tests {
		got := slugWords(tt.in)
		if got != tt.want {
			t.Errorf("slugWords(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if len(got) > 40 {
			t.Errorf("slugWords(%q) is %d chars", tt.in, len(got))
		}
	}
}

func TestIsShoalCommand(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"/usr/local/bin/shoalctl hook session-start", true},
		{"'/Users/me/my bins/shoalctl' hook stop", true},
		{`C:\tools\shoalctl.exe hook stop`, true},
		{"shoalctl hook", true},
		{"/usr/local/bin/shoalctl status", false},
		{"/usr/bin/other hook stop", false},
		{"echo shoalctl hook", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isShoalCommand(tt.cmd); got != tt.want {
			t.Errorf("isShoalCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
		}
	}
	if got := hookCommand("/my bins/shoalctl", "stop"); got != "'/my bins/shoalctl' hook stop" || !isShoalCommand(got) {
		t.Errorf("hookCommand with a space = %q", got)
	}
}

func hookCommands(t *testing.T, data []byte) map[string][]string {
	t.Helper()
	var top struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("settings not JSON: %v\n%s", err, data)
	}
	out := map[string][]string{}
	for ev, groups := range top.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				out[ev] = append(out[ev], fmt.Sprintf("%s|%d|%s", h.Command, h.Timeout, g.Matcher))
			}
		}
	}
	return out
}

func TestMergeHooks(t *testing.T) {
	exe := "/bin/shoalctl"
	start := "/bin/shoalctl hook session-start|10|"
	stop := "/bin/shoalctl hook stop|10|"
	tests := []struct {
		name  string
		in    string
		want  map[string][]string
		other string // a top-level key that must survive
	}{
		{"empty file", "", map[string][]string{"SessionStart": {start}, "Stop": {stop}}, ""},
		{"keeps other keys and hooks", `{"permissions": {"allow": ["Bash(ls)"]}, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "notify-send done"}]}], "PreToolUse": [{"matcher": "Edit", "hooks": [{"type": "command", "command": "lint"}]}]}}`,
			map[string][]string{"SessionStart": {start}, "Stop": {"notify-send done|0|", stop}, "PreToolUse": {"lint|0|Edit"}}, "permissions"},
		{"replaces an old binary path", `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "/old/shoalctl hook session-start", "timeout": 10}]}]}}`,
			map[string][]string{"SessionStart": {start}, "Stop": {stop}}, ""},
		{"collapses duplicates and keeps neighbours", `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/bin/shoalctl hook stop", "timeout": 10}, {"type": "command", "command": "keep-me"}]}, {"hooks": [{"type": "command", "command": "/bin/shoalctl hook stop", "timeout": 10}]}]}}`,
			map[string][]string{"SessionStart": {start}, "Stop": {"keep-me|0|", stop}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, changed, err := mergeHooks([]byte(tt.in), exe)
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			got := hookCommands(t, out)
			for ev, want := range tt.want {
				if fmt.Sprint(got[ev]) != fmt.Sprint(want) {
					t.Errorf("%s = %v, want %v", ev, got[ev], want)
				}
			}
			if tt.other != "" && !strings.Contains(string(out), `"`+tt.other+`"`) {
				t.Errorf("lost key %q:\n%s", tt.other, out)
			}
			again, changed, err := mergeHooks(out, exe)
			if err != nil || changed || string(again) != string(out) {
				t.Fatalf("second merge changed the file (changed=%v err=%v)", changed, err)
			}
		})
	}
}

func TestMergeHooksRejectsInvalid(t *testing.T) {
	for _, in := range []string{"not json", "[1, 2]", `{"hooks": []}`, `{"hooks": {"Stop": {"x": 1}}}`, `{"hooks": {"Stop": [{"hooks": "nope"}]}}`, "null"} {
		if _, _, err := mergeHooks([]byte(in), "/bin/shoalctl"); err == nil {
			t.Errorf("mergeHooks(%q): want error", in)
		}
	}
}

func checksum(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func TestInitIsIdempotent(t *testing.T) {
	h := newHarness(t)
	settings := filepath.Join(h.repo, ".claude", "settings.local.json")
	testutil.WriteFile(t, settings, `{"model": "keep-me", "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "say done"}]}]}}`)

	out := h.mustRun("init")
	p := h.project()
	for _, want := range []string{p.id, p.dbPath(), filepath.Join(p.dir, "config.toml"), settings} {
		if !strings.Contains(out, want) {
			t.Errorf("init output missing %q:\n%s", want, out)
		}
	}
	exclude := filepath.Join(h.repo, ".git", "info", "exclude")
	files := []string{settings, exclude, p.metaPath(), filepath.Join(p.dir, "config.toml")}
	before := map[string]string{}
	for _, f := range files {
		before[f] = checksum(t, f)
	}
	h.mustRun("init")
	for _, f := range files {
		if checksum(t, f) != before[f] {
			t.Errorf("second init changed %s", f)
		}
	}

	got := hookCommands(t, []byte(readFile(t, settings)))
	if fmt.Sprint(got["Stop"]) != fmt.Sprint([]string{"say done|0|", fakeExe + " hook stop|10|"}) {
		t.Errorf("Stop hooks = %v", got["Stop"])
	}
	if !strings.Contains(readFile(t, settings), `"keep-me"`) {
		t.Error("init dropped an existing settings key")
	}
	ex := readFile(t, exclude)
	for _, line := range []string{"/.claude/settings.local.json", "/.shoal/"} {
		if strings.Count(ex, line+"\n") != 1 {
			t.Errorf("exclude has %q %d times:\n%s", line, strings.Count(ex, line+"\n"), ex)
		}
	}
	if status := testutil.Git(t, h.repo, "status", "--porcelain"); status != "" {
		t.Errorf("init left the repo dirty:\n%s", status)
	}

	for _, d := range []string{p.home, p.dir} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Errorf("%s mode %o, want 700", d, info.Mode().Perm())
		}
	}
	var meta projectMeta
	if err := json.Unmarshal([]byte(readFile(t, p.metaPath())), &meta); err != nil || meta.Root != h.repo || meta.ID != p.id {
		t.Fatalf("project.json = %+v, %v", meta, err)
	}
	if want := gitx.ProjectID(gitx.Canonical(testutil.Git(t, h.repo, "remote", "get-url", "origin"))); p.id != want {
		t.Errorf("project id %s, want hash of the remote %s", p.id, want)
	}
}

func TestInitRefusesInvalidSettings(t *testing.T) {
	h := newHarness(t)
	settings := filepath.Join(h.repo, ".claude", "settings.local.json")
	testutil.WriteFile(t, settings, `{"hooks": oops`)
	h.expectCode(ExitConfig, "init")
	if got := readFile(t, settings); got != `{"hooks": oops` {
		t.Fatalf("invalid settings were overwritten: %q", got)
	}
}

func TestInitRefusesTrackedLocalSettings(t *testing.T) {
	h := newHarness(t)
	settings := filepath.Join(h.repo, ".claude", "settings.local.json")
	const body = `{"permissions": {"allow": ["Bash(ls)"]}}`
	testutil.WriteFile(t, settings, body)
	testutil.Git(t, h.repo, "add", ".claude/settings.local.json")
	testutil.Git(t, h.repo, "commit", "-q", "-m", "track local settings")
	exclude := filepath.Join(h.repo, ".git", "info", "exclude")
	excludeBefore, _ := os.ReadFile(exclude)

	errOut := h.expectCode(ExitConfig, "init")
	for _, want := range []string{"tracked", "git rm --cached .claude/settings.local.json"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q: %s", want, errOut)
		}
	}
	if got := readFile(t, settings); got != body {
		t.Errorf("settings changed: %s", got)
	}
	if after, _ := os.ReadFile(exclude); string(after) != string(excludeBefore) {
		t.Errorf("info/exclude changed:\n%s", after)
	}
	if status := testutil.Git(t, h.repo, "status", "--porcelain"); status != "" {
		t.Errorf("repo dirty after refused init:\n%s", status)
	}
	if _, err := os.Stat(h.project().dir); !os.IsNotExist(err) {
		t.Errorf("refused init left project state behind: %v", err)
	}
}

func TestInitRefusesShoalHooksInCommittedSettings(t *testing.T) {
	h := newHarness(t)
	testutil.WriteFile(t, filepath.Join(h.repo, ".claude", "settings.json"),
		`{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/x/shoalctl hook stop"}]}]}}`)
	if errOut := h.expectCode(ExitConfig, "init"); !strings.Contains(errOut, "fire twice") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestInitRejectsNonRepos(t *testing.T) {
	h := newHarness(t)
	h.env.Dir = t.TempDir()
	h.expectCode(ExitConfig, "init")

	bare := filepath.Join(t.TempDir(), "b.git")
	testutil.Git(t, filepath.Dir(bare), "init", "-q", "--bare", bare)
	h.env.Dir = bare
	h.expectCode(ExitConfig, "init")
}

func TestInitRebind(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	p := h.project()
	meta := readFile(t, p.metaPath())
	testutil.WriteFile(t, p.metaPath(), strings.Replace(meta, h.repo, "/somewhere/else", 1))

	h.expectCode(ExitConfig, "init")
	h.expectCode(ExitConfig, "status")
	h.mustRun("init", "--rebind")
	h.mustRun("status")
}

func TestInitGoModuleTemplateAndCommitConfig(t *testing.T) {
	h := newHarness(t)
	testutil.WriteFile(t, filepath.Join(h.repo, "go.mod"), "module example.com/x\n")
	h.mustRun("init", "--commit-config")
	cfg := readFile(t, filepath.Join(h.repo, ".shoal.toml"))
	if !strings.Contains(cfg, `command = "go test -json ./..."`) || !strings.Contains(cfg, `format = "go-json"`) {
		t.Fatalf(".shoal.toml:\n%s", cfg)
	}
	if _, err := os.Stat(filepath.Join(h.project().dir, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("stealth config written alongside .shoal.toml: %v", err)
	}
	h.mustRun("init")
}

func TestInitWithoutRemoteUsesPath(t *testing.T) {
	h := newHarness(t)
	testutil.Git(t, h.repo, "remote", "remove", "origin")
	h.mustRun("init")
	if p := h.project(); p.id != gitx.ProjectID(h.repo) || p.remote != h.repo {
		t.Fatalf("id %s remote %s, want hash of %s", p.id, p.remote, h.repo)
	}
}

func TestNew(t *testing.T) {
	h := newHarness(t)
	h.expectCode(ExitConfig, "new", "feature", "Before init")
	h.mustRun("init")

	if out := h.mustRun("new", "feature", "Users page sort broken"); out != "1-users-page-sort-broken\n" {
		t.Fatalf("slug output %q", out)
	}
	askFile := filepath.Join(t.TempDir(), "ask.md")
	testutil.WriteFile(t, askFile, "Fix the crash\nwhen sorting.\n")
	h.mustRun("new", "--ask-file", askFile, "bugfix", "Crash")
	h.mustRun("new", "hotfix", "Prod down", "--ask", "Restore login")
	testutil.WriteFile(t, filepath.Join(h.repo, "notes", "ask.md"), "Relative ask\n")
	h.mustRun("new", "feature", "Relative", "--ask-file", filepath.Join("notes", "ask.md"))

	tests := []struct {
		slug, lane, ask string
	}{
		{"1-users-page-sort-broken", "feature", "Users page sort broken"},
		{"2-crash", "bugfix", "Fix the crash\nwhen sorting."},
		{"3-prod-down", "hotfix", "Restore login"},
		{"4-relative", "feature", "Relative ask"},
	}
	for _, tt := range tests {
		got := h.task(tt.slug)
		if got.Lane != tt.lane || got.Ask != tt.ask || got.Stage != "new" {
			t.Errorf("%s: %+v", tt.slug, got)
		}
	}

	for _, args := range [][]string{
		{"new", "chore", "x"},
		{"new", "feature"},
		{"new", "feature", ""},
		{"new", "feature", strings.Repeat("x", 121)},
		{"new", "feature", "two\nlines"},
		{"new", "feature", "x", "--ask", "a", "--ask-file", askFile},
		{"new", "feature", "x", "--nope"},
		{"new", "feature", "x", "--ask-file", filepath.Join(t.TempDir(), "missing")},
	} {
		h.expectCode(ExitUsage, args...)
	}
}

func TestUsageAndNotFound(t *testing.T) {
	h := newHarness(t)
	h.expectCode(ExitUsage)
	h.expectCode(ExitUsage, "frobnicate")
	h.expectCode(ExitUsage, "status", "extra")
	h.expectCode(ExitUsage, "version", "x")
	if out := h.mustRun("version"); out != "shoalctl test\n" {
		t.Fatalf("version output %q", out)
	}
	h.mustRun("init")
	h.expectCode(ExitNotFound, "start", "99")
	h.expectCode(ExitNotFound, "done", "no-such-task")
}

func TestStatusEmptyAndJSON(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	if out := h.mustRun("status", "--json"); strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty status json = %q", out)
	}
	h.mustRun("new", "feature", "One")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(h.mustRun("status", "--json")), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("status json: %v %v", rows, err)
	}
	for _, k := range []string{"id", "slug", "lane", "stage", "base", "baseline", "window", "last_seen", "worktree"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("status json lacks %q", k)
		}
	}
	if rows[0]["baseline"] != "-" || rows[0]["window"] != "-" {
		t.Errorf("unstarted task row: %v", rows[0])
	}
	if out := h.mustRun("status"); !strings.Contains(out, "SLUG") || !strings.Contains(out, "1-one") {
		t.Fatalf("status table:\n%s", out)
	}
}
