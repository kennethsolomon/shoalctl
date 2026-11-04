package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestPhase4Schema(t *testing.T) {
	s, _ := openTest(t)
	snap := schemaSnapshot(t, s)
	for _, want := range []string{"table:conventions:", "table:lesson_hits:", "trigger:conventions_fts_insert:", "trigger:conventions_fts_update:"} {
		if !strings.Contains(snap, want) {
			t.Errorf("schema lacks %q", want)
		}
	}
	migs, _ := Migrations()
	if len(migs) < 7 || migs[6].Name != "conventions" {
		t.Fatalf("migrations: %+v", migs)
	}
}

// Hits counted before migration 7 are spread over the tasks whose sessions
// were shown the lesson, one per session, compacted marks included.
func TestPhase4MigrationBackfillsLessonHitsPerTask(t *testing.T) {
	migs, _ := Migrations()
	path := filepath.Join(t.TempDir(), "memory.db")
	ctx := context.Background()
	old, err := open(ctx, path, testOpts(), migs[:6])
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO tasks (id, slug, lane, title, ask, stage, created_at, updated_at) VALUES (1, '1-a', 'feature', 'A', 'a', 'done', 'x', 'x'), (2, '2-b', 'feature', 'B', 'b', 'done', 'x', 'x')`,
		`INSERT INTO lessons (id, task_id, pattern, fix, true_at_sha, hits, created_at) VALUES (1, 1, 'p', 'f', 'abc', 3, 'x')`,
		`INSERT INTO hook_marks (session_id, key, task_id, created_at) VALUES
			('s1', 'lesson:1', 1, '2026-01-01T00:00:00Z'),
			('s1', 'shown:lesson:1', 1, '2026-01-01T00:00:00Z'),
			('s2', 'shown:lesson:1', 1, '2026-01-02T00:00:00Z'),
			('s3', 'lesson:1', 2, '2026-01-03T00:00:00Z'),
			('s3', 'lesson:99', 2, '2026-01-03T00:00:00Z'),
			('s3', 'decision:1', 2, '2026-01-03T00:00:00Z')`,
	} {
		if _, err := old.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	s, err := Open(ctx, path, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.LessonTaskHits(ctx)
	if err != nil || len(got) != 1 || got[1] != 2 {
		t.Fatalf("tasks per lesson %v, %v", got, err)
	}
	var hits int
	if err := s.db.QueryRow(`SELECT hits FROM lesson_hits WHERE lesson_id = 1 AND task_id = 1`).Scan(&hits); err != nil || hits != 2 {
		t.Fatalf("task 1 hits %d, %v", hits, err)
	}
}

func TestMarkCountsLessonHitsPerTask(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	a, b := memoryTask(t, s, "A"), memoryTask(t, s, "B")
	id, err := s.AddLesson(ctx, Lesson{TaskID: a.ID, Pattern: "p", Fix: "f", PathGlob: "src/**", TrueAtSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		session string
		task    int64
	}{{"s1", a.ID}, {"s1", a.ID}, {"s2", a.ID}, {"s3", b.ID}} {
		if _, err := s.Mark(ctx, m.session, m.task, []string{LessonKey(id)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LessonTaskHits(ctx)
	if err != nil || got[id] != 2 {
		t.Fatalf("tasks per lesson %v, %v", got, err)
	}
	if err := s.PromoteLesson(ctx, id, 3); !errors.Is(err, ErrStage) || !strings.Contains(err.Error(), "shown in 2 task(s)") {
		t.Fatalf("promotion after 2 tasks: %v", err)
	}
	c := memoryTask(t, s, "C")
	if _, err := s.Mark(ctx, "s4", c.ID, []string{LessonKey(id)}); err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteLesson(ctx, id, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteLesson(ctx, id, 3); !errors.Is(err, ErrStage) {
		t.Fatalf("second promotion: %v", err)
	}
	ls, _ := s.Lessons(ctx)
	if !ls[0].Promoted || ls[0].Hits != 4 {
		t.Fatalf("lesson %+v", ls[0])
	}
}

func TestConventionLifecycle(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := memoryTask(t, s, "Payments")
	id, existing, err := s.ProposeConvention(ctx, Convention{Rule: "Use the strategy pattern", ScopeGlob: "src/pay/**", SourceTaskID: task.ID}, 0)
	if err != nil || existing {
		t.Fatal(err, existing)
	}
	again, existing, err := s.ProposeConvention(ctx, Convention{Rule: " use the STRATEGY pattern ", ScopeGlob: "src/pay/**"}, 0)
	if err != nil || !existing || again != id {
		t.Fatalf("duplicate proposal: %d %v %v", again, existing, err)
	}
	other, existing, err := s.ProposeConvention(ctx, Convention{Rule: "Use the strategy pattern", ScopeGlob: "**"}, 0)
	if err != nil || existing || other == id {
		t.Fatalf("other scope: %d %v %v", other, existing, err)
	}
	if err := s.AgreeConvention(ctx, id, Agreement{Rule: "Use the strategy pattern for providers", ScopeGlob: "src/pay/**", By: "Kenneth"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AgreeConvention(ctx, id, Agreement{Rule: "x", ScopeGlob: "**", By: "Kenneth"}); !errors.Is(err, ErrStage) {
		t.Fatalf("agree twice: %v", err)
	}
	c, err := s.GetConvention(ctx, id)
	if err != nil || c.Status != ConventionActive || c.AgreedBy != "Kenneth" || c.Rule != "Use the strategy pattern for providers" || c.SourceTaskID != task.ID {
		t.Fatalf("agreed: %+v %v", c, err)
	}
	hits, err := s.Search(ctx, "providers", 10)
	if err != nil || len(hits) != 1 || hits[0] != (SearchHit{Kind: KindConvention, Ref: id}) {
		t.Fatalf("search after the rule changed: %+v %v", hits, err)
	}
	if err := s.RetireConvention(ctx, id, "Kenneth", "providers moved to a plugin registry"); err != nil {
		t.Fatal(err)
	}
	if err := s.RetireConvention(ctx, id, "Kenneth", "again"); !errors.Is(err, ErrStage) {
		t.Fatalf("retire twice: %v", err)
	}
	c, _ = s.GetConvention(ctx, id)
	if c.Status != ConventionRetired || c.RetireReason != "providers moved to a plugin registry" || c.RetiredAt == "" {
		t.Fatalf("retired: %+v", c)
	}
	if hits, _ := s.Search(ctx, "registry", 10); len(hits) != 1 {
		t.Fatalf("the retire reason is not searchable: %+v", hits)
	}
	if _, err := s.GetConvention(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing convention: %v", err)
	}
}

// A convention that came from a lesson promotes the lesson when agreed and
// takes it out of use when retired.
func TestConventionFromLesson(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := memoryTask(t, s, "A")
	lid, err := s.AddLesson(ctx, Lesson{TaskID: task.ID, Pattern: "p", Fix: "f", PathGlob: "src/**", TrueAtSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.ProposeConvention(ctx, Convention{Rule: "p Fix: f", ScopeGlob: "src/**", LessonID: lid, Conflict: "CLAUDE.md:3: never f"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if again, existing, _ := s.ProposeConvention(ctx, Convention{Rule: "other words", ScopeGlob: "src/**", LessonID: lid}, 0); !existing || again != id {
		t.Fatalf("second proposal from the same lesson: %d %v", again, existing)
	}
	if err := s.AgreeConvention(ctx, id, Agreement{Rule: "p Fix: f", ScopeGlob: "src/**", By: "K"}); err != nil {
		t.Fatal(err)
	}
	ls, _ := s.Lessons(ctx)
	if !ls[0].Promoted {
		t.Fatal("agreeing the lesson's convention did not promote it")
	}
	if err := s.ReopenConvention(ctx, id, "CLAUDE.md:3: never f"); err != nil {
		t.Fatal(err)
	}
	ls, _ = s.Lessons(ctx)
	c, _ := s.GetConvention(ctx, id)
	if ls[0].Promoted || c.Status != ConventionProposed || c.Conflict == "" {
		t.Fatalf("reopened: %+v %+v", c, ls[0])
	}
	if err := s.AgreeConvention(ctx, id, Agreement{Rule: "p Fix: f", ScopeGlob: "src/**", By: "K"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RetireConvention(ctx, id, "K", "no longer true"); err != nil {
		t.Fatal(err)
	}
	ls, _ = s.Lessons(ctx)
	if !ls[0].Stale || !strings.Contains(ls[0].StaleReason, "no longer true") {
		t.Fatalf("lesson after its convention retired: %+v", ls[0])
	}
}

func TestProposeFromPromotedLesson(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	task := memoryTask(t, s, "A")
	lid, _ := s.AddLesson(ctx, Lesson{TaskID: task.ID, Pattern: "p", Fix: "f", TrueAtSHA: "abc"})
	if _, err := s.db.Exec(`UPDATE lessons SET promoted = 1`); err != nil {
		t.Fatal(err)
	}
	id, err := s.ProposeFromPromotedLesson(ctx, Convention{Rule: "p Fix: f", ScopeGlob: "**", LessonID: lid, Conflict: "CLAUDE.md:1: x"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ProposeFromPromotedLesson(ctx, Convention{Rule: "p Fix: f", ScopeGlob: "**", LessonID: lid, Conflict: "CLAUDE.md:1: x"})
	if err != nil || again != id {
		t.Fatalf("second reconcile: %d %v", again, err)
	}
	ls, _ := s.Lessons(ctx)
	cs, _ := s.Conventions(ctx)
	if ls[0].Promoted || len(cs) != 1 || cs[0].Status != ConventionProposed || cs[0].LessonID != lid {
		t.Fatalf("after the conflict: %+v %+v", ls[0], cs)
	}
}

func TestParseConventionRef(t *testing.T) {
	for in, want := range map[string]int64{"3": 3, "convention:12": 12} {
		if got, err := ParseConventionRef(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "-1", "lesson:3", "convention:", "3x", "convention:03"} {
		if _, err := ParseConventionRef(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// Open proposals are capped, and a rule the human retired is not silently
// proposed again with the same scope.
func TestProposeConventionLimits(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 3; i++ {
		id, _, err := s.ProposeConvention(ctx, Convention{Rule: fmt.Sprintf("rule %d", i), ScopeGlob: "**"}, 3)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, _, err := s.ProposeConvention(ctx, Convention{Rule: "rule 3", ScopeGlob: "**"}, 3); !errors.Is(err, ErrProposalRefused) || !strings.Contains(err.Error(), "3 conventions are already proposed") {
		t.Fatalf("past the limit: %v", err)
	}
	if id, existing, err := s.ProposeConvention(ctx, Convention{Rule: "RULE 1", ScopeGlob: "**"}, 3); err != nil || !existing || id != ids[1] {
		t.Fatalf("a duplicate at the limit: %d %v %v", id, existing, err)
	}
	if err := s.AgreeConvention(ctx, ids[0], Agreement{Rule: "rule 0", ScopeGlob: "**", By: "Kenneth"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ProposeConvention(ctx, Convention{Rule: "rule 3", ScopeGlob: "**"}, 3); err != nil {
		t.Fatalf("an agreed proposal still counts against the limit: %v", err)
	}
	if err := s.RetireConvention(ctx, ids[1], "Kenneth", "too vague"); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.ProposeConvention(ctx, Convention{Rule: " Rule 1 ", ScopeGlob: "**"}, 0)
	if !errors.Is(err, ErrProposalRefused) || !strings.Contains(err.Error(), fmt.Sprintf("convention:%d", ids[1])) || !strings.Contains(err.Error(), "too vague") {
		t.Fatalf("re-proposing a retired rule: %v", err)
	}
	if _, existing, err := s.ProposeConvention(ctx, Convention{Rule: "rule 1", ScopeGlob: "src/**"}, 0); err != nil || existing {
		t.Fatalf("the retired rule in another scope: %v %v", existing, err)
	}
}

// A lesson whose convention the human retired is never promoted.
func TestPromoteLessonRefusesRetiredConvention(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	var tasks []Task
	for _, title := range []string{"A", "B", "C"} {
		tasks = append(tasks, memoryTask(t, s, title))
	}
	lid, err := s.AddLesson(ctx, Lesson{TaskID: tasks[0].ID, Pattern: "p", Fix: "f", PathGlob: "src/**", TrueAtSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range tasks {
		if _, err := s.Mark(ctx, fmt.Sprintf("s%d", i), task.ID, []string{LessonKey(lid)}); err != nil {
			t.Fatal(err)
		}
	}
	cid, _, err := s.ProposeConvention(ctx, Convention{Rule: "p Fix: f", ScopeGlob: "src/**", LessonID: lid, Conflict: "CLAUDE.md:3: never f"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteLesson(ctx, lid, 3); !errors.Is(err, ErrStage) || !strings.Contains(err.Error(), "proposed") {
		t.Fatalf("promoting a lesson under discussion: %v", err)
	}
	if err := s.RetireConvention(ctx, cid, "K", "no"); err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteLesson(ctx, lid, 3); !errors.Is(err, ErrStage) || !strings.Contains(err.Error(), ConventionKey(cid)) || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("promoting a declined lesson: %v", err)
	}
	if ls, _ := s.Lessons(ctx); ls[0].Promoted {
		t.Fatal("promoted")
	}
}
