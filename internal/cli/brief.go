package cli

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/kennethsolomon/shoalctl/internal/globx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// Token budgets of the injected context (§4.6). Budgets are checked with
// approxTokens and kept below the spec's ceilings to leave a margin.
const (
	briefBudget   = 900 // spec: <= 1k tokens
	perFileBudget = 280 // spec: <= 300 tokens per file
	briefTodo     = 8   // todo.md lines the brief shows
	briefLessons  = 5
)

// longRun is the length past which a run of letters and digits (a hash,
// base64, a minified identifier) stops being one BPE word.
const longRun = 16

// approxTokens is a conservative, offline token estimate: the largest of
// three counts. By bytes: one token per 3 bytes of UTF-8, rounded up, so
// scripts without spaces outside the CJK ranges (Thai, Lao, Khmer) and emoji
// runs are not undercounted. By characters: one token per 3, but one per Han, Hiragana,
// Katakana or Hangul character. By pieces: one per word, punctuation mark
// or CJK character, and one per 2 characters of a run longer than longRun.
// Claude's tokenizer averages about 3.5-4 characters per token on English
// and code, merges words and frequent punctuation, and spends about one
// token per CJK character and two to three characters per token of hex or
// base64, so both counts sit at or above the real one.
func approxTokens(s string) int {
	chars, cjk, pieces, run := 0, 0, 0, 0
	flush := func() {
		switch {
		case run > longRun:
			pieces += (run + 1) / 2
		case run > 0:
			pieces++
		}
		run = 0
	}
	for _, r := range s {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			flush()
			cjk++
			pieces++
		case unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_':
			chars++
			run++
		case unicode.IsSpace(r):
			flush()
			chars++
		default:
			flush()
			chars++
			pieces++
		}
	}
	flush()
	return max((chars+2)/3+cjk, pieces, (len(s)+2)/3)
}

// briefData is what the session-start brief shows (§4.6).
type briefData struct {
	task      store.Task
	maxRounds int
	todo      []string
	waitsOn   []string // tasks this one starts or merges after
	unread    int      // unread messages for shoal-lead
	lessons   []string // rendered lesson lines

	// The brainstorm (§11.5): proposed conventions and lessons proposed for
	// promotion. The agreed rules of context.md: active conventions (rules)
	// and promoted lessons (promotedRules). The hidden counts are what the
	// budget cut; the brief says how many and where to see them, never
	// drops them silently.
	proposed, promotions, rules, promotedRules    []string
	hiddenProposed, hiddenPromotions, hiddenRules int
}

// renderSessionBrief renders the brief inside briefBudget and returns how
// many lessons it kept. It drops, from the end: lessons, todo lines, the
// promotion proposals and proposed conventions past the first of each,
// active convention lines (they also arrive per file), promoted lesson
// lines (in stealth this brief is their only way in), then the last
// promotion proposal and proposed convention. Each dropped rule or
// proposal is counted in a "+N more" line. Agreed rules thus keep their
// space however many proposals wait, while the brainstorm still opens the
// brief. Then the text is cut.
func renderSessionBrief(d briefData) (string, int) {
	for {
		s := d.render()
		if approxTokens(s) <= briefBudget {
			return s, len(d.lessons)
		}
		switch {
		case len(d.lessons) > 0:
			d.lessons = d.lessons[:len(d.lessons)-1]
		case len(d.todo) > 0:
			d.todo = d.todo[:len(d.todo)-1]
		case len(d.promotions) > 1:
			d.promotions, d.hiddenPromotions = d.promotions[:len(d.promotions)-1], d.hiddenPromotions+1
		case len(d.proposed) > 1:
			d.proposed, d.hiddenProposed = d.proposed[:len(d.proposed)-1], d.hiddenProposed+1
		case len(d.rules) > 0:
			d.rules, d.hiddenRules = d.rules[:len(d.rules)-1], d.hiddenRules+1
		case len(d.promotedRules) > 0:
			d.promotedRules, d.hiddenRules = d.promotedRules[:len(d.promotedRules)-1], d.hiddenRules+1
		case len(d.promotions) > 0:
			d.promotions, d.hiddenPromotions = d.promotions[:len(d.promotions)-1], d.hiddenPromotions+1
		case len(d.proposed) > 0:
			d.proposed, d.hiddenProposed = d.proposed[:len(d.proposed)-1], d.hiddenProposed+1
		default:
			return fitTokens(s, briefBudget-5) + "\n[cut to fit]", len(d.lessons)
		}
	}
}

func (d briefData) render() string {
	t := d.task
	var b strings.Builder
	if n := len(d.proposed) + d.hiddenProposed + len(d.promotions) + d.hiddenPromotions; n > 0 {
		fmt.Fprintf(&b, "BEFORE ANYTHING ELSE, brainstorm with the human: %d proposed project rule(s) wait for a decision. "+
			"For each, discuss its scope (which files) and its exceptions with the human, and do not start the task until they are settled "+
			"or the human says to go on. Only the human decides: shoalctl convention agree <id> [--rule \"...\"] [--scope <glob>], "+
			"shoalctl convention retire <id> --why \"...\", or shoalctl promote lesson:<id>.\n", n)
		if len(d.proposed)+d.hiddenProposed > 0 {
			b.WriteString("Proposed conventions:\n")
			for _, l := range d.proposed {
				b.WriteString(l + "\n")
			}
			if d.hiddenProposed > 0 {
				fmt.Fprintf(&b, "- +%d more proposed: shoalctl convention list\n", d.hiddenProposed)
			}
		}
		if len(d.promotions)+d.hiddenPromotions > 0 {
			fmt.Fprintf(&b, "Lessons proposed as project rules (shown in %d+ tasks):\n", promoteTasks)
			for _, l := range d.promotions {
				b.WriteString(l + "\n")
			}
			if d.hiddenPromotions > 0 {
				fmt.Fprintf(&b, "- +%d more proposed for promotion: shoalctl convention list\n", d.hiddenPromotions)
			}
		}
	}
	fmt.Fprintf(&b, "shoal task %s (%s, stage %s, QA round %d of %d): %s\n", t.Slug, t.Lane, t.Stage, t.QARound, d.maxRounds, clip(t.Title, 120))
	b.WriteString("Task brief: .shoal/brief.md. Coordinate only through shoalctl.\n")
	if len(d.waitsOn) > 0 {
		verb := "merges after"
		if t.Stage == store.StageBlocked {
			verb = "starts after"
		}
		fmt.Fprintf(&b, "Blocked: %s %s.\n", verb, strings.Join(d.waitsOn, ", "))
	}
	if t.Stage == store.StageNeedsRebase {
		fmt.Fprintf(&b, "Needs rebase: a merge changed files this task claims; the next shoalctl qa %s rebases it.\n", t.Slug)
	}
	if t.Attention != "" {
		fmt.Fprintf(&b, "Needs attention: %s\n", clip(t.Attention, 120))
	}
	if d.unread > 0 {
		fmt.Fprintf(&b, "Unread messages for shoal-lead: %d (shoalctl msg inbox %s --as shoal-lead).\n", d.unread, t.Slug)
	}
	if len(d.rules)+len(d.promotedRules)+d.hiddenRules > 0 {
		b.WriteString("Project rules agreed with the human (follow them; the project's CLAUDE.md wins on a conflict):\n")
		for _, l := range append(slices.Clip(d.rules), d.promotedRules...) {
			b.WriteString(clip(l, 300) + "\n")
		}
		if d.hiddenRules > 0 {
			fmt.Fprintf(&b, "- +%d more rules: shoalctl convention list\n", d.hiddenRules)
		}
	}
	if len(d.todo) > 0 {
		b.WriteString("todo.md (head):\n")
		for _, l := range d.todo {
			b.WriteString("  " + clip(l, 160) + "\n")
		}
	}
	if len(d.lessons) > 0 {
		b.WriteString("Lessons from earlier tasks on your files. " + verifyNote + "\n")
		for _, l := range d.lessons {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// briefTodoLines returns the first non-blank lines of the worktree's todo.md.
func briefTodoLines(wt string, n int) ([]string, error) {
	f, err := os.Open(filepath.Join(wt, ".shoal", "todo.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read todo.md: %w", err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() && len(out) < n {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read todo.md: %w", err)
	}
	return out, nil
}

// taskLessons are the newest briefLessons injectable lessons for a task
// (newest first, ties by id): path_glob overlapping one of the task's open
// claims, or a stack-wide lesson of its stack.
func taskLessons(all []memo, claims []store.Claim, stack string) []memo {
	var out []memo
	for _, m := range all {
		if m.kind != store.KindLesson || !m.inject {
			continue
		}
		hit := m.glob == "" && stack != "" && m.stack == stack
		for _, c := range claims {
			if m.glob != "" && globx.Overlaps(m.glob, c.Glob) {
				hit = true
			}
		}
		if hit {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b memo) int {
		return cmp.Or(strings.Compare(b.created, a.created), cmp.Compare(b.id, a.id))
	})
	return out[:min(len(out), briefLessons)]
}

// sessionBrief gathers and renders the brief of t, and returns the keys of
// the lessons it shows. gitDir is the session's checkout: the rules of
// context.md are in the brief unless it is the main checkout and its
// CLAUDE.md imports them.
func sessionBrief(ctx context.Context, p *project, t store.Task, gitDir string) (string, []string, error) {
	d := briefData{task: t, maxRounds: p.cfg.Lanes.QAMaxRounds}
	var err error
	if t.Worktree != "" {
		if d.todo, err = briefTodoLines(t.Worktree, briefTodo); err != nil {
			return "", nil, err
		}
	}
	blockers, err := p.db.Blockers(ctx, t.ID)
	if err != nil {
		return "", nil, err
	}
	slugs, err := taskSlugs(ctx, p.db)
	if err != nil {
		return "", nil, err
	}
	for _, id := range blockers {
		d.waitsOn = append(d.waitsOn, slugs[id])
	}
	if d.unread, err = p.db.UnreadCount(ctx, t.ID, leadAgent); err != nil {
		return "", nil, err
	}
	all, err := memos(ctx, p.db)
	if err != nil {
		return "", nil, err
	}
	claims, err := p.db.Claims(ctx, t.ID)
	if err != nil {
		return "", nil, err
	}
	var keys []string
	for _, m := range taskLessons(all, claims, p.cfg.Stack.Name) {
		d.lessons = append(d.lessons, m.line(ctx, gitDir, slugs))
		keys = append(keys, m.ref)
	}
	rs, err := loadRuleState(ctx, p.db)
	if err != nil {
		return "", nil, err
	}
	for _, c := range rs.withStatus(store.ConventionProposed) {
		d.proposed = append(d.proposed, proposedLine(c, slugs))
	}
	for _, l := range rs.candidates() {
		d.promotions = append(d.promotions, candidateLine(l, rs.taskHits[l.ID]))
	}
	// A task worktree's import may be unapproved (Claude Code can ask again
	// in each new worktree, and an unattended session cannot answer), so its
	// brief always carries the rules; the main checkout relies on its block.
	if gitDir != p.root || !importsContext(gitDir) {
		d.rules, d.promotedRules = rs.conventionRuleLines(), rs.promotedRuleLines()
	}
	out, kept := renderSessionBrief(d)
	// Only lessons that survived the budget count as shown.
	return out, keys[:kept], nil
}

// cmdBrief prints the session-start brief of a task: the named one, or the
// one whose worktree this is.
func (env Env) cmdBrief(ctx context.Context, args []string) error {
	pos, err := parseArgsRange(flag.NewFlagSet("brief", flag.ContinueOnError), args, 0, 1)
	if err != nil {
		return err
	}
	ref := ""
	if len(pos) == 1 {
		ref = pos[0]
	} else {
		home, err := env.shoalHome()
		if err != nil {
			return err
		}
		wt, ok := shoalWorktree(env.Dir, home)
		if !ok {
			return fail(ExitUsage, "%s is not a shoal task worktree; name the task: shoalctl brief <task>", env.Dir)
		}
		tf, err := readTaskFile(wt)
		if err != nil {
			return err
		}
		ref = tf.Slug
	}
	p, t, err := env.openTask(ctx, ref)
	if err != nil {
		return err
	}
	defer p.close()
	dir := p.root
	if t.Worktree != "" && fileExists(t.Worktree) {
		dir = t.Worktree
	}
	out, _, err := sessionBrief(ctx, p, t, dir)
	if err != nil {
		return err
	}
	fmt.Fprint(env.Stdout, out)
	return nil
}
