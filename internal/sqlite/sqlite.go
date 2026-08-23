// Package sqlite provides a read-only handle to a SQLite database file.
//
// A backup is a SQLite database. The DB type opens one read-only: the
// mode=ro URI parameter opens the file without ever writing to it, and
// a missing file fails the open instead of being created. The Integrity
// method verifies the database with integrity_check.
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// DB is a read-only handle to a single SQLite database file. Create
// one with New and release it with Close.
type DB struct {
	db *sql.DB
}

// New opens the database file read-only. The mode=ro URI parameter
// opens without write access: the file is never modified, and a
// missing database fails the open instead of being created. The path
// is percent-escaped into the file: URI (url.PathEscape): a raw '?'
// or '#' in dbPath would be misread by SQLite's URI parser as query
// or fragment syntax, and mode=ro would silently not apply. Opening a
// WAL-mode database read-only initializes the WAL infrastructure and
// leaves two artifacts next to the database (-shm and -wal); both are
// inert — the connection is read-only and integrity_check never
// writes — so they never affect the result.
func New(dbPath string) (*DB, error) {
	conn, err := sql.Open("sqlite", "file:"+url.PathEscape(dbPath)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// sql.Open opens connections lazily: Ping forces the open, so a
	// missing or unreadable database fails here, not at the first
	// query.
	err = conn.Ping()
	if err != nil {
		closeErr := conn.Close()
		return nil, errors.Join(fmt.Errorf("failed to open database: %w", err), closeErr)
	}

	return &DB{db: conn}, nil
}

// Close releases the database handle.
func (d *DB) Close() error {
	return d.db.Close()
}

// Integrity verifies the database with PRAGMA integrity_check.
// integrity_check returns a single "ok" row when the database is
// healthy, or one row per problem otherwise — the first row decides
// pass/fail, so the result set need not be exhausted.
func (d *DB) Integrity() error {
	var result string
	err := d.db.QueryRow("PRAGMA integrity_check;").Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("integrity_check returned no rows")
	}
	if err != nil {
		return fmt.Errorf("failed to execute integrity_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check failed, result was: %s", result)
	}
	return nil
}
