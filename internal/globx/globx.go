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

// Overlaps reports whether some path could match both globs. A literal glob
// also claims every path below it (a claim on "api" covers "api/x.go"); a
// wildcard glob claims exactly the paths it matches. Wildcard against
// wildcard is decided exactly, by walking both patterns together.
func Overlaps(a, b string) bool {
	switch {
	case a == b:
		return true
	case literal(a) && literal(b):
		return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
	case literal(a):
		return Match(b, a) || under(b, a)
	case literal(b):
		return Match(a, b) || under(a, b)
	}
	sa, oka := parseGlob(a)
	sb, okb := parseGlob(b)
	if !oka || !okb {
		return true // unparseable: fail closed
	}
	return pathsMeet(sa, sb)
}

// segment is one parsed path segment of a glob; star is a whole "**".
type segment struct {
	star   bool
	tokens []token
}

type tokenKind int

const (
	tokLit   tokenKind = iota
	tokAny             // ?
	tokStar            // *
	tokClass           // [...]
)

type token struct {
	kind   tokenKind
	r      rune
	neg    bool
	ranges [][2]rune
}

func parseGlob(g string) ([]segment, bool) {
	var out []segment
	for _, seg := range strings.Split(g, "/") {
		if seg == "**" {
			out = append(out, segment{star: true})
			continue
		}
		toks, ok := parseSegment(seg)
		if !ok {
			return nil, false
		}
		out = append(out, segment{tokens: toks})
	}
	return out, true
}

func parseSegment(seg string) ([]token, bool) {
	rs := []rune(seg)
	var out []token
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '*':
			out = append(out, token{kind: tokStar})
		case '?':
			out = append(out, token{kind: tokAny})
		case '\\':
			return nil, false // Validate refuses backslashes
		case '[':
			t := token{kind: tokClass}
			i++
			if i < len(rs) && rs[i] == '^' {
				t.neg = true
				i++
			}
			for ; i < len(rs) && rs[i] != ']'; i++ {
				lo, hi := rs[i], rs[i]
				if i+2 < len(rs) && rs[i+1] == '-' && rs[i+2] != ']' {
					hi = rs[i+2]
					i += 2
				}
				t.ranges = append(t.ranges, [2]rune{lo, hi})
			}
			if i >= len(rs) || len(t.ranges) == 0 {
				return nil, false
			}
			out = append(out, t)
		default:
			out = append(out, token{kind: tokLit, r: rs[i]})
		}
	}
	return out, true
}

func (t token) has(r rune) bool {
	switch t.kind {
	case tokLit:
		return r == t.r
	case tokClass:
		in := false
		for _, rg := range t.ranges {
			if rg[0] <= r && r <= rg[1] {
				in = true
				break
			}
		}
		return in != t.neg
	}
	return true // ? and * take any rune but /, which never occurs in a segment
}

// witnesses are the runes where the membership of a or b can change: one of
// them lies in every interval on which both are constant, so if a and b
// share any rune they share one of these.
func witnesses(a, b token) []rune {
	out := []rune{1, '/' + 1, 'a', '0', 0xE9}
	for _, t := range []token{a, b} {
		if t.kind == tokLit {
			out = append(out, t.r)
		}
		for _, rg := range t.ranges {
			out = append(out, rg[0], rg[1]+1)
		}
	}
	return out
}

// sameRune reports whether one rune other than / satisfies both tokens.
func sameRune(a, b token) bool {
	for _, r := range witnesses(a, b) {
		if r != '/' && r > 0 && r <= 0x10FFFF && a.has(r) && b.has(r) {
			return true
		}
	}
	return false
}

// segmentsMeet reports whether one non-empty segment matches both token
// lists: a search over pairs of positions, where * may stay put.
func segmentsMeet(a, b []token) bool {
	type state struct {
		i, j     int
		consumed bool
	}
	seen := map[state]bool{}
	stack := []state{{}}
	push := func(s state) {
		if !seen[s] {
			seen[s] = true
			stack = append(stack, s)
		}
	}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s.i == len(a) && s.j == len(b) && s.consumed {
			return true
		}
		if s.i < len(a) && a[s.i].kind == tokStar {
			push(state{s.i + 1, s.j, s.consumed})
		}
		if s.j < len(b) && b[s.j].kind == tokStar {
			push(state{s.i, s.j + 1, s.consumed})
		}
		if s.i < len(a) && s.j < len(b) && sameRune(a[s.i], b[s.j]) {
			ni, nj := s.i+1, s.j+1
			if a[s.i].kind == tokStar {
				ni = s.i
			}
			if b[s.j].kind == tokStar {
				nj = s.j
			}
			push(state{ni, nj, true})
		}
	}
	return false
}

var anySegment = []token{{kind: tokStar}}

// pathsMeet reports whether one path matches both parsed globs; ** spans
// any number of segments, including none.
func pathsMeet(a, b []segment) bool {
	type state struct {
		i, j     int
		consumed bool
	}
	seen := map[state]bool{}
	stack := []state{{}}
	push := func(s state) {
		if !seen[s] {
			seen[s] = true
			stack = append(stack, s)
		}
	}
	tokensOf := func(s segment) []token {
		if s.star {
			return anySegment
		}
		return s.tokens
	}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s.i == len(a) && s.j == len(b) && s.consumed {
			return true
		}
		if s.i < len(a) && a[s.i].star {
			push(state{s.i + 1, s.j, s.consumed})
		}
		if s.j < len(b) && b[s.j].star {
			push(state{s.i, s.j + 1, s.consumed})
		}
		if s.i < len(a) && s.j < len(b) && segmentsMeet(tokensOf(a[s.i]), tokensOf(b[s.j])) {
			ni, nj := s.i+1, s.j+1
			if a[s.i].star {
				ni = s.i
			}
			if b[s.j].star {
				nj = s.j
			}
			push(state{ni, nj, true})
		}
	}
	return false
}

// under reports whether a claim on the literal directory dir would hold a
// path g can match (a claim on "api" covers "api/**").
func under(g, dir string) bool {
	return Match(g, dir) || strings.HasPrefix(g, dir+"/")
}
