// Package releaseguard checks GoReleaser's dist/ before a release is
// published (spec §13): every archive holds the shoalctl binary, LICENSE and
// README.md and nothing else, no path anywhere is a database file or under a
// reports/ directory, and checksums.txt covers every archive.
package releaseguard

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// allowed are the only files an archive may hold, by base name.
var allowed = map[string]bool{"shoalctl": true, "shoalctl.exe": true, "LICENSE": true, "README.md": true}

// dbFile matches a SQLite database and its companions: x.db, x.db-wal,
// x.db-shm, x.db-journal, x.db.bak-7.
var dbFile = regexp.MustCompile(`(?i)\.db([-.].*)?$`)

// Forbidden reports why a path must never ship, or "".
func Forbidden(p string) string {
	p = strings.TrimSuffix(filepath.ToSlash(p), "/")
	for _, seg := range strings.Split(p, "/") {
		if strings.EqualFold(seg, "reports") {
			return "a reports/ path"
		}
	}
	if dbFile.MatchString(path.Base(p)) {
		return "a .db file"
	}
	return ""
}

// Check inspects the archives and checksums.txt at the top of dist and
// returns every problem found, or an error when dist cannot be read.
func Check(dist string) ([]string, error) {
	entries, err := os.ReadDir(dist)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dist, err)
	}
	var problems []string
	sums := map[string]string{}
	archives := 0
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(dist, name)
		if why := Forbidden(name); why != "" && e.Type().IsRegular() {
			problems = append(problems, fmt.Sprintf("%s: %s in dist", name, why))
		}
		var list func(string) ([]string, error)
		switch {
		case strings.HasSuffix(name, ".tar.gz"):
			list = tarNames
		case strings.HasSuffix(name, ".zip"):
			list = zipNames
		default:
			continue
		}
		archives++
		names, err := list(full)
		if err != nil {
			return nil, err
		}
		problems = append(problems, archiveProblems(name, names)...)
		sum, err := fileSHA256(full)
		if err != nil {
			return nil, err
		}
		sums[name] = sum
	}
	if archives == 0 {
		return append(problems, "no .tar.gz or .zip archive in "+dist), nil
	}
	listed, err := readChecksums(filepath.Join(dist, "checksums.txt"))
	if err != nil {
		return append(problems, err.Error()), nil
	}
	for _, name := range sortedKeys(sums) {
		switch got, ok := listed[name]; {
		case !ok:
			problems = append(problems, name+": not in checksums.txt")
		case got != sums[name]:
			problems = append(problems, fmt.Sprintf("%s: checksums.txt says %s, the file is %s", name, got, sums[name]))
		}
	}
	return problems, nil
}

func archiveProblems(archive string, names []string) []string {
	var problems []string
	files := map[string]bool{}
	for _, n := range names {
		if why := Forbidden(n); why != "" {
			problems = append(problems, fmt.Sprintf("%s: %s is %s", archive, n, why))
			continue
		}
		if strings.HasSuffix(n, "/") {
			continue
		}
		base := path.Base(n)
		if !allowed[base] {
			problems = append(problems, fmt.Sprintf("%s: %s is not the binary, LICENSE or README.md", archive, n))
		}
		files[base] = true
	}
	if !files["shoalctl"] && !files["shoalctl.exe"] {
		problems = append(problems, archive+": no shoalctl binary")
	}
	return problems
}

func tarNames(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		name := h.Name
		if h.Typeflag == tar.TypeDir && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		out = append(out, name)
	}
}

func zipNames(p string) ([]string, error) {
	r, err := zip.OpenReader(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	defer r.Close()
	var out []string
	for _, f := range r.File {
		out = append(out, f.Name)
	}
	return out, nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", p, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readChecksums reads a sha256sum-style file: "<hex>  <name>" per line.
func readChecksums(p string) (map[string]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("checksums.txt: %w", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 {
			out[fields[1]] = fields[0]
		}
	}
	return out, sc.Err()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
