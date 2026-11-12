// Package gitx runs git and derives the project identity shoal keys everything on.
package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

var (
	ErrNotRepo  = errors.New("not a git repository")
	ErrBareRepo = errors.New("bare repositories are not supported")
)

// Run runs git in dir and returns its stdout with surrounding whitespace trimmed.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	return runEnv(ctx, dir, nil, args...)
}

// runEnv is Run with extra KEY=VALUE environment entries, such as
// GIT_INDEX_FILE, on top of the process environment.
func runEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s (in %s): %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// MainCheckout returns the absolute, symlink-resolved path of the main checkout
// that dir belongs to: the parent of the common git dir.
func MainCheckout(ctx context.Context, dir string) (string, error) {
	bare, err := Run(ctx, dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		return "", fmt.Errorf("%s: %w", dir, ErrNotRepo)
	}
	if bare == "true" {
		return "", fmt.Errorf("%s: %w", dir, ErrBareRepo)
	}
	if inside, err := Run(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil || inside != "true" {
		return "", fmt.Errorf("%s is not inside a work tree: %w", dir, ErrNotRepo)
	}
	common, err := CommonDir(ctx, dir)
	if err != nil {
		return "", err
	}
	main := filepath.Dir(common)
	resolved, err := filepath.EvalSymlinks(main)
	if err != nil {
		return "", fmt.Errorf("resolve main checkout %s: %w", main, err)
	}
	return resolved, nil
}

// CommonDir returns the absolute path of the git dir shared by all worktrees.
func CommonDir(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Clean(out), nil
}

// RemoteURL returns remote.origin.url, else the URL of the alphabetically first
// remote, else "" when the repo has no remotes.
func RemoteURL(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "remote")
	if err != nil {
		return "", err
	}
	remotes := strings.Fields(out)
	if len(remotes) == 0 {
		return "", nil
	}
	sort.Strings(remotes)
	name := remotes[0]
	for _, r := range remotes {
		if r == "origin" {
			name = r
		}
	}
	return Run(ctx, dir, "remote", "get-url", name)
}

// Canonical normalizes a remote URL so that equivalent spellings of one remote
// (https, ssh, scp-style, with or without .git) produce the same string.
func Canonical(remote string) string {
	s := strings.TrimSpace(remote)
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		host, path, _ := strings.Cut(rest, "/")
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		if c := strings.LastIndex(host, ":"); c >= 0 && !strings.HasSuffix(host, "]") {
			host = host[:c]
		}
		s = strings.ToLower(host) + "/" + path
		if host == "" {
			s = "/" + path
		}
	} else if host, path, ok := scpParts(s); ok {
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		s = strings.ToLower(host) + "/" + strings.TrimPrefix(path, "/")
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return strings.TrimSuffix(s, "/")
}

// scpParts splits an scp-style "[user@]host:path". A colon after the first
// slash, or a single-letter drive prefix like C:, is not scp syntax.
func scpParts(s string) (host, path string, ok bool) {
	colon := strings.Index(s, ":")
	if colon <= 0 {
		return "", "", false
	}
	if slash := strings.Index(s, "/"); slash >= 0 && slash < colon {
		return "", "", false
	}
	if colon == 1 {
		return "", "", false
	}
	return s[:colon], s[colon+1:], true
}

// ProjectID is the first 12 hex characters of SHA-256 of the canonical form.
func ProjectID(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:12]
}

// Changed reports whether any path matching pathspecs differs between the
// commit from and HEAD in dir (git diff --quiet: exit 1 means changed). Any
// other failure, such as from being unknown in this clone, is an error.
func Changed(ctx context.Context, dir, from string, pathspecs []string) (bool, error) {
	args := append([]string{"diff", "--quiet", "--no-ext-diff", from, "HEAD", "--"}, pathspecs...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return false, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return true, nil
	}
	return false, fmt.Errorf("git %s (in %s): %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(stderr.String()))
}

// CodeTree returns the git tree id of the code in worktree wt as it is on
// disk: tracked files with any uncommitted edits, plus untracked files git
// does not ignore. Ignored files (shoal's own .shoal/ is excluded by init)
// do not count. Equal code gives an equal id, whatever the commits.
//
// A clean worktree is HEAD's tree. A dirty one is staged into a copy of its
// index (the copy keeps git's stat cache, so unchanged files are not
// re-read) and written as a tree; the real index is never touched. That
// stores blobs for the edited files in the object database, as git add
// would; git gc prunes them once nothing refers to them.
func CodeTree(ctx context.Context, wt string) (string, error) {
	// --untracked-files=normal overrides status.showUntrackedFiles=no, which
	// would hide a new file and return HEAD's tree.
	dirty, err := Run(ctx, wt, "--no-optional-locks", "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", err
	}
	if dirty == "" {
		return Run(ctx, wt, "rev-parse", "HEAD^{tree}")
	}
	index, err := Run(ctx, wt, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(index) {
		index = filepath.Join(wt, index)
	}
	tmp, err := os.MkdirTemp("", "shoal-index-")
	if err != nil {
		return "", fmt.Errorf("code tree of %s: %w", wt, err)
	}
	defer os.RemoveAll(tmp)
	copyIndex := filepath.Join(tmp, "index")
	data, err := os.ReadFile(index)
	switch {
	case err == nil:
		if err := os.WriteFile(copyIndex, data, 0o600); err != nil {
			return "", fmt.Errorf("code tree of %s: %w", wt, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("code tree of %s: read index: %w", wt, err)
	}
	env := []string{"GIT_INDEX_FILE=" + copyIndex}
	if _, err := runEnv(ctx, wt, env, "add", "--all"); err != nil {
		return "", err
	}
	return runEnv(ctx, wt, env, "write-tree")
}
