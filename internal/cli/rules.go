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
// CLAUDE.md, CLAUDE.local.md or a file under .claude/rules/ (§5.3).
type projectRule struct {
	where string // "CLAUDE.md:12"
	text  string
}

// readProjectRules reads every rule line of the main checkout at root: the
// non-blank lines of CLAUDE.md, CLAUDE.local.md and .claude/rules/**/*.md,
// outside code fences and outside shoal's managed block. Missing files are
// no rules.
func readProjectRules(root string) ([]projectRule, error) {
	files := []string{"CLAUDE.md", "CLAUDE.local.md"}
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

// fences tracks markdown code fences line by line: a trimmed line starting
// with ``` or ~~~ opens or closes one. Text inside is an example, never a
// rule, a marker or an import.
type fences struct {
	open int // line number of the open fence; 0 when outside
}

// skip reports whether the trimmed line l (line number n) is a fence line
// or inside a fence.
func (f *fences) skip(l string, n int) bool {
	if strings.HasPrefix(l, "```") || strings.HasPrefix(l, "~~~") {
		if f.open == 0 {
			f.open = n
		} else {
			f.open = 0
		}
		return true
	}
	return f.open != 0
}

func ruleLines(name string, data []byte) []projectRule {
	var out []projectRule
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var fence fences
	managed := false
	for n := 1; sc.Scan(); n++ {
		l := strings.TrimSpace(sc.Text())
		if fence.skip(l, n) {
			continue
		}
		switch {
		case l == blockBegin:
			managed = true
			continue
		case l == blockEnd:
			managed = false
			continue
		}
		l = listMarker.ReplaceAllString(l, "")
		if managed || l == "" {
			continue
		}
		out = append(out, projectRule{where: fmt.Sprintf("%s:%d", name, n), text: l})
	}
	return out
}

// conflictWith returns the first project rule that contradicts text, as
// "<file>:<line>: <rule>", or "" when none does. The check is cheap and
// deterministic (no LLM) and deliberately conservative: text and rule are
// split into clauses, and two clauses contradict when exactly one of them
// is negated (never, not, don't, avoid, ...) and they share at least two
// content words (one when both name a single thing), at least half of the
// shorter clause's. Paraphrases, synonyms and implied negation ("prefer A
// over B") are not caught.
func conflictWith(text string, rules []projectRule) string {
	for _, a := range clauses(text) {
		for _, r := range rules {
			for _, b := range clauses(r.text) {
				if a.negated != b.negated && sameSubject(a.words, b.words) {
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
