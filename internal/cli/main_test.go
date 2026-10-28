package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
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
		os.Exit(Main(os.Args[1:], env, "test"))
	}
	os.Exit(m.Run())
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
