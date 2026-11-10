package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// projectIDPattern is gitx.ProjectID's shape: 12 lowercase hex characters.
var projectIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// registeredProject finds a project under $SHOAL_HOME by id, without a
// checkout: offboarding works after the repo is gone.
func (env Env) registeredProject(id string) (*project, projectMeta, error) {
	if !projectIDPattern.MatchString(id) {
		return nil, projectMeta{}, fail(ExitUsage, "%q is not a project id (12 hex characters, as init prints it)", id)
	}
	home, err := env.shoalHome()
	if err != nil {
		return nil, projectMeta{}, err
	}
	p := &project{id: id, home: home, dir: filepath.Join(home, "projects", id)}
	meta, ok, err := readMeta(p.metaPath())
	if err != nil {
		return nil, meta, err
	}
	if !ok {
		return nil, meta, fail(ExitNotFound, "no project %s under %s", id, filepath.Join(home, "projects"))
	}
	p.remote, p.root = meta.Remote, meta.Root
	return p, meta, nil
}

// exportDoc is what export writes: every table of memory.db and every
// rendered report.
type exportDoc struct {
	Format        string                      `json:"format"`
	FormatVersion int                         `json:"format_version"`
	ExportedAt    string                      `json:"exported_at"`
	Project       projectMeta                 `json:"project"`
	SchemaVersion int                         `json:"schema_version"`
	Tables        map[string][]map[string]any `json:"tables"`
	Reports       map[string]string           `json:"reports"`
}

// cmdExport writes a project's records and reports as one JSON document
// (§13 offboarding), to stdout or to a new file.
func (env Env) cmdExport(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("export", flag.ContinueOnError)
	outPath := fset.String("out", "", "write to this new file (mode 0600) instead of stdout")
	pos, err := parseArgs(fset, args, 1)
	if err != nil {
		return err
	}
	p, meta, err := env.registeredProject(pos[0])
	if err != nil {
		return err
	}
	if p.db, err = store.Open(ctx, p.dbPath(), store.Options{Migrate: true, BusyTimeout: 5 * time.Second, Retries: 2}); err != nil {
		return err
	}
	defer p.close()
	dump, err := p.db.Dump(ctx)
	if err != nil {
		return err
	}
	reports, err := readReports(filepath.Join(p.dir, "reports"))
	if err != nil {
		return err
	}
	doc := exportDoc{Format: "shoalctl-export", FormatVersion: 1, ExportedAt: nowUTC(), Project: meta,
		SchemaVersion: dump.SchemaVersion, Tables: dump.Tables, Reports: reports}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the export: %w", err)
	}
	body = append(body, '\n')
	if *outPath == "" {
		_, err := env.Stdout.Write(body)
		return err
	}
	path := *outPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(env.Dir, path)
	}
	if err := writeNewFile(path, body); err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "exported project %s (%d tables, %d reports) to %s\n", p.id, len(dump.Tables), len(reports), path)
	return nil
}

// readReports reads every rendered report, by file name.
func readReports(dir string) (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read report %s: %w", e.Name(), err)
		}
		out[e.Name()] = string(data)
	}
	return out, nil
}

// writeNewFile creates path (mode 0600) and refuses to replace a file.
func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fail(ExitState, "%s already exists; export never overwrites a file", path)
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// cmdPurge deletes a project's directory under $SHOAL_HOME: memory.db,
// reports, runs, context.md, config and backups (§13 offboarding). Only a
// human at a terminal, who types the project id, can purge; a project with
// task worktrees left is refused, since their git metadata lives in the
// repo. The repo itself is not touched (uninstall does that).
func (env Env) cmdPurge(ctx context.Context, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("purge", flag.ContinueOnError), args, 1)
	if err != nil {
		return err
	}
	if err := env.requireHuman("purge"); err != nil {
		return err
	}
	p, meta, err := env.registeredProject(pos[0])
	if err != nil {
		return err
	}
	if err := worktreesLeft(p); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "purge deletes %s: memory.db (%s), reports, test runs, context.md, config and backups of project %s (%s).\n",
		p.dir, env.purgeContents(ctx, p), p.id, meta.Remote)
	if hooksStillRegistered(meta.Root) {
		fmt.Fprintf(env.Stdout, "%s still registers shoal's hooks; they do nothing without the project, but run shoalctl uninstall there to remove them.\n",
			filepath.Join(meta.Root, ".claude", "settings.local.json"))
	}
	fmt.Fprintf(env.Stdout, "It cannot be undone; shoalctl export %s keeps a copy first.\nType the project id to delete it: ", p.id)
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read the answer: %w", err)
	}
	if strings.TrimSpace(line) != p.id {
		return fail(ExitState, "purge not confirmed (the answer was not %s); nothing was deleted", p.id)
	}

	// No other shoalctl may be using the project: take its locks, check
	// again, then move the directory aside in one rename so nothing ever
	// sees half of it, and delete it.
	var held []*lockfile.Lock
	defer func() {
		for _, l := range held {
			l.Release()
		}
	}()
	for _, name := range []string{"merge.lock", "git.lock"} {
		l, err := lockfile.Acquire(filepath.Join(p.dir, "locks", name), 0, lockfile.Info{PID: os.Getpid(), Command: "purge", StartedAt: nowUTC()})
		if errors.Is(err, lockfile.ErrHeld) {
			return fail(ExitBusy, "another shoalctl holds %s of project %s (%v); let it finish, then purge. Nothing was deleted", name, p.id, err)
		}
		if err != nil {
			return err
		}
		held = append(held, l)
	}
	if err := worktreesLeft(p); err != nil {
		return err
	}
	for _, l := range held {
		l.Release()
	}
	held = nil
	trash := filepath.Join(p.home, "projects", fmt.Sprintf(".purged-%s-%d", p.id, os.Getpid()))
	if err := os.Rename(p.dir, trash); err != nil {
		return fmt.Errorf("move %s aside to delete it: %w; nothing was deleted", p.dir, err)
	}
	if err := os.RemoveAll(trash); err != nil {
		return fmt.Errorf("delete %s (moved from %s): %w; delete what is left yourself", trash, p.dir, err)
	}
	fmt.Fprintf(env.Stdout, "purged project %s: %s is gone\n", p.id, p.dir)
	return nil
}

// worktreesLeft refuses a purge while task worktrees exist: git records
// each one in the repo, and deleting it here would leave that record dangling.
func worktreesLeft(p *project) error {
	dir := filepath.Join(p.dir, "worktrees")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, filepath.Join(dir, e.Name()))
	}
	if len(left) == 0 {
		return nil
	}
	sort.Strings(left)
	return fail(ExitState, "project %s still has %d task worktree(s): %s. Merge the tasks and run shoalctl gc, or remove them with git worktree remove in %s, then purge. Nothing was deleted",
		p.id, len(left), strings.Join(left, ", "), p.root)
}

// purgeContents says what memory.db holds, for the confirmation prompt.
func (env Env) purgeContents(ctx context.Context, p *project) string {
	s, err := store.Open(ctx, p.dbPath(), store.Options{BusyTimeout: 5 * time.Second})
	if err != nil {
		return fmt.Sprintf("not readable: %v", err)
	}
	defer s.Close()
	d, err := s.Dump(ctx)
	if err != nil {
		return fmt.Sprintf("not readable: %v", err)
	}
	records := 0
	for _, t := range []string{"bugs", "decisions", "lessons", "qa", "conventions", "summaries"} {
		records += len(d.Tables[t])
	}
	return fmt.Sprintf("%d task(s), %d record(s)", len(d.Tables["tasks"]), records)
}

// hooksStillRegistered reports whether root's settings.local.json registers
// a shoal hook; false when it cannot be read.
func hooksStillRegistered(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.local.json"))
	if err != nil {
		return false
	}
	has, err := hasShoalHooks(data)
	return err == nil && has
}
