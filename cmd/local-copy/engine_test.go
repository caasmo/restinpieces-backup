package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caasmo/restinpieces/backup"
	"github.com/caasmo/restinpieces/config"
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
	// Deferred, not trailing: the t.Fatalf paths (Goexit) must not
	// leak the handle.
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

// setupTest creates a temporary directory, a source database with the users
// table and some data, and a backup config with the destination directory
// pointing to the temporary path.
func setupTest(t *testing.T, withData bool) (cfg *config.Backup, sourceDbPath, backupDir string) {
	t.Helper()

	tempDir := t.TempDir()
	sourceDbPath = filepath.Join(tempDir, "source.db")
	backupDir = filepath.Join(tempDir, "backups")

	if err := os.Mkdir(backupDir, 0755); err != nil {
		t.Fatalf("Failed to create backup dir: %v", err)
	}

	// Create and populate the source database
	createUsersDB(t, sourceDbPath, withData)

	// Create a config for the test
	cfg = &config.Backup{}

	return cfg, sourceDbPath, backupDir
}

// addDatabase adds a database file entry to the Backup files map.
func addDatabase(cfg *config.Backup, backupDir, key, sourcePath string, compress bool, strategy, frequency string) {
	freq, err := time.ParseDuration(frequency)
	if err != nil {
		panic(fmt.Sprintf("invalid test frequency %q: %v", frequency, err))
	}
	if cfg.Files == nil {
		cfg.Files = make(map[string]config.BackupFile)
	}
	entry := config.BackupFile{
		SourcePath:  sourcePath,
		DestPath:    backupDir,
		Compression: compress,
		Strategy:    strategy,
		Frequency:   config.Duration{Duration: freq},
	}
	if strategy == "" || strategy == config.BackupStrategyOnline {
		entry.OnlineAPIPagesPerStep = 100
	}
	cfg.Files[key] = entry
}

// verifyBackup checks if a backup file is a valid, non-empty SQLite database.
// It handles both compressed (.bck.gz) and uncompressed (.db) backup files.
func verifyBackup(t *testing.T, backupPath string, expectData bool, isCompressed bool) {
	t.Helper()

	dbPath := backupPath
	if isCompressed {
		// Decompress the backup file
		gzFile, err := os.Open(backupPath)
		if err != nil {
			t.Fatalf("Failed to open gzipped backup file: %v", err)
		}
		defer func() {
			if err := gzFile.Close(); err != nil {
				t.Logf("Failed to close gzipped backup file: %v", err)
			}
		}()

		gzReader, err := gzip.NewReader(gzFile)
		if err != nil {
			t.Fatalf("Failed to create gzip reader: %v", err)
		}
		defer func() {
			if err := gzReader.Close(); err != nil {
				t.Logf("Failed to close gzip reader: %v", err)
			}
		}()

		dbPath = backupPath + ".db"
		destFile, err := os.Create(dbPath)
		if err != nil {
			t.Fatalf("Failed to create decompressed destination file: %v", err)
		}
		defer func() {
			if err := destFile.Close(); err != nil {
				t.Logf("Failed to close decompressed destination file: %v", err)
			}
		}()

		if _, err := io.Copy(destFile, gzReader); err != nil {
			t.Fatalf("Failed to decompress file: %v", err)
		}
	}

	// Verify the contents of the database
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("Failed to open database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Logf("Failed to close database connection: %v", err)
		}
	}()

	var count int
	err = db.QueryRow("SELECT count(*) FROM users").Scan(&count)
	if err != nil {
		t.Fatalf("Failed to query database: %v", err)
	}

	if expectData && count == 0 {
		t.Error("Expected data in backup, but users table is empty")
	}
	if !expectData && count > 0 {
		t.Errorf("Expected empty backup, but found %d users", count)
	}
}

func TestEngine_Handle_SingleDB(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)

	testCases := []struct {
		name        string
		strategy    string
		compression bool
	}{
		{"OnlineCompressed", config.BackupStrategyOnline, true},
		{"OnlineUncompressed", config.BackupStrategyOnline, false},
		{"VacuumCompressed", config.BackupStrategyVacuum, true},
		{"VacuumUncompressed", config.BackupStrategyVacuum, false},
		{"DefaultStrategyCompressed", "", true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, sourcePath, backupDir := setupTest(t, true)
			addDatabase(cfg, backupDir, "source", sourcePath, tc.compression, tc.strategy, "24h")

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			engine := NewEngine(cfg, logger)

			err := engine.handle(context.Background(), mockTime)
			if err != nil {
				t.Fatalf("handle() error = %v, want nil", err)
			}

			// Verify the backup file exists
			dbName := "source-source.db"
			isCompressed := tc.compression
			expectedPath := filepath.Join(backupDir, (backupFile{
				backupID:   dbName,
				time:       mockTime,
				compressed: isCompressed,
			}).String())
			if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
				t.Fatalf("Expected backup file not found at %s", expectedPath)
			}
			verifyBackup(t, expectedPath, true, isCompressed)

			// Check latest link presence
			latestPath := filepath.Join(backupDir, fmt.Sprintf(backup.LatestFmt, dbName))
			if isCompressed {
				if _, err := os.Stat(latestPath); !os.IsNotExist(err) {
					t.Fatalf("Unexpected latest link for compressed backup at %s", latestPath)
				}
			} else {
				if _, err := os.Stat(latestPath); os.IsNotExist(err) {
					t.Fatalf("Expected latest link not found at %s", latestPath)
				}
			}
		})
	}
}

func TestEngine_Handle_MultiDB(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)

	// Create first database (source.db) via setupTest
	cfg, sourcePath, backupDir := setupTest(t, true)

	// Create a second database (second.db) in the same temp directory
	secondDbPath := filepath.Join(filepath.Dir(sourcePath), "second.db")
	createUsersDB(t, secondDbPath, true)

	// Add both databases: first uncompressed (online), second compressed (vacuum)
	addDatabase(cfg, backupDir, "first", sourcePath, false, config.BackupStrategyOnline, "24h")
	addDatabase(cfg, backupDir, "second", secondDbPath, true, config.BackupStrategyVacuum, "24h")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)

	err := engine.handle(context.Background(), mockTime)
	if err != nil {
		t.Fatalf("handle() error = %v, want nil", err)
	}

	// Verify the uncompressed backup file exists and has a latest link
	uncompressedPath := filepath.Join(backupDir, (backupFile{
		backupID:   "first-source.db",
		time:       mockTime,
		compressed: false,
	}).String())
	if _, err := os.Stat(uncompressedPath); os.IsNotExist(err) {
		t.Fatalf("Expected uncompressed backup not found at %s", uncompressedPath)
	}
	verifyBackup(t, uncompressedPath, true, false)

	latestPath := filepath.Join(backupDir, fmt.Sprintf(backup.LatestFmt, "first-source.db"))
	if _, err := os.Stat(latestPath); os.IsNotExist(err) {
		t.Fatalf("Expected latest link not found at %s", latestPath)
	}

	// Verify the compressed backup file exists but has no latest link
	compressedPath := filepath.Join(backupDir, (backupFile{
		backupID:   "second-second.db",
		time:       mockTime,
		compressed: true,
	}).String())
	if _, err := os.Stat(compressedPath); os.IsNotExist(err) {
		t.Fatalf("Expected compressed backup not found at %s", compressedPath)
	}
	verifyBackup(t, compressedPath, true, true)

	compressedLatest := filepath.Join(backupDir, fmt.Sprintf(backup.LatestFmt, "second-second.db"))
	if _, err := os.Stat(compressedLatest); !os.IsNotExist(err) {
		t.Fatalf("Unexpected latest link for compressed backup at %s", compressedLatest)
	}
}

func TestEngine_Handle_NoFiles(t *testing.T) {
	cfg, _, _ := setupTest(t, true) // backupDir is set, but no files added
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)
	err := engine.handle(context.Background(), time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("handle() with no files should not error, got: %v", err)
	}
}

func TestEngine_Handle_Deactivated(t *testing.T) {
	cfg, sourcePath, _ := setupTest(t, true)
	addDatabase(cfg, "", "source", sourcePath, false, config.BackupStrategyOnline, "24h") // empty dest_path deactivates the entry

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)

	err := engine.handle(context.Background(), time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("handle() with empty dest_path should not error, got: %v", err)
	}
}

func TestEngine_Handle_EmptySourcePathSkipped(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)
	cfg, sourcePath, backupDir := setupTest(t, true)
	addDatabase(cfg, backupDir, "active", sourcePath, false, config.BackupStrategyOnline, "24h")
	cfg.Files["deactivated"] = config.BackupFile{
		SourcePath: "",
		DestPath:   backupDir,
		Frequency:  config.Duration{Duration: 24 * time.Hour},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)

	err := engine.handle(context.Background(), mockTime)
	if err != nil {
		t.Fatalf("handle() with a deactivated entry should not error, got: %v", err)
	}

	// The active entry is backed up; the deactivated entry produces nothing.
	activePath := filepath.Join(backupDir, (backupFile{
		backupID:   "active-source.db",
		time:       mockTime,
		compressed: false,
	}).String())
	if _, err := os.Stat(activePath); os.IsNotExist(err) {
		t.Fatalf("expected active backup not found at %s", activePath)
	}
	entries, readErr := os.ReadDir(backupDir)
	if readErr != nil {
		t.Fatalf("failed to read backup dir: %v", readErr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "deactivated-") {
			t.Fatalf("unexpected artifact for deactivated entry: %s", e.Name())
		}
	}
}

func TestEngine_Handle_DestPathNotADirectory(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)
	cfg, sourcePath, _ := setupTest(t, true)
	// Point dest_path at an existing file instead of a directory.
	notADir := filepath.Join(filepath.Dir(sourcePath), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0644); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	addDatabase(cfg, notADir, "source", sourcePath, false, config.BackupStrategyOnline, "24h")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)

	err := engine.handle(context.Background(), mockTime)
	if err == nil {
		t.Fatal("handle() expected an error for dest_path being a file, got nil")
	}
}

func TestEngine_Handle_FrequencyRespected(t *testing.T) {
	cfg, sourcePath, backupDir := setupTest(t, true)
	addDatabase(cfg, backupDir, "source", sourcePath, false, config.BackupStrategyOnline, "2h") // frequency: 2 hours

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)

	// First backup at T=0
	t0 := time.Date(2025, 8, 1, 10, 0, 0, 0, time.UTC)
	if err := engine.handle(context.Background(), t0); err != nil {
		t.Fatalf("first backup failed: %v", err)
	}

	// Second attempt at T+30min — should be skipped (not due yet)
	t1 := t0.Add(30 * time.Minute)
	if err := engine.handle(context.Background(), t1); err != nil {
		t.Fatalf("second backup (skipped) failed: %v", err)
	}

	// Third attempt at T+2h — should run (due)
	t2 := t0.Add(2 * time.Hour)
	if err := engine.handle(context.Background(), t2); err != nil {
		t.Fatalf("third backup failed: %v", err)
	}

	// Verify only two backup files exist (first and third)
	dbName := "source-source.db"
	backup1 := filepath.Join(backupDir, (backupFile{
		backupID:   dbName,
		time:       t0,
		compressed: false,
	}).String())
	backup2 := filepath.Join(backupDir, (backupFile{
		backupID:   dbName,
		time:       t2,
		compressed: false,
	}).String())

	if _, err := os.Stat(backup1); os.IsNotExist(err) {
		t.Fatalf("Expected backup 1 not found at %s", backup1)
	}
	if _, err := os.Stat(backup2); os.IsNotExist(err) {
		t.Fatalf("Expected backup 2 not found at %s", backup2)
	}

	// Count total backup files (should be exactly 2)
	skippedPath := filepath.Join(backupDir, (backupFile{
		backupID:   dbName,
		time:       t1,
		compressed: false,
	}).String())
	if _, err := os.Stat(skippedPath); !os.IsNotExist(err) {
		t.Fatalf("Unexpected backup file for skipped attempt at %s", skippedPath)
	}

	// Latest link should be a hardlink to the third backup (same inode)
	latestPath := filepath.Join(backupDir, fmt.Sprintf(backup.LatestFmt, dbName))
	fiBackup, err := os.Stat(backup2)
	if err != nil {
		t.Fatalf("Failed to stat backup2: %v", err)
	}
	fiLink, err := os.Stat(latestPath)
	if err != nil {
		t.Fatalf("Expected latest link not found at %s: %v", latestPath, err)
	}
	if !os.SameFile(fiBackup, fiLink) {
		t.Fatal("Latest link does not share the same inode as the most recent backup")
	}
}

func TestEngine_Handle_ErrorCases(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)

	t.Run("SourceNotFound", func(t *testing.T) {
		cfg, _, backupDir := setupTest(t, true)
		addDatabase(cfg, backupDir, "source", "/path/to/nonexistent/source.db", false, config.BackupStrategyOnline, "24h")
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		engine := NewEngine(cfg, logger)

		err := engine.handle(context.Background(), mockTime)
		if err == nil {
			t.Fatal("handle() expected an error, but got nil")
		}
	})

	t.Run("BackupDirNotWritable", func(t *testing.T) {
		cfg, sourcePath, backupDir := setupTest(t, true)
		addDatabase(cfg, backupDir, "source", sourcePath, false, config.BackupStrategyOnline, "24h")
		// Make the backup directory read-only
		if err := os.Chmod(backupDir, 0400); err != nil {
			t.Fatalf("Failed to make backup dir read-only: %v", err)
		}

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		engine := NewEngine(cfg, logger)

		err := engine.handle(context.Background(), mockTime)
		if err == nil {
			t.Fatal("handle() expected an error for non-writable dir, but got nil")
		}
	})
}

func TestEngine_Handle_EmptyDatabase(t *testing.T) {
	cfg, sourcePath, backupDir := setupTest(t, false) // false -> don't add data
	addDatabase(cfg, backupDir, "source", sourcePath, false, config.BackupStrategyOnline, "24h")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)

	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)

	err := engine.handle(context.Background(), mockTime)
	if err != nil {
		t.Fatalf("handle() with empty db error = %v, want nil", err)
	}

	expectedPath := filepath.Join(backupDir, (backupFile{
		backupID:   "source-source.db",
		time:       mockTime,
		compressed: false,
	}).String())

	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Fatalf("Expected backup file not found at %s", expectedPath)
	}

	verifyBackup(t, expectedPath, false, false)
}

func TestEngine_Handle_EmptySource(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)
	tempDir := t.TempDir()
	sourcePath := filepath.Join(tempDir, "empty.db")
	if err := os.WriteFile(sourcePath, nil, 0644); err != nil {
		t.Fatalf("failed to create empty source file: %v", err)
	}
	backupDir := filepath.Join(tempDir, "backups")
	if err := os.Mkdir(backupDir, 0755); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}

	cfg := &config.Backup{}
	cfg.Files = map[string]config.BackupFile{
		"source": {
			SourcePath:           sourcePath,
			DestPath:             backupDir,
			Frequency:            config.Duration{Duration: 24 * time.Hour},
			OnlineAPIPagesPerStep: 100,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)

	err := engine.handle(context.Background(), mockTime)
	if err != nil {
		t.Fatalf("handle() with empty source db error = %v, want nil", err)
	}

	expectedPath := filepath.Join(backupDir, (backupFile{
		backupID:   "source-empty.db",
		time:       mockTime,
		compressed: false,
	}).String())
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Fatalf("Expected backup file not found at %s", expectedPath)
	}
}

func TestEngine_Handle_NotADatabaseFile(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)
	tempDir := t.TempDir()
	sourcePath := filepath.Join(tempDir, "garbage.db")
	if err := os.WriteFile(sourcePath, []byte("this is not a sqlite database file"), 0644); err != nil {
		t.Fatalf("failed to create non-database source file: %v", err)
	}
	backupDir := filepath.Join(tempDir, "backups")
	if err := os.Mkdir(backupDir, 0755); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}

	cfg := &config.Backup{}
	cfg.Files = map[string]config.BackupFile{
		"source": {
			SourcePath:           sourcePath,
			DestPath:             backupDir,
			Frequency:            config.Duration{Duration: 24 * time.Hour},
			OnlineAPIPagesPerStep: 100,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)

	err := engine.handle(context.Background(), mockTime)
	if err == nil {
		t.Fatal("handle() expected an error for a non-database source file, got nil")
	}

	entries, readErr := os.ReadDir(backupDir)
	if readErr != nil {
		t.Fatalf("failed to read backup dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no backup files for a non-database source, found %d", len(entries))
	}
}

func TestEngine_Handle_MissingSourceFile(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)
	tempDir := t.TempDir()
	backupDir := filepath.Join(tempDir, "backups")
	if err := os.Mkdir(backupDir, 0755); err != nil {
		t.Fatalf("failed to create backup dir: %v", err)
	}

	cfg := &config.Backup{}
	cfg.Files = map[string]config.BackupFile{
		"source": {
			SourcePath:           filepath.Join(tempDir, "missing.db"),
			DestPath:             backupDir,
			Frequency:            config.Duration{Duration: 24 * time.Hour},
			OnlineAPIPagesPerStep: 100,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)

	err := engine.handle(context.Background(), mockTime)
	if err == nil {
		t.Fatal("handle() expected an error for a missing source file, got nil")
	}

	entries, readErr := os.ReadDir(backupDir)
	if readErr != nil {
		t.Fatalf("failed to read backup dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no backup files for missing source, found %d", len(entries))
	}
}

func TestBackupFileString(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name string
		file backupFile
		want string
	}{
		{
			name: "compressed",
			file: backupFile{backupID: "app.db", time: mockTime, compressed: true},
			want: "app.db-20250801T103000Z.bck.gz",
		},
		{
			name: "uncompressed",
			file: backupFile{backupID: "app.db", time: mockTime},
			want: "app.db-20250801T103000Z.db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.file.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBackupFileRoundTrip(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)
	tests := []backupFile{
		{backupID: "app.db", time: mockTime, compressed: true},
		{backupID: "app.db", time: mockTime},
		{backupID: "app_db-app.db", time: mockTime, compressed: true},
	}
	for _, f := range tests {
		got, err := parseBackupFile(f.String())
		if err != nil {
			t.Errorf("round-trip %q: %v", f.String(), err)
			continue
		}
		if got.backupID != f.backupID || !got.time.Equal(f.time) || got.compressed != f.compressed {
			t.Errorf("round-trip %q = %+v, want %+v", f.String(), got, f)
		}
	}
}

func TestBuildTempPath(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)
	dbName := "app.db"

	engine := &Engine{}
	path := engine.buildTempPath(dbName, mockTime)

	prefix := filepath.Join(os.TempDir(), "backup-app.db-")
	if !strings.HasPrefix(path, prefix) {
		t.Fatalf("buildTempPath() = %q, want prefix %q", path, prefix)
	}
	if !strings.HasSuffix(path, ".db") {
		t.Fatalf("buildTempPath() = %q, want suffix '.db'", path)
	}
}

func TestParseBackupFile(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     backupFile
		wantOK   bool
	}{
		{
			name:     "valid compressed",
			filename: "app.db-20250801T103000Z.bck.gz",
			want: backupFile{
				backupID:   "app.db",
				time:       time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC),
				compressed: true,
			},
			wantOK: true,
		},
		{
			name:     "valid uncompressed",
			filename: "app.db-20250801T103000Z.db",
			want: backupFile{
				backupID:   "app.db",
				time:       time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC),
				compressed: false,
			},
			wantOK: true,
		},
		{
			name:     "legacy versioned filename ignored",
			filename: "app.db-20250801T103000Z-4821.bck.gz",
			wantOK:   false,
		},
		{
			name:     "stale tmp file",
			filename: "app.db-20250801T103000Z.bck.gz.tmp",
			wantOK:   false,
		},
		{
			name:     "regexp matches but invalid date",
			filename: "app.db-20251301T103000Z.bck.gz",
			wantOK:   false,
		},
		{
			name:     "latest link ignored",
			filename: "latest-app.db",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBackupFile(tt.filename)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("parseBackupFile(%q) error = %v, want nil", tt.filename, err)
				}
				if got.backupID != tt.want.backupID ||
					!got.time.Equal(tt.want.time) ||
					got.compressed != tt.want.compressed {
					t.Fatalf("parseBackupFile(%q) = %+v, want %+v", tt.filename, got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseBackupFile(%q) expected error, got nil", tt.filename)
			}
			if !errors.Is(err, errInvalidBackupFile) {
				t.Fatalf("parseBackupFile(%q) error = %v, want wrapping of errInvalidBackupFile", tt.filename, err)
			}
		})
	}
}

// TestCompressFile_SourceNotFound verifies compressFile returns an error
// when the source file does not exist.
func TestCompressFile_SourceNotFound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := &Engine{logger: logger}

	err := engine.compressFile("/nonexistent/source.db", filepath.Join(t.TempDir(), "out.bck.gz"))
	if err == nil {
		t.Fatal("compressFile() expected error for missing source, got nil")
	}
}

// TestLinkLatest_SourceNotFound verifies linkLatest returns an error
// when the backup file does not exist.
func TestLinkLatest_SourceNotFound(t *testing.T) {
	engine := &Engine{}
	backupDir := t.TempDir()

	err := engine.linkLatest("/nonexistent/backup.db", filepath.Join(backupDir, "latest-link"))
	if err == nil {
		t.Fatal("linkLatest() expected error for missing source, got nil")
	}
}

// TestEngine_Handle_CompressedError verifies error handling for
// compressed backup with non-existent source and read-only backup dir.
func TestEngine_Handle_CompressedError(t *testing.T) {
	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)

	t.Run("SourceMissing", func(t *testing.T) {
		cfg, _, backupDir := setupTest(t, true)
		addDatabase(cfg, backupDir, "source", "/nonexistent/source.db", true, config.BackupStrategyOnline, "24h")

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		engine := NewEngine(cfg, logger)

		err := engine.handle(context.Background(), mockTime)
		if err == nil {
			t.Fatal("handle() expected error for missing source, got nil")
		}
	})

	t.Run("BackupDirNotWritable", func(t *testing.T) {
		cfg, sourcePath, backupDir := setupTest(t, true)
		addDatabase(cfg, backupDir, "source", sourcePath, true, config.BackupStrategyOnline, "24h")
		if err := os.Chmod(backupDir, 0400); err != nil {
			t.Fatalf("Failed to make backup dir read-only: %v", err)
		}

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		engine := NewEngine(cfg, logger)

		err := engine.handle(context.Background(), mockTime)
		if err == nil {
			t.Fatal("handle() expected error for read-only backup dir, got nil")
		}
	})
}

// TestModuloLogger_Log verifies the progress logger's log method is exercised
// by creating a database large enough for multi-step online backup.
func TestModuloLogger_Log(t *testing.T) {
	cfg, sourcePath, backupDir := setupTest(t, false) // empty DB schema first
	// Add enough data to fill multiple database pages
	db, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatalf("Failed to open source db: %v", err)
	}
	// Insert 500 rows to create many pages (each page is 4096 bytes)
	for i := range 500 {
		name := fmt.Sprintf("user-%d", i)
		email := fmt.Sprintf("user%d@example.com", i)
		if _, execErr := db.Exec("INSERT INTO users (name, email) VALUES (?, ?)", name, email); execErr != nil {
			_ = db.Close()
			t.Fatalf("Failed to insert test data row %d: %v", i, execErr)
		}
	}
	if err := db.Close(); err != nil {
		t.Logf("Failed to close db connection: %v", err)
	}

	addDatabase(cfg, backupDir, "source", sourcePath, false, config.BackupStrategyOnline, "24h")
	// force many small steps
	source := cfg.Files["source"]
	source.OnlineAPIPagesPerStep = 1
	cfg.Files["source"] = source

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, logger)

	mockTime := time.Date(2025, 8, 1, 10, 30, 0, 0, time.UTC)
	err = engine.handle(context.Background(), mockTime)
	if err != nil {
		t.Fatalf("handle() with large db error = %v, want nil", err)
	}

	// Verify backup exists
	expectedPath := filepath.Join(backupDir, (backupFile{
		backupID:   "source-source.db",
		time:       mockTime,
		compressed: false,
	}).String())
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Fatalf("Expected backup file not found at %s", expectedPath)
	}
}