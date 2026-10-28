package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
)

const hookTimeoutSeconds = 10

var hookEvents = []struct{ event, arg string }{
	{"SessionStart", "session-start"},
	{"Stop", "stop"},
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./\\-]+$`)

func hookCommand(exe, arg string) string {
	prog := exe
	if !plainWord.MatchString(exe) {
		prog = "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
	}
	return prog + " hook " + arg
}

// firstWord splits off the first shell word, honouring simple quotes.
func firstWord(s string) (string, string) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", ""
	}
	if q := s[0]; q == '\'' || q == '"' {
		if end := strings.IndexByte(s[1:], q); end >= 0 {
			return s[1 : end+1], s[end+2:]
		}
		return s[1:], ""
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// isShoalCommand matches a hook command whose program basename is shoalctl
// and whose first argument is hook (§4.6).
func isShoalCommand(cmd string) bool {
	prog, rest := firstWord(cmd)
	base := prog[strings.LastIndexAny(prog, `/\`)+1:]
	base = strings.TrimSuffix(base, ".exe")
	arg, _ := firstWord(rest)
	return base == "shoalctl" && arg == "hook"
}

type hookGroup = map[string]json.RawMessage

func entryCommand(entry map[string]json.RawMessage) string {
	var cmd string
	if raw, ok := entry["command"]; ok && json.Unmarshal(raw, &cmd) == nil {
		return cmd
	}
	return ""
}

func groupEntries(g hookGroup) ([]map[string]json.RawMessage, error) {
	var entries []map[string]json.RawMessage
	if raw, ok := g["hooks"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("a hook group's \"hooks\" is not an array of objects: %w", err)
		}
	}
	return entries, nil
}

// mergeHooks returns settings with exactly one shoal entry per event, keeping
// every other key and entry. It reports changed=false, and returns data
// untouched, when the entries are already exactly right.
func mergeHooks(data []byte, exe string) ([]byte, bool, error) {
	top := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &top); err != nil || top == nil {
			return nil, false, fmt.Errorf("not a JSON object: %v", err)
		}
	}
	hooks := map[string]json.RawMessage{}
	if raw, ok := top["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil || hooks == nil {
			return nil, false, fmt.Errorf("\"hooks\" is not a JSON object: %v", err)
		}
	}
	changed := false
	for _, h := range hookEvents {
		var groups []hookGroup
		if raw, ok := hooks[h.event]; ok {
			if err := json.Unmarshal(raw, &groups); err != nil {
				return nil, false, fmt.Errorf("hooks.%s is not an array of objects: %w", h.event, err)
			}
		}
		next, same, err := ensureEntry(groups, hookCommand(exe, h.arg))
		if err != nil {
			return nil, false, fmt.Errorf("hooks.%s: %w", h.event, err)
		}
		if same {
			continue
		}
		changed = true
		if hooks[h.event], err = json.Marshal(next); err != nil {
			return nil, false, err
		}
	}
	if !changed {
		return data, false, nil
	}
	var err error
	if top["hooks"], err = json.Marshal(hooks); err != nil {
		return nil, false, err
	}
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(out, '\n'), true, nil
}

func ensureEntry(groups []hookGroup, want string) ([]hookGroup, bool, error) {
	var shoal int
	exact := false
	for _, g := range groups {
		entries, err := groupEntries(g)
		if err != nil {
			return nil, false, err
		}
		for _, e := range entries {
			if !isShoalCommand(entryCommand(e)) {
				continue
			}
			shoal++
			var timeout int
			_ = json.Unmarshal(e["timeout"], &timeout)
			exact = len(entries) == 1 && entryCommand(e) == want && timeout == hookTimeoutSeconds && string(e["type"]) == `"command"`
		}
	}
	if shoal == 1 && exact {
		return groups, true, nil
	}

	var out []hookGroup
	for _, g := range groups {
		entries, _ := groupEntries(g)
		var kept []map[string]json.RawMessage
		for _, e := range entries {
			if !isShoalCommand(entryCommand(e)) {
				kept = append(kept, e)
			}
		}
		if len(kept) == len(entries) {
			out = append(out, g)
			continue
		}
		if len(kept) == 0 {
			continue
		}
		raw, err := json.Marshal(kept)
		if err != nil {
			return nil, false, err
		}
		copied := hookGroup{}
		for k, v := range g {
			copied[k] = v
		}
		copied["hooks"] = raw
		out = append(out, copied)
	}
	entry, err := json.Marshal([]map[string]any{{"type": "command", "command": want, "timeout": hookTimeoutSeconds}})
	if err != nil {
		return nil, false, err
	}
	return append(out, hookGroup{"hooks": entry}), false, nil
}

// hasShoalHooks reports whether settings data registers any shoal hook.
func hasShoalHooks(data []byte) (bool, error) {
	var top struct {
		Hooks map[string][]hookGroup `json:"hooks"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return false, err
	}
	for _, groups := range top.Hooks {
		for _, g := range groups {
			entries, err := groupEntries(g)
			if err != nil {
				return false, err
			}
			for _, e := range entries {
				if isShoalCommand(entryCommand(e)) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// hookPreflight refuses, before init changes anything, a repo where shoal's
// hook file cannot stay out of git or would double-register hooks.
func hookPreflight(ctx context.Context, root string) error {
	const local = ".claude/settings.local.json"
	if _, err := gitx.Run(ctx, root, "ls-files", "--error-unmatch", "--", local); err == nil {
		return fail(ExitConfig, "%s is tracked by git, so shoal's hook entries would show up as a change in git status (stealth mode adds it to info/exclude, which does not apply to tracked files). "+
			"Untrack it with `git rm --cached %s` and commit, or move its contents to .claude/settings.json, then run shoalctl init again. Nothing was changed", filepath.Join(root, local), local)
	}
	committed := filepath.Join(root, ".claude", "settings.json")
	if data, err := os.ReadFile(committed); err == nil {
		if found, err := hasShoalHooks(data); err == nil && found {
			return fail(ExitConfig, "%s already registers shoalctl hooks; remove them there so hooks do not fire twice", committed)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", committed, err)
	}
	return nil
}

// installHooks merges shoal's entries into <root>/.claude/settings.local.json.
func installHooks(root, exe string) (string, bool, error) {
	path := filepath.Join(root, ".claude", "settings.local.json")
	data, err := os.ReadFile(path)
	perm := fs.FileMode(0o600)
	switch {
	case err == nil:
		if info, serr := os.Stat(path); serr == nil {
			perm = info.Mode().Perm()
		}
	case errors.Is(err, fs.ErrNotExist):
		data = nil
	default:
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	out, changed, err := mergeHooks(data, exe)
	if err != nil {
		return "", false, fail(ExitConfig, "%s: %v; fix or remove it, shoal will not overwrite it", path, err)
	}
	if !changed {
		return path, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	return path, true, writeFileAtomic(path, out, perm)
}

// ensureExclude appends missing lines to <git-common-dir>/info/exclude.
func ensureExclude(commonDir string, lines []string) (bool, error) {
	path := filepath.Join(commonDir, "info", "exclude")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(data), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add strings.Builder
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		add.WriteString("\n")
	}
	missing := false
	for _, l := range lines {
		if !have[l] {
			add.WriteString(l + "\n")
			missing = true
		}
	}
	if !missing {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.WriteString(add.String()); err != nil {
		f.Close()
		return false, fmt.Errorf("append to %s: %w", path, err)
	}
	return true, f.Close()
}
