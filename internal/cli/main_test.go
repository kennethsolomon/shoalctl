package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/store"
)

// asBinaryEnv makes the test binary behave as shoalctl, so tests can run real
// concurrent processes and send them signals.
const asBinaryEnv = "SHOALCTL_TEST_AS_BINARY"

func TestMain(m *testing.M) {
	if os.Getenv(asBinaryEnv) == "1" {
		env, err := DefaultEnv()
		if err != nil {
			os.Stderr.WriteString("shoalctl (test binary): " + err.Error() + "\n")
			os.Exit(ExitInternal)
		}
		env.Exe = os.Getenv("SHOALCTL_TEST_EXE")
		env.Claude = os.Getenv("SHOALCTL_TEST_CLAUDE")
		env.TmuxArgs = strings.Fields(os.Getenv("SHOALCTL_TEST_TMUX_ARGS"))
		// Subprocess starts in tests launch no watcher; the attention test
		// runs start in-process with the real spawner.
		env.SpawnWatcher = func(*project, store.Task) error { return nil }
		os.Exit(Main(os.Args[1:], env, "test"))
	}
	dir, err := directGit()
	if err != nil {
		os.Stderr.WriteString("cli tests: " + err.Error() + "\n")
		os.Exit(2)
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

// directGit puts the real git ahead of macOS's /usr/bin/git, an xcrun
// trampoline that adds about 7 ms to every call: the tests run thousands of
// git commands, in-process and from shoalctl. Only a symlink named git goes
// on PATH, so nothing else in the developer directory shadows the host's
// tools. Elsewhere it does nothing and returns "".
func directGit() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", nil
	}
	if p, err := exec.LookPath("git"); err != nil || p != "/usr/bin/git" {
		return "", nil
	}
	out, err := exec.Command("xcrun", "-f", "git").Output()
	if err != nil {
		return "", &exec.Error{Name: "xcrun -f git", Err: err}
	}
	real := strings.TrimSpace(string(out))
	dir, err := os.MkdirTemp("", "shoal-cli-git-")
	if err != nil {
		return "", err
	}
	if err := os.Symlink(real, filepath.Join(dir, "git")); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// command builds a shoalctl subprocess with the harness's env, running in the
// repo. The caller starts and waits for it.
func (h *harness) command(args ...string) *exec.Cmd {
	h.t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = h.env.Dir
	cmd.Env = append(os.Environ(), asBinaryEnv+"=1", "SHOALCTL_TEST_EXE="+h.env.Exe, "SHOALCTL_TEST_CLAUDE="+h.env.Claude,
		"SHOALCTL_TEST_TMUX_ARGS="+strings.Join(h.env.TmuxArgs, " "))
	return cmd
}
