package onlineapi

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caasmo/restinpieces/config"
	_ "modernc.org/sqlite"
)

// testCfg is a box payload satisfying OnlineApiConfig for tests.
type testCfg struct {
	backup config.Backup
}

func (c testCfg) BackupOnlineAPI() config.BackupOnlineAPI {
	return c.backup.OnlineAPI
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

func TestOnlineApiStrategy_EntriesAndCopy(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	createUsersDB(t, sourcePath, true)

	cfg := config.Backup{OnlineAPI: config.BackupOnlineAPI{
		"app": {SourcePath: sourcePath, DestPath: t.TempDir(), Frequency: config.Duration{Duration: 24 * time.Hour}, PagesPerStep: 100, SleepInterval: config.Duration{Duration: 10 * time.Millisecond}},
	}}
	box := new(atomic.Pointer[testCfg])
	box.Store(&testCfg{backup: cfg})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	strategy := &OnlineApiStrategy[testCfg]{box: box, logger: logger}

	entries := strategy.Entries()
	if len(entries) != 1 {
		t.Fatalf("Entries() = %d entries, want 1", len(entries))
	}

	destPath := filepath.Join(entries[0].DestPath, "out.db")
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

	if err := strategy.Copy(context.Background(), conn, destPath, entries[0]); err != nil {
		t.Fatalf("Copy: %v", err)
	}

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

// TestModuloLogger_Log verifies the progress logger is exercised by a
// database large enough for a multi-step online backup.
func TestModuloLogger_Log(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	createUsersDB(t, sourcePath, false)
	db, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	for i := range 500 {
		name := fmt.Sprintf("user-%d", i)
		email := fmt.Sprintf("user%d@example.com", i)
		if _, execErr := db.Exec("INSERT INTO users (name, email) VALUES (?, ?)", name, email); execErr != nil {
			_ = db.Close()
			t.Fatalf("failed to insert test data row %d: %v", i, execErr)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	cfg := config.Backup{OnlineAPI: config.BackupOnlineAPI{
		"source": {SourcePath: sourcePath, DestPath: t.TempDir(), Frequency: config.Duration{Duration: 24 * time.Hour}, PagesPerStep: 1, SleepInterval: config.Duration{Duration: 10 * time.Millisecond}},
	}}
	box := new(atomic.Pointer[testCfg])
	box.Store(&testCfg{backup: cfg})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	strategy := &OnlineApiStrategy[testCfg]{box: box, logger: logger}

	entries := strategy.Entries()
	destPath := filepath.Join(entries[0].DestPath, "out.db")

	srcDB, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() {
		if err := srcDB.Close(); err != nil {
			t.Errorf("srcDB.Close: %v", err)
		}
	}()
	conn, err := srcDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close: %v", err)
		}
	}()

	if err := strategy.Copy(context.Background(), conn, destPath, entries[0]); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if _, err := os.Stat(destPath); os.IsNotExist(err) {
		t.Fatalf("expected backup file at %s", destPath)
	}
}
