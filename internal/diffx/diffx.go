// Package diffx parses git diff output: per-file line counts (--numstat -z)
// and the new-side line ranges of each hunk (-U0), and tells test files from
// production files (spec §6.3, §8.8).
package diffx

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Stat is one file of git diff --numstat. Binary files have Binary set and
// zero counts.
type Stat struct {
	Path    string
	Added   int
	Removed int
	Binary  bool
}

// ParseNumstatZ parses git diff --numstat -z: "added\tremoved\tpath\0", or
// for a rename "added\tremoved\t\0from\0to\0".
func ParseNumstatZ(out string) ([]Stat, error) {
	var stats []Stat
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		rec := fields[i]
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("parse git diff --numstat -z: bad record %q", rec)
		}
		s := Stat{Path: parts[2]}
		if parts[0] == "-" && parts[1] == "-" {
			s.Binary = true
		} else {
			a, err1 := strconv.Atoi(parts[0])
			r, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("parse git diff --numstat -z: bad counts in %q", rec)
			}
			s.Added, s.Removed = a, r
		}
		if s.Path == "" { // rename: the next two fields are from and to
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("parse git diff --numstat -z: truncated rename after %q", rec)
			}
			s.Path = fields[i+2]
			i += 2
		}
		stats = append(stats, s)
	}
	return stats, nil
}

var hunkRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ChangedLines parses git diff -U0 --no-color output into the new-side line
// numbers each file gained or changed. Deleted files and pure deletions add
// nothing.
func ChangedLines(out string) (map[string][]int, error) {
	res := map[string][]int{}
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			name := strings.TrimPrefix(line, "+++ ")
			cur = ""
			if name != "/dev/null" {
				cur = strings.TrimPrefix(unquote(name), "b/")
			}
		case strings.HasPrefix(line, "@@ "):
			if cur == "" {
				continue
			}
			m := hunkRE.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("parse git diff -U0: bad hunk header %q", line)
			}
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			for n := start; n < start+count; n++ {
				res[cur] = append(res[cur], n)
			}
		}
	}
	return res, nil
}

// unquote undoes git's C-style quoting of unusual paths ("b/sp\303\251c").
func unquote(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}

var testName = regexp.MustCompile(`(?i)(_test\.go$|_test\.py$|^test_.*\.py$|\.(test|spec)\.[a-z0-9]+$|_spec\.rb$|_test\.rb$|Tests?\.(php|java|kt|cs|swift)$|_test\.(c|cc|cpp|rs|exs?)$)`)

var testDirs = map[string]bool{"test": true, "tests": true, "__tests__": true, "spec": true, "specs": true, "testdata": true, "__snapshots__": true}

// IsTestFile tells whether a repo-relative path is a test file by name or
// directory convention.
func IsTestFile(p string) bool {
	p = strings.ReplaceAll(p, `\`, "/")
	if testName.MatchString(path.Base(p)) {
		return true
	}
	for _, dir := range strings.Split(path.Dir(p), "/") {
		if testDirs[strings.ToLower(dir)] {
			return true
		}
	}
	return false
}
