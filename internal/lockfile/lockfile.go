// Package lockfile provides OS advisory file locks (flock / LockFileEx), which
// the OS releases when the holding process dies (spec §10.3).
package lockfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var ErrHeld = errors.New("lock is held by another process")

type Info struct {
	PID       int    `json:"pid"`
	Command   string `json:"command"`
	Task      string `json:"task"`
	StartedAt string `json:"started_at"`
}

type Lock struct{ f *os.File }

// Acquire takes an exclusive lock on path, polling for up to wait, and writes
// info into the file. On timeout it returns ErrHeld with the holder's info.
func Acquire(path string, wait time.Duration, info Info) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			holder, _ := os.ReadFile(path)
			f.Close()
			return nil, fmt.Errorf("%w: %s after %s (holder: %s)", ErrHeld, path, wait, holder)
		}
		time.Sleep(100 * time.Millisecond)
	}
	body, err := json.Marshal(info)
	if err == nil {
		err = f.Truncate(0)
	}
	if err == nil {
		_, err = f.WriteAt(body, 0)
	}
	if err != nil {
		_ = unlock(f)
		f.Close()
		return nil, fmt.Errorf("write lock info to %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

func (l *Lock) Release() error {
	uerr := unlock(l.f)
	cerr := l.f.Close()
	if uerr != nil {
		return fmt.Errorf("unlock %s: %w", l.f.Name(), uerr)
	}
	return cerr
}
