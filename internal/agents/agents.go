// Package agents ships the shoal-* Claude Code agent definitions (spec §7)
// and installs them without overwriting files a person edited.
package agents

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed defs/*.md
var defsFS embed.FS

// Version is the shoal_version every shipped definition carries.
const Version = 1

// Def is one shipped agent definition.
type Def struct {
	Name string // shoal-<role>
	Body []byte
}

// Defs returns the shipped definitions in name order.
func Defs() ([]Def, error) {
	entries, err := fs.ReadDir(defsFS, "defs")
	if err != nil {
		return nil, fmt.Errorf("read embedded agent definitions: %w", err)
	}
	var out []Def
	for _, e := range entries {
		body, err := defsFS.ReadFile("defs/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read agent definition %s: %w", e.Name(), err)
		}
		out = append(out, Def{Name: strings.TrimSuffix(e.Name(), ".md"), Body: body})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Manifest records the SHA-256 of every file shoal wrote, by absolute path.
type Manifest struct {
	Files map[string]string `json:"files"`
}

func readManifest(path string) (Manifest, error) {
	m := Manifest{Files: map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return m, nil
}

func hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Status of an installed definition.
const (
	StatusInstalled = "installed" // matches the shipped file
	StatusOutdated  = "outdated"  // written by shoal, older than the shipped file
	StatusModified  = "modified"  // changed by someone since shoal wrote it
	StatusMissing   = "missing"
)

// Entry is one definition's state in a directory.
type Entry struct {
	Name, Path, Status string
	Action             string // install: written, updated, unchanged, left alone
}

// List reports the state of every shipped definition in dir.
func List(dir, manifestPath string) ([]Entry, error) {
	return walk(dir, manifestPath, false)
}

// Install writes every shipped definition into dir. A missing file is
// written; a file shoal wrote earlier and nobody changed is replaced; a file
// someone edited (or that shoal did not write) is left alone and reported.
func Install(dir, manifestPath string) ([]Entry, error) {
	return walk(dir, manifestPath, true)
}

func walk(dir, manifestPath string, write bool) ([]Entry, error) {
	defs, err := Defs()
	if err != nil {
		return nil, err
	}
	m, err := readManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	if write {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	var out []Entry
	changed := false
	for _, d := range defs {
		path := filepath.Join(dir, d.Name+".md")
		e := Entry{Name: d.Name, Path: path}
		want := hash(d.Body)
		cur, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			e.Status = StatusMissing
		case err != nil:
			return nil, fmt.Errorf("read %s: %w", path, err)
		case hash(cur) == want:
			e.Status = StatusInstalled
		case m.Files[path] == hash(cur):
			e.Status = StatusOutdated
		default:
			e.Status = StatusModified
		}
		if write {
			switch e.Status {
			case StatusMissing, StatusOutdated:
				if err := writeFile(path, d.Body); err != nil {
					return nil, err
				}
				e.Action = map[string]string{StatusMissing: "written", StatusOutdated: "updated"}[e.Status]
				e.Status = StatusInstalled
				m.Files[path] = want
				changed = true
			case StatusInstalled:
				e.Action = "unchanged"
				if m.Files[path] != want {
					m.Files[path] = want
					changed = true
				}
			case StatusModified:
				e.Action = "left alone (edited since shoal wrote it, or not written by shoal)"
			}
		}
		out = append(out, e)
	}
	if write && changed {
		body, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", filepath.Dir(manifestPath), err)
		}
		if err := writeFile(manifestPath, append(body, '\n')); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
