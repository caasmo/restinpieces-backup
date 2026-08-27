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
	_ "modernc.org/sqlite"
)

// testCfg is a box payload satisfying VacuumConfig for tests.
type testCfg struct {
	backup config.Backup
}

func (c testCfg) BackupVacuum() config.BackupVacuum {
	return c.backup.Vacuum
}

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

	cfg := config.Backup{Vacuum: config.BackupVacuum{
		"app": {SourcePath: sourcePath, DestPath: t.TempDir(), Frequency: config.Duration{Duration: 24 * time.Hour}, Compression: true},
	}}
	box := new(atomic.Pointer[testCfg])
	box.Store(&testCfg{backup: cfg})
	strategy := &VacuumStrategy[testCfg]{box: box}

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

func TestNew_DaemonRunStop(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	createUsersDB(t, sourcePath, true)
	backupDir := t.TempDir()

	cfg := config.Backup{Vacuum: config.BackupVacuum{
		"app": {SourcePath: sourcePath, DestPath: backupDir, Frequency: config.Duration{Duration: time.Hour}},
	}}
	box := new(atomic.Pointer[testCfg])
	box.Store(&testCfg{backup: cfg})
	d := New[testCfg](box, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The first copy runs immediately at startup: wait for a backup
	// file to appear.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, readErr := os.ReadDir(backupDir)
		if readErr == nil && len(entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backup was not created by the startup copy")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
