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
	"strconv"
	"strings"
	"testing"
)

// isolatedEmail is the git identity Isolate sets; Repo checks for it.
const isolatedEmail = "test@example.invalid"

// Isolate points HOME, USERPROFILE, the XDG dirs and git's global config at a
// fresh temp dir, clears SHOAL_HOME, TMUX and CLAUDE_* variables, and returns
// the fake home.
func Isolate(t *testing.T) string {
	t.Helper()
	home := Resolve(t, t.TempDir())
	for k, v := range IsolatedEnv(home) {
		t.Setenv(k, v)
	}
	return home
}

// IsolatedEnv is the environment Isolate sets for the fake home: give it to
// code under test (Env.Getenv) and subprocesses (exec.Cmd.Env) instead of
// t.Setenv, and the test can run in parallel.
func IsolatedEnv(home string) map[string]string {
	env := map[string]string{
		"HOME":                home,
		"USERPROFILE":         home,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   filepath.Join(home, ".gitconfig"),
		"GIT_AUTHOR_NAME":     "shoal test",
		"GIT_AUTHOR_EMAIL":    isolatedEmail,
		"GIT_COMMITTER_NAME":  "shoal test",
		"GIT_COMMITTER_EMAIL": isolatedEmail,
	}
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		env[k] = filepath.Join(home, "xdg", strings.ToLower(k))
	}
	for _, k := range []string{"SHOAL_HOME", "TMUX", "TMUX_PANE", "CLAUDE_PROJECT_DIR", "CLAUDE_CONFIG_DIR", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"} {
		env[k] = ""
	}
	return env
}

// IsolateProcess applies IsolatedEnv to this whole process for a fresh fake
// home, and sets SHELL to an idle shell for the tmux servers TmuxServer
// starts. It is for a TestMain whose parallel tests each pass their own
// IsolatedEnv, so that what those tests do not override (git and tmux
// subprocesses, testutil.Git, the identity Repo checks) never sees the real
// home. It returns the fake home's parent dir, for the caller to remove.
func IsolateProcess() (string, error) {
	dir, err := os.MkdirTemp("", "shoal-test-home-")
	if err != nil {
		return "", err
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return "", err
	}
	home := filepath.Join(dir, "home")
	shell := filepath.Join(dir, "idle-shell")
	if err := os.Mkdir(home, 0o755); err != nil {
		return dir, err
	}
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		return dir, err
	}
	env := IsolatedEnv(home)
	env["SHELL"] = shell
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			return dir, err
		}
	}
	return dir, nil
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
// as origin, and returns the repo path. It fails the test unless Isolate (or
// IsolateProcess) ran first, so a test never commits with the host's git identity (CI runners have
// none, which would otherwise fail only there).
func Repo(t *testing.T) string {
	t.Helper()
	if got := os.Getenv("GIT_AUTHOR_EMAIL"); got != isolatedEmail {
		t.Fatalf("testutil.Repo: call testutil.Isolate(t) first (GIT_AUTHOR_EMAIL = %q, want %q)", got, isolatedEmail)
	}
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
	args := TmuxServer(t)
	shell := filepath.Join(t.TempDir(), "idle-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatalf("write idle shell: %v", err)
	}
	// The server's default shell would otherwise be the user's real shell,
	// which writes history and rc files into the fake HOME.
	t.Setenv("SHELL", shell)
	return args
}

// TmuxServer is Tmux without setting SHELL, for parallel tests in a process
// that IsolateProcess set up.
func TmuxServer(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random socket name: %v", err)
	}
	name := "shoal-test-" + hex.EncodeToString(b)
	args := []string{"-L", name}
	tmpDir := os.Getenv("TMUX_TMPDIR")
	if tmpDir == "" {
		tmpDir = "/tmp"
	}
	socket := filepath.Join(tmpDir, "tmux-"+strconv.Itoa(os.Getuid()), name)
	t.Cleanup(func() {
		_ = exec.Command("tmux", append(args, "kill-server")...).Run()
		if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove tmux socket %s: %v", socket, err)
		}
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
