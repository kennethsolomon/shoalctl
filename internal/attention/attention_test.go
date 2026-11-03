package attention

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectFixtures(t *testing.T) {
	cases := []struct {
		file   string
		want   bool
		reason string
	}{
		{"trust.txt", true, "dialog: Do you trust the files in this folder?"},
		{"bash-permission.txt", true, "dialog: Do you want to proceed?"},
		{"edit-permission.txt", true, "dialog: Do you want to make this edit to User.php?"},
		{"yes-no.txt", true, "prompt: Ok to proceed? (y/n)"},
		{"idle.txt", false, ""},
		{"working.txt", false, ""},
		{"menu-in-scrollback.txt", false, ""},
		{"empty.txt", false, ""},
	}
	for _, c := range cases {
		data, err := os.ReadFile(filepath.Join("testdata", c.file))
		if err != nil {
			t.Fatal(err)
		}
		reason, ok := Detect(string(data))
		if ok != c.want || reason != c.reason {
			t.Errorf("%s: Detect = %q, %v; want %q, %v", c.file, reason, ok, c.reason, c.want)
		}
	}
}

func TestDetectNeedsTwoOptions(t *testing.T) {
	if r, ok := Detect("some output\n❯ 1. only one\n"); ok {
		t.Fatalf("a lone option was a dialog: %q", r)
	}
	if r, ok := Detect("Pick one\n❯ 1. a\n  2. b\n"); !ok || r != "dialog: a menu is waiting for a choice" {
		t.Fatalf("menu without a question: %q %v", r, ok)
	}
	long := "Do you want to proceed with a very long question that goes on and on and on and on and on and on and on and on?"
	if r, _ := Detect(long + "\n❯ 1. Yes\n  2. No\n"); len([]rune(r)) > len("dialog: ")+100 {
		t.Fatalf("reason not clipped: %q", r)
	}
}
