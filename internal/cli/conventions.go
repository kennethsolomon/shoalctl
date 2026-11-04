package cli

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/kennethsolomon/shoalctl/internal/globx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// promoteTasks is how many different tasks a lesson must be shown in before
// shoal proposes it as a project rule (§11.4).
const promoteTasks = 3

// maxOpenProposals caps the proposed conventions of a project: past it,
// propose is refused until the human settles some (§16 row 64).
const maxOpenProposals = 10

func (p *project) contextPath() string { return filepath.Join(p.dir, "context.md") }

func scopeText(glob string) string {
	if glob == "" {
		return "the whole stack"
	}
	return glob
}

// wordAt is an @ that starts a word: what Claude Code reads as an import.
var wordAt = regexp.MustCompile(`(^|\s)@`)

// inert makes agent-written text safe to put in a file Claude Code loads
// as instructions (context.md, imported by the managed block): one line,
// no word-initial @ (an @path import) and no HTML comment marker (an
// unclosed <!-- would hide every later rule).
func inert(s string, n int) string {
	s = strings.NewReplacer("<!--", "< !--", "-->", "-- >").Replace(clip(s, n))
	return wordAt.ReplaceAllString(s, "${1}@ ")
}

// literal renders text as a markdown code span, which Claude Code does not
// read imports from and markdown does not parse: the fence is one backtick
// longer than the longest run inside.
func literal(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if longest > 0 {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

func scopeLiteral(glob string) string {
	if glob == "" {
		return scopeText(glob)
	}
	return literal(inert(glob, maxRecordText))
}

// conventionLine and promotedLine are the lines of context.md: every piece
// of recorded text is inert and in a code span (§16 row 70).
func conventionLine(c store.Convention) string {
	s := fmt.Sprintf("- %s (files %s): %s", store.ConventionKey(c.ID), scopeLiteral(c.ScopeGlob), literal(inert(c.Rule, maxRecordText)))
	if c.Conflict != "" {
		s += " (agreed although " + literal(inert(c.Conflict, 160)) + " seemed to say otherwise)"
	}
	return s
}

func promotedLine(l store.Lesson) string {
	return fmt.Sprintf("- %s (files %s): %s", store.LessonKey(l.ID), scopeLiteral(l.PathGlob), literal(inert(l.Pattern, 300)+" Fix: "+inert(l.Fix, 300)))
}

func proposedLine(c store.Convention, slugs map[int64]string) string {
	s := fmt.Sprintf("- %s (files %s", store.ConventionKey(c.ID), c.ScopeGlob)
	if c.SourceTaskID != 0 {
		s += ", proposed in task " + slugs[c.SourceTaskID]
	}
	if c.LessonID != 0 {
		s += ", from " + store.LessonKey(c.LessonID)
	}
	s += "): " + clip(c.Rule, 300)
	if c.Conflict != "" {
		s += " [conflicts with " + clip(c.Conflict, 160) + "; the project rule wins until the human changes it]"
	}
	return s
}

func candidateLine(l store.Lesson, tasks int) string {
	return fmt.Sprintf("- promote %s (files %s, shown in %d tasks): %s Fix: %s", store.LessonKey(l.ID), scopeText(l.PathGlob), tasks,
		clip(l.Pattern, 200), clip(l.Fix, 200))
}

// ruleState is everything the convention lifecycle shows, read once.
type ruleState struct {
	conventions []store.Convention
	lessons     []store.Lesson
	taskHits    map[int64]int // lesson id -> different tasks it was shown in
}

func loadRuleState(ctx context.Context, db *store.Store) (ruleState, error) {
	var s ruleState
	var err error
	if s.conventions, err = db.Conventions(ctx); err != nil {
		return s, err
	}
	if s.lessons, err = db.Lessons(ctx); err != nil {
		return s, err
	}
	s.taskHits, err = db.LessonTaskHits(ctx)
	return s, err
}

func (s ruleState) withStatus(status string) []store.Convention {
	var out []store.Convention
	for _, c := range s.conventions {
		if c.Status == status {
			out = append(out, c)
		}
	}
	return out
}

// carried reports whether a lesson's rule lives on in a convention: one
// active (agreed from the lesson), or one proposed or retired (its
// promotion is under discussion, or the human declined it).
func (s ruleState) carried(lessonID int64) (store.Convention, bool) {
	for _, c := range s.conventions {
		if c.LessonID == lessonID {
			return c, true
		}
	}
	return store.Convention{}, false
}

// promoted are the promoted lessons that context.md lists on their own:
// not stale, and not carried by an active convention.
func (s ruleState) promoted() []store.Lesson {
	var out []store.Lesson
	for _, l := range s.lessons {
		if c, ok := s.carried(l.ID); l.Promoted && !l.Stale && !(ok && c.Status == store.ConventionActive) {
			out = append(out, l)
		}
	}
	return out
}

// candidates are the lessons to propose for promotion (§11.4): shown in
// promoteTasks different tasks, not promoted, not stale, and not already
// discussed as a convention.
func (s ruleState) candidates() []store.Lesson {
	var out []store.Lesson
	for _, l := range s.lessons {
		if _, ok := s.carried(l.ID); !l.Promoted && !l.Stale && !ok && s.taskHits[l.ID] >= promoteTasks {
			out = append(out, l)
		}
	}
	return out
}

// contextRuleLines are the agreed rules every session follows: active
// conventions, then promoted lessons, oldest first.
func (s ruleState) contextRuleLines() []string {
	return append(s.conventionRuleLines(), s.promotedRuleLines()...)
}

func (s ruleState) conventionRuleLines() []string {
	var out []string
	for _, c := range s.withStatus(store.ConventionActive) {
		out = append(out, conventionLine(c))
	}
	return out
}

func (s ruleState) promotedRuleLines() []string {
	var out []string
	for _, l := range s.promoted() {
		out = append(out, promotedLine(l))
	}
	return out
}

const contextHeader = "<!-- Written by shoalctl from memory.db; edits here are overwritten. Change the rules with shoalctl convention and shoalctl promote. -->\n" +
	"# Project rules from shoal\n\n" +
	"Conventions agreed with the human (each applies to the files in its scope) and lessons promoted after they recurred in " +
	"3 different tasks. The project's own CLAUDE.md rules win over these.\n\n"

// renderContext writes context.md (§4.1) from memory.db: deterministic,
// and atomic (temp file and rename), so a session never reads half a file.
func renderContext(ctx context.Context, p *project) error {
	s, err := loadRuleState(ctx, p.db)
	if err != nil {
		return err
	}
	body := contextHeader
	lines := s.contextRuleLines()
	if len(lines) == 0 {
		body += "No agreed conventions or promoted lessons yet.\n"
	} else {
		body += strings.Join(lines, "\n") + "\n"
	}
	return writeFileAtomic(p.contextPath(), []byte(body), 0o600)
}

// conflictOf checks text against the project's own rules (§5.3).
func conflictOf(p *project, text string) (string, error) {
	rules, err := readProjectRules(p.root)
	if err != nil {
		return "", err
	}
	return conflictWith(text, rules), nil
}

// reconcileRules runs the §5.3 check over what is already agreed: an active
// convention a project rule now contradicts goes back to proposed, and so
// does the rule of a promoted lesson. Agreements the human made over a
// flagged conflict (agree --force) stay. It returns one line per change.
func reconcileRules(ctx context.Context, p *project) ([]string, error) {
	rules, err := readProjectRules(p.root)
	if err != nil {
		return nil, err
	}
	s, err := loadRuleState(ctx, p.db)
	if err != nil {
		return nil, err
	}
	var notes []string
	for _, c := range s.withStatus(store.ConventionActive) {
		if c.Conflict != "" {
			continue
		}
		if conflict := conflictWith(c.Rule, rules); conflict != "" {
			if err := p.db.ReopenConvention(ctx, c.ID, conflict); err != nil {
				return nil, err
			}
			notes = append(notes, fmt.Sprintf("%s conflicts with %s: back to proposed, the project rule wins", store.ConventionKey(c.ID), conflict))
		}
	}
	for _, l := range s.promoted() {
		if conflict := conflictWith(l.Fix, rules); conflict != "" {
			id, err := p.db.ProposeFromPromotedLesson(ctx, store.Convention{Rule: lessonRule(l), ScopeGlob: lessonScope(l), Conflict: conflict, LessonID: l.ID})
			if err != nil {
				return nil, err
			}
			notes = append(notes, fmt.Sprintf("%s conflicts with %s: unpromoted and proposed as %s, the project rule wins",
				store.LessonKey(l.ID), conflict, store.ConventionKey(id)))
		}
	}
	return notes, nil
}

func lessonRule(l store.Lesson) string { return l.Pattern + " Fix: " + l.Fix }

func lessonScope(l store.Lesson) string {
	if l.PathGlob == "" {
		return "**"
	}
	return l.PathGlob
}

func scopeFlag(name, v string) (string, error) {
	v = strings.TrimSpace(v)
	if err := globx.Validate(v); err != nil {
		return "", fail(ExitUsage, "--%s: %v", name, err)
	}
	return v, nil
}

func (env Env) cmdConvention(ctx context.Context, args []string) error {
	const usageLine = "usage: convention propose|agree|retire|list ...; see shoalctl help"
	if len(args) == 0 {
		return fail(ExitUsage, usageLine)
	}
	switch args[0] {
	case "propose":
		return env.conventionPropose(ctx, args[1:])
	case "agree":
		return env.conventionAgree(ctx, args[1:])
	case "retire":
		return env.conventionRetire(ctx, args[1:])
	case "list":
		return env.conventionList(ctx, args[1:])
	}
	return fail(ExitUsage, "unknown convention command %q; %s", args[0], usageLine)
}

// conventionPropose records a rule an agent or the human wants from now on
// (§11.5 step 1). Anyone may propose; only the human agrees.
func (env Env) conventionPropose(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("convention propose", flag.ContinueOnError)
	rule := fs.String("rule", "", "the rule, as it should be followed")
	scope := fs.String("scope", "", "the files it applies to, a glob (** for every file)")
	pos, err := parseArgsRange(fs, args, 0, 1)
	if err != nil {
		return err
	}
	text, err := recordText("rule", *rule)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*scope) == "" {
		return fail(ExitUsage, "--scope is required: the glob of the files the rule applies to (** for every file)")
	}
	glob, err := scopeFlag("scope", *scope)
	if err != nil {
		return err
	}
	var p *project
	var source int64
	if len(pos) == 1 {
		var t store.Task
		if p, t, err = env.openTask(ctx, pos[0]); err != nil {
			return err
		}
		source = t.ID
	} else if p, err = env.openProject(ctx); err != nil {
		return err
	}
	defer p.close()
	conflict, err := conflictOf(p, text)
	if err != nil {
		return err
	}
	id, existing, err := p.db.ProposeConvention(ctx, store.Convention{Rule: text, ScopeGlob: glob, Conflict: conflict, SourceTaskID: source}, maxOpenProposals)
	if err != nil {
		return err
	}
	key := store.ConventionKey(id)
	switch {
	case existing:
		fmt.Fprintf(env.Stdout, "%s already holds this rule; nothing added\n", key)
	case conflict != "":
		fmt.Fprintf(env.Stdout, "proposed %s; it conflicts with %s, and the project rule wins until the human changes it. "+
			"The next session opens with a brainstorm about it\n", key, conflict)
	default:
		fmt.Fprintf(env.Stdout, "proposed %s; the next session opens with a brainstorm, and the human agrees with shoalctl convention agree %d\n", key, id)
	}
	return nil
}

// conventionAgree is the human's yes after the brainstorm (§11.5 step 3),
// with the rule and scope as agreed.
func (env Env) conventionAgree(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("convention agree", flag.ContinueOnError)
	rule := fs.String("rule", "", "the rule as agreed (default: as proposed)")
	scope := fs.String("scope", "", "the scope as agreed (default: as proposed)")
	force := fs.Bool("force", false, "agree although shoal's check finds a project rule that contradicts it")
	pos, err := parseArgs(fs, args, 1)
	if err != nil {
		return err
	}
	id, err := store.ParseConventionRef(pos[0])
	if err != nil {
		return fail(ExitUsage, "%v", err)
	}
	if err := env.requireHuman("convention agree"); err != nil {
		return err
	}
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	c, err := p.db.GetConvention(ctx, id)
	if err != nil {
		return err
	}
	a := store.Agreement{Rule: c.Rule, ScopeGlob: c.ScopeGlob, By: humanName(ctx, p.root)}
	if *rule != "" {
		if a.Rule, err = recordText("rule", *rule); err != nil {
			return err
		}
	}
	if *scope != "" {
		if a.ScopeGlob, err = scopeFlag("scope", *scope); err != nil {
			return err
		}
	}
	check := a.Rule
	if c.LessonID != 0 && *rule == "" {
		l, err := lessonByID(ctx, p.db, c.LessonID)
		if err != nil {
			return err
		}
		check = l.Fix
	}
	if a.Conflict, err = conflictOf(p, check); err != nil {
		return err
	}
	if a.Conflict != "" && !*force {
		return fail(ExitState, "%s conflicts with %s, and project rules win: change that rule first, retire this proposal "+
			"(shoalctl convention retire %d --why \"...\"), or, if the match is wrong, agree with --force", store.ConventionKey(id), a.Conflict, id)
	}
	line := conventionLine(store.Convention{ID: id, Rule: a.Rule, ScopeGlob: a.ScopeGlob, Conflict: a.Conflict})
	if ok, err := env.confirm(fmt.Sprintf("%s becomes a project rule, written to %s as:\n%s\nAgree? [y/N]: ", store.ConventionKey(id), p.contextPath(), line)); err != nil || !ok {
		return cmp.Or(err, fail(ExitState, "%s not agreed; nothing changed", store.ConventionKey(id)))
	}
	if err := p.db.AgreeConvention(ctx, id, a); err != nil {
		return err
	}
	if err := renderContext(ctx, p); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s is active for %s; injected when those files are touched and written to %s\n", store.ConventionKey(id), a.ScopeGlob, p.contextPath())
	return nil
}

// confirm prints prompt and reports whether the human answered y.
func (env Env) confirm(prompt string) (bool, error) {
	fmt.Fprint(env.Stdout, prompt)
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read the answer: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(line), "y"), nil
}

func lessonByID(ctx context.Context, db *store.Store, id int64) (store.Lesson, error) {
	ls, err := db.Lessons(ctx)
	if err != nil {
		return store.Lesson{}, err
	}
	for _, l := range ls {
		if l.ID == id {
			return l, nil
		}
	}
	return store.Lesson{}, fmt.Errorf("lesson:%d: %w", id, store.ErrNotFound)
}

// conventionRetire ends a convention, or declines a proposal; the record
// keeps why (§11.5 step 4).
func (env Env) conventionRetire(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("convention retire", flag.ContinueOnError)
	why := fs.String("why", "", "why it no longer applies; kept with the record")
	pos, err := parseArgs(fs, args, 1)
	if err != nil {
		return err
	}
	id, err := store.ParseConventionRef(pos[0])
	if err != nil {
		return fail(ExitUsage, "%v", err)
	}
	reason, err := recordText("why", *why)
	if err != nil {
		return err
	}
	if err := env.requireHuman("convention retire"); err != nil {
		return err
	}
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	if err := p.db.RetireConvention(ctx, id, humanName(ctx, p.root), reason); err != nil {
		return err
	}
	if err := renderContext(ctx, p); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s retired; no longer injected, still found by recall with its reason\n", store.ConventionKey(id))
	return nil
}

func (env Env) conventionList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("convention list", flag.ContinueOnError)
	all := fs.Bool("all", false, "also list retired conventions and why")
	if _, err := parseArgs(fs, args, 0); err != nil {
		return err
	}
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	s, err := loadRuleState(ctx, p.db)
	if err != nil {
		return err
	}
	slugs, err := taskSlugs(ctx, p.db)
	if err != nil {
		return err
	}
	w := env.Stdout
	section := func(title string, lines []string) {
		if len(lines) > 0 {
			fmt.Fprintf(w, "%s:\n%s\n", title, strings.Join(lines, "\n"))
		}
	}
	var proposed, candidates, retired []string
	for _, c := range s.withStatus(store.ConventionProposed) {
		proposed = append(proposed, proposedLine(c, slugs))
	}
	for _, l := range s.candidates() {
		candidates = append(candidates, candidateLine(l, s.taskHits[l.ID]))
	}
	for _, c := range s.withStatus(store.ConventionRetired) {
		retired = append(retired, fmt.Sprintf("- %s (files %s): %s [retired by %s at %s: %s]", store.ConventionKey(c.ID), c.ScopeGlob,
			clip(c.Rule, maxRecordText), c.RetiredBy, c.RetiredAt, c.RetireReason))
	}
	section("Proposed (to brainstorm with the human)", proposed)
	section("Lessons proposed for promotion (shoalctl promote lesson:<id>)", candidates)
	section("Project rules ("+p.contextPath()+")", s.contextRuleLines())
	if *all {
		section("Retired", retired)
	}
	if len(proposed)+len(candidates)+len(s.contextRuleLines()) == 0 && (!*all || len(retired) == 0) {
		fmt.Fprintln(w, "no conventions yet")
	}
	return nil
}

// cmdPromote is the human's approval of one lesson promotion (§11.4): the
// rule goes into context.md, promoted = 1, and its per-file injection
// stops. A lesson a project rule contradicts is not promoted: it becomes a
// proposed convention with the conflict, for the brainstorm (§5.3).
func (env Env) cmdPromote(ctx context.Context, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("promote", flag.ContinueOnError), args, 1)
	if err != nil {
		return err
	}
	kind, id, err := store.ParseRecordRef(pos[0])
	if err != nil || kind != store.KindLesson {
		return fail(ExitUsage, "promote takes lesson:<id>, got %q", pos[0])
	}
	if err := env.requireHuman("promote"); err != nil {
		return err
	}
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	l, err := lessonByID(ctx, p.db, id)
	if err != nil {
		return err
	}
	if ok, err := env.confirm(fmt.Sprintf("%s becomes a project rule, written to %s as:\n%s\nPromote it into context.md? [y/N]: ",
		store.LessonKey(id), p.contextPath(), promotedLine(l))); err != nil || !ok {
		return cmp.Or(err, fail(ExitState, "%s not promoted; nothing changed", store.LessonKey(id)))
	}
	conflict, err := conflictOf(p, l.Fix)
	if err != nil {
		return err
	}
	if conflict != "" {
		s, err := loadRuleState(ctx, p.db)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(s.candidates(), func(c store.Lesson) bool { return c.ID == id }) {
			return fail(ExitState, "lesson:%d is not up for promotion: it must be shown in %d different tasks (now %d), not stale, not promoted, "+
				"and not already a convention", id, promoteTasks, s.taskHits[id])
		}
		cid, _, err := p.db.ProposeConvention(ctx, store.Convention{Rule: lessonRule(l), ScopeGlob: lessonScope(l), Conflict: conflict, LessonID: l.ID}, 0)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "lesson:%d not promoted: it conflicts with %s, and the project rule wins. Recorded as proposed %s for the next brainstorm\n",
			id, conflict, store.ConventionKey(cid))
		return nil
	}
	if err := p.db.PromoteLesson(ctx, id, promoteTasks); err != nil {
		return err
	}
	if err := renderContext(ctx, p); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "lesson:%d promoted into %s; it is no longer injected per file\n", id, p.contextPath())
	return nil
}
