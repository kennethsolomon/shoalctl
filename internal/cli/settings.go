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

// hookEntry is the entry shoal registers, in the key order it is written.
type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type hookGroupOut struct {
	Hooks []hookEntry `json:"hooks"`
}

func wantEntry(exe, arg string) hookEntry {
	return hookEntry{Type: "command", Command: hookCommand(exe, arg), Timeout: hookTimeoutSeconds}
}

// entryIs reports whether an existing entry is exactly want, with no other keys.
func entryIs(e map[string]json.RawMessage, want hookEntry) bool {
	raw, err := json.Marshal(want)
	if err != nil {
		return false
	}
	var w map[string]json.RawMessage
	if json.Unmarshal(raw, &w) != nil || len(w) != len(e) {
		return false
	}
	for k, v := range w {
		if !jsonEqual(e[k], v) {
			return false
		}
	}
	return true
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return fmt.Sprint(x) == fmt.Sprint(y)
}

// decodeHooks validates the settings shape shoal edits and returns the groups
// of each event.
func decodeHooks(data []byte) (map[string][]hookGroup, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return nil, fmt.Errorf("not a JSON object: %v", err)
	}
	out := map[string][]hookGroup{}
	raw, ok := top["hooks"]
	if !ok {
		return out, nil
	}
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(raw, &hooks); err != nil || hooks == nil {
		return nil, fmt.Errorf("\"hooks\" is not a JSON object: %v", err)
	}
	for _, h := range hookEvents {
		raw, ok := hooks[h.event]
		if !ok {
			continue
		}
		var groups []hookGroup
		if err := json.Unmarshal(raw, &groups); err != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			return nil, fmt.Errorf("hooks.%s is not an array of objects: %v", h.event, err)
		}
		for _, g := range groups {
			if _, err := groupEntries(g); err != nil {
				return nil, fmt.Errorf("hooks.%s: %w", h.event, err)
			}
		}
		out[h.event] = groups
	}
	return out, nil
}

// exactlyOne reports whether groups hold exactly one shoal entry, equal to
// want and alone in its group.
func exactlyOne(groups []hookGroup, want hookEntry) bool {
	shoal, exact := 0, false
	for _, g := range groups {
		entries, _ := groupEntries(g)
		for _, e := range entries {
			if isShoalCommand(entryCommand(e)) {
				shoal++
				exact = len(entries) == 1 && entryIs(e, want)
			}
		}
	}
	return shoal == 1 && exact
}

// mergeHooks returns settings with exactly one shoal entry per event. It edits
// only shoal's own entries in place, so every other byte of the file (key
// order, escapes, unicode, indentation, line endings, final newline) stays as
// it was. It reports changed=false, and returns data untouched, when the
// entries are already exactly right.
func mergeHooks(data []byte, exe string) ([]byte, bool, error) {
	doc := data
	if len(bytes.TrimSpace(data)) == 0 {
		doc = []byte("{\n}\n")
	}
	events, err := decodeHooks(doc)
	if err != nil {
		return nil, false, err
	}
	st := detectStyle(doc)
	changed := false
	for _, h := range hookEvents {
		want := wantEntry(exe, h.arg)
		if exactlyOne(events[h.event], want) {
			continue
		}
		changed = true
		doc = removeShoalEntries(doc, h.event)
		doc = appendGroup(doc, h.event, hookGroupOut{Hooks: []hookEntry{want}}, st)
	}
	if !changed {
		return data, false, nil
	}
	return doc, true, nil
}

// eventArray finds hooks.<event> in doc.
func eventArray(doc []byte, event string) (span, bool) {
	hooks, ok := member(doc, topValue(doc), "hooks")
	if !ok {
		return span{}, false
	}
	ev, ok := member(doc, hooks.val, event)
	if !ok {
		return span{}, false
	}
	return ev.val, true
}

// removeShoalEntries deletes every shoal entry under hooks.<event>, and any
// group left without entries.
func removeShoalEntries(doc []byte, event string) []byte {
	for {
		arr, ok := eventArray(doc, event)
		if !ok {
			return doc
		}
		next, removed := removeFirstShoalEntry(doc, arr)
		if !removed {
			return doc
		}
		doc = next
	}
}

func removeFirstShoalEntry(doc []byte, arr span) ([]byte, bool) {
	groups := arrayElements(doc, arr)
	for gi, g := range groups {
		hm, ok := member(doc, g, "hooks")
		if !ok || doc[hm.val.start] != '[' {
			continue
		}
		entries := arrayElements(doc, hm.val)
		for ei, e := range entries {
			var entry map[string]json.RawMessage
			if json.Unmarshal(doc[e.start:e.end], &entry) != nil || !isShoalCommand(entryCommand(entry)) {
				continue
			}
			if len(entries) == 1 {
				return removeItem(doc, arr, groups, gi), true
			}
			return removeItem(doc, hm.val, entries, ei), true
		}
	}
	return doc, false
}

// appendGroup adds group as the last group of hooks.<event>, creating the
// "hooks" object and the event array when missing.
func appendGroup(doc []byte, event string, group hookGroupOut, st jsonStyle) []byte {
	top := topValue(doc)
	hooks, ok := member(doc, top, "hooks")
	if !ok {
		doc = appendMember(doc, top, "hooks", "{}", st)
		hooks, _ = member(doc, topValue(doc), "hooks")
	}
	if _, ok := member(doc, hooks.val, event); !ok {
		doc = appendMember(doc, hooks.val, event, "[]", st)
	}
	arr, _ := eventArray(doc, event)
	return appendItem(doc, arr, arrayElements(doc, arr), st, func(ind string) string { return render(group, ind, st) })
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

// installHooks merges shoal's entries into <root>/.claude/settings.local.json
// (its target, when that is a symlink). The first time it changes an existing
// file it keeps the original bytes at bakPath; later runs never overwrite
// that backup. The write is atomic and keeps the file's mode.
func installHooks(root, exe, bakPath string) (string, bool, error) {
	path := filepath.Join(root, ".claude", "settings.local.json")
	target := path
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		if target, err = filepath.EvalSymlinks(path); err != nil {
			return "", false, fail(ExitConfig, "%s is a symlink shoal cannot follow: %v", path, err)
		}
	}
	data, err := os.ReadFile(target)
	existed := err == nil
	perm := fs.FileMode(0o600)
	switch {
	case existed:
		info, serr := os.Stat(target)
		if serr != nil {
			return "", false, fmt.Errorf("stat %s: %w", target, serr)
		}
		perm = info.Mode().Perm()
	case errors.Is(err, fs.ErrNotExist):
		data = nil
	default:
		return "", false, fmt.Errorf("read %s: %w", target, err)
	}
	out, changed, err := mergeHooks(data, exe)
	if err != nil {
		return "", false, fail(ExitConfig, "%s: %v; fix or remove it, shoal will not overwrite it", path, err)
	}
	if !changed {
		return path, false, nil
	}
	if existed {
		if _, err := os.Stat(bakPath); errors.Is(err, fs.ErrNotExist) {
			if err := writePrivate(bakPath, data); err != nil {
				return "", false, fmt.Errorf("back up %s: %w", target, err)
			}
		} else if err != nil {
			return "", false, fmt.Errorf("stat backup %s: %w", bakPath, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", false, fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}
	return path, true, writeFileAtomic(target, out, perm)
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
