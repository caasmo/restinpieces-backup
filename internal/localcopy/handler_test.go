package localcopy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caasmo/restinpieces/db"
)

// TestHandler_HandleCreatesBackup runs one scheduled pass through the
// handler: the entry never ran before, so it is due and a snapshot
// appears in the destination directory.
func TestHandler_HandleCreatesBackup(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	createUsersDB(t, sourcePath, true)
	backupDir := filepath.Join(t.TempDir(), "backups")
	if err := os.Mkdir(backupDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	handler := NewHandler(&fakeStrategy{entries: []Entry{
		{Label: "app_db", SourcePath: sourcePath, DestPath: backupDir, Frequency: time.Hour},
	}}, nil)
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
