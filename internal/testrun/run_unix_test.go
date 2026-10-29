//go:build unix

package testrun

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRunCancelDuringGateIsCancelled: an interrupt that lands while the suite
// is held at its gate kills the held shell, so releasing it fails. The run was
// interrupted, not misconfigured, and must say so.
func TestRunCancelDuringGateIsCancelled(t *testing.T) {
	tests := []struct {
		name    string
		sigterm bool
	}{
		{"context cancelled", false},
		{"context cancelled and group sent SIGTERM", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, dir := runSpec(t, "exit-code", "echo ran > ran")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			spec.OnStart = func(pid int) error {
				cancel()
				if tt.sigterm {
					if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
						t.Errorf("SIGTERM group %d: %v", pid, err)
					}
				}
				time.Sleep(300 * time.Millisecond)
				return nil
			}
			out := Run(ctx, spec)
			if out.Status != StatusCancelled {
				t.Fatalf("status = %s (%s), want cancelled", out.Status, out.Problem)
			}
			if _, err := os.Stat(filepath.Join(dir, "ran")); !os.IsNotExist(err) {
				t.Fatalf("the suite ran although it was interrupted at its gate: %v", err)
			}
		})
	}
}
