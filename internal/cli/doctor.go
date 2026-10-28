package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// cmdDoctor is the minimal doctor that ships with the MVP: it checks only the
// hook entries in the main checkout's .claude/settings.local.json, and with
// --fix rewrites them to run this binary. The full doctor (§10.4) is Phase 2.
func (env Env) cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fix := fs.Bool("fix", false, "point shoal's hook entries at this binary")
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}
	p, err := env.identify(ctx)
	if err != nil {
		return err
	}
	path := filepath.Join(p.root, ".claude", "settings.local.json")
	problems, binaries, err := hookProblems(path)
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		fmt.Fprintf(env.Stdout, "ok   hooks in %s run %v\n", path, binaries)
		return nil
	}
	for _, pr := range problems {
		fmt.Fprintf(env.Stderr, "FAIL %s\n", pr)
	}
	if !*fix {
		return fail(ExitDoctor, "%d problem(s) with shoal's hooks in %s. Hooks fail open, so these sessions run without shoal's heartbeats. Run shoalctl doctor --fix to point them at %s",
			len(problems), path, env.Exe)
	}
	if err := requireAbsoluteExe(env.Exe); err != nil {
		return err
	}
	if err := hookPreflight(ctx, p.root); err != nil {
		return err
	}
	if _, _, err := installHooks(p.root, env.Exe, filepath.Join(p.dir, "backups", "settings.local.json.bak")); err != nil {
		return err
	}
	if left, _, err := hookProblems(path); err != nil {
		return err
	} else if len(left) > 0 {
		return fail(ExitDoctor, "problems remain after --fix: %v", left)
	}
	fmt.Fprintf(env.Stdout, "fixed: hooks in %s now run %s\n", path, env.Exe)
	return nil
}

// hookProblems lists what is wrong with shoal's entries in a settings file:
// an event with no entry or several, a binary that is missing or not
// executable, or an entry that is not in the current shape.
func hookProblems(path string) (problems, binaries []string, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{fmt.Sprintf("%s does not exist: SessionStart and Stop hooks are not registered", path)}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	events, err := decodeHooks(data)
	if err != nil {
		return nil, nil, fail(ExitConfig, "%s: %v; fix it by hand, shoal will not overwrite it", path, err)
	}
	for _, h := range hookEvents {
		var cmds []string
		for _, g := range events[h.event] {
			entries, _ := groupEntries(g)
			for _, e := range entries {
				if c := entryCommand(e); isShoalCommand(c) {
					cmds = append(cmds, c)
				}
			}
		}
		switch len(cmds) {
		case 0:
			problems = append(problems, fmt.Sprintf("%s: shoal hook not registered", h.event))
			continue
		case 1:
		default:
			problems = append(problems, fmt.Sprintf("%s: %d shoal entries, so the hook would run %d times", h.event, len(cmds), len(cmds)))
			continue
		}
		prog, _ := commandProgram(cmds[0])
		binaries = append(binaries, prog)
		if msg := binaryProblem(prog); msg != "" {
			problems = append(problems, fmt.Sprintf("%s: %s", h.event, msg))
			continue
		}
		if !exactlyOne(events[h.event], entryFor(runtime.GOOS, prog, h.arg)) {
			problems = append(problems, fmt.Sprintf("%s: entry is outdated; want %q", h.event, hookCommandFor(runtime.GOOS, prog, h.arg)))
		}
	}
	return problems, binaries, nil
}

func binaryProblem(prog string) string {
	if !filepath.IsAbs(prog) {
		return fmt.Sprintf("binary path %q is not absolute", prog)
	}
	info, err := os.Stat(prog)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("binary %s is missing (moved or deleted)", prog)
	case err != nil:
		return fmt.Sprintf("binary %s: %v", prog, err)
	case !info.Mode().IsRegular():
		return fmt.Sprintf("binary %s is not a regular file", prog)
	case runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0:
		return fmt.Sprintf("binary %s is not executable", prog)
	}
	return ""
}

// requireAbsoluteExe refuses to register a binary path that would depend on
// the hook's working directory or PATH.
func requireAbsoluteExe(exe string) error {
	if !filepath.IsAbs(exe) {
		return fail(ExitInternal, "shoalctl binary path %q is not absolute; hooks must name the binary by absolute path", exe)
	}
	return nil
}
