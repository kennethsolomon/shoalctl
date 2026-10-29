package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kennethsolomon/shoalctl/internal/agents"
	"github.com/kennethsolomon/shoalctl/internal/gitx"
)

// userAgentsDir is Claude Code's user-level agents directory.
func userAgentsDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		if !filepath.IsAbs(d) {
			return "", fail(ExitConfig, "CLAUDE_CONFIG_DIR %q must be an absolute path", d)
		}
		return filepath.Join(d, "agents"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fail(ExitConfig, "find home directory for ~/.claude/agents: %v", err)
	}
	return filepath.Join(home, ".claude", "agents"), nil
}

func (env Env) cmdAgents(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fail(ExitUsage, "usage: agents install [--commit] | agents list")
	}
	home, err := shoalHome()
	if err != nil {
		return err
	}
	manifest := filepath.Join(home, "agents-manifest.json")
	userDir, err := userAgentsDir()
	if err != nil {
		return err
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("agents install", flag.ContinueOnError)
		commit := fs.Bool("commit", false, "write into <repo>/.claude/agents/ for version control instead of ~/.claude/agents/")
		if _, err := parseArgs(fs, args[1:], 0); err != nil {
			return err
		}
		dir := userDir
		if *commit {
			if err := checkGit(ctx); err != nil {
				return err
			}
			root, err := gitx.MainCheckout(ctx, env.Dir)
			if err != nil {
				return withCode(ExitConfig, err)
			}
			dir = filepath.Join(root, ".claude", "agents")
		}
		entries, err := agents.Install(dir, manifest)
		if err != nil {
			return err
		}
		edited := 0
		for _, e := range entries {
			fmt.Fprintf(env.Stdout, "%-22s %s\n", e.Name, e.Action)
			if e.Status == agents.StatusModified {
				edited++
			}
		}
		fmt.Fprintf(env.Stdout, "agent definitions in %s (shoal_version %d)\n", dir, agents.Version)
		if *commit {
			fmt.Fprintln(env.Stdout, "commit them yourself when you are ready; shoal never commits")
		}
		if edited > 0 {
			fmt.Fprintf(env.Stderr, "%d edited definition(s) left alone; delete one to get shoal's version back\n", edited)
		}
		return nil
	case "list":
		if _, err := parseArgs(flag.NewFlagSet("agents list", flag.ContinueOnError), args[1:], 0); err != nil {
			return err
		}
		dirs := []string{userDir}
		if root, err := gitx.MainCheckout(ctx, env.Dir); err == nil {
			if _, err := os.Stat(filepath.Join(root, ".claude", "agents")); err == nil {
				dirs = append(dirs, filepath.Join(root, ".claude", "agents"))
			}
		}
		for _, dir := range dirs {
			entries, err := agents.List(dir, manifest)
			if err != nil {
				return err
			}
			fmt.Fprintln(env.Stdout, dir)
			for _, e := range entries {
				fmt.Fprintf(env.Stdout, "  %-22s %s\n", e.Name, e.Status)
			}
		}
		return nil
	}
	return fail(ExitUsage, "unknown agents command %q: use install or list", args[0])
}
