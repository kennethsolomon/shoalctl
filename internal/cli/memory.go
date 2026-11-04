package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/globx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// lessonDecay: a lesson not hit for this long is no longer injected (§11.4).
const lessonDecay = 90 * 24 * time.Hour

// maxRecordText bounds each text field of a record, so injected records
// stay inside the hook budgets.
const maxRecordText = 500

// verifyNote travels with every record shoal shows an agent (§11.1).
const verifyNote = "Each record holds what was true at its commit: verify it against the current code before relying on it. " +
	"If the code disagrees, the code wins: run shoalctl flag-stale <record> \"<why>\"."

// memo is one memory record, of any kind, as why, recall and the hooks show it.
type memo struct {
	ref     string // "lesson:3", or "summary:<slug>"
	kind    string
	id      int64
	taskID  int64
	sha     string   // true_at_sha
	files   []string // repo paths the record is about
	commits []string // a summary's commits
	glob    string   // a lesson's path glob ("" for every other kind)
	stack   string   // a lesson's stack
	text    string
	stale   string // why it was flagged stale; "" when it is not
	status  string // a convention's status
	inject  bool   // hooks may inject it
	lastHit string
	created string
}

func clip(s string, n int) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), n)
}

// memos loads every record of the project. The tables are small (one row
// per task, QA failure or answered question), so filtering happens here.
func memos(ctx context.Context, db *store.Store) ([]memo, error) {
	var out []memo
	// Conventions come first: the human agreed to them, so they win the
	// per-file budget.
	conventions, err := db.Conventions(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range conventions {
		m := memo{ref: store.ConventionKey(c.ID), kind: store.KindConvention, id: c.ID, taskID: c.SourceTaskID, glob: c.ScopeGlob,
			text: clip(c.Rule, maxRecordText), inject: c.Status == store.ConventionActive, status: c.Status, created: c.ProposedAt}
		switch {
		case c.Status == store.ConventionRetired:
			m.text += " [retired: " + clip(c.RetireReason, 200) + "]"
		case c.Conflict != "" && c.Status == store.ConventionProposed:
			m.text += " [conflicts with " + clip(c.Conflict, 160) + "]"
		}
		out = append(out, m)
	}
	bugs, err := db.Bugs(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range bugs {
		var tests []string
		for _, rt := range b.Tests {
			tests = append(tests, rt.Test)
		}
		text := fmt.Sprintf("%s. Root cause: %s. Fixed in %s", clip(b.Symptom, 200), clip(b.RootCause, 300), shortSHA(b.FixSHA))
		if len(tests) > 0 {
			text += "; regression test " + strings.Join(tests, ", ")
		}
		out = append(out, memo{ref: fmt.Sprintf("bug:%d", b.ID), kind: store.KindBug, id: b.ID, taskID: b.TaskID, sha: b.TrueAtSHA, files: b.Files,
			text: text, stale: staleReason(b.Stale, b.StaleReason), inject: !b.Stale, created: b.CreatedAt})
	}
	decisions, err := db.Decisions(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range decisions {
		m := memo{ref: fmt.Sprintf("decision:%d", d.ID), kind: store.KindDecision, id: d.ID, taskID: d.TaskID, sha: d.TrueAtSHA, files: d.Files,
			text:  fmt.Sprintf("%s: %s. Why: %s", clip(d.Topic, 100), clip(d.Choice, 200), clip(d.Rationale, 300)),
			stale: staleReason(d.Stale, d.StaleReason), inject: !d.Stale && d.SupersededBy == 0, created: d.CreatedAt}
		if d.SupersededBy != 0 {
			m.text += fmt.Sprintf(" (superseded by decision:%d)", d.SupersededBy)
		}
		out = append(out, m)
	}
	lessons, err := db.Lessons(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range lessons {
		out = append(out, memo{ref: store.LessonKey(l.ID), kind: store.KindLesson, id: l.ID, taskID: l.TaskID, sha: l.TrueAtSHA, glob: l.PathGlob,
			stack: l.Stack, text: fmt.Sprintf("%s Fix: %s", clip(l.Pattern, 300), clip(l.Fix, 300)),
			stale: staleReason(l.Stale, l.StaleReason), inject: !l.Stale && !l.Promoted && !decayed(l), lastHit: l.LastHitAt, created: l.CreatedAt})
	}
	notes, err := db.QANotes(ctx)
	if err != nil {
		return nil, err
	}
	for _, q := range notes {
		out = append(out, memo{ref: fmt.Sprintf("qa:%d", q.ID), kind: store.KindQA, id: q.ID, taskID: q.TaskID, sha: q.TrueAtSHA, files: q.Files,
			text: fmt.Sprintf("Q: %s A: %s", clip(q.Question, 200), clip(q.Answer, 300)), stale: staleReason(q.Stale, q.StaleReason), created: q.CreatedAt})
	}
	sums, err := db.SummaryRecords(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range sums {
		out = append(out, memo{ref: "summary:" + s.Slug, kind: store.KindSummary, id: s.TaskID, taskID: s.TaskID, sha: s.TrueAtSHA, files: s.Files, commits: s.Commits,
			text: fmt.Sprintf("%s: %s. Why: %s", clip(s.Title, 100), clip(s.Decision, 200), clip(s.Why, 300))})
	}
	return out, nil
}

func staleReason(stale bool, reason string) string {
	if !stale {
		return ""
	}
	if reason == "" {
		return "flagged stale"
	}
	return reason
}

// decayed reports whether a lesson has gone unused past lessonDecay.
func decayed(l store.Lesson) bool {
	last := l.LastHitAt
	if last == "" {
		last = l.CreatedAt
	}
	at, err := time.Parse(time.RFC3339, last)
	return err == nil && time.Since(at) > lessonDecay
}

// about reports whether the record concerns the repo path rel.
func (m memo) about(rel string) bool {
	if m.glob != "" && globx.Overlaps(m.glob, rel) {
		return true
	}
	for _, f := range m.files {
		if globx.Overlaps(f, rel) {
			return true
		}
	}
	return false
}

// pathspecs are the record's paths for git, matched as written.
func (m memo) pathspecs() []string {
	var specs []string
	if m.glob != "" {
		specs = append(specs, ":(glob)"+m.glob)
	}
	for _, f := range m.files {
		specs = append(specs, ":(literal)"+f)
	}
	return specs
}

// stalenessTag is the cheap pre-check of §11.2: git diff --quiet between
// the record's commit and HEAD in dir, over the record's paths.
func stalenessTag(ctx context.Context, dir string, m memo) string {
	specs := m.pathspecs()
	if m.sha == "" || len(specs) == 0 {
		return ""
	}
	changed, err := gitx.Changed(ctx, dir, m.sha, specs)
	switch {
	case err != nil:
		return " [cannot check: " + shortSHA(m.sha) + " is not in this checkout]"
	case changed:
		return " [files changed since record]"
	}
	return ""
}

// line renders a record on one line for an agent or a human.
func (m memo) line(ctx context.Context, dir string, slugs map[int64]string) string {
	if m.kind == store.KindConvention {
		// A convention is a rule from now on, not a fact at a commit.
		return fmt.Sprintf("- %s (%s convention, files %s): %s", m.ref, m.status, m.glob, m.text)
	}
	where := slugs[m.taskID]
	if where == "" {
		where = strconv.FormatInt(m.taskID, 10)
	}
	s := fmt.Sprintf("- %s (task %s, at %s)", m.ref, where, shortSHA(m.sha))
	if m.glob != "" {
		s += " on " + m.glob
	}
	s += stalenessTag(ctx, dir, m)
	if m.stale != "" {
		s += " [stale: " + clip(m.stale, 100) + "]"
	}
	return s + ": " + m.text
}

func taskSlugs(ctx context.Context, db *store.Store) (map[int64]string, error) {
	tasks, err := db.ListTasks(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(tasks))
	for _, t := range tasks {
		out[t.ID] = t.Slug
	}
	return out, nil
}

// recordText validates one text flag of a record.
func recordText(name, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fail(ExitUsage, "--%s is required", name)
	}
	if n := utf8.RuneCountInString(v); n > maxRecordText {
		return "", fail(ExitUsage, "--%s is %d characters; keep it under %d and point at files instead of pasting them", name, n, maxRecordText)
	}
	return v, nil
}

// recordFiles validates --file values: repo-relative paths, as claims are.
func recordFiles(files []string) ([]string, error) {
	var out []string
	for _, f := range files {
		f = strings.TrimSpace(f)
		if err := globx.Validate(f); err != nil {
			return nil, fail(ExitUsage, "--file: %v", err)
		}
		if strings.ContainsAny(f, "*?[") {
			return nil, fail(ExitUsage, "--file %q must be a path, not a glob (a lesson takes --glob)", f)
		}
		if !contains(out, f) {
			out = append(out, f)
		}
	}
	return out, nil
}

// recordSHA is the commit a new record is true at: the task's HEAD, else
// its base, else the main checkout's HEAD.
func recordSHA(ctx context.Context, p *project, t store.Task) (string, error) {
	if t.Worktree != "" && fileExists(t.Worktree) {
		return gitx.Run(ctx, t.Worktree, "rev-parse", "HEAD")
	}
	if t.BaseSHA != "" {
		return t.BaseSHA, nil
	}
	return gitx.Run(ctx, p.root, "rev-parse", "HEAD")
}

// cmdRecord saves the WHY of a task: a lesson after a QA failure, a
// decision, or an answered question (§4.5, §11).
func (env Env) cmdRecord(ctx context.Context, args []string) error {
	const usageLine = "usage: record lesson|decision|qa <task> ...; see shoalctl help"
	if len(args) == 0 {
		return fail(ExitUsage, usageLine)
	}
	kind := args[0]
	fs := flag.NewFlagSet("record "+kind, flag.ContinueOnError)
	var files multiFlag
	var pattern, fix, glob, topic, choice, why, question, answer *string
	switch kind {
	case "lesson":
		pattern = fs.String("pattern", "", "what went wrong, so the next agent recognises it")
		fix = fs.String("fix", "", "what fixed it")
		glob = fs.String("glob", "", "files the lesson applies to; empty: the whole stack")
	case "decision":
		topic = fs.String("topic", "", "what was decided")
		choice = fs.String("choice", "", "the option chosen")
		why = fs.String("why", "", "the reason, and what was rejected")
		fs.Var(&files, "file", "a file the decision governs; repeatable")
	case "qa":
		question = fs.String("question", "", "the question")
		answer = fs.String("answer", "", "the answer")
		fs.Var(&files, "file", "a file the answer is about; repeatable")
	default:
		return fail(ExitUsage, "unknown record kind %q: use lesson, decision or qa", kind)
	}
	pos, err := parseArgs(fs, args[1:], 1)
	if err != nil {
		return err
	}
	var texts []string
	for _, f := range []struct {
		name string
		v    *string
	}{{"pattern", pattern}, {"fix", fix}, {"topic", topic}, {"choice", choice}, {"why", why}, {"question", question}, {"answer", answer}} {
		if f.v == nil {
			continue
		}
		v, err := recordText(f.name, *f.v)
		if err != nil {
			return err
		}
		texts = append(texts, v)
	}
	paths, err := recordFiles(files)
	if err != nil {
		return err
	}
	g := ""
	if glob != nil {
		g = strings.TrimSpace(*glob)
		if g != "" {
			if err := globx.Validate(g); err != nil {
				return fail(ExitUsage, "--glob: %v", err)
			}
		}
	}
	p, t, err := env.openTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	sha, err := recordSHA(ctx, p, t)
	if err != nil {
		return err
	}
	var id int64
	switch kind {
	case "lesson":
		var round int
		if round, err = lastFailedRound(ctx, p, t); err != nil {
			return err
		}
		id, err = p.db.AddLesson(ctx, store.Lesson{TaskID: t.ID, QARound: round, Pattern: texts[0], Fix: texts[1], Stack: p.cfg.Stack.Name,
			PathGlob: g, TrueAtSHA: sha})
	case "decision":
		id, err = p.db.AddDecision(ctx, store.Decision{TaskID: t.ID, Topic: texts[0], Choice: texts[1], Rationale: texts[2], Files: paths, TrueAtSHA: sha})
	case "qa":
		id, err = p.db.AddQANote(ctx, store.QANote{TaskID: t.ID, Question: texts[0], Answer: texts[1], Files: paths, TrueAtSHA: sha})
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "recorded %s:%d for %s at %s\n", kind, id, t.Slug, shortSHA(sha))
	return nil
}

// lastFailedRound is the task's newest failed QA round, 0 when none failed.
func lastFailedRound(ctx context.Context, p *project, t store.Task) (int, error) {
	rounds, err := p.db.QARounds(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	last := 0
	for _, r := range rounds {
		if !r.Passed && r.Round > last {
			last = r.Round
		}
	}
	return last, nil
}

// cmdFlagStale marks a record that no longer matches the code (§11.1).
func (env Env) cmdFlagStale(ctx context.Context, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("flag-stale", flag.ContinueOnError), args, 2)
	if err != nil {
		return err
	}
	if strings.HasPrefix(pos[0], store.KindConvention+":") {
		return fail(ExitUsage, "a convention is not flagged stale but retired, with its reason: shoalctl convention retire %s --why \"<reason>\"", pos[0])
	}
	kind, id, err := store.ParseRecordRef(pos[0])
	if err != nil {
		return fail(ExitUsage, "%v", err)
	}
	reason, err := recordText("reason", pos[1])
	if err != nil {
		return fail(ExitUsage, "the reason must be 1-%d characters", maxRecordText)
	}
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	changed, err := p.db.FlagStale(ctx, kind, id, reason)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(env.Stdout, "%s was already stale\n", pos[0])
		return nil
	}
	fmt.Fprintf(env.Stdout, "%s flagged stale; it stays searchable and is no longer injected\n", pos[0])
	return nil
}

// repoPath turns a path argument into a slash-separated path relative to
// the top of the checkout env.Dir is in.
func (env Env) repoPath(ctx context.Context, arg string) (string, error) {
	if filepath.IsAbs(arg) {
		top, err := gitx.Run(ctx, env.Dir, "rev-parse", "--show-toplevel")
		if err != nil {
			return "", err
		}
		rel, err := relativeTo(top, arg)
		if err != nil {
			return "", fail(ExitUsage, "%s is outside the checkout %s", arg, top)
		}
		return rel, nil
	}
	prefix, err := gitx.Run(ctx, env.Dir, "rev-parse", "--show-prefix")
	if err != nil {
		return "", err
	}
	rel := path.Clean(prefix + filepath.ToSlash(arg))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fail(ExitUsage, "%s is outside the checkout", arg)
	}
	return rel, nil
}

// relativeTo returns p relative to dir, both symlink-resolved, or an error
// when p is outside dir. p need not exist: its nearest existing ancestor is
// resolved (a file about to be created, or one already deleted).
func relativeTo(dir, p string) (string, error) {
	rdir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	rp, err := resolveExisting(filepath.Clean(p))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rdir, rp)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside %s", p, dir)
	}
	return rel, nil
}

// resolveExisting resolves the symlinks of p's nearest existing ancestor and
// appends the rest of p unchanged.
func resolveExisting(p string) (string, error) {
	r, err := filepath.EvalSymlinks(p)
	if err == nil {
		return r, nil
	}
	parent := filepath.Dir(p)
	if parent == p {
		return "", err
	}
	rp, perr := resolveExisting(parent)
	if perr != nil {
		return "", perr
	}
	return filepath.Join(rp, filepath.Base(p)), nil
}

var hexSHA = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

// cmdWhy shows the stored WHY for a file, a commit or a record, next to what
// git says now (§4.4, §11.3).
func (env Env) cmdWhy(ctx context.Context, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("why", flag.ContinueOnError), args, 1)
	if err != nil {
		return err
	}
	arg := pos[0]
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	all, err := memos(ctx, p.db)
	if err != nil {
		return err
	}
	slugs, err := taskSlugs(ctx, p.db)
	if err != nil {
		return err
	}
	if strings.HasPrefix(arg, store.KindConvention+":") {
		id, err := store.ParseConventionRef(arg)
		if err != nil {
			return fail(ExitUsage, "%v", err)
		}
		for _, m := range all {
			if m.kind == store.KindConvention && m.id == id {
				fmt.Fprintln(env.Stdout, m.line(ctx, env.Dir, slugs))
				return nil
			}
		}
		return fail(ExitNotFound, "%s: no such convention", arg)
	}
	if kind, id, err := store.ParseRecordRef(arg); err == nil {
		if kind == store.KindBug {
			return env.whyBug(ctx, p, id, slugs)
		}
		for _, m := range all {
			if m.kind == kind && m.id == id {
				fmt.Fprintln(env.Stdout, m.line(ctx, env.Dir, slugs))
				return env.history(ctx, "History of its files", m.pathspecs(), "")
			}
		}
		return fail(ExitNotFound, "%s: no such record", arg)
	} else if strings.Contains(arg, ":") && !strings.Contains(arg, "/") {
		return fail(ExitUsage, "%v", err)
	}
	if hexSHA.MatchString(arg) {
		if full, err := gitx.Run(ctx, env.Dir, "rev-parse", "--verify", "--quiet", arg+"^{commit}"); err == nil && full != "" {
			return env.whySHA(ctx, full, all, slugs)
		}
	}
	rel, err := env.repoPath(ctx, arg)
	if err != nil {
		return err
	}
	var found []memo
	for _, m := range all {
		if m.about(rel) {
			found = append(found, m)
		}
	}
	if len(found) == 0 {
		fmt.Fprintf(env.Stdout, "no stored WHY for %s\n", rel)
	} else {
		fmt.Fprintf(env.Stdout, "Stored WHY for %s. %s\n", rel, verifyNote)
		for _, m := range found {
			fmt.Fprintln(env.Stdout, m.line(ctx, env.Dir, slugs))
		}
	}
	return env.history(ctx, "History (git log)", []string{":(literal)" + rel}, "")
}

// history prints git log for pathspecs, newest first; since limits it to
// commits after that one.
func (env Env) history(ctx context.Context, title string, specs []string, since string) error {
	if len(specs) == 0 {
		return nil
	}
	args := []string{"log", "--no-color", "-n", "20", "--format=  %h %an %as %s"}
	if since != "" {
		args = append(args, since+"..HEAD")
	}
	out, err := gitx.Run(ctx, env.Dir, append(append(args, "--"), specs...)...)
	if err != nil {
		return err
	}
	if out == "" {
		out = "  (no commits)"
	}
	fmt.Fprintf(env.Stdout, "%s:\n  %s\n", title, out)
	return nil
}

// whyBug is the regression lookup of §11.3: the stored root cause and fix,
// the commits that touched the bug's files after the fix, and whether the
// regression test is still there and how it last ran.
func (env Env) whyBug(ctx context.Context, p *project, id int64, slugs map[int64]string) error {
	b, err := p.db.GetBug(ctx, id)
	if err != nil {
		return err
	}
	w := env.Stdout
	fmt.Fprintf(w, "bug:%d (task %s, recorded at %s)", b.ID, slugs[b.TaskID], shortSHA(b.TrueAtSHA))
	if b.Stale {
		fmt.Fprintf(w, " [stale: %s]", b.StaleReason)
	}
	fmt.Fprintf(w, "\n  Symptom: %s\n  Root cause: %s\n  Fix: %s\n  Files: %s\n", b.Symptom, b.RootCause, shortSHA(b.FixSHA), strings.Join(b.Files, ", "))
	var specs []string
	for _, f := range b.Files {
		specs = append(specs, ":(literal)"+f)
	}
	for _, rt := range b.Tests {
		specs = append(specs, ":(literal)"+rt.File)
	}
	if err := env.history(ctx, fmt.Sprintf("Commits touching these files after the fix (git log %s..HEAD)", shortSHA(b.FixSHA)), specs, b.FixSHA); err != nil {
		return err
	}
	if len(b.Tests) == 0 {
		fmt.Fprintln(w, "Regression test: none recorded")
	}
	for _, rt := range b.Tests {
		if err := env.regressionTestState(ctx, p, w, rt); err != nil {
			return err
		}
	}
	return nil
}

func (env Env) regressionTestState(ctx context.Context, p *project, w io.Writer, rt store.RegressionTest) error {
	state := "still in HEAD"
	if rt.File == "" {
		state = "file not recorded"
	} else if _, err := gitx.Run(ctx, env.Dir, "cat-file", "-e", "HEAD:"+rt.File); err != nil {
		del, derr := gitx.Run(ctx, env.Dir, "log", "--no-color", "-n", "1", "--diff-filter=D", "--format=%h %an %as %s", "--", rt.File)
		if derr != nil {
			return derr
		}
		state = "DELETED"
		if del != "" {
			state += " in " + del
		}
	}
	fmt.Fprintf(w, "Regression test %s (%s): %s\n", rt.Test, rt.File, state)
	last, ok, err := p.db.LatestResult(ctx, rt.Test)
	if err != nil {
		return err
	}
	if ok {
		fmt.Fprintf(w, "  last recorded result: %s in %s run %d at %s\n", last.Status, last.Kind, last.RunID, shortSHA(last.SHA))
	} else {
		fmt.Fprintln(w, "  no recorded run has it")
	}
	return nil
}

// whySHA shows git's own record of a commit and every stored WHY tied to it.
func (env Env) whySHA(ctx context.Context, full string, all []memo, slugs map[int64]string) error {
	head, err := gitx.Run(ctx, env.Dir, "log", "--no-color", "-n", "1", "--format=%h %an %as %s", full)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s\n", head)
	var found []memo
	for _, m := range all {
		if m.sha == full || contains(m.commits, full) {
			found = append(found, m)
		}
	}
	if len(found) == 0 {
		fmt.Fprintln(env.Stdout, "no stored WHY for this commit")
		return nil
	}
	fmt.Fprintln(env.Stdout, "Stored WHY. "+verifyNote)
	for _, m := range found {
		fmt.Fprintln(env.Stdout, m.line(ctx, env.Dir, slugs))
	}
	return nil
}

// cmdRecall searches every stored record: full text, or the records about
// one file.
func (env Env) cmdRecall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	file := fs.String("file", "", "show the records about this file")
	pos, err := parseArgsRange(fs, args, 0, -1)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(pos, " "))
	if (query == "") == (*file == "") {
		return fail(ExitUsage, "usage: recall <query> | recall --file <path>")
	}
	p, err := env.openProject(ctx)
	if err != nil {
		return err
	}
	defer p.close()
	all, err := memos(ctx, p.db)
	if err != nil {
		return err
	}
	slugs, err := taskSlugs(ctx, p.db)
	if err != nil {
		return err
	}
	var found []memo
	if *file != "" {
		rel, err := env.repoPath(ctx, *file)
		if err != nil {
			return err
		}
		for _, m := range all {
			if m.about(rel) {
				found = append(found, m)
			}
		}
	} else {
		hits, err := p.db.Search(ctx, query, 20)
		if err != nil {
			return err
		}
		byRef := map[string]memo{}
		for _, m := range all {
			byRef[fmt.Sprintf("%s:%d", m.kind, m.id)] = m
		}
		for _, h := range hits {
			m, ok := byRef[fmt.Sprintf("%s:%d", h.Kind, h.Ref)]
			if !ok {
				return fmt.Errorf("search hit %s:%d has no record: %w", h.Kind, h.Ref, errors.New("full-text index out of step with its tables"))
			}
			found = append(found, m)
		}
	}
	if len(found) == 0 {
		fmt.Fprintln(env.Stdout, "nothing recorded matches")
		return nil
	}
	fmt.Fprintln(env.Stdout, verifyNote)
	for _, m := range found {
		fmt.Fprintln(env.Stdout, m.line(ctx, env.Dir, slugs))
	}
	return nil
}
