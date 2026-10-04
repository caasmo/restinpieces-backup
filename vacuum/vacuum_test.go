package vacuum

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caasmo/restinpieces/config"
	"github.com/caasmo/restinpieces/db"
	_ "modernc.org/sqlite"
)

// createUsersDB creates a database file holding a users table, with
// one row when withData is true.
func createUsersDB(t *testing.T, path string, withData bool) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	}()
	_, err = db.Exec("CREATE TABLE users (name TEXT, email TEXT)")
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if withData {
		_, err = db.Exec("INSERT INTO users (name, email) VALUES ('test-user', 'test@example.com')")
		if err != nil {
			t.Fatalf("INSERT: %v", err)
		}
	}
}

func TestVacuumStrategy_EntriesAndCopy(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	createUsersDB(t, sourcePath, true)

	cfg := config.Config{Backup: config.Backup{Vacuum: config.BackupVacuum{
		"app": {SourcePath: sourcePath, DestPath: t.TempDir(), Frequency: config.Duration{Duration: 24 * time.Hour}, Compression: true},
	}}}
	pointer := new(atomic.Pointer[config.Config])
	pointer.Store(&cfg)
	strategy := &VacuumStrategy{cfgPointer: pointer}

	entries := strategy.Entries()
	if len(entries) != 1 {
		t.Fatalf("Entries() = %d entries, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Label != "app" || entry.SourcePath != sourcePath || !entry.Compression {
		t.Fatalf("Entries()[0] = %+v, want app/source/compressed", entry)
	}

	destPath := filepath.Join(entry.DestPath, "out.db")
	db, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	}()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close: %v", err)
		}
	}()

	if err := strategy.Copy(context.Background(), conn, destPath, entry); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	// The copy is a valid SQLite database holding the row.
	backupDB, err := sql.Open("sqlite", destPath)
	if err != nil {
		t.Fatalf("sql.Open(backup): %v", err)
	}
	defer func() {
		if err := backupDB.Close(); err != nil {
			t.Errorf("backupDB.Close: %v", err)
		}
	}()
	var count int
	if err := backupDB.QueryRow("SELECT count(*) FROM users").Scan(&count); err != nil {
		t.Fatalf("query backup: %v", err)
	}
	if count != 1 {
		t.Fatalf("users count = %d, want 1", count)
	}
}

// TestHandler_HandleCreatesBackup runs one scheduled pass through the
// handler: the entry never ran before, so it is due and a snapshot
// appears in the destination directory.
func TestHandler_HandleCreatesBackup(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	createUsersDB(t, sourcePath, true)
	backupDir := t.TempDir()

	cfg := config.Config{Backup: config.Backup{Vacuum: config.BackupVacuum{
		"app": {SourcePath: sourcePath, DestPath: backupDir, Frequency: config.Duration{Duration: time.Hour}},
	}}}
	pointer := new(atomic.Pointer[config.Config])
	pointer.Store(&cfg)
	handler := New(pointer, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	defer handler.ClosePools()

	if err := handler.Handle(context.Background(), db.Job{}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no backup file created by the pass")
	}
}
