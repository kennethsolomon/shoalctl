package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefsShape(t *testing.T) {
	defs, err := Defs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"shoal-backend", "shoal-docs", "shoal-frontend", "shoal-integration", "shoal-lead", "shoal-qa-breaker", "shoal-qa-regression", "shoal-qa-scope"}
	if len(defs) != len(want) {
		t.Fatalf("defs: %d", len(defs))
	}
	for i, d := range defs {
		body := string(d.Body)
		if d.Name != want[i] || !strings.HasPrefix(body, "---\nname: "+d.Name+"\n") || !strings.Contains(body, "\nmodel: inherit\n") ||
			!strings.Contains(body, "\nshoal_version: 1\n") || !strings.Contains(body, "\ntools: ") || !strings.Contains(body, "\ndescription: ") {
			t.Errorf("%s: bad frontmatter:\n%s", d.Name, body)
		}
	}
}

func TestInstallKeepsEditedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agents")
	man := filepath.Join(t.TempDir(), "m.json")
	got, err := Install(dir, man)
	if err != nil || len(got) != 8 || got[0].Action != "written" {
		t.Fatalf("%+v %v", got, err)
	}
	lead := filepath.Join(dir, "shoal-lead.md")
	os.WriteFile(lead, []byte("my own lead\n"), 0o644)
	stale := filepath.Join(dir, "shoal-docs.md")
	os.WriteFile(stale, []byte("old shoal docs\n"), 0o644)
	// Pretend shoal wrote the old docs file.
	body, _ := json.Marshal(Manifest{Files: map[string]string{stale: hash([]byte("old shoal docs\n"))}})
	os.WriteFile(man, body, 0o600)
	list, _ := List(dir, man)
	status := map[string]string{}
	for _, e := range list {
		status[e.Name] = e.Status
	}
	if status["shoal-lead"] != StatusModified || status["shoal-docs"] != StatusOutdated || status["shoal-qa-scope"] != StatusInstalled {
		t.Fatalf("list %+v", status)
	}
	if _, err := Install(dir, man); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(lead); string(b) != "my own lead\n" {
		t.Fatal("edited file overwritten")
	}
	if b, _ := os.ReadFile(stale); !strings.Contains(string(b), "name: shoal-docs") {
		t.Fatal("outdated shoal file not updated")
	}
}

func TestRemoveKeepsEditedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agents")
	man := filepath.Join(t.TempDir(), "m.json")
	if _, err := Install(dir, man); err != nil {
		t.Fatal(err)
	}
	lead := filepath.Join(dir, "shoal-lead.md")
	os.WriteFile(lead, []byte("my own lead\n"), 0o644)
	os.Remove(filepath.Join(dir, "shoal-docs.md")) // already gone: not reported
	mine := filepath.Join(dir, "my-agent.md")
	os.WriteFile(mine, []byte("not shoal's\n"), 0o644)

	got, err := Remove(dir, man)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, e := range got {
		actions[e.Name] = e.Action
	}
	if len(got) != 7 || actions["shoal-backend"] != "removed" || !strings.HasPrefix(actions["shoal-lead"], "left alone") {
		t.Fatalf("Remove: %+v", got)
	}
	if _, ok := actions["shoal-docs"]; ok {
		t.Fatalf("a missing file was reported: %+v", got)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 2 {
		t.Fatalf("left in %s: %v (want shoal-lead.md and my-agent.md)", dir, left)
	}
	if b, _ := os.ReadFile(lead); string(b) != "my own lead\n" {
		t.Fatal("edited file changed")
	}
	m, _ := readManifest(man)
	if len(m.Files) != 1 || m.Files[lead] == "" {
		t.Fatalf("manifest after Remove: %+v (want only the edited lead, still recorded)", m.Files)
	}
	// A second run removes nothing more.
	if again, err := Remove(dir, man); err != nil || len(again) != 1 || again[0].Name != "shoal-lead" {
		t.Fatalf("second Remove: %+v %v", again, err)
	}
}

// After a revision the human asks for, the lead re-runs the QA agents
// through shoalctl test --kind qa, and never runs the human's shoalctl qa.
func TestLeadReRunsQAAfterARevision(t *testing.T) {
	t.Parallel()
	defs, err := Defs()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		if d.Name != "shoal-lead" {
			continue
		}
		for _, want := range []string{"After any revision the human asks for in this window", "re-run `shoal-qa-breaker` and `shoal-qa-regression` (`shoalctl test <task> --kind qa`)",
			"it refuses inside Claude Code, so never run it yourself"} {
			if !strings.Contains(string(d.Body), want) {
				t.Errorf("shoal-lead lacks %q:\n%s", want, d.Body)
			}
		}
		return
	}
	t.Fatal("no shoal-lead definition")
}
