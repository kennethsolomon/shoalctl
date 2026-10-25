// Package store owns memory.db: connection settings, versioned migrations, and
// the task and test-run records (spec §4.5, §10, §15.1).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	ErrSchemaTooNew  = errors.New("database schema is newer than this binary")
	ErrSchemaChanged = errors.New("database schema does not match this binary")
	ErrBusy          = errors.New("database stayed locked")
	ErrConflict      = errors.New("concurrent update conflict")
	ErrNotFound      = errors.New("not found")
	ErrStage         = errors.New("task is in a stage that does not allow this")
)

type Options struct {
	Create      bool // create memory.db when missing
	Migrate     bool // apply pending migrations; when false the schema must already be current
	BusyTimeout time.Duration
	Retries     int // extra attempts after SQLITE_BUSY outlasts BusyTimeout
}

type Store struct {
	db      *sql.DB
	retries int
}

type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the embedded migrations in version order.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		num, name, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration file %q must be named NNNN_name.sql", e.Name())
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1..N without gaps; found %d at position %d", m.Version, i+1)
		}
	}
	return out, nil
}

// Open opens memory.db with WAL, busy_timeout, foreign keys, and immediate
// transactions, then migrates or checks the schema version.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	migs, err := Migrations()
	if err != nil {
		return nil, err
	}
	return open(ctx, path, opts, migs)
}

func open(ctx context.Context, path string, opts Options, migs []Migration) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("database path %q must be absolute", path)
	}
	if !opts.Create {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
	}
	mode := "rw"
	if opts.Create {
		mode = "rwc"
	}
	q := url.Values{}
	q.Set("mode", mode)
	q.Set("_txlock", "immediate")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", opts.BusyTimeout.Milliseconds()))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath // Windows drive path: file:///C:/...
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: q.Encode()}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, retries: opts.Retries}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if opts.Migrate {
		err = s.migrate(ctx, path, migs)
	} else {
		err = s.requireVersion(ctx, migs[len(migs)-1].Version)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) userVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int, error) {
	var v int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return v, nil
}

func (s *Store) requireVersion(ctx context.Context, want int) error {
	v, err := s.userVersion(ctx, s.db)
	if err != nil {
		return err
	}
	if v > want {
		return fmt.Errorf("%w: memory.db is schema %d; this shoalctl knows %d. Upgrade shoalctl", ErrSchemaTooNew, v, want)
	}
	if v != want {
		return fmt.Errorf("%w: memory.db is schema %d, want %d", ErrSchemaChanged, v, want)
	}
	return nil
}

func (s *Store) migrate(ctx context.Context, path string, migs []Migration) error {
	latest := migs[len(migs)-1].Version
	cur, err := s.userVersion(ctx, s.db)
	if err != nil {
		return err
	}
	if cur > latest {
		return fmt.Errorf("%w: memory.db is schema %d; this shoalctl knows %d. Upgrade shoalctl", ErrSchemaTooNew, cur, latest)
	}
	for _, m := range migs {
		if m.Version <= cur {
			continue
		}
		if cur > 0 {
			if err := s.backup(ctx, fmt.Sprintf("%s.bak-%d", path, m.Version)); err != nil {
				return err
			}
		}
		err := s.write(ctx, func(tx *sql.Tx) error {
			v, err := s.userVersion(ctx, tx)
			if err != nil || v >= m.Version {
				return err
			}
			if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
				return fmt.Errorf("apply migration %04d_%s: %w", m.Version, m.Name, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
				m.Version, m.Name, checksum(m.SQL), now()); err != nil {
				return fmt.Errorf("record migration %d: %w", m.Version, err)
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
				return fmt.Errorf("set user_version %d: %w", m.Version, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		cur = m.Version
	}
	return nil
}

func (s *Store) backup(ctx context.Context, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO "+sqlQuote(dst)); err != nil {
		return fmt.Errorf("back up database to %s: %w", dst, err)
	}
	return os.Chmod(dst, 0o600)
}

// write runs fn in one BEGIN IMMEDIATE transaction, retrying when the write
// lock stays busy past busy_timeout.
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := s.tx(ctx, fn)
		if !isBusy(err) {
			return err
		}
		if attempt >= s.retries {
			return fmt.Errorf("%w after %d attempts: %v", ErrBusy, attempt+1, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(50+rand.IntN(151)) * time.Millisecond):
		}
	}
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func isBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
