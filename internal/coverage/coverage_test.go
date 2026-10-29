package coverage

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLcov(t *testing.T) {
	wt := t.TempDir()
	abs := filepath.ToSlash(filepath.Join(wt, "src", "a.js"))
	p := write(t, wt, "lcov.info", "TN:\nSF:"+abs+"\nDA:1,1\nDA:2,0\nDA:3,0\nDA:5,2\nend_of_record\nSF:lib/b.js\nDA:1,0\nend_of_record\n")
	got, err := Parse("lcov", p, wt)
	if err != nil {
		t.Fatal(err)
	}
	a := got["src/a.js"]
	if a.Total != 4 || a.Covered != 2 || Ranges(a.Uncovered) != "2-3" {
		t.Fatalf("a: %+v (all %+v)", a, got)
	}
	if b := got["lib/b.js"]; b.Total != 1 || b.Covered != 0 || b.Rate() != 0 {
		t.Fatalf("b: %+v", b)
	}
	if _, err := Parse("lcov", write(t, wt, "bad.info", "DA:1,1\n"), wt); err == nil {
		t.Fatal("DA before SF accepted")
	}
}

func TestCobertura(t *testing.T) {
	wt := t.TempDir()
	p := write(t, wt, "cov.xml", `<?xml version="1.0"?><coverage><sources><source>`+wt+`</source></sources><packages><package><classes>
<class filename="app/x.py"><lines><line number="1" hits="3"/><line number="2" hits="0"/><line number="4" hits="0"/></lines></class>
</classes></package></packages></coverage>`)
	got, err := Parse("cobertura", p, wt)
	if err != nil {
		t.Fatal(err)
	}
	if x := got["app/x.py"]; x.Total != 3 || x.Covered != 1 || Ranges(x.Uncovered) != "2,4" {
		t.Fatalf("x: %+v (all %+v)", x, got)
	}
}

func TestGoCover(t *testing.T) {
	wt := t.TempDir()
	write(t, wt, "go.mod", "module example.com/app\n\ngo 1.22\n")
	p := write(t, wt, "cover.out", "mode: set\nexample.com/app/pkg/f.go:3.10,5.2 2 1\nexample.com/app/pkg/f.go:5.2,8.3 1 0\nexample.com/app/pkg/f.go:10.1,10.9 1 0\n")
	got, err := Parse("go-cover", p, wt)
	if err != nil {
		t.Fatal(err)
	}
	f := got["pkg/f.go"]
	if f.Total != 7 || f.Covered != 3 || Ranges(f.Uncovered) != "6-8,10" {
		t.Fatalf("f: %+v (all %+v)", f, got)
	}
}

func TestRanges(t *testing.T) {
	if r := Ranges([]int{1, 2, 3, 7, 9, 10}); r != "1-3,7,9-10" {
		t.Fatal(r)
	}
	got, err := ParseRanges("1-3,7")
	if err != nil || !reflect.DeepEqual(got, []int{1, 2, 3, 7}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParseRanges("5-2"); err == nil {
		t.Fatal("backwards range accepted")
	}
}
