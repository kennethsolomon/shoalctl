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
	in := "top\r\n  " + blockBegin + "\r\n@elsewhere\r\nextra\r\n" + blockEnd + "  \r\nbottom"
	got, changed, err := withManagedBlock("CLAUDE.md", []byte(in), testImport)
	want := "top\r\n  " + blockBegin + "\r\n" + testImport + "\r\n" + blockEnd + "  \r\nbottom"
	if err != nil || !changed || string(got) != want {
		t.Fatalf("%q %v %v", got, changed, err)
	}
}

func TestWithManagedBlockRefusesMalformed(t *testing.T) {
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := contextImport(filepath.Join(home, ".shoal", "projects", "abc", "context.md")); got != testImport {
		t.Errorf("under home: %q", got)
	}
	other := filepath.Join(t.TempDir(), "context.md")
	if got := contextImport(other); got != "@"+filepath.ToSlash(other) {
		t.Errorf("outside home: %q", got)
	}
}
