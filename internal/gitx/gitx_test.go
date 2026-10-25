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
