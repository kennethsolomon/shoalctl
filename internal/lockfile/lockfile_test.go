package lockfile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireContendRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "git.lock")
	first, err := Acquire(path, 0, Info{PID: 42, Command: "start", Task: "1-a"})
	if err != nil {
		t.Fatal(err)
	}

	var got Info
	body, _ := os.ReadFile(path)
	if err := json.Unmarshal(body, &got); err != nil || got.PID != 42 || got.Task != "1-a" {
		t.Fatalf("lock info = %s (%v)", body, err)
	}

	start := time.Now()
	if _, err := Acquire(path, 300*time.Millisecond, Info{PID: 43}); !errors.Is(err, ErrHeld) {
		t.Fatalf("second acquire: got %v, want ErrHeld", err)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Fatalf("gave up after %s, want to wait 300ms", waited)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path, 0, Info{PID: 44})
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}
