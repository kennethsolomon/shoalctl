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
