package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fence scanner follows CommonMark §4.5: an opener is 3+ backticks or
// tildes after 0-3 spaces (a backtick info string holds no backtick); the
// closer is the same character, at least as long, after 0-3 spaces, with
// only whitespace after it; an unclosed fence runs to the end.
func TestFencesCommonMark(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     string
		fenced string // per line: 'f' fence line or inside one, '.' outside
		open   int    // line of the fence still open at the end
	}{
		{"plain", "a\n```\nb\n```\nc\n", ".fff.", 0},
		{"tildes", "~~~md\nb\n~~~\n", "fff", 0},
		{"four backticks hold three", "````md\n```\nx\n```\n````\nc\n", "fffff.", 0},
		{"tilde does not close backticks", "```\nx\n~~~\ny\n```\nz\n", "fffff.", 0},
		{"backticks do not close tildes", "~~~\n```\n~~~\nz\n", "fff.", 0},
		{"closer with info string is content", "```\nx\n```bash\ny\n```\nz\n", "fffff.", 0},
		{"shorter closer is content", "````\n```\n````\nz\n", "fff.", 0},
		{"longer closer closes", "```\nx\n`````\nz\n", "fff.", 0},
		{"closer with trailing space", "```\nx\n```  \t\nz\n", "fff.", 0},
		{"three-space indent opens and closes", "   ```\nx\n   ```\nz\n", "fff.", 0},
		{"four-space indent is not a fence", "    ```\nx\n", "..", 0},
		{"four-space closer is content", "```\n    ```\nx\n```\nz\n", "ffff.", 0},
		{"tab indent is not a fence", "\t```\nx\n", "..", 0},
		{"backtick info with backtick is not a fence", "``` a`b\nx\n", "..", 0},
		{"tildes may hold backticks in info", "~~~ a`b\nx\n~~~\nz\n", "fff.", 0},
		{"two backticks are not a fence", "``\nx\n", "..", 0},
		{"inline code is not a fence", "use ```x``` here\nx\n", "..", 0},
		{"unclosed runs to the end", "a\n```go\ncode\n", ".ff", 2},
		{"unclosed inner fence stays content", "````md\n```go\nx\n````\nz\n", "ffff.", 0},
		{"crlf", "a\r\n```\r\nb\r\n```\r\nc\r\n", ".fff.", 0},
		{"bom before an opener", "\xef\xbb\xbf```\nb\n```\nc\n", "fff.", 0},
	} {
		var f fences
		var got strings.Builder
		for i, l := range splitLines([]byte(tc.in)) {
			if f.skip(l, i+1) {
				got.WriteByte('f')
			} else {
				got.WriteByte('.')
			}
		}
		if got.String() != tc.fenced || f.open != tc.open {
			t.Errorf("%s: fenced %q open %d, want %q open %d", tc.name, got.String(), f.open, tc.fenced, tc.open)
		}
	}
}

// Vex round 2 repros: markers inside a nested or mis-closed fence are an
// example; the real block is appended after it, importsContext does not
// count the example, and no fenced code is read as a project rule.
func TestFencedMarkersRepros(t *testing.T) {
	example := blockBegin + "\n@example\n" + blockEnd + "\n"
	block := blockBegin + "\n" + testImport + "\n" + blockEnd + "\n"
	for name, in := range map[string]string{
		"four backticks hold three":  "# Docs\n````md\n```\n" + example + "```\n````\n",
		"tilde inside backticks":     "```\n~~~\n" + example + "```\n",
		"bash line inside backticks": "```\n```bash\n" + example + "```\n",
		"unclosed inner fence":       "````md\n```go\n" + example + "````\n",
		"four-space indented ticks":  "Rules\n    ```\n",
		"crlf":                       strings.ReplaceAll("````md\n```\n"+example+"```\n````\n", "\n", "\r\n"),
	} {
		got, changed, err := withManagedBlock("CLAUDE.md", []byte(in), testImport)
		want := in + strings.ReplaceAll(block, "\n", eolOf(in))
		if err != nil || !changed || string(got) != want {
			t.Errorf("%s: %q %v %v, want %q", name, got, changed, err, want)
			continue
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(in), 0o644); err != nil {
			t.Fatal(err)
		}
		if importsContext(dir) {
			t.Errorf("%s: the fenced example counts as the block", name)
		}
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), got, 0o644); err != nil {
			t.Fatal(err)
		}
		if !importsContext(dir) {
			t.Errorf("%s: the appended block is not found", name)
		}
		for _, r := range ruleLines("CLAUDE.md", []byte(in)) {
			if strings.Contains(r.text, "example") {
				t.Errorf("%s: fenced line read as a rule: %+v", name, r)
			}
		}
	}
	// Nested: code inside the outer fence is not a rule, so it cannot
	// conflict.
	in := "````md\n```go\nNever use tabs\n````\nUse spaces\n"
	rules := ruleLines("CLAUDE.md", []byte(in))
	if len(rules) != 1 || rules[0].text != "Use spaces" || rules[0].where != "CLAUDE.md:5" {
		t.Errorf("nested fence rules: %+v", rules)
	}
	// An unclosed fence still runs to the end: its markers are not a block
	// and init refuses rather than append a block the fence would swallow.
	_, _, err := withManagedBlock("CLAUDE.md", []byte("a\n```go\n"+example), testImport)
	if err == nil || codeOf(err) != ExitConfig || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("unclosed fence: %v", err)
	}
}

func eolOf(s string) string {
	if strings.Contains(s, "\r\n") {
		return "\r\n"
	}
	return "\n"
}
