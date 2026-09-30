package s3

import (
	"testing"
	"time"

	"github.com/caasmo/restinpieces/config"
)

// resolvedConfig builds a config with one online entry, one vacuum entry
// and the given [backup.s3] entries.
func resolvedConfig(entries config.BackupS3) *config.Config {
	return &config.Config{
		Backup: config.Backup{
			OnlineAPI: config.BackupOnlineAPI{
				"app-online": {SourcePath: "/data/app.db", DestPath: "/data/backups", Frequency: config.Duration{Duration: time.Hour}, PagesPerStep: 100},
			},
			Vacuum: config.BackupVacuum{
				"app-vacuum": {SourcePath: "/data/other.db", DestPath: "/data/backups", Frequency: config.Duration{Duration: time.Hour}},
			},
			S3: entries,
		},
	}
}

func TestActiveEntries_Active(t *testing.T) {
	cfg := resolvedConfig(config.BackupS3{
		"app-s3":   {BackupLabel: "app-online", Frequency: config.Duration{Duration: 5 * time.Minute}, AgeRecipient: "age1..."},
		"other-s3": {BackupLabel: "app-vacuum"},
	})

	got := ActiveEntries(cfg)
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}

	byLabel := make(map[string]Entry, len(got))
	for _, entry := range got {
		byLabel[entry.Label] = entry
	}

	want := Entry{
		Label:        "app-s3",
		BackupLabel:  "app-online",
		Frequency:    5 * time.Minute,
		AgeRecipient: "age1...",
		SourcePath:   "/data/app.db",
		BackupDir:    "/data/backups",
	}
	if byLabel["app-s3"] != want {
		t.Fatalf("app-s3 = %+v, want %+v", byLabel["app-s3"], want)
	}
	if byLabel["other-s3"].SourcePath != "/data/other.db" {
		t.Fatalf("other-s3 source = %q, want %q", byLabel["other-s3"].SourcePath, "/data/other.db")
	}
}

func TestActiveEntries_Deactivated(t *testing.T) {
	cfg := &config.Config{
		Backup: config.Backup{
			OnlineAPI: config.BackupOnlineAPI{
				"app-offline": {DestPath: "/data/backups"}, // no source_path: deactivated
			},
			S3: config.BackupS3{
				"empty-label": {}, // no backup_label: deactivated
				"inactive":    {BackupLabel: "app-offline"},
			},
		},
	}

	got := ActiveEntries(cfg)
	if len(got) != 0 {
		t.Fatalf("entries = %d, want 0", len(got))
	}
}

func TestActiveEntries_UnknownBackupLabelIsSkipped(t *testing.T) {
	cfg := resolvedConfig(config.BackupS3{
		"app-s3": {BackupLabel: "missing"},
	})

	got := ActiveEntries(cfg)
	if len(got) != 0 {
		t.Fatalf("entries = %d, want 0", len(got))
	}
}
