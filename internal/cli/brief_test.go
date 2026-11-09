package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/store"
)

func TestApproxTokensCountsCJKAndBlobs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		in   string
		min  int
	}{
		{"han", strings.Repeat("漢", 300), 300},
		{"hiragana", strings.Repeat("ひ", 90), 90},
		{"katakana", strings.Repeat("カ", 90), 90},
		{"hangul", strings.Repeat("한", 90), 90},
		{"cjk inside a latin word", "abc" + strings.Repeat("字", 60) + "def", 60},
		{"hex", strings.Repeat("deadbeef", 25), 100},
		{"base64", strings.Repeat("aGVsbG8gd29ybGQ", 12) + "==", 90},
	} {
		if got := approxTokens(c.in); got < c.min {
			t.Errorf("%s: approxTokens = %d, want at least %d", c.name, got, c.min)
		}
	}
	// English and paths keep the old estimate.
	en := "Users page shows the wrong display name when first_name is null"
	if got := approxTokens(en); got != 21 {
		t.Errorf("english: approxTokens = %d, want 21", got)
	}
}

// Scripts without spaces or CJK ranges (Thai) and emoji runs cost about a
// token per UTF-8 byte triple or more, so the estimate never drops below
// ceil(bytes/3).
func TestApproxTokensAtLeastBytesOverThree(t *testing.T) {
	t.Parallel()
	thai := string([]rune(strings.Repeat("ภาษาไทยไม่มีช่องว่างระหว่างคำ", 20))[:300])
	for _, c := range []struct{ name, in string }{
		{"thai", thai},
		{"emoji", strings.Repeat("😀🚀🎉", 100)},
		{"emoji zwj", strings.Repeat("👩‍💻👨‍👩‍👧", 40)},
	} {
		want := (len(c.in) + 2) / 3
		if got := approxTokens(c.in); got < want {
			t.Errorf("%s: approxTokens = %d, want at least ceil(%d bytes/3) = %d", c.name, got, len(c.in), want)
		}
	}
}

// Worst case for the estimator: 6 matching lessons of 600 CJK characters
// each (the newest), 2 older ones of 960 hex characters, and hex blobs in the
// title and the ask. The brief stays under 1k by the estimator and by
// bytes/3, and shows the newest lessons: CJK ones.
func TestPhase3BriefUnderBudgetCJKAndBlobs(t *testing.T) {
	t.Parallel()
	h := newHarness(t).withSession()
	h.initWith(junitConfig)
	hex := strings.Repeat("0123456789abcdef", 30)
	slug := strings.TrimSpace(h.mustRun("new", "feature", "Fix "+hex[:110], "--ask", hex+hex))
	other := newTask(t, h, "feature", "Earlier task")
	h.mustRun("plan", slug, "src/**")
	h.mustRun("start", slug)
	task := h.task(slug)
	for i := 0; i < 2; i++ {
		h.mustRun("record", "lesson", other, "--pattern", hex[:480], "--fix", hex[:480], "--glob", "src/**")
	}
	cjk := strings.Repeat("一覧画面は別のアクセサを読む。ひらがなカタカナ한국어", 20)
	for i := 0; i < 6; i++ {
		text := string([]rune(fmt.Sprint(i) + cjk)[:300])
		h.mustRun("record", "lesson", other, "--pattern", text, "--fix", text, "--glob", "src/**")
	}

	brief := inHook(t, h, task.Worktree, "session-start", `{"session_id": "s-cjk", "source": "startup"}`).HookSpecificOutput.AdditionalContext
	tokens, byBytes := approxTokens(brief), (len(brief)+2)/3
	t.Logf("brief: %d bytes, %d estimated tokens, bytes/3 %d (budget 1000)", len(brief), tokens, byBytes)
	if tokens >= 1000 || byBytes >= 1000 {
		t.Fatalf("brief is %d estimated tokens, bytes/3 %d, want both < 1000:\n%s", tokens, byBytes, brief)
	}
	mustContain(t, "brief", brief, "shoal task "+slug, "Lessons from earlier tasks", "- lesson:8 ", "一覧画面")
}

// The brief shows the newest matching lessons, newest first, ties by id. A
// stack-wide lesson (no glob, the project's stack) is among them when it is
// one of the newest 5; one of another stack never is.
func TestTaskLessonsNewestFirst(t *testing.T) {
	t.Parallel()
	var all []memo
	for i := 1; i <= 7; i++ {
		created := fmt.Sprintf("2026-11-0%dT00:00:00Z", i)
		if i == 7 {
			created = "2026-11-06T00:00:00Z" // same time as lesson 6: the higher id wins
		}
		all = append(all, memo{ref: store.LessonKey(int64(i)), kind: store.KindLesson, id: int64(i), glob: "src/**", inject: true, created: created})
	}
	all = append(all,
		memo{ref: "lesson:8", kind: store.KindLesson, id: 8, stack: "laravel", inject: true, created: "2026-11-05T12:00:00Z"},
		memo{ref: "lesson:9", kind: store.KindLesson, id: 9, stack: "laravel", inject: true, created: "2026-10-01T00:00:00Z"},
		memo{ref: "lesson:10", kind: store.KindLesson, id: 10, stack: "rails", inject: true, created: "2026-12-01T00:00:00Z"},
		memo{ref: "lesson:99", kind: store.KindLesson, id: 99, glob: "docs/**", inject: true, created: "2026-12-01T00:00:00Z"})
	var got []string
	for _, m := range taskLessons(all, []store.Claim{{Glob: "src/**"}}, "laravel") {
		got = append(got, m.ref)
	}
	want := []string{"lesson:7", "lesson:6", "lesson:8", "lesson:5", "lesson:4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("taskLessons = %v, want %v", got, want)
	}
}

// The brainstorm survives the budget: lessons, todo lines and project rules
// go first, and every proposal cut is counted in a "+N more" line (§4.6).
func TestBriefBudgetKeepsTheBrainstorm(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("keep every payment provider behind one strategy interface; ", 6)
	d := briefData{task: store.Task{Slug: "1-pay", Lane: "feature", Stage: "in-progress", Title: "Pay"}, maxRounds: 3}
	for i := 0; i < 30; i++ {
		d.proposed = append(d.proposed, fmt.Sprintf("- convention:%d (files src/pay/**): %s", i+1, clip(long, 300)))
		d.lessons = append(d.lessons, fmt.Sprintf("- lesson:%d (task 1-pay, at abc1234): %s", i+1, clip(long, 300)))
		d.rules = append(d.rules, fmt.Sprintf("- convention:%d (files **): %s", 100+i, clip(long, 300)))
		d.todo = append(d.todo, long)
	}
	d.promotions = []string{"- promote lesson:7 (files src/**, shown in 3 tasks): " + clip(long, 200)}
	d.promotedRules = []string{"- lesson:3 (files src/**): " + clip(long, 300)}
	out, kept := renderSessionBrief(d)
	if approxTokens(out) > briefBudget || kept != 0 {
		t.Fatalf("%d tokens, %d lessons kept:\n%s", approxTokens(out), kept, out)
	}
	if !strings.HasPrefix(out, "BEFORE ANYTHING ELSE, brainstorm with the human: 31 proposed project rule(s)") {
		t.Fatalf("the brief does not open with the brainstorm:\n%s", out)
	}
	// One proposal and the promotion keep the brainstorm open; the agreed
	// rules get the rest of the budget before more proposals do.
	mustContain(t, "brief", out, "- convention:1 (files src/pay/**)", "- +29 more proposed: shoalctl convention list", "- promote lesson:7 ")
	if strings.Contains(out, "- convention:2 (files src/pay/**)") || strings.Contains(out, "more proposed for promotion") {
		t.Fatalf("a second proposal kept over the agreed rules:\n%s", out)
	}
	shown := strings.Count(out, "\n- convention:1") - 1 // convention:100..
	var more int
	rules := out[strings.Index(out, "Project rules"):]
	if _, err := fmt.Sscanf(rules[strings.Index(rules, "- +"):], "- +%d more rules", &more); err != nil || shown < 3 || shown+more != 30 {
		t.Fatalf("%d rules shown, %d counted as more (%v):\n%s", shown, more, err, out)
	}
	// A promoted lesson has no per-file injection, so it outlasts conventions.
	mustContain(t, "promoted rule", out, "- lesson:3 (files src/**)")
	if strings.Contains(out, "todo.md") || strings.Contains(out, "lesson:1 ") {
		t.Errorf("todo lines or lessons kept over proposals:\n%s", out)
	}
}
