package sqlitedb

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// createDB creates a database file holding one table with one row.
func createDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec("CREATE TABLE t(x)")
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	_, err = db.Exec("INSERT INTO t VALUES(1)")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	err = db.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestIntegrity checks that a healthy database passes the integrity
// check.
func TestIntegrity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "backup.db")
	createDB(t, dbPath)

	d, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		_ = d.Close()
	}()

	err = d.Integrity()
	if err != nil {
		t.Fatalf("Integrity: %v", err)
	}
}

// TestIntegrityCorrupt checks that a file that is not a database is
// rejected. The rejection can surface at either step: New fails when
// the driver validates the header eagerly, or Integrity fails when the
// open is lazy — both mean the file is not accepted as a valid backup.
func TestIntegrityCorrupt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "backup.db")
	err := os.WriteFile(dbPath, []byte("this is not a database"), 0644)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	d, err := New(dbPath)
	if err != nil {
		// Rejected at open: the corrupt file is not accepted.
		return
	}
	defer func() {
		_ = d.Close()
	}()

	err = d.Integrity()
	if err == nil {
		t.Fatal("Integrity succeeded on a corrupt database")
	}
}

// TestNewMissing checks that creating a handle for a missing database
// fails: the read-only open cannot create the file.
func TestNewMissing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "does-not-exist.db")

	_, err := New(dbPath)
	if err == nil {
		t.Fatal("New succeeded on a missing database")
	}
}

// TestPageSize checks that a database created with the default
// settings has a page size of 4096 bytes.
func TestPageSize(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "backup.db")
	createDB(t, dbPath)

	d, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		_ = d.Close()
	}()

	n, err := d.PageSize()
	if err != nil {
		t.Fatalf("PageSize: %v", err)
	}
	if n != 4096 {
		t.Fatalf("PageSize = %d, want 4096", n)
	}
}

// TestPageCount checks that a database holding one table occupies at
// least one page.
func TestPageCount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "backup.db")
	createDB(t, dbPath)

	d, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		_ = d.Close()
	}()

	n, err := d.PageCount()
	if err != nil {
		t.Fatalf("PageCount: %v", err)
	}
	if n < 1 {
		t.Fatalf("PageCount = %d, want >= 1", n)
	}
}
