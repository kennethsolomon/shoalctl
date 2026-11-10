package guide

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestREADMEQuotesSetupAndTestsVerbatimBetweenMarkers(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := strings.ReplaceAll(string(raw), "\r\n", "\n")
	for _, name := range []string{"setup", "tests"} {
		body, err := Topic(name)
		if err != nil {
			t.Fatal(err)
		}
		body = strings.ReplaceAll(body, "\r\n", "\n")
		want := "<!-- guide:" + name + " -->\n```text\n" + body + "```\n<!-- /guide:" + name + " -->\n"
		if strings.Count(readme, want) != 1 {
			t.Errorf("README.md must hold docs/guide/%s.md verbatim exactly once, as:\n%s", name, want)
		}
	}
}
