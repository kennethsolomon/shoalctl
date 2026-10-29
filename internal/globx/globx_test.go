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
		{"src/*.go", "src/*.ts", true}, // conservative
		{"src/a/*.go", "src/b/*.go", false},
		{"**/*.go", "ui/**", true},
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
