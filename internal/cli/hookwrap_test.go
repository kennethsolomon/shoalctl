package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// goFatal is a program that dies the way a Go runtime fatal error does:
// "fatal error: stack overflow", exit status 2, no recover possible.
const goFatal = `package main

import "runtime/debug"

func recurse(n int) int { return recurse(n+1) + 1 }

func main() {
	debug.SetMaxStack(1 << 16)
	recurse(0)
}
`

// crashingBinaries returns fake hook binaries that exit non-zero in every way
// a process can, keyed by name.
func crashingBinaries(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin dir")
	// Windows runs only .exe files by path, so the shell scripts are POSIX
	// only; the Go fatal program still covers exit 2 there.
	exe := "shoalctl"
	bins := map[string]string{
		"exits 2":        "#!/bin/sh\nexit 2\n",
		"exits 1":        "#!/bin/sh\nexit 1\n",
		"killed by SEGV": "#!/bin/sh\nkill -SEGV $$\n",
	}
	if runtime.GOOS == "windows" {
		exe, bins = "shoalctl.exe", nil
	}
	out := map[string]string{"missing binary": filepath.Join(dir, "gone", exe)}
	for name, script := range bins {
		path := filepath.Join(dir, name, "shoalctl")
		testutil.WriteFile(t, path, script)
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
		out[name] = path
	}

	src := filepath.Join(t.TempDir(), "main.go")
	testutil.WriteFile(t, src, goFatal)
	fatal := filepath.Join(dir, "go runtime fatal", exe)
	build := exec.Command("go", "build", "-o", fatal, src)
	build.Env = append(os.Environ(), "GO111MODULE=off")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fatal program: %v\n%s", err, b)
	}
	if err := exec.Command(fatal).Run(); exitCode(err) != 2 {
		t.Fatalf("the fatal program should exit 2 like a Go runtime fatal error, got %v", err)
	}
	out["go runtime fatal"] = fatal
	return out
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// TestHookCommandNeverExitsNonZero runs the registered command the way
// Claude Code does on macOS and Linux (bash -c), and with sh for good
// measure. Exit 2 would block the session, so every failure must become 0.
func TestHookCommandNeverExitsNonZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shells; the PowerShell form is tested below when pwsh exists")
	}
	for name, bin := range crashingBinaries(t) {
		cmd := hookCommandFor(runtime.GOOS, bin, "stop")
		if !isShoalCommand(cmd) {
			t.Errorf("%s: %s is not recognised as a shoal command", name, cmd)
		}
		if prog, _ := commandProgram(cmd); prog != bin {
			t.Errorf("%s: program %q, want %q", name, prog, bin)
		}
		for _, shell := range []string{"bash", "sh"} {
			if _, err := exec.LookPath(shell); err != nil {
				continue
			}
			if err := exec.Command(shell, "-c", cmd).Run(); exitCode(err) != 0 {
				t.Errorf("%s via %s: %s exited %d, want 0", name, shell, cmd, exitCode(err))
			}
		}
	}
}

func TestHookCommandNeverExitsNonZeroPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		if pwsh, err = exec.LookPath("powershell"); err != nil {
			t.Skip("PowerShell not installed")
		}
	}
	if runtime.GOOS != "windows" {
		testutil.RequireSh(t)
	}
	for name, bin := range crashingBinaries(t) {
		cmd := hookCommandFor("windows", bin, "stop")
		if err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", cmd).Run(); exitCode(err) != 0 {
			t.Errorf("%s via PowerShell: %s exited %d, want 0", name, cmd, exitCode(err))
		}
	}
}
