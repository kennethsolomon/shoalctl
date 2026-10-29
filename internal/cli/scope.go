package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kennethsolomon/shoalctl/internal/gitx"
	"github.com/kennethsolomon/shoalctl/internal/store"
)

// scopeCategories every user scope item needs a row in (§8.6).
var scopeCategories = []string{"happy", "boundary", "invalid", "empty", "permission", "concurrency", "failure", "ui-state"}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// caseEval is one scope row judged against a run.
type caseEval struct {
	store.ScopeCase
	Green  bool
	Waived bool
	Why    string // why the row is still open
}

type itemEval struct {
	store.ScopeItem
	Rows    []caseEval
	Missing []string // categories with no row (user items)
}

// open lists what keeps the item from done.
func (it itemEval) open() []string {
	var out []string
	if len(it.Rows) == 0 && it.Source == store.ScopeBehaviour {
		out = append(out, fmt.Sprintf("behaviour #%d %q has no test", it.ID, it.Text))
	}
	for _, c := range it.Missing {
		out = append(out, fmt.Sprintf("item #%d %q has no %s row", it.ID, it.Text, c))
	}
	for _, r := range it.Rows {
		if !r.Green && !r.Waived {
			out = append(out, fmt.Sprintf("case #%d (item #%d, %s): %s", r.ID, it.ID, r.Category, r.Why))
		}
	}
	return out
}

// evalScope judges every row against run, the effective results of the
// task's latest run, and baseline, the baseline's (for the feature rule that
// a behaviour's test must be new, §8.4).
func evalScope(items []store.ScopeItem, run, baseline map[string]string) []itemEval {
	out := make([]itemEval, 0, len(items))
	for _, it := range items {
		ev := itemEval{ScopeItem: it}
		have := map[string]bool{}
		for _, c := range it.Cases {
			have[c.Category] = true
			r := caseEval{ScopeCase: c}
			switch {
			case c.Status == "waived":
				r.Waived = true
			case c.Check == store.CheckManual && c.Status == "pass":
				r.Green = true
			case c.Check == store.CheckManual && c.Status == "fail":
				r.Why = "manual check failed"
			case c.Check == store.CheckManual:
				r.Why = "manual check not done: " + c.ManualStep
			case run == nil:
				r.Why = "no completed test run yet"
			case it.Source == store.ScopeBehaviour && baseline[c.TestID] != "":
				r.Why = fmt.Sprintf("test %s existed at baseline; a behaviour needs a new test", c.TestID)
			case run[c.TestID] == "pass":
				r.Green = true
			case run[c.TestID] == "":
				r.Why = fmt.Sprintf("test %s did not run", c.TestID)
			default:
				r.Why = fmt.Sprintf("test %s is %s", c.TestID, run[c.TestID])
			}
			ev.Rows = append(ev.Rows, r)
		}
		if it.Source == store.ScopeUser {
			for _, cat := range scopeCategories {
				if !have[cat] {
					ev.Missing = append(ev.Missing, cat)
				}
			}
		}
		out = append(out, ev)
	}
	return out
}

func scopeOpen(evs []itemEval) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, ev.open()...)
	}
	return out
}

// latestResults returns the effective results of the task's newest
// completed gate, qa or final run, or nil when there is none.
func latestResults(ctx context.Context, p *project, t store.Task) (map[string]string, error) {
	run, ok, err := p.db.LatestRun(ctx, t.ID, "gate", "qa", "final")
	if err != nil || !ok {
		return nil, err
	}
	rs, err := p.db.EffectiveResults(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	return statusMap(rs), nil
}

func baselineResults(ctx context.Context, p *project, t store.Task) (map[string]string, error) {
	if t.BaselineRunID == 0 {
		return map[string]string{}, nil
	}
	rs, err := p.db.EffectiveResults(ctx, t.BaselineRunID)
	if err != nil {
		return nil, err
	}
	return statusMap(rs), nil
}

// humanName is git's user.name in dir, the name decisions are recorded under.
func humanName(ctx context.Context, dir string) string {
	if name, err := gitx.Run(ctx, dir, "config", "user.name"); err == nil && name != "" {
		return name
	}
	return "human"
}

func (env Env) cmdScope(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fail(ExitUsage, "usage: scope add|case|show|review|check <task> ...")
	}
	switch args[0] {
	case "add":
		return env.scopeAdd(ctx, args[1:])
	case "case":
		return env.scopeCase(ctx, args[1:])
	case "show":
		return env.scopeShow(ctx, args[1:])
	case "review":
		return env.scopeReview(ctx, args[1:])
	case "check":
		return env.scopeCheck(ctx, args[1:])
	}
	return fail(ExitUsage, "unknown scope command %q: use add, case, show, review or check", args[0])
}

func validScopeText(text string) error {
	if strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > 500 {
		return fail(ExitUsage, "scope text must be 1-500 characters")
	}
	return nil
}

// openScopeTask opens a task whose scope can still change.
func (env Env) openScopeTask(ctx context.Context, ref string) (*project, store.Task, error) {
	p, t, err := env.openTask(ctx, ref)
	if err != nil {
		return nil, t, err
	}
	if t.Stage == store.StageDone || contains(closedStages, t.Stage) {
		p.close()
		return nil, t, fail(ExitState, "task %s is %s; its scope is closed", t.Slug, t.Stage)
	}
	return p, t, nil
}

func (env Env) scopeAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scope add", flag.ContinueOnError)
	behaviour := fs.Bool("behaviour", false, "a new feature behaviour (shoal-lead), not a user scope item")
	pos, err := parseArgs(fs, args, 2)
	if err != nil {
		return err
	}
	if err := validScopeText(pos[1]); err != nil {
		return err
	}
	p, t, err := env.openScopeTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	source := store.ScopeUser
	if *behaviour {
		source = store.ScopeBehaviour
	}
	id, err := p.db.AddScopeItem(ctx, t.ID, source, strings.TrimSpace(pos[1]))
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s item #%d added to %s\n", source, id, t.Slug)
	return nil
}

func (env Env) scopeCase(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scope case", flag.ContinueOnError)
	category := fs.String("category", "", strings.Join(scopeCategories, ", "))
	desc := fs.String("desc", "", "what the case checks")
	test := fs.String("test", "", "id of the automated test that covers it")
	manual := fs.String("manual", "", "exact manual steps and the expected result")
	na := fs.String("na", "", "the category does not apply: one-line reason (a human still waives it)")
	pos, err := parseArgs(fs, args, 2)
	if err != nil {
		return err
	}
	if !contains(scopeCategories, *category) {
		return fail(ExitUsage, "--category %q must be one of %s", *category, strings.Join(scopeCategories, ", "))
	}
	set := 0
	for _, v := range []string{*test, *manual, *na} {
		if strings.TrimSpace(v) != "" {
			set++
		}
	}
	if set != 1 {
		return fail(ExitUsage, "give exactly one of --test, --manual or --na")
	}
	c := store.ScopeCase{Category: *category, Description: strings.TrimSpace(*desc), Check: store.CheckAutomated, TestID: strings.TrimSpace(*test)}
	switch {
	case *manual != "":
		c.Check, c.TestID, c.ManualStep = store.CheckManual, "", strings.TrimSpace(*manual)
	case *na != "":
		c.Check, c.TestID, c.ManualStep = store.CheckManual, "", "not applicable: "+strings.TrimSpace(*na)
	}
	if c.Description == "" {
		c.Description = c.ManualStep
		if c.Check == store.CheckAutomated {
			c.Description = c.TestID
		}
	}
	for _, v := range []string{c.Description, c.ManualStep, c.TestID} {
		if utf8.RuneCountInString(v) > 1000 {
			return fail(ExitUsage, "scope case fields must be at most 1000 characters")
		}
	}
	item, err := strconv.ParseInt(pos[1], 10, 64)
	if err != nil {
		return fail(ExitUsage, "item id %q must be a number (see scope show)", pos[1])
	}
	c.ItemID = item
	p, t, err := env.openScopeTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	id, err := p.db.AddScopeCase(ctx, t.ID, c)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "case #%d added to item #%d\n", id, item)
	return nil
}

func (env Env) scopeEval(ctx context.Context, p *project, t store.Task) ([]itemEval, error) {
	items, err := p.db.ScopeItems(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	run, err := latestResults(ctx, p, t)
	if err != nil {
		return nil, err
	}
	base, err := baselineResults(ctx, p, t)
	if err != nil {
		return nil, err
	}
	return evalScope(items, run, base), nil
}

func printScope(w io.Writer, evs []itemEval) {
	if len(evs) == 0 {
		fmt.Fprintln(w, "no scope items")
		return
	}
	for _, ev := range evs {
		fmt.Fprintf(w, "item #%d [%s] %s\n", ev.ID, ev.Source, ev.Text)
		for _, r := range ev.Rows {
			state := "green"
			switch {
			case r.Waived:
				state = "waived: " + r.WaiveReason
			case !r.Green:
				state = "open: " + r.Why
			}
			target := r.TestID
			if r.Check == store.CheckManual {
				target = "manual: " + r.ManualStep
			}
			fmt.Fprintf(w, "  case #%d %-11s %s (%s) - %s\n", r.ID, r.Category, r.Description, target, state)
		}
		if len(ev.Missing) > 0 {
			fmt.Fprintf(w, "  missing categories: %s\n", strings.Join(ev.Missing, ", "))
		}
	}
}

func (env Env) scopeShow(ctx context.Context, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("scope show", flag.ContinueOnError), args, 1)
	if err != nil {
		return err
	}
	p, t, err := env.openTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	evs, err := env.scopeEval(ctx, p, t)
	if err != nil {
		return err
	}
	printScope(env.Stdout, evs)
	if open := scopeOpen(evs); len(open) > 0 {
		fmt.Fprintf(env.Stdout, "%d open row(s): done refuses until each is green or waived by a human (scope review)\n", len(open))
	}
	return nil
}

// requireHuman refuses unless a person is at a terminal: agents can't waive
// rows or record manual checks (§8.6).
func (env Env) requireHuman(what string) error {
	if !env.interactive() {
		return fail(ExitState, "%s needs a human at a terminal; agents can't waive scope rows or record manual checks", what)
	}
	return nil
}

func (env Env) scopeReview(ctx context.Context, args []string) error {
	pos, err := parseArgs(flag.NewFlagSet("scope review", flag.ContinueOnError), args, 1)
	if err != nil {
		return err
	}
	if err := env.requireHuman("scope review"); err != nil {
		return err
	}
	p, t, err := env.openScopeTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	evs, err := env.scopeEval(ctx, p, t)
	if err != nil {
		return err
	}
	by := humanName(ctx, p.root)
	in := bufio.NewReader(env.Stdin)
	ask := func(prompt string) (string, error) {
		fmt.Fprint(env.Stdout, prompt)
		line, err := in.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return "", fail(ExitState, "scope review stopped: no more input")
		}
		return strings.TrimSpace(line), nil
	}
	reason := func() (string, error) {
		for {
			r, err := ask("  reason: ")
			if err != nil || r != "" {
				return r, err
			}
		}
	}
	waived, kept := 0, 0
	for _, ev := range evs {
		for _, r := range ev.Rows {
			if r.Green || r.Waived {
				continue
			}
			fmt.Fprintf(env.Stdout, "item #%d %q, case #%d %s: %s\n  %s\n", ev.ID, ev.Text, r.ID, r.Category, r.Description, r.Why)
			key, err := ask("  [w]aive or [k]eep? ")
			if err != nil {
				return err
			}
			if key != "w" {
				kept++
				continue
			}
			why, err := reason()
			if err != nil {
				return err
			}
			if err := p.db.DecideCase(ctx, r.ScopeCase, "waived", why, "", by); err != nil {
				return err
			}
			waived++
		}
		for _, cat := range ev.Missing {
			fmt.Fprintf(env.Stdout, "item #%d %q has no %s row\n", ev.ID, ev.Text, cat)
			key, err := ask("  [w]aive or [k]eep? ")
			if err != nil {
				return err
			}
			if key != "w" {
				kept++
				continue
			}
			why, err := reason()
			if err != nil {
				return err
			}
			c := store.ScopeCase{ItemID: ev.ID, Category: cat, Description: "category not covered", Check: store.CheckManual, ManualStep: "not covered"}
			if c.ID, err = p.db.AddScopeCase(ctx, t.ID, c); err != nil {
				return err
			}
			c.Version = 1
			if err := p.db.DecideCase(ctx, c, "waived", why, "", by); err != nil {
				return err
			}
			waived++
		}
	}
	fmt.Fprintf(env.Stdout, "%d waived, %d kept open (by %s)\n", waived, kept, by)
	return nil
}

func (env Env) scopeCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scope check", flag.ContinueOnError)
	pass := fs.Bool("pass", false, "the manual check passed")
	failed := fs.Bool("fail", false, "the manual check failed")
	note := fs.String("note", "", "what you saw")
	pos, err := parseArgs(fs, args, 2)
	if err != nil {
		return err
	}
	if *pass == *failed {
		return fail(ExitUsage, "give exactly one of --pass or --fail")
	}
	caseID, err := strconv.ParseInt(pos[1], 10, 64)
	if err != nil {
		return fail(ExitUsage, "case id %q must be a number (see scope show)", pos[1])
	}
	if err := env.requireHuman("scope check"); err != nil {
		return err
	}
	p, t, err := env.openScopeTask(ctx, pos[0])
	if err != nil {
		return err
	}
	defer p.close()
	items, err := p.db.ScopeItems(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, it := range items {
		for _, c := range it.Cases {
			if c.ID != caseID {
				continue
			}
			if c.Check != store.CheckManual {
				return fail(ExitState, "case #%d is automated: it turns green when test %s passes", c.ID, c.TestID)
			}
			status := "pass"
			if *failed {
				status = "fail"
			}
			if err := p.db.DecideCase(ctx, c, status, "", strings.TrimSpace(*note), humanName(ctx, p.root)); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "case #%d marked %s\n", c.ID, status)
			return nil
		}
	}
	return fail(ExitNotFound, "case #%d is not a scope case of task %s", caseID, t.Slug)
}
