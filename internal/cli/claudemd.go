package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The managed block shoal owns in a non-stealth project's CLAUDE.md (§5.1).
const (
	blockBegin = "<!-- shoal:begin -->"
	blockEnd   = "<!-- shoal:end -->"
)

// contextImport is the CLAUDE.md import line of context.md: a home-relative
// path when context.md is under the home directory, as §5.1 shows it.
func contextImport(contextPath string) string {
	p := filepath.ToSlash(contextPath)
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, contextPath); err == nil && rel != "." && !strings.HasPrefix(filepath.ToSlash(rel), "../") && !filepath.IsAbs(rel) {
			p = "~/" + filepath.ToSlash(rel)
		}
	}
	return "@" + p
}

// splitLines splits data into lines that keep their line endings.
func splitLines(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return append(out, data)
		}
		out = append(out, data[:i+1])
		data = data[i+1:]
	}
	return out
}

func lineText(l []byte) string { return strings.TrimSpace(string(l)) }

// withManagedBlock returns data with exactly one managed block holding
// importLine, and whether that changed anything. Bytes outside the markers
// are kept as they are; a missing block is appended with the file's own line
// ending (CRLF when the file uses it) and the file keeps ending with a
// newline, or not, as it did. A block whose markers are duplicated, missing
// their partner or out of order is refused: shoal does not guess which
// lines it owns.
func withManagedBlock(name string, data []byte, importLine string) ([]byte, bool, error) {
	lines := splitLines(data)
	var begins, ends []int
	for i, l := range lines {
		switch lineText(l) {
		case blockBegin:
			begins = append(begins, i)
		case blockEnd:
			ends = append(ends, i)
		}
	}
	lineNums := func(idx []int) string {
		var s []string
		for _, i := range idx {
			s = append(s, fmt.Sprint(i+1))
		}
		return strings.Join(s, ", ")
	}
	switch {
	case len(begins) > 1 || len(ends) > 1:
		return nil, false, fail(ExitConfig, "%s has more than one shoal block (%s on lines %s, %s on lines %s); keep exactly one and rerun shoalctl init",
			name, blockBegin, lineNums(begins), blockEnd, lineNums(ends))
	case len(begins) != len(ends):
		return nil, false, fail(ExitConfig, "%s has an unpaired shoal marker (%s on lines [%s], %s on lines [%s]); fix or remove it and rerun shoalctl init",
			name, blockBegin, lineNums(begins), blockEnd, lineNums(ends))
	case len(begins) == 1 && ends[0] < begins[0]:
		return nil, false, fail(ExitConfig, "%s has %s on line %d before %s on line %d; fix the markers and rerun shoalctl init",
			name, blockEnd, ends[0]+1, blockBegin, begins[0]+1)
	}
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	if len(begins) == 1 {
		b, e := begins[0], ends[0]
		if e == b+2 && lineText(lines[b+1]) == importLine {
			return data, false, nil
		}
		if strings.HasSuffix(string(lines[b]), "\r\n") {
			eol = "\r\n"
		}
		var out []byte
		for _, l := range lines[:b+1] {
			out = append(out, l...)
		}
		out = append(out, importLine+eol...)
		for _, l := range lines[e:] {
			out = append(out, l...)
		}
		return out, true, nil
	}
	out := append([]byte(nil), data...)
	trailing := len(data) == 0 || bytes.HasSuffix(data, []byte("\n"))
	if !trailing {
		out = append(out, eol...)
	}
	out = append(out, blockBegin+eol+importLine+eol+blockEnd...)
	if trailing {
		out = append(out, eol...)
	}
	return out, true, nil
}

// installManagedBlock writes the managed block into root's CLAUDE.md,
// creating the file when it is missing. It reports whether the file changed.
func installManagedBlock(root, importLine string) (bool, error) {
	path := filepath.Join(root, "CLAUDE.md")
	perm := fs.FileMode(0o644)
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return false, fmt.Errorf("read %s: %w", path, err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return false, fail(ExitConfig, "%s is a symlink; shoal does not write through it. Replace it with a file or set [install].stealth = true", path)
	case !fi.Mode().IsRegular():
		return false, fail(ExitConfig, "%s is not a regular file", path)
	default:
		perm = fi.Mode().Perm()
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	out, changed, err := withManagedBlock("CLAUDE.md", data, importLine)
	if err != nil || !changed {
		return false, err
	}
	return true, writeFileAtomic(path, out, perm)
}

// importsContext reports whether dir's CLAUDE.md holds shoal's managed
// block, so Claude Code loads context.md itself.
func importsContext(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		return false
	}
	for _, l := range splitLines(data) {
		if lineText(l) == blockBegin {
			return true
		}
	}
	return false
}
