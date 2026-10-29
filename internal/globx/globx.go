// Package globx validates claim globs and matches them against repo paths.
// A glob is slash-separated and relative to the repo root; "*" and "?" stay
// inside one segment, "**" as a whole segment spans any number of them.
package globx

import (
	"fmt"
	"path"
	"strings"
	"unicode"
)

// Validate refuses globs that could name a path outside the repo: absolute
// paths, home or drive prefixes, backslashes, ".." segments, control
// characters, and bad bracket syntax.
func Validate(g string) error {
	switch {
	case strings.TrimSpace(g) == "":
		return fmt.Errorf("glob is empty")
	case len(g) > 512:
		return fmt.Errorf("glob %q is longer than 512 bytes", g[:40]+"...")
	case strings.HasPrefix(g, "/"), strings.HasPrefix(g, "~"), strings.HasPrefix(g, "-"):
		return fmt.Errorf("glob %q must be relative to the repo root", g)
	case strings.Contains(g, `\`):
		return fmt.Errorf("glob %q must use / as the separator", g)
	case len(g) >= 2 && g[1] == ':':
		return fmt.Errorf("glob %q must not name a drive", g)
	}
	for _, r := range g {
		if unicode.IsControl(r) {
			return fmt.Errorf("glob %q contains a control character", g)
		}
	}
	for _, seg := range strings.Split(g, "/") {
		switch seg {
		case "":
			return fmt.Errorf("glob %q has an empty path segment", g)
		case ".", "..":
			return fmt.Errorf("glob %q must not contain . or .. segments", g)
		}
		if strings.Contains(seg, "**") && seg != "**" {
			return fmt.Errorf("glob %q: ** must be a whole path segment", g)
		}
		if seg != "**" {
			if _, err := path.Match(seg, ""); err != nil {
				return fmt.Errorf("glob %q: %v", g, err)
			}
		}
	}
	return nil
}

// Match reports whether the slash-separated relative path p matches g.
func Match(g, p string) bool {
	return match(strings.Split(g, "/"), strings.Split(p, "/"))
}

func match(gs, ps []string) bool {
	for len(gs) > 0 {
		if gs[0] == "**" {
			for i := 0; i <= len(ps); i++ {
				if match(gs[1:], ps[i:]) {
					return true
				}
			}
			return false
		}
		if len(ps) == 0 {
			return false
		}
		if ok, _ := path.Match(gs[0], ps[0]); !ok {
			return false
		}
		gs, ps = gs[1:], ps[1:]
	}
	return len(ps) == 0
}

func literal(g string) bool { return !strings.ContainsAny(g, "*?[") }

// Overlaps reports whether some path could match both globs. It is exact
// when either glob is a literal path, and conservative otherwise: two
// patterns overlap unless their literal leading directories diverge.
func Overlaps(a, b string) bool {
	switch {
	case a == b:
		return true
	case literal(a) && literal(b):
		return false
	case literal(a):
		return Match(b, a) || under(b, a)
	case literal(b):
		return Match(a, b) || under(a, b)
	}
	pa, pb := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if !literal(pa[i]) || !literal(pb[i]) {
			return true
		}
		if pa[i] != pb[i] {
			return false
		}
	}
	return true
}

// under reports whether a claim on the literal directory dir would hold a
// path g can match (a claim on "api" covers "api/**").
func under(g, dir string) bool {
	return Match(g, dir) || strings.HasPrefix(g, dir+"/")
}
