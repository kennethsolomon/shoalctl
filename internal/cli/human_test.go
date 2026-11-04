package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The real check, not the Env.Terminal swap: a subprocess whose stdin is the
// null device (a character device on Unix) is no human.
func TestRequireHumanRefusesNullStdin(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("convention", "propose", "--rule", "Keep handlers thin", "--scope", "**")
	for _, args := range [][]string{
		{"convention", "agree", "1"},
		{"convention", "retire", "1", "--why", "agent says so"},
		{"merge", "--task", "x", "--accept-regression", "C::a", "--reason", "agent says so"},
	} {
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		cmd := h.command(args...)
		var stderr bytes.Buffer
		cmd.Stdin, cmd.Stderr = devnull, &stderr
		err = cmd.Run()
		devnull.Close()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != ExitState {
			t.Fatalf("%v with stdin=%s: %v, want exit %d\nstderr: %s", args, os.DevNull, err, ExitState, stderr.String())
		}
		mustContain(t, strings.Join(args, " "), stderr.String(), "needs a human at a terminal")
	}
	if c := h.mustRun("convention", "list"); !strings.Contains(c, "Proposed") {
		t.Fatalf("a refused agree changed the convention:\n%s", c)
	}
}

func TestInteractiveRealCheck(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for name, env := range map[string]Env{
		"null stdin":     {Stdin: devnull, Stdout: w},
		"pipe stdin":     {Stdin: r, Stdout: w},
		"reader stdin":   {Stdin: strings.NewReader("y\n"), Stdout: w},
		"null both ways": {Stdin: devnull, Stdout: devnull},
	} {
		if env.interactive() {
			t.Errorf("%s counts as a human at a terminal", name)
		}
	}
}

// A Claude Code session is refused even when stdin looks like a terminal.
func TestRequireHumanRefusesClaudeCodeSession(t *testing.T) {
	for _, k := range claudeCodeEnv {
		t.Run(k, func(t *testing.T) {
			h := newHarness(t)
			h.mustRun("init")
			h.mustRun("convention", "propose", "--rule", "Keep handlers thin", "--scope", "**")
			h.asHuman()
			t.Setenv(k, "1")
			mustContain(t, "agree in Claude Code", h.expectCode(ExitState, "convention", "agree", "1"),
				"inside a Claude Code session ("+k+" is set)", "your own terminal")
			mustContain(t, "promote in Claude Code", h.expectCode(ExitState, "promote", "lesson:1"), "inside a Claude Code session")
			t.Setenv(k, "")
			h.mustRun("convention", "agree", "1")
		})
	}
}
