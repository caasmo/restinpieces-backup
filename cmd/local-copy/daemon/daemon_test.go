package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caasmo/restinpieces/config"
	_ "modernc.org/sqlite"
)

// TestInterval covers the tick cadence selection: the smallest
// frequency among the active entries, with deactivated entries skipped
// and zero when nothing is active. The entries need no real files —
// interval() only reads paths and frequencies.
func TestInterval(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	backupDir := filepath.Join(t.TempDir(), "backups")

	t.Run("single active entry", func(t *testing.T) {
		cfg := &config.Backup{Online: config.BackupOnline{
			"a": {SourcePath: sourcePath, DestPath: backupDir, Frequency: config.Duration{Duration: time.Hour}},
		}}
		d := New(cfg, nil)
		if got := d.interval(); got != time.Hour {
			t.Fatalf("interval() = %v, want %v", got, time.Hour)
		}
	})

	t.Run("two entries, smaller frequency wins", func(t *testing.T) {
		cfg := &config.Backup{Online: config.BackupOnline{
			"a": {SourcePath: sourcePath, DestPath: backupDir, Frequency: config.Duration{Duration: time.Hour}},
			"b": {SourcePath: sourcePath, DestPath: backupDir, Frequency: config.Duration{Duration: 30 * time.Minute}},
		}}
		d := New(cfg, nil)
		if got := d.interval(); got != 30*time.Minute {
			t.Fatalf("interval() = %v, want %v", got, 30*time.Minute)
		}
	})

	t.Run("empty paths skipped", func(t *testing.T) {
		cfg := &config.Backup{
			Online: config.BackupOnline{
				"a": {SourcePath: "", DestPath: backupDir, Frequency: config.Duration{Duration: time.Hour}},
			},
			Vacuum: config.BackupVacuum{
				"b": {SourcePath: sourcePath, DestPath: "", Frequency: config.Duration{Duration: 30 * time.Minute}},
				"c": {SourcePath: sourcePath, DestPath: backupDir, Frequency: config.Duration{Duration: time.Minute}},
			},
		}
		d := New(cfg, nil)
		if got := d.interval(); got != time.Minute {
			t.Fatalf("interval() = %v, want %v", got, time.Minute)
		}
	})

	t.Run("no active entries", func(t *testing.T) {
		cfg := &config.Backup{Online: config.BackupOnline{
			"a": {SourcePath: "", DestPath: "", Frequency: config.Duration{Duration: time.Hour}},
		}}
		d := New(cfg, nil)
		if got := d.interval(); got != 0 {
			t.Fatalf("interval() = %v, want 0", got)
		}
	})
}

// TestLocalCopyDaemonRunStop runs the daemon over a config whose
// frequency is long, waits for the immediate startup copy to produce a
// snapshot, then stops it: Stop must return once the copy goroutine
// has exited.
func TestLocalCopyDaemonRunStop(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	createUsersDB(t, sourcePath, true)
	backupDir := filepath.Join(t.TempDir(), "backups")
	if err := os.Mkdir(backupDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	cfg := &config.Backup{Online: config.BackupOnline{
		"app_db": {
			SourcePath:   sourcePath,
			DestPath:     backupDir,
			Frequency:    config.Duration{Duration: time.Hour},
			PagesPerStep: 100,
			SleepInterval: config.Duration{Duration: 10 * time.Millisecond},
		},
	}}
	d := New(cfg, nil)
	err := d.Run()
	if err != nil {
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
	err = d.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestLocalCopyDaemonRunStopNoEntries runs the daemon over an empty
// Files map: it runs, copies nothing, and stops gracefully.
func TestLocalCopyDaemonRunStopNoEntries(t *testing.T) {
	d := New(&config.Backup{}, nil)
	err := d.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = d.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestLocalCopyDaemonRunStopAbortsInFlightCopy starts a copy whose
// throttle is 30s, waits until the copy is in flight (the .tmp
// destination exists), then stops the daemon: Stop must return
// promptly, because the cancelled context aborts the sleeping copy at
// the next step boundary. It must not wait out the throttle.
func TestLocalCopyDaemonRunStopAbortsInFlightCopy(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	createUsersDB(t, sourcePath, true)
	// Fill multiple pages so the online copy cannot finish in one step.
	db, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	for i := range 500 {
		name := fmt.Sprintf("user-%d", i)
		email := fmt.Sprintf("user%d@example.com", i)
		if _, execErr := db.Exec("INSERT INTO users (name, email) VALUES (?, ?)", name, email); execErr != nil {
			_ = db.Close()
			t.Fatalf("INSERT: %v", execErr)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	backupDir := filepath.Join(t.TempDir(), "backups")
	if err := os.Mkdir(backupDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	cfg := &config.Backup{Online: config.BackupOnline{
		"app_db": {
			SourcePath:   sourcePath,
			DestPath:     backupDir,
			Frequency:    config.Duration{Duration: time.Hour},
			PagesPerStep: 1,
			SleepInterval: config.Duration{Duration: 30 * time.Second},
		},
	}}
	d := New(cfg, nil)
	err = d.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() {
		_ = d.Stop(context.Background())
	}()

	// Wait for the copy to reach the throttle: the .tmp destination
	// exists once the backup is open and the copy is in flight.
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, readErr := os.ReadDir(backupDir)
		inFlight := false
		if readErr == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".tmp") {
					inFlight = true
					break
				}
			}
		}
		if inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("copy never reached the in-flight state")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Stop with a tight deadline: it must return well before the 30s
	// throttle expires, proving the context cancellation aborted the
	// sleeping copy instead of waiting it out.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = d.Stop(ctx)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
