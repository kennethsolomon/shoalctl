package store

import (
	"strings"
	"testing"
)

func TestPhase1Schema(t *testing.T) {
	s, _ := openTest(t)
	snap := schemaSnapshot(t, s)
	for _, want := range []string{"table:file_claims:", "table:messages:", "table:message_reads:", "table:contracts:",
		"table:test_quarantine:", "table:coverage_files:", "table:regression_proofs:", "table:scope_items:", "table:scope_cases:",
		"table:regression_overrides:", "table:qa_rounds:", "qa_round INTEGER NOT NULL DEFAULT 0", "parent_task_id", "merged_sha"} {
		if !strings.Contains(snap, want) {
			t.Errorf("schema lacks %q", want)
		}
	}
	migs, _ := Migrations()
	if len(migs) != 3 || migs[2].Name != "phase1" {
		t.Fatalf("migrations: %+v", migs)
	}
}
