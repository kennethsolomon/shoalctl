package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// fakeBinary writes an executable stand-in for the shoalctl binary.
func fakeBinary(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "shoalctl")
	testutil.WriteFile(t, path, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// jsonText is s as it appears inside a JSON string, so Windows paths match
// their escaped backslashes.
func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func TestDoctorHealthyAfterInit(t *testing.T) {
	h := newHarness(t)
	h.env.Exe = fakeBinary(t, t.TempDir())
	h.mustRun("init")
	out := h.mustRun("doctor")
	if !strings.Contains(out, "ok") || !strings.Contains(out, h.env.Exe) {
		t.Fatalf("doctor output:\n%s", out)
	}
}

func TestDoctorFindsAndFixesMovedBinary(t *testing.T) {
	h := newHarness(t)
	oldDir := t.TempDir()
	h.env.Exe = fakeBinary(t, oldDir)
	settings := filepath.Join(h.repo, ".claude", "settings.local.json")
	testutil.WriteFile(t, settings, `{"zzz": "keep & <me>", "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "echo mine"}]}]}}`)
	h.mustRun("init")
	old := h.env.Exe
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, settings)

	h.env.Exe = fakeBinary(t, t.TempDir())
	errOut := h.expectCode(ExitDoctor, "doctor")
	if !strings.Contains(errOut, old) || !strings.Contains(errOut, "--fix") {
		t.Fatalf("doctor should name the missing binary and the fix:\n%s", errOut)
	}
	if readFile(t, settings) != before {
		t.Fatal("doctor without --fix changed the settings")
	}

	h.mustRun("doctor", "--fix")
	after := readFile(t, settings)
	if strings.Contains(after, jsonText(old)) || !strings.Contains(after, jsonText(h.env.Exe)) {
		t.Fatalf("doctor --fix did not repoint the hooks:\n%s", after)
	}
	for _, keep := range []string{`"zzz": "keep & <me>"`, `"command": "echo mine"`} {
		if !strings.Contains(after, keep) {
			t.Errorf("doctor --fix lost %s:\n%s", keep, after)
		}
	}
	h.mustRun("doctor")
}

func TestDoctorReportsOutdatedAndMissingEntries(t *testing.T) {
	h := newHarness(t)
	h.env.Exe = fakeBinary(t, t.TempDir())
	settings := filepath.Join(h.repo, ".claude", "settings.local.json")

	h.mustRun("init")
	testutil.WriteFile(t, settings, `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "`+jsonText(h.env.Exe)+` hook stop", "timeout": 10}]}]}}`)
	errOut := h.expectCode(ExitDoctor, "doctor")
	for _, want := range []string{"SessionStart", "not registered", "Stop", "outdated"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, errOut)
		}
	}
	h.mustRun("doctor", "--fix")
	h.mustRun("doctor")

	os.Remove(settings)
	h.expectCode(ExitDoctor, "doctor")
	h.mustRun("doctor", "--fix")
	h.mustRun("doctor")
	if status := testutil.Git(t, h.repo, "status", "--porcelain"); status != "" {
		t.Errorf("doctor --fix left the repo dirty:\n%s", status)
	}
}

func TestInitRequiresAbsoluteBinaryPath(t *testing.T) {
	h := newHarness(t)
	h.env.Exe = "shoalctl"
	if errOut := h.expectCode(ExitInternal, "init"); !strings.Contains(errOut, "absolute") {
		t.Fatalf("stderr: %s", errOut)
	}
}
