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
	"runtime"
	"strings"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
)

const hookTimeoutSeconds = 10

var hookEvents = []struct{ event, arg string }{
	{"SessionStart", "session-start"},
	{"Stop", "stop"},
}

// posixPlain is a path bash reads as one literal word without quotes.
var posixPlain = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// psQuotes are the characters PowerShell accepts as single quotes; inside a
// single-quoted string each is escaped by doubling it.
const psQuotes = "'\u2018\u2019\u201a\u201b"

// hookCommand is the hook command for this OS.
func hookCommand(exe, arg string) string { return hookCommandFor(runtime.GOOS, exe, arg) }

// hookCommandFor quotes exe for the shell Claude Code runs the hook in: bash
// on macOS and Linux (its default), PowerShell on Windows, where the entry
// pins "shell": "powershell" because the default there depends on whether
// Git Bash is installed.
//
// The shell also maps every exit status to 0 ("|| true", "; exit 0"). The
// binary never exits 2 on purpose, but a Go runtime fatal error exits 2, and
// a crash, a kill, or a moved binary (127) are not the binary's to decide.
// Claude Code reads exit 2 as "block", so the wrapper is what makes hooks
// fail open in every case (§4.6).
func hookCommandFor(goos, exe, arg string) string {
	if goos == "windows" {
		var b strings.Builder
		for _, r := range exe {
			if strings.ContainsRune(psQuotes, r) {
				b.WriteRune(r)
			}
			b.WriteRune(r)
		}
		return "& '" + b.String() + "' hook " + arg + "; exit 0"
	}
	prog := exe
	if !posixPlain.MatchString(exe) {
		prog = "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
	}
	return prog + " hook " + arg + " || true"
}

func entryFor(goos, exe, arg string) hookEntry {
	e := hookEntry{Type: "command", Command: hookCommandFor(goos, exe, arg), Timeout: hookTimeoutSeconds}
	if goos == "windows" {
		e.Shell = "powershell"
	}
	return e
}

// commandProgram splits a hook command into its program and the rest. It
// reads both forms shoal writes (bash words, and PowerShell's "& '<path>'")
// and, leniently, an unquoted word with backslashes as a Windows path.
func commandProgram(cmd string) (string, string) {
	s := strings.TrimLeft(cmd, " \t")
	if rest, ok := strings.CutPrefix(s, "&"); ok {
		return psWord(strings.TrimLeft(rest, " \t"))
	}
	return shWord(s)
}

func psWord(s string) (string, string) {
	r := []rune(s)
	if len(r) == 0 || !strings.ContainsRune(psQuotes, r[0]) {
		return shWord(s)
	}
	var b strings.Builder
	for i := 1; i < len(r); i++ {
		if !strings.ContainsRune(psQuotes, r[i]) {
			b.WriteRune(r[i])
			continue
		}
		if i+1 < len(r) && strings.ContainsRune(psQuotes, r[i+1]) {
			b.WriteRune(r[i])
			i++
			continue
		}
		return b.String(), string(r[i+1:])
	}
	return b.String(), ""
}

// shWord reads one bash word. A word with no quotes is taken as written, so
// a bare Windows path keeps its backslashes.
func shWord(s string) (string, string) {
	s = strings.TrimLeft(s, " \t")
	end, quoted := 0, false
	for end < len(s) && s[end] != ' ' && s[end] != '\t' {
		switch s[end] {
		case '\'':
			quoted = true
			if j := strings.IndexByte(s[end+1:], '\''); j >= 0 {
				end += j + 1
			} else {
				end = len(s) - 1
			}
		case '"':
			quoted = true
			for end++; end < len(s) && s[end] != '"'; end++ {
				if s[end] == '\\' {
					end++
				}
			}
		case '\\':
			end++
		}
		end++
	}
	if end > len(s) {
		end = len(s)
	}
	word, rest := s[:end], s[end:]
	if !quoted {
		return word, rest
	}
	var b strings.Builder
	for i := 0; i < len(word); i++ {
		switch c := word[i]; c {
		case '\'':
			j := strings.IndexByte(word[i+1:], '\'')
			if j < 0 {
				j = len(word) - i - 1
			}
			b.WriteString(word[i+1 : i+1+j])
			i += j + 1
		case '"':
			for i++; i < len(word) && word[i] != '"'; i++ {
				if word[i] == '\\' && i+1 < len(word) && strings.IndexByte("\"\\$`", word[i+1]) >= 0 {
					i++
				}
				b.WriteByte(word[i])
			}
		case '\\':
			if i+1 < len(word) {
				i++
				b.WriteByte(word[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), rest
}

// isShoalCommand matches a hook command whose program basename is shoalctl
// and whose first argument is hook (§4.6).
func isShoalCommand(cmd string) bool {
	prog, rest := commandProgram(cmd)
	base := prog[strings.LastIndexAny(prog, `/\`)+1:]
	if len(base) > 4 && strings.EqualFold(base[len(base)-4:], ".exe") {
		base = base[:len(base)-4]
	}
	arg, _ := shWord(rest)
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
	Shell   string `json:"shell,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type hookGroupOut struct {
	Hooks []hookEntry `json:"hooks"`
}

func wantEntry(exe, arg string) hookEntry { return entryFor(runtime.GOOS, exe, arg) }

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
