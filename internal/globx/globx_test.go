package globx

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, g := range []string{"api/**", "src/*.go", "README.md", "a/[bc]/d", "**/*_test.go"} {
		if err := Validate(g); err != nil {
			t.Errorf("Validate(%q): %v", g, err)
		}
	}
	for _, g := range []string{"", "/etc/passwd", "~/x", "../x", "a/../../b", "a/./b", `a\b`, "C:/x", "a//b", "a/b**", "a/[", "x\ny", "-rf", "a/.."} {
		if err := Validate(g); err == nil {
			t.Errorf("Validate(%q) accepted", g)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		g, p string
		want bool
	}{
		{"api/**", "api/x/y.go", true},
		{"api/**", "api", true},
		{"api/**", "apix/y", false},
		{"**/*_test.go", "a/b/c_test.go", true},
		{"**/*_test.go", "c_test.go", true},
		{"src/*.go", "src/a/b.go", false},
		{"src/*.go", "src/b.go", true},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
	}
	for _, c := range cases {
		if got := Match(c.g, c.p); got != c.want {
			t.Errorf("Match(%q, %q) = %v", c.g, c.p, got)
		}
	}
}

func TestOverlaps(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"api/**", "api/**", true},
		{"api/**", "ui/**", false},
		{"api/**", "api/x.go", true},
		{"api/x.go", "api/y.go", false},
		{"api", "api/**", true},
		{"src/*.go", "src/*.ts", false}, // exact: no file ends in both
		{"src/a/*.go", "src/b/*.go", false},
		{"**/*.go", "ui/**", true},
		{"api", "api/x.go", true}, // a literal directory claim covers its subtree
		{"api/x.go", "api/x.go/y", true},
		{"api", "apix", false},
		{"src/*.go", "src/a*", true},
		{"src/a*.go", "src/b*", false},
		{"src/*_test.go", "src/*.go", true},
		{"src/?.go", "src/ab.go", false},
		{"src/?.go", "src/*b.go", true},
		{"src/[ab].go", "src/[cd].go", false},
		{"src/[a-c].go", "src/[c-e].go", true},
		{"src/[^a].go", "src/a.go", false},
		{"src/[^a].go", "src/[ab].go", true},
		{"src/[^a-z].go", "src/[a-z].go", false},
		{"src/*", "src/a/b.go", false}, // * stays inside one segment
		{"src/*/x.go", "src/**", true},
		{"a/**/z", "a/b/**", true},
		{"a/**/z.go", "b/**", false},
		{"**/z.go", "**/y.go", false},
		{"**/*.go", "**/*.ts", false},
		{"**/*.go", "**/a.*", true},
		{"a/*/c", "a/b/*", true},
		{"a/*/c", "a/b/d", false},
		{"*", "**", true},
		{"x/**", "**/y", true},
	}
	for _, c := range cases {
		if got := Overlaps(c.a, c.b); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v", c.a, c.b, got)
		}
		if got := Overlaps(c.b, c.a); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v (swapped)", c.b, c.a, got)
		}
	}
}

// TestOverlapsNeverMissesASharedPath checks Overlaps against brute force: a
// path that matches both globs must make them overlap, so the exact
// wildcard check can only ever be as strict as the old conservative one.
func TestOverlapsNeverMissesASharedPath(t *testing.T) {
	globs := []string{"**", "*", "a", "a/**", "**/a", "*/b", "a/*", "a/*/c.go", "**/*.go", "**/*.ts", "a*", "*b", "?", "??",
		"[ab]", "[^a]", "[a-b]*.go", "a/**/c.go", "**/c.go", "*.go", "a/b", "b/**", "*/*", "**/b/**", "a?.go", "*_t.go", "[^c]*.go"}
	segs := []string{"a", "b", "c", "ab", "ba", "c.go", "a.go", "ab.go", "b.ts", "x_t.go"}
	var paths []string
	for _, s1 := range segs {
		paths = append(paths, s1)
		for _, s2 := range segs {
			paths = append(paths, s1+"/"+s2)
			for _, s3 := range segs {
				paths = append(paths, s1+"/"+s2+"/"+s3)
			}
		}
	}
	for _, a := range globs {
		for _, b := range globs {
			if Overlaps(a, b) != Overlaps(b, a) {
				t.Errorf("Overlaps(%q, %q) is not symmetric", a, b)
			}
			if Overlaps(a, b) {
				continue
			}
			for _, p := range paths {
				if Match(a, p) && Match(b, p) {
					t.Errorf("Overlaps(%q, %q) = false, but %q matches both", a, b, p)
					break
				}
			}
		}
	}
}

// TestLiteralDirectoryMeetsWildcardsReachingIntoIt: a literal claim covers
// its subtree, so a wildcard that can match a path below it overlaps it.
func TestLiteralDirectoryMeetsWildcardsReachingIntoIt(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"src/pkg", "**/x.go", true},
		{"src/pkg", "src/*/x.go", true},
		{"internal/store", "**/*_test.go", true},
		{"src/pkg", "src/*", true},
		{"src/pkg", "**", true},
		{"src/pkg", "src/pk?/**", true},
		{"src/pkg", "src/*.go", false},
		{"src/pkg", "lib/**/x.go", false},
		{"src/pkg", "src/*/y/*.go", true},
		{"src/pkg", "src/[^p]*/x.go", false},
	}
	for _, c := range cases {
		if got := Overlaps(c.a, c.b); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := Overlaps(c.b, c.a); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v, want %v (swapped)", c.b, c.a, got, c.want)
		}
	}
}

// covers is the claim semantics: a literal covers itself and its subtree,
// a wildcard exactly the paths it matches.
func covers(g, p string) bool {
	if (reading{text: g}).literal() {
		return p == g || strings.HasPrefix(p, g+"/")
	}
	return Match(g, p)
}

// TestOverlapsNeverMissesACoveredPath is the brute-force check under claim
// semantics, where a literal also covers everything below it.
func TestOverlapsNeverMissesACoveredPath(t *testing.T) {
	globs := []string{"**", "*", "a", "a/b", "b", "a/**", "**/a", "*/b", "a/*", "a/*/c.go", "**/*.go", "**/c.go", "a/*/*",
		"*/*/c.go", "[ab]/*", "?/b/**", "a/b/c.go", "b/*.go"}
	segs := []string{"a", "b", "c", "c.go", "a.go"}
	var paths []string
	for _, s1 := range segs {
		paths = append(paths, s1)
		for _, s2 := range segs {
			paths = append(paths, s1+"/"+s2)
			for _, s3 := range segs {
				paths = append(paths, s1+"/"+s2+"/"+s3)
			}
		}
	}
	for _, a := range globs {
		for _, b := range globs {
			if Overlaps(a, b) {
				continue
			}
			for _, p := range paths {
				if covers(a, p) && covers(b, p) {
					t.Errorf("Overlaps(%q, %q) = false, but both cover %q", a, b, p)
					break
				}
			}
		}
	}
}

// TestBracketedRouteDirectories: Next.js and SvelteKit name directories with
// brackets ("app/[slug]"). A glob with brackets is read both as a pattern
// and as an exact name (brackets literal, a literal name covering its
// subtree); it overlaps when either reading does, so no overlap is lost.
func TestBracketedRouteDirectories(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"app/[slug]", "app/[slug]/page.tsx", true},
		{"app/[slug]", "**/page.tsx", true},
		{"src/routes/[id]", "src/routes/[id]/+page.svelte", true},
		{"src/routes/[id]", "**/+page.svelte", true},
		{"app/[...slug]", "app/[...slug]/page.tsx", true},
		{"app/[...slug]", "**/page.tsx", true},
		{"app/[[...slug]]", "app/[[...slug]]/page.tsx", true},
		{"app/[[...slug]]", "**/page.tsx", true},
		{"app/(shop)/[slug]", "app/(shop)/[slug]/page.tsx", true},
		{"app/(shop)/[slug]", "**/page.tsx", true},
		{"app/[slug]", "app/[slug]/**", true},
		{"app/[slug]", "app/*/page.tsx", true},
		{"app/[slug]/*.tsx", "app/[slug]/page.tsx", true},
		{"app/[slug]", "app/s", true}, // the pattern reading still holds
		{"app/[slug]", "app/[id]/page.tsx", false},
		{"app/[slug]", "lib/**", false},
		{"app/[slug]", "app/x", false},
		{"app/[slug]", "app/[slug]x/page.tsx", false},
	}
	for _, c := range cases {
		if got := Overlaps(c.a, c.b); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := Overlaps(c.b, c.a); got != c.want {
			t.Errorf("Overlaps(%q, %q) = %v, want %v (swapped)", c.b, c.a, got, c.want)
		}
	}
	for _, g := range []string{"app/[slug]", "app/[...slug]", "app/[[...slug]]", "app/(shop)/[slug]", "src/routes/[id]/+page.svelte",
		"app/[slug]/**"} {
		if err := Validate(g); err != nil {
			t.Errorf("Validate(%q): %v", g, err)
		}
	}
}

// exactName reads g with brackets as literal characters, as a path.Match
// pattern; isLit reports whether that reading has no wildcard left.
func exactName(g string) (pattern string, isLit bool) {
	return strings.NewReplacer(`[`, `\[`, `]`, `\]`).Replace(g), !strings.ContainsAny(g, "*?")
}

// coversEither is the claim semantics for globs with brackets: the pattern
// reading, or the exact-name reading (a literal name covers its subtree).
func coversEither(g, p string) bool {
	if covers(g, p) {
		return true
	}
	if !strings.Contains(g, "[") {
		return false
	}
	pat, isLit := exactName(g)
	if isLit {
		return p == g || strings.HasPrefix(p, g+"/")
	}
	return Match(pat, p)
}

// TestOverlapsNeverMissesABracketedPath is the brute-force check over both
// readings of bracketed globs, against paths whose names contain brackets.
func TestOverlapsNeverMissesABracketedPath(t *testing.T) {
	globs := []string{"**", "*", "a", "[a]", "[a]/**", "[a]/b", "**/b", "*/b", "[ab]", "[ab]/*", "a/[b]", "a/[b]/c", "a/[[b]]",
		"[a]/*", "**/[b]", "[^a]", "*]", "[a]*", "a/*", "a/**", "a/[b]/**", "[a]/[b]"}
	segs := []string{"a", "b", "c", "[a]", "[b]", "[[b]]", "[ab]", "ab"}
	var paths []string
	for _, s1 := range segs {
		paths = append(paths, s1)
		for _, s2 := range segs {
			paths = append(paths, s1+"/"+s2)
			for _, s3 := range segs {
				paths = append(paths, s1+"/"+s2+"/"+s3)
			}
		}
	}
	for _, a := range globs {
		for _, b := range globs {
			if Overlaps(a, b) != Overlaps(b, a) {
				t.Errorf("Overlaps(%q, %q) is not symmetric", a, b)
			}
			if Overlaps(a, b) {
				continue
			}
			for _, p := range paths {
				if coversEither(a, p) && coversEither(b, p) {
					t.Errorf("Overlaps(%q, %q) = false, but both cover %q", a, b, p)
					break
				}
			}
		}
	}
}
