package guide

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestREADMEQuotesQuickSetupAndTestsVerbatimBetweenMarkers(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := strings.ReplaceAll(string(raw), "\r\n", "\n")
	quick, err := Quick()
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{"quick": quick}
	for _, name := range []string{"setup", "tests"} {
		if bodies[name], err = Topic(name); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range bodies {
		body = strings.ReplaceAll(body, "\r\n", "\n")
		want := "<!-- guide:" + name + " -->\n```text\n" + body + "```\n<!-- /guide:" + name + " -->\n"
		if strings.Count(readme, want) != 1 {
			t.Errorf("README.md must hold docs/guide/%s.md verbatim exactly once, as:\n%s", name, want)
		}
	}
}

func TestCheatSheetOpensWithTheQuickFlow(t *testing.T) {
	t.Parallel()
	quick, err := Quick()
	if err != nil {
		t.Fatal(err)
	}
	index, err := Index()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(index, quick) {
		t.Errorf("the cheat sheet does not open with quick.md:\n%s", index)
	}
	if !strings.HasSuffix(quick, "\nEverything else is optional.\n") {
		t.Errorf("quick.md must end with the line \"Everything else is optional.\":\n%s", quick)
	}
	lines := strings.Split(strings.TrimSuffix(quick, "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[len(lines)-2], "5. ") {
		t.Errorf("step 5 must be directly followed by \"Everything else is optional.\":\n%s", quick)
	}
	for i := 1; i <= 5; i++ {
		if strings.Count(quick, fmt.Sprintf("\n%d. ", i)) != 1 {
			t.Errorf("quick.md must hold step %d exactly once:\n%s", i, quick)
		}
	}
	if strings.Contains(quick, "\n6. ") {
		t.Errorf("quick.md has more than five steps:\n%s", quick)
	}
}
