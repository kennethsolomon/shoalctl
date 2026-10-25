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
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
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
