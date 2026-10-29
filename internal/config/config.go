// Package config loads and validates shoal's TOML configuration (spec §4.3, §15.1).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const DefaultModel = "claude-opus-5-5"

type Config struct {
	Stack   Stack   `toml:"stack"`
	Tests   Tests   `toml:"tests"`
	Session Session `toml:"session"`
	Install Install `toml:"install"`
	Lanes   Lanes   `toml:"lanes"`
	Merge   Merge   `toml:"merge"`
	Hotfix  Hotfix  `toml:"hotfix"`
	Claims  Claims  `toml:"claims"`
	Reports Reports `toml:"reports"`
	Privacy Privacy `toml:"privacy"`
}

type Stack struct {
	Name  string `toml:"name"`
	Lint  string `toml:"lint"`
	Build string `toml:"build"`
}

type Tests struct {
	Command        string `toml:"command"`
	Format         string `toml:"format"`
	JunitPath      string `toml:"junit_path"`
	RerunCommand   string `toml:"rerun_command"`
	FlakyRetries   int    `toml:"flaky_retries"`
	Timeout        string `toml:"timeout"`
	CoverageFormat string `toml:"coverage_format"`
	CoveragePath   string `toml:"coverage_path"`
}

type Session struct {
	Model string `toml:"model"`
}

type Install struct {
	Stealth bool `toml:"stealth"`
}

type Lanes struct {
	QAMaxRounds int `toml:"qa_max_rounds"`
}

type Merge struct {
	Base     string `toml:"base"`
	Strategy string `toml:"strategy"`
}

type Hotfix struct {
	ProductionRef string `toml:"production_ref"`
	Backport      bool   `toml:"backport"`
	MaxDiffLines  int    `toml:"max_diff_lines"`
}

type Claims struct {
	StaleAfter string `toml:"stale_after"`
}

type Reports struct {
	Sink      string `toml:"sink"`
	NotesPath string `toml:"notes_path"`
}

type Privacy struct {
	DenyGlobs []string `toml:"deny_globs"`
}

func Default() Config {
	return Config{
		Tests: Tests{
			Format:       "junit",
			JunitPath:    "{junit}",
			FlakyRetries: 2,
			Timeout:      "30m",
		},
		Session: Session{Model: DefaultModel},
		Install: Install{Stealth: true},
		Lanes:   Lanes{QAMaxRounds: 3},
		Merge:   Merge{Base: "main", Strategy: "rebase"},
		Hotfix:  Hotfix{Backport: true, MaxDiffLines: 100},
		Claims:  Claims{StaleAfter: "2h"},
		Reports: Reports{Sink: "local"},
		Privacy: Privacy{DenyGlobs: []string{".env*", "storage/**"}},
	}
}

// ProjectPath picks the project config: <repo>/.shoal.toml if it exists, else
// the stealth path. Both existing at once is an error.
func ProjectPath(repoRoot, stealthPath string) (string, error) {
	committed := filepath.Join(repoRoot, ".shoal.toml")
	cOK, err := exists(committed)
	if err != nil {
		return "", err
	}
	sOK, err := exists(stealthPath)
	if err != nil {
		return "", err
	}
	if cOK && sOK {
		return "", fmt.Errorf("both %s and %s exist; keep exactly one", committed, stealthPath)
	}
	if cOK {
		return committed, nil
	}
	return stealthPath, nil
}

// Load layers defaults, then the global file, then the project file. Missing
// files are skipped. Unknown keys and invalid values are errors.
func Load(globalPath, projectPath string) (Config, error) {
	cfg := Default()
	for _, p := range []string{globalPath, projectPath} {
		if err := decodeFile(p, &cfg); err != nil {
			return Config{}, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", projectPath, err)
	}
	return cfg, nil
}

func decodeFile(path string, cfg *Config) error {
	ok, err := exists(path)
	if err != nil || !ok {
		return err
	}
	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	return nil
}

func (c Config) Validate() error {
	var errs []error
	check := func(ok bool, format string, a ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, a...))
		}
	}
	check(oneOf(c.Tests.Format, "junit", "go-json", "exit-code", "none"), "[tests].format %q must be junit, go-json, exit-code or none", c.Tests.Format)
	check(c.Tests.Format != "junit" || c.Tests.JunitPath != "", "[tests].junit_path must be set when format is junit")
	check(c.Tests.JunitPath == "" || junitPathInside(c.Tests.JunitPath),
		"[tests].junit_path %q must be {junit} or a path relative to the task worktree with no .. (shoal deletes stale matches before each run, so it must never reach outside the worktree)", c.Tests.JunitPath)
	check(c.Tests.JunitPath == "" || strings.HasSuffix(c.Tests.JunitPath, ".xml") || c.Tests.JunitPath == "{junit}",
		"[tests].junit_path %q must end in .xml (or be {junit}): shoal deletes stale matches before each run and only ever deletes .xml reports", c.Tests.JunitPath)
	check(!junitPathInGit(c.Tests.JunitPath), "[tests].junit_path %q reaches into .git; point it at a report directory in the worktree", c.Tests.JunitPath)
	check(c.Tests.FlakyRetries >= 0, "[tests].flaky_retries %d must be >= 0", c.Tests.FlakyRetries)
	check(positiveDuration(c.Tests.Timeout), "[tests].timeout %q must be a positive duration like 30m", c.Tests.Timeout)
	check(oneOf(c.Tests.CoverageFormat, "", "lcov", "cobertura", "go-cover"), "[tests].coverage_format %q must be empty, lcov, cobertura or go-cover", c.Tests.CoverageFormat)
	check(c.Tests.CoverageFormat == "" || c.Tests.CoveragePath != "", "[tests].coverage_path must be set when coverage_format is set")
	check(c.Tests.CoveragePath == "" || (junitPathInside(c.Tests.CoveragePath) && !strings.ContainsAny(c.Tests.CoveragePath, "*?[{")),
		"[tests].coverage_path %q must be a file path relative to the task worktree with no .. or wildcards (shoal deletes the old report before each run)", c.Tests.CoveragePath)
	check(!junitPathInGit(c.Tests.CoveragePath), "[tests].coverage_path %q reaches into .git", c.Tests.CoveragePath)
	check(c.Tests.RerunCommand == "" || strings.Contains(c.Tests.RerunCommand, "{tests}"), "[tests].rerun_command must contain {tests}, the ids of the tests to re-run")
	check(strings.TrimSpace(c.Session.Model) != "" && !strings.HasPrefix(c.Session.Model, "-"), "[session].model %q must be a model name", c.Session.Model)
	check(c.Install.Stealth, "[install].stealth = false is not supported until Phase 4")
	check(c.Lanes.QAMaxRounds >= 1, "[lanes].qa_max_rounds %d must be >= 1", c.Lanes.QAMaxRounds)
	check(validRefName(c.Merge.Base), "[merge].base %q must be a branch name", c.Merge.Base)
	check(oneOf(c.Merge.Strategy, "rebase", "squash"), "[merge].strategy %q must be rebase or squash", c.Merge.Strategy)
	check(c.Hotfix.ProductionRef == "" || validRefName(strings.TrimPrefix(c.Hotfix.ProductionRef, "tag:")), "[hotfix].production_ref %q is not a valid ref name", c.Hotfix.ProductionRef)
	check(c.Hotfix.MaxDiffLines >= 0, "[hotfix].max_diff_lines %d must be >= 0", c.Hotfix.MaxDiffLines)
	check(positiveDuration(c.Claims.StaleAfter), "[claims].stale_after %q must be a positive duration like 2h", c.Claims.StaleAfter)
	check(oneOf(c.Reports.Sink, "local", "notes", "both"), "[reports].sink %q must be local, notes or both", c.Reports.Sink)
	check(c.Reports.Sink == "local" || filepath.IsAbs(c.Reports.NotesPath), "[reports].notes_path must be an absolute path when sink is %q", c.Reports.Sink)
	return errors.Join(errs...)
}

// TestTimeout is the parsed [tests].timeout. Validate has already checked it.
func (c Config) TestTimeout() time.Duration {
	d, _ := time.ParseDuration(c.Tests.Timeout)
	return d
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func positiveDuration(s string) bool {
	d, err := time.ParseDuration(s)
	return err == nil && d > 0
}

func validRefName(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") && !strings.ContainsAny(s, " \t\n~^:?[\\") && !strings.Contains(s, "..")
}

// junitPathInside reports whether a junit_path stays inside the worktree as
// written: exactly {junit}, or relative with no "..", "~", root, or drive.
// Symlinks are checked against the real worktree at run time.
func junitPathInside(p string) bool {
	if p == "{junit}" {
		return true
	}
	if strings.Contains(p, "{junit}") || filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) ||
		strings.HasPrefix(p, "~") || filepath.VolumeName(p) != "" || (len(p) >= 2 && p[1] == ':') {
		return false
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return false
		}
	}
	return true
}

func junitPathInGit(p string) bool {
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".git" {
			return true
		}
	}
	return false
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

// Template is the commented config init writes for a new project.
func Template(goModule bool) string {
	command, format := `""`, `"junit"`
	if goModule {
		command, format = `"go test -json ./..."`, `"go-json"`
	}
	return `# shoal project config (spec §4.3). Unknown keys are an error.

[tests]
# Full test suite, run with sh -c in the task worktree.
# {junit} is the absolute path shoal reads JUnit XML from.
command = ` + command + `
format = ` + format + `   # junit | go-json | exit-code | none
# junit_path = "{junit}"
# timeout = "30m"

[merge]
# base = "main"   # local branch tasks start from

[hotfix]
# production_ref = "production"   # branch, tag, or "tag:<glob>"; required for hotfix tasks

[session]
# model = "` + DefaultModel + `"

[stack]
# lint = ""    # run by done when set, informational
# build = ""
`
}
