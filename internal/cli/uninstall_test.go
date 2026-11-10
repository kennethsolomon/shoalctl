package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// removeHooks undoes mergeHooks byte for byte: the file init merged into
// comes back exactly as the user had it.
func TestRemoveHooksRoundTrip(t *testing.T) {
	t.Parallel()
	exe := "/bin/shoalctl"
	if runtime.GOOS == "windows" {
		exe = `C:\bin\shoalctl.exe`
	}
	inputs := map[string]string{
		"object":             "{\n  \"permissions\": {\"allow\": [\"Bash(ls)\"]}\n}\n",
		"no final newline":   "{\n  \"model\": \"x\"\n}",
		"one line":           `{"model":"x"}`,
		"crlf and tabs":      "{\r\n\t\"model\": \"x\"\r\n}\r\n",
		"empty object":       "{}\n",
		"empty hooks":        "{\n  \"hooks\": {}\n}\n",
		"own hooks":          "{\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"hooks\": [\n          {\"type\": \"command\", \"command\": \"say done\"}\n        ]\n      }\n    ],\n    \"Notification\": []\n  }\n}\n",
		"own PreToolUse":     "{\n  \"hooks\": {\n    \"PreToolUse\": [{\"matcher\": \"Bash\", \"hooks\": [{\"type\": \"command\", \"command\": \"lint\"}]}]\n  },\n  \"env\": {\"A\": \"\\u00e9\"}\n}\n",
		"unicode and escape": "{\"a\": \"\\u2028 ü <>&\", \"hooks\": {\"SessionStart\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"x\"}]}]}}",
	}
	for name, in := range inputs {
		merged, changed, err := mergeHooks([]byte(in), exe)
		if err != nil || !changed {
			t.Fatalf("%s: merge changed=%v err=%v", name, changed, err)
		}
		out, n, err := removeHooks(merged, hasMember([]byte(in), "hooks"))
		if err != nil || n != len(hookEvents) {
			t.Fatalf("%s: removed %d (want %d) err=%v", name, n, len(hookEvents), err)
		}
		if string(out) != in {
			t.Errorf("%s: round trip differs\n got: %q\nwant: %q", name, out, in)
		}
	}
}

func TestRemoveHooksFromScratchFile(t *testing.T) {
	t.Parallel()
	merged, _, err := mergeHooks(nil, "/bin/shoalctl")
	if err != nil {
		t.Fatal(err)
	}
	out, n, err := removeHooks(merged, false)
	if err != nil || n != len(hookEvents) || !isEmptyObject(out) {
		t.Fatalf("removeHooks of a file init created: %q n=%d err=%v", out, n, err)
	}
}

func TestRemoveHooksMatchesOnlyShoal(t *testing.T) {
	t.Parallel()
	in := `{"hooks": {
  "Stop": [{"hooks": [
    {"type": "command", "command": "/usr/local/bin/shoalctl hook stop || true"},
    {"type": "command", "command": "'/Applications/My Tools/shoalctl' hook stop || true"},
    {"type": "command", "command": "/usr/local/bin/shoalctl status"},
    {"type": "command", "command": "/usr/local/bin/notshoalctl hook stop"},
    {"type": "command", "command": "echo shoalctl hook"}
  ]}],
  "SubagentStop": [{"hooks": [{"type": "command", "shell": "powershell", "command": "& 'C:\\shoal\\shoalctl.exe' hook stop; exit 0"}]}],
  "Odd": "not an array"
}}`
	out, n, err := removeHooks([]byte(in), true)
	if err != nil || n != 3 {
		t.Fatalf("removed %d (want 3: two POSIX entries and the PowerShell one) err=%v\n%s", n, err, out)
	}
	for _, kept := range []string{"shoalctl status", "notshoalctl hook stop", "echo shoalctl hook", `"Odd": "not an array"`} {
		if !strings.Contains(string(out), kept) {
			t.Errorf("removeHooks dropped %q:\n%s", kept, out)
		}
	}
	if strings.Contains(string(out), "SubagentStop") || strings.Contains(string(out), "hook stop ||") {
		t.Errorf("shoal entries left:\n%s", out)
	}
	if _, _, err := removeHooks([]byte("[1]"), false); err == nil {
		t.Error("a settings file that is not an object was accepted")
	}
	if out, n, err := removeHooks(nil, false); err != nil || n != 0 || out != nil {
		t.Errorf("empty file: %q %d %v", out, n, err)
	}
}

// withoutManagedBlock undoes withManagedBlock byte for byte.
func TestWithoutManagedBlockRoundTrip(t *testing.T) {
	t.Parallel()
	const imp = "@~/.shoal/projects/abc/context.md"
	inputs := map[string]string{
		"empty":            "",
		"trailing newline": "# Rules\n\n- Use tabs.\n",
		"no final newline": "# Rules\n- Use tabs.",
		"crlf":             "# Rules\r\n- Use tabs.\r\n",
		"crlf no final":    "# Rules\r\n- Use tabs.",
		"bom":              "\ufeff# Rules\n",
		"fenced example":   "# Rules\n```md\n<!-- shoal:begin -->\n@~/OLD\n<!-- shoal:end -->\n```\n",
		"indented example": "- item\n  <!-- shoal:begin -->\n  <!-- shoal:end -->\n",
	}
	for name, in := range inputs {
		with, changed, err := withManagedBlock("CLAUDE.md", []byte(in), imp)
		if err != nil || !changed {
			t.Fatalf("%s: install changed=%v err=%v", name, changed, err)
		}
		out, removed, err := withoutManagedBlock("CLAUDE.md", with)
		if err != nil || !removed || string(out) != in {
			t.Errorf("%s: round trip removed=%v err=%v\n got: %q\nwant: %q", name, removed, err, out, in)
		}
	}
}

func TestWithoutManagedBlockKeepsTheRest(t *testing.T) {
	t.Parallel()
	in := "# Top\n<!-- shoal:begin -->\n@~/x\n<!-- shoal:end -->\n# After\nmore\n"
	out, removed, err := withoutManagedBlock("CLAUDE.md", []byte(in))
	if err != nil || !removed || string(out) != "# Top\n# After\nmore\n" {
		t.Fatalf("got %q removed=%v err=%v", out, removed, err)
	}
	none := "# Only rules\n"
	if out, removed, err := withoutManagedBlock("CLAUDE.md", []byte(none)); err != nil || removed || string(out) != none {
		t.Fatalf("no block: %q removed=%v err=%v", out, removed, err)
	}
	for _, bad := range []string{
		"<!-- shoal:begin -->\n",
		"<!-- shoal:end -->\n<!-- shoal:begin -->\n",
		"<!-- shoal:begin -->\n<!-- shoal:end -->\n<!-- shoal:begin -->\n<!-- shoal:end -->\n",
	} {
		_, _, err := withoutManagedBlock("CLAUDE.md", []byte(bad))
		if codeOf(err) != ExitConfig || !strings.Contains(err.Error(), "rerun shoalctl uninstall") {
			t.Errorf("%q: err = %v (want exit 3 naming uninstall)", bad, err)
		}
	}
}

// footprint is everything git sees in the repo: tracked changes, untracked
// files and ignored files, plus the index.
func footprint(t *testing.T, repo string) string {
	t.Helper()
	return testutil.Git(t, repo, "status", "--porcelain=v1", "--ignored", "--untracked-files=all") + "\n--\n" + testutil.Git(t, repo, "ls-files", "-s")
}

func TestUninstallRestoresAStealthRepo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	before := footprint(t, h.repo)
	h.mustRun("init")
	if footprint(t, h.repo) == before {
		t.Fatal("init left no trace to remove; the test proves nothing")
	}
	out := h.mustRun("uninstall")
	mustContain(t, "uninstall", out, "removed 4 shoal entries and "+filepath.Join(h.repo, ".claude", "settings.local.json")+", which shoal created",
		"block    no "+filepath.Join(h.repo, "CLAUDE.md")+"\n", "kept     "+h.project().dir, "shoalctl purge "+h.project().id)
	if got := footprint(t, h.repo); got != before {
		t.Fatalf("repo after uninstall:\n%s\nbefore init:\n%s", got, before)
	}
	if fileExists(filepath.Join(h.repo, ".claude")) {
		t.Fatal(".claude/ that shoal created is still there")
	}
	mustContain(t, "second uninstall", h.mustRun("uninstall"), "no shoal entries in")
}

func TestUninstallKeepsTheUsersSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	settings := filepath.Join(h.repo, ".claude", "settings.local.json")
	user := "{\r\n\t\"permissions\": {\"allow\": [\"Bash(make test)\"]},\r\n\t\"hooks\": {\"Stop\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"say done\"}]}]}\r\n}"
	testutil.WriteFile(t, settings, user)
	os.Chmod(settings, 0o640)
	h.mustRun("init")
	if readFile(t, settings) == user {
		t.Fatal("init did not register hooks")
	}
	commitFile(t, h.repo, "CLAUDE.md", "# Rules\n")
	mustContain(t, "uninstall", h.mustRun("uninstall"), "removed 4 shoal entries from "+settings+"; your own settings stay",
		"no shoal block in "+filepath.Join(h.repo, "CLAUDE.md"))
	if got := readFile(t, settings); got != user {
		t.Fatalf("settings after uninstall:\n%q\nwant:\n%q", got, user)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(settings); info.Mode().Perm() != 0o640 {
			t.Fatalf("mode %v, want 0640", info.Mode().Perm())
		}
	}
}

func TestUninstallRemovesTheManagedBlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	user := "# Team rules\r\n- Run gofmt before committing"
	commitFile(t, h.repo, "CLAUDE.md", user)
	h.mustRun("init")
	testutil.WriteFile(t, filepath.Join(h.project().dir, "config.toml"), "[install]\nstealth = false\n")
	mustContain(t, "init", h.mustRun("init"), "block    written to CLAUDE.md")
	mustContain(t, "uninstall", h.mustRun("uninstall"), "removed the shoal block from "+filepath.Join(h.repo, "CLAUDE.md"))
	if got := readFile(t, filepath.Join(h.repo, "CLAUDE.md")); got != user {
		t.Fatalf("CLAUDE.md after uninstall: %q, want %q", got, user)
	}
	if st := testutil.Git(t, h.repo, "status", "--porcelain"); st != "" {
		t.Fatalf("git status after uninstall: %q", st)
	}

	// A CLAUDE.md init created, holding only the block, goes with it.
	h2 := newHarness(t)
	h2.mustRun("init")
	testutil.WriteFile(t, filepath.Join(h2.project().dir, "config.toml"), "[install]\nstealth = false\n")
	h2.mustRun("init")
	mustContain(t, "uninstall", h2.mustRun("uninstall"), "it held only the shoal block and git does not track it")
	if fileExists(filepath.Join(h2.repo, "CLAUDE.md")) {
		t.Fatal("the CLAUDE.md shoal created is still there")
	}
}

func TestUninstallRefusesBeforeChangingAnything(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.mustRun("init")
	settings := filepath.Join(h.repo, ".claude", "settings.local.json")
	registered := readFile(t, settings)
	broken := "# Rules\n<!-- shoal:begin -->\n"
	testutil.WriteFile(t, filepath.Join(h.repo, "CLAUDE.md"), broken)
	errOut := h.expectCode(ExitConfig, "uninstall")
	mustContain(t, "refusal", errOut, "unpaired shoal marker", "rerun shoalctl uninstall")
	if readFile(t, settings) != registered || readFile(t, filepath.Join(h.repo, "CLAUDE.md")) != broken {
		t.Fatal("a refused uninstall changed a file")
	}

	testutil.WriteFile(t, filepath.Join(h.repo, "CLAUDE.md"), "# Rules\n")
	testutil.WriteFile(t, settings, "[not, an, object]")
	mustContain(t, "bad settings", h.expectCode(ExitConfig, "uninstall"), "not a JSON object", "Nothing was changed")

	// Two "hooks" keys (top level or in one group), or one event twice: a JSON
	// parser keeps only the last, and removing by name would drop both. Refused before anything changes.
	for _, dup := range []string{
		strings.Replace(registered, "{", `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "mine"}]}]},`, 1),
		strings.Replace(registered, `"hooks": {`, `"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "mine"}]}], "Stop": [],`, 1),
		`{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "mine"}], "hooks": [{"type": "command", "command": "/bin/shoalctl hook stop || true"}]}]}}`,
	} {
		testutil.WriteFile(t, settings, dup)
		mustContain(t, "duplicate keys", h.expectCode(ExitConfig, "uninstall"), "more than once", "Nothing was changed")
		if readFile(t, settings) != dup {
			t.Fatal("a refused uninstall changed the settings")
		}
	}
}

// uninstall after purge has no project data left to keep, and says so.
func TestUninstallAfterPurge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.mustRun("init")
	p := h.project()
	h.asHuman()
	h.answers = p.id + "\n"
	h.mustRun("purge", p.id)
	out := h.mustRun("uninstall")
	mustContain(t, "uninstall", out, "data     none: "+p.dir+" is already gone")
	if strings.Contains(out, "kept     "+p.dir) {
		t.Fatalf("uninstall says it kept a purged directory:\n%s", out)
	}
}

func TestUninstallRemovesOnlyUneditedAgents(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("agents", "install")
	h.mustRun("agents", "install", "--commit")
	userDir := filepath.Join(h.home, ".claude", "agents")
	repoDir := filepath.Join(h.repo, ".claude", "agents")
	testutil.WriteFile(t, filepath.Join(userDir, "shoal-lead.md"), "my lead\n")
	testutil.WriteFile(t, filepath.Join(repoDir, "shoal-docs.md"), "my docs\n")

	// Another registered project still uses the user-level definitions.
	other := filepath.Join(h.home, ".shoal", "projects", "0123456789ab")
	testutil.WriteFile(t, filepath.Join(other, "project.json"), `{"id": "0123456789ab"}`)
	out := h.mustRun("uninstall")
	mustContain(t, "uninstall", out, "removed 7 unedited shoal-* definition(s); 1 edited one(s) left alone",
		filepath.Join(repoDir, "shoal-docs.md")+" left alone", userDir+" kept: 1 other shoal project(s) use them (0123456789ab)")
	if left, _ := os.ReadDir(repoDir); len(left) != 1 {
		t.Fatalf("repo agents left: %v", left)
	}
	if left, _ := os.ReadDir(userDir); len(left) != 8 {
		t.Fatalf("user agents touched while another project uses them: %v", left)
	}

	os.RemoveAll(other)
	mustContain(t, "uninstall", h.mustRun("uninstall"), "removed 7 unedited shoal-* definition(s); 2 edited one(s) left alone")
	if left, _ := os.ReadDir(userDir); len(left) != 1 || left[0].Name() != "shoal-lead.md" {
		t.Fatalf("user agents left: %v", left)
	}
}
