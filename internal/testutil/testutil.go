// Package testutil isolates tests from the real home directory and builds
// throwaway git repos, fake binaries, and private tmux servers.
package testutil

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Isolate points HOME, USERPROFILE, the XDG dirs and git's global config at a
// fresh temp dir, clears SHOAL_HOME, TMUX and CLAUDE_* variables, and returns
// the fake home.
func Isolate(t *testing.T) string {
	t.Helper()
	home := Resolve(t, t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(k, filepath.Join(home, "xdg", strings.ToLower(k)))
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_AUTHOR_NAME", "shoal test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "shoal test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	for _, k := range []string{"SHOAL_HOME", "TMUX", "TMUX_PANE", "CLAUDE_PROJECT_DIR", "CLAUDE_CONFIG_DIR"} {
		t.Setenv(k, "")
	}
	return home
}

// Resolve returns p with symlinks resolved (macOS temp dirs live under /private).
func Resolve(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("resolve %s: %v", p, err)
	}
	return r
}

// Git runs git in dir and fails the test on error.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Repo creates a git repo on branch main with one commit and a local bare repo
// as origin, and returns the repo path. Call Isolate first.
func Repo(t *testing.T) string {
	t.Helper()
	base := Resolve(t, t.TempDir())
	remote := filepath.Join(base, "remote.git")
	repo := filepath.Join(base, "app")
	Git(t, base, "init", "-q", "--bare", "-b", "main", remote)
	Git(t, base, "init", "-q", "-b", "main", repo)
	WriteFile(t, filepath.Join(repo, "README.md"), "app\n")
	Git(t, repo, "add", "README.md")
	Git(t, repo, "commit", "-q", "-m", "initial")
	Git(t, repo, "remote", "add", "origin", remote)
	return repo
}

// WriteFile writes content to path, creating parent dirs.
func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// RequireSh skips the test when no POSIX sh is available.
func RequireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

// FakeClaude writes an executable fake claude that answers --version and
// otherwise appends its arguments, one per line, to argsLog, then sleeps so a
// tmux window running it stays open. It returns the script path.
func FakeClaude(t *testing.T, argsLog string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a POSIX shell script")
	}
	RequireSh(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.310 (Claude Code)"; exit 0; fi
for a in "$@"; do printf '%s\n' "$a" >> '` + argsLog + `'; done
printf -- '--\n' >> '` + argsLog + `'
exec sleep 60
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

// Tmux returns args selecting a private tmux server socket, killed on cleanup.
// It skips the test when tmux is not installed.
func Tmux(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random socket name: %v", err)
	}
	args := []string{"-L", "shoal-test-" + hex.EncodeToString(b)}
	t.Cleanup(func() {
		_ = exec.Command("tmux", append(args, "kill-server")...).Run()
	})
	return args
}

// TrustProject writes a ~/.claude.json under home that marks dir as trusted.
func TrustProject(t *testing.T, home, dir string) {
	t.Helper()
	WriteFile(t, filepath.Join(home, ".claude.json"),
		`{"projects": {`+jsonString(dir)+`: {"hasTrustDialogAccepted": true}}}`)
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
