package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReadConfig pins the parse contract of readConfig: the [backup]
// section is the application configuration shape.
func TestReadConfig(t *testing.T) {
	writeConfig := func(t *testing.T, tomlText string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "vacuum.toml")
		if err := os.WriteFile(path, []byte(tomlText), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return path
	}

	t.Run("valid full entry", func(t *testing.T) {
		path := writeConfig(t, `
[backup.vacuum.app_db]
source_path = "/path/to/app.db"
dest_path = "/path/to/backups"
frequency = "24h"
compression = true
`)
		cfg, err := readConfig(path)
		if err != nil {
			t.Fatalf("readConfig: %v", err)
		}
		f := cfg.Backup.Vacuum["app_db"]
		if f.SourcePath != "/path/to/app.db" || f.DestPath != "/path/to/backups" || f.Frequency.Duration != 24*time.Hour || !f.Compression {
			t.Fatalf("entry = %+v, want source/dest/24h/compressed", f)
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
}
