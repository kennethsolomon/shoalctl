package cli

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// toolVersion runs bin with args and returns the resolved path and output.
// A missing binary or a failing command is a dependency error (exit 7).
func toolVersion(ctx context.Context, tool, bin string, args ...string) (string, string, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", "", fail(ExitDependency, "%s not found (looked for %q on PATH): %v", tool, bin, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return "", "", fail(ExitDependency, "%s %s failed: %v: %s", path, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return path, strings.TrimSpace(string(out)), nil
}

// atLeast reports whether the first X.Y in out is >= major.minor.
func atLeast(out string, major, minor int) bool {
	m := versionRe.FindStringSubmatch(out)
	if m == nil {
		return false
	}
	maj, _ := strconv.Atoi(m[1])
	mnr, _ := strconv.Atoi(m[2])
	return maj > major || (maj == major && mnr >= minor)
}

func checkGit(ctx context.Context) error {
	_, out, err := toolVersion(ctx, "git", "git", "--version")
	if err != nil {
		return err
	}
	if !atLeast(out, 2, 31) {
		return fail(ExitDependency, "git 2.31 or newer is required, found %q", out)
	}
	return nil
}

func (env Env) checkTmux(ctx context.Context) error {
	_, out, err := toolVersion(ctx, "tmux", env.Tmux, "-V")
	if err != nil {
		return err
	}
	if !atLeast(out, 3, 2) && !strings.Contains(out, "master") {
		return fail(ExitDependency, "tmux 3.2 or newer is required, found %q", out)
	}
	return nil
}

// checkClaude returns the absolute path of the claude binary.
func (env Env) checkClaude(ctx context.Context) (string, error) {
	path, _, err := toolVersion(ctx, "claude", env.Claude, "--version")
	return path, err
}
