// Command hookprobe approximates a shoalctl hook call for Phase 0 startup timing:
// open memory.db, run one FTS5 MATCH, print the hit count, exit.
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: hookprobe <memory.db>")
		os.Exit(2)
	}
	db, err := sql.Open("sqlite", os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS fts_lessons USING fts5(path, body)`); err != nil {
		fmt.Fprintf(os.Stderr, "create fts5 table: %v\n", err)
		os.Exit(1)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM fts_lessons WHERE fts_lessons MATCH 'token'`).Scan(&n); err != nil {
		fmt.Fprintf(os.Stderr, "match query: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(n)
}
