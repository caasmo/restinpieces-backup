package rsync

import (
	"database/sql"
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

// TestVerifyBackup checks the post-transfer sanity check against a
// destination directory holding one valid latest-* database.
func TestVerifyBackup(t *testing.T) {
	destDir := t.TempDir()
	createDB(t, filepath.Join(destDir, "latest-1.db"))

	err := VerifyBackup(destDir)
	if err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
}

// TestVerifyBackupEmpty checks that a destination directory without
// latest-* files fails the sanity check.
func TestVerifyBackupEmpty(t *testing.T) {
	err := VerifyBackup(t.TempDir())
	if err == nil {
		t.Fatal("VerifyBackup succeeded with no backup files")
	}
}
