package gitx

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

func TestCanonical(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://github.com/acme/app.git", "github.com/acme/app"},
		{"git@github.com:acme/app", "github.com/acme/app"},
		{"git@github.com:acme/app.git", "github.com/acme/app"},
		{"https://user:token@GitHub.com/acme/app/", "github.com/acme/app"},
		{"ssh://git@Host.example:2222/acme/app.git", "host.example/acme/app"},
		{"ssh://git@[::1]:22/acme/app", "[::1]/acme/app"},
		{"GitHub.com:acme/App.git", "github.com/acme/App"},
		{"file:///srv/git/app.git", "/srv/git/app"},
		{"/srv/git/app.git", "/srv/git/app"},
		{"../app.git", "../app"},
		{`C:\repos\app.git`, `C:\repos\app`},
		{"  https://github.com/acme/app.git  ", "github.com/acme/app"},
	}
	for _, tt := range tests {
		if got := Canonical(tt.in); got != tt.want {
			t.Errorf("Canonical(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestProjectIDEquivalentRemotes(t *testing.T) {
	a := ProjectID(Canonical("https://github.com/acme/app.git"))
	b := ProjectID(Canonical("git@github.com:acme/app"))
	if a != b {
		t.Fatalf("ids differ: %s vs %s", a, b)
	}
	if len(a) != 12 {
		t.Fatalf("id %q has length %d, want 12", a, len(a))
	}
	if a == ProjectID(Canonical("github.com/acme/other")) {
		t.Fatal("different remotes produced the same id")
	}
}

func TestMainCheckoutFromWorktree(t *testing.T) {
	testutil.Isolate(t)
	repo := testutil.Repo(t)
	ctx := context.Background()

	wt := filepath.Join(testutil.Resolve(t, t.TempDir()), "wt")
	testutil.Git(t, repo, "worktree", "add", "-q", "-b", "side", wt)

	for _, dir := range []string{repo, wt, filepath.Join(repo)} {
		got, err := MainCheckout(ctx, dir)
		if err != nil {
			t.Fatalf("MainCheckout(%s): %v", dir, err)
		}
		if got != repo {
			t.Fatalf("MainCheckout(%s) = %s, want %s", dir, got, repo)
		}
	}
}

func TestMainCheckoutRejects(t *testing.T) {
	testutil.Isolate(t)
	ctx := context.Background()

	plain := t.TempDir()
	if _, err := MainCheckout(ctx, plain); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("plain dir: got %v, want ErrNotRepo", err)
	}

	bare := filepath.Join(t.TempDir(), "b.git")
	testutil.Git(t, filepath.Dir(bare), "init", "-q", "--bare", bare)
	if _, err := MainCheckout(ctx, bare); !errors.Is(err, ErrBareRepo) {
		t.Fatalf("bare repo: got %v, want ErrBareRepo", err)
	}
}

func TestRemoteURL(t *testing.T) {
	testutil.Isolate(t)
	repo := testutil.Repo(t)
	ctx := context.Background()

	origin := testutil.Git(t, repo, "remote", "get-url", "origin")
	testutil.Git(t, repo, "remote", "add", "aaa", "https://example.com/first.git")
	if got, err := RemoteURL(ctx, repo); err != nil || got != origin {
		t.Fatalf("with origin: got %q, %v; want %q", got, err, origin)
	}

	testutil.Git(t, repo, "remote", "remove", "origin")
	testutil.Git(t, repo, "remote", "add", "zzz", "https://example.com/last.git")
	if got, err := RemoteURL(ctx, repo); err != nil || got != "https://example.com/first.git" {
		t.Fatalf("without origin: got %q, %v", got, err)
	}

	testutil.Git(t, repo, "remote", "remove", "aaa")
	testutil.Git(t, repo, "remote", "remove", "zzz")
	if got, err := RemoteURL(ctx, repo); err != nil || got != "" {
		t.Fatalf("no remotes: got %q, %v", got, err)
	}
}

func TestChanged(t *testing.T) {
	testutil.Isolate(t)
	ctx := context.Background()
	repo := testutil.Repo(t)
	first, err := Run(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(repo, "src", "a.go"), "package a\n")
	testutil.Git(t, repo, "add", "src/a.go")
	testutil.Git(t, repo, "commit", "-q", "-m", "a")
	for _, tt := range []struct {
		specs []string
		want  bool
	}{
		{[]string{":(literal)src/a.go"}, true},
		{[]string{":(glob)src/**"}, true},
		{[]string{":(literal)other.txt"}, false},
	} {
		got, err := Changed(ctx, repo, first, tt.specs)
		if err != nil || got != tt.want {
			t.Errorf("Changed(%v) = %v, %v; want %v", tt.specs, got, err, tt.want)
		}
	}
	if _, err := Changed(ctx, repo, "0123456789abcdef0123456789abcdef01234567", []string{"src"}); err == nil {
		t.Fatal("an unknown commit must be an error, not unchanged")
	}
}

// CodeTree names the code on disk: equal content, equal id, whatever the
// commits; any edit, staged or not, and any new unignored file changes it;
// ignored files never do. The real index is left as it was.
func TestCodeTree(t *testing.T) {
	testutil.Isolate(t)
	ctx := context.Background()
	repo := testutil.Repo(t)
	testutil.WriteFile(t, filepath.Join(repo, ".gitignore"), "ignored.txt\n")
	testutil.Git(t, repo, "add", ".gitignore")
	testutil.Git(t, repo, "commit", "-q", "-m", "ignore")
	tree := func() string {
		t.Helper()
		id, err := CodeTree(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	clean := tree()
	if want := testutil.Git(t, repo, "rev-parse", "HEAD^{tree}"); clean != want {
		t.Fatalf("clean worktree: %s, want HEAD's tree %s", clean, want)
	}
	testutil.WriteFile(t, filepath.Join(repo, "ignored.txt"), "x")
	if got := tree(); got != clean {
		t.Fatalf("an ignored file changed the id: %s -> %s", clean, got)
	}
	testutil.WriteFile(t, filepath.Join(repo, "README.md"), "edited\n")
	edited := tree()
	if edited == clean {
		t.Fatal("an unstaged edit kept the id")
	}
	if st := testutil.Git(t, repo, "status", "--porcelain"); st != "M README.md" {
		t.Fatalf("CodeTree touched the real index: status %q", st)
	}
	testutil.WriteFile(t, filepath.Join(repo, "new.txt"), "n")
	withNew := tree()
	if withNew == edited {
		t.Fatal("an untracked file kept the id")
	}
	testutil.Git(t, repo, "add", "-A")
	if got := tree(); got != withNew {
		t.Fatalf("staging changed the id: %s -> %s", withNew, got)
	}
	testutil.Git(t, repo, "commit", "-q", "-m", "edit")
	if got := tree(); got != withNew {
		t.Fatalf("committing changed the id: %s -> %s", withNew, got)
	}
	testutil.Git(t, repo, "rm", "-q", "new.txt")
	testutil.WriteFile(t, filepath.Join(repo, "README.md"), "app\n")
	if got := tree(); got != clean {
		t.Fatalf("back to the first content: %s, want %s", got, clean)
	}
}
