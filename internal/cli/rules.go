package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// projectRule is one line of the project's own instructions to Claude:
// CLAUDE.md, .claude/CLAUDE.md, CLAUDE.local.md or a file under
// .claude/rules/ (§5.3).
type projectRule struct {
	where string // "CLAUDE.md:12"
	text  string
}

// readProjectRules reads every rule line of the main checkout at root: the
// non-blank lines of CLAUDE.md, .claude/CLAUDE.md, CLAUDE.local.md and
// .claude/rules/**/*.md,
// outside code fences and outside shoal's managed block. Missing files are
// no rules.
func readProjectRules(root string) ([]projectRule, error) {
	files := []string{"CLAUDE.md", ".claude/CLAUDE.md", "CLAUDE.local.md"}
	rulesDir := filepath.Join(root, ".claude", "rules")
	err := filepath.WalkDir(rulesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".md") {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read project rules in %s: %w", rulesDir, err)
	}
	var out []projectRule
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read project rules: %w", err)
		}
		out = append(out, ruleLines(f, data)...)
	}
	return out, nil
}

// listMarker is a line's markdown prefix: heading, quote, bullet or number.
var listMarker = regexp.MustCompile(`^(?:[#>*+\-\s]+|\d+[.)]\s+)+`)

// fences tracks markdown code fences line by line, per CommonMark §4.5. An
// opener is 0-3 spaces then 3+ backticks or tildes (a backtick opener's
// info string holds no backtick); the closer is the same character, at
// least as long, after 0-3 spaces, with only whitespace after it. A fence
// left open runs to the end of the file. A fence may also open right after
// a list marker (`-`, `*`, `+`, `1.`, `1)` then 1-4 spaces, after 0-3
// spaces): it closes at a matching closer indented 0-3 columns past the
// item's content, or ends without one at the first non-blank line indented
// less than that content. Blockquotes and nested list markers are not
// tracked. Text inside is an example, never a rule or an import.
type fences struct {
	open int  // line number of the open fence; 0 when outside
	char byte // '`' or '~' of the open fence
	size int  // length of the open fence's run
	item int  // content column of a list-item fence; 0 for a top-level one
}

// listItemFence matches a list marker that a fence may open right after.
var listItemFence = regexp.MustCompile("^ {0,3}(?:[-*+]|[0-9]{1,9}[.)]) {1,4}[`~]")

// skip reports whether line l (line number n, with or without its line
// ending or a byte order mark) is a fence line or inside a fence.
func (f *fences) skip(l []byte, n int) bool {
	s := strings.TrimRight(strings.TrimPrefix(string(l), utf8BOM), "\r\n")
	if f.open != 0 && f.item > 0 && strings.TrimSpace(s) != "" && indentWidth(s) < f.item {
		f.open = 0 // the list item, and its fence, ended before this line
	}
	if f.open == 0 {
		ch, size, rest, ok := fenceRun(s)
		item := 0
		if !ok {
			if m := listItemFence.FindString(s); m != "" {
				item = len(m) - 1
				ch, size, rest, ok = fenceRun(s[item:])
			}
		}
		if !ok || ch == '`' && strings.ContainsRune(rest, '`') {
			return false
		}
		f.open, f.char, f.size, f.item = n, ch, size, item
		return true
	}
	if f.item > 0 {
		if w := indentWidth(s); w > f.item+3 {
			return true
		}
		s = strings.TrimLeft(s, " \t")
	}
	if ch, size, rest, ok := fenceRun(s); ok && ch == f.char && size >= f.size && strings.TrimSpace(rest) == "" {
		f.open = 0
	}
	return true
}

// indentWidth is the column of s's first non-blank character, a tab
// advancing to the next multiple of 4.
func indentWidth(s string) int {
	w := 0
	for _, c := range []byte(s) {
		switch c {
		case ' ':
			w++
		case '\t':
			w += 4 - w%4
		default:
			return w
		}
	}
	return w
}

// fenceRun splits a line that starts, after at most 3 spaces, with a run
// of 3+ backticks or tildes into the run's character, its length and the
// rest of the line.
func fenceRun(l string) (byte, int, string, bool) {
	indent := 0
	for indent < len(l) && l[indent] == ' ' {
		indent++
	}
	if indent > 3 || indent == len(l) || l[indent] != '`' && l[indent] != '~' {
		return 0, 0, "", false
	}
	l = l[indent:]
	size := 0
	for size < len(l) && l[size] == l[0] {
		size++
	}
	if size < 3 {
		return 0, 0, "", false
	}
	return l[0], size, l[size:], true
}

// ruleLines returns the rule lines of one file. Lines between a column-0
// begin marker and its end are shoal's own block; after a begin with no
// end the held lines are rules again, so an unpaired marker hides nothing
// from the conflict check.
func ruleLines(name string, data []byte) []projectRule {
	var out, held []projectRule
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var fence fences
	managed := false
	for n := 1; sc.Scan(); n++ {
		if fence.skip(sc.Bytes(), n) {
			continue
		}
		switch markerText(sc.Bytes(), n) {
		case blockBegin:
			out, held, managed = append(out, held...), nil, true
			continue
		case blockEnd:
			held, managed = nil, false
			continue
		}
		l := listMarker.ReplaceAllString(lineText(sc.Bytes()), "")
		if l == "" {
			continue
		}
		r := projectRule{where: fmt.Sprintf("%s:%d", name, n), text: l}
		if managed {
			held = append(held, r)
		} else {
			out = append(out, r)
		}
	}
	return append(out, held...)
}

// conflictWith returns the first project rule that contradicts text, as
// "<file>:<line>: <rule>", or "" when none does. The check is cheap and
// deterministic (no LLM) and deliberately conservative: text and rule are
// split into clauses, and two clauses contradict when exactly one of them
// is negated (never, not, don't, avoid, ...) and they share at least two
// content words, at least half of the shorter clause's, or one when both
// name a single thing or the negated clause names only one ("no tabs",
// "spaces, not tabs"). Paraphrases, synonyms and implied negation ("prefer
// A over B") are not caught.
func conflictWith(text string, rules []projectRule) string {
	for _, a := range clauses(text) {
		for _, r := range rules {
			for _, b := range clauses(r.text) {
				if a.negated != b.negated && (sameSubject(a.words, b.words) || negatesOneWord(a, b) || negatesOneWord(b, a)) {
					return r.where + ": " + clip(r.text, 200)
				}
			}
		}
	}
	return ""
}

type clause struct {
	negated bool
	words   map[string]bool
}

var negations = map[string]bool{"never": true, "not": true, "no": true, "avoid": true, "without": true, "forbid": true,
	"forbidden": true, "disallow": true, "disallowed": true, "cannot": true, "nor": true, "stop": true, "ban": true, "banned": true}

// fillers are words that say nothing about what a rule is about.
var fillers = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an the and or but of to in on at for by with from into onto over under as is are be been being was were
		it its this that these those there here then than so such any all each every some only also just very more most less
		do does did done doing use used uses using prefer always must should shall will would can could may might need needs
		make makes made keep keeps write writes add adds put go get set new old one ever instead when where what which who how why
		if else we you they i our your their them us me my file files code thing things way ways rule rules`) {
		m[w] = true
	}
	return m
}()

func clauses(s string) []clause {
	s = strings.ToLower(strings.NewReplacer("’", "'", "n't", " not").Replace(s))
	var out []clause
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(".;:!?,()", r) }) {
		for _, piece := range strings.Split(part, " but ") {
			c := clause{words: map[string]bool{}}
			for _, w := range strings.FieldsFunc(piece, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
				switch {
				case negations[w]:
					c.negated = true
				case !fillers[w] && len(w) > 1:
					c.words[stem(w)] = true
				}
			}
			if len(c.words) > 0 {
				out = append(out, c)
			}
		}
	}
	return out
}

// stem folds a plural onto its singular, enough for "patterns"/"pattern".
func stem(w string) string {
	if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		return w[:len(w)-1]
	}
	return w
}

// negatesOneWord reports whether n is a negated clause of one content word
// ("no tabs", the "not tabs" of "spaces, not tabs") that the other clause
// names.
func negatesOneWord(n, other clause) bool {
	if !n.negated || len(n.words) != 1 {
		return false
	}
	for w := range n.words {
		return other.words[w]
	}
	return false
}

func sameSubject(a, b map[string]bool) bool {
	shared := 0
	for w := range a {
		if b[w] {
			shared++
		}
	}
	if len(a) == 1 && len(b) == 1 {
		return shared == 1
	}
	return shared >= 2 && 2*shared >= min(len(a), len(b))
}
