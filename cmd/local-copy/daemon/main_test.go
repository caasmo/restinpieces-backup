package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReadConfig pins the parse-and-validate contract of readConfig:
// valid TOML round-trips and every failure mode returns an error.
func TestReadConfig(t *testing.T) {
	// Real paths for the valid cases: ValidateBackup requires
	// source_path to be an existing file and dest_path an existing
	// directory.
	srcPath := filepath.Join(t.TempDir(), "app.db")
	createUsersDB(t, srcPath, true)
	destDir := filepath.Join(t.TempDir(), "backups")
	if err := os.Mkdir(destDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	writeConfig := func(t *testing.T, tomlText string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "local-copy.toml")
		if err := os.WriteFile(path, []byte(tomlText), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return path
	}

	t.Run("valid full entry online", func(t *testing.T) {
		path := writeConfig(t, `
[online.app_db]
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
		f := cfg.Online["app_db"]
		if f.SourcePath != srcPath {
			t.Errorf("SourcePath: got %q, want %q", f.SourcePath, srcPath)
		}
		if f.DestPath != destDir {
			t.Errorf("DestPath: got %q, want %q", f.DestPath, destDir)
		}
		if f.Frequency.Duration != 24*time.Hour {
			t.Errorf("Frequency: got %v, want 24h", f.Frequency.Duration)
		}
		if !f.Compression {
			t.Error("Compression: got false, want true")
		}
		if f.PagesPerStep != 50 {
			t.Errorf("PagesPerStep: got %d, want 50", f.PagesPerStep)
		}
		if f.SleepInterval.Duration != 5*time.Millisecond {
			t.Errorf("SleepInterval: got %v, want 5ms", f.SleepInterval.Duration)
		}
	})

	t.Run("valid full entry vacuum", func(t *testing.T) {
		path := writeConfig(t, `
[vacuum.app_db]
source_path = "`+srcPath+`"
dest_path = "`+destDir+`"
frequency = "24h"
compression = true
`)
		cfg, err := readConfig(path)
		if err != nil {
			t.Fatalf("readConfig: %v", err)
		}
		f := cfg.Vacuum["app_db"]
		if f.SourcePath != srcPath {
			t.Errorf("SourcePath: got %q, want %q", f.SourcePath, srcPath)
		}
		if f.DestPath != destDir {
			t.Errorf("DestPath: got %q, want %q", f.DestPath, destDir)
		}
		if f.Frequency.Duration != 24*time.Hour {
			t.Errorf("Frequency: got %v, want 24h", f.Frequency.Duration)
		}
		if !f.Compression {
			t.Error("Compression: got false, want true")
		}
	})

	t.Run("missing files section", func(t *testing.T) {
		path := writeConfig(t, `
[unrelated]
x = 1
`)
		cfg, err := readConfig(path)
		if err != nil {
			t.Fatalf("readConfig: %v", err)
		}
		if len(cfg.Online)+len(cfg.Vacuum) != 0 {
			t.Errorf("Online+Vacuum: got %d entries, want 0", len(cfg.Online)+len(cfg.Vacuum))
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := readConfig(filepath.Join(t.TempDir(), "does-not-exist.toml"))
		if err == nil {
			t.Fatal("readConfig: expected error for missing file, got nil")
		}
	})

	t.Run("invalid TOML", func(t *testing.T) {
		path := writeConfig(t, "[online")
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
[online.app_db]
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
