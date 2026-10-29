package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func TestAgentsInstallStealthAndCommit(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("agents", "install")
	user := filepath.Join(h.home, ".claude", "agents")
	if !strings.Contains(out, user) || strings.Count(out, "written") != 8 {
		t.Fatalf("install:\n%s", out)
	}
	for _, role := range roles {
		body := readFile(t, filepath.Join(user, "shoal-"+role+".md"))
		if !strings.Contains(body, "model: inherit") || !strings.Contains(body, "shoal_version: 1") {
			t.Fatalf("shoal-%s:\n%s", role, body)
		}
	}
	if _, err := os.Stat(filepath.Join(h.repo, ".claude")); err == nil {
		t.Fatal("stealth install touched the repo")
	}
	testutil.WriteFile(t, filepath.Join(user, "shoal-lead.md"), "mine\n")
	out = h.mustRun("agents", "install")
	if !strings.Contains(out, "left alone") || readFile(t, filepath.Join(user, "shoal-lead.md")) != "mine\n" {
		t.Fatalf("reinstall over an edit:\n%s", out)
	}
	if list := h.mustRun("agents", "list"); !strings.Contains(list, "shoal-lead") || !strings.Contains(list, "modified") {
		t.Fatalf("list:\n%s", list)
	}

	h.mustRun("agents", "install", "--commit")
	if _, err := os.Stat(filepath.Join(h.repo, ".claude", "agents", "shoal-qa-scope.md")); err != nil {
		t.Fatal(err)
	}
	if st := testutil.Git(t, h.repo, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(st, ".claude/agents/shoal-lead.md") {
		t.Fatalf("--commit files must be visible to git (not excluded): %q", st)
	}
	h.expectCode(ExitUsage, "agents", "install", "--bogus")
}
