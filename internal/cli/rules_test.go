package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func TestReadProjectRules(t *testing.T) {
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "CLAUDE.md"), "# Project\n\n- Never use the strategy pattern\n```\nnever use code blocks\n```\n"+
		blockBegin+"\n@~/.shoal/x/context.md\n"+blockEnd+"\n1. Run gofmt\n")
	testutil.WriteFile(t, filepath.Join(root, "CLAUDE.local.md"), "> keep secrets out of logs\r\n")
	testutil.WriteFile(t, filepath.Join(root, ".claude", "rules", "api", "style.md"), "Avoid global state\n")
	testutil.WriteFile(t, filepath.Join(root, ".claude", "rules", "notes.txt"), "not a rule file\n")
	rules, err := readProjectRules(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rules {
		got = append(got, r.where+" "+r.text)
	}
	want := []string{"CLAUDE.md:1 Project", "CLAUDE.md:3 Never use the strategy pattern", "CLAUDE.md:10 Run gofmt",
		"CLAUDE.local.md:1 keep secrets out of logs", ".claude/rules/api/style.md:1 Avoid global state"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if none, err := readProjectRules(t.TempDir()); err != nil || len(none) != 0 {
		t.Fatalf("no rule files: %v %v", none, err)
	}
}

func TestConflictWith(t *testing.T) {
	rules := []projectRule{
		{"CLAUDE.md:3", "Never use the strategy pattern; keep plain switch statements."},
		{"CLAUDE.md:4", "Don't mock the database in integration tests"},
		{"CLAUDE.md:5", "Use tabs"},
		{"CLAUDE.md:6", "Commit small, focused changes"},
	}
	for _, tc := range []struct{ text, want string }{
		{"Use the strategy pattern for payment providers", "CLAUDE.md:3: Never use the strategy pattern; keep plain switch statements."},
		{"Never use switch statements for providers", "CLAUDE.md:3"},
		{"Mock the database in integration tests to keep them fast", "CLAUDE.md:4"},
		{"Never use tabs", "CLAUDE.md:5"},
		// Same polarity, or not enough in common: no conflict.
		{"Do not use the strategy pattern", ""},
		{"Never commit secrets", ""},
		{"Never use tabs in YAML files", ""},
		{"Avoid the repository pattern for payment providers", ""},
	} {
		got := conflictWith(tc.text, rules)
		if (tc.want == "") != (got == "") || !strings.HasPrefix(got, tc.want) {
			t.Errorf("%q: got %q, want %q", tc.text, got, tc.want)
		}
	}
}
