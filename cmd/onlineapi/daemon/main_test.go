package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSQLiteFile writes a minimal file so the validation path check
// for source_path (existing file) passes. The entry is never backed
// up in these tests.
func writeSQLiteFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("SQLite format 3\x00"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestReadConfig pins the parse-and-validate contract of readConfig.
func TestReadConfig(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "app.db")
	writeSQLiteFile(t, srcPath)
	destDir := filepath.Join(t.TempDir(), "backups")
	if err := os.Mkdir(destDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	writeConfig := func(t *testing.T, tomlText string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "onlineapi.toml")
		if err := os.WriteFile(path, []byte(tomlText), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return path
	}

	t.Run("valid full entry", func(t *testing.T) {
		path := writeConfig(t, `
[backup.online.app_db]
source_path = "`+srcPath+`"
dest_path = "`+destDir+`"
frequency = "24h"
compression = true
pages_per_step = 50
sleep_interval = "5ms"
`)
		cfg, err := readConfig(path)
		if err != nil {
			t.Fatalf("readConfig: %v", err)
		}
		f := cfg.Backup.OnlineAPI["app_db"]
		if f.SourcePath != srcPath || f.DestPath != destDir || f.Frequency.Duration != 24*time.Hour || !f.Compression {
			t.Fatalf("entry = %+v, want source/dest/24h/compressed", f)
		}
		if f.PagesPerStep != 50 {
			t.Errorf("PagesPerStep: got %d, want 50", f.PagesPerStep)
		}
		if f.SleepInterval.Duration != 5*time.Millisecond {
			t.Errorf("SleepInterval: got %v, want 5ms", f.SleepInterval.Duration)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := readConfig(filepath.Join(t.TempDir(), "does-not-exist.toml"))
		if err == nil {
			t.Fatal("readConfig: expected error for missing file, got nil")
		}
	})

	t.Run("invalid TOML", func(t *testing.T) {
		path := writeConfig(t, "[backup")
		_, err := readConfig(path)
		if err == nil {
			t.Fatal("readConfig: expected error for invalid TOML, got nil")
		}
		if !strings.Contains(err.Error(), "failed to parse config file") {
			t.Errorf("readConfig: error %q should mention parsing", err)
		}
	})

	t.Run("validation failure", func(t *testing.T) {
		path := writeConfig(t, `
[backup.online.app_db]
source_path = "`+srcPath+`"
dest_path = "`+destDir+`"
frequency = "0s"
`)
		_, err := readConfig(path)
		if err == nil {
			t.Fatal("readConfig: expected error for zero frequency, got nil")
		}
		if !strings.Contains(err.Error(), "config validation failed") {
			t.Errorf("readConfig: error %q should mention validation", err)
		}
	})
}
