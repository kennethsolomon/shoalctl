package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testImport = "@~/.shoal/projects/abc/context.md"

func TestWithManagedBlockAppends(t *testing.T) {
	t.Parallel()
	block := blockBegin + "\n" + testImport + "\n" + blockEnd
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", block + "\n"},
		{"trailing newline", "# Rules\n- one\n", "# Rules\n- one\n" + block + "\n"},
		{"no trailing newline", "# Rules\n- one", "# Rules\n- one\n" + block},
		{"crlf", "# Rules\r\n- one\r\n", "# Rules\r\n- one\r\n" + blockBegin + "\r\n" + testImport + "\r\n" + blockEnd + "\r\n"},
		{"crlf no trailing newline", "# Rules\r\n- one", "# Rules\r\n- one\r\n" + blockBegin + "\r\n" + testImport + "\r\n" + blockEnd},
	} {
		got, changed, err := withManagedBlock("CLAUDE.md", []byte(tc.in), testImport)
		if err != nil || !changed || string(got) != tc.want {
			t.Errorf("%s: %q %v %v, want %q", tc.name, got, changed, err, tc.want)
			continue
		}
		if !bytes.HasPrefix(got, []byte(tc.in)) {
			t.Errorf("%s: user content changed", tc.name)
		}
		again, changed, err := withManagedBlock("CLAUDE.md", got, testImport)
		if err != nil || changed || !bytes.Equal(again, got) {
			t.Errorf("%s: second run changed the file: %q %v %v", tc.name, again, changed, err)
		}
	}
}

// A block whose import line differs (edited by hand) gets the right line
// back; everything around it stays byte for byte.
func TestWithManagedBlockRepairsInside(t *testing.T) {
	t.Parallel()
	in := "top\r\n" + blockBegin + "\r\n@elsewhere\r\nextra\r\n" + blockEnd + "  \r\nbottom"
	got, changed, err := withManagedBlock("CLAUDE.md", []byte(in), testImport)
	want := "top\r\n" + blockBegin + "\r\n" + testImport + "\r\n" + blockEnd + "  \r\nbottom"
	if err != nil || !changed || string(got) != want {
		t.Fatalf("%q %v %v", got, changed, err)
	}
}

func TestWithManagedBlockRefusesMalformed(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"two begins":    blockBegin + "\n" + blockBegin + "\n" + blockEnd + "\n",
		"two blocks":    blockBegin + "\nx\n" + blockEnd + "\n" + blockBegin + "\nx\n" + blockEnd + "\n",
		"begin only":    "a\n" + blockBegin + "\n",
		"end only":      blockEnd + "\n",
		"end before":    blockEnd + "\n" + blockBegin + "\n",
		"two ends only": blockEnd + "\n" + blockEnd + "\n",
	} {
		_, _, err := withManagedBlock("CLAUDE.md", []byte(in), testImport)
		if err == nil || codeOf(err) != ExitConfig || !strings.Contains(err.Error(), "CLAUDE.md") {
			t.Errorf("%s: %v (code %d)", name, err, codeOf(err))
		}
	}
}

func TestInstallManagedBlockFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	changed, err := installManagedBlock(dir, testImport)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if got := readFile(t, filepath.Join(dir, "CLAUDE.md")); got != blockBegin+"\n"+testImport+"\n"+blockEnd+"\n" {
		t.Fatalf("created %q", got)
	}
	if !importsContext(dir) || importsContext(t.TempDir()) {
		t.Fatal("importsContext")
	}
	if changed, err := installManagedBlock(dir, testImport); err != nil || changed {
		t.Fatal("second install", changed, err)
	}
	if runtime.GOOS == "windows" {
		return // symlinks need privileges on Windows
	}
	link := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "CLAUDE.md"), filepath.Join(link, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := installManagedBlock(link, testImport); err == nil || codeOf(err) != ExitConfig {
		t.Fatalf("symlinked CLAUDE.md: %v", err)
	}
}

func TestContextImport(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := Env{Getenv: lookupIn(map[string]string{"HOME": home, "USERPROFILE": home})}
	if got := env.contextImport(filepath.Join(home, ".shoal", "projects", "abc", "context.md")); got != testImport {
		t.Errorf("under home: %q", got)
	}
	other := filepath.Join(t.TempDir(), "context.md")
	if got := env.contextImport(other); got != "@"+filepath.ToSlash(other) {
		t.Errorf("outside home: %q", got)
	}
}

// Markers inside a code fence are an example, not the block: the fence
// stays byte for byte and the real block goes outside it (§5.1).
func TestWithManagedBlockIgnoresFences(t *testing.T) {
	t.Parallel()
	block := blockBegin + "\n" + testImport + "\n" + blockEnd
	for name, in := range map[string]string{
		"backticks":       "# Docs\n```\n" + blockBegin + "\n@example\n" + blockEnd + "\n```\n",
		"tildes":          "# Docs\n~~~md\n" + blockBegin + "\n@example\n" + blockEnd + "\n~~~\n",
		"begin only":      "```\n" + blockBegin + "\n```\n",
		"indented fence":  "  ```text\n" + blockEnd + "\n  ```\n",
		"two fenced ones": "```\n" + blockBegin + "\n" + blockEnd + "\n```\n```\n" + blockBegin + "\n" + blockEnd + "\n```\n",
	} {
		got, changed, err := withManagedBlock("CLAUDE.md", []byte(in), testImport)
		if err != nil || !changed || string(got) != in+block+"\n" {
			t.Errorf("%s: %q %v %v", name, got, changed, err)
			continue
		}
		again, changed, err := withManagedBlock("CLAUDE.md", got, testImport)
		if err != nil || changed || !bytes.Equal(again, got) {
			t.Errorf("%s: second run changed the file: %q %v %v", name, again, changed, err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(in), 0o644); err != nil {
			t.Fatal(err)
		}
		if importsContext(dir) {
			t.Errorf("%s: a fenced example counts as the block", name)
		}
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), got, 0o644); err != nil {
			t.Fatal(err)
		}
		if !importsContext(dir) {
			t.Errorf("%s: the real block after a fence is not found", name)
		}
	}
	// A real block is found and repaired next to a fenced example.
	in := "```\n" + blockBegin + "\n@example\n" + blockEnd + "\n```\n" + blockBegin + "\n@old\n" + blockEnd + "\n"
	got, changed, err := withManagedBlock("CLAUDE.md", []byte(in), testImport)
	if want := "```\n" + blockBegin + "\n@example\n" + blockEnd + "\n```\n" + block + "\n"; err != nil || !changed || string(got) != want {
		t.Errorf("repair next to a fence: %q %v %v", got, changed, err)
	}
	// A file that ends inside an open fence would swallow an appended block.
	_, _, err = withManagedBlock("CLAUDE.md", []byte("a\n```go\ncode\n"), testImport)
	if err == nil || codeOf(err) != ExitConfig || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("unclosed fence: %v", err)
	}
}

// A UTF-8 byte order mark before a marker on line 1 is not part of it: the
// block is found, and the BOM stays.
func TestWithManagedBlockBOM(t *testing.T) {
	t.Parallel()
	const bom = "\xef\xbb\xbf"
	block := blockBegin + "\n" + testImport + "\n" + blockEnd + "\n"
	for name, tc := range map[string]struct{ in, want string }{
		"current block": {bom + block + "rest\n", bom + block + "rest\n"},
		"stale block":   {bom + blockBegin + "\n@old\n" + blockEnd + "\nrest\n", bom + block + "rest\n"},
		"no block":      {bom + "# Rules\n", bom + "# Rules\n" + block},
	} {
		got, _, err := withManagedBlock("CLAUDE.md", []byte(tc.in), testImport)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s: %q %v, want %q", name, got, err, tc.want)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), got, 0o644); err != nil {
			t.Fatal(err)
		}
		if !importsContext(dir) {
			t.Errorf("%s: block after a BOM not found", name)
		}
	}
	if rules := ruleLines("CLAUDE.md", []byte(bom+"Never use tabs\n")); len(rules) != 1 || rules[0].text != "Never use tabs" {
		t.Errorf("rule after a BOM: %+v", rules)
	}
}

// A marker counts only at column 0 (after a BOM on line 1), the form
// shoal writes, and outside a fence. Round 3 let 0-3 spaces count; round 4
// changed the rule: any leading whitespace (spaces, a tab, NBSP) makes it
// plain text, so list-item content and indented code can never be the
// block. Plain-text markers stay byte for byte, the real block is
// appended, importsContext does not count them and they hide no rules
// (§5.1, row 75).
func TestWithManagedBlockMarkerColumnZero(t *testing.T) {
	t.Parallel()
	const bom = "\xef\xbb\xbf"
	block := blockBegin + "\n" + testImport + "\n" + blockEnd + "\n"
	example := func(indent string) string {
		return indent + blockBegin + "\n" + indent + "@~/OLD\n" + indent + blockEnd + "\n"
	}
	appended := func(in string) string { return in + strings.ReplaceAll(block, "\n", eolOf(in)) }
	for name, tc := range map[string]struct {
		in, want string
		counts   bool // importsContext on the input
	}{
		"dash list fence":       {"- ```\n" + example("  ") + "  ```\n", "", false},
		"ordered list fence md": {"1. ```md\n" + example("   ") + "   ```\n", "", false},
		"star list fence":       {"* ```\n" + example("  ") + "  ```\n", "", false},
		"plus list fence":       {"+ ~~~\n" + example("  ") + "  ~~~\n", "", false},
		"paren list fence":      {"1) ```\n" + example("   ") + "   ```\n", "", false},
		"one space":             {example(" "), "", false},
		"two spaces":            {example("  "), "", false},
		"three spaces":          {example("   "), "", false},
		"four spaces":           {"Example:\n\n" + example("    "), "", false},
		"tab":                   {example("\t"), "", false},
		"nbsp":                  {example("\u00a0"), "", false},
		"bom on line 2":         {"# Rules\n" + bom + blockBegin + "\n@~/OLD\n" + bom + blockEnd + "\n", "", false},
		"crlf list fence":       {strings.ReplaceAll("- ```\n"+example("  ")+"  ```\n", "\n", "\r\n"), "", false},
		"unclosed list fence":   {"- ```\n" + example("  "), "", false},
		"bom then column 0":     {bom + blockBegin + "\n@~/OLD\n" + blockEnd + "\nrest\n", bom + block + "rest\n", true},
		"crlf column 0": {"a\r\n" + blockBegin + "\r\n@~/OLD\r\n" + blockEnd + "\r\n",
			"a\r\n" + blockBegin + "\r\n" + testImport + "\r\n" + blockEnd + "\r\n", true},
		"trailing space column 0": {blockBegin + " \n@~/OLD\n" + blockEnd + "\t\n",
			blockBegin + " \n" + testImport + "\n" + blockEnd + "\t\n", true},
		"indented example before the real block": {"- " + blockBegin + "\n" + example("  ") + blockBegin + "\n@old\n" + blockEnd + "\n",
			"- " + blockBegin + "\n" + example("  ") + block, true},
	} {
		want := tc.want
		if want == "" {
			want = appended(tc.in)
		}
		got, changed, err := withManagedBlock("CLAUDE.md", []byte(tc.in), testImport)
		if err != nil || !changed || string(got) != want {
			t.Errorf("%s: %q %v %v, want %q", name, got, changed, err, want)
			continue
		}
		again, changed, err := withManagedBlock("CLAUDE.md", got, testImport)
		if err != nil || changed || !bytes.Equal(again, got) {
			t.Errorf("%s: second run changed the file: %q %v %v", name, again, changed, err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(tc.in), 0o644); err != nil {
			t.Fatal(err)
		}
		if importsContext(dir) != tc.counts {
			t.Errorf("%s: importsContext = %v, want %v", name, !tc.counts, tc.counts)
		}
		if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), got, 0o644); err != nil {
			t.Fatal(err)
		}
		if !importsContext(dir) {
			t.Errorf("%s: the real block is not found", name)
		}
		for _, r := range ruleLines("CLAUDE.md", []byte(tc.in)) {
			if strings.Contains(r.text, "@~/OLD") && strings.Contains(name, "list fence") {
				t.Errorf("%s: code in a list-item fence read as a rule: %+v", name, r)
			}
		}
	}
	// A begin marker with any leading whitespace does not open the managed
	// block, so the rule after it is still read; a column-0 one does.
	for in, want := range map[string]bool{
		" " + blockBegin: true, "  " + blockBegin: true, "   " + blockBegin: true, "    " + blockBegin: true,
		"\t" + blockBegin: true, "\u00a0" + blockBegin: true, "- " + blockBegin: true,
		blockBegin + "\n" + blockEnd: true, blockBegin + "\nx\n" + blockEnd + "\n" + blockBegin + "\n" + blockEnd: true,
	} {
		rules := ruleLines("CLAUDE.md", []byte(in+"\nNever use tabs\n"))
		if read := len(rules) > 0 && rules[len(rules)-1].text == "Never use tabs"; read != want {
			t.Errorf("%q: rule after the marker read = %v, want %v (%+v)", in, read, want, rules)
		}
	}
	if rules := ruleLines("CLAUDE.md", []byte(blockBegin+"\nInside\n"+blockEnd+"\n")); len(rules) != 0 {
		t.Errorf("a column-0 block's lines read as rules: %+v", rules)
	}
}
