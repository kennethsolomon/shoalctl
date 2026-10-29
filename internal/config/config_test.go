package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsWhenFilesMissing(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "none.toml"), filepath.Join(dir, "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Session.Model != "claude-opus-5-5" || cfg.Merge.Base != "main" || cfg.Tests.Format != "junit" || cfg.TestTimeout() != 30*time.Minute {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	global := write(t, dir, "global.toml", "[session]\nmodel = \"global-model\"\n[reports]\nsink = \"local\"\n[merge]\nbase = \"develop\"\n")
	project := write(t, dir, "project.toml", "[session]\nmodel = \"project-model\"\n")
	cfg, err := Load(global, project)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Session.Model != "project-model" {
		t.Errorf("model = %q, want project-model", cfg.Session.Model)
	}
	if cfg.Merge.Base != "develop" {
		t.Errorf("base = %q, want develop from global", cfg.Merge.Base)
	}
	if cfg.Tests.Timeout != "30m" {
		t.Errorf("timeout = %q, want default", cfg.Tests.Timeout)
	}
}

func TestJunitPathInsideWorktreeAccepted(t *testing.T) {
	for _, p := range []string{"{junit}", "reports/*.xml", "build/test-results/**/*.xml", "a..b/x.xml", "./out.xml"} {
		dir := t.TempDir()
		path := write(t, dir, "p.toml", "[tests]\njunit_path = \""+p+"\"\n")
		if _, err := Load(filepath.Join(dir, "g.toml"), path); err != nil {
			t.Errorf("junit_path %q: %v", p, err)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"unknown key", "[tests]\ncomand = \"x\"\n", "unknown keys: tests.comand"},
		{"unknown table", "[nope]\nx = 1\n", "unknown keys"},
		{"bad format", "[tests]\nformat = \"xml\"\n", "[tests].format"},
		{"bad timeout", "[tests]\ntimeout = \"soon\"\n", "[tests].timeout"},
		{"negative retries", "[tests]\nflaky_retries = -1\n", "flaky_retries"},
		{"stealth off", "[install]\nstealth = false\n", "Phase 4"},
		{"bad sink", "[reports]\nsink = \"cloud\"\n", "[reports].sink"},
		{"notes without path", "[reports]\nsink = \"notes\"\n", "notes_path"},
		{"bad base", "[merge]\nbase = \"-x\"\n", "[merge].base"},
		{"bad strategy", "[merge]\nstrategy = \"merge\"\n", "[merge].strategy"},
		{"bad prod ref", "[hotfix]\nproduction_ref = \"a b\"\n", "production_ref"},
		{"empty model", "[session]\nmodel = \"\"\n", "[session].model"},
		{"type mismatch", "[tests]\nflaky_retries = \"two\"\n", "parse"},
		{"syntax", "[tests\n", "parse"},
		{"absolute junit_path", "[tests]\njunit_path = \"/home/u/victim/*.xml\"\n", "[tests].junit_path"},
		{"windows absolute junit_path", "[tests]\njunit_path = 'C:\\victim\\*.xml'\n", "[tests].junit_path"},
		{"rooted junit_path", "[tests]\njunit_path = '\\victim\\*.xml'\n", "[tests].junit_path"},
		{"parent junit_path", "[tests]\njunit_path = \"../victim/*.xml\"\n", "[tests].junit_path"},
		{"inner parent junit_path", "[tests]\njunit_path = \"reports/../../victim.xml\"\n", "[tests].junit_path"},
		{"backslash parent junit_path", "[tests]\njunit_path = 'reports\\..\\..\\x.xml'\n", "[tests].junit_path"},
		{"junit placeholder escape", "[tests]\njunit_path = \"{junit}/../../x.xml\"\n", "[tests].junit_path"},
		{"home junit_path", "[tests]\njunit_path = \"~/victim/*.xml\"\n", "[tests].junit_path"},
		{"dotfile junit_path", "[tests]\njunit_path = \".e?v\"\n", "must end in .xml"},
		{"star junit_path", "[tests]\njunit_path = \"*\"\n", "must end in .xml"},
		{"git junit_path", "[tests]\njunit_path = \".git/*.xml\"\n", ".git"},
		{"blank setup", "[tests]\nsetup = \"  \"\n", "[tests].setup"},
		{"setup with junit", "[tests]\nsetup = \"cp {junit} .\"\n", "[tests].setup"},
		{"setup with tests", "[tests]\nsetup = \"echo {tests}\"\n", "[tests].setup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			p := write(t, dir, "p.toml", tt.body)
			_, err := Load(filepath.Join(dir, "g.toml"), p)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestProjectPath(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	stealth := filepath.Join(home, "config.toml")

	if got, err := ProjectPath(repo, stealth); err != nil || got != stealth {
		t.Fatalf("neither exists: %q, %v", got, err)
	}
	write(t, repo, ".shoal.toml", "")
	if got, err := ProjectPath(repo, stealth); err != nil || got != filepath.Join(repo, ".shoal.toml") {
		t.Fatalf("committed only: %q, %v", got, err)
	}
	write(t, home, "config.toml", "")
	if _, err := ProjectPath(repo, stealth); err == nil {
		t.Fatal("both exist: want error")
	}
}

func TestTemplateParses(t *testing.T) {
	for _, goMod := range []bool{true, false} {
		dir := t.TempDir()
		p := write(t, dir, "c.toml", Template(goMod))
		cfg, err := Load(filepath.Join(dir, "g.toml"), p)
		if err != nil {
			t.Fatalf("template (go=%v) does not load: %v", goMod, err)
		}
		if goMod && (cfg.Tests.Format != "go-json" || cfg.Tests.Command != "go test -json ./...") {
			t.Fatalf("go template: %+v", cfg.Tests)
		}
		if !goMod && cfg.Tests.Command != "" {
			t.Fatalf("generic template has a command: %q", cfg.Tests.Command)
		}
	}
}
