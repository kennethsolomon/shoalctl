package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestVersionLdflags builds the binary the way the release does and checks
// that version prints the injected values.
func TestVersionLdflags(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	exe := filepath.Join(t.TempDir(), "shoalctl")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	ldflags := "-X main.version=1.2.3 -X main.commit=0123456789abcdef -X main.date=2026-11-10T00:00:00Z"
	if out, err := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(exe, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("shoalctl version: %v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "shoalctl 1.2.3 (commit 0123456789ab, built 2026-11-10T00:00:00Z)"; got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
}
