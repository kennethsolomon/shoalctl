package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kennethsolomon/shoalctl/internal/store"
)

func TestApproxTokensCountsCJKAndBlobs(t *testing.T) {
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

// Worst case for the estimator: 6 matching lessons of 600 CJK characters
// each (the newest), 2 older ones of 960 hex characters, and hex blobs in the
// title and the ask. The brief stays under 1k by the estimator and by
// bytes/3, and shows the newest lessons: CJK ones.
func TestPhase3BriefUnderBudgetCJKAndBlobs(t *testing.T) {
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

// The brief shows the newest matching lessons, newest first, ties by id.
func TestTaskLessonsNewestFirst(t *testing.T) {
	var all []memo
	for i := 1; i <= 7; i++ {
		created := fmt.Sprintf("2026-11-0%dT00:00:00Z", i)
		if i == 7 {
			created = "2026-11-06T00:00:00Z" // same time as lesson 6: the higher id wins
		}
		all = append(all, memo{ref: store.LessonKey(int64(i)), kind: store.KindLesson, id: int64(i), glob: "src/**", inject: true, created: created})
	}
	all = append(all, memo{ref: "lesson:99", kind: store.KindLesson, id: 99, glob: "docs/**", inject: true, created: "2026-12-01T00:00:00Z"})
	var got []string
	for _, m := range taskLessons(all, []store.Claim{{Glob: "src/**"}}, "") {
		got = append(got, m.ref)
	}
	want := []string{"lesson:7", "lesson:6", "lesson:5", "lesson:4", "lesson:3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("taskLessons = %v, want %v", got, want)
	}
}
