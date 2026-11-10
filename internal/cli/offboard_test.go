package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/lockfile"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func TestExportWritesEveryTableAndReport(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("new", "bugfix", "Users page totals")
	p := h.project()
	testutil.WriteFile(t, filepath.Join(p.dir, "reports", "1-users-page-totals.md"), "# report\n")

	var doc exportDoc
	if err := json.Unmarshal([]byte(h.mustRun("export", p.id)), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Format != "shoalctl-export" || doc.FormatVersion != 1 || doc.Project.ID != p.id || doc.Project.Root != p.root || doc.SchemaVersion == 0 {
		t.Fatalf("header: %+v", doc)
	}
	if tasks := doc.Tables["tasks"]; len(tasks) != 1 || tasks[0]["title"] != "Users page totals" {
		t.Fatalf("tasks: %+v", tasks)
	}
	if _, ok := doc.Tables["memory_fts"]; ok || doc.Reports["1-users-page-totals.md"] != "# report\n" {
		t.Fatalf("tables %d, reports %v", len(doc.Tables), doc.Reports)
	}

	// --out: a new 0600 file, never an overwrite.
	out := filepath.Join(t.TempDir(), "shoal.json")
	h.env.Dir = filepath.Dir(out)
	h.mustRun("export", p.id, "--out", "shoal.json")
	if info, err := os.Stat(out); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("export file: %v %v", info, err)
	}
	h.expectCode(ExitState, "export", p.id, "--out", out)
	h.expectCode(ExitUsage, "export", "../x")
	h.expectCode(ExitUsage, "export")
	h.expectCode(ExitNotFound, "export", "0123456789ab")
}

func TestPurgeNeedsAHumanWhoTypesTheID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("new", "feature", "Clients")
	p := h.project()

	mustContain(t, "agent", h.expectCode(ExitState, "purge", p.id), "needs a human at a terminal")
	h.asHuman() // answers "y", not the id
	mustContain(t, "y", h.expectCode(ExitState, "purge", p.id), "purge not confirmed", "nothing was deleted")
	if !fileExists(p.dbPath()) {
		t.Fatal("an unconfirmed purge deleted the project")
	}

	wt := filepath.Join(p.dir, "worktrees", "1-clients")
	os.MkdirAll(wt, 0o700)
	h.answers = p.id + "\n"
	mustContain(t, "worktree", h.expectCode(ExitState, "purge", p.id), "still has 1 task worktree(s): "+wt, "shoalctl gc")
	os.Remove(wt)

	l, err := lockfile.Acquire(filepath.Join(p.dir, "locks", "merge.lock"), 0, lockfile.Info{PID: 1, Command: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "busy", h.expectCode(ExitBusy, "purge", p.id), "another shoalctl holds merge.lock")
	l.Release()

	out := h.mustRun("purge", p.id)
	mustContain(t, "purge", out, "memory.db (1 task(s), 0 record(s))", "still registers shoal's hooks", "Type the project id", "purged project "+p.id)
	if fileExists(p.dir) {
		t.Fatalf("%s survived the purge", p.dir)
	}
	if left, _ := os.ReadDir(filepath.Join(p.home, "projects")); len(left) != 0 {
		t.Fatalf("left in projects/: %v", left)
	}
	h.expectCode(ExitNotFound, "export", p.id)
	if !fileExists(filepath.Join(h.repo, ".claude", "settings.local.json")) {
		t.Fatal("purge touched the repo")
	}
}
