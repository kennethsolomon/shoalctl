package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kennethsolomon/shoalctl/internal/store"
	"github.com/kennethsolomon/shoalctl/internal/testutil"
)

// Phase 3 acceptance (spec §15, "Done when") plus the two Phase 2
// deferrals (Bug A's lesson in Bug B's brief, in TestPhase2BugABugB, and the
// late overlap below). Hooks are driven the way Claude Code calls them:
// JSON on stdin, CLAUDE_PROJECT_DIR set to the task worktree.

func toolCall(session, tool, file, agent string) string {
	in := map[string]any{"session_id": session, "hook_event_name": "PreToolUse", "tool_name": tool,
		"tool_use_id": "toolu_" + session, "tool_input": map[string]string{"file_path": file}}
	if agent != "" {
		in["agent_type"] = agent
	}
	b, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// answer decodes a hook's stdout; empty stdout is the zero answer.
func answer(t *testing.T, out string) hookOutput {
	t.Helper()
	var a hookOutput
	if out == "" {
		return a
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		t.Fatalf("hook stdout is not shoal's JSON answer: %v\n%q", err, out)
	}
	return a
}

// inHook runs a hook in-process inside wt and fails unless it exits 0 with
// nothing on stderr and nothing new in hooks.log.
func inHook(t *testing.T, h *harness, wt, event, stdin string) hookOutput {
	t.Helper()
	t.Setenv("CLAUDE_PROJECT_DIR", wt)
	before := hookLog(t, h.project())
	code, out, errOut := h.hook(strings.NewReader(stdin), event)
	if code != 0 || errOut != "" {
		t.Fatalf("hook %s: code %d stderr %q", event, code, errOut)
	}
	if log := hookLog(t, h.project()); log != before {
		t.Fatalf("hook %s failed (fail-open hid it):\n%s", event, strings.TrimPrefix(log, before))
	}
	return answer(t, out)
}

// Done when 1: the session-start brief stays under 1k tokens, measured on a
// worst case: a long title and ask, a 60-line todo.md of long lines, 12
// matching lessons at the 500-character limit, two blockers and unread
// messages.
//
// Measurement: approxTokens, the largest of bytes/3, characters/3 (one per
// CJK character) and the number of words plus punctuation marks (a run over
// 16 letters or digits counts one per 2). Claude's tokenizer averages 3.5-4
// characters per token on English and code, and BPE merges words and common
// punctuation, so all three counts over-estimate this kind of text.
// TestPhase3BriefUnderBudgetCJKAndBlobs covers CJK lessons and hex blobs.
func TestPhase3BriefUnderBudget(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig + "[stack]\nname = \"laravel\"\n")
	long := strings.Repeat("Users page shows the wrong display name when first_name is null ", 6)
	slug := strings.TrimSpace(h.mustRun("new", "feature", "Rework "+long[:110], "--ask", long+long))
	other := newTask(t, h, "feature", "Earlier task")
	h.mustRun("plan", other, "resources/js/users/**")
	todo := filepath.Join(t.TempDir(), "todo.md")
	var tb strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&tb, "%d. %s\n", i, long)
	}
	testutil.WriteFile(t, todo, tb.String())
	h.env.Terminal = func() bool { return true }
	h.env.Stdin = strings.NewReader("p\n")
	h.mustRun("plan", slug, "--todo", todo, "resources/js/users/**", "app/Models/User.php")
	h.env.Terminal, h.env.Stdin = nil, nil
	h.mustRun("start", slug)
	task := h.task(slug)
	h.mustRun("record", "lesson", other, "--pattern", "stack-wide lesson", "--fix", "x")
	for i := 0; i < 12; i++ {
		text := strings.Repeat(fmt.Sprintf("lesson %d: the list view reads another accessor; ", i), 20)[:500]
		h.mustRun("record", "lesson", other, "--pattern", text, "--fix", text, "--glob", "resources/js/users/**")
	}
	p := h.project()
	sqlExec(t, p, `INSERT INTO task_blockers (task_id, blocker_id, created_at) VALUES (?, ?, 'x')`, task.ID, h.task(other).ID)
	for i := 0; i < 3; i++ {
		h.mustRun("msg", "send", slug, "--from", "shoal-backend", "--to", "shoal-lead", "--kind", "question", "question "+fmt.Sprint(i))
	}

	a := inHook(t, h, task.Worktree, "session-start", `{"session_id": "s-brief", "source": "startup"}`)
	brief := a.HookSpecificOutput.AdditionalContext
	if a.HookSpecificOutput.HookEventName != "SessionStart" || a.SystemMessage != "" {
		t.Fatalf("answer %+v", a)
	}
	tokens := approxTokens(brief)
	t.Logf("brief: %d bytes, %d estimated tokens (budget 1000)", len(brief), tokens)
	if tokens >= 1000 || (len(brief)+2)/3 >= 1000 {
		t.Fatalf("brief is %d estimated tokens (%d bytes), want < 1000:\n%s", tokens, len(brief), brief)
	}
	mustContain(t, "brief", brief, "shoal task "+slug, "Task brief: .shoal/brief.md", "merges after "+other,
		"Unread messages for shoal-lead: 3", "todo.md (head):", "0. Users page", "Lessons from earlier tasks", "verify it against the current code",
		"- lesson:13 (task "+other)
	if strings.Contains(brief, "59. Users page") {
		t.Errorf("the brief shows the whole todo.md")
	}
	// The command prints the same brief, with the same budget.
	out := h.mustRun("brief", slug)
	if approxTokens(out) >= 1000 || !strings.Contains(out, "shoal task "+slug) {
		t.Fatalf("shoalctl brief: %d tokens\n%s", approxTokens(out), out)
	}
	// Lessons shown in the brief count one hit, and are not repeated by
	// PreToolUse in the same session.
	if n := countRows(t, p, `SELECT count(*) FROM lessons WHERE hits = 1`); n == 0 {
		t.Fatal("no lesson hit counted for the brief")
	}
	pre := inHook(t, h, task.Worktree, "pre-tool-use", toolCall("s-brief", "Read", filepath.Join(task.Worktree, "resources/js/users/list.js"), ""))
	if strings.Contains(pre.HookSpecificOutput.AdditionalContext, "- lesson:13 ") {
		t.Errorf("lesson:13 injected twice in one session:\n%s", pre.HookSpecificOutput.AdditionalContext)
	}
}

// Done when 2: a lesson is injected when a matching file is touched, once
// per session; a different session gets it again; the staleness pre-check
// tags it when its files changed since the record.
func TestPhase3LessonInjectedOncePerSession(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	a := startedTask(t, h, "bugfix", "Users page wrong name")
	h.mustRun("record", "lesson", a.Slug, "--pattern", "the list view reads a different accessor", "--fix", "use User::displayName in the list",
		"--glob", "resources/js/users/**")
	h.mustRun("record", "decision", a.Slug, "--topic", "names", "--choice", "one accessor", "--why", "two drifted", "--file", "app/Models/User.php")
	b := startedTask(t, h, "feature", "Users sort")
	file := filepath.Join(b.Worktree, "resources", "js", "users", "list.js")

	got := inHook(t, h, b.Worktree, "pre-tool-use", toolCall("s1", "Edit", file, ""))
	ctxText := got.HookSpecificOutput.AdditionalContext
	if got.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("answer %+v", got)
	}
	mustContain(t, "injection", ctxText, "shoal: records about resources/js/users/list.js", "- lesson:1 (task "+a.Slug,
		"the list view reads a different accessor", "use User::displayName", "verify it against the current code", "flag-stale")
	if strings.Contains(ctxText, "decision:1") || strings.Contains(ctxText, "[files changed") {
		t.Errorf("unrelated or stale-tagged record injected:\n%s", ctxText)
	}
	if approxTokens(ctxText) > 300 {
		t.Errorf("per-file injection is %d tokens, over 300", approxTokens(ctxText))
	}

	if again := inHook(t, h, b.Worktree, "pre-tool-use", toolCall("s1", "Read", file, "")); again.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("injected twice in one session: %+v", again)
	}
	if none := inHook(t, h, b.Worktree, "pre-tool-use", toolCall("s1", "Read", filepath.Join(b.Worktree, "README"), "")); none.HookSpecificOutput.HookEventName != "" {
		t.Fatalf("a file no record is about got %+v", none)
	}
	if bash := inHook(t, h, b.Worktree, "pre-tool-use", `{"session_id": "s1", "tool_name": "Bash", "tool_input": {"command": "ls"}}`); bash.HookSpecificOutput.HookEventName != "" {
		t.Fatalf("a Bash call got %+v", bash)
	}
	// The decision is about User.php, and arrives when that file is read.
	dec := inHook(t, h, b.Worktree, "pre-tool-use", toolCall("s1", "Read", filepath.Join(b.Worktree, "app", "Models", "User.php"), ""))
	mustContain(t, "decision", dec.HookSpecificOutput.AdditionalContext, "- decision:1 ", "one accessor")

	// A new session, after the files changed since the record: injected
	// again, tagged.
	commitIn(t, b.Worktree, "resources/js/users/list.js", "export const sort = 1\n")
	next := inHook(t, h, b.Worktree, "pre-tool-use", toolCall("s2", "Read", file, ""))
	mustContain(t, "second session", next.HookSpecificOutput.AdditionalContext, "- lesson:1 ", "[files changed since record]")
	ls, err := h.db().Lessons(t.Context())
	if err != nil || len(ls) != 1 || ls[0].Hits != 2 {
		t.Fatalf("lesson hits: %+v %v", ls, err)
	}

	// Flagged stale: never injected again, still found by recall and why.
	h.mustRun("flag-stale", "lesson:1", "the list now uses displayName")
	if s3 := inHook(t, h, b.Worktree, "pre-tool-use", toolCall("s3", "Read", file, "")); s3.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("a stale lesson was injected: %+v", s3)
	}
	mustContain(t, "recall", h.mustRun("recall", "accessor"), "lesson:1", "[stale: the list now uses displayName]")
	mustContain(t, "why", h.mustRun("why", "resources/js/users/list.js"), "lesson:1", "History (git log)")
}

// A compaction drops injected records from the agent's context, so
// SessionStart with source compact (or clear) resets the session's
// injection marks: the next touch injects the lesson again, without counting
// a second hit. Two registrations of that SessionStart still reset once.
func TestPhase3CompactionReinjectsLessons(t *testing.T) {
	testutil.RequireSh(t)
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	a := startedTask(t, h, "bugfix", "Users page wrong name")
	h.mustRun("record", "lesson", a.Slug, "--pattern", "accessor drift", "--fix", "one accessor", "--glob", "src/**")
	b := startedTask(t, h, "feature", "Users sort")
	file := filepath.Join(b.Worktree, "src", "a.go")
	touch := func() string {
		t.Helper()
		return inHook(t, h, b.Worktree, "pre-tool-use", toolCall("sc", "Read", file, "")).HookSpecificOutput.AdditionalContext
	}
	hits := func() int {
		t.Helper()
		return countRows(t, h.project(), `SELECT hits FROM lessons WHERE id = 1`)
	}

	inHook(t, h, b.Worktree, "session-start", `{"session_id": "sc", "source": "startup"}`)
	mustContain(t, "first touch", touch(), "- lesson:1 ")
	if again := touch(); again != "" || hits() != 1 {
		t.Fatalf("before the compaction: injected again %q, %d hits", again, hits())
	}

	t.Setenv("CLAUDE_PROJECT_DIR", b.Worktree)
	var outs []string
	var cmds []*exec.Cmd
	var bufs []*bytes.Buffer
	for i := 0; i < 2; i++ {
		cmd := h.command("hook", "session-start")
		cmd.Stdin = strings.NewReader(`{"session_id": "sc", "source": "compact"}`)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds, bufs = append(cmds, cmd), append(bufs, &out)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("session-start compact: %v", err)
		}
		outs = append(outs, bufs[i].String())
	}
	sort.Strings(outs)
	if outs[0] != "" || outs[1] == "" {
		t.Fatalf("compact SessionStart fired twice: %q", outs)
	}
	if log := hookLog(t, h.project()); log != "" {
		t.Fatalf("hook failures:\n%s", log)
	}

	mustContain(t, "after the compaction", touch(), "- lesson:1 ")
	if again := touch(); again != "" {
		t.Fatalf("injected twice after one compaction: %q", again)
	}
	if n := hits(); n != 1 {
		t.Fatalf("the re-injection after a compaction counted a hit: %d hits", n)
	}

	inHook(t, h, b.Worktree, "session-start", `{"session_id": "sc", "source": "clear"}`)
	mustContain(t, "after a clear", touch(), "- lesson:1 ")
	if n := hits(); n != 1 {
		t.Fatalf("the re-injection after a clear counted a hit: %d hits", n)
	}
}

// Done when 3: a write outside an agent's claims prints a one-line warning
// (and the Phase 1 deferrals: a write onto another agent's claim, a
// contract edit by a non-lead). The warning is shown to the human
// (systemMessage) and to the agent (additionalContext), once per session.
func TestPhase3WriteOutsideClaimsWarns(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	task := startedTask(t, h, "feature", "Clients CRUD")
	h.mustRun("claim", task.Slug, "--as", "shoal-backend", "app/**")
	h.mustRun("claim", task.Slug, "--as", "shoal-frontend", "resources/js/**")
	wt := task.Worktree
	post := func(session, file, agent string) hookOutput {
		t.Helper()
		return inHook(t, h, wt, "post-tool-use", toolCall(session, "Write", filepath.Join(wt, file), agent))
	}

	if a := post("s", "app/Client.php", "shoal-backend"); a.HookSpecificOutput.HookEventName != "" {
		t.Fatalf("a write inside the agent's claims warned: %+v", a)
	}
	out := post("s", "config/app.php", "shoal-backend")
	want := "shoal: shoal-backend wrote config/app.php, outside its claims (app/**); claim it first: shoalctl claim " + task.Slug + " --as shoal-backend config/app.php"
	if out.SystemMessage != want || out.HookSpecificOutput.AdditionalContext != want || out.HookSpecificOutput.HookEventName != "PostToolUse" {
		t.Fatalf("warning:\n got %+v\nwant %q", out, want)
	}
	if strings.Contains(out.SystemMessage, "\n") {
		t.Fatalf("the warning is not one line: %q", out.SystemMessage)
	}
	if again := post("s", "config/app.php", "shoal-backend"); again.SystemMessage != "" {
		t.Fatalf("warned twice in one session: %+v", again)
	}
	held := post("s", "resources/js/clients/list.js", "shoal-backend")
	mustContain(t, "held", held.SystemMessage, "shoal-backend wrote resources/js/clients/list.js, which shoal-frontend claims in this task (resources/js/**)")
	contract := post("s", ".shoal/contract.md", "shoal-frontend")
	mustContain(t, "contract", contract.SystemMessage, "shoal-frontend edited .shoal/contract.md, which only shoal-lead changes", "--kind contract-change")
	if lead := post("s", ".shoal/contract.md", ""); lead.SystemMessage != "" {
		t.Fatalf("the lead's contract edit warned: %+v", lead)
	}

	// Each write outside a claim is recorded as an actual claim, once; an
	// actual claim never blocks another agent's claim.
	p := h.project()
	if n := countRows(t, p, `SELECT count(*) FROM file_claims WHERE task_id = ? AND kind = 'actual' AND agent = 'shoal-backend' AND path_glob = 'config/app.php'`, task.ID); n != 1 {
		t.Fatalf("%d actual claims on config/app.php", n)
	}
	if n := countRows(t, p, `SELECT count(*) FROM file_claims WHERE task_id = ? AND kind = 'actual' AND path_glob = 'app/Client.php'`, task.ID); n != 0 {
		t.Fatalf("a write inside an expected claim recorded %d actual claim(s)", n)
	}
	h.mustRun("claim", task.Slug, "--as", "shoal-docs", "config/**")
	// Writes outside the worktree, and reads, are not claims.
	if a := inHook(t, h, wt, "post-tool-use", toolCall("s", "Write", filepath.Join(t.TempDir(), "x.go"), "shoal-backend")); a.SystemMessage != "" {
		t.Fatalf("a write outside the worktree: %+v", a)
	}
	if a := inHook(t, h, wt, "post-tool-use", toolCall("s", "Read", filepath.Join(wt, "zzz.go"), "shoal-backend")); a.SystemMessage != "" {
		t.Fatalf("a read: %+v", a)
	}
}

// Phase 2 deferral 2: overlap found late, from actual edits (§9.2). Task X
// writes a file task E planned: both are told, and X merges after E even
// though X is done first.
func TestPhase3LateOverlapFromEdits(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	e := newTask(t, h, "feature", "Dropbox integration")
	h.mustRun("plan", e, "config/services.php")
	h.mustRun("start", e)
	x := startedTask(t, h, "feature", "Clients CRUD")

	w := inHook(t, h, x.Worktree, "post-tool-use", toolCall("sx", "Edit", filepath.Join(x.Worktree, "config", "services.php"), ""))
	want := "shoal: config/services.php is also claimed by task " + e + " (late overlap); " + x.Slug + " now merges after " + e + ", and " + e + "'s lead was told"
	if w.SystemMessage != want {
		t.Fatalf("late overlap warning:\n got %q\nwant %q", w.SystemMessage, want)
	}
	msgs := h.mustRun("msg", "inbox", e, "--as", "shoal-lead")
	mustContain(t, "E's inbox", msgs, "late overlap: "+x.Slug+" wrote config/services.php", x.Slug+" merges after this task")
	mustContain(t, "status", h.mustRun("status"), "merges after "+e)
	mustContain(t, "X brief", h.mustRun("brief", x.Slug), "Blocked: merges after "+e)

	commitIn(t, x.Worktree, "feature.txt", "x")
	finish(t, h, x.Slug, x.Worktree)
	out := h.mustRun("merge")
	mustContain(t, "first merge", out, x.Slug+": waits for "+e+" to merge first", "merged 0 task(s)")
	if got := h.task(x.Slug); got.Stage != store.StageDone {
		t.Fatalf("X merged ahead of E: %+v", got)
	}

	ew := h.task(e).Worktree
	commitIn(t, ew, "config/services.php", "<?php return [];\n")
	finish(t, h, e, ew)
	out = h.mustRun("merge")
	ei, xi := strings.Index(out, "merged "+e+" into main"), strings.Index(out, "merged "+x.Slug+" into main")
	if ei < 0 || xi < 0 || ei > xi {
		t.Fatalf("want E merged, then X:\n%s", out)
	}
	if got := h.task(x.Slug); got.Stage != store.StageMerged || got.BlockedBy != 0 {
		t.Fatalf("X after the merge: %+v", got)
	}
}

// The stuck-holder escape: X wrote onto E's claim, so X merges after E. E
// never finishes. Plain merge skips X and names the way out; merge --task X
// merges X ahead of E, and E, whose claim X changed, must rebase.
func TestPhase3MergeTaskAheadOfStuckHolder(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	e := newTask(t, h, "feature", "Dropbox integration")
	h.mustRun("plan", e, "config/services.php")
	h.mustRun("start", e)
	x := startedTask(t, h, "feature", "Clients CRUD")
	inHook(t, h, x.Worktree, "post-tool-use", toolCall("sx", "Edit", filepath.Join(x.Worktree, "config", "services.php"), ""))
	commitIn(t, x.Worktree, "config/services.php", "<?php return ['clients' => true];\n")
	finish(t, h, x.Slug, x.Worktree)

	out := h.mustRun("merge")
	mustContain(t, "plain merge", out, x.Slug+": waits for "+e+" to merge first",
		"shoalctl merge --task "+x.Slug, "merged 0 task(s)")
	if got := h.task(x.Slug); got.Stage != store.StageDone {
		t.Fatalf("plain merge moved X: %+v", got)
	}

	out = h.mustRun("merge", "--task", x.Slug)
	mustContain(t, "merge --task", out, x.Slug+": merging ahead of "+e+", as --task asked",
		"merged "+x.Slug+" into main", e+": needs-rebase: its claims overlap config/services.php", "merged 1 task(s)")
	if got := h.task(x.Slug); got.Stage != store.StageMerged {
		t.Fatalf("X after merge --task: %+v", got)
	}
	if got := h.task(e); got.Stage != store.StageNeedsRebase {
		t.Fatalf("E after X merged onto its claim: %+v", got)
	}
	if n := countRows(t, h.project(), `SELECT count(*) FROM task_blockers WHERE task_id = ?`, h.task(x.Slug).ID); n != 0 || h.task(x.Slug).BlockedBy != 0 {
		t.Errorf("X still has %d blocker row(s) after it merged", n)
	}
}

// Hooks register once, in the main checkout, and exit early elsewhere
// (Phase 0 decision). If a worktree registered them too, Claude Code would
// run each hook twice: every handler then still acts once. Proven by
// running each event twice at the same time as real processes.
func TestPhase3HooksNeverDoubleFire(t *testing.T) {
	testutil.RequireSh(t)
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	a := startedTask(t, h, "bugfix", "Users page wrong name")
	h.mustRun("record", "lesson", a.Slug, "--pattern", "accessor drift", "--fix", "one accessor", "--glob", "src/**")
	e := newTask(t, h, "feature", "Owner of src/b.go")
	h.mustRun("plan", e, "src/b.go")
	task := startedTask(t, h, "feature", "Doubly registered")
	h.mustRun("claim", task.Slug, "--as", "shoal-backend", "api/**")
	// The second registration, in the worktree's own settings.
	testutil.WriteFile(t, filepath.Join(task.Worktree, ".claude", "settings.local.json"), readFile(t, filepath.Join(h.repo, ".claude", "settings.local.json")))
	t.Setenv("CLAUDE_PROJECT_DIR", task.Worktree)

	twice := func(event, stdin string) []string {
		t.Helper()
		var outs []string
		var procs []func() string
		for i := 0; i < 2; i++ {
			cmd := h.command("hook", event)
			cmd.Stdin = strings.NewReader(stdin)
			var out bytes.Buffer
			cmd.Stdout = &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			procs = append(procs, func() string {
				if err := cmd.Wait(); err != nil {
					t.Fatalf("hook %s: %v", event, err)
				}
				return out.String()
			})
		}
		for _, wait := range procs {
			outs = append(outs, wait())
		}
		sort.Strings(outs)
		if outs[0] != "" || outs[1] == "" {
			t.Fatalf("%s fired twice: want one answer and one silent run, got %q", event, outs)
		}
		return outs
	}
	twice("session-start", `{"session_id": "dup", "source": "startup"}`)
	twice("pre-tool-use", toolCall("dup", "Read", filepath.Join(task.Worktree, "src", "a.go"), ""))
	twice("post-tool-use", toolCall("dup", "Write", filepath.Join(task.Worktree, "src", "b.go"), "shoal-backend"))

	p := h.project()
	if log := hookLog(t, p); log != "" {
		t.Fatalf("hook failures:\n%s", log)
	}
	if n := countRows(t, p, `SELECT hits FROM lessons WHERE id = 1`); n != 1 {
		t.Fatalf("the lesson counted %d hits for one touch", n)
	}
	if n := countRows(t, p, `SELECT count(*) FROM file_claims WHERE task_id = ? AND kind = 'actual'`, task.ID); n != 1 {
		t.Fatalf("%d actual claims for one write", n)
	}
	if n := countRows(t, p, `SELECT count(*) FROM messages WHERE task_id = ? AND subject LIKE 'late overlap%'`, h.task(e).ID); n != 1 {
		t.Fatalf("%d late-overlap messages for one write", n)
	}
	if n := countRows(t, p, `SELECT count(*) FROM task_blockers WHERE task_id = ?`, task.ID); n != 1 {
		t.Fatalf("%d merge-order rows for one write", n)
	}

	// The main checkout's own sessions (no shoal task) get nothing, from
	// every event, and nothing is written.
	t.Setenv("CLAUDE_PROJECT_DIR", h.repo)
	before := countRows(t, p, `SELECT count(*) FROM hook_marks`)
	for _, ev := range hookArgs {
		cmd := h.command("hook", ev)
		cmd.Stdin = strings.NewReader(toolCall("main", "Write", filepath.Join(h.repo, "src", "b.go"), ""))
		out, err := cmd.CombinedOutput()
		if err != nil || len(out) != 0 {
			t.Fatalf("%s in the main checkout: %v %q", ev, err, out)
		}
	}
	if after := countRows(t, p, `SELECT count(*) FROM hook_marks`); after != before {
		t.Fatalf("hooks in the main checkout wrote %d marks", after-before)
	}
}

// Hook latency: every event as a real process, the way Claude Code runs it,
// against a project with records to match. Reported with -v; the bound is
// loose so that -race and loaded CI machines pass, the numbers are the point.
func TestPhase3HookLatency(t *testing.T) {
	testutil.RequireSh(t)
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	a := startedTask(t, h, "feature", "Records")
	for i := 0; i < 20; i++ {
		h.mustRun("record", "lesson", a.Slug, "--pattern", fmt.Sprintf("pattern %d", i), "--fix", "fix", "--glob", fmt.Sprintf("src/m%d/**", i))
	}
	// Phase 4: 20 active conventions and 5 proposed ones, so SessionStart
	// renders the brainstorm and the rules and PreToolUse matches conventions.
	// Proposed in rounds: at most maxOpenProposals are open at once.
	for i := 0; i < 25; i++ {
		h.mustRun("convention", "propose", "--rule", fmt.Sprintf("module %d keeps one public entry point", i), "--scope", fmt.Sprintf("src/m%d/**", i))
		if i < 20 && i%maxOpenProposals == maxOpenProposals-1 {
			h.asHuman()
			for id := i + 2 - maxOpenProposals; id <= i+1; id++ {
				h.mustRun("convention", "agree", fmt.Sprint(id))
			}
			h.asAgent()
		}
	}
	task := startedTask(t, h, "feature", "Timed")
	t.Setenv("CLAUDE_PROJECT_DIR", task.Worktree)
	// Under -race every hook process is a race-instrumented binary taking
	// about a second, so fewer runs keep the bound asserted at a fraction of
	// the cost; the measured numbers come from the plain run.
	runs := 15
	if raceDetector {
		runs = 3
	}
	for _, ev := range []struct{ event, stdin string }{
		{"stop", `{"session_id": "lat"}`},
		{"session-start", `{"session_id": "lat", "source": "startup"}`},
		{"pre-tool-use", toolCall("lat", "Read", filepath.Join(task.Worktree, "src", "m3", "x.go"), "")},
		{"pre-tool-use (no match)", toolCall("lat", "Read", filepath.Join(task.Worktree, "README"), "")},
		{"post-tool-use", toolCall("lat", "Edit", filepath.Join(task.Worktree, "src", "m3", "x.go"), "")},
	} {
		event, _, _ := strings.Cut(ev.event, " ")
		var took []time.Duration
		for i := 0; i < runs; i++ {
			cmd := h.command("hook", event)
			cmd.Stdin = strings.NewReader(strings.ReplaceAll(ev.stdin, `"lat"`, fmt.Sprintf(`"lat-%d"`, i)))
			start := time.Now()
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			took = append(took, time.Since(start))
		}
		sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
		p50, p95 := took[runs/2], took[runs*95/100]
		t.Logf("hook %-24s p50 %6.1fms  p95 %6.1fms  (%d runs, process start included)", ev.event, ms(p50), ms(p95), runs)
		if p95 > 1500*time.Millisecond {
			t.Errorf("hook %s p95 %s: too close to the 2s hook deadline", ev.event, p95)
		}
	}
	if log := hookLog(t, h.project()); log != "" {
		t.Fatalf("hook failures:\n%s", log)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Spec §14 step 8 and §11.3: three weeks later the bug is back. why bug:<id>
// shows the stored root cause and fix, the later commit by another developer
// that touched the fix and deleted the regression test, and how the test
// last ran. why <sha> and recall find the same record.
func TestPhase3WhyBugCameBack(t *testing.T) {
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	bug := startedTask(t, h, "bugfix", "Users page shows wrong display name")
	commitIn(t, bug.Worktree, "tests/bug.t", "x")
	h.mustRun("repro", "red", bug.Slug, "--test", "C::bug", "--file", "tests/bug.t")
	commitIn(t, bug.Worktree, "fix.txt", "first_name ?? email\n")
	h.mustRun("repro", "green", bug.Slug, "--test", "C::bug")
	testutil.WriteFile(t, filepath.Join(bug.Worktree, ".shoal", "summary.json"),
		`{"decision": "fall back to the full name", "why": "the accessor fell back to email", "root_cause": "displayName falls back to email when first_name is null"}`)
	h.mustRun("done", "--force", bug.Slug)
	h.mustRun("merge")
	merged := h.task(bug.Slug).MergedSHA

	testutil.Git(t, h.repo, "rm", "-q", "tests/bug.t")
	testutil.WriteFile(t, filepath.Join(h.repo, "fix.txt"), "email\n")
	testutil.Git(t, h.repo, "add", "fix.txt")
	testutil.Git(t, h.repo, "commit", "-q", "--author", "Other Dev <other@example.com>", "-m", "rewrite the accessor")
	later := testutil.Git(t, h.repo, "rev-parse", "--short", "HEAD")

	out := h.mustRun("why", "bug:1")
	mustContain(t, "why bug:1", out, "bug:1 (task "+bug.Slug, "Symptom: Users page shows wrong display name",
		"Root cause: displayName falls back to email when first_name is null", "Fix: "+shortSHA(merged), "Files: fix.txt",
		"Commits touching these files after the fix", later+" Other Dev", "rewrite the accessor",
		"Regression test C::bug (tests/bug.t): DELETED in "+later+" Other Dev", "last recorded result: pass")
	mustContain(t, "why sha", h.mustRun("why", shortSHA(merged)), "bug:1", "summary:"+bug.Slug)
	mustContain(t, "why file", h.mustRun("why", "fix.txt"), "Stored WHY for fix.txt", "bug:1", "[files changed since record]", later)
	mustContain(t, "recall", h.mustRun("recall", "first_name"), "bug:1")
	mustContain(t, "recall --file", h.mustRun("recall", "--file", "fix.txt"), "bug:1", "summary:"+bug.Slug)
	mustContain(t, "nothing", h.mustRun("recall", "kubernetes"), "nothing recorded matches")
	h.expectCode(ExitNotFound, "why", "decision:9")
	h.expectCode(ExitUsage, "why", "nonsense:1")
	h.expectCode(ExitUsage, "record", "lesson", bug.Slug, "--pattern", "p")
	h.expectCode(ExitUsage, "record", "qa", bug.Slug, "--question", "q", "--answer", "a", "--file", "../etc/passwd")
	h.expectCode(ExitUsage, "flag-stale", "summary:1", "x")
}
