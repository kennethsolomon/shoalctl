package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kennethsolomon/shoalctl/internal/agents"
	"github.com/kennethsolomon/shoalctl/internal/gitx"
)

// cmdUninstall removes what init and agents install put in the repo and in
// Claude Code's config: shoal's hook entries in .claude/settings.local.json,
// the managed CLAUDE.md block, and the shoal-* agent definitions nobody
// edited (§4.4, §4.6, §5.1, §7.1). Nothing else: the project's data under
// $SHOAL_HOME stays until purge, and .git/info/exclude keeps its lines.
// Every file is checked before any is written, so a refusal changes nothing.
func (env Env) cmdUninstall(ctx context.Context, args []string) error {
	if _, err := parseArgs(flag.NewFlagSet("uninstall", flag.ContinueOnError), args, 0); err != nil {
		return err
	}
	p, err := env.identify(ctx)
	if err != nil {
		return err
	}
	hooks, err := planHookRemoval(p)
	if err != nil {
		return err
	}
	block, err := planBlockRemoval(ctx, p.root)
	if err != nil {
		return err
	}

	if err := hooks.apply(); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, "hooks    "+hooks.summary())
	if err := block.apply(); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, "block    "+block.summary())
	if err := env.uninstallAgents(p); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "kept     %s (the project's memory, reports and config): shoalctl export %s, then shoalctl purge %s, to remove it\n", p.dir, p.id, p.id)
	return nil
}

// hookRemoval is the planned edit of the main checkout's settings.local.json.
type hookRemoval struct {
	path    string
	perm    fs.FileMode
	out     []byte
	removed int
	remove  bool // shoal created the file and nothing else is left in it
}

func planHookRemoval(p *project) (hookRemoval, error) {
	h := hookRemoval{path: filepath.Join(p.root, ".claude", "settings.local.json")}
	if err := refuseSymlinkedSettings(p.root); err != nil {
		return h, err
	}
	data, err := os.ReadFile(h.path)
	if errors.Is(err, fs.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return h, fmt.Errorf("read %s: %w", h.path, err)
	}
	info, err := os.Stat(h.path)
	if err != nil {
		return h, fmt.Errorf("stat %s: %w", h.path, err)
	}
	h.perm = info.Mode().Perm()
	// init keeps the file's bytes from before its first change here; with
	// no backup, shoal created the file.
	backup, err := os.ReadFile(filepath.Join(p.dir, "backups", "settings.local.json.bak"))
	created := errors.Is(err, fs.ErrNotExist)
	if err != nil && !created {
		return h, fmt.Errorf("read the backup of %s: %w", h.path, err)
	}
	if h.out, h.removed, err = removeHooks(data, hasMember(backup, "hooks")); err != nil {
		return h, fail(ExitConfig, "%s: %v; fix it, shoal will not rewrite it. Nothing was changed", h.path, err)
	}
	h.remove = h.removed > 0 && created && isEmptyObject(h.out)
	return h, nil
}

func (h hookRemoval) apply() error {
	switch {
	case h.removed == 0:
		return nil
	case h.remove:
		if err := os.Remove(h.path); err != nil {
			return fmt.Errorf("remove %s: %w", h.path, err)
		}
		return removeIfEmpty(filepath.Dir(h.path))
	}
	return writeFileAtomic(h.path, h.out, h.perm)
}

func (h hookRemoval) summary() string {
	switch {
	case h.removed == 0:
		return "no shoal entries in " + h.path
	case h.remove:
		return fmt.Sprintf("removed %d shoal entr%s and %s, which shoal created", h.removed, plural(h.removed, "y", "ies"), h.path)
	}
	return fmt.Sprintf("removed %d shoal entr%s from %s; your own settings stay", h.removed, plural(h.removed, "y", "ies"), h.path)
}

// blockRemoval is the planned edit of the main checkout's CLAUDE.md.
type blockRemoval struct {
	path    string
	perm    fs.FileMode
	out     []byte
	found   bool
	remove  bool   // nothing is left and git does not track the file
	skipped string // why the file was not read
	absent  bool
}

func planBlockRemoval(ctx context.Context, root string) (blockRemoval, error) {
	b := blockRemoval{path: filepath.Join(root, "CLAUDE.md")}
	info, err := os.Lstat(b.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		b.absent = true
		return b, nil
	case err != nil:
		return b, fmt.Errorf("stat %s: %w", b.path, err)
	case info.Mode()&fs.ModeSymlink != 0:
		b.skipped = "a symlink, which init never writes through"
		return b, nil
	case !info.Mode().IsRegular():
		b.skipped = "not a regular file"
		return b, nil
	}
	b.perm = info.Mode().Perm()
	data, err := os.ReadFile(b.path)
	if err != nil {
		return b, fmt.Errorf("read %s: %w", b.path, err)
	}
	if b.out, b.found, err = withoutManagedBlock("CLAUDE.md", data); err != nil || !b.found {
		return b, err
	}
	if len(b.out) == 0 {
		_, terr := gitx.Run(ctx, root, "ls-files", "--error-unmatch", "--", "CLAUDE.md")
		b.remove = terr != nil
	}
	return b, nil
}

func (b blockRemoval) apply() error {
	switch {
	case !b.found:
		return nil
	case b.remove:
		if err := os.Remove(b.path); err != nil {
			return fmt.Errorf("remove %s: %w", b.path, err)
		}
		return nil
	}
	return writeFileAtomic(b.path, b.out, b.perm)
}

func (b blockRemoval) summary() string {
	switch {
	case b.absent:
		return "no " + b.path
	case b.skipped != "":
		return fmt.Sprintf("%s not read: %s", b.path, b.skipped)
	case !b.found:
		return "no shoal block in " + b.path
	case b.remove:
		return "removed " + b.path + ": it held only the shoal block and git does not track it"
	}
	return "removed the shoal block from " + b.path + "; every other line stays"
}

// uninstallAgents removes the shoal-* definitions nobody edited from the
// repo's .claude/agents/, and from the user-level directory too unless
// another shoal project still uses them (§7.1).
func (env Env) uninstallAgents(p *project) error {
	manifest := filepath.Join(p.home, "agents-manifest.json")
	dirs := []string{filepath.Join(p.root, ".claude", "agents")}
	userDir, err := env.userAgentsDir()
	if err != nil {
		return err
	}
	others, err := otherProjects(p)
	if err != nil {
		return err
	}
	if len(others) == 0 {
		dirs = append(dirs, userDir)
	}
	removed, kept := 0, 0
	for _, dir := range dirs {
		entries, err := agents.Remove(dir, manifest)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Action == "removed" {
				removed++
				continue
			}
			kept++
			fmt.Fprintf(env.Stdout, "agents   %s %s\n", e.Path, e.Action)
		}
	}
	fmt.Fprintf(env.Stdout, "agents   removed %d unedited shoal-* definition(s)", removed)
	if kept > 0 {
		fmt.Fprintf(env.Stdout, "; %d edited one(s) left alone", kept)
	}
	fmt.Fprintln(env.Stdout)
	if len(others) > 0 {
		fmt.Fprintf(env.Stdout, "agents   %s kept: %d other shoal project(s) use them (%s); purge those first, or delete the files yourself\n",
			userDir, len(others), others[0])
	}
	return nil
}

// otherProjects lists the ids of the projects registered under $SHOAL_HOME
// besides p.
func otherProjects(p *project) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(p.home, "projects"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", filepath.Join(p.home, "projects"), err)
	}
	var out []string
	for _, e := range entries {
		if e.Name() == p.id || !e.IsDir() {
			continue
		}
		if fileExists(filepath.Join(p.home, "projects", e.Name(), "project.json")) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// removeIfEmpty removes dir when it holds nothing.
func removeIfEmpty(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return nil
	}
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("remove the empty %s: %w", dir, err)
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
