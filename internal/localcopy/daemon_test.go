package localcopy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInterval covers the tick cadence selection: the smallest
// frequency among the active entries, with deactivated entries skipped
// and zero when nothing is active.
func TestInterval(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	backupDir := filepath.Join(t.TempDir(), "backups")

	t.Run("single active entry", func(t *testing.T) {
		d := New("TestDaemon", &fakeStrategy{entries: []Entry{
			{Label: "a", SourcePath: sourcePath, DestPath: backupDir, Frequency: time.Hour},
		}}, nil)
		if got := d.interval(); got != time.Hour {
			t.Fatalf("interval() = %v, want %v", got, time.Hour)
		}
	})

	t.Run("two entries, smaller frequency wins", func(t *testing.T) {
		d := New("TestDaemon", &fakeStrategy{entries: []Entry{
			{Label: "a", SourcePath: sourcePath, DestPath: backupDir, Frequency: time.Hour},
			{Label: "b", SourcePath: sourcePath, DestPath: backupDir, Frequency: 30 * time.Minute},
		}}, nil)
		if got := d.interval(); got != 30*time.Minute {
			t.Fatalf("interval() = %v, want %v", got, 30*time.Minute)
		}
	})

	t.Run("empty paths skipped", func(t *testing.T) {
		d := New("TestDaemon", &fakeStrategy{entries: []Entry{
			{Label: "a", SourcePath: "", DestPath: backupDir, Frequency: time.Hour},
			{Label: "b", SourcePath: sourcePath, DestPath: "", Frequency: 30 * time.Minute},
			{Label: "c", SourcePath: sourcePath, DestPath: backupDir, Frequency: time.Minute},
		}}, nil)
		if got := d.interval(); got != time.Minute {
			t.Fatalf("interval() = %v, want %v", got, time.Minute)
		}
	})

	t.Run("no active entries", func(t *testing.T) {
		d := New("TestDaemon", &fakeStrategy{entries: []Entry{
			{Label: "a", SourcePath: "", DestPath: "", Frequency: time.Hour},
		}}, nil)
		if got := d.interval(); got != 0 {
			t.Fatalf("interval() = %v, want 0", got)
		}
	})
}

// TestLocalCopyDaemonRunStop runs the daemon over an entry whose
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

	d := New("TestDaemon", &fakeStrategy{entries: []Entry{
		{Label: "app_db", SourcePath: sourcePath, DestPath: backupDir, Frequency: time.Hour},
	}}, nil)
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

// TestLocalCopyDaemonRunStopNoEntries runs the daemon over an empty
// entries list: it runs, copies nothing, and stops gracefully.
func TestLocalCopyDaemonRunStopNoEntries(t *testing.T) {
	d := New("TestDaemon", &fakeStrategy{}, nil)
	if err := d.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
