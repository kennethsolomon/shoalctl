package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kennethsolomon/shoalctl/docs/guide"
)

// usageCommands maps each command of the usage text to its synopses (the
// part of a line before the two-space gap of its description).
func usageCommands(t *testing.T) map[string][]string {
	t.Helper()
	cmds := map[string][]string{"help": nil}
	for _, line := range strings.Split(usage, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		synopsis, _, _ := strings.Cut(strings.TrimSpace(line), "  ")
		name := strings.Fields(synopsis)[0]
		cmds[name] = append(cmds[name], synopsis)
	}
	if len(cmds) < 20 {
		t.Fatalf("parsed only %d commands from usage; the usage layout changed", len(cmds))
	}
	return cmds
}

var bareWord = regexp.MustCompile(`^[a-z][a-z-]*$`)

// subcommands are the literal words a command takes as its first argument
// ("agents install", "repro red|green"); none for a command like "new".
func subcommands(name string, synopses []string) map[string]bool {
	subs := map[string]bool{}
	for _, s := range synopses {
		for _, alt := range strings.Split(s, " | ") {
			f := strings.Fields(alt)
			if len(f) < 2 || f[0] != name || strings.ContainsAny(f[1][:1], "<[\"") {
				continue
			}
			for _, w := range strings.Split(f[1], "|") {
				if bareWord.MatchString(w) {
					subs[w] = true
				}
			}
		}
	}
	return subs
}

func guideTexts(t *testing.T) map[string]string {
	t.Helper()
	index, err := guide.Index()
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]string{"index": index}
	for _, name := range guide.Topics {
		if texts[name], err = guide.Topic(name); err != nil {
			t.Fatal(err)
		}
	}
	return texts
}

// guideInvocation is "shoalctl" and the words that follow it with single
// spaces; two spaces start the description.
var guideInvocation = regexp.MustCompile(`shoalctl((?: [^ \n]+)+)`)

func TestGuideNamesOnlyCommandsSubcommandsAndFlagsInUsage(t *testing.T) {
	t.Parallel()
	cmds := usageCommands(t)
	for file, text := range guideTexts(t) {
		for _, m := range guideInvocation.FindAllStringSubmatch(text, -1) {
			var words []string
			for _, w := range strings.Fields(m[1]) {
				trimmed := strings.TrimRight(w, ":.,)")
				words = append(words, trimmed)
				if trimmed != w {
					break
				}
			}
			synopses, ok := cmds[words[0]]
			if !ok {
				t.Errorf("%s.md: %q names command %q, which is not in usage", file, m[0], words[0])
				continue
			}
			if words[0] == "guide" {
				if len(words) > 1 {
					if _, err := guide.Topic(words[1]); err != nil {
						t.Errorf("%s.md: %q: %v", file, m[0], err)
					}
				}
				continue
			}
			if subs := subcommands(words[0], synopses); len(subs) > 0 && len(words) > 1 && bareWord.MatchString(words[1]) && !subs[words[1]] {
				t.Errorf("%s.md: %q: %q is not a subcommand of %s in usage", file, m[0], words[1], words[0])
			}
			all := strings.Join(synopses, " ")
			for _, w := range words[1:] {
				for _, flag := range regexp.MustCompile(`--[a-z][a-z-]*`).FindAllString(w, -1) {
					if !strings.Contains(all, flag) {
						t.Errorf("%s.md: %q: flag %s is not in the usage of %s", file, m[0], flag, words[0])
					}
				}
			}
		}
	}
}

func TestGuideCheatSheetListsEveryTopicFile(t *testing.T) {
	t.Parallel()
	index, err := guide.Index()
	if err != nil {
		t.Fatal(err)
	}
	_, list, ok := strings.Cut(index, "\nTopics:\n")
	if !ok {
		t.Fatal("index.md has no \"Topics:\" section")
	}
	var listed []string
	for _, m := range regexp.MustCompile(`shoalctl guide (\S+)`).FindAllStringSubmatch(list, -1) {
		listed = append(listed, m[1])
		if _, err := guide.Topic(m[1]); err != nil {
			t.Errorf("listed topic %q does not resolve: %v", m[1], err)
		}
	}
	entries, err := os.ReadDir(filepath.Join("..", "..", "docs", "guide"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".md"); ok && name != "index" && name != "quick" {
			files = append(files, name)
		}
	}
	sort.Strings(listed)
	sort.Strings(files)
	topics := append([]string(nil), guide.Topics...)
	sort.Strings(topics)
	if strings.Join(listed, ",") != strings.Join(files, ",") || strings.Join(topics, ",") != strings.Join(files, ",") {
		t.Errorf("topic files %v, guide.Topics %v and the cheat sheet's list %v differ", files, topics, listed)
	}
	if !strings.Contains(index, guide.ConfigPlaceholder) {
		t.Errorf("index.md lost the %s placeholder", guide.ConfigPlaceholder)
	}
}

func checkPlainScreen(t *testing.T, name, out string) {
	t.Helper()
	if out == "" || !strings.HasSuffix(out, "\n") {
		t.Errorf("%s: output is empty or does not end in a newline: %q", name, out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) > 40 {
		t.Errorf("%s: %d lines, more than one 40-line screen", name, len(lines))
	}
	for i, line := range lines {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("%s line %d: %d columns, over 80: %q", name, i+1, n, line)
		}
		for _, r := range line {
			if r < 0x20 || r > 0x7e {
				t.Errorf("%s line %d: %U is not printable ASCII (no ANSI, no emoji): %q", name, i+1, r, line)
			}
		}
	}
}

func TestGuideOutputIsPlainASCIIUnder80ColumnsAndOneScreen(t *testing.T) {
	t.Parallel()
	notRepo := newHarness(t)
	notRepo.env.Dir = t.TempDir()
	out := notRepo.mustRun("guide")
	if !strings.Contains(out, "not in a git repo: cd into your repo, then run shoalctl init first") {
		t.Errorf("guide outside a repo does not say to run init:\n%s", out)
	}
	checkPlainScreen(t, "guide (not a repo)", out)

	h := newHarness(t)
	out = h.mustRun("guide")
	if !strings.Contains(out, "not set up here yet: run shoalctl init first") {
		t.Errorf("guide in an uninitialized repo does not say to run init:\n%s", out)
	}
	checkPlainScreen(t, "guide (not initialized)", out)
	for _, topic := range guide.Topics {
		checkPlainScreen(t, "guide "+topic, h.mustRun("guide", topic))
	}
}

func TestGuidePrintsTheConfigPathOfAnInitializedRepo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.mustRun("init")
	want := "\n      " + filepath.Join(h.project().dir, "config.toml") + "\n"
	if out := h.mustRun("guide"); !strings.Contains(out, want) || strings.Contains(out, guide.ConfigPlaceholder) {
		t.Errorf("guide does not show the config path %q:\n%s", want, out)
	}
}

func TestGuideRefusesAnUnknownTopicOrExtraArguments(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, args := range [][]string{{"guide", "nope"}, {"guide", "setup", "tests"}, {"guide", "--help"}} {
		errOut := h.expectCode(ExitUsage, args...)
		if !strings.Contains(errOut, "topics: "+strings.Join(guide.Topics, ", ")) {
			t.Errorf("shoalctl %v: the usage error does not list the topics: %s", args, errOut)
		}
	}
}

func TestHelpPointsNewcomersAtTheGuide(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	out := h.mustRun("--help")
	if !strings.HasPrefix(out, "New here? Run: shoalctl guide\nusage: shoalctl <command> [args]\n") {
		t.Errorf("help does not open with the guide line:\n%s", out)
	}
	if !strings.Contains(out, "\n  guide [<topic>]") {
		t.Errorf("help does not list guide:\n%s", out)
	}
}

func TestGuideQuickFlowQuotesTheOpeningWordsStartSendsClaude(t *testing.T) {
	t.Parallel()
	quick, err := guide.Quick()
	if err != nil {
		t.Fatal(err)
	}
	opening, _, _ := strings.Cut(sessionPrompt, ". ")
	if !strings.Contains(quick, "\""+opening+"...\"") {
		t.Errorf("quick.md does not quote the opening words %q of the prompt start sends claude:\n%s", opening, quick)
	}
}
