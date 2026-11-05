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

// "No X" and "X, not Y" negate one word, which is enough against a rule
// that names it (§5.3).
func TestConflictWithOneWordNegation(t *testing.T) {
	rules := []projectRule{{"CLAUDE.md:2", "Use tabs for indentation"}}
	for _, tc := range []struct{ text, want string }{
		{"Indent with spaces, not tabs", "CLAUDE.md:2"},
		{"No tabs", "CLAUDE.md:2"},
		{"Never tabs; spaces only", "CLAUDE.md:2"},
		// A negated clause of several words still needs two in common.
		{"Never use tabs in YAML files", ""},
		{"Indent with spaces", ""},
	} {
		got := conflictWith(tc.text, rules)
		if (tc.want == "") != (got == "") || !strings.HasPrefix(got, tc.want) {
			t.Errorf("%q: got %q, want %q", tc.text, got, tc.want)
		}
	}
	// A one-word negated project rule against a longer convention.
	if got := conflictWith("Use tabs for indentation in Go files", []projectRule{{".claude/CLAUDE.md:1", "No tabs"}}); !strings.HasPrefix(got, ".claude/CLAUDE.md:1") {
		t.Errorf("one-word negated rule: %q", got)
	}
}

func TestReadProjectRulesDotClaude(t *testing.T) {
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, ".claude", "CLAUDE.md"), "Use tabs for indentation\n")
	rules, err := readProjectRules(root)
	if err != nil || len(rules) != 1 || rules[0].where != ".claude/CLAUDE.md:1" {
		t.Fatalf("rules %+v %v", rules, err)
	}
}

// An unpaired begin marker (init refuses that file, but agree, promote and
// propose still read it) hides nothing: the rules after it are read, so the
// conflict check is not blind. A paired block's lines stay unread.
func TestRuleLinesReadPastUnpairedBegin(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want []string
	}{
		"unpaired begin":        {"Use spaces\n" + blockBegin + "\nNever use tabs\n", []string{"Use spaces", "Never use tabs"}},
		"stray end":             {blockEnd + "\nNever use tabs\n", []string{"Never use tabs"}},
		"paired then unpaired":  {blockBegin + "\n@x\n" + blockEnd + "\n" + blockBegin + "\nNever use tabs\n", []string{"Never use tabs"}},
		"paired block unread":   {"a\n" + blockBegin + "\n@x\n" + blockEnd + "\nb\n", []string{"a", "b"}},
		"unpaired begin, crlf":  {blockBegin + "\r\nNever use tabs\r\n", []string{"Never use tabs"}},
		"fenced end not a pair": {blockBegin + "\n```\n" + blockEnd + "\n```\nNever use tabs\n", []string{"Never use tabs"}},
	} {
		var got []string
		for _, r := range ruleLines("CLAUDE.md", []byte(tc.in)) {
			got = append(got, r.text)
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
	root := t.TempDir()
	testutil.WriteFile(t, filepath.Join(root, "CLAUDE.md"), "# Rules\n"+blockBegin+"\nNever use tabs\n")
	rules, err := readProjectRules(root)
	if err != nil {
		t.Fatal(err)
	}
	if c := conflictWith("Use tabs for indentation", rules); c != "CLAUDE.md:3: Never use tabs" {
		t.Errorf("conflict after an unpaired begin: %q", c)
	}
}
