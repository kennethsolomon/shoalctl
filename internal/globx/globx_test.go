package globx

import "testing"

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
