package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/config"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func countRows(t *testing.T, p *project, query string, args ...any) int {
	t.Helper()
	db := openRaw(t, p.dbPath())
	defer db.Close()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func claudeLaunches(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, block := range strings.Split(strings.TrimSuffix(string(data), "--\n"), "--\n") {
		out = append(out, strings.Split(strings.TrimSuffix(block, "\n"), "\n"))
	}
	return out
}

func (h *harness) tmuxOut(args ...string) string {
	h.t.Helper()
	out, err := exec.Command("tmux", append(append([]string{}, h.env.TmuxArgs...), args...)...).Output()
	if err != nil {
		h.t.Fatalf("tmux %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestStartTwoTasksShareBaseline(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	h.mustRun("new", "feature", "Add export")
	h.mustRun("new", "bugfix", "Fix sort")

	out := h.mustRun("start", "1-add-export")
	if !strings.Contains(out, "attach with: tmux attach -t shoal-"+p.id[:8]) {
		t.Errorf("first start should print the attach command:\n%s", out)
	}
	out = h.mustRun("start", "2")
	if !strings.Contains(out, "reusing run") || strings.Contains(out, "attach with") {
		t.Errorf("second start should reuse the baseline and the session:\n%s", out)
	}
	if n := countRows(t, p, `SELECT count(*) FROM test_runs WHERE kind = 'baseline'`); n != 1 {
		t.Fatalf("baseline rows = %d, want 1", n)
	}

	windows := h.tmuxOut("list-windows", "-a", "-F", "#{window_id} #{window_name} #{pane_current_path}")
	for _, slug := range []string{"1-add-export", "2-fix-sort"} {
		task := h.task(slug)
		if task.Stage != "in-progress" || task.Model != config.DefaultModel || task.TmuxWindow == "" || task.BaselineRunID == 0 {
			t.Errorf("%s after start: %+v", slug, task)
		}
		if task.Worktree != p.worktree(slug) || task.Branch != task.Lane+"/"+slug || task.BaseRef != "main" {
			t.Errorf("%s worktree/branch: %+v", slug, task)
		}
		if !strings.Contains(windows, task.TmuxWindow+" "+slug+" ") {
			t.Errorf("window %s for %s not found in:\n%s", task.TmuxWindow, slug, windows)
		}
		if got := testutil.Git(t, task.Worktree, "rev-parse", "HEAD"); got != task.BaseSHA {
			t.Errorf("%s worktree at %s, want %s", slug, got, task.BaseSHA)
		}
		var tf taskFile
		if err := json.Unmarshal([]byte(readFile(t, filepath.Join(task.Worktree, ".shoal", "task.json"))), &tf); err != nil ||
			tf.ProjectID != p.id || tf.TaskID != task.ID || tf.Slug != slug {
			t.Errorf("task.json = %+v, %v", tf, err)
		}
		if status := testutil.Git(t, task.Worktree, "status", "--porcelain"); status != "" {
			t.Errorf("%s worktree dirty after start (.shoal must be excluded):\n%s", slug, status)
		}
		brief := readFile(t, filepath.Join(task.Worktree, ".shoal", "brief.md"))
		for _, want := range []string{"# Task " + slug, "Lane: " + task.Lane, "1 pass, 1 fail, 1 skip", "- C::pre", "shoalctl done " + slug, "sh ./test.sh {junit}"} {
			if !strings.Contains(brief, want) {
				t.Errorf("brief for %s lacks %q:\n%s", slug, want, brief)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(h.task("1").Worktree, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Errorf("start must not write hook settings into a worktree: %v", err)
	}

	waitFor(t, func() bool { return len(claudeLaunches(t, h.argsLog)) == 2 })
	for _, args := range claudeLaunches(t, h.argsLog) {
		want := []string{"--model", config.DefaultModel, sessionPrompt}
		if strings.Join(args, "|") != strings.Join(want, "|") {
			t.Errorf("claude args = %q, want %q", args, want)
		}
	}

	var rows []statusRow
	if err := json.Unmarshal([]byte(h.mustRun("status", "--json")), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Window != "alive" || r.Baseline != "1/1F/1S" || !strings.HasPrefix(r.Base, "main@") {
			t.Errorf("status row %+v", r)
		}
	}
	h.tmuxOut("kill-window", "-t", h.task("1").TmuxWindow)
	if err := json.Unmarshal([]byte(h.mustRun("status", "--json")), &rows); err != nil {
		t.Fatal(err)
	}
	if rows[1].Slug != "1-add-export" || rows[1].Window != "gone" || rows[0].Window != "alive" {
		t.Errorf("after killing window 1: %+v", rows)
	}

	h.expectCode(ExitState, "start", "1")
}

func TestStartModelOverride(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig + "[session]\nmodel = \"config-model\"\n")
	h.mustRun("new", "feature", "One")
	h.mustRun("new", "feature", "Two")
	h.mustRun("start", "1")
	h.mustRun("start", "2", "--model", "claude-other")
	if m := h.task("1").Model; m != "config-model" {
		t.Errorf("task 1 model %q, want config-model", m)
	}
	if m := h.task("2").Model; m != "claude-other" {
		t.Errorf("task 2 model %q, want claude-other", m)
	}
	waitFor(t, func() bool { return len(claudeLaunches(t, h.argsLog)) == 2 })
	launched := map[string]bool{}
	for _, args := range claudeLaunches(t, h.argsLog) {
		launched[args[1]] = true
	}
	if !launched["config-model"] || !launched["claude-other"] {
		t.Errorf("launched models %v", launched)
	}
	h.expectCode(ExitUsage, "start", "1", "--model", "--dangerous")
}

func TestStartBaselineFailedThenResume(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith("[tests]\ncommand = \"echo no junit here; exit 1\"\nformat = \"junit\"\n")
	h.mustRun("new", "bugfix", "Crash")

	errOut := h.expectCode(ExitBaseline, "start", "1")
	task := h.task("1")
	if task.Stage != "baseline-failed" || task.TmuxWindow != "" {
		t.Fatalf("after failed baseline: %+v", task)
	}
	if !strings.Contains(errOut, "output.log") {
		t.Errorf("stderr should name output.log: %s", errOut)
	}
	if _, err := os.Stat(task.Worktree); err != nil {
		t.Fatalf("worktree must stay in place: %v", err)
	}
	if len(claudeLaunches(t, h.argsLog)) != 0 {
		t.Fatal("claude launched without a baseline")
	}

	testutil.WriteFile(t, filepath.Join(p.dir, "config.toml"), junitConfig)
	h.mustRun("start", "1")
	if got := h.task("1"); got.Stage != "in-progress" || got.BaseSHA != task.BaseSHA {
		t.Fatalf("after resume: %+v", got)
	}
}

func TestStartTrustPreflight(t *testing.T) {
	tests := []struct {
		name, claudeJSON, want string
	}{
		{"missing ~/.claude.json", "", "does not exist"},
		{"malformed ~/.claude.json", "{nope", "not valid JSON"},
		{"main checkout not trusted", `{"projects": {"/somewhere/else": {"hasTrustDialogAccepted": true}}}`, "has not trusted"},
		{"trust explicitly false", `{"projects": {"REPO": {"hasTrustDialogAccepted": false}}}`, "has not trusted"},
		{"only the parent folder trusted", `{"projects": {"PARENT": {"hasTrustDialogAccepted": true}}}`, "has not trusted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t).withSession()
			expand := strings.NewReplacer("PARENT", filepath.Dir(h.repo), "REPO", h.repo).Replace
			claudeJSON := filepath.Join(h.home, ".claude.json")
			os.Remove(claudeJSON)
			if tt.claudeJSON != "" {
				testutil.WriteFile(t, claudeJSON, expand(tt.claudeJSON))
			}
			h.initWith(junitConfig)
			h.mustRun("new", "feature", "One")
			errOut := h.expectCode(ExitUntrusted, "start", "1")
			if !strings.Contains(errOut, tt.want) || !strings.Contains(errOut, "run `claude` once in "+h.repo) {
				t.Fatalf("stderr: %s", errOut)
			}
			if task := h.task("1"); task.Stage != "new" || task.Worktree != "" {
				t.Fatalf("preflight must run before any change: %+v", task)
			}
			if tt.claudeJSON != "" && readFile(t, claudeJSON) != expand(tt.claudeJSON) {
				t.Fatal("start modified ~/.claude.json")
			}
		})
	}
}

func TestStartHonoursClaudeConfigDir(t *testing.T) {
	h := newHarness(t).withSession()
	os.Remove(filepath.Join(h.home, ".claude.json"))
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	testutil.TrustProject(t, dir, h.repo)
	h.initWith(junitConfig)
	h.mustRun("new", "feature", "One")
	h.mustRun("start", "1")
}

func TestStartDependencyAndConfigErrors(t *testing.T) {
	h := newHarness(t).withSession()
	p := h.initWith(junitConfig)
	h.mustRun("new", "feature", "One")

	claude := h.env.Claude
	h.env.Claude = filepath.Join(t.TempDir(), "no-claude")
	h.expectCode(ExitDependency, "start", "1")
	h.env.Claude = claude

	tmuxBin := h.env.Tmux
	h.env.Tmux = "no-such-tmux-binary"
	h.expectCode(ExitDependency, "start", "1")
	h.env.Tmux = tmuxBin

	testutil.WriteFile(t, filepath.Join(p.dir, "config.toml"), "[tests]\ncommand = \"\"\n")
	h.expectCode(ExitConfig, "start", "1")
	testutil.WriteFile(t, filepath.Join(p.dir, "config.toml"), junitConfig+"[merge]\nbase = \"develop\"\n")
	h.expectCode(ExitConfig, "start", "1")
	testutil.WriteFile(t, filepath.Join(p.dir, "config.toml"), junitConfig+"[tests]\nbogus = 1\n")
	h.expectCode(ExitConfig, "start", "1")
	if task := h.task("1"); task.Stage != "new" {
		t.Fatalf("failed starts changed the task: %+v", task)
	}
}

func TestStartRefusesForeignBranch(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	h.mustRun("new", "feature", "One")
	testutil.Git(t, h.repo, "branch", "feature/1-one")
	h.expectCode(ExitState, "start", "1")
	if out := testutil.Git(t, h.repo, "rev-parse", "feature/1-one"); out != testutil.Git(t, h.repo, "rev-parse", "main") {
		t.Fatal("start touched an existing branch")
	}
}

func TestStartNoneFormat(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith("[tests]\nformat = \"none\"\n")
	h.mustRun("new", "feature", "One")
	h.mustRun("start", "1")
	brief := readFile(t, filepath.Join(h.task("1").Worktree, ".shoal", "brief.md"))
	if !strings.Contains(brief, "Baseline (none)") || !strings.Contains(brief, "protocol is inactive") {
		t.Fatalf("brief:\n%s", brief)
	}
	var rows []statusRow
	json.Unmarshal([]byte(h.mustRun("status", "--json")), &rows)
	if rows[0].Baseline != "none" {
		t.Fatalf("status baseline %q", rows[0].Baseline)
	}
}

func TestResolveBase(t *testing.T) {
	testutil.Isolate(t)
	repo := testutil.Repo(t)
	main := testutil.Git(t, repo, "rev-parse", "HEAD")
	testutil.WriteFile(t, filepath.Join(repo, "b.txt"), "b")
	testutil.Git(t, repo, "add", "b.txt")
	testutil.Git(t, repo, "commit", "-q", "-m", "second")
	second := testutil.Git(t, repo, "rev-parse", "HEAD")
	testutil.Git(t, repo, "branch", "production", main)
	testutil.Git(t, repo, "tag", "v1.9.0", main)
	testutil.Git(t, repo, "tag", "-a", "-m", "release", "v1.10.0", second)
	testutil.Git(t, repo, "tag", "both", main)
	testutil.Git(t, repo, "branch", "both", second)

	tests := []struct {
		lane, base, prod, wantRef, wantSHA string
		wantErr                            bool
	}{
		{"feature", "main", "", "main", second, false},
		{"bugfix", "missing", "", "", "", true},
		{"hotfix", "main", "", "", "", true},
		{"hotfix", "main", "production", "production", main, false},
		{"hotfix", "main", "v1.9.0", "refs/tags/v1.9.0", main, false},
		{"hotfix", "main", "tag:v1.*", "refs/tags/v1.10.0", second, false},
		{"hotfix", "main", "tag:v9.*", "", "", true},
		{"hotfix", "main", "both", "", "", true},
		{"hotfix", "main", "nowhere", "", "", true},
	}
	for _, tt := range tests {
		cfg := config.Default()
		cfg.Merge.Base, cfg.Hotfix.ProductionRef = tt.base, tt.prod
		p := &project{root: repo, cfg: cfg, cfgPath: "config.toml"}
		ref, sha, err := resolveBase(context.Background(), p, tt.lane)
		if tt.wantErr {
			if codeOf(err) != ExitConfig {
				t.Errorf("%s %q/%q: got %q %q %v, want config error", tt.lane, tt.base, tt.prod, ref, sha, err)
			}
			continue
		}
		if err != nil || ref != tt.wantRef || sha != tt.wantSHA {
			t.Errorf("%s %q/%q: got %q %q %v, want %q %q", tt.lane, tt.base, tt.prod, ref, sha, err, tt.wantRef, tt.wantSHA)
		}
	}
}
