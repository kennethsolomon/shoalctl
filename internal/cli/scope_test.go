package cli

import (
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/store"
)

func TestEvalScope(t *testing.T) {
	items := []store.ScopeItem{
		{ID: 1, Source: store.ScopeUser, Text: "CSV", Cases: []store.ScopeCase{
			{ID: 1, Category: "happy", Check: store.CheckAutomated, TestID: "C::ok", Status: "pending"},
			{ID: 2, Category: "boundary", Check: store.CheckAutomated, TestID: "C::bad", Status: "pending"},
			{ID: 3, Category: "ui-state", Check: store.CheckManual, ManualStep: "look", Status: "pending"},
			{ID: 4, Category: "empty", Check: store.CheckManual, ManualStep: "look", Status: "pass"},
			{ID: 5, Category: "invalid", Check: store.CheckAutomated, TestID: "C::gone", Status: "waived"},
		}},
		{ID: 2, Source: store.ScopeBehaviour, Text: "rejects dup", Cases: []store.ScopeCase{
			{ID: 6, Category: "happy", Check: store.CheckAutomated, TestID: "C::old", Status: "pending"},
		}},
		{ID: 3, Source: store.ScopeBehaviour, Text: "no test"},
		{ID: 4, Source: store.ScopeBehaviour, Text: "new test", Cases: []store.ScopeCase{
			{ID: 7, Category: "happy", Check: store.CheckAutomated, TestID: "C::new", Status: "pending"},
		}},
	}
	run := map[string]string{"C::ok": "pass", "C::bad": "fail", "C::old": "pass", "C::new": "pass"}
	base := map[string]string{"C::ok": "pass", "C::old": "pass"}
	evs := evalScope(items, run, base)
	open := strings.Join(scopeOpen(evs), "\n")
	for _, want := range []string{"case #2 (item #1, boundary): test C::bad is fail", "case #3 (item #1, ui-state): manual check not done",
		"no permission row", "no concurrency row", "no failure row", "case #6 (item #2, happy): test C::old existed at baseline",
		`behaviour #3 "no test" has no test`} {
		if !strings.Contains(open, want) {
			t.Errorf("open rows lack %q:\n%s", want, open)
		}
	}
	for _, not := range []string{"case #1 ", "case #4 ", "case #5 ", "case #7 ", "no happy row", "no empty row"} {
		if strings.Contains(open, not) {
			t.Errorf("open rows should not contain %q:\n%s", not, open)
		}
	}
	if got := scopeOpen(evalScope(items[3:], nil, nil)); len(got) != 1 || !strings.Contains(got[0], "no completed test run") {
		t.Fatalf("no run: %v", got)
	}
}

func TestScopeCommands(t *testing.T) {
	h := newHarness(t)
	h.initWith(junitConfig)
	slug := strings.TrimSpace(h.mustRun("new", "feature", "Export", "--scope", "also test CSV export"))
	h.mustRun("scope", "add", slug, "--behaviour", "GET /export returns CSV")
	h.mustRun("scope", "case", slug, "1", "--category", "happy", "--test", "C::a")
	h.mustRun("scope", "case", slug, "1", "--category", "ui-state", "--na", "no UI")
	h.mustRun("scope", "case", slug, "1", "--category", "empty", "--manual", "export with no rows: header only")
	h.expectCode(ExitUsage, "scope", "case", slug, "1", "--category", "weird", "--test", "x")
	h.expectCode(ExitUsage, "scope", "case", slug, "1", "--category", "happy", "--test", "x", "--manual", "y")
	h.expectCode(ExitNotFound, "scope", "case", slug, "9", "--category", "happy", "--test", "x")

	show := h.mustRun("scope", "show", slug)
	for _, want := range []string{"item #1 [user] also test CSV export", "item #2 [behaviour]", "case #2 ui-state", "not applicable: no UI",
		"missing categories: boundary, invalid, permission, concurrency, failure", "open row(s)"} {
		if !strings.Contains(show, want) {
			t.Errorf("show lacks %q:\n%s", want, show)
		}
	}

	// Agents (no terminal) can't waive or check.
	h.expectCode(ExitState, "scope", "review", slug)
	h.expectCode(ExitState, "scope", "check", slug, "3", "--pass")

	h.env.Terminal = func() bool { return true }
	h.mustRun("scope", "check", slug, "3", "--pass", "--note", "header only, ok")
	h.expectCode(ExitState, "scope", "check", slug, "1", "--pass")
	h.expectCode(ExitNotFound, "scope", "check", slug, "99", "--pass")

	// Review: case #1 (no run yet) keep, case #2 waive, then the 5 missing categories: waive all but the last.
	h.env.Stdin = strings.NewReader("k\nw\n\nno UI in this app\nw\nr1\nw\nr2\nw\nr3\nw\nr4\nk\n")
	out := h.mustRun("scope", "review", slug)
	if !strings.Contains(out, "5 waived, 2 kept open") {
		t.Fatalf("review:\n%s", out)
	}
	show = h.mustRun("scope", "show", slug)
	for _, want := range []string{"waived: no UI in this app", "manual: export with no rows: header only) - green", "missing categories: failure"} {
		if !strings.Contains(show, want) {
			t.Errorf("show after review lacks %q:\n%s", want, show)
		}
	}
	h.env.Stdin = strings.NewReader("w\n")
	h.expectCode(ExitState, "scope", "review", slug)
}
