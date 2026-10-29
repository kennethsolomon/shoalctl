// Package coverage reads per-file line coverage from lcov, Cobertura XML and
// Go cover profiles (spec §8.5 step 5, §8.8).
package coverage

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// File is the line coverage of one source file. Path is relative to the
// worktree, with forward slashes, when the report names a file inside it.
type File struct {
	Path      string
	Total     int
	Covered   int
	Uncovered []int // line numbers no test executed, ascending
}

// lines maps a line number to whether any test executed it.
type lines map[int]bool

func (l lines) mark(n int, hit bool) { l[n] = l[n] || hit }

// Parse reads the report at path in format (lcov, cobertura, go-cover).
// worktree turns absolute paths into worktree-relative ones; for go-cover,
// the module path from worktree/go.mod is stripped from import paths.
func Parse(format, path, worktree string) (map[string]File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s coverage report: %w", format, err)
	}
	defer f.Close()
	var byFile map[string]lines
	switch format {
	case "lcov":
		byFile, err = parseLcov(f)
	case "cobertura":
		byFile, err = parseCobertura(f)
	case "go-cover":
		byFile, err = parseGoCover(f, modulePath(worktree))
	default:
		return nil, fmt.Errorf("unknown coverage format %q", format)
	}
	if err != nil {
		return nil, fmt.Errorf("parse %s coverage report %s: %w", format, path, err)
	}
	out := make(map[string]File, len(byFile))
	for name, ls := range byFile {
		rel := relPath(worktree, name)
		cf := out[rel]
		cf.Path = rel
		merged := lines{}
		for _, n := range cf.Uncovered {
			merged[n] = false
		}
		for n, hit := range ls {
			merged.mark(n, hit)
		}
		cf.Total, cf.Covered, cf.Uncovered = 0, 0, nil
		for n, hit := range merged {
			cf.Total++
			if hit {
				cf.Covered++
			} else {
				cf.Uncovered = append(cf.Uncovered, n)
			}
		}
		sort.Ints(cf.Uncovered)
		out[rel] = cf
	}
	return out, nil
}

func relPath(worktree, name string) string {
	name = filepath.FromSlash(name)
	if filepath.IsAbs(name) && worktree != "" {
		if rel, err := filepath.Rel(worktree, name); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			name = rel
		}
	}
	return filepath.ToSlash(filepath.Clean(name))
}

func parseLcov(r io.Reader) (map[string]lines, error) {
	out := map[string]lines{}
	var cur lines
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "SF:"):
			name := strings.TrimPrefix(line, "SF:")
			if out[name] == nil {
				out[name] = lines{}
			}
			cur = out[name]
		case strings.HasPrefix(line, "DA:"):
			if cur == nil {
				return nil, fmt.Errorf("DA record before any SF record")
			}
			parts := strings.Split(strings.TrimPrefix(line, "DA:"), ",")
			if len(parts) < 2 {
				return nil, fmt.Errorf("bad DA record %q", line)
			}
			n, err1 := strconv.Atoi(parts[0])
			hits, err2 := strconv.ParseFloat(parts[1], 64)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("bad DA record %q", line)
			}
			cur.mark(n, hits > 0)
		case line == "end_of_record":
			cur = nil
		}
	}
	return out, sc.Err()
}

type cobertura struct {
	Sources []string `xml:"sources>source"`
	Classes []struct {
		Filename string `xml:"filename,attr"`
		Lines    []struct {
			Number int     `xml:"number,attr"`
			Hits   float64 `xml:"hits,attr"`
		} `xml:"lines>line"`
	} `xml:"packages>package>classes>class"`
}

func parseCobertura(r io.Reader) (map[string]lines, error) {
	var doc cobertura
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, err
	}
	out := map[string]lines{}
	for _, c := range doc.Classes {
		name := c.Filename
		if !filepath.IsAbs(filepath.FromSlash(name)) && len(doc.Sources) == 1 && filepath.IsAbs(filepath.FromSlash(strings.TrimSpace(doc.Sources[0]))) {
			name = filepath.Join(strings.TrimSpace(doc.Sources[0]), name)
		}
		if out[name] == nil {
			out[name] = lines{}
		}
		for _, l := range c.Lines {
			out[name].mark(l.Number, l.Hits > 0)
		}
	}
	return out, nil
}

// parseGoCover reads a go test -coverprofile file: "mode: set" then
// "import/path/file.go:startLine.startCol,endLine.endCol statements count".
func parseGoCover(r io.Reader, module string) (map[string]lines, error) {
	out := map[string]lines{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			return nil, fmt.Errorf("bad profile line %q", line)
		}
		name, rest := line[:colon], strings.Fields(line[colon+1:])
		if len(rest) != 3 {
			return nil, fmt.Errorf("bad profile line %q", line)
		}
		span := strings.Split(rest[0], ",")
		if len(span) != 2 {
			return nil, fmt.Errorf("bad profile block %q", line)
		}
		start, err1 := strconv.Atoi(strings.Split(span[0], ".")[0])
		end, err2 := strconv.Atoi(strings.Split(span[1], ".")[0])
		count, err3 := strconv.Atoi(rest[2])
		if err1 != nil || err2 != nil || err3 != nil || end < start {
			return nil, fmt.Errorf("bad profile line %q", line)
		}
		if module != "" {
			if rel, ok := strings.CutPrefix(name, module+"/"); ok {
				name = rel
			}
		}
		if out[name] == nil {
			out[name] = lines{}
		}
		for n := start; n <= end; n++ {
			out[name].mark(n, count > 0)
		}
	}
	return out, sc.Err()
}

func modulePath(worktree string) string {
	if worktree == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(worktree, "go.mod"))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(data), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
			return strings.Trim(strings.TrimSpace(m), `"`)
		}
	}
	return ""
}

// Ranges compacts ascending line numbers into "1-3,7".
func Ranges(ns []int) string {
	var parts []string
	for i := 0; i < len(ns); {
		j := i
		for j+1 < len(ns) && ns[j+1] == ns[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(ns[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ns[i], ns[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// ParseRanges expands "1-3,7" into line numbers.
func ParseRanges(s string) ([]int, error) {
	var out []int
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	for _, part := range strings.Split(s, ",") {
		a, b, isRange := strings.Cut(part, "-")
		lo, err := strconv.Atoi(a)
		if err != nil {
			return nil, fmt.Errorf("bad line range %q", part)
		}
		hi := lo
		if isRange {
			if hi, err = strconv.Atoi(b); err != nil || hi < lo {
				return nil, fmt.Errorf("bad line range %q", part)
			}
		}
		for n := lo; n <= hi; n++ {
			out = append(out, n)
		}
	}
	return out, nil
}

// Rate is the covered share of a file's lines; a file with no lines is 1.
func (f File) Rate() float64 {
	if f.Total == 0 {
		return 1
	}
	return float64(f.Covered) / float64(f.Total)
}
