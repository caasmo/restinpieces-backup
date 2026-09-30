// Package s3 holds the S3 feature's shared pieces: the resolution of the
// [backup.s3] entries against the backup tables. The upload daemon that
// uses them lives in s3/upload.
package s3

import (
	"time"

	"github.com/caasmo/restinpieces/config"
)

// Entry is one active [backup.s3] entry resolved against the online and
// vacuum tables: the label it is keyed by, the backup it points at, and
// the settings and paths the upload needs.
type Entry struct {
	Label        string
	BackupLabel  string
	Frequency    time.Duration
	AgeRecipient string
	SourcePath   string
	BackupDir    string
}

// ActiveEntries returns the entries that can run, flattened into a list
// and resolved so the caller needs nothing else. An entry is left out
// when its backup_label is empty (deactivated) or when the online or
// vacuum backup it names is deactivated (no source or destination path).
func ActiveEntries(cfg *config.Config) []Entry {
	var entries []Entry
	for label, entry := range cfg.BackupS3() {
		if entry.BackupLabel == "" {
			continue
		}

		online, inOnline := cfg.BackupOnlineAPI()[entry.BackupLabel]
		vacuum, inVacuum := cfg.BackupVacuum()[entry.BackupLabel]

		var sourcePath, destPath string
		if inOnline {
			sourcePath, destPath = online.SourcePath, online.DestPath
		} else if inVacuum {
			sourcePath, destPath = vacuum.SourcePath, vacuum.DestPath
		}
		if sourcePath == "" || destPath == "" {
			continue
		}

		entries = append(entries, Entry{
			Label:        label,
			BackupLabel:  entry.BackupLabel,
			Frequency:    entry.Frequency.Duration,
			AgeRecipient: entry.AgeRecipient,
			SourcePath:   sourcePath,
			BackupDir:    destPath,
		})
	}
	return entries
}
