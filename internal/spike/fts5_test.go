package spike

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestFTS5CreateInsertMatch(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE VIRTUAL TABLE fts_lessons USING fts5(path, body)`); err != nil {
		t.Fatalf("create fts5 table: %v", err)
	}

	rows := [][2]string{
		{"app/Http/Controllers/UserController.php", "null name on users caused by missing eager load"},
		{"resources/js/Pages/Users.vue", "token expiry handled in axios interceptor"},
		{"database/migrations/2026_users.php", "always backfill before adding NOT NULL"},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO fts_lessons(path, body) VALUES (?, ?)`, r[0], r[1]); err != nil {
			t.Fatalf("insert %q: %v", r[0], err)
		}
	}

	var got []string
	res, err := db.Query(`SELECT path FROM fts_lessons WHERE fts_lessons MATCH ? ORDER BY rank`, "token OR eager")
	if err != nil {
		t.Fatalf("match query: %v", err)
	}
	defer res.Close()
	for res.Next() {
		var p string
		if err := res.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, p)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("MATCH 'token OR eager' returned %d rows, want 2: %v", len(got), got)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM fts_lessons WHERE fts_lessons MATCH 'body:backfill'`).Scan(&n); err != nil {
		t.Fatalf("column filter query: %v", err)
	}
	if n != 1 {
		t.Fatalf("column-filtered MATCH returned %d rows, want 1", n)
	}
}
