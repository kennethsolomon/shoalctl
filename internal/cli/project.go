package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/config"
	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

type project struct {
	id      string
	remote  string // canonical remote, or the main checkout path when there is none
	root    string // main checkout
	home    string // $SHOAL_HOME
	dir     string // $SHOAL_HOME/projects/<id>
	cfgPath string
	cfg     config.Config
	db      *store.Store
}

type projectMeta struct {
	ID        string `json:"id"`
	Remote    string `json:"remote"`
	Root      string `json:"root"`
	CreatedAt string `json:"created_at"`
}

func (p *project) dbPath() string   { return filepath.Join(p.dir, "memory.db") }
func (p *project) metaPath() string { return filepath.Join(p.dir, "project.json") }
func (p *project) worktree(slug string) string {
	return filepath.Join(p.dir, "worktrees", slug)
}
func (p *project) runDir(id int64) string { return filepath.Join(p.dir, "runs", fmt.Sprint(id)) }
func (p *project) close() {
	if p.db != nil {
		p.db.Close()
	}
}

// shoalHome is $SHOAL_HOME, default ~/.shoal. It must be absolute.
func shoalHome() (string, error) {
	if h := os.Getenv("SHOAL_HOME"); h != "" {
		if !filepath.IsAbs(h) {
			return "", fail(ExitConfig, "SHOAL_HOME %q must be an absolute path", h)
		}
		return filepath.Clean(h), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fail(ExitConfig, "find home directory for ~/.shoal: %v", err)
	}
	return filepath.Join(home, ".shoal"), nil
}

// identify resolves the main checkout and the project id (§15.1 "Project id").
func (env Env) identify(ctx context.Context) (*project, error) {
	if err := checkGit(ctx); err != nil {
		return nil, err
	}
	root, err := gitx.MainCheckout(ctx, env.Dir)
	if err != nil {
		return nil, withCode(ExitConfig, err)
	}
	remote, err := gitx.RemoteURL(ctx, root)
	if err != nil {
		return nil, err
	}
	canon := root
	if remote != "" {
		canon = gitx.Canonical(remote)
	}
	home, err := shoalHome()
	if err != nil {
		return nil, err
	}
	id := gitx.ProjectID(canon)
	return &project{id: id, remote: canon, root: root, home: home, dir: filepath.Join(home, "projects", id)}, nil
}

func readMeta(path string) (projectMeta, bool, error) {
	var m projectMeta
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, false, fail(ExitConfig, "%s is not valid JSON: %v", path, err)
	}
	return m, true, nil
}

func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// openProject loads an initialized project: metadata, config, and the DB.
func (env Env) openProject(ctx context.Context) (*project, error) {
	p, err := env.identify(ctx)
	if err != nil {
		return nil, err
	}
	meta, ok, err := readMeta(p.metaPath())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fail(ExitConfig, "project %s (%s) is not initialized; run shoalctl init in %s", p.id, p.remote, p.root)
	}
	if !sameDir(meta.Root, p.root) {
		return nil, fail(ExitConfig, "project %s is registered at %s, not %s; run shoalctl init --rebind to move it", p.id, meta.Root, p.root)
	}
	if err := env.loadConfig(p); err != nil {
		return nil, err
	}
	if _, err := os.Stat(p.dbPath()); err != nil {
		return nil, fail(ExitConfig, "%s is missing (%v); run shoalctl init", p.dbPath(), err)
	}
	p.db, err = store.Open(ctx, p.dbPath(), store.Options{Migrate: true, BusyTimeout: 5 * time.Second, Retries: 2})
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (env Env) loadConfig(p *project) error {
	path, err := config.ProjectPath(p.root, filepath.Join(p.dir, "config.toml"))
	if err != nil {
		return withCode(ExitConfig, err)
	}
	cfg, err := config.Load(filepath.Join(p.home, "config.toml"), path)
	if err != nil {
		return withCode(ExitConfig, err)
	}
	p.cfgPath, p.cfg = path, cfg
	return nil
}

// writeFileAtomic writes through a temp file in the same dir and renames it.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(perm); err != nil {
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
