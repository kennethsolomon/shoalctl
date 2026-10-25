package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/config"
	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

func (env Env) cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	commitConfig := fs.Bool("commit-config", false, "write .shoal.toml in the repo instead of the stealth config")
	rebind := fs.Bool("rebind", false, "update the registered root when the checkout moved")
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}

	p, err := env.identify(ctx)
	if err != nil {
		return err
	}
	meta, exists, err := readMeta(p.metaPath())
	if err != nil {
		return err
	}
	if exists && !sameDir(meta.Root, p.root) && !*rebind {
		return fail(ExitConfig, "project %s is registered at %s; run shoalctl init --rebind to move it to %s", p.id, meta.Root, p.root)
	}

	for _, d := range []string{p.home, filepath.Join(p.home, "logs"), p.dir, filepath.Join(p.dir, "worktrees"),
		filepath.Join(p.dir, "runs"), filepath.Join(p.dir, "reports"), filepath.Join(p.dir, "locks")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	if !exists || meta.Root != p.root {
		if !exists {
			meta = projectMeta{ID: p.id, Remote: p.remote, CreatedAt: nowUTC()}
		}
		meta.Root = p.root
		body, err := json.MarshalIndent(meta, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFileAtomic(p.metaPath(), append(body, '\n'), 0o600); err != nil {
			return err
		}
	}

	db, err := store.Open(ctx, p.dbPath(), store.Options{Create: true, Migrate: true, BusyTimeout: 5 * time.Second, Retries: 2})
	if err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close %s: %w", p.dbPath(), err)
	}

	cfgPath, err := config.ProjectPath(p.root, filepath.Join(p.dir, "config.toml"))
	if err != nil {
		return withCode(ExitConfig, err)
	}
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		perm := os.FileMode(0o600)
		if *commitConfig {
			cfgPath, perm = filepath.Join(p.root, ".shoal.toml"), 0o644
		}
		_, gerr := os.Stat(filepath.Join(p.root, "go.mod"))
		if err := writeFileAtomic(cfgPath, []byte(config.Template(gerr == nil)), perm); err != nil {
			return err
		}
	}
	if err := env.loadConfig(p); err != nil {
		return err
	}

	hookFile, _, err := installHooks(p.root, env.Exe)
	if err != nil {
		return err
	}
	common, err := gitx.CommonDir(ctx, p.root)
	if err != nil {
		return err
	}
	if _, err := ensureExclude(common, []string{"/.claude/settings.local.json", "/.shoal/"}); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "project  %s (%s)\ndb       %s\nconfig   %s\nhooks    %s\n", p.id, p.remote, p.dbPath(), p.cfgPath, hookFile)
	if p.cfg.Tests.Command == "" && p.cfg.Tests.Format != "none" {
		fmt.Fprintf(env.Stdout, "\nSet [tests].command in %s before starting a task, or set format = \"none\" if the project has no tests.\n", p.cfgPath)
	}
	return nil
}
