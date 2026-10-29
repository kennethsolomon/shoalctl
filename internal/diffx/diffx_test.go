package diffx

import (
	"reflect"
	"testing"
)

func TestParseNumstatZ(t *testing.T) {
	out := "3\t1\tapp/a.go\x00-\t-\timg.png\x002\t0\t\x00old.go\x00new.go\x00"
	got, err := ParseNumstatZ(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []Stat{{Path: "app/a.go", Added: 3, Removed: 1}, {Path: "img.png", Binary: true}, {Path: "new.go", Added: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if _, err := ParseNumstatZ("x\ty\tz\x00"); err == nil {
		t.Fatal("bad counts accepted")
	}
}

func TestChangedLines(t *testing.T) {
	out := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -3,0 +4,2 @@ func x
+a
+b
@@ -10 +12 @@
-x
+y
@@ -20,3 +21,0 @@
-gone
diff --git a/old.go b/old.go
--- a/old.go
+++ /dev/null
@@ -1,2 +0,0 @@
diff --git a/sp b/"sp\303\251c.go"
--- /dev/null
+++ "b/sp\303\251c.go"
@@ -0,0 +1 @@
+z
`
	got, err := ChangedLines(out)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]int{"a.go": {4, 5, 12}, "spéc.go": {1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestIsTestFile(t *testing.T) {
	for p, want := range map[string]bool{
		"pkg/a_test.go": true, "tests/Feature/UserTest.php": true, "src/x.spec.ts": true, "app/__tests__/y.js": true,
		"test_api.py": true, "spec/models/user_spec.rb": true, "src/main/java/FooTest.java": true,
		"pkg/a.go": false, "app/Models/User.php": false, "src/latest.go": false, "contest/x.go": false, "README.md": false,
	} {
		if got := IsTestFile(p); got != want {
			t.Errorf("IsTestFile(%q) = %v, want %v", p, got, want)
		}
	}
}
